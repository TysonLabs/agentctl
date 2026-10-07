package keychain_test

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TysonLabs/agentctl/internal/keychain"
	"github.com/TysonLabs/agentctl/internal/keychain/keychaintest"
)

func TestParseRef(t *testing.T) {
	good := map[string]string{"keychain:svc.prod": "svc.prod", "keychain:a_b-c.d-1": "a_b-c.d-1"}
	for in, want := range good {
		got, err := keychain.ParseRef(in)
		if err != nil || got != want {
			t.Errorf("ParseRef(%q) = %q, %v; want %q", in, got, err, want)
		}
	}
	for _, in := range []string{"", "svc.prod", "keychain:", "keychain:svc", "keychain:svc.prod.x", "keychain:svc prod.x", "keychain:-a\";x.y", "file:svc.prod"} {
		if _, err := keychain.ParseRef(in); err == nil {
			t.Errorf("ParseRef(%q) accepted", in)
		}
	}
}

func TestParseRefErrorDoesNotEchoValue(t *testing.T) {
	const secret = "bearer_token_accidentally_pasted_here"
	for _, ref := range []string{secret, "keychain:" + secret} {
		_, err := keychain.ParseRef(ref)
		if err == nil {
			t.Fatalf("ParseRef(%q) accepted", ref)
		}
		if strings.Contains(err.Error(), secret) {
			t.Fatalf("ParseRef error leaked token_ref value: %q", err)
		}
	}
}

func TestGet(t *testing.T) {
	keychaintest.Temp(t)
	keychaintest.Put(t, "svc.prod", "tok+/=abc123")
	got, err := keychain.Get("svc.prod")
	if err != nil || got != "tok+/=abc123" {
		t.Fatalf("Get = %q, %v", got, err)
	}
	if _, err := keychain.Get("svc.dev"); !errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("missing item: err = %v, want ErrNotFound", err)
	}
	if _, err := keychain.Get("bad account"); err == nil || errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("invalid account: err = %v", err)
	}
}

// A security failure other than "not found" is an error, never a token.
func TestGetFailureIsError(t *testing.T) {
	fake := filepath.Join(t.TempDir(), "security")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'locked' >&2\nexit 51\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	old, oldSup := keychain.Bin, keychain.Supported
	keychain.Bin, keychain.Supported = fake, true
	t.Cleanup(func() { keychain.Bin, keychain.Supported = old, oldSup })
	_, err := keychain.Get("svc.prod")
	if err == nil || errors.Is(err, keychain.ErrNotFound) {
		t.Fatalf("err = %v, want a lookup failure", err)
	}
}

func TestUnsupported(t *testing.T) {
	old := keychain.Supported
	keychain.Supported = false
	t.Cleanup(func() { keychain.Supported = old })
	if _, err := keychain.Get("svc.prod"); !errors.Is(err, keychain.ErrUnsupported) {
		t.Fatalf("err = %v, want ErrUnsupported", err)
	}
}
