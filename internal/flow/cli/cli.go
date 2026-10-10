// Package cli implements the agentflow command dispatch. Exit codes are
// assigned in this package and nowhere else, one table per command
// (exitCodes for codex, coderabbitExitCodes for coderabbit, shipExitCodes
// for ship verify, prExitCodes for pr wait, replyExitCodes for pr reply,
// prMergeExitCodes for pr merge, lessonsExitCodes for lessons,
// redcheckExitCodes for redcheck). 0 is success, 1 a
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
  agentflow coderabbit [flags]                 run the CodeRabbit CLI's local review safely;
                                               JSON findings (see: agentflow coderabbit --help)
  agentflow ship verify <svc.env> --sha REV    wait until a deployed service runs REV
  agentflow ship announce <svc.env> --verified FILE ...
                                               post a verified deploy to Slack
                                               (see: agentflow ship --help)
  agentflow pr wait <number> [--repo O/N]      wait for CodeRabbit's review of the PR head;
                                               list open threads (see: agentflow pr --help)
  agentflow pr thread <thread-id> [--repo O/N] print one review thread's comments (read-only)
  agentflow pr reply <thread-id> --fixed SHA --note TEXT | --keep REASON
                                               reply to a review thread, then resolve it
  agentflow pr merge <number> [--sync-branch B] merge a ready PR pinned to its head;
                                               report the merge sha
  agentflow worktree new <branch>|--scratch    create a worktree under .claude/worktrees
  agentflow worktree done <branch|path>        remove a merged, clean, unused worktree
  agentflow worktree sweep [--yes]             list (or remove) every such worktree
                                               (see: agentflow worktree --help)
  agentflow lessons <subcommand>               code-review lessons: brief, bump, add, seen,
                                               search, triage, retire, stats
                                               (see: agentflow lessons --help)
  agentflow redcheck --test CMD --commit SHA|--base REF|--uncommitted
                                               prove a fix's new test fails without the fix
                                               (see: agentflow redcheck --help)
  agentflow version                           print agentflow's own version

Output: every command that prints a JSON result also takes --format json|text.
json (the default) is the stable API. text is a short summary for reading:
"<command>: <status> (exit N)", then one "key: value" per line, then lists as
"- ..." lines. Exit codes are the same in both formats, and a command that
saves its JSON (<out>/result.json) saves it in both. Text from reviews and
other tools is printed with control characters escaped (\x1b, \u202e).

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
  --lessons TOPICS        append the learned-checks section for these lesson
                          topics (comma-separated) to the prompt; needs a prompt
                          for codex (claude's default brief counts)
  --lessons-repo NAME     rank that repo's lessons first (default: the name of
                          --dir's origin remote)
  --lessons-dir DIR       the lessons folder (default: $AGENTFLOW_LESSONS_DIR)
  --format json|text      result on stdout as JSON (default) or a text summary
  --protocol fix|review   wrap the brief in the built-in review protocol: rules
                          before it, the fixed output format after it. fix needs
                          --write; review must be read-only. The brief is then
                          optional with a scope. The JSON adds "findings",
                          parsed from final.md (null plus a warning if it does
                          not follow the format; the exit code is unchanged)
  --test-cmd CMD          fix protocol: a targeted test command the reviewer
                          runs after its fixes (repeatable)

  A scope with a prompt inlines the scoped diff under your prompt. A scope
  with no prompt runs codex's built-in reviewer (codex exec review); claude
  has none, so it gets a short default review brief with the diff inlined.
  stdin is never read and never passed on to the agent.

  Sandboxing: codex runs with -c sandbox_mode=read-only (or workspace-write).
  claude runs --restricted with only Read, Grep and Glob; in fix mode it also
  gets Edit, Write and Bash, with Bash in Claude Code's sandbox (writes only
  under --dir, no network). Prompt-driven runs are told they are a sub-agent:
  do the task, report, stop, and start no other agents.

Output: the JSON result on stdout (also saved as <out>/result.json, in both
formats); the review itself is in the file named by "final".

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
	case "coderabbit":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runCodeRabbit(ctx, args[1:], stdout, stderr)
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
	case "lessons":
		return runLessons(args[1:], stdout, stderr)
	case "redcheck":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runRedcheck(ctx, args[1:], stdout, stderr)
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
		lessonTops string
		lessonRepo string
		lessonDir  string
		protoFlag  string
		testCmds   pathList
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
	fs.StringVar(&lessonTops, "lessons", "", "")
	fs.StringVar(&lessonRepo, "lessons-repo", "", "")
	fs.StringVar(&lessonDir, "lessons-dir", "", "")
	format := formatFlag(fs)
	fs.StringVar(&protoFlag, "protocol", "", "")
	fs.Var(&testCmds, "test-cmd", "")
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
	proto, err := protocolFlags(protoFlag, testCmds, o.Write)
	if err != nil {
		return fail("%v", err)
	}
	if proto != agent.ProtocolNone {
		// Protocol first, then the brief and the output format; the
		// learned-checks section and the diff follow below.
		prompt = agent.WrapBrief(agent.ProtocolPrompt{Mode: proto, TestCmds: testCmds, Lessons: lessonTops != ""}, prompt)
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

	if lessonTops == "" && (lessonRepo != "" || lessonDir != "") {
		return fail("--lessons-repo and --lessons-dir need --lessons")
	}
	if lessonTops != "" {
		if prompt == "" {
			return fail("--lessons needs --prompt, --prompt-file or --protocol (codex's built-in reviewer takes no extra instructions)")
		}
		section, err := lessonsSection(o.Dir, lessonDir, lessonTops, lessonRepo)
		if err != nil {
			return fail("--lessons: %v", err)
		}
		// Before BuildPrompt, so the diff stays last and the size cap counts the section.
		prompt = strings.TrimRight(prompt, "\n") + "\n\n" + section
	}

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
	res.SetFindings(proto)
	out, _ := json.MarshalIndent(res, "", "  ")
	out = append(out, '\n')
	_ = os.WriteFile(filepath.Join(o.OutDir, "result.json"), out, 0o644)
	code, ok := exitCodes[res.Status]
	if !ok {
		code = exitCodes[agent.StatusFailed]
	}
	emit(stdout, *format, out, func() string { return agentText(name, res, code, o.OutDir) })
	if res.Status != agent.StatusOK {
		fmt.Fprintf(stderr, "agentflow %s: %s: %s (logs: %s)\n", name, res.Status, res.Error, o.OutDir)
	}
	if !ok {
		fmt.Fprintf(stderr, "agentflow %s: unknown result status %q\n", name, res.Status)
	}
	return code
}

// protocolFlags checks --protocol and --test-cmd against --write. The mode is
// explicit, not implied by --write, so a dropped --write is refused instead of
// silently turning a fix run into a review.
func protocolFlags(mode string, testCmds []string, write bool) (agent.Protocol, error) {
	p, err := agent.ParseProtocol(mode)
	if err != nil {
		return "", err
	}
	switch {
	case p == agent.ProtocolFix && !write:
		return "", errors.New("--protocol fix needs --write: the reviewer must be able to edit files")
	case p == agent.ProtocolReview && write:
		return "", errors.New("--protocol review is read-only: drop --write, or use --protocol fix")
	case len(testCmds) > 0 && p != agent.ProtocolFix:
		return "", errors.New("--test-cmd needs --protocol fix: a read-only reviewer runs no tests")
	}
	for _, c := range testCmds {
		if strings.TrimSpace(c) == "" || strings.ContainsAny(c, "`\n\r") {
			return "", fmt.Errorf("--test-cmd %q must be one non-empty line without backticks", c)
		}
	}
	return p, nil
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
