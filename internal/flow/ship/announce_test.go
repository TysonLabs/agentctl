package ship

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const (
	testHook  = "https://hooks.slack.com/services/T0FAKE/B0FAKE/fakeSecretPart123"
	testSHA   = "f316cb0f9211b2261c554e55074aec2e5d3dff70"
	testToken = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef" // made up
)

var testNow = time.Date(2026, 10, 7, 15, 0, 0, 0, time.UTC)

const testConfig = `
[rcx.prod]
base_url = "https://rcx.example.com"
token    = "` + testToken + `"

[rcx.dev]
base_url = "https://dev.rcx.example.com"
token    = "REPLACE_ME"

[rcx.announce]
webhook = "` + testHook + `"
channel = "#rcx-releases"
`

func writeFile(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// recorder is a Poster that records what it was asked to post.
type recorder struct {
	mu       sync.Mutex
	payloads [][]byte
	hooks    []string
	err      error
	delay    time.Duration
}

func (r *recorder) post(_ context.Context, webhook string, payload []byte) error {
	time.Sleep(r.delay)
	r.mu.Lock()
	defer r.mu.Unlock()
	r.payloads = append(r.payloads, payload)
	r.hooks = append(r.hooks, webhook)
	return r.err
}

func (r *recorder) count() int { r.mu.Lock(); defer r.mu.Unlock(); return len(r.payloads) }

func proof() Result {
	return Result{Status: StatusDeployed, Service: "rcx.prod", Expected: testSHA, Running: testSHA,
		Match: "exact", CheckedAt: testNow.Add(-5 * time.Minute).Format(time.RFC3339)}
}

func baseOpts(t *testing.T, rec *recorder) AnnounceOptions {
	t.Helper()
	return AnnounceOptions{
		Service:    "rcx.prod",
		ConfigPath: writeFile(t, "services.toml", testConfig),
		Proof:      proof(),
		Title:      "Ring groups: paused agents now follow the no-answer action",
		Body:       "*What changed:* a paused agent no longer rings.\n*How to test:* pause, then call the group.",
		PRURL:      "https://github.com/acme/rcx/pull/1094",
		MaxAge:     time.Hour,
		StatePath:  filepath.Join(t.TempDir(), "state", "announce.json"),
		Post:       rec.post,
		Now:        func() time.Time { return testNow },
	}
}

func TestAnnouncePostsOnceThenRefusesARepeat(t *testing.T) {
	rec := &recorder{}
	o := baseOpts(t, rec)
	res := Announce(context.Background(), o)
	if res.Status != AnnouncePosted || res.Channel != "#rcx-releases" || res.SHA != testSHA || res.Error != "" {
		t.Fatalf("got %+v", res)
	}
	if rec.count() != 1 || rec.hooks[0] != testHook {
		t.Fatalf("posted %d times to %v", rec.count(), rec.hooks)
	}
	var msg struct {
		Text   string `json:"text"`
		Blocks []struct {
			Type     string `json:"type"`
			Text     struct{ Text string }
			Elements []struct{ Text string }
		} `json:"blocks"`
	}
	if err := json.Unmarshal(rec.payloads[0], &msg); err != nil || len(msg.Blocks) != 3 {
		t.Fatalf("payload %s: %v", rec.payloads[0], err)
	}
	if msg.Blocks[0].Text.Text != o.Title || !strings.HasPrefix(msg.Text, "rcx prod: ") {
		t.Errorf("header/fallback wrong: %s", rec.payloads[0])
	}
	ctxLine := msg.Blocks[1].Elements[0].Text
	for _, want := range []string{"*rcx* deployed to *prod*", "`f316cb0f`", "<https://github.com/acme/rcx/pull/1094|PR #1094>", "verified 2026-10-07 14:55 UTC"} {
		if !strings.Contains(ctxLine, want) {
			t.Errorf("context line %q missing %q", ctxLine, want)
		}
	}
	if !strings.Contains(msg.Blocks[2].Text.Text, "*How to test:*") {
		t.Errorf("body lost: %q", msg.Blocks[2].Text.Text)
	}

	again := Announce(context.Background(), o)
	if again.Status != AnnounceAlready || again.PostedAt != res.PostedAt || rec.count() != 1 {
		t.Fatalf("repeat: got %+v after %d posts", again, rec.count())
	}
	o.Force = true
	if forced := Announce(context.Background(), o); forced.Status != AnnouncePosted || rec.count() != 2 {
		t.Fatalf("--force: got %+v after %d posts", forced, rec.count())
	}
}

func TestAnnounceRefusals(t *testing.T) {
	cases := []struct {
		name    string
		edit    func(*AnnounceOptions)
		wantErr string
	}{
		{"verify timed out", func(o *AnnounceOptions) { o.Proof.Status = StatusTimeout }, `want "deployed"`},
		{"verify for another service", func(o *AnnounceOptions) { o.Proof.Service = "rcx.dev" }, `not "rcx.prod"`},
		{"stale verify", func(o *AnnounceOptions) { o.Proof.CheckedAt = testNow.Add(-2 * time.Hour).Format(time.RFC3339) }, "more than 1h0m0s ago"},
		{"future verify", func(o *AnnounceOptions) { o.Proof.CheckedAt = testNow.Add(time.Hour).Format(time.RFC3339) }, "more than"},
		{"verify from an old agentflow", func(o *AnnounceOptions) { o.Proof.CheckedAt = "" }, "no checked_at"},
		{"verify without a match", func(o *AnnounceOptions) { o.Proof.Match = "" }, "no expected commit"},
		{"exact match with another commit", func(o *AnnounceOptions) { o.Proof.Running = "d63a514" }, "does not match"},
		{"contains match without a running commit", func(o *AnnounceOptions) { o.Proof.Match = "contains"; o.Proof.Running = "not-a-sha <!channel>" }, "running commit"},
		{"env not enabled", func(o *AnnounceOptions) { o.Service = "rcx.dev"; o.Proof.Service = "rcx.dev" }, `does not include "dev"`},
		{"no announce table", func(o *AnnounceOptions) { o.Service = "other.prod"; o.Proof.Service = "other.prod" }, "no [other.announce] table"},
		{"bad service", func(o *AnnounceOptions) { o.Service = "rcx" }, "want service.env"},
		{"empty body", func(o *AnnounceOptions) { o.Body = " \n " }, "non-empty body"},
		{"bad PR URL", func(o *AnnounceOptions) { o.PRURL = "https://evil.example.com/pull/1" }, "--pr-url"},
		{"missing config", func(o *AnnounceOptions) { o.ConfigPath = filepath.Join(t.TempDir(), "nope.toml") }, "not found"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			rec := &recorder{}
			o := baseOpts(t, rec)
			c.edit(&o)
			res := Announce(context.Background(), o)
			if res.Status != AnnounceRefused || !strings.Contains(res.Error, c.wantErr) {
				t.Fatalf("got status=%s error=%q, want refused containing %q", res.Status, res.Error, c.wantErr)
			}
			if rec.count() != 0 {
				t.Errorf("a refused announce posted")
			}
			if _, err := os.Stat(o.StatePath); err == nil {
				t.Errorf("a refused announce wrote state")
			}
		})
	}
}

func TestAnnounceConfigErrorsNeverPrintTheWebhook(t *testing.T) {
	cases := map[string]string{
		"http webhook":       `webhook = "http://hooks.slack.com/services/T/B/fakeSecretPart123"` + "\nchannel = \"#x\"",
		"other host":         `webhook = "https://evil.example.com/services/fakeSecretPart123"` + "\nchannel = \"#x\"",
		"no channel":         `webhook = "` + testHook + `"`,
		"unknown key":        `webhook = "` + testHook + `"` + "\nchannel = \"#x\"\nwebook2 = \"x\"",
		"wrong type":         `webhook = "` + testHook + `"` + "\nchannel = 7",
		"syntax error":       `webhook = "` + testHook,
		"short path segment": `webhook = "https://hooks.slack.com/services/T/B/fakeSecretPart123"` + "\nchannel = \"#x\"",
		"query string":       `webhook = "` + testHook + `?x=fakeSecretPart123"` + "\nchannel = \"#x\"",
		"userinfo smuggle":   `webhook = "https://fakeSecretPart123@hooks.slack.com/services/T/B/x"` + "\nchannel = \"#x\"",
		"secret key name":    `webhook = "` + testHook + `"` + "\nchannel = \"#x\"\n\"fakeSecretPart123\" = \"x\"",
		"webhook in channel": `webhook = "` + testHook + "\"\nchannel = \"" + testHook + "\"",
		"nested unknown key": `webhook = "` + testHook + `"` + "\nchannel = \"#x\"\n[rcx.announce.extra]\nvalue = \"x\"",
	}
	for name, table := range cases {
		t.Run(name, func(t *testing.T) {
			p := writeFile(t, "services.toml", "[rcx.prod]\nbase_url = \"https://x.example.com\"\ntoken = \""+testToken+"\"\n\n[rcx.announce]\n"+table+"\n")
			_, err := LoadAnnounceConfig(p, "rcx")
			if err == nil {
				t.Fatal("want an error")
			}
			if strings.Contains(err.Error(), "fakeSecretPart123") || strings.Contains(err.Error(), testToken) {
				t.Errorf("error leaks a secret: %v", err)
			}
		})
	}
}

func TestAnnounceEnvRefusalDoesNotLeakConfigValues(t *testing.T) {
	rec := &recorder{}
	o := baseOpts(t, rec)
	o.Service, o.Proof.Service = "rcx.dev", "rcx.dev"
	o.ConfigPath = writeFile(t, "services.toml", strings.Replace(testConfig, "[rcx.announce]", "[rcx.announce]\nenvs = [\"prod\", \""+testToken+"\"]", 1))
	res := Announce(context.Background(), o)
	if res.Status != AnnounceRefused || strings.Contains(res.Error, testToken) {
		t.Fatalf("config value leaked in refusal: %+v", res)
	}
}

func TestAnnounceConfigDefaultsEnvsToProd(t *testing.T) {
	cfg, err := LoadAnnounceConfig(writeFile(t, "services.toml", testConfig), "rcx")
	if err != nil || len(cfg.Envs) != 1 || cfg.Envs[0] != "prod" || cfg.Channel != "#rcx-releases" {
		t.Fatalf("cfg %+v, err %v", cfg, err)
	}
}

func TestAnnounceScrubsTheBody(t *testing.T) {
	rec := &recorder{}
	o := baseOpts(t, rec)
	o.Title = "Fix\u202e for <!channel>"
	o.Body = strings.Join([]string{
		"webhook " + testHook,
		"path only fakeSecretPart123",
		"hex token " + testToken,
		"base64 key Zm9vYmFyQmF6+UXV4MTIzNDU2Nzg5MGFiY2RlZg==",
		"slack token xoxb-1234-5678-abcdefghijkl",
		"ping <!channel> and <@U123> and <https://evil.example.com|docs>",
		"commit " + testSHA + " in crates/rcx-console-queue/src/audio_fetch.rs",
		"bidi \u202ereversed\u202c and zero\u200bwidth/non\u200cjoiner/\u200djoiner/word\u2060joiner/soft\u00adhyphen",
	}, "\n")
	if res := Announce(context.Background(), o); res.Status != AnnouncePosted {
		t.Fatalf("got %+v", res)
	}
	p := string(rec.payloads[0])
	var decoded map[string]any
	_ = json.Unmarshal(rec.payloads[0], &decoded)
	var buf strings.Builder // compare on decoded text, not JSON's \u0026-style escapes
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(decoded)
	text := buf.String()
	// The title's header block is plain_text, where <!channel> is inert and
	// should remain human-readable. Mrkdwn copies are checked escaped below.
	for _, leaked := range []string{"fakeSecretPart123", testToken, "Zm9vYmFyQmF6", "xoxb-", "<@U123>", "<https://evil", "\u202e", "\u200b", "\u200c", "\u200d", "\u2060", "\u00ad"} {
		if strings.Contains(text, leaked) || strings.Contains(p, leaked) {
			t.Errorf("payload still contains %q:\n%s", leaked, p)
		}
	}
	for _, kept := range []string{testSHA, "crates/rcx-console-queue/src/audio_fetch.rs", "&lt;!channel&gt;", "reversed"} {
		if !strings.Contains(text, kept) && !strings.Contains(p, kept) {
			t.Errorf("payload lost %q:\n%s", kept, p)
		}
	}
}

func TestAnnounceTruncatesToSlackLimits(t *testing.T) {
	rec := &recorder{}
	o := baseOpts(t, rec)
	o.Title = strings.Repeat("t", 400)
	o.Body = strings.Repeat("x", 5000) // not hex, or the token scrubber takes it
	if res := Announce(context.Background(), o); res.Status != AnnouncePosted {
		t.Fatalf("got %+v", res)
	}
	var msg struct {
		Blocks []struct{ Text struct{ Text string } } `json:"blocks"`
	}
	_ = json.Unmarshal(rec.payloads[0], &msg)
	if n := len([]rune(msg.Blocks[0].Text.Text)); n != maxTitleRunes {
		t.Errorf("title is %d runes, want %d", n, maxTitleRunes)
	}
	if n := len([]rune(msg.Blocks[2].Text.Text)); n != maxBodyRunes || !strings.HasSuffix(msg.Blocks[2].Text.Text, "…") {
		t.Errorf("body is %d runes, want %d ending in …", n, maxBodyRunes)
	}
}

func TestAnnounceUsesPlainTextForHeaderAndLimitsEscapedBody(t *testing.T) {
	rec := &recorder{}
	o := baseOpts(t, rec)
	o.Title = "R&D <release>"
	o.Body = strings.Repeat("<", maxBodyRunes)
	if res := Announce(context.Background(), o); res.Status != AnnouncePosted {
		t.Fatalf("got %+v", res)
	}
	var msg struct {
		Text   string
		Blocks []struct{ Text struct{ Text string } } `json:"blocks"`
	}
	if err := json.Unmarshal(rec.payloads[0], &msg); err != nil {
		t.Fatal(err)
	}
	if got := msg.Blocks[0].Text.Text; got != o.Title {
		t.Errorf("plain-text header = %q, want %q", got, o.Title)
	}
	if strings.Contains(msg.Text, "<release>") || !strings.Contains(msg.Text, "&lt;release&gt;") {
		t.Errorf("mrkdwn fallback did not escape the title: %q", msg.Text)
	}
	if got := len([]rune(msg.Blocks[2].Text.Text)); got > maxBodyRunes {
		t.Errorf("escaped body is %d runes, Slack limit budget is %d", got, maxBodyRunes)
	}
}

func TestBuildPayloadCleansContextIdentifiers(t *testing.T) {
	o := AnnounceOptions{Proof: proof(), Title: "title", Body: "body"}
	payload, err := buildPayload("rcx\u202e", "prod\u200b", o, testNow, testHook)
	if err != nil {
		t.Fatal(err)
	}
	if strings.ContainsAny(string(payload), "\u202e\u200b") {
		t.Fatalf("context identifiers retain invisible/bidi characters: %s", payload)
	}
}

func TestBuildPayloadRefusesOversizeContext(t *testing.T) {
	o := AnnounceOptions{Proof: proof(), Title: "title", Body: "body"}
	if _, err := buildPayload(strings.Repeat("&", 3000), "prod", o, testNow, testHook); err == nil {
		t.Fatal("want an error for a context block over Slack's limit")
	}
}

func TestAnnounceSlackErrorIsRedactedAndNotRecorded(t *testing.T) {
	rec := &recorder{err: errors.New(`Post "` + testHook + `": T0FAKE B0FAKE fakeSecretPart123 refused`)}
	o := baseOpts(t, rec)
	res := Announce(context.Background(), o)
	if res.Status != AnnounceSlackError {
		t.Fatalf("got %+v", res)
	}
	for _, secretPart := range []string{"T0FAKE", "B0FAKE", "fakeSecretPart123"} {
		if strings.Contains(res.Error, secretPart) {
			t.Fatalf("Slack error leaked webhook segment %q: %+v", secretPart, res)
		}
	}
	rec.err = nil
	if retry := Announce(context.Background(), o); retry.Status != AnnouncePosted {
		t.Fatalf("a failed post must not count as announced: %+v", retry)
	}
}

func TestAnnounceDryRunPostsNothing(t *testing.T) {
	rec := &recorder{}
	o := baseOpts(t, rec)
	o.DryRun = true
	res := Announce(context.Background(), o)
	if res.Status != AnnounceDryRun || len(res.Payload) == 0 || rec.count() != 0 {
		t.Fatalf("got %+v after %d posts", res, rec.count())
	}
	if strings.Contains(string(res.Payload), "fakeSecretPart123") {
		t.Errorf("dry-run payload leaks the webhook")
	}
	if _, err := os.Stat(o.StatePath); err == nil {
		t.Errorf("a dry run wrote state")
	}
}

func TestAnnounceConcurrentRunsPostOnce(t *testing.T) {
	rec := &recorder{delay: 100 * time.Millisecond}
	o := baseOpts(t, rec)
	var posted atomic.Int32
	var wg sync.WaitGroup
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if Announce(context.Background(), o).Status == AnnouncePosted {
				posted.Add(1)
			}
		}()
	}
	wg.Wait()
	if posted.Load() != 1 || rec.count() != 1 {
		t.Fatalf("%d runs posted, %d posts; want exactly 1", posted.Load(), rec.count())
	}
}

func TestAnnounceTreatsEquivalentShortAndFullSHAsAsOneCommit(t *testing.T) {
	rec := &recorder{}
	o := baseOpts(t, rec)
	o.Proof.Expected = testSHA[:8]
	if first := Announce(context.Background(), o); first.Status != AnnouncePosted {
		t.Fatalf("short proof: %+v", first)
	}
	o.Proof.Expected = testSHA
	if second := Announce(context.Background(), o); second.Status != AnnounceAlready || rec.count() != 1 {
		t.Fatalf("full proof after short proof: %+v after %d posts", second, rec.count())
	}
}

func TestAnnounceRefusesNullStateBeforePosting(t *testing.T) {
	rec := &recorder{}
	o := baseOpts(t, rec)
	if err := os.MkdirAll(filepath.Dir(o.StatePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.StatePath, []byte("null\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	res := Announce(context.Background(), o)
	if res.Status != AnnounceRefused || rec.count() != 0 {
		t.Fatalf("got %+v after %d posts", res, rec.count())
	}
}

func TestAnnounceStateFileIsPrivateDespiteAStaleTempFile(t *testing.T) {
	rec := &recorder{}
	o := baseOpts(t, rec)
	if err := os.MkdirAll(filepath.Dir(o.StatePath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.StatePath+".tmp", []byte("stale\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(o.StatePath+".tmp", 0o644); err != nil {
		t.Fatal(err)
	}
	if res := Announce(context.Background(), o); res.Status != AnnouncePosted {
		t.Fatalf("got %+v", res)
	}
	info, err := os.Stat(o.StatePath)
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Fatalf("state mode = %o, want 600", got)
	}
}

func TestAnnounceRefusesBeforePostingWhenStateCannotBeRecorded(t *testing.T) {
	rec := &recorder{}
	o := baseOpts(t, rec)
	dir := filepath.Dir(o.StatePath)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.StatePath, []byte("{}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(o.StatePath+".lock", nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	res := Announce(context.Background(), o)
	if res.Status != AnnounceRefused || rec.count() != 0 {
		t.Fatalf("got %+v after %d posts", res, rec.count())
	}
}

func TestSlackPost(t *testing.T) {
	var hits atomic.Int32
	mux := http.NewServeMux()
	mux.HandleFunc("/ok", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.Header.Get("Content-Type") != "application/json" {
			http.Error(w, "bad request", 400)
			return
		}
		_, _ = w.Write([]byte("ok"))
	})
	mux.HandleFunc("/bad", func(w http.ResponseWriter, _ *http.Request) { http.Error(w, "invalid_payload", 400) })
	mux.HandleFunc("/redirect", func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, "/elsewhere", http.StatusFound) })
	mux.HandleFunc("/elsewhere", func(w http.ResponseWriter, _ *http.Request) { hits.Add(1); _, _ = w.Write([]byte("ok")) })
	srv := httptest.NewServer(mux)
	defer srv.Close()

	post := SlackPost(5 * time.Second)
	if err := post(context.Background(), srv.URL+"/ok", []byte(`{}`)); err != nil {
		t.Errorf("ok: %v", err)
	}
	if err := post(context.Background(), srv.URL+"/bad", []byte(`{}`)); err == nil || !strings.Contains(err.Error(), "HTTP 400: invalid_payload") {
		t.Errorf("bad: %v", err)
	}
	if err := post(context.Background(), srv.URL+"/redirect", []byte(`{}`)); err == nil || hits.Load() != 0 {
		t.Errorf("redirect followed (err %v, hits %d)", err, hits.Load())
	}
	dead := srv.URL + "/ok"
	srv.Close()
	if err := post(context.Background(), dead, []byte(`{}`)); err == nil || strings.Contains(err.Error(), dead) {
		t.Errorf("transport error must not name the URL: %v", err)
	}
}
