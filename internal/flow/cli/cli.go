// Package cli implements the agentflow command dispatch. Exit codes are
// assigned in this package and nowhere else, one table per command
// (exitCodes for codex, shipExitCodes for ship verify). 0 is success, 1 a
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
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/codex"
)

// Version is injected from main via ldflags.
var Version = "dev"

const usage = `agentflow — small, single-purpose workflow commands for coding agents

Usage:
  agentflow codex [flags]                      run codex exec safely and report a JSON result
  agentflow ship verify <svc.env> --sha REV    wait until a deployed service runs REV
                                               (see: agentflow ship --help)
  agentflow version                            print agentflow's own version

codex flags:
  --dir DIR               repository to work in (default: current directory)
  --prompt TEXT           prompt text
  --prompt-file FILE      prompt read from FILE
  --base REF              review the diff REF...HEAD
  --commit SHA            review one commit
  --uncommitted           review staged, unstaged and untracked changes
  --path PATHSPEC         limit the reviewed diff (repeatable; needs a prompt)
  --write                 workspace-write sandbox, for fix mode (default: read-only)
  --model MODEL           codex model
  --timeout DUR           hard cap on the run (default 40m)
  --stall DUR             kill after this long with no activity (default 10m; 0 disables)
  --out DIR               where to write prompt.md, final.md, events.jsonl, stderr.log
                          and result.json (default: a new temp dir)
  --max-prompt-bytes N    refuse prompts over N bytes (default 80000)

  A scope with no prompt runs codex's built-in reviewer (codex exec review).
  A scope with a prompt inlines the scoped diff under your prompt.
  stdin is never read and never passed on to codex.

Output: the JSON result on stdout (also saved as <out>/result.json); the
review itself is in the file named by "final".

Exit codes: 0 ok · 1 usage/precondition · 3 codex failed · 4 no final answer
            5 rate/usage limited · 124 timeout · 125 stalled · 130 interrupted
`

var exitCodes = map[codex.Status]int{
	codex.StatusOK:          0,
	codex.StatusFailed:      3,
	codex.StatusNoAnswer:    4,
	codex.StatusRateLimited: 5,
	codex.StatusTimeout:     124,
	codex.StatusStalled:     125,
	codex.StatusInterrupted: 130,
}

// Run executes agentflow with args (without the program name).
func Run(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprint(stderr, usage)
		return 1
	}
	switch args[0] {
	case "codex":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runCodex(ctx, args[1:], stdout, stderr)
	case "ship":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return runShip(ctx, args[1:], stdout, stderr)
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

func runCodex(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("agentflow codex", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		o          codex.Options
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
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow codex: "+format+"\n", a...)
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
	if len(paths) > 0 && (prompt == "" || scopes == 0) {
		return fail("--path needs both a prompt and a scope (codex's built-in reviewer cannot narrow paths)")
	}
	if o.Timeout <= 0 {
		return fail("--timeout must be positive")
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
	o.Bin = os.Getenv("AGENTFLOW_CODEX")

	if prompt != "" {
		built, err := codex.BuildPrompt(o.Dir, prompt, o.Scope, maxBytes)
		if err != nil {
			return fail("%v", err)
		}
		o.Prompt = built
	} else if err := codex.ValidateScope(o.Dir, o.Scope); err != nil {
		return fail("%v", err)
	}
	if o.OutDir == "" {
		d, err := os.MkdirTemp("", "agentflow-codex-")
		if err != nil {
			return fail("%v", err)
		}
		o.OutDir = d
	}
	unlock, err := lockOutDir(o.OutDir)
	if err != nil {
		return fail("%v", err)
	}
	defer unlock()

	res, err := codex.Run(ctx, o)
	if err != nil {
		return fail("%v", err)
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	out = append(out, '\n')
	_ = os.WriteFile(filepath.Join(o.OutDir, "result.json"), out, 0o644)
	_, _ = stdout.Write(out)
	if res.Status != codex.StatusOK {
		fmt.Fprintf(stderr, "agentflow codex: %s: %s (logs: %s)\n", res.Status, res.Error, o.OutDir)
	}
	code, ok := exitCodes[res.Status]
	if !ok {
		fmt.Fprintf(stderr, "agentflow codex: unknown result status %q\n", res.Status)
		return exitCodes[codex.StatusFailed]
	}
	return code
}

// lockOutDir prevents concurrent runs from truncating each other's event log
// or supplying the final.md that another run mistakes for its own answer.
func lockOutDir(dir string) (func(), error) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	f, err := os.Open(dir)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("--out %s is already in use by another agentflow codex run", dir)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, nil
}
