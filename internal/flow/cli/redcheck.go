package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/redcheck"
)

const redcheckUsage = `agentflow redcheck --test CMD (--commit SHA | --base REF | --uncommitted) [flags]

Proves that a test added with a fix fails without the fix. It builds the
change's "after" state in a temporary detached worktree (your checkout is
never written), reverts the changed SOURCE files to the "before" state while
keeping TEST files at "after", and runs --test there: it must fail (red).
Then it restores the source and runs --test again: it must pass (green).

Scopes (exactly one):
  --commit SHA      the commit vs its first parent
  --base REF        committed HEAD vs merge-base(REF, HEAD)
  --uncommitted     HEAD plus staged, unstaged and untracked changes vs HEAD

Flags:
  --test CMD        shell command (/bin/sh -c) run at the worktree root, stdin
                    /dev/null; AGENTFLOW_REDCHECK_PHASE is "red" or "green"
  --test-paths GLOB extra test-file patterns (repeatable)
  --keep GLOB       source files to keep at "after" too, e.g. new fixtures
                    (repeatable)
  --no-green        skip the green run
  --no-splice       report a Rust file whose inline #[cfg(test)] module
                    changed as inconclusive instead of splicing it
  --clone-target D  copy-on-write clone build directory D into the worktree
                    (cp -c on macOS, cp --reflink=always on Linux; skipped
                    with a note when that is not possible)
  --dir DIR         repository (default: current directory)
  --timeout DUR     cap on each test run (default 20m)
  --out DIR         where red.log, green.log and result.json go
                    (default: a new temp dir)

Test files: *_test.go, test_*.py, *_test.py, conftest.py, *.test.{js,jsx,ts,tsx},
*.spec.{js,jsx,ts,tsx}, and anything under a tests/, test/, __tests__/, spec/
or testdata/ directory. A pattern without "/" matches the base name; with
"/", the repo-relative path, where ** is any number of directories.

A reverted source file whose own diff adds or changes test code (Rust
#[test]/#[cfg(test)], Python def test_, JS test()/it()) makes the result
inconclusive, because reverting it also reverts the test. Exception: a Rust
file whose inline #[cfg(test)] module (at the end of the file) changed is
spliced, before's code with after's test module, and listed in "spliced".

Build caches: the environment is passed through. For Rust, set
CARGO_TARGET_DIR to a separate directory, or use --clone-target target, so the
runs never rebuild into your main target directory.

Output: JSON on stdout (also <out>/result.json): status, red/green (exit,
secs, log), reverted, spliced, kept_tests, kept, test_in_source, before, after.

Exit codes: 0 red (and green, unless --no-green) · 1 usage/precondition/error
            2 not red: passes without the fix · 3 inconclusive
            4 green failed: fails with the fix too · 124 timeout · 130 interrupted
`

// redcheckExitCodes is redcheck's own table (see the package comment).
var redcheckExitCodes = map[redcheck.Status]int{
	redcheck.StatusRed:          0,
	redcheck.StatusError:        1,
	redcheck.StatusNotRed:       2,
	redcheck.StatusInconclusive: 3,
	redcheck.StatusGreenFailed:  4,
	redcheck.StatusTimeout:      124,
	redcheck.StatusInterrupted:  130,
}

func runRedcheck(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow redcheck: "+format+"\n", a...)
		return 1
	}
	fs := flag.NewFlagSet("agentflow redcheck", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		o               redcheck.Options
		testPaths, keep pathList
	)
	fs.StringVar(&o.Test, "test", "", "")
	fs.StringVar(&o.Scope.Commit, "commit", "", "")
	fs.StringVar(&o.Scope.Base, "base", "", "")
	fs.BoolVar(&o.Scope.Uncommitted, "uncommitted", false, "")
	fs.Var(&testPaths, "test-paths", "")
	fs.Var(&keep, "keep", "")
	fs.BoolVar(&o.NoGreen, "no-green", false, "")
	fs.BoolVar(&o.NoSplice, "no-splice", false, "")
	fs.StringVar(&o.CloneTarget, "clone-target", "", "")
	fs.StringVar(&o.Dir, "dir", "", "")
	fs.DurationVar(&o.Timeout, "timeout", 20*time.Minute, "")
	fs.StringVar(&o.OutDir, "out", "", "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, redcheckUsage)
			return 0
		}
		return fail("%v (see: agentflow redcheck --help)", err)
	}
	if fs.NArg() > 0 {
		return fail("unexpected argument %q: pass the test command with --test", fs.Arg(0))
	}
	if o.Test == "" {
		return fail("--test is required")
	}
	scopes := 0
	for _, set := range []bool{o.Scope.Commit != "", o.Scope.Base != "", o.Scope.Uncommitted} {
		if set {
			scopes++
		}
	}
	if scopes != 1 {
		return fail("pass exactly one of --commit, --base and --uncommitted")
	}
	if o.Timeout <= 0 {
		return fail("--timeout must be positive")
	}
	for _, list := range []struct {
		name string
		in   []string
		out  *[]string
	}{{"--test-paths", testPaths, &o.TestPaths}, {"--keep", keep, &o.Keep}} {
		for _, p := range list.in {
			n, err := redcheck.NormalizePattern(p)
			if err != nil {
				return fail("%s: %v", list.name, err)
			}
			*list.out = append(*list.out, n)
		}
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
	o.Git = os.Getenv("AGENTFLOW_GIT")
	if o.OutDir == "" {
		d, err := os.MkdirTemp("", "agentflow-redcheck-out-")
		if err != nil {
			return fail("%v", err)
		}
		o.OutDir = d
	} else if abs, err := filepath.Abs(o.OutDir); err == nil {
		o.OutDir = abs
	}
	unlock, err := lockOutDir(o.OutDir, "redcheck")
	if err != nil {
		return fail("%v", err)
	}
	defer unlock()

	res := redcheck.Run(ctx, o)
	out, _ := json.MarshalIndent(res, "", "  ")
	out = append(out, '\n')
	_ = os.WriteFile(filepath.Join(o.OutDir, "result.json"), out, 0o644)
	_, _ = stdout.Write(out)
	if res.Status != redcheck.StatusRed {
		msg := res.Error
		if msg == "" {
			msg = res.Next
		}
		fmt.Fprintf(stderr, "agentflow redcheck: %s: %s\n", res.Status, msg)
	}
	if res.CleanupError != "" {
		fmt.Fprintf(stderr, "agentflow redcheck: cleanup: %s\n", res.CleanupError)
	}
	code, ok := redcheckExitCodes[res.Status]
	if !ok {
		fmt.Fprintf(stderr, "agentflow redcheck: unknown result status %q\n", res.Status)
		return 1
	}
	return code
}
