package worktree

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fixture is an origin (bare), a "seed" clone that plays GitHub (merges and
// pushes main), and the repo under test with its worktrees.
type fixture struct {
	t      *testing.T
	root   string
	origin string
	seed   string
	repo   string
	env    Env
}

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@e", "-c", "user.name=t"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &fixture{t: t, root: root, origin: filepath.Join(root, "origin.git"), seed: filepath.Join(root, "seed"), repo: filepath.Join(root, "repo")}
	git(t, root, "init", "-q", "--bare", "-b", "main", f.origin)
	git(t, root, "clone", "-q", f.origin, f.seed)
	writeFile(t, f.seed, ".gitignore", "target/\n")
	writeFile(t, f.seed, "a.txt", "a\n")
	git(t, f.seed, "add", ".")
	git(t, f.seed, "commit", "-qm", "base")
	git(t, f.seed, "push", "-q", "origin", "main")
	git(t, root, "clone", "-q", f.origin, f.repo)

	gh := filepath.Join(root, "gh")
	writeFile(t, root, "gh", "#!/bin/sh\ncat \""+filepath.Join(root, "gh.out")+"\" 2>/dev/null\n")
	if err := os.Chmod(gh, 0o755); err != nil {
		t.Fatal(err)
	}
	f.env = Env{GH: gh}
	return f
}

func writeFile(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func (f *fixture) setPRs(lines ...string) {
	writeFile(f.t, f.root, "gh.out", strings.Join(lines, "\n")+"\n")
}

// worktree adds a worktree on a new branch with one pushed commit.
func (f *fixture) worktree(branch string) string {
	f.t.Helper()
	path := filepath.Join(f.root, "wt-"+strings.ReplaceAll(branch, "/", "-"))
	git(f.t, f.repo, "worktree", "add", "-q", "-b", branch, path, "origin/main")
	writeFile(f.t, path, branch+".txt", branch+"\n")
	git(f.t, path, "add", ".")
	git(f.t, path, "commit", "-qm", branch)
	git(f.t, path, "push", "-q", "origin", branch)
	return path
}

// mergeOnOrigin merges branch into main the way GitHub's merge button does.
func (f *fixture) mergeOnOrigin(branch string) {
	f.t.Helper()
	git(f.t, f.seed, "fetch", "-q", "origin")
	git(f.t, f.seed, "merge", "-q", "--no-ff", "-m", "merge "+branch, "origin/"+branch)
	git(f.t, f.seed, "push", "-q", "origin", "main")
}

// squashOnOrigin squash-merges branch: main gets the change, not the commits.
func (f *fixture) squashOnOrigin(branch string) {
	f.t.Helper()
	git(f.t, f.seed, "fetch", "-q", "origin")
	git(f.t, f.seed, "merge", "-q", "--squash", "origin/"+branch)
	git(f.t, f.seed, "commit", "-qm", "squash "+branch)
	git(f.t, f.seed, "push", "-q", "origin", "main")
}

func (f *fixture) inspect(target string) Check {
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
	tg, err := DefaultTarget(ctx, f.env, f.repo, "")
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
	return c
}

func (f *fixture) remove(c Check, deleteRemote bool) Removed {
	f.t.Helper()
	tg, _ := DefaultTarget(context.Background(), f.env, f.repo, "")
	r, err := Remove(context.Background(), f.env, f.repo, c, tg, deleteRemote)
	if err != nil {
		f.t.Fatal(err)
	}
	return r
}

func refusedFor(c Check, substr string) bool {
	for _, r := range c.Refusals {
		if strings.Contains(r, substr) {
			return true
		}
	}
	return false
}

func remoteHas(t *testing.T, f *fixture, branch string) bool {
	return git(t, f.repo, "ls-remote", "--heads", "origin", "refs/heads/"+branch) != ""
}

func localHas(f *fixture, branch string) bool {
	return exec.Command("git", "-C", f.repo, "rev-parse", "--verify", "--quiet", "refs/heads/"+branch).Run() == nil
}

func TestMergedCleanWorktreeIsRemovedWithItsBuildOutput(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/a")
	writeFile(t, path, "target/big.bin", strings.Repeat("x", 4096)) // ignored build output
	f.mergeOnOrigin("feat/a")
	head := git(t, path, "rev-parse", "HEAD")
	f.setPRs("12 " + head)

	c := f.inspect("feat/a") // origin/main is stale locally until Inspect's fetch
	if !c.OK || c.MergedVia != "contained in origin/main" || c.KeepBranch != "" {
		t.Fatalf("got %+v", c)
	}
	r := f.remove(c, true)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("worktree dir still exists (%v)", err)
	}
	if !r.BranchDeleted || localHas(f, "feat/a") {
		t.Errorf("local branch not deleted: %+v", r)
	}
	if !r.RemoteDeleted || remoteHas(t, f, "feat/a") {
		t.Errorf("remote branch not deleted: %+v", r)
	}
	if r.FreedBytes < 4096 {
		t.Errorf("freed %d bytes, want at least the 4096 of target/", r.FreedBytes)
	}
}

func TestRemoteBranchKeptWithoutPRProof(t *testing.T) {
	f := newFixture(t)
	f.worktree("feat/b")
	f.mergeOnOrigin("feat/b")
	f.setPRs() // gh knows of no merged PR
	c := f.inspect("feat/b")
	r := f.remove(c, true)
	if r.RemoteDeleted || !remoteHas(t, f, "feat/b") || !strings.Contains(r.Note, "no merged PR") {
		t.Fatalf("remote branch should stay without PR proof: %+v", r)
	}
	if !r.BranchDeleted {
		t.Errorf("local branch should still be deleted: %+v", r)
	}
}

func TestSquashMergeProvenByPRHead(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/sq")
	f.squashOnOrigin("feat/sq")
	head := git(t, path, "rev-parse", "HEAD")

	f.setPRs("7 " + strings.Repeat("0", 40)) // a merged PR with a different head
	if c := f.inspect("feat/sq"); c.OK || !refusedFor(c, "not merged") {
		t.Fatalf("a PR with another head must not prove the merge: %+v", c)
	}
	f.setPRs("7 " + strings.ToUpper(head))
	c := f.inspect("feat/sq")
	if !c.OK || c.MergedVia != "PR #7" {
		t.Fatalf("got %+v", c)
	}
	if r := f.remove(c, true); !r.BranchDeleted || !r.RemoteDeleted {
		t.Errorf("squash-merged branch not cleaned up: %+v", r)
	}
}

func TestUnmergedIsRefused(t *testing.T) {
	f := newFixture(t)
	f.worktree("feat/open")
	f.setPRs()
	if c := f.inspect("feat/open"); c.OK || !refusedFor(c, "not merged") {
		t.Fatalf("got %+v", c)
	}
}

func TestWorkThatWouldBeLostIsRefused(t *testing.T) {
	for _, name := range []string{"modified", "untracked"} {
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			path := f.worktree("feat/" + name)
			f.mergeOnOrigin("feat/" + name)
			if name == "modified" {
				writeFile(t, path, "a.txt", "changed\n")
			} else {
				writeFile(t, path, "notes/draft.md", "keep me\n")
			}
			c := f.inspect("feat/" + name)
			if c.OK || !refusedFor(c, "would be lost") {
				t.Fatalf("got %+v", c)
			}
		})
	}
}

func TestLockedIsRefused(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/locked")
	f.mergeOnOrigin("feat/locked")
	git(t, f.repo, "worktree", "lock", "--reason", "claude session x (pid 999999 start now)", path)
	c := f.inspect("feat/locked")
	if c.OK || !refusedFor(c, "locked (claude session x") || !refusedFor(c, "probably stale") {
		t.Fatalf("got %+v", c)
	}
}

func TestInUseIsRefused(t *testing.T) {
	if _, err := exec.LookPath("lsof"); err != nil {
		t.Skip("lsof not installed")
	}
	f := newFixture(t)
	path := f.worktree("feat/busy")
	f.mergeOnOrigin("feat/busy")
	sleeper := exec.Command("sleep", "30")
	sleeper.Dir = filepath.Join(path)
	if err := sleeper.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sleeper.Process.Kill(); _ = sleeper.Wait() }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		c := f.inspect("feat/busy")
		if refusedFor(c, "in use: sleep") {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a process inside the worktree was not detected: %+v", c)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestMainWorktreeIsRefused(t *testing.T) {
	f := newFixture(t)
	c := f.inspect(f.repo)
	if c.OK || !refusedFor(c, "main working tree") {
		t.Fatalf("got %+v", c)
	}
}

func TestLongLivedBranchSurvives(t *testing.T) {
	f := newFixture(t)
	// The main checkout moves off main; a worktree holds main itself.
	git(t, f.repo, "switch", "-q", "-c", "side")
	wt := filepath.Join(f.root, "wt-main")
	git(t, f.repo, "worktree", "add", "-q", wt, "main")
	f.setPRs("1 " + git(t, wt, "rev-parse", "HEAD")) // even a "PR proof" must not matter
	c := f.inspect("main")
	if !c.OK || c.KeepBranch != "it is the merge target" {
		t.Fatalf("got %+v", c)
	}
	r := f.remove(c, true)
	if r.BranchDeleted || r.RemoteDeleted || !localHas(f, "main") || !remoteHas(t, f, "main") {
		t.Fatalf("main must survive: %+v", r)
	}
	if _, err := os.Stat(wt); !os.IsNotExist(err) {
		t.Errorf("the worktree itself should still be removed")
	}
	for _, b := range []string{"development", "release/1.2", "staging"} {
		if keepBranch(b, Target{Remote: "origin", Branch: "main"}) == "" {
			t.Errorf("%s should be kept", b)
		}
	}
}

func TestRemoteBranchThatMovedIsKept(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/moved")
	f.mergeOnOrigin("feat/moved")
	head := git(t, path, "rev-parse", "HEAD")
	f.setPRs("3 " + head)
	// Someone pushes another commit to the remote branch after the merge.
	git(t, f.seed, "fetch", "-q", "origin")
	git(t, f.seed, "switch", "-q", "-c", "moved", "origin/feat/moved")
	writeFile(t, f.seed, "more.txt", "more\n")
	git(t, f.seed, "add", ".")
	git(t, f.seed, "commit", "-qm", "more")
	git(t, f.seed, "push", "-q", "origin", "moved:feat/moved")
	c := f.inspect("feat/moved")
	r := f.remove(c, true)
	if r.RemoteDeleted || !remoteHas(t, f, "feat/moved") || !strings.Contains(r.Note, "points at") {
		t.Fatalf("got %+v", r)
	}
}

func TestStaleRecordIsPrunable(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/gone")
	if err := os.RemoveAll(path); err != nil {
		t.Fatal(err)
	}
	c := f.inspect("feat/gone")
	if c.OK || !c.Prunable {
		t.Fatalf("got %+v", c)
	}
	if err := Prune(context.Background(), f.env, f.repo); err != nil {
		t.Fatal(err)
	}
	list, _ := List(context.Background(), f.env, f.repo)
	if len(list) != 1 {
		t.Fatalf("stale record not pruned: %+v", list)
	}
}

func TestFind(t *testing.T) {
	list := []Worktree{
		{Path: "/r", Branch: "main", Main: true},
		{Path: "/r/.claude/worktrees/one", Branch: "feat/one"},
		{Path: "/elsewhere/one", Branch: "feat/two"},
	}
	if w, err := Find(list, "/r", "feat/two"); err != nil || w.Path != "/elsewhere/one" {
		t.Errorf("by branch: %+v %v", w, err)
	}
	if w, err := Find(list, "/r", ".claude/worktrees/one"); err != nil || w.Branch != "feat/one" {
		t.Errorf("by relative path: %+v %v", w, err)
	}
	if _, err := Find(list, "/r", "one"); err == nil || !strings.Contains(err.Error(), "matches 2") {
		t.Errorf("ambiguous base name: %v", err)
	}
	if _, err := Find(list, "/r", "nope"); err == nil {
		t.Error("no match: want an error")
	}
}
