package cli

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TysonLabs/agentctl/internal/keychain/keychaintest"
)

func run(t *testing.T, stdin string, args ...string) (int, string, string) {
	t.Helper()
	var out, errb strings.Builder
	code := Run(args, strings.NewReader(stdin), &out, &errb)
	return code, out.String(), errb.String()
}

func TestUsageAndExitCodes(t *testing.T) {
	cfgPath := filepath.Join(t.TempDir(), "services.toml")
	if code, _, _ := run(t, "", "--help"); code != 0 {
		t.Errorf("--help exit %d", code)
	}
	if code, _, _ := run(t, ""); code != 2 {
		t.Errorf("no args exit %d", code)
	}
	if code, _, _ := run(t, "", "bogus"); code != 2 {
		t.Errorf("unknown command exit %d", code)
	}
	if code, _, _ := run(t, "", "set", "a.prod", "--config", cfgPath); code != 2 {
		t.Errorf("set without --base-url exit %d", code)
	}
	if code, _, _ := run(t, "", "ui", "--idle", "5s", "--config", cfgPath); code != 2 {
		t.Errorf("ui with a tiny --idle exit %d", code)
	}
	if code, _, errs := run(t, "", "set", "a.prod", "--base-url", "ftp://x", "--config", cfgPath); code != 1 || !strings.Contains(errs, "scheme") {
		t.Errorf("bad URL: exit %d %s", code, errs)
	}
	if code, out, _ := run(t, "", "ls", "--config", cfgPath); code != 0 || !strings.Contains(out, "no services") {
		t.Errorf("empty ls: exit %d %s", code, out)
	}
	for _, args := range [][]string{
		{"ls", "--base-url", "https://x", "--config", cfgPath},
		{"set", "a.prod", "--base-url", "https://x", "--idle", "1m", "--config", cfgPath},
		{"ui", "--base-url", "https://x", "--config", cfgPath},
		{"version", "extra"},
		{"set", "not-a-full-name", "--base-url", "https://x", "--config", cfgPath},
		{"meta", "bad.name", "repo=x", "--config", cfgPath},
		{"meta", "pay", "repo=a", "repo=b", "--config", cfgPath},
		{"meta", "pay", "=x", "--config", cfgPath},
		{"rm", "not-a-full-name", "--config", cfgPath},
		{"test", "not-a-full-name", "--config", cfgPath},
	} {
		if code, _, _ := run(t, "", args...); code != 2 {
			t.Errorf("%q exit %d, want usage exit 2", args, code)
		}
	}
}

func TestPipedTokenInputPreservesWhitespaceForValidation(t *testing.T) {
	for _, in := range []string{
		"valid_token_123 ",
		" valid_token_123\n",
		"valid_token_123\nsecond-line\n",
	} {
		code, _, errs := run(t, in, "token", "svc.prod")
		if code != 1 || !strings.Contains(errs, "spaces or control characters") {
			t.Errorf("input %q: exit %d, stderr %q", in, code, errs)
		}
	}

	for _, in := range []string{"valid_token_123", "valid_token_123\n", "valid_token_123\r\n", "valid_token_123\r"} {
		got, err := readTokenValue(strings.NewReader(in), false)
		if err != nil || got != "valid_token_123" {
			t.Errorf("input %q: got %q, %v", in, got, err)
		}
	}
}

func TestPipedTokenInputRejectsTruncation(t *testing.T) {
	_, err := readTokenValue(strings.NewReader(strings.Repeat("a", maxTokenBytes+1)), false)
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("overlong token error = %v", err)
	}
}

func TestTerminalEchoFailureRefusesToken(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := io.WriteString(f, "valid_token_123\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	a := &app{
		stdin: f, stderr: &stderr,
		isTerminal: func(*os.File) bool { return true },
		setEcho: func(*os.File, string) error {
			return errors.New("stty failed")
		},
	}
	if tok, err := a.readToken("svc.prod"); err == nil || tok != "" {
		t.Fatalf("readToken = %q, %v; want refusal", tok, err)
	}
	pos, err := f.Seek(0, io.SeekCurrent)
	if err != nil {
		t.Fatal(err)
	}
	if pos != 0 {
		t.Fatalf("read %d bytes after echo disable failed", pos)
	}
}

func TestTerminalEchoRestoreFailureWarns(t *testing.T) {
	f, err := os.CreateTemp(t.TempDir(), "stdin")
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if _, err := io.WriteString(f, "valid_token_123\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	var stderr strings.Builder
	a := &app{
		stdin: f, stderr: &stderr,
		isTerminal: func(*os.File) bool { return true },
		setEcho: func(_ *os.File, arg string) error {
			if arg == "echo" {
				return errors.New("stty failed")
			}
			return nil
		},
	}
	if tok, err := a.readToken("svc.prod"); err != nil || tok != "valid_token_123" || !strings.Contains(stderr.String(), "stty echo") {
		t.Fatalf("readToken = %q, %v, stderr %q; want the token and a stty echo warning", tok, err, stderr.String())
	}
}

func TestSetTokenLsMigrateTest(t *testing.T) {
	keychaintest.Temp(t)
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer kc_cli_token_1" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Write([]byte(`{"version":"v42"}`))
	}))
	defer svc.Close()

	cfgPath := filepath.Join(t.TempDir(), "services.toml")
	os.WriteFile(cfgPath, []byte("[old.prod]\nbase_url = \"https://old.example.com\"\ntoken = \"old_plain_token_1\"\n"), 0o600)
	c := func(stdin string, args ...string) (int, string, string) {
		return run(t, stdin, append(args, "--config", cfgPath)...)
	}

	if code, _, errs := c("", "set", "pay.prod", "--base-url", svc.URL); code != 0 {
		t.Fatalf("set: %d %s", code, errs)
	}
	if code, _, errs := c("", "meta", "pay", "repo=~/src/pay", "unit=pay.service"); code != 0 {
		t.Fatalf("meta: %d %s", code, errs)
	}
	if code, _, _ := c("", "test", "pay.prod"); code != 3 {
		t.Fatalf("test without a token: exit %d, want 3", code)
	}
	if code, _, errs := c("kc_cli_token_1\n", "token", "pay.prod"); code != 0 {
		t.Fatalf("token: %d %s", code, errs)
	}
	code, out, _ := c("", "ls")
	if code != 0 || strings.Contains(out, "kc_cli_token_1") || !strings.Contains(out, "keychain tok:") || !strings.Contains(out, "agentcfg migrate") {
		t.Fatalf("ls: %d\n%s", code, out)
	}
	if code, out, errs := c("", "test", "pay.prod"); code != 0 || !strings.Contains(out, "v42") {
		t.Fatalf("test: %d %s %s", code, out, errs)
	}
	if code, out, _ := c("", "migrate"); code != 0 || !strings.Contains(out, "old.prod") {
		t.Fatalf("migrate: %d %s", code, out)
	}
	b, _ := os.ReadFile(cfgPath)
	if strings.Contains(string(b), "old_plain_token_1") || strings.Contains(string(b), "kc_cli_token_1") {
		t.Fatalf("a token is in the file:\n%s", b)
	}
	if code, _, _ := c("", "rm", "old.prod"); code != 0 {
		t.Fatal("rm failed")
	}
	if code, out, _ := c("", "ls"); code != 0 || strings.Contains(out, "old.prod") || strings.Contains(out, "plaintext") {
		t.Fatalf("ls after rm: %s", out)
	}
}

func TestAnnounceCommand(t *testing.T) {
	keychaintest.Temp(t)
	const hook = "https://hooks.slack.com/services/T0FAKE1/B0FAKE1/fakeSecretPart123"
	cfgPath := filepath.Join(t.TempDir(), "services.toml")
	os.WriteFile(cfgPath, []byte("[pay.prod]\nbase_url = \"https://pay.example.com\"\n"), 0o600)
	c := func(stdin string, args ...string) (int, string, string) {
		return run(t, stdin, append(args, "--config", cfgPath)...)
	}
	if code, _, _ := c("", "announce", "pay"); code != 2 {
		t.Fatalf("announce with nothing to change: exit %d", code)
	}
	if code, _, _ := c("", "announce", "pay", "--remove", "--channel", "#x"); code != 2 {
		t.Fatalf("--remove with --channel: exit %d", code)
	}
	if code, _, _ := c("", "set", "pay.prod", "--channel", "#x"); code != 2 {
		t.Fatalf("--channel on set: exit %d", code)
	}
	if code, out, errs := c(hook+"\n", "announce", "pay", "--channel", "#pay", "--envs", "prod, dev", "--webhook"); code != 0 || strings.Contains(out+errs, "fakeSecretPart123") {
		t.Fatalf("announce: %d %s %s", code, out, errs)
	}
	code, out, _ := c("", "ls")
	if code != 0 || !strings.Contains(out, "#pay") || !strings.Contains(out, "prod,dev") || !strings.Contains(out, "keychain tok:") || strings.Contains(out, "fakeSecretPart123") {
		t.Fatalf("ls: %d\n%s", code, out)
	}
	if b, _ := os.ReadFile(cfgPath); strings.Contains(string(b), "fakeSecretPart123") {
		t.Fatal("webhook written to the file")
	}
	if code, _, errs := c("", "announce", "pay", "--remove"); code != 0 {
		t.Fatalf("remove: %d %s", code, errs)
	}
	if _, out, _ := c("", "ls"); strings.Contains(out, "SLACK") {
		t.Fatalf("ls after remove:\n%s", out)
	}
}

func TestLsShowsAnnounceWithoutAnEnvironment(t *testing.T) {
	const hook = "https://hooks.slack.com/services/T0FAKE1/B0FAKE1/fakeSecretPart123"
	cfgPath := filepath.Join(t.TempDir(), "services.toml")
	if err := os.WriteFile(cfgPath, []byte("[pay.announce]\nwebhook = \""+hook+"\"\nchannel = \"#pay\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	code, out, errs := run(t, "", "ls", "--config", cfgPath)
	if code != 0 || !strings.Contains(out, "SLACK") || !strings.Contains(out, "pay") || !strings.Contains(out, "1 secret(s)") || strings.Contains(out+errs, "fakeSecretPart123") {
		t.Fatalf("ls: %d\nstdout: %s\nstderr: %s", code, out, errs)
	}
}
