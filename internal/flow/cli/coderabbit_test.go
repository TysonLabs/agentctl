package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/coderabbit"
	"github.com/TysonLabs/agentctl/internal/flow/namedlock"
)

func TestCodeRabbitUsageErrors(t *testing.T) {
	t.Setenv("AGENTFLOW_CODERABBIT", "/nonexistent/coderabbit") // never run: each case fails first
	t.Setenv("AGENTFLOW_GH", "/nonexistent/gh")                 // no PR hint
	dir := t.TempDir()
	cases := []struct {
		args []string
		want string
	}{
		{[]string{"coderabbit", "--base", "a", "--uncommitted"}, "mutually exclusive"},
		{[]string{"coderabbit", "--base", "a", "--base-commit", "abc1234"}, "mutually exclusive"},
		{[]string{"coderabbit", "--base", "--deep"}, "must not start with '-'"},
		{[]string{"coderabbit", "--base-commit", "HEAD~1"}, "not a 7-40 character hex commit"},
		{[]string{"coderabbit", "--timeout", "0s"}, "--timeout must be positive"},
		{[]string{"coderabbit", "--stall", "-1s"}, "--stall must not be negative"},
		{[]string{"coderabbit", "--lock-timeout", "0s"}, "--lock-timeout must be positive"},
		{[]string{"coderabbit", "--rate-limit-retries", "-1"}, "--rate-limit-retries must not be negative"},
		{[]string{"coderabbit", "--rate-limit-max-wait", "-1s"}, "--rate-limit-max-wait must not be negative"},
		{[]string{"coderabbit", "--config", filepath.Join(dir, "missing.md"), "--dir", dir}, "is not a readable file"},
		{[]string{"coderabbit", "--config", dir, "--dir", dir}, "is not a readable file"},
		{[]string{"coderabbit", "--dir", filepath.Join(dir, "nope")}, "is not a directory"},
		{[]string{"coderabbit", "--dir", dir, "--uncommitted"}, "not inside a git repository"},
		{[]string{"coderabbit", "--dir", dir}, "a review scope is required"},
		{[]string{"coderabbit", "--dir", dir, "--base", "origin/main"}, "is not a commit in"},
		{[]string{"coderabbit", "extra"}, "unexpected argument"},
		{[]string{"coderabbit", "--bogus"}, "flag provided but not defined"},
	}
	for _, c := range cases {
		var out, errb bytes.Buffer
		if code := Run(c.args, &out, &errb); code != 1 || !strings.Contains(errb.String(), c.want) {
			t.Errorf("%v: exit %d, stderr %q; want 1 and %q", c.args, code, errb.String(), c.want)
		}
	}
}

func TestCodeRabbitHelp(t *testing.T) {
	for _, args := range [][]string{{"coderabbit", "--help"}, {"coderabbit", "-h"}} {
		var out, errb bytes.Buffer
		if code := Run(args, &out, &errb); code != 0 || !strings.Contains(out.String(), "untrusted review data") {
			t.Errorf("%v: exit %d, stdout %q", args, code, out.String())
		}
	}
	var out, errb bytes.Buffer
	Run([]string{"--help"}, &out, &errb)
	if !strings.Contains(out.String(), "agentflow coderabbit") {
		t.Error("top-level usage does not list coderabbit")
	}
}

func TestCodeRabbitExitCodesCoverEveryStatus(t *testing.T) {
	all := []coderabbit.Status{coderabbit.StatusClean, coderabbit.StatusFindings, coderabbit.StatusFailed,
		coderabbit.StatusNoResult, coderabbit.StatusRateLimited, coderabbit.StatusTimeout,
		coderabbit.StatusStalled, coderabbit.StatusInterrupted, coderabbit.StatusLockBusy, coderabbit.StatusLockTimeout}
	seen := map[int]coderabbit.Status{}
	for _, s := range all {
		code, ok := coderabbitExitCodes[s]
		if !ok {
			t.Errorf("no exit code for %s", s)
		}
		if prev, dup := seen[code]; dup {
			t.Errorf("exit %d used by %s and %s", code, prev, s)
		}
		seen[code] = s
	}
	if len(coderabbitExitCodes) != len(all) {
		t.Errorf("coderabbitExitCodes has %d entries, want %d", len(coderabbitExitCodes), len(all))
	}
}

// crRepo is a git repo whose origin/HEAD points at origin/trunk, and a fake
// coderabbit script that records its argv and prints a clean review.
func crRepo(t *testing.T) (repo, rec string) {
	t.Helper()
	repo, rec = t.TempDir(), t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "trunk"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"update-ref", "refs/remotes/origin/trunk", "HEAD"},
		{"symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	script := filepath.Join(rec, "coderabbit")
	body := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"" + filepath.Join(rec, "args.txt") + "\"\npwd > \"" + filepath.Join(rec, "pwd.txt") + "\"\n" +
		`echo '{"type":"complete","status":"review_completed","findings":0,"reviewedFiles":[],"outcome":"completed"}'` + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_CODERABBIT", script)
	// Hermetic: no real gh, and no use-credits switch from the caller's env.
	t.Setenv("AGENTFLOW_GH", filepath.Join(rec, "no-gh"))
	t.Setenv("AGENTFLOW_CR_USE_CREDITS", "")
	return repo, rec
}

func TestCodeRabbitBaseScopeAndConfigPaths(t *testing.T) {
	repo, rec := crRepo(t)
	// --config is relative to the invocation cwd, not to --dir.
	cwd := t.TempDir()
	if err := os.WriteFile(filepath.Join(cwd, "AGENTS.md"), []byte("rules\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	t.Chdir(cwd)
	var out, errb bytes.Buffer
	code := Run([]string{"coderabbit", "--dir", repo, "--base", "origin/trunk", "--deep", "--config", "AGENTS.md", "--out", filepath.Join(t.TempDir(), "o")}, &out, &errb)
	if code != 0 {
		t.Fatalf("exit %d, stderr %q, stdout %s", code, errb.String(), out.String())
	}
	args, _ := os.ReadFile(filepath.Join(rec, "args.txt"))
	want := "review\n--agent\n--deep\n--committed\n--base\norigin/trunk\n-c\n" + filepath.Join(cwd, "AGENTS.md") + "\n"
	// macOS temp dirs resolve through /private; compare without it.
	if strings.ReplaceAll(string(args), "/private", "") != strings.ReplaceAll(want, "/private", "") {
		t.Fatalf("argv = %q, want %q", args, want)
	}
	pwd, _ := os.ReadFile(filepath.Join(rec, "pwd.txt"))
	if strings.ReplaceAll(strings.TrimSpace(string(pwd)), "/private", "") != strings.ReplaceAll(repo, "/private", "") {
		t.Fatalf("ran in %q, want %q", pwd, repo)
	}
	var res coderabbit.Result
	if err := json.Unmarshal(out.Bytes(), &res); err != nil || res.Status != coderabbit.StatusClean {
		t.Fatalf("result %s: %v", out.String(), err)
	}
}

func readResult(t *testing.T, b []byte) coderabbit.Result {
	t.Helper()
	var res coderabbit.Result
	if err := json.Unmarshal(b, &res); err != nil {
		t.Fatalf("result %s: %v", b, err)
	}
	return res
}

func TestCodeRabbitNoWaitLockRefusesWhileRepoLocked(t *testing.T) {
	repo, rec := crRepo(t)
	unlock, _, err := coderabbit.LockRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	outDir := filepath.Join(t.TempDir(), "o")
	var out, errb bytes.Buffer
	if code := Run([]string{"coderabbit", "--dir", repo, "--base", "origin/trunk", "--no-wait-lock", "--out", outDir}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "another agentflow coderabbit run") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(rec, "args.txt")); err == nil {
		t.Fatal("coderabbit ran despite the lock")
	}
	res := readResult(t, out.Bytes())
	if res.Status != coderabbit.StatusLockBusy || res.Attempts != 0 || res.LockHolder == nil || res.LockHolder.PID != os.Getpid() {
		t.Fatalf("result %+v", res)
	}
	if saved, err := os.ReadFile(filepath.Join(outDir, "result.json")); err != nil || readResult(t, saved).Status != coderabbit.StatusLockBusy {
		t.Fatalf("result.json: %v", err)
	}
}

// signalWriter collects writes and closes hit once a write contains want.
type signalWriter struct {
	mu   sync.Mutex
	buf  bytes.Buffer
	want string
	hit  chan struct{}
	once sync.Once
}

func (w *signalWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.buf.Write(p)
	if strings.Contains(w.buf.String(), w.want) {
		w.once.Do(func() { close(w.hit) })
	}
	return len(p), nil
}

func (w *signalWriter) String() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.buf.String()
}

func TestCodeRabbitWaitsForLockThenRuns(t *testing.T) {
	repo, rec := crRepo(t)
	unlock, _, err := coderabbit.WaitRepo(context.Background(), repo, namedlock.NewInfo("holder", repo), nil)
	if err != nil {
		t.Fatal(err)
	}
	errw := &signalWriter{want: fmt.Sprintf("held by pid %d", os.Getpid()), hit: make(chan struct{})}
	var out bytes.Buffer
	done := make(chan int, 1)
	go func() {
		done <- runCodeRabbit(context.Background(), []string{"--dir", repo, "--base", "origin/trunk", "--out", filepath.Join(t.TempDir(), "o")}, &out, errw)
	}()
	select {
	case <-errw.hit:
	case code := <-done:
		unlock()
		t.Fatalf("returned %d without waiting; stderr %q", code, errw.String())
	case <-time.After(10 * time.Second):
		unlock()
		t.Fatalf("no waiting line; stderr %q", errw.String())
	}
	if !strings.Contains(errw.String(), "waiting up to 1h0m0s for the repository lock") {
		t.Fatalf("waiting line %q", errw.String())
	}
	if _, err := os.Stat(filepath.Join(rec, "args.txt")); err == nil {
		t.Fatal("coderabbit ran while the lock was held")
	}
	unlock()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d, stderr %q", code, errw.String())
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the queued run did not start after the lock was released")
	}
	res := readResult(t, out.Bytes())
	if res.Status != coderabbit.StatusClean || res.Attempts != 1 || res.LockWaitS <= 0 || res.WaitedS != res.LockWaitS+res.RateLimitWaitS {
		t.Fatalf("result %+v", res)
	}
}

func TestCodeRabbitLockTimeout(t *testing.T) {
	repo, rec := crRepo(t)
	unlock, _, err := coderabbit.WaitRepo(context.Background(), repo, namedlock.NewInfo("holder", repo), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var out, errb bytes.Buffer
	code := Run([]string{"coderabbit", "--dir", repo, "--base", "origin/trunk", "--lock-timeout", "200ms", "--out", filepath.Join(t.TempDir(), "o")}, &out, &errb)
	if code != 6 {
		t.Fatalf("exit %d, stderr %q; want 6", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(rec, "args.txt")); err == nil {
		t.Fatal("coderabbit ran after the lock timeout")
	}
	res := readResult(t, out.Bytes())
	if res.Status != coderabbit.StatusLockTimeout || res.Attempts != 0 || res.LockWaitS < 0.2 || res.WaitedS != res.LockWaitS ||
		res.LockHolder == nil || res.LockHolder.PID != os.Getpid() || res.LockHolder.Cmd != "holder" {
		t.Fatalf("result %+v", res)
	}
}

// A holder record whose pid is dead, with no flock held, never blocks.
func TestCodeRabbitStaleHolderRecordDoesNotBlock(t *testing.T) {
	repo, rec := crRepo(t)
	_, infoPath, err := coderabbit.RepoLockPaths(repo)
	if err != nil {
		t.Fatal(err)
	}
	dead := exec.Command("true")
	if err := dead.Run(); err != nil {
		t.Fatal(err)
	}
	stale := fmt.Sprintf(`{"pid":%d,"cmd":"agentflow coderabbit","dir":"/gone","since":"2026-10-10T00:00:00Z"}`, dead.Process.Pid)
	if err := os.WriteFile(infoPath, []byte(stale), 0o644); err != nil {
		t.Fatal(err)
	}
	var out, errb bytes.Buffer
	if code := Run([]string{"coderabbit", "--dir", repo, "--base", "origin/trunk", "--lock-timeout", "5s", "--out", filepath.Join(t.TempDir(), "o")}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if strings.Contains(errb.String(), "waiting") {
		t.Fatalf("waited on a stale record: %q", errb.String())
	}
	if _, err := os.Stat(filepath.Join(rec, "args.txt")); err != nil {
		t.Fatal("coderabbit did not run")
	}
}

func TestCodeRabbitUseCreditsPassthrough(t *testing.T) {
	for _, c := range []struct {
		name string
		args []string
		env  string
		want bool
	}{
		{"default off", nil, "", false},
		{"flag", []string{"--use-credits"}, "", true},
		{"env", nil, "1", true},
		{"flag overrides env", []string{"--use-credits=false"}, "1", false},
	} {
		t.Run(c.name, func(t *testing.T) {
			repo, rec := crRepo(t)
			t.Setenv("AGENTFLOW_CR_USE_CREDITS", c.env)
			var out, errb bytes.Buffer
			args := append([]string{"coderabbit", "--dir", repo, "--base", "origin/trunk", "--out", filepath.Join(t.TempDir(), "o")}, c.args...)
			if code := Run(args, &out, &errb); code != 0 {
				t.Fatalf("exit %d, stderr %q", code, errb.String())
			}
			argv, _ := os.ReadFile(filepath.Join(rec, "args.txt"))
			if got := strings.Contains(string(argv), "--use-credits\n"); got != c.want {
				t.Fatalf("argv %q: --use-credits present = %v, want %v", argv, got, c.want)
			}
		})
	}
}

func TestCodeRabbitStartFailureWritesResult(t *testing.T) {
	repo, _ := crRepo(t)
	t.Setenv("AGENTFLOW_CODERABBIT", filepath.Join(t.TempDir(), "missing-coderabbit"))
	outDir := filepath.Join(t.TempDir(), "out")
	var out, errb bytes.Buffer
	if code := Run([]string{"coderabbit", "--dir", repo, "--base", "origin/trunk", "--out", outDir}, &out, &errb); code != 3 {
		t.Fatalf("exit %d, stderr %q; want failed exit 3", code, errb.String())
	}
	res := readResult(t, out.Bytes())
	if res.Status != coderabbit.StatusFailed || res.Attempts != 0 || !strings.Contains(res.Error, "starting") {
		t.Fatalf("result %+v", res)
	}
	if !strings.Contains(out.String(), `"retry_after_s": null`) {
		t.Fatalf("result omits null retry_after_s: %s", out.String())
	}
	saved, err := os.ReadFile(filepath.Join(outDir, "result.json"))
	if err != nil || readResult(t, saved).Status != coderabbit.StatusFailed {
		t.Fatalf("result.json: %v", err)
	}
}

func TestCodeRabbitRateLimitRetriedThroughCLI(t *testing.T) {
	repo, rec := crRepo(t)
	// Replace the fake: the first run is rate-limited, the second is clean.
	script := filepath.Join(rec, "coderabbit")
	body := "#!/bin/sh\nn=$(cat \"" + filepath.Join(rec, "n") + "\" 2>/dev/null || echo 0)\nn=$((n+1))\necho $n > \"" + filepath.Join(rec, "n") + "\"\n" +
		"if [ $n -eq 1 ]; then\n" +
		`echo '{"type":"error","errorType":"rate_limit","message":"Rate limit exceeded","recoverable":true,"metadata":{"isProUser":false,"waitTime":"3 minutes and 2 seconds"}}'` + "\nexit 1\nfi\n" +
		`echo '{"type":"complete","status":"review_completed","findings":0,"reviewedFiles":[],"outcome":"completed"}'` + "\n"
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	crHooks.sleep = func(_ context.Context, d time.Duration) error { waits = append(waits, d); return nil }
	crHooks.jitter = func() time.Duration { return 0 }
	t.Cleanup(func() { crHooks.sleep, crHooks.jitter = nil, nil })

	var out, errb bytes.Buffer
	if code := Run([]string{"coderabbit", "--dir", repo, "--base", "origin/trunk", "--out", filepath.Join(t.TempDir(), "o")}, &out, &errb); code != 0 {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if len(waits) != 1 || waits[0] != 3*time.Minute+2*time.Second {
		t.Fatalf("waits %v", waits)
	}
	if !strings.Contains(errb.String(), "rate limited (attempt 1 of 2") {
		t.Fatalf("no wait log: %q", errb.String())
	}
	res := readResult(t, out.Bytes())
	if res.Status != coderabbit.StatusClean || res.Attempts != 2 || res.RateLimitWaitS != 182 || res.WaitedS < 182 {
		t.Fatalf("result %+v", res)
	}

	// --rate-limit-retries 0: exit 5 at once, with the wait in the JSON.
	if err := os.Remove(filepath.Join(rec, "n")); err != nil {
		t.Fatal(err)
	}
	waits = nil
	out.Reset()
	errb.Reset()
	if code := Run([]string{"coderabbit", "--dir", repo, "--base", "origin/trunk", "--rate-limit-retries", "0", "--out", filepath.Join(t.TempDir(), "o")}, &out, &errb); code != 5 {
		t.Fatalf("exit %d, stderr %q; want 5", code, errb.String())
	}
	res = readResult(t, out.Bytes())
	if len(waits) != 0 || res.Status != coderabbit.StatusRateLimited || res.Attempts != 1 || res.RetryAfterS == nil || *res.RetryAfterS != 182 || res.RateLimitWait != "3 minutes and 2 seconds" {
		t.Fatalf("waits %v, result %+v", waits, res)
	}
}

// fakeGHPR installs a gh script that prints body and exits with code.
func fakeGHPR(t *testing.T, body string, code int) (calls string) {
	t.Helper()
	d := t.TempDir()
	gh, calls := filepath.Join(d, "gh"), filepath.Join(d, "calls")
	script := "#!/bin/sh\nprintf '%s\\n' \"$*\" >> \"" + calls + "\"\ncat <<'EOF'\n" + body + "\nEOF\nexit " + fmt.Sprint(code) + "\n"
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_GH", gh)
	return calls
}

// TestCodeRabbitRequiresScope: without --base/--base-commit/--uncommitted
// the run refuses (exit 1, stderr only) before it takes the lock, writes no
// result and never starts CodeRabbit. It never falls back to origin/HEAD.
func TestCodeRabbitRequiresScope(t *testing.T) {
	for _, c := range []struct {
		name, ghBody string
		ghExit       int
		want, not    string
	}{
		{"open PR names its base", `{"baseRefName":"development","number":123,"state":"OPEN"}`, 0,
			"--base is required; the open PR #123 targets development: pass --base origin/development", ""},
		{"stacked PR names the feature branch", `{"baseRefName":"feat/parent","number":7,"state":"OPEN"}`, 0,
			"the open PR #7 targets feat/parent: pass --base origin/feat/parent", ""},
		{"closed PR gives no hint", `{"baseRefName":"development","number":9,"state":"CLOSED"}`, 0,
			"a review scope is required", "#9"},
		{"merged PR gives no hint", `{"baseRefName":"development","number":9,"state":"MERGED"}`, 0,
			"a review scope is required", "#9"},
		{"gh fails (no PR, unauthenticated)", `no pull requests found for branch "trunk"`, 1,
			"a review scope is required", "PR #"},
		{"gh prints garbage", `not json`, 0, "a review scope is required", "PR #"},
	} {
		t.Run(c.name, func(t *testing.T) {
			repo, rec := crRepo(t)
			calls := fakeGHPR(t, c.ghBody, c.ghExit)
			outDir := filepath.Join(t.TempDir(), "o")
			var out, errb bytes.Buffer
			code := Run([]string{"coderabbit", "--dir", repo, "--deep", "--out", outDir}, &out, &errb)
			if code != 1 || !strings.Contains(errb.String(), c.want) || (c.not != "" && strings.Contains(errb.String(), c.not)) {
				t.Fatalf("exit %d, stderr %q; want 1 with %q", code, errb.String(), c.want)
			}
			if out.Len() != 0 {
				t.Fatalf("stdout %q; a usage error writes no result", out.String())
			}
			if _, err := os.Stat(outDir); !os.IsNotExist(err) {
				t.Fatalf("out dir created: %v", err)
			}
			if _, err := os.Stat(filepath.Join(rec, "args.txt")); !os.IsNotExist(err) {
				t.Fatal("coderabbit ran without a scope")
			}
			if got, _ := os.ReadFile(calls); strings.TrimSpace(string(got)) != "pr view --json baseRefName,number,state" {
				t.Fatalf("gh calls %q", got)
			}
		})
	}
}

func TestCodeRabbitRequiresScopeWhenGHMissing(t *testing.T) {
	repo, _ := crRepo(t)
	t.Setenv("AGENTFLOW_GH", filepath.Join(t.TempDir(), "no-gh"))
	var out, errb bytes.Buffer
	if code := Run([]string{"coderabbit", "--dir", repo}, &out, &errb); code != 1 || !strings.Contains(errb.String(), "a review scope is required") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}

// A hung gh is cut off by its bounded timeout: plain error, no hang.
func TestCodeRabbitRequiresScopeWhenGHHangs(t *testing.T) {
	repo, _ := crRepo(t)
	d := t.TempDir()
	gh := filepath.Join(d, "gh")
	if err := os.WriteFile(gh, []byte("#!/bin/sh\nexec sleep 30\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_GH", gh)
	old := ghPRTimeout
	ghPRTimeout = 200 * time.Millisecond
	defer func() { ghPRTimeout = old }()
	start := time.Now()
	var out, errb bytes.Buffer
	code := Run([]string{"coderabbit", "--dir", repo}, &out, &errb)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("took %s; the gh lookup is not bounded", elapsed)
	}
	if code != 1 || !strings.Contains(errb.String(), "a review scope is required") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}

// An explicit --base that names no commit refuses before the lock; it is
// never swapped for another ref.
func TestCodeRabbitBaseMustExist(t *testing.T) {
	repo, rec := crRepo(t)
	outDir := filepath.Join(t.TempDir(), "o")
	var out, errb bytes.Buffer
	code := Run([]string{"coderabbit", "--dir", repo, "--base", "origin/development", "--out", outDir}, &out, &errb)
	if code != 1 || !strings.Contains(errb.String(), `--base "origin/development" is not a commit`) {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
	if _, err := os.Stat(filepath.Join(rec, "args.txt")); !os.IsNotExist(err) {
		t.Fatal("coderabbit ran with a missing base")
	}
}

// The requested base is in the result even when CodeRabbit never ran.
func TestCodeRabbitResultRecordsBase(t *testing.T) {
	repo, _ := crRepo(t)
	unlock, _, err := coderabbit.LockRepo(repo)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	var out, errb bytes.Buffer
	Run([]string{"coderabbit", "--dir", repo, "--base", "origin/trunk", "--no-wait-lock", "--out", filepath.Join(t.TempDir(), "o")}, &out, &errb)
	if res := readResult(t, out.Bytes()); res.Status != coderabbit.StatusLockBusy || res.Base != "origin/trunk" {
		t.Fatalf("result %+v", res)
	}
}
