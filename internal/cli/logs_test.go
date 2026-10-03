package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The three /agent/logs shapes in production.
const (
	rcxLogs    = `{"configured":true,"count":1,"entries":[{"level":"WARN","message":"serve_loop error addr=TCP","target":"rsipstack::transport","ts":"2026-10-03T14:28:04.291Z"}]}`
	dialerLogs = `{"enabled":true,"matched":1,"entries":[{"time":"2026-10-03T14:42:03.873Z","level":"info","message":"HTTP request","fields":{"status":200,"path":"/health"}}]}`
	ccLogs     = `{"count":1,"entries":[{"dt":"2026-10-03T14:42:00.734Z","level":"info","msg":"workqueue item dispatched","fields":{"source":"aarons","external_id":"17243278"}}]}`
	emptyLogs  = `{"count":0,"entries":[]}`
)

// logSurface serves /agent/logs with the given body and records each query.
func logSurface(t *testing.T, h http.HandlerFunc) (cfg string, queries *[]url.Values) {
	t.Helper()
	var mu sync.Mutex
	var qs []url.Values
	srv := newSurface(t, map[string]http.HandlerFunc{"/agent/logs": func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			t.Errorf("/agent/logs method = %s, want GET", r.Method)
		}
		mu.Lock()
		qs = append(qs, r.URL.Query())
		mu.Unlock()
		h(w, r)
	}})
	return standardConfig(t, srv.URL), &qs
}

func TestLogsRendersEveryShape(t *testing.T) {
	cases := []struct{ body, want string }{
		{rcxLogs, "2026-10-03T14:28:04.291Z WARN  rsipstack::transport  serve_loop error addr=TCP\n"},
		{dialerLogs, "2026-10-03T14:42:03.873Z INFO   HTTP request | path=/health status=200\n"},
		{ccLogs, "2026-10-03T14:42:00.734Z INFO   workqueue item dispatched | external_id=17243278 source=aarons\n"},
	}
	for _, c := range cases {
		cfg, _ := logSurface(t, jsonHandler(c.body))
		code, out, errOut := run(t, "--config", cfg, "logs", "payments.dev")
		if code != 0 || out != c.want {
			t.Errorf("exit %d stderr %q\n got %q\nwant %q", code, errOut, out, c.want)
		}
	}
}

func TestLogsPassesFiltersEncoded(t *testing.T) {
	cfg, qs := logSurface(t, jsonHandler(ccLogs))
	before := time.Now()
	code, _, errOut := run(t, "--config", cfg, "logs", "payments.dev", "--q", "dial customer|TEST & co", "--level", "WARNING", "--since", "30m", "--limit", "40")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	q := (*qs)[0]
	if q.Get("q") != "dial customer|TEST & co" || q.Get("level") != "warn" || q.Get("limit") != "40" {
		t.Errorf("query = %v", q)
	}
	since, err := time.Parse(time.RFC3339Nano, q.Get("since"))
	if err != nil || since.After(before.Add(-29*time.Minute)) || since.Before(before.Add(-31*time.Minute)) {
		t.Errorf("since = %q (%v), want about 30m ago", q.Get("since"), err)
	}
	cfg2, qs2 := logSurface(t, jsonHandler(ccLogs))
	run(t, "--config", cfg2, "logs", "payments.dev", "--since", "2026-10-03T14:00:00Z")
	if got := (*qs2)[0].Get("since"); got != "2026-10-03T14:00:00Z" {
		t.Errorf("absolute since = %q", got)
	}
}

func TestLogsJSON(t *testing.T) {
	cfg, _ := logSurface(t, jsonHandler(rcxLogs))
	code, out, _ := run(t, "--config", cfg, "logs", "payments.dev", "--json")
	var got []map[string]any
	if code != 0 || json.Unmarshal([]byte(out), &got) != nil || len(got) != 1 || got[0]["source"] != "rsipstack::transport" || got[0]["level"] != "WARN" {
		t.Fatalf("exit %d out %s", code, out)
	}
	cfg, _ = logSurface(t, jsonHandler(emptyLogs))
	if code, out, _ := run(t, "--config", cfg, "logs", "payments.dev", "--json"); code != 0 || strings.TrimSpace(out) != "[]" {
		t.Errorf("empty --json: exit %d out %q", code, out)
	}
}

func TestLogsStripsTerminalEscapes(t *testing.T) {
	body := `{"entries":[{"ts":"t","level":"info","message":"a\u001b[31mred\u009b2Jb\u007f\u202eevil\u2028next","fields":{"k\u001b":"v\u0007"}}]}`
	cfg, _ := logSurface(t, jsonHandler(body))
	_, out, _ := run(t, "--config", cfg, "logs", "payments.dev")
	if strings.ContainsAny(out, "\x1b\x07\x7f") || strings.ContainsRune(out, '\u009b') || strings.ContainsRune(out, '\u202e') || strings.ContainsRune(out, '\u2028') || strings.Count(out, "\n") != 1 {
		t.Fatalf("control characters or extra lines leaked: %q", out)
	}
	if !strings.Contains(out, "a[31mred2Jbevil ⏎ next") {
		t.Errorf("message text lost: %q", out)
	}
	_, out, _ = run(t, "--config", cfg, "logs", "payments.dev", "--json")
	if strings.ContainsRune(out, '\u202e') || strings.ContainsRune(out, '\u2028') {
		t.Fatalf("dangerous Unicode leaked in JSON output: %q", out)
	}
}

func TestLogsScrubsConfiguredToken(t *testing.T) {
	body := fmt.Sprintf(`{"entries":[{"ts":"t","level":%q,"message":%q,"fields":{%q:[%q]}}]}`, testToken, testToken, testToken, testToken)
	cfg, _ := logSurface(t, jsonHandler(body))
	for _, extra := range [][]string{nil, {"--json"}} {
		args := append([]string{"--config", cfg, "logs", "payments.dev"}, extra...)
		code, out, errOut := run(t, args...)
		if code != 0 || strings.Contains(out+errOut, testToken) || !strings.Contains(out, "tok:") {
			t.Errorf("%v: exit %d stdout %q stderr %q", extra, code, out, errOut)
		}
	}
}

func TestLogsPreservesLargeFieldNumbers(t *testing.T) {
	body := `{"entries":[{"ts":"t","level":"info","message":"m","fields":{"external_id":9007199254740993}}]}`
	cfg, _ := logSurface(t, jsonHandler(body))
	code, out, errOut := run(t, "--config", cfg, "logs", "payments.dev")
	if code != 0 || !strings.Contains(out, "external_id=9007199254740993") {
		t.Fatalf("exit %d stdout %q stderr %q", code, out, errOut)
	}
}

func TestLogsWaitMatchesLater(t *testing.T) {
	var calls atomic.Int32
	cfg, qs := logSurface(t, func(w http.ResponseWriter, r *http.Request) {
		switch calls.Add(1) {
		case 1:
			fmt.Fprint(w, emptyLogs)
		case 2:
			w.WriteHeader(http.StatusServiceUnavailable) // restarting
		default:
			fmt.Fprint(w, ccLogs)
		}
	})
	code, out, errOut := run(t, "--config", cfg, "logs", "payments.dev", "--q", "dispatched", "--wait", "5s", "--interval", "20ms")
	if code != 0 || !strings.Contains(out, "workqueue item dispatched") || calls.Load() != 3 {
		t.Fatalf("exit %d calls %d out %q err %q", code, calls.Load(), out, errOut)
	}
	// The first request calibrates against the service's Date; actual polls
	// then keep one fixed since value.
	if first := (*qs)[0].Get("since"); first != "" {
		t.Errorf("calibration request since = %q, want empty", first)
	}
	fixed := (*qs)[1].Get("since")
	if _, err := time.Parse(time.RFC3339Nano, fixed); err != nil {
		t.Errorf("--wait must send a fixed since after calibration, got %q", fixed)
	}
	for _, q := range (*qs)[1:] {
		if q.Get("since") != fixed {
			t.Errorf("since moved between polls: %q then %q", fixed, q.Get("since"))
		}
	}
}

func TestLogsWaitAdjustsForServiceClock(t *testing.T) {
	var calls atomic.Int32
	serviceNow := time.Now().Add(-10 * time.Minute).Truncate(time.Second)
	cfg, qs := logSurface(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Date", serviceNow.UTC().Format(http.TimeFormat)) // HTTP dates are GMT
		if calls.Add(1) == 1 {
			fmt.Fprint(w, emptyLogs)
			return
		}
		fmt.Fprint(w, ccLogs)
	})
	code, _, errOut := run(t, "--config", cfg, "logs", "payments.dev", "--wait", "2s", "--interval", "20ms")
	if code != 0 {
		t.Fatalf("exit %d: %s", code, errOut)
	}
	since, err := time.Parse(time.RFC3339Nano, (*qs)[1].Get("since"))
	if err != nil || since.Before(serviceNow.Add(-2*time.Second)) || since.After(serviceNow.Add(2*time.Second)) {
		t.Fatalf("clock-adjusted since = %q (%v), want about %s", (*qs)[1].Get("since"), err, serviceNow)
	}
}

func TestLogsWaitTimesOut(t *testing.T) {
	var calls atomic.Int32
	cfg, _ := logSurface(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		fmt.Fprint(w, emptyLogs)
	})
	start := time.Now()
	code, out, errOut := run(t, "--config", cfg, "logs", "payments.dev", "--q", "x", "--wait", "300ms", "--interval", "100ms")
	elapsed := time.Since(start)
	if code != 4 || out != "" || !strings.Contains(errOut, "no matching entry") {
		t.Fatalf("exit %d out %q err %q", code, out, errOut)
	}
	if elapsed < 300*time.Millisecond {
		t.Errorf("gave up after %s, before the 300ms deadline", elapsed)
	}
	if calls.Load() < 3 {
		t.Errorf("only %d polls in 300ms at 100ms", calls.Load())
	}
}

func TestLogsWaitFailsFastOnPermanentHTTP(t *testing.T) {
	var calls atomic.Int32
	cfg, _ := logSurface(t, func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprintf(w, "{\"error\":\"denied \u202e%s\"}", testToken)
	})
	code, out, _ := run(t, "--config", cfg, "logs", "payments.dev", "--wait", "5s", "--interval", "10ms")
	if code != 2 || calls.Load() != 1 || !strings.Contains(out, "denied") || strings.Contains(out, testToken) || strings.ContainsRune(out, '\u202e') {
		t.Fatalf("exit %d after %d calls with stdout %q, want 2 after 1 with body", code, calls.Load(), out)
	}
}

func TestLogsWaitRequestCannotOutliveDeadline(t *testing.T) {
	cfg, _ := logSurface(t, func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done()
	})
	start := time.Now()
	code, _, _ := run(t, "--config", cfg, "logs", "payments.dev", "--wait", "100ms", "--interval", "20ms", "--timeout", "5s")
	if elapsed := time.Since(start); code != 4 || elapsed > time.Second {
		t.Fatalf("exit %d after %s, want exit 4 near the 100ms wait deadline", code, elapsed)
	}
}

func TestRetryableWaitOnlyRetries5xx(t *testing.T) {
	if !retryableWait(&httpError{status: 599}) || retryableWait(&httpError{status: 600}) {
		t.Fatal("retryableWait must accept exactly the 5xx HTTP range")
	}
}

func TestLogsUsageErrors(t *testing.T) {
	cfg, _ := logSurface(t, jsonHandler(ccLogs))
	cases := []struct {
		args    []string
		wantErr string
	}{
		{[]string{"logs"}, "usage: agentctl logs"},
		{[]string{"logs", "payments.dev", "--level", "loud"}, "invalid --level"},
		{[]string{"logs", "payments.dev", "--since", "yesterday"}, "invalid --since"},
		{[]string{"logs", "payments.dev", "--limit", "0"}, "invalid --limit"},
		{[]string{"logs", "payments.dev", "--interval", "1s"}, "only applies with --wait"},
		{[]string{"logs", "payments.dev", "--json=yes"}, "takes no value"},
		{[]string{"get", "payments.dev", "version", "--q", "x"}, "only apply to logs"},
		{[]string{"logs", "legacy.dev"}, "not wired"},
	}
	for _, c := range cases {
		code, _, errOut := run(t, append([]string{"--config", cfg}, c.args...)...)
		if code != 1 || !strings.Contains(errOut, c.wantErr) {
			t.Errorf("%v: exit %d stderr %q, want 1 and %q", c.args, code, errOut, c.wantErr)
		}
	}
}

func TestLogsNonEntriesBody(t *testing.T) {
	cfg, _ := logSurface(t, jsonHandler(`{"configured":false}`))
	if code, _, errOut := run(t, "--config", cfg, "logs", "payments.dev"); code != 1 || !strings.Contains(errOut, "did not return an entries list") {
		t.Fatalf("exit %d stderr %q", code, errOut)
	}
}
