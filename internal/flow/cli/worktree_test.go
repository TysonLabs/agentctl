package cli

import (
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
