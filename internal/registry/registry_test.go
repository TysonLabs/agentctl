package registry

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "services.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadValid(t *testing.T) {
	p := writeConfig(t, `
[payments.dev]
base_url = "https://dev.example.com/"
token    = "at_realtoken123"

[payments.prod]
base_url = "https://pay.example.com"
token    = "REPLACE_ME"

[payments.meta]
repo = "org/payments"
unit = "payments.service"
`)
	reg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Services) != 2 {
		t.Fatalf("want 2 services, got %d", len(reg.Services))
	}
	dev, ok := reg.Lookup("payments.dev")
	if !ok || !dev.Wired {
		t.Fatalf("payments.dev should be wired: %+v", dev)
	}
	if dev.BaseURL != "https://dev.example.com" {
		t.Errorf("trailing slash not stripped: %q", dev.BaseURL)
	}
	if dev.Meta["repo"] != "org/payments" || dev.Meta["unit"] != "payments.service" {
		t.Errorf("meta not attached: %+v", dev.Meta)
	}
	prod, _ := reg.Lookup("payments.prod")
	if prod.Wired || prod.NotWiredReason == "" {
		t.Errorf("payments.prod should be not wired: %+v", prod)
	}
}

func TestLoadSkipsTheAnnounceTable(t *testing.T) {
	const hook = "https://hooks.slack.com/services/T000/B000/secretpart"
	p := writeConfig(t, `
[payments.prod]
base_url = "https://pay.example.com"
token    = "at_realtoken123"

[payments.announce]
webhook = "`+hook+`"
channel = "#payments-releases"
envs    = ["prod"]
`)
	reg, err := Load(p)
	if err != nil {
		t.Fatalf("an announce table must not break agentctl: %v", err)
	}
	if len(reg.Services) != 1 || reg.Services[0].FullName() != "payments.prod" {
		t.Fatalf("announce read as an environment: %+v", reg.Services)
	}
	if dump := fmt.Sprintf("%+v %v", reg, reg.Warnings); strings.Contains(dump, "secretpart") || strings.Contains(dump, "announce") {
		t.Errorf("agentctl decoded the announce table: %s", dump)
	}
}

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err == nil {
		t.Fatal("want error")
	}
	if !strings.Contains(err.Error(), "nope.toml") || !strings.Contains(err.Error(), "base_url") {
		t.Errorf("error should include path and example TOML: %v", err)
	}
}

func TestPlaceholderVariants(t *testing.T) {
	cases := []struct {
		token string
		wired bool
	}{
		{"REPLACE_ME", false},
		{"replace_me", false},
		{"CHANGEME", false},
		{"changeme", false},
		{"TODO", false},
		{"todo", false},
		{"…", false},
		{"...", false},
		{"<your-token-here>", false},
		{"xxxxxxxxxxxx", false},
		{"************", false},
		{"____----....", false},
		{"", false},
		{"short", false},
		{"1234567", false},
		{"at_real_token_12345", true},
		{"12345678", true},
	}
	for _, c := range cases {
		t.Run(fmt.Sprintf("%q", c.token), func(t *testing.T) {
			p := writeConfig(t, fmt.Sprintf("[svc.dev]\nbase_url = \"https://x.example.com\"\ntoken = %q\n", c.token))
			reg, err := Load(p)
			if err != nil {
				t.Fatal(err)
			}
			svc, _ := reg.Lookup("svc.dev")
			if svc.Wired != c.wired {
				t.Errorf("token %q: wired=%v, want %v (reason %q)", c.token, svc.Wired, c.wired, svc.NotWiredReason)
			}
			if !c.wired && svc.NotWiredReason == "" {
				t.Errorf("token %q: want a not-wired reason", c.token)
			}
		})
	}
}

func TestBadBaseURLs(t *testing.T) {
	bad := []string{
		"",
		"ftp://x.example.com",
		"file:///etc/passwd",
		"//x.example.com",
		"https://user:pw@x.example.com",
		"https://x.example.com?q=1",
		"https://x.example.com#frag",
		"not a url",
	}
	for _, u := range bad {
		p := writeConfig(t, fmt.Sprintf("[svc.dev]\nbase_url = %q\ntoken = \"at_real_token_1\"\n", u))
		if _, err := Load(p); err == nil {
			t.Errorf("base_url %q: want error, got nil", u)
		}
	}
}

func TestUnknownKeyWarns(t *testing.T) {
	p := writeConfig(t, "[svc.dev]\nbase_url = \"https://x.example.com\"\ntoken = \"at_real_token_1\"\nextra = \"stuff\"\n")
	reg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range reg.Warnings {
		if strings.Contains(w, "extra") {
			found = true
		}
	}
	if !found {
		t.Errorf("want warning about unknown key, got %v", reg.Warnings)
	}
}

func TestPermsWarning(t *testing.T) {
	p := writeConfig(t, "[svc.dev]\nbase_url = \"https://x.example.com\"\ntoken = \"at_real_token_1\"\n")
	if err := os.Chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	reg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, w := range reg.Warnings {
		if strings.Contains(w, "chmod 600") {
			found = true
		}
	}
	if !found {
		t.Errorf("want perms warning, got %v", reg.Warnings)
	}
}

func TestResolvePathPrecedence(t *testing.T) {
	t.Setenv("AGENTCTL_CONFIG", "/env/path.toml")
	if got := ResolvePath("/flag/path.toml"); got != "/flag/path.toml" {
		t.Errorf("flag should win: %q", got)
	}
	if got := ResolvePath(""); got != "/env/path.toml" {
		t.Errorf("env should win over default: %q", got)
	}
	t.Setenv("AGENTCTL_CONFIG", "")
	got := ResolvePath("")
	if !strings.HasSuffix(got, filepath.Join(".config", "agentctl", "services.toml")) {
		t.Errorf("default path wrong: %q", got)
	}
}

func TestSecretMasking(t *testing.T) {
	s := NewSecret("super-secret-token-value")
	fp := s.Fingerprint()
	if !strings.HasPrefix(fp, "tok:") || len(fp) != 12 {
		t.Fatalf("bad fingerprint %q", fp)
	}
	for _, out := range []string{
		fmt.Sprint(s),
		fmt.Sprintf("%v", s),
		fmt.Sprintf("%+v", s),
		fmt.Sprintf("%#v", s),
		fmt.Sprintf("%s", s),
		fmt.Sprintf("%q", s),
		fmt.Sprintf("%d", s),
		fmt.Sprintf("%x", s),
		s.String(),
		s.GoString(),
	} {
		if strings.Contains(out, "super-secret") {
			t.Errorf("leak in formatted output: %q", out)
		}
		if !strings.Contains(out, fp) {
			t.Errorf("output %q missing fingerprint %q", out, fp)
		}
	}
	j, err := json.Marshal(struct{ T Secret }{s})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(j), "super-secret") {
		t.Errorf("leak in JSON: %s", j)
	}
	if s.Reveal() != "super-secret-token-value" {
		t.Errorf("Reveal must return the raw value")
	}
}

func TestParseErrorNeverEchoesTokenValue(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "services.toml")
	const leaked = "supersecrettokenvalue123"
	// Unquoted token value: a one-character mistake that makes the file
	// unparseable. The decode error must not echo the token material.
	content := "[svc.dev]\nbase_url = \"https://dev.example.com\"\ntoken = " + leaked + "\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("want parse error, got nil")
	}
	msg := err.Error()
	if strings.Contains(msg, leaked) || strings.Contains(msg, "supersecret") {
		t.Fatalf("parse error leaks token material: %q", msg)
	}
	if !strings.Contains(msg, "line 3") {
		t.Errorf("parse error should still point at the offending line: %q", msg)
	}
}

func TestNonParseDecodeErrorRedacted(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "services.toml")
	// svc.dev must be a table; a string there fails decode after a
	// successful parse. The message must not echo file content.
	if err := os.WriteFile(p, []byte("[svc]\ndev = \"hunter2hunter2\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(p)
	if err == nil {
		t.Fatal("want decode error, got nil")
	}
	if strings.Contains(err.Error(), "hunter2") {
		t.Fatalf("decode error leaks file content: %q", err.Error())
	}
}
