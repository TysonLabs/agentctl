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
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/pr"
)

var threadExitCodes = map[pr.ThreadStatus]int{
	pr.ThreadOK:          0,
	pr.ThreadGHError:     1,
	pr.ThreadRefused:     2,
	pr.ThreadInterrupted: 130,
}

// runPRThread prints one review thread's comments. It is read-only.
func runPRThread(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow pr thread: "+format+"\n", a...)
		return 1
	}
	fs := flag.NewFlagSet("agentflow pr thread", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o pr.ThreadOptions
	fs.StringVar(&o.Repo, "repo", "", "")
	format := formatFlag(fs)
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, prUsage)
			return 0
		}
		return fail("%v (see: agentflow pr --help)", err)
	}
	if len(pos) != 1 {
		return fail("want exactly one thread id, got %d", len(pos))
	}
	o.Thread = pos[0]
	if !pr.ValidThreadID(o.Thread) {
		return fail("thread id %q is not a review-thread node id (PRRT_…, as agentflow pr wait prints it)", o.Thread)
	}
	if o.Repo != "" && !repoRe.MatchString(o.Repo) {
		return fail("--repo %q is not OWNER/NAME", o.Repo)
	}
	bin := os.Getenv("AGENTFLOW_GH")
	if bin == "" {
		if bin, err = exec.LookPath("gh"); err != nil {
			return fail("gh not found: put it on PATH or set AGENTFLOW_GH")
		}
	}
	o.GH = pr.GHCLI(bin, 60*time.Second)

	res := pr.ReadThread(ctx, o)
	out, _ := json.MarshalIndent(res, "", "  ")
	code, ok := threadExitCodes[res.Status]
	if !ok {
		code = 1
	}
	emit(stdout, *format, append(out, '\n'), func() string { return threadText(res, code) })
	if !ok {
		fmt.Fprintf(stderr, "agentflow pr thread: unknown result status %q\n", res.Status)
		return code
	}
	if code != 0 {
		fmt.Fprintf(stderr, "agentflow pr thread: %s: %s\n", res.Status, res.Error)
	}
	return code
}
