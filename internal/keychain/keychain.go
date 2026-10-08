// Package keychain reads secrets from the macOS Keychain through
// /usr/bin/security. It only reads: agentctl and agentflow import it, and
// both stay read-only toward the Keychain. Writes live in agentcfg
// (internal/cfg), which the import boundary test keeps out of both.
//
// Every item is a generic password. agentctl's /agent tokens use Keychain
// service Service, account "<name>.<env>" (a registry token_ref). agentflow's
// Slack webhooks use AgentflowService, account "<name>.announce" (a
// webhook_ref); agentflow reads only that service (see the boundary test).
package keychain

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Service is the Keychain service name of every agentctl token item.
const Service = "agentctl"

// AgentflowService is the Keychain service name of agentflow's credentials
// (the [name.announce] Slack webhook), kept apart from agentctl's tokens.
const AgentflowService = "agentflow"

// RefPrefix starts a token_ref value: token_ref = "keychain:<account>".
const RefPrefix = "keychain:"

// Bin is the security binary. Tests replace it with a fake.
var Bin = "/usr/bin/security"

// Supported reports whether this OS has a Keychain. Tests that use a fake
// Bin set it.
var Supported = runtime.GOOS == "darwin"

// lookupTimeout bounds one security call; a locked keychain over SSH can
// otherwise wait on a prompt nobody sees.
const lookupTimeout = 10 * time.Second

// ErrNotFound means the keychain has no item for the account.
var ErrNotFound = errors.New("keychain item not found")

// ErrUnsupported means this OS has no macOS Keychain.
var ErrUnsupported = errors.New("keychain tokens need macOS")

var accountRe = regexp.MustCompile(`^[A-Za-z0-9_-]+\.[A-Za-z0-9_-]+$`)

// ValidAccount reports whether a is a "<name>.<env>" account. The charset is
// also what lets agentcfg pass it to `security -i` without quoting.
func ValidAccount(a string) bool { return accountRe.MatchString(a) }

// ParseRef returns the account of a "keychain:<account>" token_ref.
func ParseRef(ref string) (string, error) {
	acct, ok := strings.CutPrefix(ref, RefPrefix)
	if !ok {
		// Do not quote ref: a user can accidentally paste the bearer token in
		// token_ref, and registry parse errors are safe to print.
		return "", fmt.Errorf("token_ref must look like %q", RefPrefix+"<name>.<env>")
	}
	if !ValidAccount(acct) {
		return "", errors.New("token_ref account must be <name>.<env> (letters, digits, _ and -)")
	}
	return acct, nil
}

// Path returns the keychain file to use: $AGENTCTL_KEYCHAIN, or "" for the
// user's default keychain search list.
func Path() string { return os.Getenv("AGENTCTL_KEYCHAIN") }

// Get returns the agentctl token stored for account.
func Get(account string) (string, error) { return GetFrom(Service, account) }

// GetFrom returns the secret stored under Keychain service and account.
func GetFrom(service, account string) (string, error) {
	if !Supported {
		return "", ErrUnsupported
	}
	if !ValidAccount(account) {
		return "", fmt.Errorf("invalid keychain account %q", account)
	}
	if service != Service && service != AgentflowService {
		return "", fmt.Errorf("unknown keychain service %q", service)
	}
	args := []string{"find-generic-password", "-s", service, "-a", account, "-w"}
	if p := Path(); p != "" {
		args = append(args, p)
	}
	ctx, cancel := context.WithTimeout(context.Background(), lookupTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, Bin, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	if err != nil {
		var ee *exec.ExitError
		// 44 is errSecItemNotFound's exit status from security(1).
		if errors.As(err, &ee) && ee.ExitCode() == 44 {
			return "", ErrNotFound
		}
		if ctx.Err() != nil {
			return "", fmt.Errorf("keychain lookup timed out after %s (is the keychain locked?)", lookupTimeout)
		}
		// security's stderr never holds the secret (it is only on stdout).
		return "", fmt.Errorf("keychain lookup failed: %s", firstLine(stderr.String(), err))
	}
	tok := strings.TrimSuffix(stdout.String(), "\n")
	if tok == "" {
		return "", ErrNotFound
	}
	return tok, nil
}

func firstLine(s string, fallback error) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback.Error()
	}
	line, _, _ := strings.Cut(s, "\n")
	return line
}
