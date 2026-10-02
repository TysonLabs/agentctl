package ship

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The three /agent/version shapes in use.
const (
	rcxBody    = `{"started_at":"2026-10-02T16:45:23Z","uptime_seconds":2780,"version":"rustpbx 0.4.4\nBuild Time: 2026-10-02 16:40:55 +00:00\nGit Commit: f316cb0f\nGit Branch: HEAD"}`
	dialerBody = `{"service":"vector-dialer","version":"d63a514","commit":"d63a514","build_time":"unknown"}`
	ccBody     = `{"build_time":"2026-10-02T17:10:10Z","git_sha":"9788904","started_at":"2026-10-02T17:10:49Z","uptime_seconds":1256}`
)

func TestParseVersionShapes(t *testing.T) {
	cases := []struct{ body, commit, started string }{
		{rcxBody, "f316cb0f", "2026-10-02T16:45:23Z"},
		{dialerBody, "d63a514", ""},
		{ccBody, "9788904", "2026-10-02T17:10:49Z"},
		{`{"git_commit":"ABCDEF1234"}`, "abcdef1234", ""},
		{`{"version":"build\nGit Commit: 0123abcd"}`, "0123abcd", ""},
		{`{"commit":"0123abcd","git_sha":"0123abcd"}`, "0123abcd", ""},
	}
	for _, c := range cases {
		got, started, err := ParseVersion([]byte(c.body))
		if err != nil || got != c.commit || started != c.started {
			t.Errorf("ParseVersion(%s) = %q, %q, %v; want %q, %q", c.body, got, started, err, c.commit, c.started)
		}
	}
}

func TestParseVersionRejects(t *testing.T) {
	for _, body := range []string{
		`{"version":"1.2.3"}`,                                   // semver is not a commit
		`{"commit":"abc"}`,                                      // too short
		`{"commit":"unknown"}`,                                  // not hex
		`{"sha":"f316cb0f"}`,                                    // generic hashes are not source commits
		`{"notes":"Git Commit: f316cb0f"}`,                      // only the version banner is trusted
		`{"version":"Last Git Commit: f316cb0f"}`,               // the label must start a line
		`{"commit":"f316cb0f","git_sha":"d63a514"}`,             // recognized fields conflict
		`{"commit":"f316cb0","git_sha":"f316cb0f"}`,             // prefix agreement is still ambiguous
		`{"commit":"not-hex","version":"Git Commit: f316cb0f"}`, // malformed authoritative field
		`not json`,     // not a document
		`["f316cb0f"]`, // not an object
	} {
		if c, _, err := ParseVersion([]byte(body)); err == nil {
			t.Errorf("ParseVersion(%s) = %q, want an error", body, c)
		}
	}
}

func TestMatches(t *testing.T) {
	yes := [][2]string{{"f316cb0f", "f316cb0f9211b2261c554e55074aec2e5d3dff70"}, {"F316CB0", "f316cb0f"}, {"d63a514", "d63a514"}}
	no := [][2]string{{"f316cb0f", "f316cb0e"}, {"f316cb", "f316cb0f"}, {"", "f316cb0f"}, {"f316cb0f", "xyz"}}
	for _, p := range yes {
		if !Matches(p[0], p[1]) || !Matches(p[1], p[0]) {
			t.Errorf("Matches(%q, %q) = false, want true", p[0], p[1])
		}
	}
	for _, p := range no {
		if Matches(p[0], p[1]) {
			t.Errorf("Matches(%q, %q) = true, want false", p[0], p[1])
		}
	}
}

// script returns a Fetch that replays responses in order, repeating the last.
func script(steps ...func() ([]byte, int, error)) (Fetch, *atomic.Int32) {
	var n atomic.Int32
	return func(ctx context.Context, service string) ([]byte, int, error) {
		i := int(n.Add(1)) - 1
		if i >= len(steps) {
			i = len(steps) - 1
		}
		return steps[i]()
	}, &n
}

func body(s string) func() ([]byte, int, error) {
	return func() ([]byte, int, error) { return []byte(s), 0, nil }
}
func exit(code int, msg string) func() ([]byte, int, error) {
	return func() ([]byte, int, error) { return []byte(msg), code, nil }
}

func opts(f Fetch, expected string) Options {
	return Options{Service: "svc.prod", Expected: expected, Timeout: 5 * time.Second, Interval: 10 * time.Millisecond, Fetch: f}
}

func TestWaitsThroughRestartUntilMatch(t *testing.T) {
	f, n := script(
		body(`{"commit":"1111111"}`),                       // old build still running
		exit(3, "agentctl: transport: connection refused"), // restarting
		exit(2, "agentctl: HTTP 502"),                      // proxy up, app not yet
		body(`{"commit":"2222222"}`),                       // new build
	)
	res := Verify(context.Background(), opts(f, "2222222abcdef"))
	if res.Status != StatusDeployed || res.Match != "exact" || res.Running != "2222222" || n.Load() != 4 || res.Error != "" {
		t.Fatalf("got %+v after %d fetches", res, n.Load())
	}
}

func TestTimeoutReportsLastState(t *testing.T) {
	f, _ := script(body(`{"commit":"1111111"}`))
	o := opts(f, "2222222")
	o.Timeout = 100 * time.Millisecond
	res := Verify(context.Background(), o)
	if res.Status != StatusTimeout || res.Running != "1111111" || !strings.Contains(res.Error, "1111111") {
		t.Fatalf("got %+v", res)
	}
}

func TestTimeoutWhileUnreachableKeepsError(t *testing.T) {
	f, _ := script(exit(3, "agentctl: transport: timeout"))
	o := opts(f, "2222222")
	o.Timeout = 100 * time.Millisecond
	res := Verify(context.Background(), o)
	if res.Status != StatusTimeout || !strings.Contains(res.Error, "transport") {
		t.Fatalf("got %+v", res)
	}
}

func TestTimeoutCancelsInflightFetch(t *testing.T) {
	f := func(ctx context.Context, _ string) ([]byte, int, error) {
		select {
		case <-ctx.Done():
			return nil, 0, ctx.Err()
		case <-time.After(500 * time.Millisecond):
			return []byte(`{"commit":"2222222"}`), 0, nil
		}
	}
	o := opts(f, "2222222")
	o.Timeout = 50 * time.Millisecond
	start := time.Now()
	res := Verify(context.Background(), o)
	if res.Status != StatusTimeout || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("got %+v after %s", res, time.Since(start))
	}
}

func TestLateMatchDoesNotBeatDeadline(t *testing.T) {
	f := func(context.Context, string) ([]byte, int, error) {
		time.Sleep(80 * time.Millisecond) // deliberately ignores cancellation
		return []byte(`{"commit":"2222222"}`), 0, nil
	}
	o := opts(f, "2222222")
	o.Timeout = 30 * time.Millisecond
	if res := Verify(context.Background(), o); res.Status != StatusTimeout {
		t.Fatalf("got %+v", res)
	}
}

func TestUnreadableFailsFast(t *testing.T) {
	f, n := script(body(`{"version":"1.2.3"}`))
	res := Verify(context.Background(), opts(f, "2222222"))
	if res.Status != StatusUnreadable || n.Load() != 1 {
		t.Fatalf("got %+v after %d fetches", res, n.Load())
	}
}

func TestAgentctlUsageErrorFailsFast(t *testing.T) {
	f, n := script(exit(1, `agentctl: unknown service "svc.prod"`))
	res := Verify(context.Background(), opts(f, "2222222"))
	if res.Status != StatusAgentctl || n.Load() != 1 || !strings.Contains(res.Error, "unknown service") {
		t.Fatalf("got %+v", res)
	}
}

func TestPermanentHTTPErrorFailsFast(t *testing.T) {
	for _, status := range []string{"400", "401", "403"} {
		f, n := script(exit(2, "agentctl: HTTP "+status+" from svc.prod /agent/version"))
		res := Verify(context.Background(), opts(f, "2222222"))
		if res.Status != StatusAgentctl || n.Load() != 1 || !strings.Contains(res.Error, "HTTP "+status) {
			t.Errorf("HTTP %s: got %+v after %d fetches", status, res, n.Load())
		}
	}
}

func TestUnexpectedAgentctlExitFailsFast(t *testing.T) {
	f, n := script(exit(17, "unexpected wrapper failure"))
	res := Verify(context.Background(), opts(f, "2222222"))
	if res.Status != StatusAgentctl || n.Load() != 1 || !strings.Contains(res.Error, "exit 17") {
		t.Fatalf("got %+v after %d fetches", res, n.Load())
	}
}

func TestFetchErrorFailsFast(t *testing.T) {
	f, _ := script(func() ([]byte, int, error) { return nil, 0, errors.New("exec: agentctl: not found") })
	if res := Verify(context.Background(), opts(f, "2222222")); res.Status != StatusAgentctl {
		t.Fatalf("got %+v", res)
	}
}

func TestOnce(t *testing.T) {
	f, n := script(body(`{"commit":"1111111"}`))
	o := opts(f, "2222222")
	o.Once = true
	res := Verify(context.Background(), o)
	if res.Status != StatusNotDeployed || n.Load() != 1 || !strings.Contains(res.Error, "1111111") {
		t.Fatalf("got %+v", res)
	}
	f, _ = script(exit(3, "agentctl: transport: refused"))
	o.Fetch = f
	if res := Verify(context.Background(), o); res.Status != StatusNotDeployed || !strings.Contains(res.Error, "refused") {
		t.Fatalf("unreachable --once: got %+v", res)
	}
}

func TestInterrupted(t *testing.T) {
	f, _ := script(body(`{"commit":"1111111"}`))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	if res := Verify(ctx, opts(f, "2222222")); res.Status != StatusInterrupted {
		t.Fatalf("got %+v", res)
	}
}

func TestContainsViaAncestry(t *testing.T) {
	repo := t.TempDir()
	run := func(args ...string) string {
		out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	run("init", "-q")
	commit := func(msg string) string {
		run("-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", msg)
		return run("rev-parse", "HEAD")
	}
	old := commit("old")
	expected := commit("expected")
	newer := commit("newer")
	run("checkout", "-q", "-b", "side", old)
	sibling := commit("sibling") // not a descendant of expected

	cases := []struct {
		running string
		want    Status
	}{
		{newer[:8], StatusDeployed},      // newer deploy contains expected
		{old[:8], StatusNotDeployed},     // older deploy does not
		{sibling[:8], StatusNotDeployed}, // diverged deploy does not
		{"abcdef1", StatusNotDeployed},   // unknown to the clone: plain no
	}
	for _, c := range cases {
		f, _ := script(body(`{"commit":"` + c.running + `"}`))
		o := opts(f, expected)
		o.Once, o.Contains, o.Repo = true, true, repo
		res := Verify(context.Background(), o)
		if res.Status != c.want {
			t.Errorf("running %s: got %+v, want %s", c.running, res, c.want)
		}
		if c.want == StatusDeployed && res.Match != "contains" {
			t.Errorf("running %s: match = %q, want contains", c.running, res.Match)
		}
	}
	// Descendant acceptance is opt-in so a rollback to expected does not
	// succeed while the newer deployment is still running.
	f, _ := script(body(`{"commit":"` + newer[:8] + `"}`))
	o := opts(f, expected)
	o.Once, o.Repo = true, repo
	if res := Verify(context.Background(), o); res.Status != StatusNotDeployed {
		t.Errorf("exact default: got %+v", res)
	}
	// Without a repo there is no ancestry check even if requested.
	f, _ = script(body(`{"commit":"` + newer[:8] + `"}`))
	o = opts(f, expected)
	o.Once, o.Contains = true, true
	if res := Verify(context.Background(), o); res.Status != StatusNotDeployed {
		t.Errorf("no repo: got %+v", res)
	}
}

func TestAgentctlFetchExitCodes(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "agentctl")
	// A fake agentctl: behaviour chosen by the service name.
	sh := `#!/bin/sh
case "$2" in
  ok.prod)      echo '{"commit":"2222222"}' ;;
  http.prod)    echo 'body'; echo 'agentctl: HTTP 502 from http.prod' >&2; exit 2 ;;
  usage.prod)   echo 'agentctl: unknown service "usage.prod"' >&2; exit 1 ;;
  slow.prod)    sleep 5 ;;
esac
`
	if err := os.WriteFile(bin, []byte(sh), 0o755); err != nil {
		t.Fatal(err)
	}
	f := AgentctlFetch(bin, 5*time.Second)
	ctx := context.Background()
	if b, code, err := f(ctx, "ok.prod"); err != nil || code != 0 || !strings.Contains(string(b), "2222222") {
		t.Errorf("ok: %q %d %v", b, code, err)
	}
	if b, code, err := f(ctx, "http.prod"); err != nil || code != 2 || !strings.Contains(string(b), "HTTP 502") {
		t.Errorf("http: %q %d %v (stderr should win over the body)", b, code, err)
	}
	if b, code, err := f(ctx, "usage.prod"); err != nil || code != 1 || !strings.Contains(string(b), "unknown service") {
		t.Errorf("usage: %q %d %v", b, code, err)
	}
	start := time.Now()
	if _, code, err := AgentctlFetch(bin, 300*time.Millisecond)(ctx, "slow.prod"); err != nil || code != 3 {
		t.Errorf("slow: code %d err %v, want a transport-like 3", code, err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Errorf("slow: a 300ms call took %s (waited on the orphaned child)", d)
	}
	if _, _, err := AgentctlFetch(filepath.Join(dir, "missing"), time.Second)(ctx, "ok.prod"); err == nil {
		t.Error("missing binary: want an error")
	}
}
