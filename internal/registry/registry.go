package registry

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"runtime"
	"sort"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"

	"github.com/TysonLabs/agentctl/internal/configpath"
	"github.com/TysonLabs/agentctl/internal/keychain"
)

// Service is one [name.env] table from services.toml.
type Service struct {
	Name           string
	Env            string
	BaseURL        string
	Token          Secret
	TokenRef       string // "keychain:<account>" when the token lives in the Keychain
	Wired          bool
	NotWiredReason string
	Meta           map[string]string // from the sibling [name.meta] table
}

// FullName returns "name.env".
func (s Service) FullName() string { return s.Name + "." + s.Env }

// Registry is the parsed services.toml.
type Registry struct {
	Path     string
	Services []Service
	Warnings []string
}

// ExampleTOML is shown when the config file is missing so an agent can write one.
const ExampleTOML = `[myservice.dev]
base_url = "https://dev.example.com"
token    = "REPLACE_ME"
`

// ResolvePath picks the config path: --config flag > AGENTCTL_CONFIG > default.
func ResolvePath(flagVal string) string { return configpath.Resolve(flagVal) }

// reservedTables are per-service tables that are not environments. agentctl
// reads [name.meta]; [name.announce] belongs to agentflow (it holds a Slack
// webhook, a write credential) and agentctl never decodes it.
var reservedTables = map[string]bool{"meta": true, "announce": true}

// Load reads and validates the registry file, then reads every token_ref
// from the Keychain.
func Load(path string) (*Registry, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("config file not found: %s\nCreate it with entries like:\n\n%s", path, ExampleTOML)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %v", path, err)
	}
	reg, err := Parse(path, data)
	if err != nil {
		return nil, err
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		reg.Warnings = append([]string{fmt.Sprintf("config file %s is readable by group/other — consider chmod 600", path)}, reg.Warnings...)
	}
	reg.ResolveKeychain()
	return reg, nil
}

// Parse validates registry file content without touching the Keychain:
// services with a token_ref come back not wired until ResolveKeychain runs.
// path is used only in messages. agentcfg runs it on every file it writes.
func Parse(path string, data []byte) (*Registry, error) {
	reg := &Registry{Path: path}
	var raw map[string]map[string]toml.Primitive
	md, err := toml.Decode(string(data), &raw)
	if err != nil {
		return nil, sanitizeTOMLError(path, err)
	}

	names := make([]string, 0, len(raw))
	for name := range raw {
		names = append(names, name)
	}
	sort.Strings(names)

	metas := map[string]map[string]string{}
	for _, name := range names {
		if prim, ok := raw[name]["meta"]; ok {
			var m map[string]string
			if err := md.PrimitiveDecode(prim, &m); err != nil {
				return nil, fmt.Errorf("parsing [%s.meta] in %s: %v", name, path, err)
			}
			metas[name] = m
		}
	}

	type envTable struct {
		BaseURL  string `toml:"base_url"`
		Token    string `toml:"token"`
		TokenRef string `toml:"token_ref"`
	}

	for _, name := range names {
		envs := make([]string, 0, len(raw[name]))
		for env := range raw[name] {
			if !reservedTables[env] {
				envs = append(envs, env)
			}
		}
		sort.Strings(envs)
		for _, env := range envs {
			var et envTable
			var loose map[string]any
			if err := md.PrimitiveDecode(raw[name][env], &loose); err != nil {
				return nil, fmt.Errorf("parsing [%s.%s] in %s: %v", name, env, path, err)
			}
			for k, v := range loose {
				switch k {
				case "base_url":
					s, ok := v.(string)
					if !ok {
						return nil, fmt.Errorf("[%s.%s] base_url must be a string in %s", name, env, path)
					}
					et.BaseURL = s
				case "token":
					s, ok := v.(string)
					if !ok {
						return nil, fmt.Errorf("[%s.%s] token must be a string in %s", name, env, path)
					}
					et.Token = s
				case "token_ref":
					s, ok := v.(string)
					if !ok {
						return nil, fmt.Errorf("[%s.%s] token_ref must be a string in %s", name, env, path)
					}
					et.TokenRef = s
				default:
					reg.Warnings = append(reg.Warnings, fmt.Sprintf("[%s.%s] unknown key %q ignored", name, env, k))
				}
			}
			base, err := validateBaseURL(et.BaseURL)
			if err != nil {
				return nil, fmt.Errorf("[%s.%s] in %s: %v", name, env, path, err)
			}
			_, hasToken := loose["token"]
			_, hasTokenRef := loose["token_ref"]
			if hasToken && hasTokenRef {
				return nil, fmt.Errorf("[%s.%s] in %s: set token or token_ref, not both", name, env, path)
			}
			svc := Service{
				Name:     name,
				Env:      env,
				BaseURL:  base,
				Token:    NewSecret(et.Token),
				TokenRef: et.TokenRef,
				Meta:     metas[name],
			}
			if hasTokenRef {
				if _, err := keychain.ParseRef(et.TokenRef); err != nil {
					return nil, fmt.Errorf("[%s.%s] in %s: %v", name, env, path, err)
				}
				svc.NotWiredReason = "keychain token not read yet"
			} else if reason := placeholderReason(et.Token); reason != "" {
				svc.NotWiredReason = reason
			} else {
				svc.Wired = true
			}
			reg.Services = append(reg.Services, svc)
		}
	}
	return reg, nil
}

// ResolveKeychain reads every token_ref, in parallel (one security call each,
// ~20 ms). A missing or unreadable item leaves the service not wired, with the
// reason; it never falls back to another token.
func (r *Registry) ResolveKeychain() {
	var wg sync.WaitGroup
	for i := range r.Services {
		svc := &r.Services[i]
		if svc.TokenRef == "" {
			continue
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			// Start from not wired every time, so a rerun never keeps a token
			// whose item is gone.
			svc.Token, svc.Wired = Secret{}, false
			acct, _ := keychain.ParseRef(svc.TokenRef) // validated by Parse
			tok, err := keychain.Get(acct)
			switch {
			case errors.Is(err, keychain.ErrNotFound):
				svc.NotWiredReason = fmt.Sprintf("no keychain item %s/%s — run: agentcfg token %s", keychain.Service, acct, svc.FullName())
			case err != nil:
				svc.NotWiredReason = err.Error()
			default:
				svc.Token = NewSecret(tok)
				if reason := placeholderReason(tok); reason != "" {
					svc.NotWiredReason = "keychain " + reason
				} else {
					svc.Wired, svc.NotWiredReason = true, ""
				}
			}
		}()
	}
	wg.Wait()
}

// sanitizeTOMLError converts a TOML decode error into a message that never
// echoes file content. Raw parse errors quote the offending lexeme, which for
// a malformed `token = ...` line would print the token value to stderr —
// defeating the never-print-tokens guarantee. Report only position and key.
func sanitizeTOMLError(path string, err error) error {
	var pe toml.ParseError
	if errors.As(err, &pe) {
		loc := fmt.Sprintf("line %d", pe.Position.Line)
		if pe.LastKey != "" {
			loc += fmt.Sprintf(" (key %q)", pe.LastKey)
		}
		return fmt.Errorf("parsing %s: TOML syntax error at %s (file content redacted — check quoting and syntax on that line)", path, loc)
	}
	return fmt.Errorf("parsing %s: TOML decode error (file content redacted — check the file's syntax)", path)
}

// Lookup finds a service by "name.env".
func (r *Registry) Lookup(full string) (Service, bool) {
	for _, s := range r.Services {
		if s.FullName() == full {
			return s, true
		}
	}
	return Service{}, false
}

// Secrets returns every wired token, for scrubbing output.
func (r *Registry) Secrets() []Secret {
	var out []Secret
	for _, s := range r.Services {
		if !s.Token.IsZero() {
			out = append(out, s.Token)
		}
	}
	return out
}

func validateBaseURL(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("missing base_url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", fmt.Errorf("invalid base_url: %v", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return "", fmt.Errorf("base_url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", fmt.Errorf("base_url must have a host")
	}
	if u.User != nil {
		return "", fmt.Errorf("base_url must not contain userinfo")
	}
	if u.RawQuery != "" {
		return "", fmt.Errorf("base_url must not contain a query")
	}
	if u.Fragment != "" || u.RawFragment != "" {
		return "", fmt.Errorf("base_url must not contain a fragment")
	}
	return strings.TrimRight(raw, "/"), nil
}

var fillerOnly = regexp.MustCompile(`^[xX*._\x{2026}-]+$`)

// PlaceholderReason returns a non-empty reason if the token is a placeholder.
// agentcfg uses it to refuse storing one.
func PlaceholderReason(tok string) string { return placeholderReason(tok) }

func placeholderReason(tok string) string {
	t := strings.TrimSpace(tok)
	if t == "" {
		return "empty token"
	}
	up := strings.ToUpper(t)
	switch {
	case up == "REPLACE_ME", up == "CHANGEME", up == "TODO", t == "…", t == "...":
		return "placeholder token"
	case strings.HasPrefix(t, "<") && strings.HasSuffix(t, ">"):
		return "placeholder token"
	case fillerOnly.MatchString(t):
		return "placeholder token"
	case len(t) < 8:
		return "token too short"
	}
	return ""
}
