package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"

	"github.com/TysonLabs/agentctl/internal/flow/pr"
)

func TestPRMergeUsageErrors(t *testing.T) {
	t.Setenv("AGENTFLOW_GH", "/nonexistent/gh") // never run: each case fails first
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"pr", "merge"}, "exactly one PR number"},
		{[]string{"pr", "merge", "x"}, "not a positive integer"},
		{[]string{"pr", "merge", "7", "--method", "ff"}, "not merge, squash or rebase"},
		{[]string{"pr", "merge", "7", "--head", "abc123"}, "full 40-character"},
		{[]string{"pr", "merge", "7", "--sync-branch", "-x"}, "plain branch name"},
		{[]string{"pr", "merge", "7", "--sync-branch", "a/../b"}, "plain branch name"},
		{[]string{"pr", "merge", "7", "--sync-branch", "a b"}, "plain branch name"},
		{[]string{"pr", "merge", "7", "--repo", "nope"}, "not OWNER/NAME"},
		{[]string{"pr", "merge", "7", "--repo", "./repo"}, "not OWNER/NAME"},
		{[]string{"pr", "merge", "7", "--repo", "owner/.."}, "not OWNER/NAME"},
		{[]string{"pr", "merge", "7", "--delete-branch"}, "flag provided but not defined"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if code := Run(c.args, &out, &errb); code != 1 || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%v: exit %d, stderr %q; want 1 and %q", c.args, code, errb.String(), c.want)
		}
	}
}

func TestPRMergeCancellationDuringRepoDiscovery(t *testing.T) {
	t.Setenv("AGENTFLOW_GH", "/nonexistent/gh")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out, errb bytes.Buffer
	if code := runPRMerge(ctx, []string{"7"}, &out, &errb); code != 130 {
		t.Fatalf("exit %d, stderr %q; want 130", code, errb.String())
	}
}

func TestPRMergeHelp(t *testing.T) {
	var out, errb bytes.Buffer
	if code := Run([]string{"pr", "merge", "--help"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "agentflow pr merge <number>") {
		t.Errorf("exit %d, stdout %q", code, out.String())
	}
}

func TestPRMergeExitCodesCoverEveryStatus(t *testing.T) {
	all := []pr.MergeStatus{pr.MergeMerged, pr.MergeRefused, pr.MergeSyncFailed, pr.MergeUnconfirmed,
		pr.MergeGHError, pr.MergeInterrupted}
	seen := map[int]pr.MergeStatus{}
	for _, s := range all {
		code, ok := prMergeExitCodes[s]
		if !ok {
			t.Errorf("no exit code for %s", s)
		}
		if prev, dup := seen[code]; dup {
			t.Errorf("exit %d used by %s and %s", code, prev, s)
		}
		seen[code] = s
	}
	if len(prMergeExitCodes) != len(all) {
		t.Errorf("prMergeExitCodes has %d entries, want %d", len(prMergeExitCodes), len(all))
	}
}
