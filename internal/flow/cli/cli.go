// Package cli implements the agentflow command dispatch. Exit codes are
// assigned in this package and nowhere else, one table per command
// (exitCodes for codex, shipExitCodes for ship verify, prExitCodes for pr
// wait, prMergeExitCodes for pr merge). 0 is success, 1 a
// usage or precondition error, 124 a timeout and 130 an interruption for
// every command.
package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/agent"
)

// Version is injected from main via ldflags.
var Version = "dev"

const usage = `agentflow — small, single-purpose workflow commands for coding agents

Usage:
  agentflow codex [flags]                      run Codex (codex exec) safely; JSON result
  agentflow claude [flags]                     run Claude Code (claude -p) safely; JSON result
  agentflow ship verify <svc.env> --sha REV    wait until a deployed service runs REV
  agentflow ship announce <svc.env> --verified FILE ...
                                               post a verified deploy to Slack
                                               (see: agentflow ship --help)
  agentflow pr wait <number> [--repo O/N]      wait for CodeRabbit's review of the PR head;
                                               list open threads (see: agentflow pr --help)
  agentflow pr merge <number> [--sync-branch B] merge a ready PR pinned to its head;
                                               report the merge sha
  agentflow worktree done <branch|path>        remove a merged, clean, unused worktree
  agentflow worktree sweep [--yes]             list (or remove) every such worktree
                                               (see: agentflow worktree --help)
  agentflow version                           print agentflow's own version

codex and claude flags:
  --dir DIR               repository to work in (default: current directory)
  --prompt TEXT           prompt text
  --prompt-file FILE      prompt read from FILE
  --base REF              review the diff REF...HEAD
  --commit SHA            review one commit
  --uncommitted           review staged, unstaged and untracked changes
  --path PATHSPEC         limit the reviewed diff (repeatable; needs a prompt)
  --write                 fix mode: edits allowed, confined to --dir (default: read-only)
  --model MODEL           model to use
  --timeout DUR           hard cap on the run (default 40m)
  --stall DUR             kill after this long with no activity (default 10m; 0 disables)
  --out DIR               where to write prompt.md, final.md, events.jsonl, stderr.log
                          and result.json (default: a new temp dir)
  --max-prompt-bytes N    refuse prompts over N bytes (default 80000)
  --max-budget-usd N      claude only: stop the run at this spend

  A scope with a prompt inlines the scoped diff under your prompt. A scope
  with no prompt runs codex's built-in reviewer (codex exec review); claude
  has none, so it gets a short default review brief with the diff inlined.
  stdin is never read and never passed on to the agent.

  Sandboxing: codex runs with -c sandbox_mode=read-only (or workspace-write).
  claude runs --restricted with only Read, Grep and Glob; in fix mode it also
  gets Edit, Write and Bash, with Bash in Claude Code's sandbox (writes only
  under --dir, no network). Prompt-driven runs are told they are a sub-agent:
  do the task, report, stop, and start no other agents.

Output: the JSON result on stdout (also saved as <out>/result.json); the
review itself is in the file named by "final".

Exit codes: 0 ok · 1 usage/precondition · 3 agent failed · 4 no final answer
            5 rate/usage limited · 124 timeout · 125 stalled · 130 interrupted
`

var exitCodes = map[agent.Status]int{
	agent.StatusOK:           0,
	agent.StatusFailed:       3,
	agent.StatusClaudeFailed: 3,
	agent.StatusNoAnswer:     4,
	agent.StatusRateLimited:  5,
	agent.StatusTimeout:      124,
	agent.StatusStalled:      125,
	agent.StatusInterrupted:  130,
}

// Run executes agentflow with args (without the program name).
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 1
	}
	switch args[0] {
	case "codex", "claude":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		b := agent.Codex
		if args[0] == "claude" {
			b = agent.Claude
		}
		return runAgent(ctx, args[0], b, args[1:], stdout, stderr)
	case "ship":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runShip(ctx, args[1:], stdout, stderr)
	case "worktree":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runWorktree(ctx, args[1:], stdout, stderr)
	case "pr":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runPR(ctx, args[1:], stdout, stderr)
	case "version", "--version":
		fmt.Fprintln(stdout, "agentflow "+Version)
		return 0
	case "help", "--help", "-h":
		fmt.Fprint(stdout, usage)
		return 0
	default:
		fmt.Fprintf(stderr, "agentflow: unknown command %q\n\n%s", args[0], usage)
		return 1
	}
}

type pathList []string

func (p *pathList) String() string     { return strings.Join(*p, " ") }
func (p *pathList) Set(v string) error { *p = append(*p, v); return nil }

// defaultReviewBrief is used when claude gets a scope but no prompt (it has no
// built-in reviewer like `codex exec review`).
const defaultReviewBrief = "Review the diff below for correctness bugs, regressions and security problems. " +
	"For each finding give the severity (Blocker, Major or Minor), file:line, what is wrong and why. " +
	"Skip style nits. If there are no real findings, say so plainly."

const defaultFixLine = " Fix each real finding directly in the working tree and say what you changed."

func runAgent(ctx context.Context, name string, b agent.Backend, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agentflow "+name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		o          agent.Options
		prompt     string
		promptFile string
		maxBytes   int
		paths      pathList
	)
	fs.StringVar(&o.Dir, "dir", "", "")
	fs.StringVar(&prompt, "prompt", "", "")
	fs.StringVar(&promptFile, "prompt-file", "", "")
	fs.StringVar(&o.Scope.Base, "base", "", "")
	fs.StringVar(&o.Scope.Commit, "commit", "", "")
	fs.BoolVar(&o.Scope.Uncommitted, "uncommitted", false, "")
	fs.Var(&paths, "path", "")
	fs.BoolVar(&o.Write, "write", false, "")
	fs.StringVar(&o.Model, "model", "", "")
	fs.DurationVar(&o.Timeout, "timeout", 40*time.Minute, "")
	fs.DurationVar(&o.Stall, "stall", 10*time.Minute, "")
	fs.StringVar(&o.OutDir, "out", "", "")
	fs.IntVar(&maxBytes, "max-prompt-bytes", 80000, "")
	if b == agent.Claude {
		fs.Float64Var(&o.MaxBudgetUSD, "max-budget-usd", 0, "")
	}
	o.Agent = b
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow "+name+": "+format+"\n", a...)
		return 1
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, usage)
			return 0
		}
		return fail("%v (see: agentflow --help)", err)
	}
	if fs.NArg() > 0 {
		return fail("unexpected argument %q: pass the prompt with --prompt or --prompt-file", fs.Arg(0))
	}
	o.Scope.Paths = paths

	scopes := 0
	for _, set := range []bool{o.Scope.Base != "", o.Scope.Commit != "", o.Scope.Uncommitted} {
		if set {
			scopes++
		}
	}
	if scopes > 1 {
		return fail("--base, --commit and --uncommitted are mutually exclusive")
	}
	if prompt != "" && promptFile != "" {
		return fail("--prompt and --prompt-file are mutually exclusive")
	}
	if promptFile != "" {
		b, err := os.ReadFile(promptFile)
		if err != nil {
			return fail("%v", err)
		}
		prompt = string(b)
		if strings.TrimSpace(prompt) == "" {
			return fail("prompt file %s is empty", promptFile)
		}
	}
	if prompt == "" && scopes == 0 {
		return fail("give --prompt/--prompt-file, a review scope (--base, --commit, --uncommitted), or both")
	}
	if prompt == "" && b == agent.Claude {
		prompt = defaultReviewBrief
		if o.Write {
			prompt += defaultFixLine
		}
	}
	if len(paths) > 0 && (prompt == "" || scopes == 0) {
		return fail("--path needs both a prompt and a scope (codex's built-in reviewer cannot narrow paths)")
	}
	if o.Timeout <= 0 {
		return fail("--timeout must be positive")
	}
	if o.MaxBudgetUSD < 0 || math.IsNaN(o.MaxBudgetUSD) || math.IsInf(o.MaxBudgetUSD, 0) {
		return fail("--max-budget-usd must be a finite non-negative number")
	}
	if o.Stall < 0 {
		return fail("--stall must not be negative")
	}
	if maxBytes <= 0 {
		return fail("--max-prompt-bytes must be positive")
	}
	if o.Dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fail("%v", err)
		}
		o.Dir = wd
	}
	if fi, err := os.Stat(o.Dir); err != nil || !fi.IsDir() {
		return fail("--dir %s is not a directory", o.Dir)
	}
	o.Bin = os.Getenv("AGENTFLOW_" + strings.ToUpper(name))

	if prompt != "" {
		built, err := agent.BuildPrompt(o.Dir, prompt, o.Scope, maxBytes)
		if err != nil {
			return fail("%v", err)
		}
		o.Prompt = built
	} else if err := agent.ValidateScope(o.Dir, o.Scope); err != nil {
		return fail("%v", err)
	}
	if o.OutDir == "" {
		d, err := os.MkdirTemp("", "agentflow-"+name+"-")
		if err != nil {
			return fail("%v", err)
		}
		o.OutDir = d
	}
	unlock, err := lockOutDir(o.OutDir, name)
	if err != nil {
		return fail("%v", err)
	}
	defer unlock()

	res, err := agent.Run(ctx, o)
	if err != nil {
		return fail("%v", err)
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	out = append(out, '\n')
	_ = os.WriteFile(filepath.Join(o.OutDir, "result.json"), out, 0o644)
	_, _ = stdout.Write(out)
	if res.Status != agent.StatusOK {
		fmt.Fprintf(stderr, "agentflow %s: %s: %s (logs: %s)\n", name, res.Status, res.Error, o.OutDir)
	}
	code, ok := exitCodes[res.Status]
	if !ok {
		fmt.Fprintf(stderr, "agentflow %s: unknown result status %q\n", name, res.Status)
		return exitCodes[agent.StatusFailed]
	}
	return code
}

// lockOutDir prevents concurrent runs from truncating each other's event log
// or supplying the final.md that another run mistakes for its own answer.
func lockOutDir(dir, agentName string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("--out %s is already in use by another agentflow %s run", dir, agentName)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
