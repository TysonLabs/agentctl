package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TysonLabs/agentctl/internal/flow/redcheck"
)

func TestRedcheckUsageErrors(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"no test", []string{"redcheck", "--uncommitted"}, "--test is required"},
		{"no scope", []string{"redcheck", "--test", "true"}, "exactly one"},
		{"two scopes", []string{"redcheck", "--test", "true", "--base", "main", "--uncommitted"}, "exactly one"},
		{"positional", []string{"redcheck", "--test", "true", "--uncommitted", "extra"}, "unexpected argument"},
		{"bad timeout", []string{"redcheck", "--test", "true", "--uncommitted", "--timeout", "0s"}, "--timeout"},
		{"bad keep", []string{"redcheck", "--test", "true", "--uncommitted", "--keep", "a/["}, "--keep"},
		{"missing dir", []string{"redcheck", "--test", "true", "--uncommitted", "--dir", "/no/such/dir"}, "not a directory"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, errOut := run(t, c.args...)
			if code != 1 || !strings.Contains(errOut, c.wantErr) {
				t.Errorf("code=%d stderr=%q, want 1 and %q", code, errOut, c.wantErr)
			}
		})
	}
	if code, out, _ := run(t, "redcheck", "--help"); code != 0 || !strings.Contains(out, "Exit codes") {
		t.Errorf("--help: code=%d", code)
	}
}

func TestRedcheckExitCodesCoverEveryStatus(t *testing.T) {
	seen := map[int]redcheck.Status{}
	for _, s := range redcheck.AllStatuses {
		code, ok := redcheckExitCodes[s]
		if !ok {
			t.Errorf("no exit code for %s", s)
		}
		if prev, dup := seen[code]; dup {
			t.Errorf("exit %d used by %s and %s", code, prev, s)
		}
		seen[code] = s
	}
	if len(redcheckExitCodes) != len(redcheck.AllStatuses) {
		t.Errorf("redcheckExitCodes has %d entries, want %d", len(redcheckExitCodes), len(redcheck.AllStatuses))
	}
}

func TestRedcheckEndToEnd(t *testing.T) {
	dir := t.TempDir()
	gitc := func(args ...string) {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.email=t@e", "-c", "user.name=t"}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	put := func(name, content string) {
		t.Helper()
		p := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	gitc("init", "-q")
	put("src/v.txt", "broken\n")
	gitc("add", "-A")
	gitc("commit", "-qm", "base")
	put("src/v.txt", "fixed\n")
	put("tests/check.sh", "grep -q fixed src/v.txt\n")

	out := filepath.Join(t.TempDir(), "out")
	code, stdout, errOut := run(t, "redcheck", "--dir", dir, "--test", "sh tests/check.sh", "--uncommitted", "--out", out)
	if code != 0 {
		t.Fatalf("code=%d stderr=%s\n%s", code, errOut, stdout)
	}
	var res redcheck.Result
	if err := json.Unmarshal([]byte(stdout), &res); err != nil {
		t.Fatal(err)
	}
	if res.Status != redcheck.StatusRed || res.Green == nil || filepath.Dir(res.Red.Log) != out {
		t.Errorf("got %+v", res)
	}
	if _, err := os.Stat(filepath.Join(out, "result.json")); err != nil {
		t.Error(err)
	}

	// The test does not need the fix: not red, exit 2.
	code, _, errOut = run(t, "redcheck", "--dir", dir, "--test", "true", "--uncommitted")
	if code != 2 || !strings.Contains(errOut, "not_red") {
		t.Errorf("code=%d stderr=%s", code, errOut)
	}
	// A hang: timeout, exit 124.
	code, _, _ = run(t, "redcheck", "--dir", dir, "--test", "sleep 30", "--uncommitted", "--timeout", "200ms")
	if code != 124 {
		t.Errorf("timeout: code=%d", code)
	}
}
