package cfg

import (
	"context"
	"errors"
	"fmt"
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/BurntSushi/toml"

	"github.com/TysonLabs/agentctl/internal/client"
	"github.com/TysonLabs/agentctl/internal/gatespec"
	"github.com/TysonLabs/agentctl/internal/keychain"
	"github.com/TysonLabs/agentctl/internal/registry"
	"github.com/TysonLabs/agentctl/internal/render"
	"github.com/TysonLabs/agentctl/internal/slackhook"
)

// State is what `agentcfg ls` and the UI show. It never holds a token, only
// its fingerprint.
type State struct {
	Path      string         `json:"path"`
	Version   string         `json:"version"`
	Error     string         `json:"error,omitempty"` // the file exists but agentctl cannot use it
	Warnings  []string       `json:"warnings"`
	Services  []ServiceView  `json:"services"`
	Announces []AnnounceView `json:"announces"`
	Gates     []GateView     `json:"gates"`
	Plaintext int            `json:"plaintext"` // real tokens and webhooks still in the file
}

// GateView is one [name.gate] table (agentflow gate's steps), in order.
// Error says why agentflow would refuse it; Steps then holds what could be
// read.
type GateView struct {
	Name  string            `json:"name"`
	Meta  map[string]string `json:"meta"` // string keys of [name.meta], for gate-only projects
	Lock  string            `json:"lock"`
	Steps []GateStep        `json:"steps"`
	Error string            `json:"error,omitempty"`
}

// AnnounceView is one [name.announce] table (agentflow's Slack settings).
// It never holds the webhook, only its fingerprint.
type AnnounceView struct {
	Name    string            `json:"name"`
	Meta    map[string]string `json:"meta"`
	Channel string            `json:"channel"`
	Envs    []string          `json:"envs"`
	Webhook TokenView         `json:"webhook"`
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
	st := &State{Path: s.Path, Warnings: []string{}, Services: []ServiceView{}, Announces: []AnnounceView{}, Gates: []GateView{}}
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
	var inline int
	st.Announces, inline = announceViews(data)
	st.Plaintext += inline
	st.Gates = gateViews(data)
	return st
}

// gateViews reads every [name.gate] table. Validity comes from gatespec, the
// rules agentflow gate applies.
func gateViews(data []byte) []GateView {
	out := []GateView{}
	tree := map[string]any{}
	if _, err := toml.Decode(string(data), &tree); err != nil {
		return out
	}
	for _, name := range sortedKeys(tree) {
		svc, _ := tree[name].(map[string]any)
		raw, ok := svc["gate"]
		if !ok {
			continue
		}
		v := GateView{Name: name, Meta: map[string]string{}, Steps: []GateStep{}}
		if meta, ok := svc["meta"].(map[string]any); ok {
			for k, mv := range meta {
				if s, ok := mv.(string); ok {
					v.Meta[k] = s
				}
			}
		}
		if _, err := gatespec.FromTable(name, raw); err != nil {
			v.Error = err.Error()
		}
		tbl, _ := raw.(map[string]any)
		v.Lock, _ = tbl["lock"].(string)
		steps, _ := gatespec.StepTables(tbl["steps"])
		for _, m := range steps {
			var st GateStep
			st.Name, _ = m["name"].(string)
			st.Run, _ = m["run"].(string)
			st.Timeout, _ = m["timeout"].(string)
			st.StopOnFail, _ = m["stop_on_fail"].(bool)
			v.Steps = append(v.Steps, st)
		}
		out = append(out, v)
	}
	return out
}

// announceViews reads every [name.announce] table and counts the inline
// webhooks Migrate would move. Ready (Webhook.Wired) comes from
// validateAnnounceTable, the same rules SetAnnounce enforces and agentflow
// applies, so the page and the CLI never disagree with them.
func announceViews(data []byte) ([]AnnounceView, int) {
	out := []AnnounceView{}
	plaintext := 0
	tree := map[string]any{}
	if _, err := toml.Decode(string(data), &tree); err != nil {
		return out, 0
	}
	for _, name := range sortedKeys(tree) {
		svc, _ := tree[name].(map[string]any)
		tbl, ok := svc["announce"].(map[string]any)
		if !ok {
			continue
		}
		v := AnnounceView{Name: name, Meta: map[string]string{}, Envs: []string{"prod"}}
		if meta, ok := svc["meta"].(map[string]any); ok {
			for k, mv := range meta {
				if s, ok := mv.(string); ok {
					v.Meta[k] = s
				}
			}
		}
		hook, isStr := tbl["webhook"].(string)
		_, hasRef := tbl["webhook_ref"]
		switch {
		case hasRef:
			v.Webhook.Source = "keychain"
			if ref, ok := tbl["webhook_ref"].(string); ok {
				if acct, err := keychain.ParseRef(ref); err == nil {
					hook, _ = keychain.GetFrom(keychain.AgentflowService, acct)
				}
			}
		case isStr:
			v.Webhook.Source = "file"
			if slackhook.Valid(hook) {
				plaintext++
			}
		default:
			v.Webhook.Source = "none"
		}
		if hook != "" {
			v.Webhook.Fingerprint = registry.NewSecret(hook).Fingerprint()
		}
		// Display values pass through agentflow's cleaner, so a webhook
		// pasted into the wrong field is never shown.
		v.Channel, _ = tbl["channel"].(string)
		v.Channel = slackhook.CleanText(v.Channel, hook)
		if envs, ok := tbl["envs"].([]any); ok {
			v.Envs = []string{}
			for _, e := range envs {
				if s, ok := e.(string); ok {
					v.Envs = append(v.Envs, slackhook.CleanText(s, hook))
				}
			}
		}
		if err := validateAnnounceTable(name, tbl, ""); err != nil {
			v.Webhook.Reason = err.Error()
		} else {
			v.Webhook.Wired = true
		}
		out = append(out, v)
	}
	return out, plaintext
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
