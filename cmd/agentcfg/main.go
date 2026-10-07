// agentcfg edits the agentctl service registry (services.toml) and keeps its
// tokens in the macOS Keychain, from the command line or a local settings
// page (agentcfg ui). It is a separate binary from agentctl so agentctl keeps
// its read-only guarantee; it is meant for a person, not for agents.
package main

import (
	"os"

	"github.com/TysonLabs/agentctl/internal/cfg/cli"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cli.Version = version
	os.Exit(cli.Run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
