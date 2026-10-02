package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/ship"
)

const shipUsage = `agentflow ship verify <service.env> --sha REV [flags]

Wait until a deployed service runs REV, read through agentctl's /agent/version.

  --sha REV          commit to expect: a hex SHA (7+ chars) or any git rev
                     (origin/main, HEAD, a tag), resolved in --repo
  --repo DIR         git checkout used to resolve REV (default: current dir);
                     required explicitly with --contains
  --timeout DUR      give up after DUR (default 40m)
  --interval DUR     delay between checks (default 30s)
  --once             check once and exit (0 deployed, 2 not yet)
  --contains         also accept a running descendant of REV; use only for
                     forward deploys, not when verifying a rollback

A short SHA matches a full one. Transient fetch failures while the service
restarts are retried; a version with no recognizable commit fails at once.

Output: a JSON result on stdout. Exit codes: 0 deployed · 1 usage/agentctl
error · 2 not deployed (--once) · 3 version unreadable · 124 timeout
· 130 interrupted.
`

var shipExitCodes = map[ship.Status]int{
	ship.StatusDeployed:    0,
	ship.StatusAgentctl:    1,
	ship.StatusNotDeployed: 2,
	ship.StatusUnreadable:  3,
	ship.StatusTimeout:     124,
	ship.StatusInterrupted: 130,
}

func runShip(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprint(stdout, shipUsage)
		return 0
	}
	if args[0] != "verify" {
		fmt.Fprintf(stderr, "agentflow ship: unknown subcommand %q\n\n%s", args[0], shipUsage)
		return 1
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow ship verify: "+format+"\n", a...)
		return 1
	}
	fs := flag.NewFlagSet("agentflow ship verify", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		rev string
		o   ship.Options
	)
	fs.StringVar(&rev, "sha", "", "")
	fs.StringVar(&o.Repo, "repo", "", "")
	fs.DurationVar(&o.Timeout, "timeout", 40*time.Minute, "")
	fs.DurationVar(&o.Interval, "interval", 30*time.Second, "")
	fs.BoolVar(&o.Once, "once", false, "")
	fs.BoolVar(&o.Contains, "contains", false, "")
	pos, err := parseInterleaved(fs, args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, shipUsage)
			return 0
		}
		return fail("%v (see: agentflow ship --help)", err)
	}
	if len(pos) != 1 {
		return fail("want exactly one service.env (e.g. recursivecx.prod), got %d", len(pos))
	}
	o.Service = pos[0]
	if rev == "" {
		return fail("--sha is required")
	}
	if o.Timeout <= 0 || o.Interval <= 0 {
		return fail("--timeout and --interval must be positive")
	}

	repoGiven := o.Repo != ""
	if !repoGiven {
		o.Repo, _ = os.Getwd()
	}
	inRepo := gitWorkTree(ctx, o.Repo)
	if repoGiven && !inRepo {
		return fail("--repo %s is not a git work tree", o.Repo)
	}
	if !inRepo {
		o.Repo = "" // no ancestry check outside a checkout
	}
	if o.Contains && (!repoGiven || !inRepo) {
		return fail("--contains needs an explicit git checkout (use --repo DIR)")
	}
	if ship.IsHexSHA(rev) {
		// A hex SHA is compared as text. It is expanded when this clone knows
		// it, but a merge not fetched yet, or a SHA from another service's
		// repo, must not turn into an error or a false timeout.
		o.Expected = strings.ToLower(rev)
		if inRepo {
			if full, err := resolveCommit(ctx, o.Repo, rev); err == nil {
				o.Expected = full
			}
		}
	} else {
		if !inRepo {
			return fail("--sha %q is not a hex SHA and there is no git checkout to resolve it (use --repo)", rev)
		}
		out, err := resolveCommit(ctx, o.Repo, rev)
		if err != nil {
			return fail("cannot resolve --sha %q in %s", rev, o.Repo)
		}
		o.Expected = out
	}

	bin, err := agentctlBin()
	if err != nil {
		return fail("%v", err)
	}
	o.Fetch = ship.AgentctlFetch(bin, 20*time.Second)

	res := ship.Verify(ctx, o)
	out, _ := json.MarshalIndent(res, "", "  ")
	_, _ = stdout.Write(append(out, '\n'))
	code, ok := shipExitCodes[res.Status]
	if !ok {
		fmt.Fprintf(stderr, "agentflow ship verify: unknown result status %q\n", res.Status)
		return 1
	}
	if code != 0 {
		fmt.Fprintf(stderr, "agentflow ship verify: %s: %s\n", res.Status, res.Error)
	}
	return code
}

// parseInterleaved parses flags that may come before or after positionals
// (`ship verify svc.env --sha X` and `ship verify --sha X svc.env`).
func parseInterleaved(fs *flag.FlagSet, args []string) ([]string, error) {
	var afterDash []string
	for i, arg := range args {
		if arg == "--" {
			afterDash = args[i+1:]
			args = args[:i]
			break
		}
	}
	var pos []string
	for {
		if err := fs.Parse(args); err != nil {
			return nil, err
		}
		args = fs.Args()
		if len(args) == 0 {
			return append(pos, afterDash...), nil
		}
		pos = append(pos, args[0])
		args = args[1:]
	}
}

// agentctlBin finds agentctl: $AGENTCTL, then PATH, then ~/bin.
func agentctlBin() (string, error) {
	if b := os.Getenv("AGENTCTL"); b != "" {
		return b, nil
	}
	if b, err := exec.LookPath("agentctl"); err == nil {
		return b, nil
	}
	if home, err := os.UserHomeDir(); err == nil {
		b := filepath.Join(home, "bin", "agentctl")
		if fi, err := os.Stat(b); err == nil && !fi.IsDir() {
			return b, nil
		}
	}
	return "", errors.New("agentctl not found: put it on PATH or set AGENTCTL")
}

func gitWorkTree(ctx context.Context, dir string) bool {
	out, err := exec.CommandContext(ctx, "git", "-C", dir, "rev-parse", "--is-inside-work-tree").Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

func resolveCommit(ctx context.Context, repo, rev string) (string, error) {
	out, err := exec.CommandContext(ctx, "git", "-C", repo, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}").Output()
	return strings.ToLower(strings.TrimSpace(string(out))), err
}
