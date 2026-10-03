package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
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
	// The fake gh applies the caller's real --jq expression to gh.json with
	// jq, as gh does, so a malformed expression fails the test.
	if _, err := exec.LookPath("jq"); err != nil {
		t.Skip("jq not installed")
	}
	writeFile(t, root, "gh", "#!/bin/sh\nprintf '%s\\n' \"$*\" > \""+filepath.Join(root, "gh.args")+"\"\n"+
		"expr=''\nwhile [ $# -gt 0 ]; do if [ \"$1\" = --jq ]; then shift; expr=\"$1\"; fi; shift; done\n"+
		"[ -f \""+filepath.Join(root, "gh.json")+"\" ] || exit 0\n"+
		"exec jq -r \"$expr\" \""+filepath.Join(root, "gh.json")+"\"\n")
	if err := os.Chmod(gh, 0o755); err != nil {
		t.Fatal(err)
	}
	lsof := filepath.Join(root, "lsof")
	writeFile(t, root, "lsof", "#!/bin/sh\nexit 0\n")
	if err := os.Chmod(lsof, 0o755); err != nil {
		t.Fatal(err)
	}
	f.env = Env{GH: gh, Lsof: lsof}
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

// setPRs records merged PRs as "number headOid" pairs; the fake gh returns
// them as gh's JSON (headRepository.nameWithOwner = repo, "" for unknown).
func (f *fixture) setPRs(lines ...string) {
	f.setPRsFrom("", lines...)
}

func (f *fixture) setPRsFrom(repo string, lines ...string) {
	prs := []map[string]any{}
	for _, l := range lines {
		num, oid, _ := strings.Cut(strings.TrimSpace(l), " ")
		n, _ := strconv.Atoi(num)
		pr := map[string]any{"number": n, "headRefOid": oid}
		if repo != "" {
			pr["headRepository"] = map[string]any{"nameWithOwner": repo}
		} else {
			pr["headRepository"] = nil
		}
		prs = append(prs, pr)
	}
	b, _ := json.Marshal(prs)
	writeFile(f.t, f.root, "gh.json", string(b))
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
	gitLog := filepath.Join(f.root, "git.log")
	gitWrapper := filepath.Join(f.root, "git-wrapper")
	writeFile(t, f.root, "git-wrapper", "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \""+gitLog+"\"\nexec git \"$@\"\n")
	if err := os.Chmod(gitWrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	f.env.Git = gitWrapper
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
	log, err := os.ReadFile(gitLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(log), "--force-with-lease=refs/heads/feat/a:"+head) {
		t.Errorf("remote deletion was not protected by an exact lease:\n%s", log)
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

func TestLocalBranchDeletionDoesNotDereferenceSymbolicRef(t *testing.T) {
	f := newFixture(t)
	f.worktree("feat/symbolic-race")
	// Make the fetched target equal the feature tip so replacing the feature
	// ref with a symbolic ref to main still satisfies update-ref's old OID.
	git(t, f.seed, "fetch", "-q", "origin")
	git(t, f.seed, "reset", "-q", "--hard", "origin/feat/symbolic-race")
	git(t, f.seed, "push", "-q", "--force", "origin", "main")
	git(t, f.repo, "fetch", "-q", "origin")
	git(t, f.repo, "reset", "-q", "--hard", "origin/main")
	c := f.inspect("feat/symbolic-race")
	mainHead := git(t, f.repo, "rev-parse", "refs/remotes/origin/main")
	if mainHead != c.Head {
		t.Fatalf("test setup target = %s, feature = %s", mainHead, c.Head)
	}

	gitWrapper := filepath.Join(f.root, "git-symbolic-race")
	script := "#!/bin/sh\nif [ \"$1\" = update-ref ]; then\n  git symbolic-ref refs/heads/feat/symbolic-race refs/heads/main\nfi\nexec git \"$@\"\n"
	writeFile(t, f.root, "git-symbolic-race", script)
	if err := os.Chmod(gitWrapper, 0o755); err != nil {
		t.Fatal(err)
	}
	f.env.Git = gitWrapper
	r := f.remove(c, false)
	if !r.BranchDeleted || localHas(f, "feat/symbolic-race") {
		t.Fatalf("named feature ref was not deleted: %+v", r)
	}
	if got := git(t, f.repo, "rev-parse", "refs/heads/main"); got != mainHead {
		t.Fatalf("symbolic feature ref deletion changed main from %s to %s", mainHead, got)
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

func TestIgnoredNestedWorktreeIsRefused(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/parent")
	writeFile(t, path, ".gitignore", "target/\nnested/\n")
	git(t, path, "add", ".gitignore")
	git(t, path, "commit", "-qm", "ignore nested directory")
	git(t, path, "push", "-q", "origin", "feat/parent")
	f.mergeOnOrigin("feat/parent")
	nested := filepath.Join(path, "nested")
	git(t, f.repo, "worktree", "add", "-q", "-b", "feat/child", nested, "origin/main")
	if status := git(t, path, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("nested worktree must be ignored for this regression test, status=%q", status)
	}
	c := f.inspect("feat/parent")
	if c.OK || !refusedFor(c, "contains registered worktree") {
		t.Fatalf("ignored nested worktree must prevent parent deletion: %+v", c)
	}
}

func TestIgnoredNestedRepositoryFromAnotherRepoIsRefused(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/parent")
	writeFile(t, path, ".gitignore", "target/\nnested/\n")
	git(t, path, "add", ".gitignore")
	git(t, path, "commit", "-qm", "ignore nested directory")
	git(t, path, "push", "-q", "origin", "feat/parent")
	f.mergeOnOrigin("feat/parent")
	nested := filepath.Join(path, "nested")
	git(t, path, "init", "-q", "-b", "main", nested)
	writeFile(t, nested, "valuable.txt", "do not delete\n")
	git(t, nested, "add", "valuable.txt")
	git(t, nested, "commit", "-qm", "valuable independent work")
	if status := git(t, path, "status", "--porcelain", "--untracked-files=all"); status != "" {
		t.Fatalf("nested repository must be ignored for this regression test, status=%q", status)
	}
	c := f.inspect("feat/parent")
	if c.OK || !refusedFor(c, "nested Git repository") {
		t.Fatalf("ignored nested repository must prevent parent deletion: %+v", c)
	}
	if _, err := os.Stat(filepath.Join(nested, "valuable.txt")); err != nil {
		t.Fatalf("nested repository was changed during inspection: %v", err)
	}
}

func TestNestedRepositoryDetectionValidatesMetadataAndFindsBareRepos(t *testing.T) {
	root := t.TempDir()
	writeFile(t, root, "fixture/.git/README", "not repository metadata\n")
	bare := filepath.Join(root, "archives", "valuable.git")
	git(t, root, "init", "-q", "--bare", "-b", "main", bare)
	repos, err := nestedRepositories(root, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 || canonical(repos[0]) != canonical(bare) {
		t.Fatalf("nested repositories = %v, want only %s", repos, bare)
	}
}

func TestInitializedSubmoduleIsRefusedBeforeRemove(t *testing.T) {
	f := newFixture(t)
	sub := filepath.Join(f.root, "sub")
	git(t, f.root, "init", "-q", "-b", "main", sub)
	git(t, sub, "commit", "--allow-empty", "-qm", "submodule base")
	git(t, f.repo, "-c", "protocol.file.allow=always", "submodule", "add", "-q", sub, "modules/sub")
	git(t, f.repo, "commit", "-qam", "add submodule")
	git(t, f.repo, "push", "-q", "origin", "main")
	path := f.worktree("feat/submodule")
	git(t, path, "-c", "protocol.file.allow=always", "submodule", "update", "-q", "--init")
	f.mergeOnOrigin("feat/submodule")
	c := f.inspect("feat/submodule")
	if c.OK || !refusedFor(c, "initialized submodule") {
		t.Fatalf("initialized submodule must be reported before git remove refuses: %+v", c)
	}
}

func TestRemoveRechecksHeadAfterInspection(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/race")
	f.mergeOnOrigin("feat/race")
	c := f.inspect("feat/race")
	git(t, path, "switch", "-q", "--detach")
	writeFile(t, path, "later.txt", "new committed work\n")
	git(t, path, "add", "later.txt")
	git(t, path, "commit", "-qm", "commit after inspection")
	_, err := Remove(context.Background(), f.env, f.repo, c, Target{Remote: "origin", Branch: "main"}, true)
	if !errors.Is(err, ErrRefused) {
		t.Fatalf("Remove error = %v, want ErrRefused", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("worktree changed after inspection was removed: %v", err)
	}
}

func TestLsofPartialOutputStillFailsClosed(t *testing.T) {
	dir := t.TempDir()
	lsof := filepath.Join(dir, "lsof")
	writeFile(t, dir, "lsof", "#!/bin/sh\nprintf 'p123\\ncworker\\nn/tmp\\n'\nexit 1\n")
	if err := os.Chmod(lsof, 0o755); err != nil {
		t.Fatal(err)
	}
	if users, err := cwdUsers(context.Background(), Env{Lsof: lsof}, dir); err == nil || users != nil {
		t.Fatalf("cwdUsers = %v, %v; partial lsof output must not be accepted", users, err)
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
	f.env.Lsof = "lsof"
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

func TestRemoteBranchKeptWhenPushURLDiffersFromFetchedRemote(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/push-url")
	f.mergeOnOrigin("feat/push-url")
	f.setPRs("31 " + git(t, path, "rev-parse", "HEAD"))

	other := filepath.Join(f.root, "other.git")
	git(t, f.root, "clone", "-q", "--bare", f.origin, other)
	git(t, f.repo, "remote", "set-url", "--push", "origin", other)
	c := f.inspect("feat/push-url")
	r := f.remove(c, true)
	if r.RemoteDeleted || !remoteHas(t, f, "feat/push-url") || !strings.Contains(r.Note, "push URL identical") {
		t.Fatalf("remote with a different push URL must be kept: %+v", r)
	}
	if got := git(t, f.repo, "ls-remote", "--heads", other, "refs/heads/feat/push-url"); got == "" {
		t.Fatal("different push repository unexpectedly lost its branch")
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

func TestDefaultTargetDistinguishesRemoteFromSlashInBranch(t *testing.T) {
	f := newFixture(t)
	git(t, f.repo, "remote", "add", "upstream", f.origin)
	ctx := context.Background()
	if got, err := DefaultTarget(ctx, f.env, f.repo, "release/1.2"); err != nil || got != (Target{Remote: "origin", Branch: "release/1.2"}) {
		t.Fatalf("branch with slash: got %+v, %v", got, err)
	}
	if got, err := DefaultTarget(ctx, f.env, f.repo, "upstream/release/1.2"); err != nil || got != (Target{Remote: "upstream", Branch: "release/1.2"}) {
		t.Fatalf("explicit remote: got %+v, %v", got, err)
	}
}

func TestGitHubRemoteScopesPRLookup(t *testing.T) {
	f := newFixture(t)
	git(t, f.repo, "remote", "set-url", "origin", "git@github.example:acme/widgets.git")
	repo, owner, ok := githubRemote(context.Background(), f.env, f.repo, "origin")
	if !ok || repo != "github.example/acme/widgets" || owner != "acme" {
		t.Fatalf("githubRemote = %q, %q, %v", repo, owner, ok)
	}
	if !prHeadMatchesRemote("acme/widgets", repo) || prHeadMatchesRemote("acme/widgets-fork", repo) || prHeadMatchesRemote("other/widgets", repo) {
		t.Fatal("remote deletion must require the PR head to come from the exact remote repository")
	}
}

func TestMergedPRProofIsRestrictedToTargetBranch(t *testing.T) {
	f := newFixture(t)
	path := f.worktree("feat/base")
	f.squashOnOrigin("feat/base")
	f.setPRs("9 " + git(t, path, "rev-parse", "HEAD"))
	if c := f.inspect("feat/base"); !c.OK || c.MergedVia != "PR #9" {
		t.Fatalf("got %+v", c)
	}
	args, err := os.ReadFile(filepath.Join(f.root, "gh.args"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(args), "--base main") {
		t.Fatalf("merged PR lookup was not restricted to the requested target branch: %s", args)
	}
	if !strings.Contains(string(args), "--repo "+f.origin) {
		t.Fatalf("merged PR lookup did not explicitly name the target repository: %s", args)
	}
}
