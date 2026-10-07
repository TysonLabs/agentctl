package cli

import (
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
