package client

import (
	"strings"

	"github.com/TysonLabs/agentctl/internal/registry"
)

// Scrub replaces every occurrence of any known raw token in s with its
// fingerprint. Idempotent; belt-and-suspenders on top of the Secret type.
func Scrub(s string, secrets ...registry.Secret) string {
	for _, sec := range secrets {
		raw := sec.Reveal()
		if raw == "" {
			continue
		}
		s = strings.ReplaceAll(s, raw, sec.Fingerprint())
	}
	return s
}
