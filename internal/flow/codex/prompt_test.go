package codex

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	gitT(t, dir, "init", "-q", "-b", "main")
	gitT(t, dir, "config", "user.email", "t@example.com")
	gitT(t, dir, "config", "user.name", "t")
	write(t, dir, "a.go", "package a\n")
	write(t, dir, "b/b.go", "package b\n")
	gitT(t, dir, "add", ".")
	gitT(t, dir, "commit", "-q", "-m", "base")
	return dir
}

func gitT(t *testing.T, dir string, args ...string) string {
	t.Helper()
	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func write(t *testing.T, dir, name, body string) {
	t.Helper()
	p := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestPromptWithoutScopeIsUnchanged(t *testing.T) {
	got, err := BuildPrompt(t.TempDir(), "just this", Scope{}, 0)
	if err != nil || got != "just this" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestBaseScopeInlinesDiff(t *testing.T) {
	dir := gitRepo(t)
	gitT(t, dir, "switch", "-q", "-c", "feat")
	write(t, dir, "a.go", "package a\n\nfunc New() {}\n")
	gitT(t, dir, "commit", "-qam", "change")
	got, err := BuildPrompt(dir, "Find bugs.", Scope{Base: "main"}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"Find bugs.", "main...HEAD", "+func New() {}", "```diff", "Review only the changes"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestCommitScopeAndRenames(t *testing.T) {
	dir := gitRepo(t)
	gitT(t, dir, "mv", "a.go", "renamed.go")
	gitT(t, dir, "commit", "-qm", "rename")
	sha := gitT(t, dir, "rev-parse", "HEAD")
	got, err := BuildPrompt(dir, "p", Scope{Commit: sha}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "rename from a.go") || !strings.Contains(got, "rename to renamed.go") {
		t.Errorf("rename not detected:\n%s", got)
	}
}

func TestUncommittedIncludesUntracked(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.go", "package a // edited\n")
	write(t, dir, "new.go", "package a\n\nvar Fresh = 1\n")
	got, err := BuildPrompt(dir, "p", Scope{Uncommitted: true}, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"package a // edited", "+var Fresh = 1", "new.go"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

func TestPathsNarrowTheDiff(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.go", "package a // A\n")
	write(t, dir, "b/b.go", "package b // B\n")
	write(t, dir, "b/new.go", "package b // NEW\n")
	got, err := BuildPrompt(dir, "p", Scope{Uncommitted: true, Paths: []string{"b"}}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(got, "// A") || !strings.Contains(got, "// B") || !strings.Contains(got, "// NEW") {
		t.Errorf("path filter wrong:\n%s", got)
	}
}

func TestEmptyScopeIsAnError(t *testing.T) {
	dir := gitRepo(t)
	if _, err := BuildPrompt(dir, "p", Scope{Uncommitted: true}, 0); !errors.Is(err, ErrEmptyDiff) {
		t.Fatalf("err = %v, want ErrEmptyDiff", err)
	}
}

func TestSizeCap(t *testing.T) {
	dir := gitRepo(t)
	write(t, dir, "a.go", "package a\n"+strings.Repeat("// filler line\n", 500))
	_, err := BuildPrompt(dir, "p", Scope{Uncommitted: true}, 1000)
	var tl *TooLargeError
	if !errors.As(err, &tl) || tl.Max != 1000 || tl.Size <= 1000 {
		t.Fatalf("err = %v, want TooLargeError over 1000", err)
	}
	if !strings.Contains(err.Error(), "--path") {
		t.Errorf("error should suggest splitting with --path: %v", err)
	}
}

func TestBadRefIsAnError(t *testing.T) {
	dir := gitRepo(t)
	if _, err := BuildPrompt(dir, "p", Scope{Base: "no-such-branch"}, 0); err == nil || errors.Is(err, ErrEmptyDiff) {
		t.Fatalf("err = %v, want a git error (not an empty diff)", err)
	}
}
