package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"os/signal"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"

	"github.com/TysonLabs/agentctl/internal/flow/branch"
)

// syncFixture is a bare origin, a "seed" clone that plays the other
// developers (it pushes to the base branch), and the repo under test on a
// feature branch.
type syncFixture struct {
	t                  *testing.T
	root, origin, seed string
	repo               string
	base               string
}

func newSyncFixture(t *testing.T, base string) *syncFixture {
	t.Helper()
	// Keep the developer's global git config (signing, hooks, merge.ff)
	// out of the fixture.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_NOSYSTEM", "1")
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	f := &syncFixture{t: t, root: root, origin: filepath.Join(root, "origin.git"), seed: filepath.Join(root, "seed"), repo: filepath.Join(root, "repo"), base: base}
	cliGit(t, root, "init", "-q", "--bare", "-b", base, f.origin)
	cliGit(t, root, "clone", "-q", f.origin, f.seed)
	cliGit(t, f.seed, "checkout", "-q", "-b", base)
	writeFile(t, f.seed, "a.txt", "a1\na2\na3\n")
	writeFile(t, f.seed, "d.txt", "d\n")
	cliGit(t, f.seed, "add", ".")
	cliGit(t, f.seed, "commit", "-qm", "base")
	cliGit(t, f.seed, "push", "-q", "origin", base)
	cliGit(t, root, "clone", "-q", f.origin, f.repo)
	cliGit(t, f.repo, "config", "user.email", "t@e")
	cliGit(t, f.repo, "config", "user.name", "t")
	cliGit(t, f.repo, "checkout", "-q", "-b", "feat/x")
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

// commit writes files ("" deletes) in dir and commits them.
func (f *syncFixture) commit(dir, msg string, files map[string]string) string {
	f.t.Helper()
	for name, body := range files {
		if body == "" {
			cliGit(f.t, dir, "rm", "-q", name)
			continue
		}
		writeFile(f.t, dir, name, body)
		cliGit(f.t, dir, "add", name)
	}
	cliGit(f.t, dir, "commit", "-qm", msg)
	return cliGit(f.t, dir, "rev-parse", "HEAD")
}

// upstream commits on the base branch and pushes it.
func (f *syncFixture) upstream(msg string, files map[string]string) string {
	f.t.Helper()
	sha := f.commit(f.seed, msg, files)
	cliGit(f.t, f.seed, "push", "-q", "origin", f.base)
	return sha
}

func (f *syncFixture) sync(args ...string) (int, branchResult, string) {
	f.t.Helper()
	code, out, errOut := run(f.t, append([]string{"branch", "sync", "--dir", f.repo}, args...)...)
	var res branchResult
	if out != "" {
		if err := json.Unmarshal([]byte(out), &res); err != nil {
			f.t.Fatalf("bad JSON (exit %d): %v\n%s\nstderr: %s", code, err, out, errOut)
		}
	}
	return code, res, errOut
}

func (f *syncFixture) mergeInProgress() bool {
	f.t.Helper()
	_, err := os.Stat(filepath.Join(f.repo, ".git", "MERGE_HEAD"))
	return err == nil
}

func TestBranchSyncReport(t *testing.T) {
	f := newSyncFixture(t, "main")
	f.commit(f.repo, "ours", map[string]string{"a.txt": "a1\nours\na3\n", "b.txt": "b\n"})
	f.upstream("one", map[string]string{"c.txt": "c\n"})
	f.upstream("two", map[string]string{"a.txt": "theirs\na2\na3\n"})
	three := f.upstream("three", map[string]string{"c.txt": "c2\n"})
	head := cliGit(t, f.repo, "rev-parse", "HEAD")

	code, res, errOut := f.sync()
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	if res.Branch != "feat/x" || res.Head != head || res.Base != "origin/main" || res.BaseSHA != three {
		t.Errorf("identity: %+v", res.Report)
	}
	if res.Ahead != 1 || res.Behind != 3 || res.IncomingTotal != 3 || res.UpToDate {
		t.Errorf("counts: ahead %d behind %d total %d up_to_date %v", res.Ahead, res.Behind, res.IncomingTotal, res.UpToDate)
	}
	var subjects []string
	for _, c := range res.Incoming {
		subjects = append(subjects, c.Subject)
	}
	if !reflect.DeepEqual(subjects, []string{"three", "two", "one"}) || res.Incoming[0].SHA != three {
		t.Errorf("incoming: %+v", res.Incoming)
	}
	if !reflect.DeepEqual(res.OverlappingFiles, []string{"a.txt"}) {
		t.Errorf("overlapping_files: %v, want [a.txt]", res.OverlappingFiles)
	}
	if res.MergeBase == "" || !strings.Contains(res.Next, "--merge") || res.Merge != nil {
		t.Errorf("merge_base %q next %q merge %v", res.MergeBase, res.Next, res.Merge)
	}
	if got := cliGit(t, f.repo, "rev-parse", "HEAD"); got != head {
		t.Errorf("report-only run moved HEAD to %s", got)
	}

	_, res, _ = f.sync("--max-incoming", "2")
	if len(res.Incoming) != 2 || res.IncomingTotal != 3 {
		t.Errorf("capped: %d listed, total %d", len(res.Incoming), res.IncomingTotal)
	}
}

func TestBranchSyncUpToDate(t *testing.T) {
	f := newSyncFixture(t, "main")
	f.commit(f.repo, "ours", map[string]string{"b.txt": "b\n"})
	head := cliGit(t, f.repo, "rev-parse", "HEAD")
	code, res, _ := f.sync("--merge")
	if code != 0 || !res.UpToDate || res.Ahead != 1 || res.Behind != 0 || res.Merge != nil || res.Next != "" {
		t.Errorf("exit %d result %+v", code, res)
	}
	if len(res.Incoming) != 0 || len(res.OverlappingFiles) != 0 {
		t.Errorf("expected empty lists: %+v", res.Report)
	}
	if got := cliGit(t, f.repo, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD moved")
	}
}

func TestBranchSyncCleanMerge(t *testing.T) {
	f := newSyncFixture(t, "main")
	ours := f.commit(f.repo, "ours", map[string]string{"b.txt": "b\n"})
	theirs := f.upstream("theirs", map[string]string{"c.txt": "c\n"})
	code, res, errOut := f.sync("--merge", "--abort-on-conflict")
	if code != 0 || res.Merge == nil {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	head := cliGit(t, f.repo, "rev-parse", "HEAD")
	if res.Merge.Commit != head || res.Merge.FastForward || res.Merge.InProgress {
		t.Errorf("merge: %+v (HEAD %s)", res.Merge, head)
	}
	if parents := cliGit(t, f.repo, "rev-parse", "HEAD^1", "HEAD^2"); parents != ours+"\n"+theirs {
		t.Errorf("parents %q, want ours then theirs", parents)
	}
	if subj := cliGit(t, f.repo, "log", "-1", "--format=%s"); !strings.Contains(subj, "origin/main") || !strings.Contains(subj, "feat/x") {
		t.Errorf("merge subject %q", subj)
	}
	if f.mergeInProgress() || cliGit(t, f.repo, "status", "--porcelain") != "" {
		t.Error("tree not clean after a clean merge")
	}
}

func TestBranchSyncMergeOverridesFFOnlyConfig(t *testing.T) {
	f := newSyncFixture(t, "main")
	cliGit(t, f.repo, "config", "merge.ff", "only")
	f.commit(f.repo, "ours", map[string]string{"b.txt": "b\n"})
	f.upstream("theirs", map[string]string{"c.txt": "c\n"})
	if code, res, errOut := f.sync("--merge"); code != 0 || res.Merge == nil || res.Merge.FastForward {
		t.Fatalf("exit %d merge %+v: %s", code, res.Merge, errOut)
	}
}

func TestBranchSyncFastForward(t *testing.T) {
	f := newSyncFixture(t, "main")
	theirs := f.upstream("theirs", map[string]string{"c.txt": "c\n"})
	code, res, errOut := f.sync("--merge")
	if code != 0 || res.Merge == nil || !res.Merge.FastForward || res.Merge.Commit != theirs {
		t.Fatalf("exit %d merge %+v: %s", code, res.Merge, errOut)
	}
}

// conflictSetup makes every conflict kind a two-sided merge can produce
// with renames off: both modified, both added, deleted by them, deleted by us.
func conflictSetup(f *syncFixture) {
	f.commit(f.seed, "seed z", map[string]string{"z.txt": "z\n"})
	cliGit(f.t, f.seed, "push", "-q", "origin", f.base)
	cliGit(f.t, f.repo, "pull", "-q", "--no-rebase", "origin", f.base)
	f.commit(f.repo, "ours", map[string]string{"a.txt": "a1\nours\na3\n", "e.txt": "ours\n", "d.txt": "d ours\n", "z.txt": ""})
	f.upstream("theirs", map[string]string{"a.txt": "a1\ntheirs\na3\n", "e.txt": "theirs\n", "d.txt": "", "z.txt": "z theirs\n"})
}

func TestBranchSyncConflictLeftInProgress(t *testing.T) {
	f := newSyncFixture(t, "main")
	conflictSetup(f)
	writeFile(t, f.repo, "scratch.md", "keep me\n") // untracked: allowed, and kept
	code, res, errOut := f.sync("--merge")
	if code != 2 || res.Merge == nil {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	want := []struct{ path, kind string }{
		{"a.txt", "both modified"}, {"d.txt", "deleted by them"}, {"e.txt", "both added"}, {"z.txt", "deleted by us"},
	}
	if len(res.Merge.Conflicts) != len(want) {
		t.Fatalf("conflicts %+v", res.Merge.Conflicts)
	}
	for i, w := range want {
		if c := res.Merge.Conflicts[i]; c.Path != w.path || c.Kind != w.kind {
			t.Errorf("conflict %d = %+v, want %s %s", i, c, w.path, w.kind)
		}
	}
	if !res.Merge.InProgress || res.Merge.Aborted || !f.mergeInProgress() {
		t.Errorf("merge should be left in progress: %+v", res.Merge)
	}
	if !strings.Contains(res.Next, "git add") || !strings.Contains(res.Next, "git merge --abort") {
		t.Errorf("next %q", res.Next)
	}
	if b, _ := os.ReadFile(filepath.Join(f.repo, "scratch.md")); string(b) != "keep me\n" {
		t.Errorf("untracked file changed: %q", b)
	}

	// A second run refuses: a merge is in progress and files are unmerged.
	code, res, _ = f.sync("--merge")
	if code != 2 || len(res.Refusals) != 2 || !strings.Contains(res.Refusals[0], "merge is in progress") || !strings.Contains(res.Refusals[1], "uncommitted") {
		t.Errorf("exit %d refusals %q", code, res.Refusals)
	}
	if !f.mergeInProgress() {
		t.Error("refused run must not touch the in-progress merge")
	}
}

func TestBranchSyncAbortOnConflict(t *testing.T) {
	f := newSyncFixture(t, "main")
	conflictSetup(f)
	writeFile(t, f.repo, "scratch.md", "keep me\n")
	head := cliGit(t, f.repo, "rev-parse", "HEAD")
	before := cliGit(t, f.repo, "status", "--porcelain", "--untracked-files=all")
	code, res, errOut := f.sync("--merge", "--abort-on-conflict")
	if code != 2 || res.Merge == nil || !res.Merge.Aborted || res.Merge.InProgress || len(res.Merge.Conflicts) != 4 {
		t.Fatalf("exit %d merge %+v: %s", code, res.Merge, errOut)
	}
	if f.mergeInProgress() {
		t.Error("merge still in progress after --abort-on-conflict")
	}
	if got := cliGit(t, f.repo, "rev-parse", "HEAD"); got != head {
		t.Errorf("HEAD %s, want %s", got, head)
	}
	if after := cliGit(t, f.repo, "status", "--porcelain", "--untracked-files=all"); after != before {
		t.Errorf("status changed:\nbefore %q\nafter  %q", before, after)
	}
}

func TestBranchSyncRefusesDirtyTree(t *testing.T) {
	f := newSyncFixture(t, "main")
	f.upstream("theirs", map[string]string{"c.txt": "c\n"})
	writeFile(t, f.repo, "a.txt", "local edit\n")
	head := cliGit(t, f.repo, "rev-parse", "HEAD")
	code, res, errOut := f.sync("--merge")
	if code != 2 || len(res.Refusals) != 1 || !strings.Contains(res.Refusals[0], "a.txt") || res.Merge != nil {
		t.Fatalf("exit %d refusals %q: %s", code, res.Refusals, errOut)
	}
	if !strings.Contains(errOut, "refused:") || res.Behind != 1 {
		t.Errorf("stderr %q behind %d", errOut, res.Behind)
	}
	if b, _ := os.ReadFile(filepath.Join(f.repo, "a.txt")); string(b) != "local edit\n" || cliGit(t, f.repo, "rev-parse", "HEAD") != head {
		t.Error("refused merge changed the tree")
	}
	// Staged-only changes are just as dirty.
	cliGit(t, f.repo, "add", "a.txt")
	if code, res, _ := f.sync("--merge"); code != 2 || len(res.Refusals) != 1 {
		t.Errorf("staged: exit %d refusals %q", code, res.Refusals)
	}
}

func TestBranchSyncRefusesDirtyTreeWhenUpToDate(t *testing.T) {
	f := newSyncFixture(t, "main")
	writeFile(t, f.repo, "a.txt", "local edit\n")
	head := cliGit(t, f.repo, "rev-parse", "HEAD")

	code, res, errOut := f.sync("--merge")
	if code != 2 || len(res.Refusals) != 1 || !strings.Contains(res.Refusals[0], "a.txt") || res.Merge != nil {
		t.Fatalf("exit %d refusals %q merge %+v: %s", code, res.Refusals, res.Merge, errOut)
	}
	if !res.UpToDate || res.Behind != 0 {
		t.Errorf("report should still be up to date: %+v", res.Report)
	}
	if b, _ := os.ReadFile(filepath.Join(f.repo, "a.txt")); string(b) != "local edit\n" || cliGit(t, f.repo, "rev-parse", "HEAD") != head {
		t.Error("refused merge changed the tree")
	}
}

func TestBranchSyncRefusesSkipWorktreeTrackedChange(t *testing.T) {
	f := newSyncFixture(t, "main")
	f.upstream("theirs", map[string]string{"a.txt": "upstream\n"})
	writeFile(t, f.repo, "a.txt", "local hidden edit\n")
	cliGit(t, f.repo, "update-index", "--skip-worktree", "a.txt")
	if got := cliGit(t, f.repo, "status", "--porcelain=v1", "--untracked-files=no"); got != "" {
		t.Fatalf("fixture edit is not hidden from status: %q", got)
	}
	head := cliGit(t, f.repo, "rev-parse", "HEAD")

	code, res, errOut := f.sync("--merge")
	if code != 2 || len(res.Refusals) != 1 || !strings.Contains(res.Refusals[0], "a.txt") || res.Merge != nil {
		t.Fatalf("exit %d refusals %q merge %+v: %s", code, res.Refusals, res.Merge, errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(f.repo, "a.txt")); string(b) != "local hidden edit\n" || cliGit(t, f.repo, "rev-parse", "HEAD") != head {
		t.Error("refused merge changed the hidden tracked edit")
	}
	if got := cliGit(t, f.repo, "ls-files", "-v", "a.txt"); got != "S a.txt" {
		t.Errorf("refusal changed skip-worktree state: %q", got)
	}
}

func TestBranchSyncMergeClassifiesLateTrackedChange(t *testing.T) {
	f := newSyncFixture(t, "main")
	f.upstream("theirs", map[string]string{"a.txt": "upstream\n"})
	code, report, errOut := f.sync()
	if code != 0 {
		t.Fatalf("report exit %d: %s", code, errOut)
	}
	writeFile(t, f.repo, "a.txt", "late local edit\n")

	result, err := branch.Merge(context.Background(), branch.Env{}, f.repo, report.Report, branch.Base{Remote: "origin", Branch: "main"}, false)
	if err != nil || result.Outcome != branch.TrackedChanges || !strings.Contains(result.GitOutput, "a.txt") {
		t.Fatalf("outcome %v error %v output %q, want tracked-change refusal", result.Outcome, err, result.GitOutput)
	}
	if b, _ := os.ReadFile(filepath.Join(f.repo, "a.txt")); string(b) != "late local edit\n" {
		t.Errorf("late tracked edit changed: %q", b)
	}
}

func TestBranchSyncRefusesOnBaseAndDetached(t *testing.T) {
	f := newSyncFixture(t, "main")
	f.upstream("theirs", map[string]string{"c.txt": "c\n"})
	cliGit(t, f.repo, "checkout", "-q", "main")
	code, res, _ := f.sync("--merge")
	if code != 2 || len(res.Refusals) != 1 || !strings.Contains(res.Refusals[0], "base branch") {
		t.Errorf("on base: exit %d refusals %q", code, res.Refusals)
	}
	cliGit(t, f.repo, "checkout", "-q", "--detach")
	code, res, _ = f.sync("--merge")
	if code != 2 || res.Branch != "" || len(res.Refusals) != 1 || !strings.Contains(res.Refusals[0], "detached") {
		t.Errorf("detached: exit %d branch %q refusals %q", code, res.Branch, res.Refusals)
	}
	// The report alone works on both.
	if code, res, _ := f.sync(); code != 0 || res.Behind != 1 {
		t.Errorf("report: exit %d behind %d", code, res.Behind)
	}
}

func TestBranchSyncRefusesUntrackedOverwrite(t *testing.T) {
	f := newSyncFixture(t, "main")
	f.commit(f.repo, "ours", map[string]string{"b.txt": "b\n"})
	f.upstream("theirs", map[string]string{"new.txt": "theirs\n"})
	writeFile(t, f.repo, "new.txt", "mine, untracked\n")
	head := cliGit(t, f.repo, "rev-parse", "HEAD")
	code, res, errOut := f.sync("--merge")
	if code != 2 || len(res.Refusals) != 1 || !strings.Contains(res.Refusals[0], "untracked") {
		t.Fatalf("exit %d refusals %q: %s", code, res.Refusals, errOut)
	}
	if b, _ := os.ReadFile(filepath.Join(f.repo, "new.txt")); string(b) != "mine, untracked\n" {
		t.Errorf("untracked file overwritten: %q", b)
	}
	if f.mergeInProgress() || cliGit(t, f.repo, "rev-parse", "HEAD") != head {
		t.Error("tree changed")
	}
}

func TestBranchSyncDerivesBaseFromRemote(t *testing.T) {
	f := newSyncFixture(t, "trunk")
	theirs := f.upstream("theirs", map[string]string{"c.txt": "c\n"})
	code, res, errOut := f.sync()
	if code != 0 || res.Base != "origin/trunk" || res.BaseSHA != theirs || res.Behind != 1 {
		t.Fatalf("exit %d base %q sha %s behind %d: %s", code, res.Base, res.BaseSHA, res.Behind, errOut)
	}

	// The remote renames its default branch: the clone's origin/HEAD is now
	// stale, and the remote's answer wins.
	cliGit(t, f.seed, "push", "-q", "origin", "trunk:refs/heads/stable")
	cliGit(t, f.origin, "symbolic-ref", "HEAD", "refs/heads/stable")
	if code, res, _ := f.sync(); code != 0 || res.Base != "origin/stable" || res.BaseSource != "remote HEAD" {
		t.Errorf("renamed default: exit %d base %q source %q", code, res.Base, res.BaseSource)
	}

	// --base overrides, as REMOTE/BRANCH or BRANCH.
	for _, arg := range []string{"origin/trunk", "trunk"} {
		if code, res, _ := f.sync("--base", arg); code != 0 || res.Base != "origin/trunk" || res.BaseSource != "--base" {
			t.Errorf("--base %s: exit %d base %q", arg, code, res.Base)
		}
	}
	if code, _, errOut := f.sync("--base", "origin/nope"); code != 3 || !strings.Contains(errOut, "cannot fetch") {
		t.Errorf("missing base: exit %d %q", code, errOut)
	}
}

func TestBranchSyncUsageErrors(t *testing.T) {
	t.Chdir(t.TempDir())
	cases := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"branch", "merge"}, "unknown subcommand"},
		{[]string{"branch", "sync", "--abort-on-conflict"}, "needs --merge"},
		{[]string{"branch", "sync", "--max-incoming", "-1"}, "negative"},
		{[]string{"branch", "sync", "extra"}, "unexpected argument"},
		{[]string{"branch", "sync", "--into", "x"}, "flag provided but not defined"},
		{[]string{"branch", "sync"}, "not inside a git checkout"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			if code, _, errOut := run(t, c.args...); code != 1 || !strings.Contains(errOut, c.wantErr) {
				t.Errorf("exit %d stderr %q, want 1 and %q", code, errOut, c.wantErr)
			}
		})
	}
	if code, out, _ := run(t, "branch", "--help"); code != 0 || !strings.Contains(out, "never rebases") {
		t.Errorf("help: exit %d", code)
	}
}

func TestBranchSyncInterruptedDuringCheckoutCheck(t *testing.T) {
	f := newSyncFixture(t, "main")
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var stdout, stderr bytes.Buffer

	code := runBranch(ctx, []string{"sync", "--dir", f.repo}, &stdout, &stderr)
	if code != 130 || stdout.Len() != 0 || !strings.Contains(stderr.String(), "interrupted") {
		t.Fatalf("exit %d stdout %q stderr %q, want interruption", code, stdout.String(), stderr.String())
	}
}

func TestBranchSyncInterruptedDuringRefusalChecks(t *testing.T) {
	f := newSyncFixture(t, "main")
	f.upstream("theirs", map[string]string{"c.txt": "c\n"})
	code, report, errOut := f.sync()
	if code != 0 {
		t.Fatalf("report exit %d: %s", code, errOut)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	res := branchResult{Report: report.Report}
	var stderr bytes.Buffer

	code = branchMerge(ctx, branch.Env{}, f.repo, branch.Base{Remote: "origin", Branch: "main"}, &res, false, &stderr)
	if code != 130 || !strings.Contains(stderr.String(), "interrupted") {
		t.Fatalf("exit %d stderr %q, want interruption", code, stderr.String())
	}
}

func TestBranchSyncInterruptedDuringMergeWaitsForGit(t *testing.T) {
	f := newSyncFixture(t, "main")
	theirs := f.upstream("theirs", map[string]string{"c.txt": "c\n"})
	wrapper := filepath.Join(f.root, "git-wrapper")
	script := `#!/bin/sh
for arg in "$@"; do
	if [ "$arg" = merge ]; then
		kill -USR1 "$AGENTFLOW_TEST_PID"
		sleep 0.1
		break
	fi
done
exec git "$@"
`
	if err := os.WriteFile(wrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_GIT", wrapper)
	t.Setenv("AGENTFLOW_TEST_PID", strconv.Itoa(os.Getpid()))
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGUSR1)
	defer stop()
	var stdout, stderr bytes.Buffer

	code := runBranch(ctx, []string{"sync", "--dir", f.repo, "--merge"}, &stdout, &stderr)
	var res branchResult
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("bad JSON %q: %v", stdout.String(), err)
	}
	if code != 130 || ctx.Err() == nil || res.Merge == nil || res.Merge.Commit != theirs || !res.Merge.FastForward {
		t.Fatalf("exit %d canceled %v merge %+v stderr %q", code, ctx.Err(), res.Merge, stderr.String())
	}
	if !strings.Contains(stderr.String(), "interrupted") || f.mergeInProgress() {
		t.Errorf("stderr %q merge-in-progress %v", stderr.String(), f.mergeInProgress())
	}
}
