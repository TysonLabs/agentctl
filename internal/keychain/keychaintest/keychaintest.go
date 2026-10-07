// Package keychaintest gives tests a throwaway keychain, so no test touches
// the login keychain. Import it from _test.go files only.
package keychaintest

import (
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/TysonLabs/agentctl/internal/keychain"
)

// Temp creates an unlocked keychain file in t.TempDir, points
// AGENTCTL_KEYCHAIN at it, and deletes it when the test ends. It skips the
// test off macOS.
func Temp(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" {
		t.Skip("needs the macOS Keychain")
	}
	p := filepath.Join(t.TempDir(), "test.keychain-db")
	run(t, "create-keychain", "-p", "test", p)
	t.Cleanup(func() { _ = exec.Command(keychain.Bin, "delete-keychain", p).Run() })
	run(t, "unlock-keychain", "-p", "test", p)
	t.Setenv("AGENTCTL_KEYCHAIN", p)
	return p
}

// Put stores an agentctl token directly with security(1), bypassing agentcfg.
func Put(t *testing.T, account, token string) { PutIn(t, keychain.Service, account, token) }

// PutIn stores a secret under any Keychain service.
func PutIn(t *testing.T, service, account, secret string) {
	t.Helper()
	run(t, "add-generic-password", "-U", "-s", service, "-a", account, "-w", secret, keychain.Path())
}

func run(t *testing.T, args ...string) {
	t.Helper()
	if out, err := exec.Command(keychain.Bin, args...).CombinedOutput(); err != nil {
		t.Fatalf("security %s: %v\n%s", args[0], err, out)
	}
}
