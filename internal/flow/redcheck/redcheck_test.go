package redcheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(), "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@e", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@e")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newRepo(t *testing.T) string {
	t.Helper()
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	git(t, dir, "init", "-q", "-b", "main")
	return dir
}

// write sets files (nil content deletes) in the working tree.
func write(t *testing.T, dir string, files map[string]*string) {
	t.Helper()
	for name, content := range files {
		p := filepath.Join(dir, name)
		if content == nil {
			if err := os.Remove(p); err != nil {
				t.Fatal(err)
			}
			continue
		}
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(*content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func commit(t *testing.T, dir string, files map[string]*string, msg string) string {
	t.Helper()
	write(t, dir, files)
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "--allow-empty", "-m", msg)
	return git(t, dir, "rev-parse", "HEAD")
}

func s(v string) *string { return &v }

// fingerprint hashes every file of the checkout (working tree and the git
// state a user would notice: index, HEAD, refs, config), skipping the object
// store, which redcheck may add unreachable objects to.
func fingerprint(t *testing.T, dir string) string {
	t.Helper()
	var lines []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		if rel == filepath.Join(".git", "objects") || rel == filepath.Join(".git", "logs") {
			return fs.SkipDir
		}
		if d.IsDir() {
			lines = append(lines, "d "+rel)
			return nil
		}
		b, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		fi, _ := d.Info()
		h := sha256.Sum256(b)
		lines = append(lines, rel+" "+fi.Mode().String()+" "+hex.EncodeToString(h[:]))
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return strings.Join(lines, "\n")
}

func worktreeCount(t *testing.T, dir string) int {
	t.Helper()
	return strings.Count(git(t, dir, "worktree", "list", "--porcelain"), "worktree ")
}

func runCheck(t *testing.T, o Options) Result {
	t.Helper()
	if o.OutDir == "" {
		o.OutDir = t.TempDir()
	}
	if o.Timeout == 0 {
		o.Timeout = 2 * time.Minute
	}
	o.Grace = 200 * time.Millisecond
	before := fingerprint(t, o.Dir)
	res := Run(context.Background(), o)
	if after := fingerprint(t, o.Dir); after != before {
		t.Errorf("the checkout changed during the run")
	}
	assertCleanedUp(t, o.Dir, res)
	return res
}

func assertCleanedUp(t *testing.T, dir string, res Result) {
	t.Helper()
	if n := worktreeCount(t, dir); n != 1 {
		t.Errorf("%d worktrees registered after the run, want 1", n)
	}
	if res.Worktree != "" {
		if _, err := os.Stat(filepath.Dir(res.Worktree)); !os.IsNotExist(err) {
			t.Errorf("temporary dir %s still exists (err=%v)", filepath.Dir(res.Worktree), err)
		}
	}
	if res.CleanupError != "" {
		t.Errorf("cleanup error: %s", res.CleanupError)
	}
}

// shellFixture: src/value.txt holds "broken"; the fix makes it "fixed" and
// adds tests/check.sh, which passes only on the fixed value.
func shellFixture(t *testing.T) (dir, fix string) {
	t.Helper()
	dir = newRepo(t)
	commit(t, dir, map[string]*string{"src/value.txt": s("broken\n"), "README": s("x\n")}, "base")
	fix = commit(t, dir, map[string]*string{
		"src/value.txt":  s("fixed\n"),
		"tests/check.sh": s("grep -q fixed src/value.txt\n"),
	}, "fix")
	return dir, fix
}

func TestRedGoFixture(t *testing.T) {
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go not on PATH")
	}
	dir := newRepo(t)
	commit(t, dir, map[string]*string{
		"go.mod":  s("module example.com/calc\n\ngo 1.21\n"),
		"calc.go": s("package calc\n\nfunc Add(a, b int) int { return a - b }\n"),
	}, "base")
	fix := commit(t, dir, map[string]*string{
		"calc.go":      s("package calc\n\nfunc Add(a, b int) int { return a + b }\n"),
		"calc_test.go": s("package calc\n\nimport \"testing\"\n\nfunc TestAdd(t *testing.T) {\n\tif Add(2, 3) != 5 {\n\t\tt.Fatal(\"Add(2, 3) != 5\")\n\t}\n}\n"),
	}, "fix")
	res := runCheck(t, Options{Dir: dir, Test: "GOFLAGS=-mod=mod go test -count=1 ./...", Scope: Scope{Commit: fix}})
	if res.Status != StatusRed || res.Red == nil || res.Red.Exit == 0 || res.Green == nil || res.Green.Exit != 0 {
		t.Fatalf("got %+v", res)
	}
	if strings.Join(res.Reverted, ",") != "calc.go" || strings.Join(res.KeptTests, ",") != "calc_test.go" {
		t.Errorf("reverted=%v kept_tests=%v", res.Reverted, res.KeptTests)
	}
	if res.After != fix || res.Before != git(t, dir, "rev-parse", fix+"^") {
		t.Errorf("before=%s after=%s", res.Before, res.After)
	}
	if b, _ := os.ReadFile(res.Red.Log); !strings.Contains(string(b), "Add(2, 3) != 5") {
		t.Errorf("red log does not show the failing test:\n%s", b)
	}
}

func TestRedShellFixtureAndBaseScope(t *testing.T) {
	dir, _ := shellFixture(t)
	res := runCheck(t, Options{Dir: dir, Test: "sh tests/check.sh", Scope: Scope{Base: "HEAD~1"}})
	if res.Status != StatusRed || res.Green == nil || res.Green.Exit != 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestNotRed(t *testing.T) {
	dir := newRepo(t)
	commit(t, dir, map[string]*string{"src/value.txt": s("broken\n")}, "base")
	fix := commit(t, dir, map[string]*string{
		"src/value.txt":  s("fixed\n"),
		"tests/check.sh": s("true\n"), // does not look at the fix
	}, "fix")
	res := runCheck(t, Options{Dir: dir, Test: "sh tests/check.sh", Scope: Scope{Commit: fix}})
	if res.Status != StatusNotRed || res.Green != nil || res.Next == "" {
		t.Fatalf("got %+v", res)
	}
}

func TestGreenFails(t *testing.T) {
	dir := newRepo(t)
	commit(t, dir, map[string]*string{"src/value.txt": s("broken\n")}, "base")
	fix := commit(t, dir, map[string]*string{
		"src/value.txt":  s("fixed\n"),
		"tests/check.sh": s("grep -q never src/value.txt\n"),
	}, "fix")
	res := runCheck(t, Options{Dir: dir, Test: "sh tests/check.sh", Scope: Scope{Commit: fix}})
	if res.Status != StatusGreenFailed || res.Red == nil || res.Red.Exit == 0 || res.Green == nil || res.Green.Exit == 0 {
		t.Fatalf("got %+v", res)
	}
	res = runCheck(t, Options{Dir: dir, Test: "sh tests/check.sh", Scope: Scope{Commit: fix}, NoGreen: true})
	if res.Status != StatusRed || res.Green != nil || len(res.Notes) == 0 {
		t.Fatalf("--no-green: got %+v", res)
	}
}

func TestPhasesSeeTheRightFiles(t *testing.T) {
	dir := newRepo(t)
	commit(t, dir, map[string]*string{"src/value.txt": s("broken\n"), "src/old.txt": s("old\n")}, "base")
	fix := commit(t, dir, map[string]*string{
		"src/value.txt":  s("fixed\n"),
		"src/new.txt":    s("new\n"),
		"src/old.txt":    nil,
		"tests/check.sh": s("ls src > \"$OUT/$AGENTFLOW_REDCHECK_PHASE\"; cat src/value.txt >> \"$OUT/$AGENTFLOW_REDCHECK_PHASE\"; grep -q fixed src/value.txt\n"),
	}, "fix")
	out := t.TempDir()
	res := runCheck(t, Options{Dir: dir, Test: "OUT=" + out + " sh tests/check.sh", Scope: Scope{Commit: fix}})
	if res.Status != StatusRed {
		t.Fatalf("got %+v", res)
	}
	red, _ := os.ReadFile(filepath.Join(out, "red"))
	green, _ := os.ReadFile(filepath.Join(out, "green"))
	if string(red) != "old.txt\nvalue.txt\nbroken\n" {
		t.Errorf("red state:\n%s", red)
	}
	if string(green) != "new.txt\nvalue.txt\nfixed\n" {
		t.Errorf("green state:\n%s", green)
	}
}

func TestPhasesRemoveDirectoriesThatDoNotExistInTheSelectedState(t *testing.T) {
	dir := newRepo(t)
	commit(t, dir, map[string]*string{
		"src/value.txt":       s("broken\n"),
		"src/removed/old.txt": s("old\n"),
	}, "base")
	fix := commit(t, dir, map[string]*string{
		"src/value.txt":       s("fixed\n"),
		"src/added/new.txt":   s("new\n"),
		"src/removed/old.txt": nil,
		"tests/check.sh": s(`if [ "$AGENTFLOW_REDCHECK_PHASE" = red ]; then
  test ! -d src/added || exit 8
  test -d src/removed || exit 9
else
  test -d src/added || exit 10
  test ! -d src/removed || exit 11
fi
grep -q fixed src/value.txt
`),
	}, "fix")
	res := runCheck(t, Options{Dir: dir, Test: "sh tests/check.sh", Scope: Scope{Commit: fix}})
	if res.Status != StatusRed || res.Red == nil || res.Red.Exit != 1 || res.Green == nil || res.Green.Exit != 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestUncommittedWithUntrackedTest(t *testing.T) {
	dir := newRepo(t)
	commit(t, dir, map[string]*string{"src/value.txt": s("broken\n"), ".gitignore": s("ignored/\n")}, "base")
	write(t, dir, map[string]*string{
		"src/value.txt":     s("fixed\n"),                       // unstaged
		"tests/check.sh":    s("grep -q fixed src/value.txt\n"), // untracked
		"ignored/build.out": s("not part of the change\n"),      // ignored
	})
	git(t, dir, "add", "src/value.txt") // staged too: the index must survive byte-identical
	write(t, dir, map[string]*string{"src/extra.txt": s("unstaged new\n")})
	status := git(t, dir, "status", "--porcelain")
	res := runCheck(t, Options{Dir: dir, Test: "sh tests/check.sh", Scope: Scope{Uncommitted: true}})
	if res.Status != StatusRed {
		t.Fatalf("got %+v", res)
	}
	if strings.Join(res.KeptTests, ",") != "tests/check.sh" || strings.Join(res.Reverted, ",") != "src/extra.txt,src/value.txt" {
		t.Errorf("kept_tests=%v reverted=%v", res.KeptTests, res.Reverted)
	}
	if got := git(t, dir, "status", "--porcelain"); got != status {
		t.Errorf("status changed:\n%s\nwant:\n%s", got, status)
	}
	if res.Before != git(t, dir, "rev-parse", "HEAD") || res.After == res.Before {
		t.Errorf("before=%s after=%s", res.Before, res.After)
	}
}

func TestUncommittedIncludesForceAddedIgnoredFile(t *testing.T) {
	dir := newRepo(t)
	commit(t, dir, map[string]*string{
		".gitignore":     s("*.fixture\n"),
		"src/value.txt":  s("broken\n"),
		"tests/check.sh": s("test -f data.fixture || exit 5\ngrep -q fixed src/value.txt\n"),
	}, "base")
	write(t, dir, map[string]*string{
		"src/value.txt": s("fixed\n"),
		"data.fixture":  s("staged despite ignore\n"),
	})
	git(t, dir, "add", "-f", "data.fixture")
	res := runCheck(t, Options{
		Dir: dir, Test: "sh tests/check.sh", Scope: Scope{Uncommitted: true}, Keep: []string{"data.fixture"},
	})
	if res.Status != StatusRed || res.Red == nil || res.Red.Exit != 1 || res.Green == nil || res.Green.Exit != 0 {
		t.Fatalf("got %+v", res)
	}
	if strings.Join(res.Kept, ",") != "data.fixture" {
		t.Errorf("kept=%v", res.Kept)
	}
}

func TestUncommittedNothingToCheck(t *testing.T) {
	dir, _ := shellFixture(t)
	res := runCheck(t, Options{Dir: dir, Test: "true", Scope: Scope{Uncommitted: true}})
	if res.Status != StatusError || !strings.Contains(res.Error, "no changes") {
		t.Fatalf("got %+v", res)
	}
}

func TestKeepFixture(t *testing.T) {
	dir := newRepo(t)
	commit(t, dir, map[string]*string{"src/value.txt": s("broken\n")}, "base")
	fix := commit(t, dir, map[string]*string{
		"src/value.txt":     s("fixed\n"),
		"fixtures/data.txt": s("fixture\n"),
		"tests/check.sh":    s("test -f fixtures/data.txt || exit 3; grep -q fixed src/value.txt\n"),
	}, "fix")
	res := runCheck(t, Options{Dir: dir, Test: "sh tests/check.sh", Scope: Scope{Commit: fix}})
	if res.Status != StatusRed || res.Red.Exit != 3 {
		t.Fatalf("without --keep the fixture is reverted (exit 3): got %+v", res.Red)
	}
	res = runCheck(t, Options{Dir: dir, Test: "sh tests/check.sh", Scope: Scope{Commit: fix}, Keep: []string{"fixtures/**", "nomatch/**"}})
	if res.Status != StatusRed || res.Red.Exit != 1 {
		t.Fatalf("with --keep the fixture stays (exit 1 from grep): got %+v", res.Red)
	}
	if strings.Join(res.Kept, ",") != "fixtures/data.txt" || strings.Join(res.Reverted, ",") != "src/value.txt" {
		t.Errorf("kept=%v reverted=%v", res.Kept, res.Reverted)
	}
	if len(res.Notes) != 1 || !strings.Contains(res.Notes[0], "nomatch/**") {
		t.Errorf("notes=%v", res.Notes)
	}
}

const rustBefore = `pub fn add(a: i32, b: i32) -> i32 {
    a - b
}
`

func TestInconclusiveRustTestOutsideModule(t *testing.T) {
	dir := newRepo(t)
	commit(t, dir, map[string]*string{"src/lib.rs": s(rustBefore)}, "base")
	fix := commit(t, dir, map[string]*string{"src/lib.rs": s(`pub fn add(a: i32, b: i32) -> i32 {
    a + b
}

#[test]
fn adds() {
    assert_eq!(add(2, 3), 5);
}
`)}, "fix")
	res := runCheck(t, Options{Dir: dir, Test: "exit 1", Scope: Scope{Commit: fix}})
	if res.Status != StatusInconclusive || len(res.TestInSource) != 1 || res.TestInSource[0].Path != "src/lib.rs" || res.Red != nil {
		t.Fatalf("got %+v", res)
	}
}

func TestRustInlineModuleIsSpliced(t *testing.T) {
	dir := newRepo(t)
	commit(t, dir, map[string]*string{"src/lib.rs": s(rustBefore)}, "base")
	after := `pub fn add(a: i32, b: i32) -> i32 {
    a + b
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn adds() {
        assert_eq!(add(2, 3), 5);
    }
}
`
	fix := commit(t, dir, map[string]*string{"src/lib.rs": s(after)}, "fix")
	out := t.TempDir()
	// The "test" saves what it ran against, then passes only on the fix.
	test := "cp src/lib.rs " + out + "/$AGENTFLOW_REDCHECK_PHASE.rs; grep -q 'a + b' src/lib.rs && grep -q 'fn adds' src/lib.rs"
	res := runCheck(t, Options{Dir: dir, Test: test, Scope: Scope{Commit: fix}})
	if res.Status != StatusRed || strings.Join(res.Spliced, ",") != "src/lib.rs" || len(res.Reverted) != 0 {
		t.Fatalf("got %+v", res)
	}
	red, _ := os.ReadFile(filepath.Join(out, "red.rs"))
	want := rustBefore + after[strings.Index(after, "#[cfg(test)]"):]
	if string(red) != want {
		t.Errorf("spliced file:\n%s\nwant:\n%s", red, want)
	}
	if green, _ := os.ReadFile(filepath.Join(out, "green.rs")); string(green) != after {
		t.Errorf("green file:\n%s", green)
	}

	res = runCheck(t, Options{Dir: dir, Test: test, Scope: Scope{Commit: fix}, NoSplice: true})
	if res.Status != StatusInconclusive || len(res.TestInSource) != 1 {
		t.Fatalf("--no-splice: got %+v", res)
	}
}

func TestRustSpliceUsesBeforeFileMode(t *testing.T) {
	dir := newRepo(t)
	write(t, dir, map[string]*string{"src/lib.rs": s(rustBefore)})
	if err := os.Chmod(filepath.Join(dir, "src/lib.rs"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "base")
	after := rustBefore[:strings.Index(rustBefore, "a - b")] + "a + b\n}\n\n#[cfg(test)]\nmod tests {\n    #[test]\n    fn adds() {}\n}\n"
	write(t, dir, map[string]*string{"src/lib.rs": s(after)})
	if err := os.Chmod(filepath.Join(dir, "src/lib.rs"), 0o644); err != nil {
		t.Fatal(err)
	}
	git(t, dir, "add", "-A")
	git(t, dir, "commit", "-q", "-m", "fix")
	fix := git(t, dir, "rev-parse", "HEAD")
	test := `if [ "$AGENTFLOW_REDCHECK_PHASE" = red ] && [ ! -x src/lib.rs ]; then exit 0; fi
grep -q 'a + b' src/lib.rs`
	res := runCheck(t, Options{Dir: dir, Test: test, Scope: Scope{Commit: fix}})
	if res.Status != StatusRed || res.Green == nil || res.Green.Exit != 0 {
		t.Fatalf("got %+v", res)
	}
}

func TestOnlyTestsChangedIsInconclusive(t *testing.T) {
	dir := newRepo(t)
	commit(t, dir, map[string]*string{"src/value.txt": s("fixed\n")}, "base")
	c := commit(t, dir, map[string]*string{"tests/check.sh": s("grep -q fixed src/value.txt\n")}, "test only")
	res := runCheck(t, Options{Dir: dir, Test: "sh tests/check.sh", Scope: Scope{Commit: c}})
	if res.Status != StatusInconclusive || res.Red != nil {
		t.Fatalf("got %+v", res)
	}
}

func TestCommandNotFoundIsError(t *testing.T) {
	dir, fix := shellFixture(t)
	res := runCheck(t, Options{Dir: dir, Test: "no-such-command-redcheck", Scope: Scope{Commit: fix}})
	if res.Status != StatusError || !strings.Contains(res.Error, "127") {
		t.Fatalf("got %+v", res)
	}
}

func TestCommandNotFoundDuringGreenIsError(t *testing.T) {
	dir, fix := shellFixture(t)
	test := `if [ "$AGENTFLOW_REDCHECK_PHASE" = red ]; then exit 1; fi
no-such-command-redcheck`
	res := runCheck(t, Options{Dir: dir, Test: test, Scope: Scope{Commit: fix}})
	if res.Status != StatusError || !strings.Contains(res.Error, "127") {
		t.Fatalf("got %+v", res)
	}
}

func TestBaseScopeDoesNotRefreshUserIndex(t *testing.T) {
	dir := newRepo(t)
	commit(t, dir, map[string]*string{"src/value.txt": s("broken\n"), "stable.txt": s("stable\n")}, "base")
	commit(t, dir, map[string]*string{
		"src/value.txt":  s("fixed\n"),
		"tests/check.sh": s("grep -q fixed src/value.txt\n"),
	}, "fix")
	old := time.Unix(1, 0)
	if err := os.Chtimes(filepath.Join(dir, "stable.txt"), old, old); err != nil {
		t.Fatal(err)
	}
	res := runCheck(t, Options{Dir: dir, Test: "sh tests/check.sh", Scope: Scope{Base: "HEAD~1"}})
	if res.Status != StatusRed {
		t.Fatalf("got %+v", res)
	}
}

func TestTimeoutCleansUp(t *testing.T) {
	dir, fix := shellFixture(t)
	start := time.Now()
	res := runCheck(t, Options{Dir: dir, Test: "sleep 30", Scope: Scope{Commit: fix}, Timeout: 300 * time.Millisecond})
	if res.Status != StatusTimeout || res.Red == nil || !res.Red.TimedOut {
		t.Fatalf("got %+v", res)
	}
	if time.Since(start) > 20*time.Second {
		t.Errorf("took %s", time.Since(start))
	}
}

func TestCleanupFailureIsAnError(t *testing.T) {
	dir, fix := shellFixture(t)
	fakeGit := filepath.Join(t.TempDir(), "git")
	write(t, filepath.Dir(fakeGit), map[string]*string{
		filepath.Base(fakeGit): s("#!/bin/sh\ncase \" $* \" in *\" worktree prune \"*) exit 99;; esac\nexec git \"$@\"\n"),
	})
	if err := os.Chmod(fakeGit, 0o755); err != nil {
		t.Fatal(err)
	}
	res := Run(context.Background(), Options{
		Dir: dir, Test: "sh tests/check.sh", Scope: Scope{Commit: fix},
		OutDir: t.TempDir(), Timeout: time.Minute, Grace: 200 * time.Millisecond, Git: fakeGit,
	})
	if res.Status != StatusError || res.CleanupError == "" || !strings.Contains(res.Error, "cleanup") {
		t.Fatalf("got %+v", res)
	}
	if n := worktreeCount(t, dir); n != 1 {
		t.Fatalf("%d worktrees registered after cleanup, want 1", n)
	}
}

func TestInterruptCleansUp(t *testing.T) {
	dir, fix := shellFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	marker := filepath.Join(t.TempDir(), "started")
	go func() {
		for {
			if _, err := os.Stat(marker); err == nil {
				cancel()
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	before := fingerprint(t, dir)
	res := Run(ctx, Options{Dir: dir, Test: "touch " + marker + "; sleep 30", Scope: Scope{Commit: fix},
		OutDir: t.TempDir(), Timeout: time.Minute, Grace: 200 * time.Millisecond})
	if res.Status != StatusInterrupted {
		t.Fatalf("got %+v", res)
	}
	if fingerprint(t, dir) != before {
		t.Error("the checkout changed")
	}
	assertCleanedUp(t, dir, res)
}

func TestBadScope(t *testing.T) {
	dir, _ := shellFixture(t)
	root := git(t, dir, "rev-list", "--max-parents=0", "HEAD")
	for _, sc := range []Scope{{Commit: "nope"}, {Commit: root}, {Base: "nope"}} {
		res := runCheck(t, Options{Dir: dir, Test: "true", Scope: sc})
		if res.Status != StatusError || res.Error == "" {
			t.Errorf("%+v: got %+v", sc, res)
		}
	}
}
