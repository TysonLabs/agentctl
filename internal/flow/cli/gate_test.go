package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func gateRepo(t *testing.T, steps string) (repo, cfg string) {
	t.Helper()
	repo = t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	cfg = filepath.Join(t.TempDir(), "services.toml")
	content := fmt.Sprintf("[p.meta]\nrepo = %q\n", repo) + steps
	if err := os.WriteFile(cfg, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return repo, cfg
}

func TestGateExitCodes(t *testing.T) {
	cases := []struct {
		name, steps string
		code        int
		status      string
	}{
		{"pass", "[[p.gate.steps]]\nname = \"a\"\nrun = \"true\"\n", 0, "passed"},
		{"fail", "[[p.gate.steps]]\nname = \"a\"\nrun = \"exit 4\"\n", 2, "failed"},
		{"timeout", "[[p.gate.steps]]\nname = \"a\"\nrun = \"sleep 20\"\ntimeout = \"300ms\"\n", 124, "timeout"},
		{"fail beats timeout", "[[p.gate.steps]]\nname = \"a\"\nrun = \"sleep 20\"\ntimeout = \"300ms\"\n[[p.gate.steps]]\nname = \"b\"\nrun = \"false\"\n", 2, "failed"},
		{"no gate", "", 1, "error"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			repo, cfg := gateRepo(t, c.steps)
			code, out, errOut := run(t, "gate", "--dir", repo, "--config", cfg, "--out", t.TempDir())
			if code != c.code {
				t.Fatalf("exit %d, want %d (stderr %s)", code, c.code, errOut)
			}
			var res struct {
				Status string `json:"status"`
				OK     bool   `json:"ok"`
			}
			if err := json.Unmarshal([]byte(out), &res); err != nil || res.Status != c.status || res.OK != (c.code == 0) {
				t.Fatalf("stdout %s: %v", out, err)
			}
		})
	}
}

func TestGateCheckAndUsage(t *testing.T) {
	repo, cfg := gateRepo(t, "[[p.gate.steps]]\nname = \"a\"\nrun = \"true\"\n")
	g := func(args ...string) (int, string, string) {
		return run(t, append([]string{"gate", "--dir", repo, "--config", cfg}, args...)...)
	}
	for _, args := range [][]string{
		{"--tree", "abc"},
		{"--check", "--only", "a"},
		{"--check", "--force"},
		{"--check", "--tree", strings.Repeat("a", 40), "--rev", "HEAD"},
		{"--only", " , "},
		{"extra"},
	} {
		if code, _, _ := g(args...); code != 1 {
			t.Errorf("%q: exit %d, want 1", args, code)
		}
	}
	code, out, _ := g("--check")
	if code != 2 || !strings.Contains(out, `"missing"`) || !strings.Contains(out, `"a"`) {
		t.Fatalf("check before a run: %d %s", code, out)
	}
	if code, _, errOut := g("--out", t.TempDir()); code != 0 {
		t.Fatalf("gate: %d %s", code, errOut)
	}
	if code, out, _ := g("--check", "--rev", "HEAD"); code != 0 || !strings.Contains(out, `"ok": true`) {
		t.Fatalf("check after a run: %d %s", code, out)
	}
	if code, out, _ := run(t, "gate", "--help"); code != 0 || !strings.Contains(out, "receipts.json") {
		t.Fatalf("help: %d", code)
	}
}
