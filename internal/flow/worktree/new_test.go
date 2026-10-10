package worktree

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func (f *fixture) newWT(opt NewOptions) (Created, error) {
	f.t.Helper()
	return New(context.Background(), f.env, f.repo, opt)
}

// advanceOrigin pushes a commit to origin/main that f.repo has not fetched.
func (f *fixture) advanceOrigin(name string) string {
	f.t.Helper()
	git(f.t, f.seed, "pull", "-q", "origin", "main")
	writeFile(f.t, f.seed, name, name+"\n")
	git(f.t, f.seed, "add", ".")
	git(f.t, f.seed, "commit", "-qm", name)
	git(f.t, f.seed, "push", "-q", "origin", "main")
	return git(f.t, f.seed, "rev-parse", "HEAD")
}

func refusedWith(err error, substr string) bool {
	var r *RefusedError
	if !errors.As(err, &r) || !errors.Is(err, ErrRefused) {
		return false
	}
	for _, reason := range r.Reasons {
		if strings.Contains(reason, substr) {
			return true
		}
	}
	return false
}

func TestNewFetchesAndBranchesFromDerivedDefault(t *testing.T) {
	f := newFixture(t)
	fresh := f.advanceOrigin("fresh.txt") // the repo's origin/main is now stale
	c, err := f.newWT(NewOptions{Branch: "feat/x"})
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(f.repo, ".claude", "worktrees", "feat-x")
	if c.Path != want || c.Branch != "feat/x" || c.Base != "origin/main" || c.BaseSHA != fresh {
		t.Fatalf("created %+v, want path %s base origin/main@%s", c, want, fresh)
	}
	if got := git(t, c.Path, "rev-parse", "HEAD"); got != fresh {
		t.Fatalf("HEAD %s, want the freshly fetched %s", got, fresh)
	}
	if got := git(t, c.Path, "branch", "--show-current"); got != "feat/x" {
		t.Fatalf("branch %q", got)
	}
	// No upstream: a bare `git push` must never target main.
	if out, err := exec.Command("git", "-C", c.Path, "config", "branch.feat/x.merge").Output(); err == nil {
		t.Fatalf("branch has an upstream %q", out)
	}
	if c.Cloned == nil {
		t.Fatal("cloned must be [] not null")
	}
	if len(c.Notes) == 0 || !strings.Contains(c.Notes[0], "not ignored") {
		t.Fatalf("want a note that .claude/worktrees is not ignored: %+v", c.Notes)
	}
}

func TestNewFetchesBeforeResolvingDerivedDefault(t *testing.T) {
	f := newFixture(t)
	log := filepath.Join(f.root, "git-order.log")
	wrapper := filepath.Join(f.root, "git-order")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> \"" + log + "\"\n" +
		"if [ \"$1\" = symbolic-ref ] && ! grep -q '^fetch ' \"" + log + "\"; then exit 97; fi\n" +
		"exec git \"$@\"\n"
	writeFile(t, f.root, "git-order", script)
	if err := os.Chmod(wrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	f.env.Git = wrapper
	if _, err := f.newWT(NewOptions{Branch: "feat/order"}); err != nil {
		t.Fatalf("New resolved the default branch before fetching: %v", err)
	}
}

func TestNewRefusesSymlinkedWorktreeRootOutsideMainCheckout(t *testing.T) {
	f := newFixture(t)
	outside := filepath.Join(f.root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.repo, ".claude")); err != nil {
		t.Fatal(err)
	}
	_, err := f.newWT(NewOptions{Branch: "feat/escape"})
	if !refusedWith(err, "outside the main checkout") {
		t.Fatalf("symlinked worktree root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(outside, "worktrees")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("New wrote through .claude symlink: %v", err)
	}
}

func TestNewCleansWorktreeLeftByFailingCheckoutHook(t *testing.T) {
	for _, scratch := range []bool{false, true} {
		name := "branch"
		if scratch {
			name = "scratch"
		}
		t.Run(name, func(t *testing.T) {
			f := newFixture(t)
			hook := filepath.Join(f.repo, ".git", "hooks", "post-checkout")
			if err := os.WriteFile(hook, []byte("#!/bin/sh\nexit 23\n"), 0o755); err != nil {
				t.Fatal(err)
			}
			opt := NewOptions{Scratch: scratch, Branch: "feat/hook-failure"}
			c, err := f.newWT(opt)
			if err == nil {
				t.Fatal("New succeeded despite failing post-checkout hook")
			}
			if _, statErr := os.Stat(c.Path); !errors.Is(statErr, os.ErrNotExist) {
				t.Fatalf("failed add left worktree %s: %v", c.Path, statErr)
			}
			if !scratch && localHas(f, opt.Branch) {
				t.Fatalf("failed add left branch %s", opt.Branch)
			}
			if strings.Contains(git(t, f.repo, "worktree", "list", "--porcelain"), c.Path) {
				t.Fatalf("failed add left registered worktree %s", c.Path)
			}
		})
	}
}

func TestNewFromExplicitBaseAndFromLinkedWorktree(t *testing.T) {
	f := newFixture(t)
	base := git(t, f.repo, "rev-parse", "HEAD")
	f.advanceOrigin("later.txt")
	other := f.worktree("feat/other")
	c, err := New(context.Background(), f.env, other, NewOptions{Branch: "feat/y", From: base})
	if err != nil {
		t.Fatal(err)
	}
	// Created under the MAIN checkout, not under the linked worktree.
	if c.Path != filepath.Join(f.repo, ".claude", "worktrees", "feat-y") || c.BaseSHA != base || c.Base != base {
		t.Fatalf("created %+v", c)
	}
}

func TestNewRefusesExistingBranchRemoteBranchAndPath(t *testing.T) {
	f := newFixture(t)
	git(t, f.repo, "branch", "feat/local")
	_, err := f.newWT(NewOptions{Branch: "feat/local"})
	if !refusedWith(err, "branch feat/local already exists") {
		t.Fatalf("existing local branch: %v", err)
	}
	git(t, f.seed, "push", "-q", "origin", "HEAD:refs/heads/feat/theirs")
	git(t, f.repo, "config", "--replace-all", "remote.origin.fetch", "+refs/heads/main:refs/remotes/origin/main")
	git(t, f.repo, "update-ref", "-d", "refs/remotes/origin/feat/theirs")
	_, err = f.newWT(NewOptions{Branch: "feat/theirs"})
	if !refusedWith(err, "origin/feat/theirs already exists") {
		t.Fatalf("existing remote branch: %v", err)
	}
	if localHas(f, "feat/theirs") {
		t.Fatal("a refused call created the branch")
	}
	writeFile(t, f.repo, ".claude/worktrees/feat-taken/keep.txt", "mine\n")
	_, err = f.newWT(NewOptions{Branch: "feat/taken"})
	if !refusedWith(err, "already exists") || localHas(f, "feat/taken") {
		t.Fatalf("existing path: %v", err)
	}
	_, err = f.newWT(NewOptions{Branch: "bad..name"})
	if !refusedWith(err, "not a valid branch name") {
		t.Fatalf("invalid name: %v", err)
	}
	_, err = f.newWT(NewOptions{Branch: "feat/z", From: "no-such-ref"})
	if !refusedWith(err, "does not name a commit") || localHas(f, "feat/z") {
		t.Fatalf("bad base: %v", err)
	}
	for _, d := range []string{"../x", "/abs", "a/.git", ".", ".claude", ".claude/worktrees/x"} {
		if _, err := f.newWT(NewOptions{Branch: "feat/c", CloneDirs: []string{d}}); !refusedWith(err, "--clone-dir") {
			t.Fatalf("clone dir %q: %v", d, err)
		}
	}
}

func TestNewClonesDirectoriesCopyOnWrite(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cp -c is macOS-only")
	}
	f := newFixture(t)
	writeFile(t, f.repo, "target/debug/app.bin", "built\n")
	c, err := f.newWT(NewOptions{Branch: "feat/warm", CloneDirs: []string{"target", "node_modules"}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(c.Path, "target", "debug", "app.bin"))
	if err != nil || string(b) != "built\n" {
		t.Fatalf("clone missing: %v %q", err, b)
	}
	if len(c.Cloned) != 1 || c.Cloned[0] != "target" || len(c.Skipped) != 1 || c.Skipped[0].Dir != "node_modules" {
		t.Fatalf("cloned %v skipped %+v", c.Cloned, c.Skipped)
	}
	// The clone is independent of the source.
	writeFile(t, c.Path, "target/debug/app.bin", "changed\n")
	if b, _ := os.ReadFile(filepath.Join(f.repo, "target", "debug", "app.bin")); string(b) != "built\n" {
		t.Fatalf("source changed: %q", b)
	}
}

func TestCloneFailureLeavesNoPartialCopy(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cloning only runs on macOS")
	}
	f := newFixture(t)
	writeFile(t, f.repo, "target/a.bin", "a\n")
	old := cloneCommand
	cloneCommand = func(_ context.Context, _, dst string) error {
		_ = os.MkdirAll(dst, 0o755)
		writeFile(t, dst, "partial", "x")
		return errors.New("clonefile: not supported")
	}
	defer func() { cloneCommand = old }()
	c, err := f.newWT(NewOptions{Branch: "feat/cold", CloneDirs: []string{"target"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Skipped) != 1 || !strings.Contains(c.Skipped[0].Reason, "no full copy") {
		t.Fatalf("skipped %+v", c.Skipped)
	}
	if _, err := os.Stat(filepath.Join(c.Path, "target")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial clone left behind: %v", err)
	}
}

func TestCloneRefusesSymlinkedParentOutsideWorktree(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("cloning only runs on macOS")
	}
	f := newFixture(t)
	outside := filepath.Join(f.root, "outside")
	if err := os.MkdirAll(outside, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(f.seed, "web")); err != nil {
		t.Fatal(err)
	}
	git(t, f.seed, "add", "web")
	git(t, f.seed, "commit", "-qm", "symlink")
	git(t, f.seed, "push", "-q", "origin", "main")
	writeFile(t, f.repo, "web/node_modules/m.js", "x\n") // repo/web is a plain dir (not pulled)
	c, err := f.newWT(NewOptions{Branch: "feat/link", CloneDirs: []string{"web/node_modules"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Skipped) != 1 || !strings.Contains(c.Skipped[0].Reason, "outside") {
		t.Fatalf("skipped %+v cloned %v", c.Skipped, c.Cloned)
	}
	if _, err := os.Stat(filepath.Join(outside, "node_modules")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cloned through the symlink: %v", err)
	}
}

func (f *fixture) scratch() Created {
	f.t.Helper()
	c, err := f.newWT(NewOptions{Scratch: true})
	if err != nil {
		f.t.Fatal(err)
	}
	return c
}

func (f *fixture) find(path string) Worktree {
	f.t.Helper()
	list, err := List(context.Background(), f.env, f.repo)
	if err != nil {
		f.t.Fatal(err)
	}
	w, err := Find(list, f.repo, path)
	if err != nil {
		f.t.Fatal(err)
	}
	return w
}

func TestScratchCreateAndRemoveDiscardsDirtyWork(t *testing.T) {
	f := newFixture(t)
	fresh := f.advanceOrigin("fresh.txt")
	c := f.scratch()
	if !c.Scratch || c.Branch != "" || c.BaseSHA != fresh || !strings.HasPrefix(filepath.Base(c.Path), "scratch-") ||
		filepath.Dir(c.Path) != filepath.Join(f.repo, ".claude", "worktrees") {
		t.Fatalf("created %+v", c)
	}
	w := f.find(c.Path)
	if w.Branch != "" || !w.Locked || !strings.HasPrefix(w.LockReason, scratchLockPrefix) {
		t.Fatalf("scratch must be detached and locked: %+v", w)
	}
	// Ordinary checks refuse it (locked), whatever its state.
	if oc := f.inspect(c.Path); oc.OK || !refusedFor(oc, "locked") {
		t.Fatalf("ordinary inspect: %+v", oc)
	}
	writeFile(t, c.Path, "a.txt", "modified\n")
	writeFile(t, c.Path, "probe.txt", "untracked\n")
	git(t, c.Path, "commit", "-qam", "probe commit")
	head := git(t, c.Path, "rev-parse", "HEAD")
	w = f.find(c.Path)
	sc, err := InspectScratch(context.Background(), f.env, f.repo, w, time.Now(), 0)
	if err != nil || !sc.OK || !sc.Scratch || sc.DirtyFiles != 1 {
		t.Fatalf("inspect: %+v %v", sc, err)
	}
	r, err := RemoveScratch(context.Background(), f.env, f.repo, sc, time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if r.DiscardedHead != head || r.DiscardedDirtyFiles != 1 {
		t.Fatalf("removed %+v", r)
	}
	if _, err := os.Stat(c.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("scratch dir still there: %v", err)
	}
	if strings.Contains(git(t, f.repo, "worktree", "list"), c.Path) {
		t.Fatal("scratch record still registered")
	}
}

func TestScratchRemovalRefusesUnmarkedWorktrees(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/real")
	writeFile(t, path, "wip.txt", "uncommitted work\n")
	sc, err := InspectScratch(context.Background(), f.env, f.repo, f.find(path), time.Now(), 0)
	if err != nil || sc.OK || sc.Scratch || !strings.Contains(strings.Join(sc.Refusals, ";"), "not marked") {
		t.Fatalf("unmarked: %+v %v", sc, err)
	}
	if _, err := RemoveScratch(context.Background(), f.env, f.repo, sc, time.Now(), 0); err == nil {
		t.Fatal("RemoveScratch accepted a refused check")
	}
	// A lock that merely looks like ours is not the marker.
	git(t, f.repo, "worktree", "lock", "--reason", scratchLockPrefix+" forged", path)
	sc, _ = InspectScratch(context.Background(), f.env, f.repo, f.find(path), time.Now(), 0)
	if sc.OK || sc.Scratch {
		t.Fatalf("forged lock reason accepted: %+v", sc)
	}
	// Even a forced call with a doctored check re-inspects and refuses.
	sc.OK, sc.Scratch, sc.Refusals = true, true, nil
	if _, err := RemoveScratch(context.Background(), f.env, f.repo, sc, time.Now(), 0); !errors.Is(err, ErrRefused) {
		t.Fatalf("doctored check: %v", err)
	}
	if _, err := os.Stat(filepath.Join(path, "wip.txt")); err != nil {
		t.Fatalf("unmarked worktree was touched: %v", err)
	}
	// The main checkout is never scratch.
	sc, _ = InspectScratch(context.Background(), f.env, f.repo, f.find(f.repo), time.Now(), 0)
	if sc.OK {
		t.Fatalf("main checkout: %+v", sc)
	}
}

func TestScratchRefusesForeignLockWithAgentflowPrefix(t *testing.T) {
	f := newFixture(t)
	c := f.scratch()
	git(t, f.repo, "worktree", "unlock", c.Path)
	git(t, f.repo, "worktree", "lock", "--reason", scratchLockPrefix+" foreign session", c.Path)
	sc, err := InspectScratch(context.Background(), f.env, f.repo, f.find(c.Path), time.Now(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if sc.OK || !strings.Contains(strings.Join(sc.Refusals, ";"), "locked by someone else") {
		t.Fatalf("foreign prefix lock accepted: %+v", sc)
	}
}

func TestScratchRemovalRefusesReplacementAtSamePath(t *testing.T) {
	f := newFixture(t)
	c := f.scratch()
	old, err := InspectScratch(context.Background(), f.env, f.repo, f.find(c.Path), time.Now(), 0)
	if err != nil || !old.OK {
		t.Fatalf("initial inspect: %+v %v", old, err)
	}
	git(t, f.repo, "worktree", "remove", "--force", "--force", c.Path)
	created := time.Now().Add(time.Minute).UTC()
	marker := ScratchMarker{ID: strings.Repeat("b", 32), Created: created, PID: os.Getpid(), PPID: os.Getppid(), Base: c.Base, BaseSHA: c.BaseSHA}
	reason := scratchLockReason(c.Path, marker)
	git(t, f.repo, "worktree", "add", "--quiet", "--detach", "--lock", "--reason", reason, c.Path, c.BaseSHA)
	if err := writeScratchMarker(context.Background(), f.env, f.repo, c.Path, marker); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoveScratch(context.Background(), f.env, f.repo, old, time.Now().Add(2*time.Minute), 0); !errors.Is(err, ErrRefused) {
		t.Fatalf("replacement removal error = %v, want ErrRefused", err)
	}
	if _, err := os.Stat(c.Path); err != nil {
		t.Fatalf("replacement scratch was removed: %v", err)
	}
}

func TestScratchMarkerFailurePreservesConcurrentWork(t *testing.T) {
	f := newFixture(t)
	old := scratchMarkerWriter
	scratchMarkerWriter = func(_ context.Context, _ Env, _, path string, _ ScratchMarker) error {
		writeFile(t, path, "valuable.txt", "do not delete\n")
		return errors.New("marker storage failed")
	}
	defer func() { scratchMarkerWriter = old }()
	c, err := f.newWT(NewOptions{Scratch: true})
	if err == nil {
		t.Fatal("New succeeded despite marker failure")
	}
	if b, readErr := os.ReadFile(filepath.Join(c.Path, "valuable.txt")); readErr != nil || string(b) != "do not delete\n" {
		t.Fatalf("concurrent work was deleted after marker failure: %q, %v", b, readErr)
	}
}

func TestScratchMarkerWriteRefusesPreexistingTempSymlink(t *testing.T) {
	f := newFixture(t)
	c := f.scratch()
	w := f.find(c.Path)
	m, err := ReadScratch(context.Background(), f.env, f.repo, w)
	if err != nil || m == nil {
		t.Fatalf("read marker: %+v %v", m, err)
	}
	dir, err := adminDir(context.Background(), f.env, f.repo, c.Path)
	if err != nil || dir == "" {
		t.Fatalf("admin dir: %q %v", dir, err)
	}
	if err := os.Remove(filepath.Join(dir, scratchMarkerFile)); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(f.root, "outside-marker-target")
	if err := os.WriteFile(outside, []byte("keep\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, scratchMarkerFile+".tmp")); err != nil {
		t.Fatal(err)
	}
	if err := writeScratchMarker(context.Background(), f.env, f.repo, c.Path, *m); err == nil {
		t.Fatal("marker writer followed a preexisting temporary symlink")
	}
	if b, err := os.ReadFile(outside); err != nil || string(b) != "keep\n" {
		t.Fatalf("marker writer changed symlink target: %q, %v", b, err)
	}
}

func TestScratchUnusedChecksStillApply(t *testing.T) {
	f := newFixture(t)
	c := f.scratch()
	lsof := filepath.Join(f.root, "lsof-busy")
	writeFile(t, f.root, "lsof-busy", "#!/bin/sh\nprintf 'p999999\\ncprobe\\nn%s\\n' '"+c.Path+"'\n")
	if err := os.Chmod(lsof, 0o755); err != nil {
		t.Fatal(err)
	}
	busy := f.env
	busy.Lsof = lsof
	sc, err := InspectScratch(context.Background(), busy, f.repo, f.find(c.Path), time.Now(), 0)
	if err != nil || sc.OK || !strings.Contains(strings.Join(sc.Refusals, ";"), "in use: probe") {
		t.Fatalf("busy scratch: %+v %v", sc, err)
	}
	failing := f.env
	failing.Lsof = filepath.Join(f.root, "no-such-lsof")
	if sc, _ := InspectScratch(context.Background(), failing, f.repo, f.find(c.Path), time.Now(), 0); sc.OK {
		t.Fatalf("lsof failure must refuse: %+v", sc)
	}
	nested := filepath.Join(c.Path, "nested")
	git(t, c.Path, "init", "-q", "-b", "main", nested)
	writeFile(t, nested, "v.txt", "valuable\n")
	git(t, nested, "add", ".")
	git(t, nested, "commit", "-qm", "valuable")
	sc, _ = InspectScratch(context.Background(), f.env, f.repo, f.find(c.Path), time.Now(), 0)
	if sc.OK || !strings.Contains(strings.Join(sc.Refusals, ";"), "nested Git repository") {
		t.Fatalf("nested repo: %+v", sc)
	}
}

func TestScratchMinAgeAndUnreadableMarker(t *testing.T) {
	f := newFixture(t)
	created := time.Now()
	c, err := f.newWT(NewOptions{Scratch: true, Now: created})
	if err != nil {
		t.Fatal(err)
	}
	w := f.find(c.Path)
	sc, _ := InspectScratch(context.Background(), f.env, f.repo, w, created.Add(time.Hour), 24*time.Hour)
	if sc.OK || !strings.Contains(strings.Join(sc.Refusals, ";"), "younger than") {
		t.Fatalf("young scratch: %+v", sc)
	}
	sc, _ = InspectScratch(context.Background(), f.env, f.repo, w, created.Add(25*time.Hour), 24*time.Hour)
	if !sc.OK {
		t.Fatalf("old scratch: %+v", sc)
	}
	dir, err := adminDir(context.Background(), f.env, f.repo, c.Path)
	if err != nil || dir == "" {
		t.Fatalf("admin dir: %q %v", dir, err)
	}
	writeFile(t, dir, scratchMarkerFile, "{not json")
	sc, _ = InspectScratch(context.Background(), f.env, f.repo, w, created.Add(25*time.Hour), 0)
	if sc.OK || !strings.Contains(strings.Join(sc.Refusals, ";"), "unreadable") {
		t.Fatalf("bad marker: %+v", sc)
	}
	writeFile(t, dir, scratchMarkerFile, `{"created":"`+created.UTC().Format(time.RFC3339Nano)+`"}`)
	sc, _ = InspectScratch(context.Background(), f.env, f.repo, w, created.Add(25*time.Hour), 0)
	if sc.OK || !strings.Contains(strings.Join(sc.Refusals, ";"), "invalid scratch marker") {
		t.Fatalf("incomplete marker: %+v", sc)
	}
}

func TestScratchWhoseDirectoryIsGoneIsPruned(t *testing.T) {
	f := newFixture(t)
	c := f.scratch()
	if err := os.RemoveAll(c.Path); err != nil {
		t.Fatal(err)
	}
	sc, err := InspectScratch(context.Background(), f.env, f.repo, f.find(c.Path), time.Now(), 0)
	if err != nil || !sc.OK || !sc.Prunable {
		t.Fatalf("gone scratch: %+v %v", sc, err)
	}
	if _, err := RemoveScratch(context.Background(), f.env, f.repo, sc, time.Now(), 0); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(git(t, f.repo, "worktree", "list"), c.Path) {
		t.Fatal("stale scratch record kept")
	}
}
