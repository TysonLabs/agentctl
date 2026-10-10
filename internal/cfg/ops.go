package cfg

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode"

	"github.com/TysonLabs/agentctl/internal/keychain"
	"github.com/TysonLabs/agentctl/internal/registry"
	"github.com/TysonLabs/agentctl/internal/slackhook"
)

var nameRe = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)

// reservedEnvs are per-service tables that are not environments: agentctl
// reads [name.meta], and [name.announce] and [name.gate] belong to agentflow.
var reservedEnvs = map[string]bool{"meta": true, "announce": true, "gate": true}

// metaKeys are the [name.meta] keys agentcfg edits. Other meta keys are kept
// as they are.
var metaKeys = map[string]bool{"repo": true, "unit": true}

// SplitFull parses "name.env" and validates both parts.
func SplitFull(full string) (name, env string, err error) {
	name, env, ok := strings.Cut(full, ".")
	if !ok {
		return "", "", fmt.Errorf("want <name>.<env>, got %q", full)
	}
	if err := checkName(name); err != nil {
		return "", "", err
	}
	if !nameRe.MatchString(env) {
		return "", "", fmt.Errorf("env %q must be 1-64 letters, digits, _ or -", env)
	}
	if reservedEnvs[env] {
		return "", "", fmt.Errorf("%q is a reserved table name, not an env", env)
	}
	return name, env, nil
}

func checkName(name string) error {
	if !nameRe.MatchString(name) {
		return fmt.Errorf("service name %q must be 1-64 letters, digits, _ or -", name)
	}
	return nil
}

// account is the Keychain account agentcfg uses for name.env.
func account(name, env string) string { return name + "." + env }

func (d *Doc) service(name string, create bool) (map[string]any, error) {
	v, ok := d.Tree[name]
	if !ok {
		if !create {
			return nil, fmt.Errorf("no service %q", name)
		}
		m := map[string]any{}
		d.Tree[name] = m
		return m, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%q is not a table; fix the file by hand", name)
	}
	return m, nil
}

func (d *Doc) env(name, env string, create bool) (map[string]any, error) {
	svc, err := d.service(name, create)
	if err != nil {
		return nil, err
	}
	v, ok := svc[env]
	if !ok {
		if !create {
			return nil, fmt.Errorf("no env %s.%s", name, env)
		}
		m := map[string]any{}
		svc[env] = m
		return m, nil
	}
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s.%s is not a table; fix the file by hand", name, env)
	}
	return m, nil
}

// SetBaseURL sets name.env's base_url, creating the env if it is new.
// registry.Parse validates the URL when the store writes.
func (s *Store) SetBaseURL(expect, full, baseURL string) (*Result, error) {
	name, env, err := SplitFull(full)
	if err != nil {
		return nil, err
	}
	baseURL = strings.TrimSpace(baseURL)
	if baseURL == "" {
		return nil, fmt.Errorf("base URL is empty")
	}
	return s.Edit(expect, func(d *Doc) error {
		tbl, err := d.env(name, env, true)
		if err != nil {
			return err
		}
		tbl["base_url"] = baseURL
		return nil
	})
}

// SetMeta sets [name.meta] keys; an empty value removes the key. Only repo
// and unit are editable. Setting a value on an unknown name creates it.
func (s *Store) SetMeta(expect, name string, kv map[string]string) (*Result, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	for k, v := range kv {
		if !metaKeys[k] {
			return nil, fmt.Errorf("meta key %q is not editable (repo, unit)", k)
		}
		if strings.ContainsFunc(v, unicode.IsControl) {
			return nil, fmt.Errorf("meta %s has control characters", k)
		}
	}
	setsValue := false
	for _, v := range kv {
		if strings.TrimSpace(v) != "" {
			setsValue = true
		}
	}
	return s.Edit(expect, func(d *Doc) error {
		// Setting a value may create the project: a gate-only project has
		// meta and gate, and no env.
		svc, err := d.service(name, setsValue)
		if err != nil {
			return err
		}
		meta, _ := svc["meta"].(map[string]any)
		if meta == nil {
			if _, exists := svc["meta"]; exists {
				return fmt.Errorf("%s.meta is not a table; fix the file by hand", name)
			}
			meta = map[string]any{}
		}
		for k, v := range kv {
			if v = strings.TrimSpace(v); v == "" {
				delete(meta, k)
			} else {
				meta[k] = v
			}
		}
		if len(meta) == 0 {
			delete(svc, "meta")
			if len(svc) == 0 {
				delete(d.Tree, name)
			}
		} else {
			svc["meta"] = meta
		}
		return nil
	})
}

// CheckToken rejects a token agentctl would treat as a placeholder, or one
// with whitespace or control characters (a paste accident).
func CheckToken(tok string) error {
	if tok == "" {
		return fmt.Errorf("token is empty")
	}
	if strings.ContainsFunc(tok, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return fmt.Errorf("token has spaces or control characters")
	}
	if reason := registry.PlaceholderReason(tok); reason != "" {
		return fmt.Errorf("refusing to store a %s", reason)
	}
	return nil
}

// SetToken stores name.env's token in the Keychain and points the file at it
// (token_ref), removing any plaintext token. The env must exist.
func (s *Store) SetToken(expect, full, tok string) (*Result, error) {
	name, env, err := SplitFull(full)
	if err != nil {
		return nil, err
	}
	if err := CheckToken(tok); err != nil {
		return nil, err
	}
	return s.Edit(expect, func(d *Doc) error {
		tbl, err := d.env(name, env, false)
		if err != nil {
			return fmt.Errorf("%v — add it first with a base URL", err)
		}
		acct := account(name, env)
		d.BeforeSave(func() error { return keychainSet(keychain.Service, acct, tok) })
		delete(tbl, "token")
		tbl["token_ref"] = keychain.RefPrefix + acct
		return nil
	})
}

// Remove deletes name.env, then its Keychain item if no other env refers to
// it. [name.meta] goes too once the service has no env and no gate left;
// tables agentcfg does not own (announce) stay.
func (s *Store) Remove(expect, full string) (*Result, error) {
	name, env, err := SplitFull(full)
	if err != nil {
		return nil, err
	}
	return s.Edit(expect, func(d *Doc) error {
		svc, err := d.service(name, false)
		if err != nil {
			return err
		}
		tbl, err := d.env(name, env, false)
		if err != nil {
			return err
		}
		ref, _ := tbl["token_ref"].(string)
		delete(svc, env)
		hasEnv := false
		for k := range svc {
			if !reservedEnvs[k] {
				hasEnv = true
			}
		}
		// [name.gate] resolves its project through meta.repo, so a gate keeps
		// the meta table alive after the last env goes.
		if _, hasGate := svc["gate"]; !hasEnv && !hasGate {
			delete(svc, "meta")
		}
		if len(svc) == 0 {
			delete(d.Tree, name)
		}
		if acct, err := keychain.ParseRef(ref); err == nil && !d.refersTo(ref) {
			d.OnSaved(func() error { return keychainDelete(keychain.Service, acct) })
		}
		return nil
	})
}

func (d *Doc) refersTo(ref string) bool {
	return d.refersToKey("token_ref", ref)
}

func (d *Doc) refersToKey(key, ref string) bool {
	for _, sv := range d.Tree {
		svc, _ := sv.(map[string]any)
		for _, tv := range svc {
			if tbl, ok := tv.(map[string]any); ok && tbl[key] == ref {
				return true
			}
		}
	}
	return false
}

// MigrateResult lists what Migrate did.
type MigrateResult struct {
	*Result
	Moved   []string // name.env moved into the Keychain
	Skipped []string // "name.env: reason"
}

// Migrate moves every plaintext token into the Keychain in one edit. Each
// item is written and read back before the file changes; any failure leaves
// the file as it was (items already written are overwritten on a rerun).
func (s *Store) Migrate(expect string) (*MigrateResult, error) {
	mr := &MigrateResult{}
	res, err := s.Edit(expect, func(d *Doc) error {
		mr.Moved, mr.Skipped = nil, nil
		for _, name := range sortedKeys(d.Tree) {
			svc, _ := d.Tree[name].(map[string]any)
			for _, env := range sortedKeys(svc) {
				tbl, _ := svc[env].(map[string]any)
				tok, isStr := tbl["token"].(string)
				if reservedEnvs[env] || tbl == nil || !isStr {
					continue
				}
				full := name + "." + env
				if _, hasRef := tbl["token_ref"]; hasRef {
					return fmt.Errorf("%s sets both token and token_ref; refusing to choose one", full)
				}
				if !nameRe.MatchString(name) || !nameRe.MatchString(env) {
					mr.Skipped = append(mr.Skipped, full+": name has characters a keychain account cannot hold")
					continue
				}
				if err := CheckToken(tok); err != nil {
					mr.Skipped = append(mr.Skipped, full+": "+err.Error())
					continue
				}
				acct := account(name, env)
				d.BeforeSave(func() error {
					if err := keychainSet(keychain.Service, acct, tok); err != nil {
						return fmt.Errorf("%s: %v (file unchanged)", full, err)
					}
					return nil
				})
				delete(tbl, "token")
				tbl["token_ref"] = keychain.RefPrefix + acct
				mr.Moved = append(mr.Moved, full)
			}
			if err := migrateWebhook(d, name, svc, mr); err != nil {
				return err
			}
		}
		if len(mr.Moved) == 0 {
			return errNothingToDo
		}
		return nil
	})
	if err == errNothingToDo {
		return mr, nil
	}
	if err != nil {
		return nil, err
	}
	mr.Result = res
	return mr, nil
}

var errNothingToDo = fmt.Errorf("nothing to do")

// migrateWebhook moves an inline [name.announce] webhook into the Keychain
// (service "agentflow"), the same way Migrate moves tokens.
func migrateWebhook(d *Doc, name string, svc map[string]any, mr *MigrateResult) error {
	tbl, _ := svc["announce"].(map[string]any)
	hook, isStr := tbl["webhook"].(string)
	if tbl == nil || !isStr {
		return nil
	}
	full := name + ".announce"
	if _, hasRef := tbl["webhook_ref"]; hasRef {
		return fmt.Errorf("%s sets both webhook and webhook_ref; refusing to choose one", full)
	}
	if !nameRe.MatchString(name) {
		mr.Skipped = append(mr.Skipped, full+": name has characters a keychain account cannot hold")
		return nil
	}
	if !slackhook.Valid(hook) {
		mr.Skipped = append(mr.Skipped, full+": webhook is not a Slack incoming webhook")
		return nil
	}
	acct := announceAccount(name)
	d.BeforeSave(func() error {
		if err := keychainSet(keychain.AgentflowService, acct, hook); err != nil {
			return fmt.Errorf("%s: %v (file unchanged)", full, err)
		}
		return nil
	})
	delete(tbl, "webhook")
	tbl["webhook_ref"] = keychain.RefPrefix + acct
	mr.Moved = append(mr.Moved, full)
	return nil
}

// AnnounceEdit changes [name.announce], agentflow's Slack settings. A nil
// field keeps its current value.
type AnnounceEdit struct {
	Channel *string
	Envs    []string // nil keeps; agentflow defaults a missing list to ["prod"]
	Webhook string   // "" keeps; otherwise stored in the Keychain
}

// announceAccount is the Keychain account (service "agentflow") of name's
// Slack webhook.
func announceAccount(name string) string { return name + ".announce" }

func checkAnnounceEdit(e AnnounceEdit) error {
	if e.Channel != nil {
		c := strings.TrimSpace(*e.Channel)
		if c == "" || len(c) > 80 || strings.ContainsFunc(c, unicode.IsControl) || strings.Contains(c, "hooks.slack.com") {
			return fmt.Errorf("channel must be a short label such as \"#releases\"")
		}
		*e.Channel = c
	}
	if e.Envs != nil {
		if len(e.Envs) == 0 {
			return fmt.Errorf("envs must name at least one env")
		}
		for _, env := range e.Envs {
			if !nameRe.MatchString(env) || reservedEnvs[env] {
				// Do not quote the submitted value: a webhook pasted into the
				// wrong field must not be reflected in an API/CLI error.
				return fmt.Errorf("envs entries must be 1-64 letters, digits, _ or -")
			}
		}
	}
	if e.Webhook != "" && !slackhook.Valid(e.Webhook) {
		return fmt.Errorf("webhook must be a Slack incoming webhook (https://hooks.slack.com/services/...)")
	}
	return nil
}

var announceKeys = map[string]bool{"webhook": true, "webhook_ref": true, "channel": true, "envs": true}

// validateAnnounceTable applies agentflow's full-table rules to the result of
// an edit. pendingWebhook is the value that a BeforeSave callback will put at
// name.announce; it lets validation happen before that external side effect.
func validateAnnounceTable(name string, tbl map[string]any, pendingWebhook string) error {
	for key := range tbl {
		if !announceKeys[key] {
			return fmt.Errorf("%s.announce has an unknown key; fix the file by hand", name)
		}
	}
	rawHook, hasHook := tbl["webhook"]
	rawRef, hasRef := tbl["webhook_ref"]
	if hasHook && hasRef {
		return fmt.Errorf("%s.announce sets both webhook and webhook_ref; refusing to choose one", name)
	}
	if !hasHook && !hasRef {
		return fmt.Errorf("%s.announce needs a webhook — add --webhook (read from stdin)", name)
	}

	var hook string
	if hasHook {
		var ok bool
		hook, ok = rawHook.(string)
		if !ok {
			return fmt.Errorf("%s.announce webhook must be a string", name)
		}
	} else {
		ref, ok := rawRef.(string)
		if !ok {
			return fmt.Errorf("%s.announce webhook_ref must be a string", name)
		}
		acct, err := keychain.ParseRef(ref)
		if err != nil {
			return fmt.Errorf("%s.announce webhook_ref is malformed", name)
		}
		if pendingWebhook != "" && acct == announceAccount(name) {
			hook = pendingWebhook
		} else {
			hook, err = keychain.GetFrom(keychain.AgentflowService, acct)
			if errors.Is(err, keychain.ErrNotFound) {
				return fmt.Errorf("%s.announce has no Keychain webhook; add --webhook", name)
			}
			if err != nil {
				return fmt.Errorf("%s.announce cannot read its Keychain webhook: %v", name, err)
			}
		}
	}
	if !slackhook.Valid(hook) {
		return fmt.Errorf("%s.announce webhook must be a Slack incoming webhook", name)
	}
	channel, ok := tbl["channel"].(string)
	if !ok || strings.TrimSpace(channel) == "" {
		return fmt.Errorf("%s.announce needs a channel label — add --channel \"#releases\"", name)
	}
	if len(channel) > 80 || !slackhook.SafeLabel(channel, hook) {
		return fmt.Errorf("channel must be a short label such as \"#releases\"")
	}
	if raw, exists := tbl["envs"]; exists {
		envs, ok := raw.([]any)
		if !ok {
			return fmt.Errorf("%s.announce envs must be a list of strings", name)
		}
		for _, env := range envs {
			if _, ok := env.(string); !ok {
				return fmt.Errorf("%s.announce envs must be a list of strings", name)
			}
		}
	}
	return nil
}

// SetAnnounce edits [name.announce]. A new webhook goes to the Keychain
// (service "agentflow") and the table points at it with webhook_ref; any
// inline webhook is removed. The result must have a channel and exactly one
// webhook source, or nothing is written.
func (s *Store) SetAnnounce(expect, name string, e AnnounceEdit) (*Result, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	if err := checkAnnounceEdit(e); err != nil {
		return nil, err
	}
	return s.Edit(expect, func(d *Doc) error {
		svc, err := d.service(name, false)
		if err != nil {
			return err
		}
		tbl, ok := svc["announce"].(map[string]any)
		if !ok {
			if _, exists := svc["announce"]; exists {
				return fmt.Errorf("%s.announce is not a table; fix the file by hand", name)
			}
			tbl = map[string]any{}
		}
		if e.Channel != nil {
			tbl["channel"] = *e.Channel
		}
		if e.Envs != nil {
			envs := make([]any, len(e.Envs))
			for i, env := range e.Envs {
				envs[i] = env
			}
			tbl["envs"] = envs
		}
		oldRef, _ := tbl["webhook_ref"].(string)
		if e.Webhook != "" {
			acct := announceAccount(name)
			delete(tbl, "webhook")
			tbl["webhook_ref"] = keychain.RefPrefix + acct
		}
		if err := validateAnnounceTable(name, tbl, e.Webhook); err != nil {
			return err
		}
		if e.Webhook != "" {
			acct := announceAccount(name)
			d.BeforeSave(func() error { return keychainSet(keychain.AgentflowService, acct, e.Webhook) })
			newRef := keychain.RefPrefix + acct
			if oldRef != "" && oldRef != newRef && !d.refersToKey("webhook_ref", oldRef) {
				if oldAcct, err := keychain.ParseRef(oldRef); err == nil {
					d.OnSaved(func() error { return keychainDelete(keychain.AgentflowService, oldAcct) })
				}
			}
		}
		svc["announce"] = tbl
		return nil
	})
}

// RemoveAnnounce deletes [name.announce] and then its Keychain webhook.
func (s *Store) RemoveAnnounce(expect, name string) (*Result, error) {
	if err := checkName(name); err != nil {
		return nil, err
	}
	return s.Edit(expect, func(d *Doc) error {
		svc, err := d.service(name, false)
		if err != nil {
			return err
		}
		tbl, ok := svc["announce"].(map[string]any)
		if !ok {
			return fmt.Errorf("no [%s.announce] table", name)
		}
		delete(svc, "announce")
		if len(svc) == 0 {
			delete(d.Tree, name)
		}
		if ref, _ := tbl["webhook_ref"].(string); ref != "" && !d.refersToKey("webhook_ref", ref) {
			if acct, err := keychain.ParseRef(ref); err == nil {
				d.OnSaved(func() error { return keychainDelete(keychain.AgentflowService, acct) })
			}
		}
		return nil
	})
}
