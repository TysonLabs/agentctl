package cfg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/TysonLabs/agentctl/internal/client"
	"github.com/TysonLabs/agentctl/internal/registry"
	"github.com/TysonLabs/agentctl/internal/render"
)

// State is what `agentcfg ls` and the UI show. It never holds a token, only
// its fingerprint.
type State struct {
	Path      string        `json:"path"`
	Version   string        `json:"version"`
	Error     string        `json:"error,omitempty"` // the file exists but agentctl cannot use it
	Warnings  []string      `json:"warnings"`
	Services  []ServiceView `json:"services"`
	Plaintext int           `json:"plaintext"` // envs whose real token is still in the file
}

// ServiceView is one name.env.
type ServiceView struct {
	Name    string            `json:"name"`
	Env     string            `json:"env"`
	BaseURL string            `json:"base_url"`
	Meta    map[string]string `json:"meta"`
	Token   TokenView         `json:"token"`
}

// TokenView describes a token without revealing it.
type TokenView struct {
	Source      string `json:"source"` // keychain, file or none
	Fingerprint string `json:"fingerprint,omitempty"`
	Wired       bool   `json:"wired"`
	Reason      string `json:"reason,omitempty"`
}

// State reads the file once, so Version always matches what is shown.
func (s *Store) State() *State {
	st := &State{Path: s.Path, Warnings: []string{}, Services: []ServiceView{}}
	data, err := os.ReadFile(s.Path)
	if errors.Is(err, os.ErrNotExist) {
		st.Version = VersionOf(nil)
		return st
	}
	if err != nil {
		st.Error = fmt.Sprintf("reading %s: %v", s.Path, err)
		return st
	}
	st.Version = VersionOf(data)
	if fi, err := os.Stat(s.Path); err == nil && runtime.GOOS != "windows" && fi.Mode().Perm()&0o077 != 0 {
		st.Warnings = append(st.Warnings, fmt.Sprintf("%s is readable by group/other — run: chmod 600 %s", s.Path, s.Path))
	}
	reg, err := registry.Parse(s.Path, data)
	if err != nil {
		st.Error = err.Error()
		return st
	}
	reg.ResolveKeychain()
	st.Warnings = append(st.Warnings, reg.Warnings...)
	for _, svc := range reg.Services {
		v := ServiceView{Name: svc.Name, Env: svc.Env, BaseURL: svc.BaseURL, Meta: svc.Meta}
		if v.Meta == nil {
			v.Meta = map[string]string{}
		}
		switch {
		case svc.TokenRef != "":
			v.Token.Source = "keychain"
		case !svc.Token.IsZero():
			v.Token.Source = "file"
			if svc.Wired { // a placeholder is no secret, and Migrate skips it
				st.Plaintext++
			}
		default:
			v.Token.Source = "none"
		}
		if !svc.Token.IsZero() {
			v.Token.Fingerprint = svc.Token.Fingerprint()
		}
		v.Token.Wired, v.Token.Reason = svc.Wired, svc.NotWiredReason
		st.Services = append(st.Services, v)
	}
	return st
}

// TestResult is one GET /agent/version with the stored token.
type TestResult struct {
	OK      bool   `json:"ok"`
	Status  int    `json:"status,omitempty"`
	Version string `json:"version,omitempty"`
	Error   string `json:"error,omitempty"`
	Millis  int64  `json:"ms"`
}

// Test calls name.env's /agent/version through agentctl's own client, so a
// pass here means agentctl works too.
func (s *Store) Test(full, ua string, timeout time.Duration) TestResult {
	reg, err := registry.Load(s.Path)
	if err != nil {
		return TestResult{Error: err.Error()}
	}
	svc, ok := reg.Lookup(full)
	if !ok {
		return TestResult{Error: "no service " + full}
	}
	if !svc.Wired {
		return TestResult{Error: "not wired: " + svc.NotWiredReason}
	}
	c, err := client.New(svc.BaseURL, svc.Token, timeout, ua)
	if err != nil {
		return TestResult{Error: err.Error()}
	}
	start := time.Now()
	resp, err := c.Get(context.Background(), "/agent/version", "")
	ms := time.Since(start).Milliseconds()
	if err != nil {
		return TestResult{Error: client.Scrub(err.Error(), reg.Secrets()...), Millis: ms}
	}
	r := TestResult{Status: resp.Status, Millis: ms}
	if resp.Status >= 400 {
		r.Error = fmt.Sprintf("HTTP %d", resp.Status)
		if resp.Status == 401 || resp.Status == 403 {
			r.Error += " — the service rejected the token"
		}
		return r
	}
	r.OK, r.Version = true, safeVersion(render.VersionFromBody(resp.Body), reg.Secrets())
	return r
}

// hiddenVersion replaces a version string that overlaps a token.
const hiddenVersion = "(hidden: it matches a token)"

// safeVersion keeps a service-controlled version string out of agentcfg's
// output when it could be token material. A service can reflect the bearer
// token, and VersionFromBody truncates to 40 characters, so an exact-match
// scrub would miss a token prefix: hide any version that contains a token
// or is a fragment (6+ characters) of one.
func safeVersion(v string, secrets []registry.Secret) string {
	for _, s := range secrets {
		tok := s.Reveal()
		if tok == "" {
			continue
		}
		if strings.Contains(v, tok) || (len(v) >= 6 && strings.Contains(tok, v)) {
			return hiddenVersion
		}
	}
	return v
}
