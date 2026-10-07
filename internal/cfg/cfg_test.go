package cfg

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/TysonLabs/agentctl/internal/keychain"
	"github.com/TysonLabs/agentctl/internal/keychain/keychaintest"
	"github.com/TysonLabs/agentctl/internal/registry"
)

func newStore(t *testing.T, content string) *Store {
	t.Helper()
	p := filepath.Join(t.TempDir(), "services.toml")
	if content != "" {
		if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return &Store{Path: p}
}

func read(t *testing.T, s *Store) string {
	t.Helper()
	b, err := os.ReadFile(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

const sample = `# my notes
[pay.prod]
token = "plain_prod_token_1"
base_url = "https://pay.example.com"
extra = 7

[pay.meta]
unit = "pay.service"
repo = "~/src/pay"
owner = "ops"

[pay.announce]
webhook = "https://hooks.example.com/x"
channels = ["a", "b"]

[dial.dev]
base_url = "https://dev.dial.example.com"
token = "REPLACE_ME"
`

func TestRenderKeepsUnknownTablesAndOrders(t *testing.T) {
	s := newStore(t, sample)
	res, err := s.SetBaseURL("", "dial.dev", "https://dev2.dial.example.com")
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Warnings) != 1 || !strings.Contains(res.Warnings[0], "comments") {
		t.Errorf("want a dropped-comments warning, got %v", res.Warnings)
	}
	want := header + `
[dial.dev]
base_url = "https://dev2.dial.example.com"
token = "REPLACE_ME"

[pay.meta]
owner = "ops"
repo = "~/src/pay"
unit = "pay.service"

[pay.prod]
base_url = "https://pay.example.com"
token = "plain_prod_token_1"
extra = 7

[pay.announce]
channels = ["a", "b"]
webhook = "https://hooks.example.com/x"
`
	if got := read(t, s); got != want {
		t.Fatalf("rendered file:\n%s\nwant:\n%s", got, want)
	}
	// A second write of the managed file has no comments to drop.
	res, err = s.SetMeta("", "pay", map[string]string{"unit": "pay2.service"})
	if err != nil || len(res.Warnings) != 0 {
		t.Fatalf("SetMeta: %v, warnings %v", err, res.Warnings)
	}
}

func TestRenderPreservesTOMLScalarTypes(t *testing.T) {
	content := `[a.prod]
base_url = "https://a.example.com"
"key with spaces" = "quote: \"; newline:\n; unicode: 雪"
integer = 7
float = 7.0
not_a_number = nan
date = 2026-10-07
offset_datetime = 2026-10-07T12:34:56+05:30
empty_array = []

[a.announce.empty]

[unused]
`
	s := newStore(t, content)
	if _, err := s.SetBaseURL("", "a.prod", "https://b.example.com"); err != nil {
		t.Fatal(err)
	}
	doc, _, err := readDoc(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	prod := doc.Tree["a"].(map[string]any)["prod"].(map[string]any)
	if _, ok := prod["integer"].(int64); !ok {
		t.Fatalf("integer changed type: %T", prod["integer"])
	}
	if _, ok := prod["float"].(float64); !ok {
		t.Fatalf("float changed type: %T", prod["float"])
	}
	announce := doc.Tree["a"].(map[string]any)["announce"].(map[string]any)
	if _, ok := announce["empty"]; !ok {
		t.Fatal("nested empty table was lost")
	}
	if _, ok := doc.Tree["unused"]; !ok {
		t.Fatal("top-level empty table was lost")
	}
}

func TestEditConflictAndValidation(t *testing.T) {
	s := newStore(t, sample)
	before := read(t, s)
	if _, err := s.SetBaseURL("deadbeef", "pay.prod", "https://x.example.com"); !errors.Is(err, ErrConflict) {
		t.Fatalf("stale version: err = %v, want ErrConflict", err)
	}
	if _, err := s.SetBaseURL(VersionOf([]byte(before)), "pay.prod", "ftp://x.example.com"); err == nil {
		t.Fatal("invalid base URL accepted")
	}
	if _, err := s.SetBaseURL("", "pay.meta", "https://x"); err == nil {
		t.Fatal("reserved env name accepted")
	}
	if _, err := s.SetBaseURL("", "pay.prod.x", "https://x"); err == nil {
		t.Fatal("dotted env accepted")
	}
	if _, err := s.SetMeta("", "pay", map[string]string{"owner": "me"}); err == nil {
		t.Fatal("non-editable meta key accepted")
	}
	if got := read(t, s); got != before {
		t.Fatal("a refused edit changed the file")
	}
	res, err := s.SetBaseURL(VersionOf([]byte(before)), "pay.prod", "https://x.example.com/")
	if err != nil {
		t.Fatal(err)
	}
	if res.Version != VersionOf([]byte(read(t, s))) {
		t.Fatal("result version does not match the file")
	}
}

func TestNewFileIsPrivate(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "agentctl")
	s := &Store{Path: filepath.Join(dir, "services.toml")}
	if _, err := s.SetBaseURL("none", "a.prod", "https://a.example.com"); err != nil {
		t.Fatal(err)
	}
	fi, err := os.Stat(s.Path)
	if err != nil || fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v, %v; want 0600", fi.Mode(), err)
	}
	di, _ := os.Stat(dir)
	if di.Mode().Perm() != 0o700 {
		t.Fatalf("dir mode = %v; want 0700", di.Mode())
	}
}

func TestSymlinkIsKept(t *testing.T) {
	real := newStore(t, "[a.prod]\nbase_url = \"https://a\"\n")
	link := filepath.Join(t.TempDir(), "services.toml")
	if err := os.Symlink(real.Path, link); err != nil {
		t.Fatal(err)
	}
	s := &Store{Path: link}
	if _, err := s.SetBaseURL("", "a.prod", "https://b.example.com"); err != nil {
		t.Fatal(err)
	}
	if fi, _ := os.Lstat(link); fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced by a file")
	}
	if !strings.Contains(read(t, real), "https://b.example.com") {
		t.Fatal("the link target was not updated")
	}
}

func TestBrokenSymlinkIsNotReplaced(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "services.toml")
	if err := os.Symlink(filepath.Join(dir, "missing.toml"), link); err != nil {
		t.Fatal(err)
	}
	s := &Store{Path: link}
	if _, err := s.SetBaseURL("", "a.prod", "https://a.example.com"); err == nil {
		t.Fatal("edit through a broken symlink succeeded")
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatalf("broken symlink disappeared: %v", err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("broken symlink was replaced: mode=%v", fi.Mode())
	}
}

func TestSymlinkAndTargetUseSameLock(t *testing.T) {
	real := newStore(t, "[a.prod]\nbase_url = \"https://a.example.com\"\n")
	link := filepath.Join(t.TempDir(), "services.toml")
	if err := os.Symlink(real.Path, link); err != nil {
		t.Fatal(err)
	}
	realTarget, err := resolveStorePath(real.Path)
	if err != nil {
		t.Fatal(err)
	}
	linkTarget, err := resolveStorePath(link)
	if err != nil {
		t.Fatal(err)
	}
	if realTarget != linkTarget {
		t.Fatalf("lock identities differ: target=%q symlink=%q", realTarget, linkTarget)
	}
}

// The lock serialises read-modify-write: concurrent edits all land.
func TestConcurrentEditsAllLand(t *testing.T) {
	s := newStore(t, "")
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := s.SetBaseURL("", fmt.Sprintf("svc%d.prod", i), "https://example.com"); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	reg, err := registry.Parse(s.Path, []byte(read(t, s)))
	if err != nil {
		t.Fatal(err)
	}
	if len(reg.Services) != 12 {
		t.Fatalf("got %d services, want 12: an edit was lost", len(reg.Services))
	}
}

func TestUnsupportedShapesFailClosed(t *testing.T) {
	for name, content := range map[string]string{
		"root scalar":     "x = 1\n[a.prod]\nbase_url = \"https://a\"\n",
		"array of tables": "[a.prod]\nbase_url = \"https://a\"\n[[a.prod.list]]\nk = 1\n",
		"quoted name":     "[\"a b\".prod]\nbase_url = \"https://a\"\n",
	} {
		s := newStore(t, content)
		if _, err := s.SetBaseURL("", "z.prod", "https://z"); err == nil {
			t.Errorf("%s: edit accepted", name)
		}
		if read(t, s) != content {
			t.Errorf("%s: file changed", name)
		}
	}
}

func TestBadTOMLNeverEchoed(t *testing.T) {
	s := newStore(t, "[a.prod]\ntoken = \"secret_tok_123\n")
	_, err := s.SetBaseURL("", "a.prod", "https://a")
	if err == nil || strings.Contains(err.Error(), "secret_tok_123") {
		t.Fatalf("err = %v", err)
	}
}

func TestSetTokenMovesToKeychain(t *testing.T) {
	keychaintest.Temp(t)
	s := newStore(t, sample)
	for _, bad := range []string{"", "REPLACE_ME", "short", "has space here", "tab\tinside1"} {
		if _, err := s.SetToken("", "pay.prod", bad); err == nil {
			t.Errorf("token %q accepted", bad)
		}
	}
	if _, err := s.SetToken("", "nope.prod", "kc_token_value_1"); err == nil {
		t.Error("token for a missing env accepted")
	}
	if _, err := s.SetToken("", "pay.prod", "kc_token_value_1"); err != nil {
		t.Fatal(err)
	}
	got := read(t, s)
	if strings.Contains(got, "plain_prod_token_1") || strings.Contains(got, "kc_token_value_1") {
		t.Fatalf("a token is still in the file:\n%s", got)
	}
	if !strings.Contains(got, `token_ref = "keychain:pay.prod"`) {
		t.Fatalf("no token_ref:\n%s", got)
	}
	reg, err := registry.Load(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	svc, _ := reg.Lookup("pay.prod")
	if !svc.Wired || svc.Token.Reveal() != "kc_token_value_1" {
		t.Fatalf("agentctl does not see the keychain token: %+v", svc)
	}
	st := s.State()
	if st.Plaintext != 0 {
		t.Fatalf("plaintext = %d", st.Plaintext)
	}
}

func TestMigrate(t *testing.T) {
	keychaintest.Temp(t)
	s := newStore(t, sample+"\n[web.prod]\nbase_url = \"https://web.example.com\"\ntoken = \"web_token_abc1\"\n")
	if st := s.State(); st.Plaintext != 2 {
		t.Fatalf("plaintext before = %d, want 2", st.Plaintext)
	}
	mr, err := s.Migrate("")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(mr.Moved, ",") != "pay.prod,web.prod" {
		t.Fatalf("moved %v", mr.Moved)
	}
	if len(mr.Skipped) != 1 || !strings.HasPrefix(mr.Skipped[0], "dial.dev") {
		t.Fatalf("skipped %v; want the placeholder dial.dev", mr.Skipped)
	}
	got := read(t, s)
	for _, tok := range []string{"plain_prod_token_1", "web_token_abc1"} {
		if strings.Contains(got, tok) {
			t.Fatalf("%s still in the file", tok)
		}
	}
	if !strings.Contains(got, "webhook") {
		t.Fatal("the announce table was lost")
	}
	reg, err := registry.Load(s.Path)
	if err != nil {
		t.Fatal(err)
	}
	for full, want := range map[string]string{"pay.prod": "plain_prod_token_1", "web.prod": "web_token_abc1"} {
		svc, _ := reg.Lookup(full)
		if !svc.Wired || svc.Token.Reveal() != want {
			t.Fatalf("%s after migrate: %+v", full, svc)
		}
	}
	again, err := s.Migrate("")
	if err != nil || len(again.Moved) != 0 {
		t.Fatalf("second migrate: %v, %v", again, err)
	}
}

// If the Keychain write fails, the file keeps its plaintext token.
func TestKeychainFailureLeavesFileUnchanged(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "security")
	// Accepts the write (exit 0, like `security -i` on failure) but never
	// stores anything, so the read-back finds nothing.
	if err := os.WriteFile(fake, []byte("#!/bin/sh\ncat >/dev/null\n[ \"$1\" = -i ] && exit 0\nexit 44\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old, oldSup := keychain.Bin, keychain.Supported
	keychain.Bin, keychain.Supported = fake, true
	t.Cleanup(func() { keychain.Bin, keychain.Supported = old, oldSup })

	s := newStore(t, sample)
	before := read(t, s)
	if _, err := s.SetToken("", "pay.prod", "kc_token_value_1"); err == nil || !strings.Contains(err.Error(), "not confirmed") {
		t.Fatalf("SetToken err = %v", err)
	}
	if _, err := s.Migrate(""); err == nil {
		t.Fatal("Migrate succeeded with a failing keychain")
	}
	if read(t, s) != before {
		t.Fatal("the file changed after a failed keychain write")
	}
}

func TestMigrateRenderFailureDoesNotTouchKeychain(t *testing.T) {
	marker := filepath.Join(t.TempDir(), "called")
	fake := filepath.Join(t.TempDir(), "security")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\nprintf called >>\"$AGENTCFG_KEYCHAIN_MARKER\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTCFG_KEYCHAIN_MARKER", marker)
	old, oldSup := keychain.Bin, keychain.Supported
	keychain.Bin, keychain.Supported = fake, true
	t.Cleanup(func() { keychain.Bin, keychain.Supported = old, oldSup })
	s := newStore(t, `[a.prod]
base_url = "https://a.example.com"
token = "plain_token_value_1"

["z z".prod]
base_url = "https://z.example.com"
`)
	before := read(t, s)
	if _, err := s.Migrate(""); err == nil {
		t.Fatal("Migrate succeeded for a tree agentcfg cannot render")
	}
	if got := read(t, s); got != before {
		t.Fatal("failed migration changed the file")
	}
	if _, err := os.Stat(marker); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("failed migration invoked security: %v", err)
	}
}

func TestInvalidSourceIsNotRepairedByRemove(t *testing.T) {
	for _, ref := range []string{"", "keychain:a.prod"} {
		content := `[a.prod]
base_url = "https://a.example.com"
token = "plain_token_value_1"
token_ref = "` + ref + `"
`
		s := newStore(t, content)
		if _, err := s.Remove("", "a.prod"); err == nil {
			t.Errorf("Remove accepted ambiguous token source with token_ref %q", ref)
		}
		if got := read(t, s); got != content {
			t.Errorf("refused removal changed registry with token_ref %q", ref)
		}
	}
}

func TestRemove(t *testing.T) {
	kc := keychaintest.Temp(t)
	s := newStore(t, sample)
	if _, err := s.SetToken("", "pay.prod", "kc_token_value_1"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Remove("", "pay.prod"); err != nil {
		t.Fatal(err)
	}
	got := read(t, s)
	if strings.Contains(got, "[pay.prod]") || strings.Contains(got, "[pay.meta]") {
		t.Fatalf("pay.prod or its now-orphaned meta survived:\n%s", got)
	}
	if !strings.Contains(got, "[pay.announce]") {
		t.Fatalf("agentflow's announce table was removed:\n%s", got)
	}
	if err := exec.Command(keychain.Bin, "find-generic-password", "-s", keychain.Service, "-a", "pay.prod", kc).Run(); err == nil {
		t.Fatal("the keychain item survived the remove")
	}
	if _, err := s.Remove("", "pay.prod"); err == nil {
		t.Fatal("removing a missing env succeeded")
	}
}

func TestStateNeverHoldsTokens(t *testing.T) {
	s := newStore(t, sample)
	st := s.State()
	if st.Error != "" || len(st.Services) != 2 || st.Plaintext != 1 {
		t.Fatalf("state %+v", st)
	}
	if fmt.Sprintf("%+v", st) == "" || strings.Contains(fmt.Sprintf("%#v", st), "plain_prod_token_1") {
		t.Fatal("state leaks a token")
	}
	missing := (&Store{Path: filepath.Join(t.TempDir(), "none.toml")}).State()
	if missing.Error != "" || missing.Version != "none" || len(missing.Services) != 0 {
		t.Fatalf("missing file state %+v", missing)
	}
}

func TestSafeVersionHidesTokenMaterial(t *testing.T) {
	tok := strings.Repeat("ab12", 16) // 64 chars, longer than VersionFromBody's 40-char cut
	secrets := []registry.Secret{registry.NewSecret(tok)}
	for _, v := range []string{tok, tok[:40], "v=" + tok, tok[10:20]} {
		if got := safeVersion(v, secrets); got != hiddenVersion {
			t.Errorf("safeVersion(%q) = %q, want hidden", v, got)
		}
	}
	for _, v := range []string{"1f45ae32", "v1.2.3", "ab12"} {
		if got := safeVersion(v, secrets); got != v {
			t.Errorf("safeVersion(%q) = %q, want it kept", v, got)
		}
	}
}
