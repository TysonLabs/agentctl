// Package cli implements the agentctl command dispatch. Exit codes are
// assigned here and nowhere else:
//
//	0 success · 1 usage/config error · 2 HTTP status >= 400 · 3 transport
package cli

import (
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/TysonLabs/agentctl/internal/client"
	"github.com/TysonLabs/agentctl/internal/registry"
)

// Version is injected from main via ldflags.
var Version = "dev"

const usage = `agentctl — safe, read-only client for /agent observability surfaces

Usage:
  agentctl ls                                 list registered services and wiring status
  agentctl get <service.env> <path> [--raw]   GET /agent/<path>  (e.g. agentctl get payments.dev version)
  agentctl endpoints <service.env>            fetch and render the GET /agent index
  agentctl status [service.env ...]           /agent/version + /agent/health across services
  agentctl version                            print agentctl's own version

Flags (before or after the subcommand):
  --config PATH    registry file (default: $AGENTCTL_CONFIG or ~/.config/agentctl/services.toml)
  --timeout DUR    per-request timeout (default 10s; status: 8s)
  --raw            (get) byte-for-byte body passthrough
  --help           this screen

Exit codes: 0 ok · 1 usage/config error · 2 HTTP >= 400 · 3 transport/timeout
`

type usageError string

func (e usageError) Error() string { return string(e) }

type httpError struct {
	status int
	msg    string
}

func (e *httpError) Error() string { return e.msg }

type opts struct {
	config     string
	timeout    time.Duration
	timeoutSet bool
	raw        bool
	help       bool
}

type app struct {
	stdout, stderr io.Writer
	opts           opts
	secrets        []registry.Secret
}

// errf prints a masked, one-line diagnostic to stderr. Every stderr write in
// the cli package goes through here so the scrubber always applies.
func (a *app) errf(format string, args ...any) {
	msg := fmt.Sprintf(format, args...)
	fmt.Fprintln(a.stderr, "agentctl: "+client.Scrub(msg, a.secrets...))
}

func (a *app) loadRegistry() (*registry.Registry, error) {
	reg, err := registry.Load(registry.ResolvePath(a.opts.config))
	if err != nil {
		return nil, usageError(err.Error())
	}
	a.secrets = reg.Secrets()
	for _, w := range reg.Warnings {
		a.errf("warning: %s", w)
	}
	return reg, nil
}

// requireWired resolves "service.env" to a wired service or a usage error.
func (a *app) requireWired(reg *registry.Registry, name string) (registry.Service, error) {
	svc, ok := reg.Lookup(name)
	if !ok {
		return registry.Service{}, usageError(fmt.Sprintf("unknown service %q — run: agentctl ls", name))
	}
	if !svc.Wired {
		return registry.Service{}, usageError(fmt.Sprintf("%s is not wired (%s) — edit %s", name, svc.NotWiredReason, reg.Path))
	}
	return svc, nil
}

func (a *app) newClient(svc registry.Service, defaultTimeout time.Duration) (*client.Client, error) {
	timeout := defaultTimeout
	if a.opts.timeoutSet {
		timeout = a.opts.timeout
	}
	c, err := client.New(svc.BaseURL, svc.Token, timeout, Version)
	if err != nil {
		return nil, usageError(err.Error())
	}
	return c, nil
}

// parseArgs extracts global flags from anywhere in args; the rest are positionals.
func parseArgs(args []string) (opts, []string, error) {
	var o opts
	o.timeout = 10 * time.Second
	var pos []string
	for i := 0; i < len(args); i++ {
		arg := args[i]
		if !strings.HasPrefix(arg, "--") {
			pos = append(pos, arg)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimPrefix(arg, "--"), "=")
		take := func() (string, error) {
			if hasVal {
				return val, nil
			}
			if i+1 >= len(args) {
				return "", usageError("flag --" + name + " requires a value")
			}
			i++
			return args[i], nil
		}
		switch name {
		case "config":
			v, err := take()
			if err != nil {
				return o, nil, err
			}
			o.config = v
		case "timeout":
			v, err := take()
			if err != nil {
				return o, nil, err
			}
			d, err := time.ParseDuration(v)
			if err != nil || d <= 0 {
				return o, nil, usageError("invalid --timeout value: " + v)
			}
			o.timeout = d
			o.timeoutSet = true
		case "raw":
			o.raw = true
		case "help":
			o.help = true
		default:
			return o, nil, usageError("unknown flag --" + name)
		}
	}
	return o, pos, nil
}

// Run executes agentctl and returns the process exit code.
func Run(args []string, stdout, stderr io.Writer) (code int) {
	a := &app{stdout: stdout, stderr: stderr}
	defer func() {
		if r := recover(); r != nil {
			a.errf("internal error: %v", r)
			code = 1
		}
	}()

	o, pos, err := parseArgs(args)
	if err != nil {
		a.errf("%v", err)
		return 1
	}
	a.opts = o

	if o.help || len(pos) == 0 {
		fmt.Fprint(stdout, usage)
		if o.help {
			return 0
		}
		return 1
	}

	cmd, rest := pos[0], pos[1:]
	var cmdErr error
	switch cmd {
	case "ls":
		cmdErr = a.cmdLs(rest)
	case "get":
		cmdErr = a.cmdGet(rest)
	case "endpoints":
		cmdErr = a.cmdEndpoints(rest)
	case "status":
		cmdErr = a.cmdStatus(rest)
	case "version":
		fmt.Fprintln(stdout, "agentctl "+Version)
	default:
		a.errf("unknown command %q — run: agentctl --help", cmd)
		return 1
	}
	return a.exitCode(cmdErr)
}

func (a *app) exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ue usageError
	var he *httpError
	var te *client.TransportError
	switch {
	case errors.As(err, &ue):
		a.errf("%v", ue)
		return 1
	case errors.As(err, &he):
		a.errf("%s", he.msg)
		return 2
	case errors.As(err, &te):
		a.errf("%v", te)
		return 3
	default:
		a.errf("%v", err)
		return 1
	}
}
