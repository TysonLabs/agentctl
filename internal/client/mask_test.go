package client

import (
	"net/url"
	"strings"
	"testing"

	"github.com/TysonLabs/agentctl/internal/registry"
)

func TestScrub(t *testing.T) {
	a := registry.NewSecret("token-alpha-1234")
	b := registry.NewSecret("token-beta-5678")

	in := "error calling https://x?t=token-alpha-1234 with token-beta-5678 and token-alpha-1234"
	out := Scrub(in, a, b)
	if strings.Contains(out, a.Reveal()) || strings.Contains(out, b.Reveal()) {
		t.Fatalf("tokens survived scrub: %q", out)
	}
	if !strings.Contains(out, a.Fingerprint()) || !strings.Contains(out, b.Fingerprint()) {
		t.Errorf("fingerprints missing: %q", out)
	}
	// idempotent
	if again := Scrub(out, a, b); again != out {
		t.Errorf("scrub not idempotent: %q vs %q", again, out)
	}
	// empty secret is a no-op
	if got := Scrub("hello", registry.NewSecret("")); got != "hello" {
		t.Errorf("empty secret mangled string: %q", got)
	}
}

func TestScrubURLError(t *testing.T) {
	sec := registry.NewSecret("leaky-token-9999")
	ue := &url.Error{Op: "Get", URL: "https://x.example.com/?tok=leaky-token-9999", Err: url.InvalidHostError("x")}
	out := Scrub(ue.Error(), sec)
	if strings.Contains(out, "leaky-token-9999") {
		t.Fatalf("token survived in url.Error text: %q", out)
	}
}
