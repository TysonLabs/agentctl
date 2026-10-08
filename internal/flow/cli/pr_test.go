package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/TysonLabs/agentctl/internal/flow/pr"
)

func TestPRWaitUsageErrors(t *testing.T) {
	t.Setenv("AGENTFLOW_GH", "/nonexistent/gh") // never run: each case fails first
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"pr", "wait"}, "exactly one PR number"},
		{[]string{"pr", "wait", "7", "8"}, "exactly one PR number"},
		{[]string{"pr", "wait", "abc"}, "not a positive integer"},
		{[]string{"pr", "wait", "0"}, "not a positive integer"},
		{[]string{"pr", "wait", "7", "--repo", "not-a-repo"}, "not OWNER/NAME"},
		{[]string{"pr", "wait", "7", "--repo", "o/r", "--timeout", "0s"}, "must be positive"},
		{[]string{"pr", "wait", "7", "--bogus"}, "flag provided but not defined"},
		{[]string{"pr", "nope"}, "unknown subcommand"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if code := Run(c.args, &out, &errb); code != 1 || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%v: exit %d, stderr %q; want 1 and %q", c.args, code, errb.String(), c.want)
		}
	}
}

func TestPRWaitHelp(t *testing.T) {
	for _, args := range [][]string{{"pr"}, {"pr", "--help"}, {"pr", "wait", "--help"}} {
		var out, errb bytes.Buffer
		if code := Run(args, &out, &errb); code != 0 || !strings.Contains(out.String(), "agentflow pr wait") {
			t.Errorf("%v: exit %d, stdout %q", args, code, out.String())
		}
	}
}

func TestPRExitCodesCoverEveryStatus(t *testing.T) {
	all := []pr.Status{pr.StatusClean, pr.StatusOpenThreads, pr.StatusSkipped, pr.StatusRateLimited,
		pr.StatusClosed, pr.StatusWaiting, pr.StatusGHError, pr.StatusTimeout, pr.StatusInterrupted}
	seen := map[int]pr.Status{}
	for _, s := range all {
		code, ok := prExitCodes[s]
		if !ok {
			t.Errorf("no exit code for %s", s)
		}
		if prev, dup := seen[code]; dup {
			t.Errorf("exit %d used by %s and %s", code, prev, s)
		}
		seen[code] = s
	}
	if len(prExitCodes) != len(all) {
		t.Errorf("prExitCodes has %d entries, want %d", len(prExitCodes), len(all))
	}
}

func TestPRReplyUsageErrors(t *testing.T) {
	t.Setenv("AGENTFLOW_GH", "/nonexistent/gh") // never run: each case fails first
	const id = "PRRT_kwDOabc"
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"pr", "reply"}, "exactly one thread id"},
		{[]string{"pr", "reply", id, "PRRT_two", "--keep", "r"}, "exactly one thread id"},
		{[]string{"pr", "reply", "123", "--keep", "r"}, "not a review-thread node id"},
		{[]string{"pr", "reply", id}, "exactly one of --fixed"},
		{[]string{"pr", "reply", id, "--fixed", "0123abc"}, "needs --note"},
		{[]string{"pr", "reply", id, "--fixed", "xyz", "--note", "n"}, "not a commit sha"},
		{[]string{"pr", "reply", id, "--keep", "r", "--repo", "nope"}, "not OWNER/NAME"},
		{[]string{"pr", "reply", id, "--bogus"}, "flag provided but not defined"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if code := Run(c.args, &out, &errb); code != 1 || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%v: exit %d, stderr %q; want 1 and %q", c.args, code, errb.String(), c.want)
		}
	}
}

func TestPRReplyHelp(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"pr", "reply", "--help"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "agentflow pr reply") {
		t.Errorf("exit %d, stdout %q", code, out.String())
	}
}

func TestReplyExitCodesCoverEveryStatus(t *testing.T) {
	all := []pr.ReplyStatus{pr.ReplyDone, pr.ReplyRefused, pr.ReplyNotResolved, pr.ReplyGHError, pr.ReplyInterrupted}
	seen := map[int]pr.ReplyStatus{}
	for _, s := range all {
		code, ok := replyExitCodes[s]
		if !ok {
			t.Errorf("no exit code for %s", s)
		}
		if prev, dup := seen[code]; dup {
			t.Errorf("exit %d used by %s and %s", code, prev, s)
		}
		seen[code] = s
	}
	if len(replyExitCodes) != len(all) {
		t.Errorf("replyExitCodes has %d entries, want %d", len(replyExitCodes), len(all))
	}
}
