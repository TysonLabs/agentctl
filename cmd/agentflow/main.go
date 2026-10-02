// agentflow is a set of small, single-purpose workflow commands for coding
// agents. Each command does one job and reports a JSON result and an exit
// code; the workflow itself stays in prose (AGENTS.md). It is a separate
// binary from agentctl so agentctl keeps its read-only guarantee.
package main

import (
	"os"

	"github.com/TysonLabs/agentctl/internal/flow/cli"
)

// version is injected at build time via -ldflags "-X main.version=...".
var version = "dev"

func main() {
	cli.Version = version
	os.Exit(cli.Run(os.Args[1:], os.Stdout, os.Stderr))
}
