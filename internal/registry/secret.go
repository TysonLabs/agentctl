package registry

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
)

// Secret wraps a bearer token so it can never leak through formatting,
// logging, or JSON encoding. Every rendering path yields a fingerprint;
// the raw value is only available via Reveal(), which is called at exactly
// one site in the codebase (setting the Authorization header).
type Secret struct {
	v string
}

// NewSecret wraps a raw token value.
func NewSecret(v string) Secret { return Secret{v: v} }

// Reveal returns the raw token. The only legitimate call site is
// internal/client setting the Authorization header (plus tests).
func (s Secret) Reveal() string { return s.v }

// IsZero reports whether the secret is empty.
func (s Secret) IsZero() bool { return s.v == "" }

// Fingerprint returns "tok:" + first 8 hex chars of SHA-256 of the token.
func (s Secret) Fingerprint() string {
	sum := sha256.Sum256([]byte(s.v))
	return "tok:" + hex.EncodeToString(sum[:])[:8]
}

func (s Secret) String() string   { return s.Fingerprint() }
func (s Secret) GoString() string { return s.Fingerprint() }

// Format masks the secret for every fmt verb.
func (s Secret) Format(f fmt.State, verb rune) {
	fmt.Fprint(f, s.Fingerprint())
}

// MarshalJSON encodes the fingerprint, never the raw value.
func (s Secret) MarshalJSON() ([]byte, error) {
	return []byte(`"` + s.Fingerprint() + `"`), nil
}
