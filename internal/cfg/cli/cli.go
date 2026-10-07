// Package cli implements agentcfg's commands. agentcfg edits the registry
// agentctl reads; it is for a person, not for agents, so keep it off agent
// allowlists. Exit codes: 0 ok · 1 error · 2 usage · 3 test failed.
package cli

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sort"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/TysonLabs/agentctl/internal/cfg"
	"github.com/TysonLabs/agentctl/internal/cfg/ui"
	"github.com/TysonLabs/agentctl/internal/registry"
)

// Version is injected from main via ldflags.
var Version = "dev"

const usage = `agentcfg — edit the agentctl service registry and keep its tokens in the Keychain

Usage:
  agentcfg ui [--no-open] [--idle DUR]       open the settings page in a browser
  agentcfg ls                                list services, token source and wiring
  agentcfg set <name.env> --base-url URL     set a base URL (adds the env if new)
  agentcfg meta <name> repo=PATH unit=UNIT   set [name.meta] keys (key= clears one)
  agentcfg token <name.env>                  store a token in the Keychain (read from
                                             stdin; typed without echo on a terminal)
  agentcfg announce <name> [--channel C] [--envs prod,dev] [--webhook] [--remove]
                                             agentflow's Slack settings; --webhook reads
                                             the webhook URL from stdin into the Keychain
  agentcfg migrate                           move every plaintext token and webhook into
                                             the Keychain
  agentcfg rm <name.env>                     remove an env and its Keychain item
  agentcfg test <name.env>                   GET /agent/version with the stored token
  agentcfg version                           print agentcfg's version

Flags:
  --config PATH    registry file (default: $AGENTCTL_CONFIG or ~/.config/agentctl/services.toml)
  --help           this screen

agentcfg rewrites services.toml in a fixed layout; comments are not kept.
Tokens live in the login Keychain as service "agentctl", account "<name>.<env>".

Exit codes: 0 ok · 1 error · 2 usage · 3 test failed
`

type usageError string

func (e usageError) Error() string { return string(e) }

type testFailed string

func (e testFailed) Error() string { return string(e) }

type app struct {
	stdin          io.Reader
	stdout, stderr io.Writer
	store          *cfg.Store
	flags          map[string]string
	isTerminal     func(*os.File) bool
	setEcho        func(*os.File, string) error
}

// valueFlags take an argument; boolFlags do not.
var valueFlags = map[string]bool{"config": true, "base-url": true, "idle": true, "channel": true, "envs": true}
var boolFlags = map[string]bool{"help": true, "no-open": true, "webhook": true, "remove": true}

func parseArgs(args []string) (map[string]string, []string, error) {
	flags := map[string]string{}
	var pos []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			pos = append(pos, args[i+1:]...)
			break
		}
		if !strings.HasPrefix(a, "-") || a == "-" {
			pos = append(pos, a)
			continue
		}
		name, val, hasVal := strings.Cut(strings.TrimLeft(a, "-"), "=")
		switch {
		case boolFlags[name]:
			if hasVal {
				return nil, nil, usageError("--" + name + " takes no value")
			}
			flags[name] = "true"
		case valueFlags[name]:
			if !hasVal {
				if i+1 >= len(args) {
					return nil, nil, usageError("--" + name + " needs a value")
				}
				i++
				val = args[i]
			}
			flags[name] = val
		default:
			return nil, nil, usageError("unknown flag --" + name)
		}
	}
	return flags, pos, nil
}

// Run executes agentcfg and returns the process exit code.
func Run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags, pos, err := parseArgs(args)
	if err != nil {
		fmt.Fprintln(stderr, "agentcfg: "+err.Error())
		return 2
	}
	if flags["help"] != "" || len(pos) == 0 {
		fmt.Fprint(stdout, usage)
		if flags["help"] != "" {
			return 0
		}
		return 2
	}
	a := &app{
		stdin: stdin, stdout: stdout, stderr: stderr, flags: flags,
		store:      &cfg.Store{Path: registry.ResolvePath(flags["config"])},
		isTerminal: isTerminal,
		setEcho:    stty,
	}
	cmd, rest := pos[0], pos[1:]
	var cmdErr error
	if err := commandFlags(cmd, flags); err != nil {
		cmdErr = err
	} else {
		switch cmd {
		case "ls":
			cmdErr = a.cmdLs(rest)
		case "set":
			cmdErr = a.cmdSet(rest)
		case "meta":
			cmdErr = a.cmdMeta(rest)
		case "token":
			cmdErr = a.cmdToken(rest)
		case "migrate":
			cmdErr = a.cmdMigrate(rest)
		case "rm":
			cmdErr = a.cmdRm(rest)
		case "test":
			cmdErr = a.cmdTest(rest)
		case "announce":
			cmdErr = a.cmdAnnounce(rest)
		case "ui":
			cmdErr = a.cmdUI(rest)
		case "version":
			if cmdErr = a.only("version", rest, 0, ""); cmdErr == nil {
				fmt.Fprintln(stdout, "agentcfg "+Version)
			}
		default:
			cmdErr = usageError(fmt.Sprintf("unknown command %q — run: agentcfg --help", cmd))
		}
	}
	var ue usageError
	var tf testFailed
	switch {
	case cmdErr == nil:
		return 0
	case errors.As(cmdErr, &ue):
		fmt.Fprintln(stderr, "agentcfg: "+cmdErr.Error())
		return 2
	case errors.As(cmdErr, &tf):
		fmt.Fprintln(stderr, "agentcfg: "+cmdErr.Error())
		return 3
	default:
		fmt.Fprintln(stderr, "agentcfg: "+cmdErr.Error())
		return 1
	}
}

func commandFlags(cmd string, flags map[string]string) error {
	allowed := map[string]bool{"config": true}
	switch cmd {
	case "set":
		allowed["base-url"] = true
	case "ui":
		allowed["idle"], allowed["no-open"] = true, true
	case "announce":
		allowed["channel"], allowed["envs"], allowed["webhook"], allowed["remove"] = true, true, true, true
	case "ls", "meta", "token", "migrate", "rm", "test", "version":
	default:
		return nil // the unknown-command diagnostic is more useful
	}
	for name := range flags {
		if !allowed[name] {
			return usageError(fmt.Sprintf("--%s does not apply to agentcfg %s", name, cmd))
		}
	}
	return nil
}

func (a *app) only(cmd string, args []string, n int, want string) error {
	if len(args) != n {
		return usageError(fmt.Sprintf("usage: agentcfg %s %s", cmd, want))
	}
	return nil
}

func (a *app) done(res *cfg.Result, msg string) {
	for _, w := range res.Warnings {
		fmt.Fprintln(a.stderr, "agentcfg: warning: "+w)
	}
	fmt.Fprintln(a.stdout, msg)
}

func (a *app) cmdLs(args []string) error {
	if err := a.only("ls", args, 0, ""); err != nil {
		return err
	}
	st := a.store.State()
	for _, w := range st.Warnings {
		fmt.Fprintln(a.stderr, "agentcfg: warning: "+w)
	}
	if st.Error != "" {
		return errors.New(st.Error)
	}
	if len(st.Services) == 0 {
		fmt.Fprintf(a.stdout, "no services in %s — add one: agentcfg set <name.env> --base-url URL\n", st.Path)
		return nil
	}
	tw := tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "SERVICE\tBASE URL\tTOKEN\tSTATUS")
	for _, s := range st.Services {
		tok := s.Token.Source
		if s.Token.Fingerprint != "" {
			tok += " " + s.Token.Fingerprint
		}
		status := "wired"
		if !s.Token.Wired {
			status = "not wired: " + s.Token.Reason
		}
		fmt.Fprintf(tw, "%s.%s\t%s\t%s\t%s\n", s.Name, s.Env, s.BaseURL, tok, status)
	}
	tw.Flush()
	if len(st.Announces) > 0 {
		fmt.Fprintln(a.stdout)
		tw = tabwriter.NewWriter(a.stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "SLACK\tCHANNEL\tENVS\tWEBHOOK\tSTATUS")
		for _, an := range st.Announces {
			hook := an.Webhook.Source
			if an.Webhook.Fingerprint != "" {
				hook += " " + an.Webhook.Fingerprint
			}
			status := "ready"
			if !an.Webhook.Wired {
				status = "not ready: " + an.Webhook.Reason
			}
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\n", an.Name, an.Channel, strings.Join(an.Envs, ","), hook, status)
		}
		tw.Flush()
	}
	if st.Plaintext > 0 {
		fmt.Fprintf(a.stdout, "\n%d secret(s) are still in plaintext in %s — run: agentcfg migrate\n", st.Plaintext, st.Path)
	}
	return nil
}

func (a *app) cmdAnnounce(args []string) error {
	if err := a.only("announce", args, 1, "<name> [--channel C] [--envs prod,dev] [--webhook] [--remove]"); err != nil {
		return err
	}
	name := args[0]
	if _, _, err := cfg.SplitFull(name + ".x"); err != nil {
		return usageError(err.Error())
	}
	if a.flags["remove"] != "" {
		for _, f := range []string{"channel", "envs", "webhook"} {
			if _, ok := a.flags[f]; ok {
				return usageError("--remove cannot be combined with --" + f)
			}
		}
		res, err := a.store.RemoveAnnounce("", name)
		if err != nil {
			return err
		}
		a.done(res, "removed "+name+".announce and its Keychain webhook")
		return nil
	}
	var e cfg.AnnounceEdit
	if c, ok := a.flags["channel"]; ok {
		e.Channel = &c
	}
	if v, ok := a.flags["envs"]; ok {
		e.Envs = []string{}
		for _, env := range strings.Split(v, ",") {
			if env = strings.TrimSpace(env); env != "" {
				e.Envs = append(e.Envs, env)
			}
		}
	}
	if e.Channel == nil && e.Envs == nil && a.flags["webhook"] == "" {
		return usageError("nothing to change: give --channel, --envs, --webhook or --remove")
	}
	if a.flags["webhook"] != "" {
		hook, err := a.readSecret("Slack webhook for " + name)
		if err != nil {
			return err
		}
		if hook == "" {
			return usageError("--webhook read an empty value from stdin")
		}
		e.Webhook = hook
	}
	res, err := a.store.SetAnnounce("", name, e)
	if err != nil {
		return err
	}
	a.done(res, "saved "+name+".announce — agentflow ship announce "+name+".<env> uses it")
	return nil
}

func (a *app) cmdSet(args []string) error {
	if err := a.only("set", args, 1, "<name.env> --base-url URL"); err != nil {
		return err
	}
	if _, _, err := cfg.SplitFull(args[0]); err != nil {
		return usageError(err.Error())
	}
	u, ok := a.flags["base-url"]
	if !ok {
		return usageError("usage: agentcfg set <name.env> --base-url URL")
	}
	res, err := a.store.SetBaseURL("", args[0], u)
	if err != nil {
		return err
	}
	a.done(res, "set "+args[0]+" base_url")
	return nil
}

func (a *app) cmdMeta(args []string) error {
	if len(args) < 2 {
		return usageError("usage: agentcfg meta <name> repo=PATH unit=UNIT (key= clears)")
	}
	if _, _, err := cfg.SplitFull(args[0] + ".x"); err != nil {
		return usageError(err.Error())
	}
	kv := map[string]string{}
	for _, p := range args[1:] {
		k, v, ok := strings.Cut(p, "=")
		if !ok || k == "" {
			return usageError(fmt.Sprintf("want key=value, got %q", p))
		}
		if _, dup := kv[k]; dup {
			return usageError(fmt.Sprintf("meta key %q given twice", k))
		}
		kv[k] = v
	}
	res, err := a.store.SetMeta("", args[0], kv)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	a.done(res, "set "+args[0]+".meta "+strings.Join(keys, ", "))
	return nil
}

func (a *app) cmdToken(args []string) error {
	if err := a.only("token", args, 1, "<name.env>  (token on stdin)"); err != nil {
		return err
	}
	if _, _, err := cfg.SplitFull(args[0]); err != nil {
		return usageError(err.Error())
	}
	tok, err := a.readToken(args[0])
	if err != nil {
		return err
	}
	res, err := a.store.SetToken("", args[0], tok)
	if err != nil {
		return err
	}
	a.done(res, "stored the "+args[0]+" token in the Keychain — check it: agentcfg test "+args[0])
	return nil
}

const maxTokenBytes = 8192

// readToken reads a token from stdin. A pipe must contain only the token and
// one optional line ending. On a terminal it reads one line with echo off.
func (a *app) readToken(full string) (string, error) { return a.readSecret("Token for " + full) }

// readSecret reads one secret (see readToken), prompting with label.
func (a *app) readSecret(label string) (string, error) {
	f, isFile := a.stdin.(*os.File)
	tty := isFile && a.isTerminal(f)
	if tty {
		fmt.Fprintf(a.stderr, "%s (not shown): ", label)
		if err := a.setEcho(f, "-echo"); err != nil {
			fmt.Fprintln(a.stderr)
			return "", fmt.Errorf("cannot disable terminal echo; refusing to read the token: %v", err)
		}
		defer func() {
			restoreErr := a.setEcho(f, "echo")
			fmt.Fprintln(a.stderr)
			if restoreErr != nil {
				// The token was typed without echo, so it is still safe to use.
				fmt.Fprintf(a.stderr, "agentcfg: warning: could not turn terminal echo back on (%v); run: stty echo\n", restoreErr)
			}
		}()
	}
	return readTokenValue(a.stdin, tty)
}

func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

func readTokenValue(r io.Reader, terminal bool) (string, error) {
	var raw []byte
	if terminal {
		line, err := bufio.NewReader(io.LimitReader(r, maxTokenBytes+3)).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			return "", fmt.Errorf("reading token: %v", err)
		}
		raw = []byte(line)
	} else {
		var err error
		raw, err = io.ReadAll(io.LimitReader(r, maxTokenBytes+3))
		if err != nil {
			return "", fmt.Errorf("reading token: %v", err)
		}
	}
	if len(raw) > maxTokenBytes+2 {
		return "", fmt.Errorf("token exceeds %d bytes", maxTokenBytes)
	}
	value := strings.TrimSuffix(string(raw), "\n")
	value = strings.TrimSuffix(value, "\r")
	if len(value) > maxTokenBytes {
		return "", fmt.Errorf("token exceeds %d bytes", maxTokenBytes)
	}
	return value, nil
}

func stty(f *os.File, arg string) error {
	cmd := exec.Command("stty", arg)
	cmd.Stdin = f
	return cmd.Run()
}

func (a *app) cmdMigrate(args []string) error {
	if err := a.only("migrate", args, 0, ""); err != nil {
		return err
	}
	mr, err := a.store.Migrate("")
	if err != nil {
		return err
	}
	for _, s := range mr.Skipped {
		fmt.Fprintln(a.stderr, "agentcfg: skipped "+s)
	}
	if len(mr.Moved) == 0 {
		fmt.Fprintln(a.stdout, "no plaintext tokens to move")
		return nil
	}
	a.done(mr.Result, fmt.Sprintf("moved %d token(s) into the Keychain: %s", len(mr.Moved), strings.Join(mr.Moved, ", ")))
	return nil
}

func (a *app) cmdRm(args []string) error {
	if err := a.only("rm", args, 1, "<name.env>"); err != nil {
		return err
	}
	if _, _, err := cfg.SplitFull(args[0]); err != nil {
		return usageError(err.Error())
	}
	res, err := a.store.Remove("", args[0])
	if err != nil {
		return err
	}
	a.done(res, "removed "+args[0])
	return nil
}

func (a *app) cmdTest(args []string) error {
	if err := a.only("test", args, 1, "<name.env>"); err != nil {
		return err
	}
	if _, _, err := cfg.SplitFull(args[0]); err != nil {
		return usageError(err.Error())
	}
	r := a.store.Test(args[0], "agentcfg/"+Version, 10*time.Second)
	if !r.OK {
		return testFailed(args[0] + ": " + r.Error)
	}
	fmt.Fprintf(a.stdout, "%s ok — version %s (%d ms)\n", args[0], r.Version, r.Millis)
	return nil
}

func (a *app) cmdUI(args []string) error {
	if err := a.only("ui", args, 0, "[--no-open] [--idle DUR]"); err != nil {
		return err
	}
	idle := 15 * time.Minute
	if v, ok := a.flags["idle"]; ok {
		d, err := time.ParseDuration(v)
		if err != nil || d < time.Minute {
			return usageError("--idle wants a duration of at least 1m, like 30m")
		}
		idle = d
	}
	return ui.Serve(ui.Options{
		Store:   a.store,
		Version: Version,
		Idle:    idle,
		Open:    a.flags["no-open"] == "",
		Stdout:  a.stdout,
	})
}
