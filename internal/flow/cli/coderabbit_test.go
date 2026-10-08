package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TysonLabs/agentctl/internal/flow/coderabbit"
)

func TestCodeRabbitUsageErrors(t *testing.T) {
	t.Setenv("AGENTFLOW_CODERABBIT", "/nonexistent/coderabbit") // never run: each case fails first
	dir := t.TempDir()
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"coderabbit", "--base", "a", "--uncommitted"}, "mutually exclusive"},
		{[]string{"coderabbit", "--base", "a", "--base-commit", "abc1234"}, "mutually exclusive"},
		{[]string{"coderabbit", "--base", "--deep"}, "must not start with '-'"},
		{[]string{"coderabbit", "--base-commit", "HEAD~1"}, "not a 7-40 character hex commit"},
		{[]string{"coderabbit", "--timeout", "0s"}, "--timeout must be positive"},
		{[]string{"coderabbit", "--stall", "-1s"}, "--stall must not be negative"},
		{[]string{"coderabbit", "--config", filepath.Join(dir, "missing.md"), "--dir", dir}, "is not a readable file"},
		{[]string{"coderabbit", "--config", dir, "--dir", dir}, "is not a readable file"},
		{[]string{"coderabbit", "--dir", filepath.Join(dir, "nope")}, "is not a directory"},
		{[]string{"coderabbit", "--dir", dir}, "not inside a git repository"},
		{[]string{"coderabbit", "extra"}, "unexpected argument"},
		{[]string{"coderabbit", "--bogus"}, "flag provided but not defined"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if code := Run(c.args, &out, &errb); code != 1 || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%v: exit %d, stderr %q; want 1 and %q", c.args, code, errb.String(), c.want)
		}
	}
}

func TestCodeRabbitHelp(t *testing.T) {
	for _, args := range [][]string{{"coderabbit", "--help"}, {"coderabbit", "-h"}} {
		var out, errb bytes.Buffer
		if code := Run(args, &out, &errb); code != 0 || !strings.Contains(out.String(), "untrusted review data") {
			t.Errorf("%v: exit %d, stdout %q", args, code, out.String())
		}
	}
	var out, errb bytes.Buffer
	Run([]string{"--help"}, &out, &errb)
	if !strings.Contains(out.String(), "agentflow coderabbit") {
		t.Error("top-level usage does not list coderabbit")
	}
}

func TestCodeRabbitExitCodesCoverEveryStatus(t *testing.T) {
	all := []coderabbit.Status{coderabbit.StatusClean, coderabbit.StatusFindings, coderabbit.StatusFailed,
		coderabbit.StatusNoResult, coderabbit.StatusRateLimited, coderabbit.StatusTimeout,
		coderabbit.StatusStalled, coderabbit.StatusInterrupted}
	seen := map[int]coderabbit.Status{}
	for _, s := range all {
		code, ok := coderabbitExitCodes[s]
		if !ok {
			t.Errorf("no exit code for %s", s)
		}
		if prev, dup := seen[code]; dup {
			t.Errorf("exit %d used by %s and %s", code, prev, s)
		}
		seen[code] = s
	}
	if len(coderabbitExitCodes) != len(all) {
		t.Errorf("coderabbitExitCodes has %d entries, want %d", len(coderabbitExitCodes), len(all))
	}
}

// crRepo is a git repo whose origin/HEAD points at origin/trunk, and a fake
// coderabbit script that records its argv and prints a clean review.
func crRepo(t *testing.T) (repo, rec string) {
	t.Helper()
	repo, rec = t.TempDir(), t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "trunk"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"update-ref", "refs/remotes/origin/trunk", "HEAD"},
		{"symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	script := filepath.Join(rec, "coderabbit")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + filepath.Join(rec, "args.txt") + "\"\npwd > \"" + filepath.Join(rec, "pwd.txt") + "\"\n" +
		`echo '{"type":"complete","status":"review_completed","findings":0,"reviewedFiles":[],"outcome":"completed"}'` + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_CODERABBIT", script)
	return repo, rec
}

func TestCodeRabbitDefaultScopeAndConfigPaths(t *testing.T) {
	repo, rec := crRepo(t)
	// --config is relative to the invocation cwd, not to --dir.
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	var out, errb bytes.Buffer
	code := Run([]string{"coderabbit", "--dir", repo, "--deep", "--config", "AGENTS.md", "--out", filepath.Join(t.TempDir(), "o")}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, stdout %s", code, errb.String(), out.String())
	}
	args, _ := os.ReadFile(filepath.Join(rec, "args.txt"))
	want := "review\n--agent\n--deep\n--committed\n--base\norigin/trunk\n-c\n" + filepath.Join(cwd, "AGENTS.md") + "\n"
	// macOS temp dirs resolve through /private; compare without it.
	if strings.ReplaceAll(string(args), "/private", "") != strings.ReplaceAll(want, "/private", "") {
		t.Fatalf("argv = %q, want %q", args, want)
	}
	pwd, _ := os.ReadFile(filepath.Join(rec, "pwd.txt"))
	if strings.ReplaceAll(strings.TrimSpace(string(pwd)), "/private", "") != strings.ReplaceAll(repo, "/private", "") {
		t.Fatalf("ran in %q, want %q", pwd, repo)
	}
	var res coderabbit.Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Status != coderabbit.StatusClean {
		t.Fatalf("result %s: %v", out.String(), err)
	}
}

func TestCodeRabbitRefusesWhileRepoLocked(t *testing.T) {
	repo, rec := crRepo(t)
	unlock, _, err := coderabbit.LockRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var out, errb bytes.Buffer
	if code := Run([]string{"coderabbit", "--dir", repo}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "another agentflow coderabbit run") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(rec, "args.txt")); err == nil {
		t.Fatal("coderabbit ran despite the lock")
	}
}
