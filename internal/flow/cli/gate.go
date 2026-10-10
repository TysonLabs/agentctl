package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/TysonLabs/agentctl/internal/configpath"
	"github.com/TysonLabs/agentctl/internal/flow/gate"
)

const gateUsage = `agentflow gate [--project P] [--dir D] [--only s1,s2] [--force] [--out DIR]
agentflow gate --check [--project P] [--dir D] [--tree HASH | --rev REV]

Run a project's gate: the ordered steps in its [name.gate] table in
services.toml (edit it with agentcfg gate <name>):

  [myproj.meta]
  repo = "~/src/myproj"
  [myproj.gate]
  lock = "myproj-build"            # optional named lock, held for the whole run
  [[myproj.gate.steps]]
  name = "fmt"
  run  = "cargo fmt --all --check" # /bin/sh -c from the work tree root
  [[myproj.gate.steps]]
  name = "suite"
  run  = "scripts/gate.sh"
  timeout = "60m"                  # default 30m; the process group is killed
  stop_on_fail = true              # skip the later steps if this one fails

  --project P      the project (default: the one whose meta.repo is this
                   checkout, or another worktree of the same repo)
  --dir D          the checkout to run in (default: current directory)
  --only a,b       run only these steps (in configured order)
  --force          run every step, even ones that passed on this tree
  --out DIR        step logs, <step>.log (default: a new temp dir)
  --config PATH    services.toml (default: $AGENTCTL_CONFIG, then
                   ~/.config/agentctl/services.toml)

Every step runs, so one run reports every failure, unless a failed step has
stop_on_fail. stdin is /dev/null. Each step leaves a receipt in
<git-common-dir>/agentflow/gate/receipts.json keyed by the work tree's exact
content (git write-tree of the files on disk, so uncommitted and untracked
changes count). A rerun on the same tree skips steps that passed with the
same run string; any change to the tree reruns them all.

--check runs nothing and writes nothing: it exits 0 only if every step has
a passing receipt, with its current run string, for the work tree (or
--tree HASH, or --rev REV's tree). "missing" lists the steps that do not.

Output: a JSON result on stdout; progress on stderr.
Exit codes: 0 all passed (cached included) · 1 usage/precondition · 2 a step
failed (--check: a step lacks a passing receipt) · 124 a step timed out (and
none failed) · 130 interrupted.
`

var gateExitCodes = map[gate.Status]int{
	gate.StatusPassed:      0,
	gate.StatusError:       1,
	gate.StatusFailed:      2,
	gate.StatusMissing:     2,
	gate.StatusTimeout:     124,
	gate.StatusInterrupted: 130,
}

func runGate(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow gate: "+format+"\n", a...)
		return 1
	}
	fs := flag.NewFlagSet("agentflow gate", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		o            gate.Options
		only, config string
		check        bool
		tree, rev    string
	)
	fs.StringVar(&o.Project, "project", "", "")
	fs.StringVar(&o.Dir, "dir", "", "")
	fs.StringVar(&only, "only", "", "")
	fs.BoolVar(&o.Force, "force", false, "")
	fs.StringVar(&o.OutDir, "out", "", "")
	fs.StringVar(&config, "config", "", "")
	fs.BoolVar(&check, "check", false, "")
	fs.StringVar(&tree, "tree", "", "")
	fs.StringVar(&rev, "rev", "", "")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, gateUsage)
			return 0
		}
		return fail("%v (see: agentflow gate --help)", err)
	}
	if fs.NArg() > 0 {
		return fail("unexpected argument %q (see: agentflow gate --help)", fs.Arg(0))
	}
	set := map[string]bool{}
	fs.Visit(func(f *flag.Flag) { set[f.Name] = true })
	if check {
		for _, f := range []string{"only", "force", "out"} {
			if set[f] {
				return fail("--%s does not apply to --check", f)
			}
		}
		if tree != "" && rev != "" {
			return fail("--tree and --rev are mutually exclusive")
		}
	} else if set["tree"] || set["rev"] {
		return fail("--tree and --rev need --check")
	}
	if set["only"] {
		for _, s := range strings.Split(only, ",") {
			if s = strings.TrimSpace(s); s != "" {
				o.Only = append(o.Only, s)
			}
		}
		if len(o.Only) == 0 {
			return fail("--only needs at least one step name")
		}
	}
	o.DirGiven = o.Dir != ""
	if !o.DirGiven {
		wd, err := os.Getwd()
		if err != nil {
			return fail("%v", err)
		}
		o.Dir = wd
	} else if fi, err := os.Stat(o.Dir); err != nil || !fi.IsDir() {
		return fail("--dir %s is not a directory", o.Dir)
	}
	o.ConfigPath = configpath.Resolve(config)
	o.Stderr = stderr

	var (
		out    any
		status gate.Status
		msg    string
	)
	if check {
		res := gate.Check(ctx, o, tree, rev)
		out, status, msg = res, res.Status, res.Error
		if res.Status == gate.StatusMissing {
			msg = "no passing receipt for: " + strings.Join(res.Missing, ", ")
		}
	} else {
		if o.OutDir == "" {
			d, err := os.MkdirTemp("", "agentflow-gate-")
			if err != nil {
				return fail("%v", err)
			}
			o.OutDir = d
		}
		unlock, err := lockOutDir(o.OutDir, "gate")
		if err != nil {
			return fail("%v", err)
		}
		defer unlock()
		res := gate.Run(ctx, o)
		out, status, msg = res, res.Status, res.Error
		if msg == "" && !res.OK {
			var bad []string
			for _, s := range res.Steps {
				if s.Status == gate.StepFailed || s.Status == gate.StepTimeout {
					bad = append(bad, s.Name+" ("+s.Status+", log "+s.Log+")")
				}
			}
			msg = strings.Join(bad, "; ")
		}
	}
	b, _ := json.MarshalIndent(out, "", "  ")
	_, _ = stdout.Write(append(b, '\n'))
	code, ok := gateExitCodes[status]
	if !ok {
		fmt.Fprintf(stderr, "agentflow gate: unknown result status %q\n", status)
		return 1
	}
	if code != 0 {
		fmt.Fprintf(stderr, "agentflow gate: %s: %s\n", status, msg)
	}
	return code
}
