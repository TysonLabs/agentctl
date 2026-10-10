package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const cliLesson = `# Topic

### Hold the lock across the check
#cr/concurrency · alpha · 2026-06-01 ^l1

- **Avoid by:** take the lock first.
- **Used:** 2
`

func lessonsFixture(t *testing.T) string {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "Lessons")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "Concurrency.md"), []byte(cliLesson), 0o644); err != nil {
		t.Fatal(err)
	}
	return dir
}

func TestLessonsUsageErrors(t *testing.T) {
	dir := lessonsFixture(t)
	t.Setenv("AGENTFLOW_LESSONS_DIR", "")
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no subcommand", []string{"lessons"}, "agentflow lessons brief"},
		{"unknown subcommand", []string{"lessons", "bogus"}, "unknown subcommand"},
		{"no folder", []string{"lessons", "stats"}, "AGENTFLOW_LESSONS_DIR"},
		{"missing folder", []string{"lessons", "stats", "--lessons-dir", "/no/such"}, "not a directory"},
		{"brief needs topics", []string{"lessons", "brief", "--lessons-dir", dir}, "--topics is required"},
		{"unknown topic", []string{"lessons", "brief", "--lessons-dir", dir, "--topics", "nope"}, "known topics: concurrency"},
		{"bump nothing", []string{"lessons", "bump", "--lessons-dir", dir}, "nothing to bump"},
		{"bad today", []string{"lessons", "retire", "--lessons-dir", dir, "--today", "July"}, "YYYY-MM-DD"},
		{"bad days", []string{"lessons", "retire", "--lessons-dir", dir, "--days", "0"}, "--days"},
		{"flag of another subcommand", []string{"lessons", "brief", "--lessons-dir", dir, "--apply"}, "not defined"},
		{"positional", []string{"lessons", "stats", "--lessons-dir", dir, "extra"}, "unexpected argument"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, errOut := run(t, c.args...)
			if code != 1 || !strings.Contains(errOut, c.wantErr) {
				t.Errorf("code=%d stderr=%q, want 1 and %q", code, errOut, c.wantErr)
			}
		})
	}
}

func TestLessonsCommands(t *testing.T) {
	dir := lessonsFixture(t)
	t.Setenv("AGENTFLOW_LESSONS_DIR", dir)
	code, out, errOut := run(t, "lessons", "brief", "--topics", "concurrency")
	if code != 0 || !strings.Contains(out, "- **l1**: Hold the lock across the check. Check: take the lock first.") {
		t.Fatalf("brief: exit %d\n%s\n%s", code, out, errOut)
	}
	code, out, errOut = run(t, "lessons", "bump", "--used", "l1,l1", "--misled", "l1")
	var bump struct {
		Changes []struct {
			ID, Field string
			From, To  int
		}
	}
	if code != 0 || json.Unmarshal([]byte(out), &bump) != nil || len(bump.Changes) != 2 || bump.Changes[0].To != 4 {
		t.Fatalf("bump: exit %d\n%s\n%s", code, out, errOut)
	}
	before, _ := os.ReadFile(filepath.Join(dir, "Concurrency.md"))
	if code, _, errOut = run(t, "lessons", "bump", "--used", "l1", "--used", "l404"); code != 2 || !strings.Contains(errOut, "l404") {
		t.Errorf("missing id: exit %d %q, want 2", code, errOut)
	}
	if after, _ := os.ReadFile(filepath.Join(dir, "Concurrency.md")); string(after) != string(before) {
		t.Error("a bump that exits 2 must write nothing")
	}
	code, out, _ = run(t, "lessons", "retire", "--today", "2027-01-01")
	if code != 0 || !strings.Contains(out, `"applied": false`) || !strings.Contains(out, `"candidates": []`) {
		t.Errorf("retire: exit %d\n%s", code, out)
	}
	code, out, _ = run(t, "lessons", "stats")
	if code != 0 || !strings.Contains(out, `"lessons": 1`) {
		t.Errorf("stats: exit %d\n%s", code, out)
	}
}

func TestAgentLessonsFlagErrors(t *testing.T) {
	dir := lessonsFixture(t)
	t.Setenv("AGENTFLOW_CODEX", "/no/such/codex") // must never be reached
	t.Setenv("AGENTFLOW_LESSONS_DIR", dir)
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"codex native review", []string{"codex", "--base", "main", "--lessons", "concurrency"}, "--lessons needs --prompt"},
		{"repo without lessons", []string{"codex", "--prompt", "p", "--lessons-repo", "x"}, "need --lessons"},
		{"unknown topic", []string{"codex", "--prompt", "p", "--lessons", "nope"}, "known topics"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, errOut := run(t, c.args...)
			if code != 1 || !strings.Contains(errOut, c.wantErr) {
				t.Errorf("code=%d stderr=%q, want 1 and %q", code, errOut, c.wantErr)
			}
		})
	}
	t.Setenv("AGENTFLOW_LESSONS_DIR", "")
	if code, _, errOut := run(t, "codex", "--prompt", "p", "--lessons", "concurrency"); code != 1 || !strings.Contains(errOut, "AGENTFLOW_LESSONS_DIR") {
		t.Errorf("unset folder: code=%d stderr=%q", code, errOut)
	}
}

func TestClaudeLessonsSectionPrecedesDiff(t *testing.T) {
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"}, {"remote", "add", "origin", "git@example.com:org/alpha.git"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "new.go"), []byte("package x // FRESH\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	stdinCopy := filepath.Join(t.TempDir(), "stdin.txt")
	fake := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\ncat > " + stdinCopy + "\n" +
		`echo '{"type":"system","subtype":"init","session_id":"s1"}'` + "\n" +
		`echo '{"type":"result","subtype":"success","is_error":false,"result":"No findings.","session_id":"s1","total_cost_usd":0.1}'` + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_CLAUDE", fake)
	code, out, errOut := run(t, "claude", "--dir", repo, "--uncommitted", "--out", t.TempDir(),
		"--lessons", "concurrency", "--lessons-dir", lessonsFixture(t))
	if code != 0 {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	sent, _ := os.ReadFile(stdinCopy)
	s := string(sent)
	brief, lesson, diff := strings.Index(s, "Review the diff below"), strings.Index(s, "- **l1**"), strings.Index(s, "```diff")
	if brief < 0 || lesson < 0 || diff < 0 || !(brief < lesson && lesson < diff) {
		t.Errorf("want default brief, then lessons, then diff; got positions %d %d %d:\n%s", brief, lesson, diff, s)
	}
}

func TestOriginRepoName(t *testing.T) {
	repo := t.TempDir()
	if out, err := exec.Command("git", "-C", repo, "init", "-q").CombinedOutput(); err != nil {
		t.Fatalf("git: %v %s", err, out)
	}
	if got := originRepoName(repo); got != "" {
		t.Errorf("no remote: %q", got)
	}
	for url, want := range map[string]string{
		"https://github.com/org/alpha.git": "alpha",
		"git@github.com:org/beta":          "beta",
		"https://github.com/org/gamma/":    "gamma",
	} {
		exec.Command("git", "-C", repo, "remote", "remove", "origin").Run()
		if out, err := exec.Command("git", "-C", repo, "remote", "add", "origin", url).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
		if got := originRepoName(repo); got != want {
			t.Errorf("%s -> %q, want %q", url, got, want)
		}
	}
}
