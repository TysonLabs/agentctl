package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// fakeAgentctl installs a script as $AGENTCTL that answers /agent/version.
func fakeAgentctl(t *testing.T) {
	t.Helper()
	bin := filepath.Join(t.TempDir(), "agentctl")
	sh := `#!/bin/sh
case "$2" in
  rcx.prod)     printf '{"version":"rustpbx 0.4.4\\nGit Commit: f316cb0f\\nGit Branch: HEAD"}\n' ;;
  semver.prod)  echo '{"version":"1.2.3"}' ;;
  down.prod)    echo 'agentctl: transport: connection refused' >&2; exit 3 ;;
  *)            echo "agentctl: unknown service \"$2\" — run: agentctl ls" >&2; exit 1 ;;
esac
`
	if err := os.WriteFile(bin, []byte(sh), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCTL", bin)
}

func TestShipVerifyExitCodes(t *testing.T) {
	fakeAgentctl(t)
	t.Chdir(t.TempDir()) // not a git checkout: no ancestry check
	cases := []struct {
		args       []string
		code       int
		status     string
		wantStderr string
	}{
		{[]string{"ship", "verify", "rcx.prod", "--sha", "f316cb0f9211b2261c554e55074aec2e5d3dff70", "--once"}, 0, "deployed", ""},
		{[]string{"ship", "verify", "--once", "--sha", "F316CB0F", "rcx.prod"}, 0, "deployed", ""},
		{[]string{"ship", "verify", "rcx.prod", "--sha", "1111111", "--once"}, 2, "not_deployed", "running f316cb0f"},
		{[]string{"ship", "verify", "semver.prod", "--sha", "1111111", "--once"}, 3, "unreadable", "no commit"},
		{[]string{"ship", "verify", "nosuch.prod", "--sha", "1111111"}, 1, "agentctl_error", "unknown service"},
		{[]string{"ship", "verify", "down.prod", "--sha", "1111111", "--timeout", "300ms", "--interval", "50ms"}, 124, "timeout", "connection refused"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args[2:], " "), func(t *testing.T) {
			code, out, errOut := run(t, c.args...)
			if code != c.code {
				t.Fatalf("exit %d, want %d (stderr %q)", code, c.code, errOut)
			}
			var res struct{ Status string }
			if err := json.Unmarshal([]byte(out), &res); err != nil || res.Status != c.status {
				t.Fatalf("stdout %q: status %q, want %q (%v)", out, res.Status, c.status, err)
			}
			if !strings.Contains(errOut, c.wantStderr) {
				t.Errorf("stderr %q, want it to contain %q", errOut, c.wantStderr)
			}
		})
	}
}

func TestShipVerifyResolvesRefs(t *testing.T) {
	fakeAgentctl(t)
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	// HEAD resolves to a full SHA that is not f316cb0f: a clean not_deployed.
	code, out, _ := run(t, "ship", "verify", "rcx.prod", "--sha", "HEAD", "--repo", repo, "--once")
	var res struct{ Status, Expected string }
	_ = json.Unmarshal([]byte(out), &res)
	if code != 2 || res.Status != "not_deployed" || len(res.Expected) != 40 {
		t.Fatalf("exit %d, %+v", code, res)
	}
	if code, _, errOut := run(t, "ship", "verify", "rcx.prod", "--sha", "no-such-ref", "--repo", repo); code != 1 || !strings.Contains(errOut, "cannot resolve") {
		t.Errorf("bad ref: exit %d stderr %q", code, errOut)
	}
}

func TestShipVerifyUsageErrors(t *testing.T) {
	fakeAgentctl(t)
	t.Chdir(t.TempDir())
	cases := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"ship", "deploy"}, "unknown subcommand"},
		{[]string{"ship", "verify", "--sha", "1111111"}, "exactly one service"},
		{[]string{"ship", "verify", "a.prod", "b.prod", "--sha", "1111111"}, "exactly one service"},
		{[]string{"ship", "verify", "rcx.prod"}, "--sha is required"},
		{[]string{"ship", "verify", "rcx.prod", "--sha", "origin/main"}, "not a hex SHA"},
		{[]string{"ship", "verify", "rcx.prod", "--sha", "1111111", "--repo", "/no/such/dir"}, "not a git work tree"},
		{[]string{"ship", "verify", "rcx.prod", "--sha", "1111111", "--interval", "0s"}, "must be positive"},
		{[]string{"ship", "verify", "rcx.prod", "--sha", "1111111", "--bogus"}, "flag provided but not defined"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args, " "), func(t *testing.T) {
			if code, _, errOut := run(t, c.args...); code != 1 || !strings.Contains(errOut, c.wantErr) {
				t.Errorf("exit %d stderr %q, want 1 and %q", code, errOut, c.wantErr)
			}
		})
	}
	if code, out, _ := run(t, "ship", "--help"); code != 0 || !strings.Contains(out, "ship verify") {
		t.Errorf("help: exit %d", code)
	}
}
