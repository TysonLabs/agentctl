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
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/pr"
)

const prUsage = `agentflow pr wait <number> [flags]

Wait until CodeRabbit has reviewed the pull request's CURRENT head commit,
then report the CodeRabbit review threads that are still open. Run it in the
background; it exits when there is something to do.

  --repo OWNER/NAME  GitHub repository (default: the current directory's,
                     from gh; never guessed)
  --timeout DUR      give up after DUR (default 45m)
  --interval DUR     delay between checks (default 30s)
  --once             check once and exit (2 = not reviewed yet)

"Reviewed" means CodeRabbit's summary comment covers the head commit and no
review is in progress. An older round's review, the "CodeRabbit" commit
status and empty-body bot reviews are ignored: they mislead.

Output: a JSON result on stdout. "open_threads" lists each open CodeRabbit
thread with its GraphQL id (for addPullRequestReviewThreadReply and
resolveReviewThread), path, line, url and the first line of its comment;
"next" says what to do.

Exit codes: 0 reviewed, no open threads · 1 usage or gh error · 2 not
reviewed yet (--once) · 3 review skipped (draft, base, paused) · 4 rate-limited
· 5 PR closed before its head was reviewed · 10 reviewed, open threads
· 124 timeout · 130 interrupted.
`

var prExitCodes = map[pr.Status]int{
	pr.StatusClean:       0,
	pr.StatusGHError:     1,
	pr.StatusWaiting:     2,
	pr.StatusSkipped:     3,
	pr.StatusRateLimited: 4,
	pr.StatusClosed:      5,
	pr.StatusOpenThreads: 10,
	pr.StatusTimeout:     124,
	pr.StatusInterrupted: 130,
}

var repoRe = regexp.MustCompile(`^[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$`)

func runPR(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprint(stdout, prUsage)
		return 0
	}
	if args[0] != "wait" {
		fmt.Fprintf(stderr, "agentflow pr: unknown subcommand %q\n\n%s", args[0], prUsage)
		return 1
	}
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow pr wait: "+format+"\n", a...)
		return 1
	}
	fs := flag.NewFlagSet("agentflow pr wait", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o pr.Options
	fs.StringVar(&o.Repo, "repo", "", "")
	fs.DurationVar(&o.Timeout, "timeout", 45*time.Minute, "")
	fs.DurationVar(&o.Interval, "interval", 30*time.Second, "")
	fs.BoolVar(&o.Once, "once", false, "")
	pos, err := parseInterleaved(fs, args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, prUsage)
			return 0
		}
		return fail("%v (see: agentflow pr --help)", err)
	}
	if len(pos) != 1 {
		return fail("want exactly one PR number, got %d", len(pos))
	}
	n, err := strconv.Atoi(strings.TrimPrefix(pos[0], "#"))
	if err != nil || n <= 0 {
		return fail("PR number %q is not a positive integer", pos[0])
	}
	o.PR = n
	if o.Timeout <= 0 || o.Interval <= 0 {
		return fail("--timeout and --interval must be positive")
	}
	bin := os.Getenv("AGENTFLOW_GH")
	if bin == "" {
		if bin, err = exec.LookPath("gh"); err != nil {
			return fail("gh not found: put it on PATH or set AGENTFLOW_GH")
		}
	}
	o.GH = pr.GHCLI(bin, 60*time.Second)
	if o.Repo == "" {
		// Never assume a repo: a wrong default once watched another repo's
		// PR with the same number.
		out, err := o.GH(ctx, "repo", "view", "--json", "nameWithOwner")
		if err != nil {
			return fail("no --repo and the current directory has no GitHub repo: %v", err)
		}
		var rv struct {
			NameWithOwner string `json:"nameWithOwner"`
		}
		if json.Unmarshal(out, &rv) != nil || rv.NameWithOwner == "" {
			return fail("no --repo and gh repo view gave no name")
		}
		o.Repo = rv.NameWithOwner
	}
	if !repoRe.MatchString(o.Repo) {
		return fail("--repo %q is not OWNER/NAME", o.Repo)
	}

	res := pr.Wait(ctx, o)
	out, _ := json.MarshalIndent(res, "", "  ")
	_, _ = stdout.Write(append(out, '\n'))
	code, ok := prExitCodes[res.Status]
	if !ok {
		fmt.Fprintf(stderr, "agentflow pr wait: unknown result status %q\n", res.Status)
		return 1
	}
	if code != 0 {
		msg := res.Next
		if res.Error != "" {
			msg = res.Error
		}
		fmt.Fprintf(stderr, "agentflow pr wait: %s: %s\n", res.Status, msg)
	}
	return code
}
