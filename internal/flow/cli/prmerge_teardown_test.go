package cli

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TysonLabs/agentctl/internal/flow/pr"
)

// The PR #245-#256 case (2026-10-16): subagents made the worktrees and ended
// before the merge, so nobody ran done. --teardown makes the merging session
// remove them.
func TestTeardownRemovesTheMergedHeadBranchWorktree(t *testing.T) {
	root, seed, repo := cliRepo(t)
	path, tip := branchWorktree(t, root, repo, "tgeorge/sub-agent-work")
	if err := os.MkdirAll(path+"/.claude", 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+"/.claude/brief.md", []byte("brief\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	squashIntoDevelopment(t, seed, "tgeorge/sub-agent-work")
	setFakePRs(t, root, prFake{245, "MERGED", "development", "tgeorge/sub-agent-work", tip, nil})
	t.Chdir(repo)

	td := teardownMerged(context.Background(), pr.MergeResult{Status: pr.MergeMerged, HeadBranch: "tgeorge/sub-agent-work"})
	if td.Status != teardownRemoved {
		t.Fatalf("got %+v (done: %s)", td, td.Done)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree still exists (%v)", err)
	}
	var done struct {
		Removed struct {
			ScratchDeleted []string `json:"scratch_deleted"`
		} `json:"removed"`
	}
	if err := json.Unmarshal(td.Done, &done); err != nil || len(done.Removed.ScratchDeleted) != 1 {
		t.Errorf("done JSON %s (err %v): want the brief in scratch_deleted", td.Done, err)
	}
}

func TestTeardownRefusedKeepsTheWorktree(t *testing.T) {
	root, _, repo := cliRepo(t)
	path, tip := branchWorktree(t, root, repo, "feat/open")
	setFakePRs(t, root, prFake{9, "OPEN", "development", "feat/open", tip, nil})
	t.Chdir(repo)

	// The merge result says merged, but GitHub does not show it: done refuses.
	td := teardownMerged(context.Background(), pr.MergeResult{Status: pr.MergeMerged, HeadBranch: "feat/open"})
	if td.Status != teardownRefused || !strings.Contains(td.Reason, "not merged") {
		t.Fatalf("got %+v", td)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("worktree removed after a refusal: %v", err)
	}
}

func TestTeardownInterruptedKeepsTheWorktree(t *testing.T) {
	root, seed, repo := cliRepo(t)
	path, tip := branchWorktree(t, root, repo, "feat/canceled-after-merge")
	squashIntoDevelopment(t, seed, "feat/canceled-after-merge")
	setFakePRs(t, root, prFake{10, "MERGED", "development", "feat/canceled-after-merge", tip, nil})
	t.Chdir(repo)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	td := teardownMerged(ctx, pr.MergeResult{Status: pr.MergeMerged, HeadBranch: "feat/canceled-after-merge"})
	if td.Status != teardownSkipped || !strings.Contains(td.Reason, "interrupted") {
		t.Fatalf("got %+v (done: %s)", td, td.Done)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("worktree removed after an interrupt: %v", err)
	}
}

func TestTeardownUsesTheExactWorktreePath(t *testing.T) {
	root, seed, repo := cliRepo(t)
	path, tip := branchWorktree(t, root, repo, "collision")
	other := filepath.Join(root, "collision")
	cliGit(t, repo, "worktree", "add", "-q", "-b", "feat/other", other, "origin/development")
	squashIntoDevelopment(t, seed, "collision")
	setFakePRs(t, root, prFake{11, "MERGED", "development", "collision", tip, nil})
	t.Chdir(repo)

	td := teardownMerged(context.Background(), pr.MergeResult{Status: pr.MergeMerged, HeadBranch: "collision"})
	if td.Status != teardownRemoved {
		t.Fatalf("got %+v (done: %s)", td, td.Done)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("head worktree still exists (%v)", err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatalf("unrelated colliding worktree was removed: %v", err)
	}
}

func TestTeardownNoneAndSkipped(t *testing.T) {
	_, _, repo := cliRepo(t)
	t.Chdir(repo)
	ctx := context.Background()
	if td := teardownMerged(ctx, pr.MergeResult{Status: pr.MergeMerged, HeadBranch: "feat/elsewhere"}); td.Status != teardownNone {
		t.Errorf("no worktree for the branch: got %+v", td)
	}
	for _, res := range []pr.MergeResult{
		{Status: pr.MergeRefused, HeadBranch: "feat/x"},
		{Status: pr.MergeUnconfirmed, HeadBranch: "feat/x"},
		{Status: pr.MergeMerged},
	} {
		if td := teardownMerged(ctx, res); td.Status != teardownSkipped {
			t.Errorf("%+v: got %+v", res, td)
		}
	}
}
