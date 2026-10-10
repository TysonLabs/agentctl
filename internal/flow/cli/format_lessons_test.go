package cli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The lessons write commands (add, seen, search, triage) render text too.
func TestLessonsWriteCommandsFormatText(t *testing.T) {
	dir := lessonsFixture(t)
	inbox := "# Inbox\n\nNext free id: l2\n"
	if err := os.WriteFile(filepath.Join(dir, "Inbox.md"), []byte(inbox), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_LESSONS_DIR", dir)
	cases := []struct {
		args []string
		want []string
	}{
		{[]string{"lessons", "search", "lock", "--format", "text"}, []string{"lessons search: ok (exit 0)", "- l1 "}},
		{[]string{"lessons", "add", "--dry-run", "--format", "text", "--repo", "alpha", "--ref", "#1", "--topics", "concurrency",
			"--title", "Take the lock first", "--what", "w", "--why", "y", "--avoid", "take the lock first", "--date", "2026-06-02"},
			[]string{"lessons add: ok (exit 0)", "id: l2", "written: false"}},
		{[]string{"lessons", "seen", "l1", "--format", "text", "--note", "again", "--repo", "alpha", "--ref", "#2", "--date", "2026-06-03"},
			[]string{"lessons seen: ok (exit 0)", "used: 2 -> 3"}},
		{[]string{"lessons", "triage", "--format", "text"}, []string{"lessons triage: ok (exit 0)", "items: 0"}},
	}
	for _, c := range cases {
		code, out, errOut := run(t, c.args...)
		if code != 0 {
			t.Fatalf("%v: exit %d\n%s\n%s", c.args, code, out, errOut)
		}
		for _, w := range c.want {
			if !strings.Contains(out, w) {
				t.Errorf("%v: output missing %q:\n%s", c.args[:2], w, out)
			}
		}
		if strings.HasPrefix(strings.TrimSpace(out), "{") {
			t.Errorf("%v: printed JSON in text mode:\n%s", c.args[:2], out)
		}
	}
}
