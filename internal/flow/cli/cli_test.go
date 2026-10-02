package cli

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func run(t *testing.T, args ...string) (int, string, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := Run(args, &out, &errOut)
	return code, out.String(), errOut.String()
}

func TestUsageErrors(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no command", nil, "Usage:"},
		{"unknown command", []string{"bogus"}, "unknown command"},
		{"nothing to do", []string{"codex"}, "give --prompt"},
		{"two scopes", []string{"codex", "--base", "main", "--uncommitted"}, "mutually exclusive"},
		{"two prompts", []string{"codex", "--prompt", "a", "--prompt-file", "f"}, "mutually exclusive"},
		{"positional prompt", []string{"codex", "review this"}, "unexpected argument"},
		{"path without prompt", []string{"codex", "--base", "main", "--path", "x"}, "--path needs"},
		{"bad flag", []string{"codex", "--sandbox", "danger-full-access"}, "flag provided but not defined"},
		{"bad timeout", []string{"codex", "--prompt", "p", "--timeout", "0s"}, "--timeout"},
		{"bad prompt cap", []string{"codex", "--prompt", "p", "--max-prompt-bytes", "0"}, "--max-prompt-bytes"},
		{"missing dir", []string{"codex", "--prompt", "p", "--dir", "/no/such/dir"}, "not a directory"},
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

func TestEmptyPromptFile(t *testing.T) {
	f := filepath.Join(t.TempDir(), "p.md")
	if err := os.WriteFile(f, []byte(" \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if code, _, errOut := run(t, "codex", "--prompt-file", f); code != 1 || !strings.Contains(errOut, "empty") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
}

func TestEmptyScopeExitsBeforeCodex(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	t.Setenv("AGENTFLOW_CODEX", "/no/such/codex") // must never be reached
	code, _, errOut := run(t, "codex", "--dir", dir, "--uncommitted", "--prompt", "review")
	if code != 1 || !strings.Contains(errOut, "no changes") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
}

func TestNativeEmptyScopeExitsBeforeCodex(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	t.Setenv("AGENTFLOW_CODEX", "/no/such/codex") // must never be reached
	code, _, errOut := run(t, "codex", "--dir", dir, "--uncommitted")
	if code != 1 || !strings.Contains(errOut, "no changes") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
}

func TestOutDirLockRejectsConcurrentRun(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockOutDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := lockOutDir(dir); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("second lock error = %v, want already in use", err)
	}
	unlock()
	unlockAgain, err := lockOutDir(dir)
	if err != nil {
		t.Fatalf("lock after release: %v", err)
	}
	unlockAgain()
}

func TestVersion(t *testing.T) {
	if code, out, _ := run(t, "version"); code != 0 || !strings.HasPrefix(out, "agentflow ") {
		t.Errorf("code=%d out=%q", code, out)
	}
}

func TestClaudeScopeOnlyGetsDefaultBrief(t *testing.T) {
	dir := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(dir, "new.go"), []byte("package x // FRESH\n"), 0o644); err != nil {
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
	code, out, errOut := run(t, "claude", "--dir", dir, "--uncommitted", "--out", t.TempDir())
	if code != 0 || !strings.Contains(out, `"claude_exit": 0`) || !strings.Contains(out, `"agent": "claude"`) {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, out, errOut)
	}
	sent, _ := os.ReadFile(stdinCopy)
	for _, want := range []string{"Review the diff below", "// FRESH", "```diff"} {
		if !strings.Contains(string(sent), want) {
			t.Errorf("prompt missing %q:\n%s", want, sent)
		}
	}
	if code, _, errOut := run(t, "codex", "--prompt", "p", "--max-budget-usd", "1"); code != 1 || !strings.Contains(errOut, "not defined") {
		t.Errorf("--max-budget-usd must be claude-only: exit %d %q", code, errOut)
	}
}
