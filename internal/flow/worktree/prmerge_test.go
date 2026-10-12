package worktree

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// inspectInto inspects target the way done does: --into as given ("" =
// none, so only a merged PR proves the merge).
func (f *fixture) inspectInto(target, into string) (Check, Target) {
	f.t.Helper()
	ctx := context.Background()
	list, err := List(ctx, f.env, f.repo)
	if err != nil {
		f.t.Fatal(err)
	}
	w, err := Find(list, f.repo, target)
	if err != nil {
		f.t.Fatal(err)
	}
	tg, err := ResolveTarget(ctx, f.env, f.repo, into)
	if err != nil {
		f.t.Fatal(err)
	}
	if err := Fetch(ctx, f.env, f.repo, tg); err != nil {
		f.t.Fatal(err)
	}
	c, err := Inspect(ctx, f.env, w, tg)
	if err != nil {
		f.t.Fatal(err)
	}
	return c, tg
}

// mergeInto merges (or squash-merges) branch into base on origin, creating
// base from main first if it does not exist.
func (f *fixture) mergeInto(base, branch string, squash bool) {
	f.t.Helper()
	git(f.t, f.seed, "fetch", "-q", "origin")
	if git(f.t, f.seed, "ls-remote", "--heads", "origin", "refs/heads/"+base) == "" {
		git(f.t, f.seed, "push", "-q", "origin", "origin/main:refs/heads/"+base)
		git(f.t, f.seed, "fetch", "-q", "origin")
	}
	git(f.t, f.seed, "switch", "-q", "-C", "merge-"+base, "origin/"+base)
	if squash {
		git(f.t, f.seed, "merge", "-q", "--squash", "origin/"+branch)
		git(f.t, f.seed, "commit", "-qm", "squash "+branch)
	} else {
		git(f.t, f.seed, "merge", "-q", "--no-ff", "-m", "merge "+branch, "origin/"+branch)
	}
	git(f.t, f.seed, "push", "-q", "origin", "HEAD:refs/heads/"+base)
}

// The vector-payments #113 case: a PR merged into development (not origin's
// default branch) with exactly this head proves the merge without --into.
func TestMergedPRIntoNonDefaultBaseNeedsNoInto(t *testing.T) {
	for _, squash := range []bool{false, true} {
		name := "merge"
		if squash {
			name = "squash"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			path := f.worktree("tgeorge/bench-hosted-modal")
			f.mergeInto("development", "tgeorge/bench-hosted-modal", squash)
			head := git(t, path, "rev-parse", "HEAD")
			f.setPRs("tgeorge/bench-hosted-modal", "113 "+head+" MERGED development")

			c, tg := f.inspectInto("tgeorge/bench-hosted-modal", "")
			if !c.OK || c.MergedVia != "PR #113 into development" || c.PRNumber != 113 || c.PRState != "MERGED" || c.PRBase != "development" {
				t.Fatalf("got %+v", c)
			}
			if tg.Ref() != "" || tg.Default != "main" {
				t.Fatalf("no --into must mean no ancestry target (default kept for protection only): %+v", tg)
			}
			r, err := Remove(context.Background(), f.env, f.repo, c, tg, true)
			if err != nil {
				t.Fatal(err)
			}
			if !r.BranchDeleted || !r.RemoteDeleted || remoteHas(t, f, "tgeorge/bench-hosted-modal") {
				t.Fatalf("merged branch not cleaned up: %+v", r)
			}
			if !remoteHas(t, f, "development") || !remoteHas(t, f, "main") {
				t.Fatal("a long-lived branch was deleted")
			}
		})
	}
}

// Stacked PRs: #116 was opened on #113's branch, then retargeted to
// development and merged there. gh reports the PR's current base; an older
// closed PR for the same branch does not get in the way.
func TestRetargetedStackedPRIsProvenByItsCurrentBase(t *testing.T) {
	f := newFixture(t)
	f.worktree("feat/a")
	// feat/b stacks on feat/a.
	pathB := filepath.Join(f.root, "wt-feat-b")
	git(t, f.repo, "worktree", "add", "-q", "-b", "feat/b", pathB, "feat/a")
	writeFile(t, pathB, "b2.txt", "b\n")
	git(t, pathB, "add", ".")
	git(t, pathB, "commit", "-qm", "b")
	git(t, pathB, "push", "-q", "origin", "feat/b")
	f.mergeInto("development", "feat/b", true)
	headB := git(t, pathB, "rev-parse", "HEAD")
	f.setPRs("feat/b", "110 "+strings.Repeat("1", 40)+" CLOSED feat/a", "116 "+headB+" MERGED development")

	c, _ := f.inspectInto("feat/b", "")
	if !c.OK || c.MergedVia != "PR #116 into development" || c.PRBase != "development" {
		t.Fatalf("got %+v", c)
	}
	// A stacked PR whose base is still a feature branch protects that
	// branch from deletion as its merge target.
	if got := keepBranch("feat/a", Target{Remote: "origin"}, "feat/a"); got != "it is the merge target" {
		t.Fatalf("keepBranch = %q", got)
	}
}

func TestNotMergedRefusalsNameThePRAndAskForInto(t *testing.T) {
	cases := []struct {
		name, prs, into string
		want            []string
	}{
		{"open PR", "119 HEAD OPEN development", "", []string{"PR #119 is OPEN into development: not merged", "pass --into"}},
		{"closed unmerged PR", "118 HEAD CLOSED development", "", []string{"PR #118 into development was CLOSED without merging"}},
		{"merged at an older head", "117 " + strings.Repeat("2", 40) + " MERGED development", "", []string{"PR #117 was MERGED into development at 22222222", "later commits are not merged"}},
		{"no PR", "", "", []string{"GitHub has no merged PR from branch feat/x", "pass --into <branch> (no default branch is assumed)"}},
		{"open PR with --into", "119 HEAD OPEN development", "main", []string{"is not contained in origin/main", "PR #119 is OPEN"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			path := f.worktree("feat/x")
			head := git(t, path, "rev-parse", "HEAD")
			var lines []string
			if tc.prs != "" {
				lines = append(lines, strings.Replace(tc.prs, "HEAD", head, 1))
			}
			f.setPRs("feat/x", lines...)
			c, _ := f.inspectInto("feat/x", tc.into)
			if c.OK {
				t.Fatalf("must refuse: %+v", c)
			}
			for _, w := range tc.want {
				if !refusedFor(c, w) {
					t.Errorf("refusals %q lack %q", c.Refusals, w)
				}
			}
			if tc.prs != "" && c.PRNumber == 0 {
				t.Errorf("PR fields not reported: %+v", c)
			}
		})
	}
}

// Even with ancestry into the default branch, nothing is assumed without --into.
func TestAncestryIntoDefaultBranchNeedsExplicitInto(t *testing.T) {
	f := newFixture(t)
	f.worktree("feat/anc")
	f.mergeOnOrigin("feat/anc")
	f.setPRs("feat/anc")
	if c, _ := f.inspectInto("feat/anc", ""); c.OK || !refusedFor(c, "pass --into") {
		t.Fatalf("no --into: got %+v", c)
	}
	if c, _ := f.inspectInto("feat/anc", "main"); !c.OK || c.MergedVia != "contained in origin/main" {
		t.Fatalf("--into main: got %+v", c)
	}
}

func TestGHFailureIsARefusalNeverAPass(t *testing.T) {
	for _, tc := range []struct{ name, script string }{
		{"error", "#!/bin/sh\necho 'HTTP 502: bad gateway' >&2\nexit 1\n"},
		{"timeout", "#!/bin/sh\nexec sleep 30\n"},
		{"garbage", "#!/bin/sh\necho 'not a pr line'\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newFixture(t)
			path := f.worktree("feat/gh")
			f.mergeInto("development", "feat/gh", true)
			writeFile(t, f.root, "gh-bad", tc.script)
			if err := os.Chmod(filepath.Join(f.root, "gh-bad"), 0o755); err != nil {
				t.Fatal(err)
			}
			f.env.GH = filepath.Join(f.root, "gh-bad")
			f.env.GHTimeout = 300 * time.Millisecond
			start := time.Now()
			c, _ := f.inspectInto("feat/gh", "")
			if c.OK || c.PRError == "" || !refusedFor(c, "GitHub could not be checked") || !refusedFor(c, "pass --into") {
				t.Fatalf("gh failure must refuse: %+v", c)
			}
			if tc.name == "timeout" && (!strings.Contains(c.PRError, "timed out") || time.Since(start) > 15*time.Second) {
				t.Fatalf("gh was not bounded: %q after %s", c.PRError, time.Since(start))
			}
			_ = path
		})
	}
}

func TestMalformedPRFieldsAreARefusalNeverAPass(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/malformed")
	head := git(t, path, "rev-parse", "HEAD")
	writeFile(t, f.root, "gh-malformed", "#!/bin/sh\nprintf '123\\tMERGED\\t\\tfeat/malformed\\t"+head+"\\t\\n'\n")
	if err := os.Chmod(filepath.Join(f.root, "gh-malformed"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env.GH = filepath.Join(f.root, "gh-malformed")
	c, _ := f.inspectInto("feat/malformed", "")
	if c.OK || c.PRError == "" || c.PRNumber != 0 || !refusedFor(c, "GitHub could not be checked") {
		t.Fatalf("malformed PR output must not prove a merge: %+v", c)
	}
}

// With --into, ancestry is independent proof: a gh outage is recorded but
// does not block, and the remote branch is kept (no PR proof).
func TestGHFailureWithIntoFallsBackToAncestryAndKeepsRemote(t *testing.T) {
	f := newFixture(t)
	f.worktree("feat/anc-gh")
	f.mergeOnOrigin("feat/anc-gh")
	writeFile(t, f.root, "gh-bad", "#!/bin/sh\necho boom >&2\nexit 1\n")
	if err := os.Chmod(filepath.Join(f.root, "gh-bad"), 0o755); err != nil {
		t.Fatal(err)
	}
	f.env.GH = filepath.Join(f.root, "gh-bad")
	c, tg := f.inspectInto("feat/anc-gh", "main")
	if !c.OK || c.MergedVia != "contained in origin/main" || !strings.Contains(c.PRError, "boom") {
		t.Fatalf("got %+v", c)
	}
	r, err := Remove(context.Background(), f.env, f.repo, c, tg, true)
	if err != nil {
		t.Fatal(err)
	}
	if r.RemoteDeleted || !remoteHas(t, f, "feat/anc-gh") || !strings.Contains(r.Note, "GitHub could not be checked") {
		t.Fatalf("remote must be kept without PR proof: %+v", r)
	}
}

func TestMissingGHIsARefusal(t *testing.T) {
	f := newFixture(t)
	f.worktree("feat/nogh")
	f.env.GH = filepath.Join(f.root, "does-not-exist")
	if c, _ := f.inspectInto("feat/nogh", ""); c.OK || c.PRError == "" {
		t.Fatalf("got %+v", c)
	}
}

func TestDefaultBranchIsProtectedWithoutInto(t *testing.T) {
	if got := keepBranch("trunk-ish", Target{Remote: "origin", Default: "trunk-ish"}, ""); got == "" {
		t.Fatal("origin's default branch must be kept even when --into is not given")
	}
}

func TestOriginDefaultBranchIsProtectedWithAnotherIntoRemote(t *testing.T) {
	f := newFixture(t)
	git(t, f.seed, "push", "-q", "origin", "main:refs/heads/trunk-ish")
	git(t, f.origin, "symbolic-ref", "HEAD", "refs/heads/trunk-ish")
	git(t, f.repo, "remote", "add", "upstream", f.origin)
	git(t, f.repo, "fetch", "-q", "upstream")
	git(t, f.repo, "remote", "set-head", "upstream", "main")
	// Both local remote HEAD refs are stale at main. Protection must use
	// origin's current advertised HEAD, independent of the --into remote.

	tg, err := ResolveTarget(context.Background(), f.env, f.repo, "upstream/main")
	if err != nil {
		t.Fatal(err)
	}
	if tg.Remote != "upstream" || tg.Branch != "main" || tg.Default != "trunk-ish" {
		t.Fatalf("target must retain origin's default for deletion protection: %+v", tg)
	}
	if got := keepBranch("trunk-ish", tg, ""); got != "it is the remote's default branch" {
		t.Fatalf("origin default is not protected: %q", got)
	}
}

func TestUnreadableOriginHeadIsARemoteError(t *testing.T) {
	f := newFixture(t)
	git(t, f.repo, "remote", "set-url", "origin", filepath.Join(f.root, "missing.git"))
	if _, err := ResolveTarget(context.Background(), f.env, f.repo, ""); !errors.Is(err, ErrRemote) {
		t.Fatalf("got %v, want ErrRemote", err)
	}
}
