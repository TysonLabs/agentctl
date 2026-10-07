package ship

// Announce posts a verified deploy to the service's Slack channel through an
// incoming webhook. [name.announce] in services.toml names the webhook: in
// the Keychain (webhook_ref, service "agentflow", written by agentcfg) or,
// legacy, inline (webhook). This file decodes only that table and reads only
// the agentflow Keychain service, so agentflow never sees the /agent tokens. Every guard fails closed: no fresh `ship verify` proof,
// an env not opted in, or a malformed config means nothing is posted.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/BurntSushi/toml"

	"github.com/TysonLabs/agentctl/internal/keychain"
	"github.com/TysonLabs/agentctl/internal/slackhook"
)

// AnnounceStatus is the outcome of one announce. Every run ends in exactly one.
type AnnounceStatus string

const (
	AnnouncePosted     AnnounceStatus = "posted"            // Slack accepted the message
	AnnounceDryRun     AnnounceStatus = "dry_run"           // payload built and printed, nothing posted
	AnnounceRefused    AnnounceStatus = "refused"           // a guard failed: proof, env or config
	AnnounceAlready    AnnounceStatus = "already_announced" // this service.env and commit were posted before
	AnnounceSlackError AnnounceStatus = "slack_error"       // Slack or the network rejected the post
)

// AnnounceConfig is one [name.announce] table.
type AnnounceConfig struct {
	Webhook string   // write credential: never printed
	Channel string   // label for output only; the webhook decides where it posts
	Envs    []string // envs that announce (default ["prod"])
}

// Poster sends a payload to a webhook.
type Poster func(ctx context.Context, webhook string, payload []byte) error

// AnnounceOptions configures one announce.
type AnnounceOptions struct {
	Service    string        // service.env, as for ship verify
	ConfigPath string        // services.toml
	Proof      Result        // the ship verify result for this deploy
	Title      string        // one line, plain text
	Body       string        // plain language: what changed, how to test it
	PRURL      string        // optional https://github.com/<owner>/<repo>/pull/<n>
	MaxAge     time.Duration // how old the proof may be
	Force      bool          // post even if this commit was announced before
	DryRun     bool          // build and return the payload, post nothing
	StatePath  string        // announced-commit record
	Post       Poster
	Now        func() time.Time
}

// AnnounceResult is the machine-readable summary.
type AnnounceResult struct {
	Status   AnnounceStatus  `json:"status"`
	Service  string          `json:"service"`
	SHA      string          `json:"sha,omitempty"`
	Channel  string          `json:"channel,omitempty"`
	PostedAt string          `json:"posted_at,omitempty"` // this post, or the earlier one for already_announced
	Payload  json.RawMessage `json:"payload,omitempty"`   // dry_run only
	Error    string          `json:"error,omitempty"`
}

const (
	maxTitleRunes   = 150  // Slack header block limit
	maxBodyRunes    = 2900 // under the 3000-char section limit, room for the marker
	maxContextRunes = 2900 // under the 3000-char text-object limit
)

var prURLRe = regexp.MustCompile(`^https://github\.com/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+/pull/([0-9]+)$`)

// Announce checks every guard, then posts once per service.env and commit.
func Announce(ctx context.Context, o AnnounceOptions) AnnounceResult {
	if o.Now == nil {
		o.Now = time.Now
	}
	res := AnnounceResult{Service: o.Service, SHA: o.Proof.Expected}
	refuse := func(format string, a ...any) AnnounceResult {
		res.Status, res.Error = AnnounceRefused, fmt.Sprintf(format, a...)
		return res
	}

	name, env, ok := splitService(o.Service)
	if !ok {
		return refuse("want service.env (e.g. recursivecx.prod), got %q", o.Service)
	}
	cfg, err := LoadAnnounceConfig(o.ConfigPath, name)
	if err != nil {
		return refuse("%v", err)
	}
	res.Channel = cfg.Channel
	if !slices.Contains(cfg.Envs, env) {
		return refuse("[%s.announce] envs does not include %q", name, env)
	}
	checkedAt, err := checkProof(o.Proof, o.Service, o.Now(), o.MaxAge)
	if err != nil {
		return refuse("no proof the deploy is live: %v", err)
	}
	if strings.TrimSpace(o.Title) == "" || strings.TrimSpace(o.Body) == "" {
		return refuse("a title and a non-empty body are required")
	}
	if o.PRURL != "" && !prURLRe.MatchString(o.PRURL) {
		return refuse("--pr-url must look like https://github.com/<owner>/<repo>/pull/<n>")
	}
	payload, err := buildPayload(name, env, o, checkedAt, cfg.Webhook)
	if err != nil {
		return refuse("building the message: %v", err)
	}
	key := stateKey(o.Service, o.Proof.Expected)

	if o.DryRun {
		res.Status, res.Payload = AnnounceDryRun, payload
		if st, err := readState(o.StatePath); err == nil {
			if prev, ok := announcedEntry(st, o.Service, o.Proof.Expected); ok && !o.Force {
				res.PostedAt = prev.PostedAt
				res.Error = "already announced: a real run would exit already_announced (use --force to post again)"
			}
		}
		return res
	}

	unlock, err := lockState(o.StatePath)
	if err != nil {
		return refuse("locking %s: %v", o.StatePath, err)
	}
	defer unlock()
	st, err := readState(o.StatePath)
	if err != nil {
		return refuse("reading %s: %v", o.StatePath, err)
	}
	if prev, ok := announcedEntry(st, o.Service, o.Proof.Expected); ok && !o.Force {
		res.Status, res.PostedAt = AnnounceAlready, prev.PostedAt
		res.Error = "this commit was already announced for " + o.Service + " (use --force to post again)"
		return res
	}
	// Persist a reservation before the irreversible Slack call. If state
	// cannot be made durable, refusing here preserves the at-most-once
	// contract. A confirmed failed post rolls the reservation back.
	previous, hadPrevious := st[key]
	res.PostedAt = o.Now().UTC().Format(time.RFC3339)
	st[key] = stateEntry{PostedAt: res.PostedAt, Channel: cfg.Channel}
	if err := writeState(o.StatePath, st); err != nil {
		res.PostedAt = ""
		return refuse("recording announcement state in %s: %v", o.StatePath, err)
	}
	if err := o.Post(ctx, cfg.Webhook, payload); err != nil {
		if hadPrevious {
			st[key] = previous
		} else {
			delete(st, key)
		}
		rollbackErr := writeState(o.StatePath, st)
		res.Status, res.PostedAt = AnnounceSlackError, ""
		res.Error = redactSecret(err.Error(), cfg.Webhook)
		if rollbackErr != nil {
			res.Error += fmt.Sprintf("; clearing the state reservation failed: %v", rollbackErr)
		}
		return res
	}
	res.Status = AnnouncePosted
	return res
}

func splitService(s string) (name, env string, ok bool) {
	i := strings.LastIndex(s, ".")
	if i <= 0 || i == len(s)-1 {
		return "", "", false
	}
	return s[:i], s[i+1:], true
}

// LoadAnnounceConfig reads [name.announce] from services.toml. Only that table
// is decoded; the rest of the file, tokens included, stays undecoded TOML.
func LoadAnnounceConfig(path, name string) (AnnounceConfig, error) {
	var raw map[string]map[string]toml.Primitive
	md, err := toml.DecodeFile(path, &raw)
	if err != nil {
		var pe toml.ParseError
		if errors.As(err, &pe) {
			// Never echo the lexeme: it may be a token or the webhook.
			return AnnounceConfig{}, fmt.Errorf("parsing %s: TOML syntax error at line %d (file content redacted)", path, pe.Position.Line)
		}
		if errors.Is(err, os.ErrNotExist) {
			return AnnounceConfig{}, fmt.Errorf("config file not found: %s", path)
		}
		return AnnounceConfig{}, fmt.Errorf("parsing %s: TOML decode error (file content redacted)", path)
	}
	prim, ok := raw[name]["announce"]
	if !ok {
		return AnnounceConfig{}, fmt.Errorf("no [%s.announce] table in %s", name, path)
	}
	var t struct {
		Webhook    string   `toml:"webhook"`
		WebhookRef string   `toml:"webhook_ref"`
		Channel    string   `toml:"channel"`
		Envs       []string `toml:"envs"`
	}
	if err := md.PrimitiveDecode(prim, &t); err != nil {
		return AnnounceConfig{}, fmt.Errorf("[%s.announce] in %s: webhook, webhook_ref and channel must be strings, envs a list of strings", name, path)
	}
	for _, k := range md.Undecoded() {
		if len(k) >= 3 && k[0] == name && k[1] == "announce" {
			// A TOML key is user-controlled too; do not echo one that happens
			// to contain the webhook credential or another secret.
			return AnnounceConfig{}, fmt.Errorf("[%s.announce] in %s: unknown key (name redacted)", name, path)
		}
	}
	// Presence, not value, decides the source: an empty webhook_ref beside a
	// webhook is ambiguous, never a fallback.
	hasHook := md.IsDefined(name, "announce", "webhook")
	hasRef := md.IsDefined(name, "announce", "webhook_ref")
	switch {
	case hasHook && hasRef:
		return AnnounceConfig{}, fmt.Errorf("[%s.announce] in %s: set webhook or webhook_ref, not both", name, path)
	case hasRef:
		acct, err := keychain.ParseRef(t.WebhookRef)
		if err != nil {
			return AnnounceConfig{}, fmt.Errorf("[%s.announce] in %s: webhook_ref: %v", name, path, err)
		}
		hook, err := keychain.GetFrom(keychain.AgentflowService, acct)
		if errors.Is(err, keychain.ErrNotFound) {
			return AnnounceConfig{}, fmt.Errorf("[%s.announce]: no keychain item %s/%s — run: agentcfg announce %s --webhook", name, keychain.AgentflowService, acct, name)
		}
		if err != nil {
			return AnnounceConfig{}, fmt.Errorf("[%s.announce]: reading the webhook from the Keychain: %v", name, err)
		}
		t.Webhook = hook
	}
	if !slackhook.Valid(t.Webhook) {
		return AnnounceConfig{}, fmt.Errorf("[%s.announce] in %s: webhook must be a Slack incoming webhook (https://hooks.slack.com/services/...)", name, path)
	}
	if strings.TrimSpace(t.Channel) == "" {
		return AnnounceConfig{}, fmt.Errorf("[%s.announce] in %s: channel is required (a label such as \"#releases\")", name, path)
	}
	if cleanText(t.Channel, t.Webhook) != t.Channel {
		return AnnounceConfig{}, fmt.Errorf("[%s.announce] in %s: channel contains unsafe content (content redacted)", name, path)
	}
	if t.Envs == nil {
		t.Envs = []string{"prod"}
	}
	return AnnounceConfig{Webhook: t.Webhook, Channel: t.Channel, Envs: t.Envs}, nil
}

// checkProof accepts only a fresh, successful ship verify of this service.
func checkProof(p Result, service string, now time.Time, maxAge time.Duration) (time.Time, error) {
	if p.Status != StatusDeployed {
		return time.Time{}, fmt.Errorf("verify status is %q, want %q", p.Status, StatusDeployed)
	}
	if p.Service != service {
		return time.Time{}, fmt.Errorf("verify was for %q, not %q", p.Service, service)
	}
	if !IsHexSHA(p.Expected) || !IsHexSHA(p.Running) || (p.Match != "exact" && p.Match != "contains") {
		return time.Time{}, errors.New("verify result has no expected commit, running commit or match")
	}
	if p.Match == "exact" && !Matches(p.Expected, p.Running) {
		return time.Time{}, errors.New("verify result's running commit does not match the expected commit")
	}
	at, err := time.Parse(time.RFC3339, p.CheckedAt)
	if err != nil {
		return time.Time{}, errors.New("verify result has no checked_at (rerun ship verify with this agentflow)")
	}
	if age := now.Sub(at); age > maxAge || age < -time.Minute {
		return time.Time{}, fmt.Errorf("verify ran at %s, more than %s ago (rerun ship verify)", p.CheckedAt, maxAge)
	}
	return at, nil
}

func buildPayload(name, env string, o AnnounceOptions, checkedAt time.Time, webhook string) ([]byte, error) {
	// Slack parses <!channel> and links in the notification text too, so the
	// title is escaped in mrkdwn, but the header itself is plain_text.
	title := truncateRunes(strings.Join(strings.Fields(cleanText(o.Title, webhook)), " "), maxTitleRunes)
	titleMrkdwn := escapeMrkdwn(title)
	// Escaping expands characters such as '<' to '&lt;'. Apply Slack's size
	// budget after that expansion so the serialized text still fits.
	body := truncateRunes(escapeMrkdwn(strings.TrimSpace(cleanText(o.Body, webhook))), maxBodyRunes)
	sha := o.Proof.Expected[:min(8, len(o.Proof.Expected))]
	cleanName := escapeMrkdwn(cleanText(name, webhook))
	cleanEnv := escapeMrkdwn(cleanText(env, webhook))

	ctxLine := fmt.Sprintf("*%s* deployed to *%s* · `%s`", cleanName, cleanEnv, sha)
	if o.Proof.Match == "contains" {
		ctxLine += fmt.Sprintf(" (running `%s`, which includes it)", o.Proof.Running[:min(8, len(o.Proof.Running))])
	}
	if m := prURLRe.FindStringSubmatch(o.PRURL); m != nil {
		ctxLine += fmt.Sprintf(" · <%s|PR #%s>", o.PRURL, m[1])
	}
	ctxLine += " · verified " + checkedAt.UTC().Format("2006-01-02 15:04 UTC")
	if utf8.RuneCountInString(ctxLine) > maxContextRunes {
		return nil, errors.New("service, environment and PR context exceeds Slack's size limit")
	}

	msg := map[string]any{
		"text": fmt.Sprintf("%s %s: %s", cleanName, cleanEnv, titleMrkdwn), // notification fallback
		"blocks": []any{
			map[string]any{"type": "header", "text": map[string]any{"type": "plain_text", "text": title}},
			map[string]any{"type": "context", "elements": []any{map[string]any{"type": "mrkdwn", "text": ctxLine}}},
			map[string]any{"type": "section", "text": map[string]any{"type": "mrkdwn", "text": body}},
		},
		"unfurl_links": false,
		"unfurl_media": false,
	}
	return json.Marshal(msg)
}

// cleanText removes secrets and characters that can hide or reorder text.
// Secrets go first, before any escaping changes their spelling.
func cleanText(s, webhook string) string {
	return slackhook.CleanText(s, webhook)
}

func redactSecret(s, secret string) string {
	return slackhook.RedactSecret(s, secret)
}

// escapeMrkdwn escapes Slack's control characters, so a body can't ping
// <!channel> or forge a link; Slack still links bare URLs itself.
func escapeMrkdwn(s string) string {
	return strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;").Replace(s)
}

func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	r := []rune(s)
	return string(r[:n-1]) + "…"
}

// SlackPost posts to an incoming webhook. It never follows a redirect (the
// webhook is a credential) and never puts the URL in an error.
func SlackPost(timeout time.Duration) Poster {
	client := &http.Client{
		Timeout:       timeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	return func(ctx context.Context, webhook string, payload []byte) error {
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, webhook, bytes.NewReader(payload))
		if err != nil {
			return errors.New("slack: invalid webhook URL")
		}
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			var ue *url.Error
			if errors.As(err, &ue) {
				err = ue.Err
			}
			return fmt.Errorf("slack: %v", err)
		}
		defer resp.Body.Close()
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 1024))
		text := strings.TrimSpace(string(b))
		if resp.StatusCode != http.StatusOK || text != "ok" {
			return fmt.Errorf("slack: HTTP %d: %s", resp.StatusCode, cleanText(text, webhook))
		}
		return nil
	}
}

// DefaultStatePath is $XDG_STATE_HOME/agentflow/announce.json, falling back
// to ~/.local/state.
func DefaultStatePath() string {
	dir := os.Getenv("XDG_STATE_HOME")
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			home = "."
		}
		dir = filepath.Join(home, ".local", "state")
	}
	return filepath.Join(dir, "agentflow", "announce.json")
}

type stateEntry struct {
	PostedAt string `json:"posted_at"`
	Channel  string `json:"channel"`
}

func stateKey(service, sha string) string { return service + "@" + strings.ToLower(sha) }

// announcedEntry uses verify's commit identity rules rather than literal
// strings. The same commit may be reported as a short SHA in one run and a
// full SHA in another; those must share one once-only record.
func announcedEntry(st map[string]stateEntry, service, sha string) (stateEntry, bool) {
	prefix := service + "@"
	for key, entry := range st {
		if previous, ok := strings.CutPrefix(key, prefix); ok && Matches(previous, sha) {
			return entry, true
		}
	}
	return stateEntry{}, false
}

// lockState serialises announces across processes, so two sessions shipping
// the same commit can't both post.
func lockState(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { _ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func readState(path string) (map[string]stateEntry, error) {
	st := map[string]stateEntry{}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("corrupt state file (fix or remove it): %v", err)
	}
	if st == nil {
		return nil, errors.New("corrupt state file (fix or remove it): want a JSON object")
	}
	return st, nil
}

func writeState(path string, st map[string]stateEntry) error {
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".announce-*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return err
	}
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
