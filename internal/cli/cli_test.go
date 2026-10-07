package cli

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"testing"
)

const testToken = "at_e2e_token_abcdef123"

// newSurface spins up a fake /agent surface.
func newSurface(t *testing.T, handlers map[string]http.HandlerFunc) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	for p, h := range handlers {
		mux.HandleFunc(p, h)
	}
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv
}

func jsonHandler(body string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, body)
	}
}

func writeConfig(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "services.toml")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func run(t *testing.T, args ...string) (code int, stdout, stderr string) {
	t.Helper()
	var out, errBuf bytes.Buffer
	code = Run(args, &out, &errBuf)
	return code, out.String(), errBuf.String()
}

func standardConfig(t *testing.T, baseURL string) string {
	return writeConfig(t, fmt.Sprintf(`
[payments.dev]
base_url = %q
token    = %q

[legacy.dev]
base_url = "https://legacy.example.com"
token    = "REPLACE_ME"

[payments.meta]
repo = "org/payments"
`, baseURL, testToken))
}

func TestUsageScreen(t *testing.T) {
	code, out, _ := run(t)
	if code != 1 {
		t.Errorf("bare invocation exit = %d, want 1", code)
	}
	for _, cmd := range []string{"ls", "get", "endpoints", "status", "version"} {
		if !strings.Contains(out, cmd) {
			t.Errorf("usage missing command %q", cmd)
		}
	}
	code, _, _ = run(t, "--help")
	if code != 0 {
		t.Errorf("--help exit = %d, want 0", code)
	}
}

func TestVersionCommand(t *testing.T) {
	code, out, _ := run(t, "version")
	if code != 0 || !strings.Contains(out, "agentctl") {
		t.Errorf("version: code=%d out=%q", code, out)
	}
}

func TestUnknownCommand(t *testing.T) {
	code, _, errOut := run(t, "frobnicate")
	if code != 1 || !strings.Contains(errOut, "unknown command") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
}

func TestLs(t *testing.T) {
	cfg := standardConfig(t, "https://dev.example.com")
	code, out, errOut := run(t, "ls", "--config", cfg)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "payments.dev") || !strings.Contains(out, "wired") {
		t.Errorf("ls output missing wired service: %q", out)
	}
	if !strings.Contains(out, "legacy.dev") || !strings.Contains(out, "NOT WIRED") {
		t.Errorf("ls output missing not-wired service: %q", out)
	}
	if strings.Contains(out+errOut, testToken) || strings.Contains(out, "REPLACE_ME"[0:7]+"_ME") {
		t.Errorf("token material in ls output: %q", out)
	}
}

func TestLsMissingConfig(t *testing.T) {
	code, _, errOut := run(t, "ls", "--config", filepath.Join(t.TempDir(), "absent.toml"))
	if code != 1 {
		t.Errorf("exit %d, want 1", code)
	}
	if !strings.Contains(errOut, "absent.toml") || !strings.Contains(errOut, "base_url") {
		t.Errorf("missing-config error should include path and example: %q", errOut)
	}
}

func TestGetPrettyAndRaw(t *testing.T) {
	rawBody := `{"b":2,"a":{"nested":true}}`
	srv := newSurface(t, map[string]http.HandlerFunc{
		"/agent/version": jsonHandler(rawBody),
	})
	cfg := standardConfig(t, srv.URL)

	code, out, _ := run(t, "get", "payments.dev", "version", "--config", cfg)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "  \"b\": 2") {
		t.Errorf("not pretty-printed: %q", out)
	}
	// key order preserved by json.Indent
	if strings.Index(out, `"b"`) > strings.Index(out, `"a"`) {
		t.Errorf("key order not preserved: %q", out)
	}

	code, out, _ = run(t, "get", "payments.dev", "version", "--raw", "--config", cfg)
	if code != 0 || out != rawBody {
		t.Errorf("--raw not byte-identical: code=%d out=%q", code, out)
	}
}

func TestGetNonJSONPassthrough(t *testing.T) {
	srv := newSurface(t, map[string]http.HandlerFunc{
		"/agent/text": func(w http.ResponseWriter, r *http.Request) {
			fmt.Fprint(w, "plain text\x07 with bell")
		},
	})
	cfg := standardConfig(t, srv.URL)
	code, out, _ := run(t, "get", "payments.dev", "text", "--config", cfg)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "plain text with bell") || strings.Contains(out, "\x07") {
		t.Errorf("control chars not stripped: %q", out)
	}
	// --raw keeps the bell byte
	_, out, _ = run(t, "get", "payments.dev", "text", "--raw", "--config", cfg)
	if !strings.Contains(out, "\x07") {
		t.Errorf("--raw should bypass stripping: %q", out)
	}
}

func TestGet404(t *testing.T) {
	srv := newSurface(t, map[string]http.HandlerFunc{
		"/agent/exists": jsonHandler(`{}`),
	})
	cfg := standardConfig(t, srv.URL)
	code, out, errOut := run(t, "get", "payments.dev", "missing", "--config", cfg)
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
	if !strings.Contains(out, "404") && out == "" {
		t.Errorf("body should still print to stdout: %q", out)
	}
	if !strings.Contains(errOut, "HTTP 404") {
		t.Errorf("stderr should note the status: %q", errOut)
	}
}

func TestGetTransportError(t *testing.T) {
	cfg := writeConfig(t, fmt.Sprintf("[dead.dev]\nbase_url = \"http://127.0.0.1:1\"\ntoken = %q\n", testToken))
	code, _, errOut := run(t, "get", "dead.dev", "version", "--config", cfg, "--timeout", "500ms")
	if code != 3 {
		t.Errorf("exit %d, want 3 (stderr %q)", code, errOut)
	}
	if !strings.Contains(errOut, "transport") {
		t.Errorf("stderr should say transport: %q", errOut)
	}
}

func TestGetRejectedPath(t *testing.T) {
	srv := newSurface(t, nil)
	cfg := standardConfig(t, srv.URL)
	code, _, errOut := run(t, "get", "payments.dev", "../etc/passwd", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "invalid path") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
}

func TestGetNotWiredRefusedNoHTTP(t *testing.T) {
	var hits atomic.Int32
	srv := newSurface(t, map[string]http.HandlerFunc{
		"/": func(w http.ResponseWriter, r *http.Request) { hits.Add(1) },
	})
	cfg := writeConfig(t, fmt.Sprintf("[svc.dev]\nbase_url = %q\ntoken = \"REPLACE_ME\"\n", srv.URL))
	code, _, errOut := run(t, "get", "svc.dev", "version", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "not wired") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
	if hits.Load() != 0 {
		t.Errorf("server was hit %d times for a not-wired service", hits.Load())
	}
}

func TestGetUnknownService(t *testing.T) {
	cfg := standardConfig(t, "https://x.example.com")
	code, _, errOut := run(t, "get", "nope.dev", "version", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "unknown service") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
}

func TestEndpointsConvention(t *testing.T) {
	srv := newSurface(t, map[string]http.HandlerFunc{
		"/agent": jsonHandler(`{"endpoints":[{"path":"/agent/version","description":"build info"},{"path":"/agent/health","description":"liveness"}],"extra":"ignored"}`),
	})
	cfg := standardConfig(t, srv.URL)
	code, out, _ := run(t, "endpoints", "payments.dev", "--config", cfg)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, "/agent/version") || !strings.Contains(out, "build info") || !strings.Contains(out, "PATH") {
		t.Errorf("endpoints table wrong: %q", out)
	}
}

func TestEndpointsFallback(t *testing.T) {
	srv := newSurface(t, map[string]http.HandlerFunc{
		"/agent": jsonHandler(`{"something":"else"}`),
	})
	cfg := standardConfig(t, srv.URL)
	code, out, errOut := run(t, "endpoints", "payments.dev", "--config", cfg)
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(out, `"something"`) {
		t.Errorf("fallback JSON missing: %q", out)
	}
	if !strings.Contains(errOut, "convention") {
		t.Errorf("stderr should note fallback: %q", errOut)
	}
}

func statusSurface(t *testing.T, versionStatus, healthStatus int) *httptest.Server {
	return newSurface(t, map[string]http.HandlerFunc{
		"/agent/version": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(versionStatus)
			fmt.Fprint(w, `{"version":"abc1234"}`)
		},
		"/agent/health": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(healthStatus)
			fmt.Fprint(w, `{"status":"x"}`)
		},
	})
}

func TestStatusMatrix(t *testing.T) {
	okSrv := statusSurface(t, 200, 200)
	httpFailSrv := statusSurface(t, 200, 503)

	cfg := writeConfig(t, fmt.Sprintf(`
[alpha.dev]
base_url = %q
token = %q

[bravo.dev]
base_url = %q
token = %q

[charlie.dev]
base_url = "http://127.0.0.1:1"
token = %q

[delta.dev]
base_url = "https://d.example.com"
token = "REPLACE_ME"
`, okSrv.URL, testToken, httpFailSrv.URL, testToken, testToken))

	code, out, _ := run(t, "status", "--config", cfg, "--timeout", "2s")
	if code != 3 {
		t.Errorf("exit %d, want 3 (transport worst)", code)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 4 {
		t.Fatalf("want 4 lines, got %d: %q", len(lines), out)
	}
	// sorted by name
	for i, prefix := range []string{"alpha.dev", "bravo.dev", "charlie.dev", "delta.dev"} {
		if !strings.HasPrefix(lines[i], prefix) {
			t.Errorf("line %d = %q, want prefix %q", i, lines[i], prefix)
		}
	}
	if !strings.Contains(lines[0], "ok") || !strings.Contains(lines[0], "version=abc1234") {
		t.Errorf("alpha line: %q", lines[0])
	}
	if !strings.Contains(lines[1], "FAIL") || !strings.Contains(lines[1], "503") {
		t.Errorf("bravo line: %q", lines[1])
	}
	if !strings.Contains(lines[2], "FAIL") {
		t.Errorf("charlie line: %q", lines[2])
	}
	if !strings.Contains(lines[3], "SKIP") {
		t.Errorf("delta line: %q", lines[3])
	}
}

func TestStatusHTTPWorst(t *testing.T) {
	httpFailSrv := statusSurface(t, 200, 500)
	cfg := writeConfig(t, fmt.Sprintf("[x.dev]\nbase_url = %q\ntoken = %q\n", httpFailSrv.URL, testToken))
	code, _, _ := run(t, "status", "--config", cfg)
	if code != 2 {
		t.Errorf("exit %d, want 2", code)
	}
}

func TestStatusAllOK(t *testing.T) {
	okSrv := statusSurface(t, 200, 200)
	cfg := writeConfig(t, fmt.Sprintf("[x.dev]\nbase_url = %q\ntoken = %q\n", okSrv.URL, testToken))
	code, out, _ := run(t, "status", "--config", cfg)
	if code != 0 || !strings.Contains(out, "health=ok") {
		t.Errorf("code=%d out=%q", code, out)
	}
}

func TestStatusNamedSubset(t *testing.T) {
	okSrv := statusSurface(t, 200, 200)
	cfg := writeConfig(t, fmt.Sprintf(`
[x.dev]
base_url = %q
token = %q

[dead.dev]
base_url = "http://127.0.0.1:1"
token = %q
`, okSrv.URL, testToken, testToken))
	code, out, _ := run(t, "status", "x.dev", "--config", cfg)
	if code != 0 {
		t.Errorf("exit %d, want 0 (named subset should skip dead.dev)", code)
	}
	if strings.Contains(out, "dead.dev") {
		t.Errorf("unnamed service in output: %q", out)
	}
}

func TestStatusNamingNotWiredFails(t *testing.T) {
	var hits atomic.Int32
	srv := newSurface(t, map[string]http.HandlerFunc{
		"/": func(w http.ResponseWriter, r *http.Request) { hits.Add(1) },
	})
	cfg := writeConfig(t, fmt.Sprintf("[svc.dev]\nbase_url = %q\ntoken = \"TODO\"\n", srv.URL))
	code, _, errOut := run(t, "status", "svc.dev", "--config", cfg)
	if code != 1 || !strings.Contains(errOut, "not wired") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
	if hits.Load() != 0 {
		t.Errorf("HTTP call made before not-wired refusal")
	}
}

// TestNoTokenInAnyOutput runs every command and asserts the raw token never
// appears on stdout or stderr.
func TestNoTokenInAnyOutput(t *testing.T) {
	srv := newSurface(t, map[string]http.HandlerFunc{
		"/agent":         jsonHandler(`{"endpoints":[{"path":"/agent/version","description":"v"}]}`),
		"/agent/version": jsonHandler(`{"version":"abc"}`),
		"/agent/health":  jsonHandler(`{"status":"ok"}`),
	})
	cfg := standardConfig(t, srv.URL)
	invocations := [][]string{
		{"ls", "--config", cfg},
		{"get", "payments.dev", "version", "--config", cfg},
		{"get", "payments.dev", "version", "--raw", "--config", cfg},
		{"get", "payments.dev", "../bad", "--config", cfg},
		{"get", "nope.dev", "x", "--config", cfg},
		{"endpoints", "payments.dev", "--config", cfg},
		{"status", "--config", cfg},
		{"version"},
		{"--help"},
	}
	for _, inv := range invocations {
		_, out, errOut := run(t, inv...)
		if strings.Contains(out+errOut, testToken) {
			t.Errorf("token leaked by %v:\nstdout=%q\nstderr=%q", inv, out, errOut)
		}
	}
}

func TestFlagsBeforeSubcommand(t *testing.T) {
	cfg := standardConfig(t, "https://x.example.com")
	code, out, _ := run(t, "--config", cfg, "ls")
	if code != 0 || !strings.Contains(out, "payments.dev") {
		t.Errorf("global flag before subcommand failed: code=%d out=%q", code, out)
	}
}

func TestConfigEnvVar(t *testing.T) {
	cfg := standardConfig(t, "https://x.example.com")
	t.Setenv("AGENTCTL_CONFIG", cfg)
	code, out, _ := run(t, "ls")
	if code != 0 || !strings.Contains(out, "payments.dev") {
		t.Errorf("AGENTCTL_CONFIG not honored: code=%d out=%q", code, out)
	}
}

func TestBadFlag(t *testing.T) {
	code, _, errOut := run(t, "ls", "--method", "POST")
	if code != 1 || !strings.Contains(errOut, "unknown flag") {
		t.Errorf("code=%d err=%q", code, errOut)
	}
}

// TestSourceGuardGetOnly greps agentctl's non-test Go sources for mutating
// HTTP verbs — agentctl must contain GET only. The other binaries' code
// (agentflow posts to Slack; agentcfg serves its settings page) is skipped:
// internal/flow/boundary_test.go proves agentctl never imports it.
func TestSourceGuardGetOnly(t *testing.T) {
	root := "../.."
	otherBinaries := map[string]bool{
		filepath.Join(root, "cmd"):              true,
		filepath.Join(root, "internal", "flow"): true,
		filepath.Join(root, "internal", "cfg"):  true,
	}
	bad := regexp.MustCompile(`MethodPost|MethodPut|MethodDelete|MethodPatch|"POST"|"PUT"|"DELETE"|"PATCH"`)
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() {
			if info.Name() == ".git" || info.Name() == "bin" || info.Name() == ".claude" || otherBinaries[path] {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if loc := bad.Find(b); loc != nil {
			t.Errorf("mutating HTTP verb %q found in %s", loc, path)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestLsShowsMeta(t *testing.T) {
	cfg := writeConfig(t, `
[payments.dev]
base_url = "https://dev.example.com"
token    = "`+testToken+`"

[payments.meta]
repo = "org/payments"
unit = "payments.service"
`)
	code, out, errOut := run(t, "ls", "--config", cfg)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errOut)
	}
	if !strings.Contains(out, "META") {
		t.Errorf("ls output missing META column header: %q", out)
	}
	if !strings.Contains(out, "repo=org/payments") || !strings.Contains(out, "unit=payments.service") {
		t.Errorf("ls output missing meta values: %q", out)
	}
}
