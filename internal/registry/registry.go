package registry

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strings"

	"github.com/BurntSushi/toml"
)

// Service is one [name.env] table from services.toml.
type Service struct {
	Name           string
	Env            string
	BaseURL        string
	Token          Secret
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
func ResolvePath(flagVal string) string {
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

// Load reads and validates the registry file.
func Load(path string) (*Registry, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("config file not found: %s\nCreate it with entries like:\n\n%s", path, ExampleTOML)
	}
	reg := &Registry{Path: path}
	if runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		reg.Warnings = append(reg.Warnings, fmt.Sprintf("config file %s is readable by group/other — consider chmod 600", path))
	}

	var raw map[string]map[string]toml.Primitive
	md, err := toml.DecodeFile(path, &raw)
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
		BaseURL string `toml:"base_url"`
		Token   string `toml:"token"`
	}

	for _, name := range names {
		envs := make([]string, 0, len(raw[name]))
		for env := range raw[name] {
			if env != "meta" {
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
				default:
					reg.Warnings = append(reg.Warnings, fmt.Sprintf("[%s.%s] unknown key %q ignored", name, env, k))
				}
			}
			base, err := validateBaseURL(et.BaseURL)
			if err != nil {
				return nil, fmt.Errorf("[%s.%s] in %s: %v", name, env, path, err)
			}
			svc := Service{
				Name:    name,
				Env:     env,
				BaseURL: base,
				Token:   NewSecret(et.Token),
				Meta:    metas[name],
			}
			if reason := placeholderReason(et.Token); reason != "" {
				svc.Wired = false
				svc.NotWiredReason = reason
			} else {
				svc.Wired = true
			}
			reg.Services = append(reg.Services, svc)
		}
	}
	return reg, nil
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

// placeholderReason returns a non-empty reason if the token is a placeholder.
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
