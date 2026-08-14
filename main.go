// agentctl is a safe, read-only CLI for querying /agent observability
// surfaces. See README.md for the convention and security model.
package main

import (
	"os"

	"github.com/TysonLabs/agentctl/internal/cli"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cli.Version = version
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
