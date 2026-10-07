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
	headOut, err := exec.Command("git", "-C", repo, "rev-parse", "HEAD").Output()
	if err != nil {
		t.Fatal(err)
	}
	head := strings.TrimSpace(string(headOut))
	code, out, _ = run(t, "ship", "verify", "rcx.prod", "--sha", head[:8], "--repo", repo, "--once")
	_ = json.Unmarshal([]byte(out), &res)
	if code != 2 || res.Expected != head {
		t.Fatalf("abbreviated SHA: exit %d, %+v", code, res)
	}
	if code, _, errOut := run(t, "ship", "verify", "rcx.prod", "--sha", "no-such-ref", "--repo", repo); code != 1 || !strings.Contains(errOut, "cannot resolve") {
		t.Errorf("bad ref: exit %d stderr %q", code, errOut)
	}
	// A hex SHA this clone doesn't have (an unfetched merge, another repo's
	// commit) is compared as text, not rejected.
	if code, _, errOut := run(t, "ship", "verify", "rcx.prod", "--sha", "1111111", "--repo", repo, "--once"); code != 2 {
		t.Errorf("unknown abbreviated SHA: exit %d stderr %q, want 2 (not deployed)", code, errOut)
	}
	if code, out, errOut := run(t, "ship", "verify", "rcx.prod", "--sha", "f316cb0f", "--repo", repo, "--once"); code != 0 {
		t.Errorf("running commit absent from the clone: exit %d stdout %q stderr %q, want 0 (textual match)", code, out, errOut)
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
		{[]string{"ship", "verify", "rcx.prod", "--sha", "1111111", "--contains"}, "--contains needs"},
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
	if code, _, errOut := run(t, "ship", "verify", "--", "rcx.prod", "--sha", "1111111"); code != 1 || !strings.Contains(errOut, "exactly one service") {
		t.Errorf("-- terminator: exit %d stderr %q", code, errOut)
	}
}

func TestShipAnnounceChainsFromVerify(t *testing.T) {
	fakeAgentctl(t)
	dir := t.TempDir()
	t.Chdir(dir)
	t.Setenv("XDG_STATE_HOME", filepath.Join(dir, "state"))
	cfg := filepath.Join(dir, "services.toml")
	if err := os.WriteFile(cfg, []byte("[rcx.prod]\nbase_url = \"https://rcx.example.com\"\ntoken = \"REPLACE_ME\"\n\n"+
		"[rcx.announce]\nwebhook = \"https://hooks.slack.com/services/T0FAKE1/B0FAKE1/fakeSecretPart123\"\nchannel = \"#rcx-releases\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCTL_CONFIG", cfg)

	code, out, _ := run(t, "ship", "verify", "rcx.prod", "--sha", "f316cb0f", "--once")
	var v struct {
		Status    string
		CheckedAt string `json:"checked_at"`
	}
	if err := json.Unmarshal([]byte(out), &v); code != 0 || err != nil || v.CheckedAt == "" {
		t.Fatalf("verify: exit %d, %q (%v)", code, out, err)
	}
	proof := filepath.Join(dir, "verify.json")
	body := filepath.Join(dir, "note.md")
	_ = os.WriteFile(proof, []byte(out), 0o600)
	_ = os.WriteFile(body, []byte("What changed: x.\nHow to test: y."), 0o600)

	code, out, errOut := run(t, "ship", "announce", "rcx.prod", "--verified", proof, "--title", "T", "--body-file", body, "--dry-run")
	var a struct {
		Status, Channel string
		Payload         json.RawMessage
	}
	if err := json.Unmarshal([]byte(out), &a); code != 0 || err != nil || a.Status != "dry_run" || a.Channel != "#rcx-releases" || len(a.Payload) == 0 {
		t.Fatalf("dry run: exit %d stdout %q stderr %q", code, out, errOut)
	}
	if strings.Contains(out+errOut, "fakeSecretPart123") {
		t.Errorf("output leaks the webhook")
	}

	// A not-deployed verify is no proof.
	_, notYet, _ := run(t, "ship", "verify", "rcx.prod", "--sha", "1111111", "--once")
	_ = os.WriteFile(proof, []byte(notYet), 0o600)
	if code, _, errOut := run(t, "ship", "announce", "rcx.prod", "--verified", proof, "--title", "T", "--body-file", body, "--dry-run"); code != 2 || !strings.Contains(errOut, "no proof") {
		t.Errorf("unverified: exit %d stderr %q, want 2", code, errOut)
	}
}

func TestShipAnnounceUsageErrors(t *testing.T) {
	dir := t.TempDir()
	body := filepath.Join(dir, "note.md")
	_ = os.WriteFile(body, []byte("x"), 0o600)
	cases := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"ship", "announce", "--verified", body, "--title", "T", "--body-file", body}, "exactly one service"},
		{[]string{"ship", "announce", "rcx.prod", "--title", "T", "--body-file", body}, "are required"},
		{[]string{"ship", "announce", "rcx.prod", "--verified", body, "--title", "T", "--body-file", body}, "not ship verify JSON"},
		{[]string{"ship", "announce", "rcx.prod", "--verified", "/no/such", "--title", "T", "--body-file", body}, "reading --verified"},
		{[]string{"ship", "announce", "rcx.prod", "--verified", body, "--title", "T", "--body-file", body, "--max-age", "0s"}, "must be positive"},
	}
	for _, c := range cases {
		t.Run(strings.Join(c.args[2:], " "), func(t *testing.T) {
			if code, _, errOut := run(t, c.args...); code != 1 || !strings.Contains(errOut, c.wantErr) {
				t.Errorf("exit %d stderr %q, want 1 and %q", code, errOut, c.wantErr)
			}
		})
	}
}
