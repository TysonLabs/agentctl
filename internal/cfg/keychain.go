package cfg

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/TysonLabs/agentctl/internal/keychain"
)

const keychainTimeout = 15 * time.Second

// keychainSet stores token for account and reads it back. The token goes to
// `security -i` on stdin, hex-encoded (-X), so it never appears in a process
// argument list and needs no quoting. `security -i` exits 0 even when a
// command inside it fails, so the read-back is the only proof of success.
func keychainSet(account, token string) error {
	if !keychain.Supported {
		return keychain.ErrUnsupported
	}
	if !keychain.ValidAccount(account) {
		return fmt.Errorf("invalid keychain account %q", account)
	}
	line := fmt.Sprintf("add-generic-password -U -s %s -a %s -l %s:%s -X %s",
		keychain.Service, account, keychain.Service, account, hex.EncodeToString([]byte(token)))
	if p := keychain.Path(); p != "" {
		if strings.ContainsAny(p, "\"\\\n") {
			return fmt.Errorf("AGENTCTL_KEYCHAIN path has characters agentcfg cannot pass to security")
		}
		line += ` "` + p + `"`
	}
	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, keychain.Bin, "-i")
	cmd.Stdin = strings.NewReader(line + "\n")
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("keychain write failed: %v", err)
	}
	got, err := keychain.Get(account)
	if err != nil {
		return fmt.Errorf("keychain write not confirmed: %v", err)
	}
	if got != token {
		return fmt.Errorf("keychain write not confirmed: item %s/%s holds a different value", keychain.Service, account)
	}
	return nil
}

// keychainDelete removes the item for account. A missing item is success.
func keychainDelete(account string) error {
	if !keychain.Supported {
		return keychain.ErrUnsupported
	}
	if !keychain.ValidAccount(account) {
		return fmt.Errorf("invalid keychain account %q", account)
	}
	args := []string{"delete-generic-password", "-s", keychain.Service, "-a", account}
	if p := keychain.Path(); p != "" {
		args = append(args, p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), keychainTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, keychain.Bin, args...)
	var stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stderr, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if err == nil || (errors.As(err, &ee) && ee.ExitCode() == 44) {
		return nil
	}
	return fmt.Errorf("keychain delete of %s/%s failed: %s", keychain.Service, account, strings.TrimSpace(stderr.String()))
}
