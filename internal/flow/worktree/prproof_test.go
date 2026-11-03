package worktree

import (
	"path/filepath"
	"strings"
	"testing"
)

// pushMore adds a commit to branch on origin from the seed clone (another
// contributor, or the same work pushed from another worktree) and returns it.
func (f *fixture) pushMore(branch, file string) string {
	f.t.Helper()
	git(f.t, f.seed, "fetch", "-q", "origin")
	git(f.t, f.seed, "checkout", "-q", "-B", "more-"+file, "origin/"+branch)
	writeFile(f.t, f.seed, file, file+"\n")
	git(f.t, f.seed, "add", ".")
	git(f.t, f.seed, "commit", "-qm", file)
	git(f.t, f.seed, "push", "-q", "origin", "HEAD:"+branch)
	head := git(f.t, f.seed, "rev-parse", "HEAD")
	git(f.t, f.seed, "checkout", "-q", "main")
	return head
}

// A worktree left at an older commit of a PR that later merged (squash) at a
// newer head holds no commit of its own: the merged head contains it. That
// proves the merge. The remote branch is kept: only an exact head proves it
// unused (gopbx #143, #152, #155 worktrees, 2026-11-03).
func TestWorktreeBehindMergedPRHeadIsMerged(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/behind")
	old := git(t, path, "rev-parse", "HEAD")
	newer := f.pushMore("feat/behind", "later.txt")
	f.squashOnOrigin("feat/behind")
	git(t, f.repo, "fetch", "-q", "origin") // the merged head is local: no refs/pull fetch
	f.setPRs("feat/behind", "9 "+newer+" MERGED development")

	c, _ := f.inspectInto("feat/behind", "")
	if !c.OK {
		t.Fatalf("a worktree behind the merged head must be merged: %+v", c)
	}
	if want := "PR #9 into development (" + short(old) + " is contained in its merged head " + short(newer) + ")"; c.MergedVia != want {
		t.Fatalf("MergedVia = %q, want %q", c.MergedVia, want)
	}
	if c.prProof {
		t.Fatal("an ancestry proof must not authorize deleting the remote branch")
	}
	r := f.remove(c, true)
	if r.RemoteDeleted {
		t.Fatalf("remote branch deleted on an ancestry proof: %+v", r)
	}
	if out := git(t, f.repo, "ls-remote", "--heads", "origin", "feat/behind"); out == "" {
		t.Fatal("remote branch is gone")
	}
}

// The merged head need not be local: it is fetched from refs/pull/<n>/head,
// which GitHub keeps after the branch is deleted.
func TestWorktreeBehindFetchesThePRHead(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/gone")
	newer := f.pushMore("feat/gone", "later.txt")
	git(t, f.seed, "push", "-q", "origin", newer+":refs/pull/12/head")
	git(t, f.seed, "push", "-q", "origin", ":feat/gone") // deleted by the merge
	git(t, f.repo, "fetch", "-q", "--prune", "origin")
	if _, err := resolveCommit(t.Context(), f.env.withDefaults(), path, newer); err == nil {
		t.Fatal("fixture: the PR head must not be local yet")
	}
	f.setPRs("feat/gone", "12 "+newer+" MERGED main")
	if c := f.inspect("feat/gone"); !c.OK || !strings.Contains(c.MergedVia, "is contained in its merged head") {
		t.Fatalf("got %+v", c)
	}
}

// A worktree with a commit the merged head lacks is not merged.
func TestWorktreeAheadOfMergedPRHeadIsRefused(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/ahead")
	merged := git(t, path, "rev-parse", "HEAD")
	f.squashOnOrigin("feat/ahead")
	writeFile(t, path, "after.txt", "after\n")
	git(t, path, "add", ".")
	git(t, path, "commit", "-qm", "after the merge")
	f.setPRs("feat/ahead", "5 "+merged+" MERGED main")
	c := f.inspect("feat/ahead")
	if c.OK || !refusedFor(c, "PR #5 was MERGED into main at "+short(merged)) || !refusedFor(c, "its later commits are not merged") {
		t.Fatalf("got %+v", c)
	}
}

// A reused branch can have several merged PRs. A newer PR with an unrelated
// head must not hide an older merged PR whose head contains the worktree tip.
func TestWorktreeBehindAnyMergedPRHeadIsMerged(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/reused")
	old := git(t, path, "rev-parse", "HEAD")
	containing := f.pushMore("feat/reused", "later.txt")
	unrelated := git(t, f.seed, "rev-parse", "main")
	git(t, f.repo, "fetch", "-q", "origin")
	f.setPRs("feat/reused",
		"10 "+unrelated+" MERGED main",
		"9 "+containing+" MERGED development")

	c, _ := f.inspectInto("feat/reused", "")
	if !c.OK {
		t.Fatalf("an older merged PR contains the worktree head: %+v", c)
	}
	if want := "PR #9 into development (" + short(old) + " is contained in its merged head " + short(containing) + ")"; c.MergedVia != want {
		t.Fatalf("MergedVia = %q, want %q", c.MergedVia, want)
	}
}

// takeover checks out branch's PR head under a local name that tracks it,
// as a worker taking over someone else's PR does.
func (f *fixture) takeover(local, branch string) string {
	f.t.Helper()
	path := filepath.Join(f.root, "wt-"+strings.ReplaceAll(local, "/", "-"))
	git(f.t, f.repo, "fetch", "-q", "origin")
	git(f.t, f.repo, "worktree", "add", "-q", "-b", local, "--track", path, "origin/"+branch)
	return path
}

// A branch taken over under another local name finds the PR through its
// upstream; the exact or contained head then proves the merge.
func TestTakeoverBranchProvenThroughItsUpstream(t *testing.T) {
	f := newFixture(t)
	f.worktree("feat/theirs")
	path := f.takeover("takeover/155", "feat/theirs")
	head := git(t, path, "rev-parse", "HEAD")
	f.squashOnOrigin("feat/theirs")
	f.setPRs("feat/theirs", "155 "+head+" MERGED development")

	c, _ := f.inspectInto("takeover/155", "")
	if !c.OK || c.MergedVia != "PR #155 into development (head branch feat/theirs, this branch's upstream)" {
		t.Fatalf("got %+v", c)
	}
	if c.prProof {
		t.Fatal("an upstream proof must not authorize deleting a remote branch")
	}
	if r := f.remove(c, true); r.RemoteDeleted || !r.BranchDeleted {
		t.Fatalf("got %+v", r)
	}
	if out := git(t, f.repo, "ls-remote", "--heads", "origin", "feat/theirs"); out == "" {
		t.Fatal("the upstream branch was deleted")
	}
}

// The upstream is consulted only when no PR has the local name, and its
// OPEN PR still refuses.
func TestTakeoverBranchWithOpenUpstreamPRIsRefused(t *testing.T) {
	f := newFixture(t)
	f.worktree("feat/open-up")
	path := f.takeover("takeover/7", "feat/open-up")
	head := git(t, path, "rev-parse", "HEAD")
	f.setPRs("feat/open-up", "7 "+head+" OPEN development")
	if c, _ := f.inspectInto("takeover/7", ""); c.OK || !refusedFor(c, "PR #7 is OPEN into development: not merged") {
		t.Fatalf("got %+v", c)
	}
}

// A local branch with no upstream (or one on another remote) is looked up
// by its own name only.
func TestUpstreamBranchNeedsTheSameRemote(t *testing.T) {
	f := newFixture(t)
	path := f.takeover("takeover/9", "main")
	env := f.env.withDefaults()
	if got := upstreamBranch(t.Context(), env, path, "takeover/9", "origin"); got != "main" {
		t.Fatalf("upstream = %q, want main", got)
	}
	if got := upstreamBranch(t.Context(), env, path, "takeover/9", "upstream"); got != "" {
		t.Fatalf("another remote: upstream = %q, want empty", got)
	}
	git(t, path, "branch", "--unset-upstream")
	if got := upstreamBranch(t.Context(), env, path, "takeover/9", "origin"); got != "" {
		t.Fatalf("no upstream: got %q", got)
	}
}
