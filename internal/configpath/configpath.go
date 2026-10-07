// Package configpath resolves the services.toml path. agentctl's registry and
// agentflow's announce reader share it; it holds no config and no secrets, so
// both binaries may import it.
package configpath

import (
	"os"
	"path/filepath"
)

// Resolve picks the config path: flag value > AGENTCTL_CONFIG > default.
func Resolve(flagVal string) string {
	if flagVal != "" {
		return flagVal
	}
	if p := os.Getenv("AGENTCTL_CONFIG"); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = "."
	}
	return filepath.Join(home, ".config", "agentctl", "services.toml")
}
