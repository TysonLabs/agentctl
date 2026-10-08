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

var replyExitCodes = map[pr.ReplyStatus]int{
	pr.ReplyDone:        0,
	pr.ReplyGHError:     1,
	pr.ReplyRefused:     2,
	pr.ReplyNotResolved: 3,
	pr.ReplyInterrupted: 130,
}

func runPRReply(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow pr reply: "+format+"\n", a...)
		return 1
	}
	fs := flag.NewFlagSet("agentflow pr reply", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var o pr.ReplyOptions
	fs.StringVar(&o.Repo, "repo", "", "")
	fs.StringVar(&o.Fixed, "fixed", "", "")
	fs.StringVar(&o.Note, "note", "", "")
	fs.StringVar(&o.Keep, "keep", "", "")
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
	if o.Repo != "" && !repoRe.MatchString(o.Repo) {
		return fail("--repo %q is not OWNER/NAME", o.Repo)
	}
	if err := pr.ValidateReply(o); err != nil {
		return fail("%v", err)
	}
	bin := os.Getenv("AGENTFLOW_GH")
	if bin == "" {
		if bin, err = exec.LookPath("gh"); err != nil {
			return fail("gh not found: put it on PATH or set AGENTFLOW_GH")
		}
	}
	o.GH = pr.GHCLI(bin, 60*time.Second)

	res := pr.Reply(ctx, o)
	out, _ := json.MarshalIndent(res, "", "  ")
	_, _ = stdout.Write(append(out, '\n'))
	code, ok := replyExitCodes[res.Status]
	if !ok {
		fmt.Fprintf(stderr, "agentflow pr reply: unknown result status %q\n", res.Status)
		return 1
	}
	if code != 0 {
		msg := res.Error
		if res.Next != "" {
			msg += " (" + res.Next + ")"
		}
		fmt.Fprintf(stderr, "agentflow pr reply: %s: %s\n", res.Status, msg)
	}
	return code
}
