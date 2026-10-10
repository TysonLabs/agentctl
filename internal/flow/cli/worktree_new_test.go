package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWorktreeNewUsageErrors(t *testing.T) {
	t.Chdir(t.TempDir()) // not a git checkout
	cases := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"worktree", "new"}, "exactly one new branch"},
		{[]string{"worktree", "new", "a", "b"}, "exactly one new branch"},
		{[]string{"worktree", "new", "--scratch", "a"}, "takes no branch"},
		{[]string{"worktree", "new", "a", "--from", "x", "--at", "y"}, "same option"},
		{[]string{"worktree", "new", "a", "--yes"}, "flag provided but not defined"},
		{[]string{"worktree", "new", "a"}, "not inside a git checkout"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			if code, _, errOut := run(t, c.args...); code != 1 || !strings.Contains(errOut, c.wantErr) {
				t.Errorf("exit %d stderr %q, want 1 and %q", code, errOut, c.wantErr)
			}
		})
	}
}

func TestWorktreeNewScratchSweepAndDone(t *testing.T) {
	root := t.TempDir()
	origin := filepath.Join(root, "origin.git")
	seed := filepath.Join(root, "seed")
	repo := filepath.Join(root, "repo")
	cliGit(t, root, "init", "-q", "--bare", "-b", "main", origin)
	cliGit(t, root, "clone", "-q", origin, seed)
	if err := os.WriteFile(filepath.Join(seed, "a.txt"), []byte("a\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cliGit(t, seed, "add", ".")
	cliGit(t, seed, "commit", "-qm", "base")
	cliGit(t, seed, "push", "-q", "origin", "main")
	cliGit(t, root, "clone", "-q", origin, repo)
	lsof := filepath.Join(root, "lsof")
	if err := os.WriteFile(lsof, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_LSOF", lsof)
	t.Setenv("AGENTFLOW_GH", filepath.Join(root, "no-gh"))

	code, out, errOut := run(t, "worktree", "new", "feat/x", "--repo", repo)
	var made struct {
		Path, Branch, Base string
		BaseSHA            string `json:"base_sha"`
	}
	if code != 0 || json.Unmarshal([]byte(out), &made) != nil || made.Branch != "feat/x" || made.Base != "origin/main" {
		t.Fatalf("new: exit %d out %s err %s", code, out, errOut)
	}
	// Dirty, so neither sweep --yes below nor done may remove it.
	if err := os.WriteFile(filepath.Join(made.Path, "wip.txt"), []byte("wip\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	code, out, _ = run(t, "worktree", "new", "feat/x", "--repo", repo)
	if code != 2 || !strings.Contains(out, `"refusals"`) || !strings.Contains(out, "already exists") {
		t.Fatalf("second new: exit %d out %s", code, out)
	}

	code, out, errOut = run(t, "worktree", "new", "--scratch", "--repo", repo)
	var scratch struct {
		Path    string
		Scratch bool
	}
	if code != 0 || json.Unmarshal([]byte(out), &scratch) != nil || !scratch.Scratch {
		t.Fatalf("new --scratch: exit %d out %s err %s", code, out, errOut)
	}
	if err := os.WriteFile(filepath.Join(scratch.Path, "probe.txt"), []byte("dirty\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	type sweep struct {
		Worktrees []struct{ Path string }
		Scratch   []struct {
			Path     string
			OK       bool
			Refusals []string
			Removed  json.RawMessage
		}
		ScratchRemovable int `json:"scratch_removable"`
		ScratchRemoved   int `json:"scratch_removed"`
	}
	code, out, errOut = run(t, "worktree", "sweep", "--repo", repo)
	var s sweep
	if code != 0 || json.Unmarshal([]byte(out), &s) != nil {
		t.Fatalf("sweep: exit %d out %s err %s", code, out, errOut)
	}
	if len(s.Scratch) != 1 || s.Scratch[0].Path != scratch.Path || s.Scratch[0].OK ||
		!strings.Contains(strings.Join(s.Scratch[0].Refusals, ";"), "younger than 24h") {
		t.Fatalf("scratch listing: %s", out)
	}
	for _, w := range s.Worktrees {
		if w.Path == scratch.Path {
			t.Fatalf("scratch listed with ordinary worktrees: %s", out)
		}
	}
	// --yes with the default age keeps the young scratch.
	code, out, _ = run(t, "worktree", "sweep", "--yes", "--repo", repo)
	if json.Unmarshal([]byte(out), &s) != nil || code != 0 || s.ScratchRemoved != 0 {
		t.Fatalf("sweep --yes removed a young scratch: %s", out)
	}
	if _, err := os.Stat(scratch.Path); err != nil {
		t.Fatal(err)
	}

	code, out, errOut = run(t, "worktree", "done", scratch.Path, "--dry-run", "--repo", repo)
	if code != 0 || !strings.Contains(out, `"ok": true`) || strings.Contains(out, `"removed"`) {
		t.Fatalf("done --dry-run: exit %d out %s err %s", code, out, errOut)
	}
	code, out, errOut = run(t, "worktree", "done", scratch.Path, "--repo", repo)
	if code != 0 || !strings.Contains(out, `"discarded_dirty_files": 1`) {
		t.Fatalf("done scratch: exit %d out %s err %s", code, out, errOut)
	}
	if _, err := os.Stat(scratch.Path); !os.IsNotExist(err) {
		t.Fatalf("scratch still there: %v", err)
	}

	// The ordinary dirty worktree still gets the full checks.
	code, _, errOut = run(t, "worktree", "done", made.Path, "--repo", repo)
	if code != 2 || !strings.Contains(errOut, "would be lost") {
		t.Fatalf("ordinary done: exit %d err %s", code, errOut)
	}
	if _, err := os.Stat(filepath.Join(made.Path, "wip.txt")); err != nil {
		t.Fatal(err)
	}
}
