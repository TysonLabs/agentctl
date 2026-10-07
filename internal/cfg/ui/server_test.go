package ui

import (
	"encoding/json"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/TysonLabs/agentctl/internal/cfg"
	"github.com/TysonLabs/agentctl/internal/keychain/keychaintest"
)

const plainTok = "plain_secret_token_9"

func newServer(t *testing.T) (*server, *httptest.Server) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "services.toml")
	content := "[pay.prod]\nbase_url = \"https://pay.example.com\"\ntoken = \"" + plainTok + "\"\n"
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	s := &server{store: &cfg.Store{Path: p}, version: "test", key: strings.Repeat("ab", 32), last: time.Now(), quit: make(chan struct{})}
	ts := httptest.NewServer(s.routes())
	t.Cleanup(ts.Close)
	s.host = strings.TrimPrefix(ts.URL, "http://")
	return s, ts
}

type call struct {
	method, path, body string
	key, ctype, origin string
	host               string
}

func do(t *testing.T, s *server, ts *httptest.Server, c call) (int, string) {
	t.Helper()
	if c.method == "" {
		c.method = http.MethodPost
	}
	req, err := http.NewRequest(c.method, ts.URL+c.path, strings.NewReader(c.body))
	if err != nil {
		t.Fatal(err)
	}
	if c.key != "-" {
		k := c.key
		if k == "" {
			k = s.key
		}
		req.Header.Set("X-Agentcfg-Key", k)
	}
	if c.ctype != "-" && c.method != http.MethodGet {
		ct := c.ctype
		if ct == "" {
			ct = "application/json"
		}
		req.Header.Set("Content-Type", ct)
	}
	if c.origin != "-" && c.method != http.MethodGet && c.method != http.MethodHead {
		o := c.origin
		if o == "" {
			o = "http://" + s.host
		}
		req.Header.Set("Origin", o)
	}
	if c.host != "" {
		req.Host = c.host
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestGuards(t *testing.T) {
	s, ts := newServer(t)
	cases := []struct {
		name string
		c    call
		want int
	}{
		{"no key", call{method: "GET", path: "/api/state", key: "-"}, 401},
		{"wrong key", call{method: "GET", path: "/api/state", key: strings.Repeat("cd", 32)}, 401},
		{"rebound host", call{method: "GET", path: "/api/state", host: "evil.example.com:" + strings.Split(s.host, ":")[1]}, 421},
		{"localhost alias", call{method: "GET", path: "/", host: "localhost:" + strings.Split(s.host, ":")[1]}, 421},
		{"missing origin", call{path: "/api/env", body: `{}`, origin: "-"}, 403},
		{"cross origin", call{path: "/api/env", body: `{}`, origin: "https://evil.example.com"}, 403},
		{"form post", call{path: "/api/env", body: `a=b`, ctype: "application/x-www-form-urlencoded"}, 415},
		{"no content type", call{path: "/api/env", body: `{}`, ctype: "-"}, 415},
		{"unknown field", call{path: "/api/env", body: `{"service":"pay.prod","base_url":"https://x","admin":true}`}, 400},
		{"trailing value", call{path: "/api/quit", body: `{} {}`}, 400},
		{"trailing brace", call{path: "/api/quit", body: `{}}`}, 400},
		{"trailing bracket", call{path: "/api/quit", body: `{}]`}, 400},
		{"get on write route", call{method: "GET", path: "/api/env"}, 405},
		{"ok", call{method: "GET", path: "/api/state"}, 200},
		{"page needs no key", call{method: "GET", path: "/", key: "-"}, 200},
	}
	for _, tc := range cases {
		if got, body := do(t, s, ts, tc.c); got != tc.want {
			t.Errorf("%s: status %d, want %d (%s)", tc.name, got, tc.want, body)
		}
	}
}

func TestWriteRequiresExactOriginWithoutNetwork(t *testing.T) {
	s := &server{
		store: &cfg.Store{Path: filepath.Join(t.TempDir(), "services.toml")},
		key:   strings.Repeat("ab", 32),
		host:  "127.0.0.1:43210",
		last:  time.Now(),
		quit:  make(chan struct{}),
	}
	for _, tc := range []struct {
		name, origin string
		want         int
	}{
		{"missing", "", http.StatusForbidden},
		{"cross origin", "https://evil.example.com", http.StatusForbidden},
		{"exact", "http://" + s.host, http.StatusBadRequest},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "http://"+s.host+"/api/env", strings.NewReader(`{}`))
			req.Host = s.host
			req.Header.Set("X-Agentcfg-Key", s.key)
			req.Header.Set("Content-Type", "application/json")
			if tc.origin != "" {
				req.Header.Set("Origin", tc.origin)
			}
			rr := httptest.NewRecorder()
			s.routes().ServeHTTP(rr, req)
			if rr.Code != tc.want {
				t.Fatalf("status %d, want %d: %s", rr.Code, tc.want, rr.Body.String())
			}
		})
	}
}

func TestDecodeRejectsEveryTrailingJSONValue(t *testing.T) {
	for _, body := range []string{`{} {}`, `{}}`, `{}]`, `{} null`} {
		t.Run(body, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body))
			var dst struct{}
			if err := decode(req, &dst); err == nil {
				t.Fatalf("decode accepted %q", body)
			}
		})
	}
	req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader("{} \n\t"))
	var dst struct{}
	if err := decode(req, &dst); err != nil {
		t.Fatalf("decode rejected trailing whitespace: %v", err)
	}
}

func TestSecurityHeaders(t *testing.T) {
	_, ts := newServer(t)
	resp, err := http.Get(ts.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	for h, want := range map[string]string{
		"X-Frame-Options": "DENY", "Cache-Control": "no-store", "Referrer-Policy": "no-referrer",
	} {
		if resp.Header.Get(h) != want {
			t.Errorf("%s = %q", h, resp.Header.Get(h))
		}
	}
	if csp := resp.Header.Get("Content-Security-Policy"); !strings.Contains(csp, "script-src 'self'") || strings.Contains(csp, "unsafe") {
		t.Errorf("CSP = %q", csp)
	}
}

func TestStateNeverHoldsTokenAndConflicts(t *testing.T) {
	s, ts := newServer(t)
	code, body := do(t, s, ts, call{method: "GET", path: "/api/state"})
	if code != 200 || strings.Contains(body, plainTok) {
		t.Fatalf("state %d leaks the token: %s", code, body)
	}
	var st struct {
		Version   string `json:"version"`
		Plaintext int    `json:"plaintext"`
	}
	if err := json.Unmarshal([]byte(body), &st); err != nil || st.Plaintext != 1 {
		t.Fatalf("state %s (%v)", body, err)
	}
	if code, _ := do(t, s, ts, call{path: "/api/env", body: `{"version":"stale","service":"pay.prod","base_url":"https://x.example.com"}`}); code != 409 {
		t.Fatalf("stale write: %d, want 409", code)
	}
	code, body = do(t, s, ts, call{path: "/api/env", body: `{"version":"` + st.Version + `","service":"pay.dev","base_url":"https://dev.example.com"}`})
	if code != 200 || !strings.Contains(body, "dev.example.com") || strings.Contains(body, plainTok) {
		t.Fatalf("add env: %d %s", code, body)
	}
	code, body = do(t, s, ts, call{path: "/api/env", body: `{"service":"pay.dev","base_url":"javascript:alert(1)"}`})
	if code != 400 {
		t.Fatalf("bad URL: %d %s", code, body)
	}
}

func TestTokenAndMigrateOverAPI(t *testing.T) {
	keychaintest.Temp(t)
	s, ts := newServer(t)
	code, body := do(t, s, ts, call{path: "/api/migrate", body: `{"version":""}`})
	if code != 200 || strings.Contains(body, plainTok) || !strings.Contains(body, `"plaintext":0`) {
		t.Fatalf("migrate: %d %s", code, body)
	}
	if b, _ := os.ReadFile(s.store.Path); strings.Contains(string(b), plainTok) {
		t.Fatal("token still in the file after migrate")
	}
	code, body = do(t, s, ts, call{path: "/api/token", body: `{"version":"","service":"pay.prod","token":"replacement_tok_1"}`})
	if code != 200 || strings.Contains(body, "replacement_tok_1") || !strings.Contains(body, `"source":"keychain"`) {
		t.Fatalf("token: %d %s", code, body)
	}
}

func TestTestEndpointUsesStoredToken(t *testing.T) {
	var gotAuth string
	svc := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		if r.URL.Path != "/agent/version" {
			http.NotFound(w, r)
			return
		}
		// A compromised or buggy service can reflect the bearer token, whole
		// or as a fragment, in a field that normally looks harmless.
		w.Write([]byte(`{"version":"` + plainTok + `"}`))
	}))
	defer svc.Close()
	s, ts := newServer(t)
	if _, err := s.store.SetBaseURL("", "pay.prod", svc.URL); err != nil {
		t.Fatal(err)
	}
	code, body := do(t, s, ts, call{path: "/api/test", body: `{"service":"pay.prod"}`})
	if code != 200 || !strings.Contains(body, `"ok":true`) {
		t.Fatalf("test: %d %s", code, body)
	}
	if gotAuth != "Bearer "+plainTok {
		t.Fatalf("service saw Authorization %q", gotAuth)
	}
	if strings.Contains(body, plainTok) {
		t.Fatal("test result leaks the token")
	}
	if !strings.Contains(body, "hidden") {
		t.Fatalf("a reflected token should be reported as hidden: %s", body)
	}
}

func TestBrowserCodeKeepsSecretsAndAddFormStable(t *testing.T) {
	js, err := fs.ReadFile(assets, "assets/app.js")
	if err != nil {
		t.Fatal(err)
	}
	source := string(js)
	if strings.Contains(source, `f-token").value.trim()`) {
		t.Fatal("browser code trims a token instead of letting cfg.CheckToken reject whitespace")
	}
	start := strings.Index(source, "function openAdd(")
	end := strings.Index(source, "function openURL(")
	if start < 0 || end <= start {
		t.Fatal("could not find add flow")
	}
	add := source[start:end]
	firstWrite := strings.Index(add, `await write("/api/env"`)
	if !strings.Contains(add, "const form = {") || firstWrite < 0 {
		t.Fatal("add flow does not snapshot the form before writing")
	}
	for _, liveRead := range []string{`val("f-`, `$("f-token").value`} {
		if strings.Contains(add[firstWrite:], liveRead) {
			t.Fatalf("add flow reads live form state after its first write: %s", liveRead)
		}
	}
	doneStart := strings.Index(source, `$("done").addEventListener`)
	if doneStart < 0 {
		t.Fatal("could not find quit flow")
	}
	done := source[doneStart:]
	reject := strings.Index(done, "if (e.status)")
	stopped := strings.Index(done, `$("stopped").hidden = false`)
	if reject < 0 || stopped < 0 || reject > stopped {
		t.Fatal("quit flow reports stopped before handling an HTTP rejection")
	}
	slackStart := strings.Index(source, "function openSlack(")
	slackEnd := strings.Index(source, "function openMeta(")
	if slackStart < 0 || slackEnd <= slackStart {
		t.Fatal("could not find Slack flow")
	}
	if !strings.Contains(source[slackStart:slackEnd], `: ["prod"]`) {
		t.Fatal("clearing the Slack env list does not restore agentflow's prod default")
	}
	groupsStart := strings.Index(source, "function groups(")
	renderStart := strings.Index(source, "function render(")
	if groupsStart < 0 || renderStart <= groupsStart || !strings.Contains(source[groupsStart:renderStart], "state.announces") {
		t.Fatal("service cards omit announce-only services")
	}
	if !strings.Contains(source[renderStart:slackStart], "!gs.length") {
		t.Fatal("announce-only services are replaced by the empty state")
	}
}

func TestQuit(t *testing.T) {
	s, ts := newServer(t)
	if code, _ := do(t, s, ts, call{path: "/api/quit", body: `{}`}); code != 200 {
		t.Fatalf("quit: %d", code)
	}
	select {
	case <-s.quit:
	default:
		t.Fatal("quit did not signal")
	}
	if code, _ := do(t, s, ts, call{path: "/api/quit", body: `{}`}); code != 200 {
		t.Fatal("a second quit must not panic")
	}
}

// Serve binds loopback only and prints a URL whose key is in the fragment.
func TestServeLoopbackAndIdle(t *testing.T) {
	p := filepath.Join(t.TempDir(), "services.toml")
	var out strings.Builder
	urlc := make(chan string, 1)
	done := make(chan error, 1)
	go func() {
		done <- Serve(Options{Store: &cfg.Store{Path: p}, Version: "t", Idle: 200 * time.Millisecond, Stdout: &out, Ready: func(u string) { urlc <- u }})
	}()
	u := <-urlc
	if !strings.HasPrefix(u, "http://127.0.0.1:") || !strings.Contains(u, "/#k=") {
		t.Fatalf("url %q", u)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("server did not stop when idle")
	}
}

func TestServeRejectsNonPositiveIdle(t *testing.T) {
	err := Serve(Options{Store: &cfg.Store{Path: filepath.Join(t.TempDir(), "s.toml")}, Idle: 0, Stdout: io.Discard})
	if err == nil {
		t.Fatal("Serve accepted a zero idle timeout")
	}
}

func TestAnnounceOverAPI(t *testing.T) {
	keychaintest.Temp(t)
	const hook = "https://hooks.slack.com/services/T0FAKE1/B0FAKE1/fakeSecretPart123"
	s, ts := newServer(t)
	code, body := do(t, s, ts, call{path: "/api/announce", body: `{"version":"","name":"pay","channel":"#pay","envs":null,"webhook":"` + hook + `"}`})
	if code != 200 || strings.Contains(body, "fakeSecretPart123") || !strings.Contains(body, `"channel":"#pay"`) || !strings.Contains(body, `"source":"keychain"`) {
		t.Fatalf("announce: %d %s", code, body)
	}
	// An edit without a webhook keeps the stored one.
	code, body = do(t, s, ts, call{path: "/api/announce", body: `{"version":"","name":"pay","channel":"#pay2","envs":["prod"],"webhook":""}`})
	if code != 200 || !strings.Contains(body, `"channel":"#pay2"`) || !strings.Contains(body, `"wired":true`) {
		t.Fatalf("edit: %d %s", code, body)
	}
	code, body = do(t, s, ts, call{path: "/api/announce", body: `{"version":"","name":"pay","channel":"#x","envs":null,"webhook":"https://evil.example.com/fakeSecretPart123"}`})
	if code != 400 || strings.Contains(body, "fakeSecretPart123") {
		t.Fatalf("bad webhook: %d %s", code, body)
	}
	if code, body = do(t, s, ts, call{path: "/api/announce/remove", body: `{"version":"","name":"pay"}`}); code != 200 || strings.Contains(body, `"name":"pay","channel"`) {
		t.Fatalf("remove: %d %s", code, body)
	}
	if code, _ := do(t, s, ts, call{path: "/api/announce/remove", body: `{"name":"pay"}`, origin: "https://evil.example.com"}); code != 403 {
		t.Fatalf("cross-origin remove: %d", code)
	}
}
