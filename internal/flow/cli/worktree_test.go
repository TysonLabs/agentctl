package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeUsageErrors(t *testing.T) {
	t.Chdir(t.TempDir()) // not a git checkout
	cases := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"worktree", "finish"}, "unknown subcommand"},
		{[]string{"worktree", "done"}, "exactly one"},
		{[]string{"worktree", "done", "a", "b"}, "exactly one"},
		{[]string{"worktree", "sweep", "a"}, "no positional"},
		{[]string{"worktree", "sweep", "--dry-run"}, "flag provided but not defined"},
		{[]string{"worktree", "done", "x", "--yes"}, "flag provided but not defined"},
		{[]string{"worktree", "done", "x"}, "not inside a git checkout"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			if code, _, errOut := run(t, c.args...); code != 1 || !strings.Contains(errOut, c.wantErr) {
				t.Errorf("exit %d stderr %q, want 1 and %q", code, errOut, c.wantErr)
			}
		})
	}
	if code, out, _ := run(t, "worktree", "--help"); code != 0 || !strings.Contains(out, "never forces") {
		t.Errorf("help: exit %d", code)
	}
}

func cliGit(t *testing.T, dir string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-c", "user.email=t@e", "-c", "user.name=t"}, args...)...)
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}
	return strings.TrimSpace(string(out))
}

func TestSweepContinuesAfterOneWorktreeErrors(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	repo := filepath.Join(root, "repo")
	cliGit(t, root, "init", "-q", "--bare", "-b", "main", origin)
	cliGit(t, root, "clone", "-q", origin, seed)
	cliGit(t, seed, "commit", "--allow-empty", "-qm", "base")
	cliGit(t, seed, "push", "-q", "origin", "main")
	cliGit(t, root, "clone", "-q", origin, repo)

	worktrees := map[string]string{}
	for _, branch := range []string{"feat/good", "feat/error"} {
		path := filepath.Join(root, strings.ReplaceAll(branch, "/", "-"))
		worktrees[branch] = path
		cliGit(t, repo, "worktree", "add", "-q", "-b", branch, path, "origin/main")
		change := strings.ReplaceAll(branch, "/", "-") + ".txt"
		if err := os.WriteFile(filepath.Join(path, change), []byte(branch+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		cliGit(t, path, "add", change)
		cliGit(t, path, "commit", "-qm", branch)
		cliGit(t, path, "push", "-q", "origin", branch)
	}
	cliGit(t, seed, "fetch", "-q", "origin")
	for _, branch := range []string{"feat/good", "feat/error"} {
		cliGit(t, seed, "merge", "-q", "--no-ff", "-m", "merge "+branch, "origin/"+branch)
	}
	cliGit(t, seed, "push", "-q", "origin", "main")

	gitWrapper := filepath.Join(root, "git-wrapper")
	badPath := worktrees["feat/error"]
	script := "#!/bin/sh\nif [ \"$1\" = status ] && [ \"${PWD##*/}\" = feat-error ]; then\n  echo injected status failure >&2\n  exit 7\nfi\nexec git \"$@\"\n"
	if err := os.WriteFile(gitWrapper, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	lsof := filepath.Join(root, "lsof")
	if err := os.WriteFile(lsof, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_GIT", gitWrapper)
	t.Setenv("AGENTFLOW_LSOF", lsof)

	code, out, errOut := run(t, "worktree", "sweep", "--yes", "--keep-remote", "--repo", repo)
	if code != 3 || !strings.Contains(errOut, "could not be processed") {
		t.Fatalf("exit=%d stderr=%q, want a reported partial failure", code, errOut)
	}
	var got struct {
		Removed   int `json:"removed"`
		Worktrees []struct {
			Path    string          `json:"path"`
			Error   string          `json:"error"`
			Removed json.RawMessage `json:"removed"`
		} `json:"worktrees"`
	}
	if err := json.Unmarshal([]byte(out), &got); err != nil {
		t.Fatalf("invalid JSON %q: %v", out, err)
	}
	if got.Removed != 1 || len(got.Worktrees) != 2 {
		t.Fatalf("partial sweep result = %+v", got)
	}
	var errorsSeen int
	for _, w := range got.Worktrees {
		if w.Error != "" {
			errorsSeen++
		}
	}
	if errorsSeen != 1 {
		t.Fatalf("worktree errors = %d, want 1: %s", errorsSeen, out)
	}
	if _, err := os.Stat(worktrees["feat/good"]); !os.IsNotExist(err) {
		t.Fatalf("safe worktree was not removed after another entry failed: %v", err)
	}
	if _, err := os.Stat(badPath); err != nil {
		t.Fatalf("errored worktree was removed: %v", err)
	}
}
