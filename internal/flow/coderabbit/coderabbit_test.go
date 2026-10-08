package coderabbit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// The test binary doubles as a fake coderabbit: with FAKE_CR_MODE set it acts
// out one CLI behaviour. FAKE_CR_REC names a directory where it records its
// argv, its stdin and any child pid.
func TestMain(m *testing.M) {
	if mode := os.Getenv("FAKE_CR_MODE"); mode != "" {
		os.Exit(fakeCodeRabbit(mode))
	}
	os.Exit(m.Run())
}

const (
	ctxLine    = `{"type":"review_context","reviewType":"committed","currentBranch":"feat","baseBranch":"origin/main","baseCommit":"abc1234","workingDirectory":"/x"}`
	heartbeat  = `{"type":"heartbeat","status":"reviewing"}`
	findingOne = `{"type":"finding","severity":"major","fileName":"a.go","codegenInstructions":"Nil map write in Save.","suggestions":["use make"]}`
	findingTwo = `{"type":"finding","severity":"minor","fileName":"b.go","codegenInstructions":"Typo.","suggestions":[]}`
)

func complete(n int) string {
	return fmt.Sprintf(`{"type":"complete","status":"review_completed","findings":%d,"reviewedFiles":["a.go","b.go"],"outcome":"completed","message":"Review completed"}`, n)
}

func fakeCodeRabbit(mode string) int {
	rec := os.Getenv("FAKE_CR_REC")
	_ = os.WriteFile(filepath.Join(rec, "args.txt"), []byte(strings.Join(os.Args[1:], "\n")), 0o644)
	// Reading stdin to EOF is exactly what hangs when stdin is left open.
	in, _ := io.ReadAll(os.Stdin)
	_ = os.WriteFile(filepath.Join(rec, "stdin.txt"), in, 0o644)
	emit := func(s string) { fmt.Println(s) }
	emit(ctxLine)
	emit(`{"type":"status","phase":"connecting","status":"connecting_to_review_service"}`)
	switch mode {
	case "clean":
		emit("not json: progress bar")
		emit(heartbeat)
		emit(complete(0))
	case "findings":
		emit(findingOne)
		emit(heartbeat)
		emit(findingTwo)
		emit(complete(2))
	case "mismatch":
		emit(findingOne)
		emit(complete(2))
	case "no-count":
		emit(`{"type":"complete","status":"review_completed","outcome":"completed"}`)
	case "ratelimit-no-count":
		emit(`{"type":"status","phase":"reviewing","status":"error","message":"Rate limit exceeded"}`)
		emit(`{"type":"complete","status":"review_completed","outcome":"completed"}`)
	case "ratelimit-mismatch":
		emit(`{"type":"status","phase":"reviewing","status":"error","message":"Rate limit exceeded"}`)
		emit(findingOne)
		emit(complete(2))
	case "no-complete":
		emit(heartbeat)
	case "bad-outcome":
		emit(`{"type":"complete","status":"review_failed","findings":0,"outcome":"failed","message":"sandbox error"}`)
	case "fail":
		fmt.Fprintln(os.Stderr, "Error: not authenticated")
		return 2
	case "ratelimit":
		emit(`{"type":"status","phase":"connecting","status":"error","message":"Rate limit exceeded, try again in 12 minutes"}`)
		return 1
	case "ratelimit-on-success":
		emit(`{"type":"status","phase":"setup","status":"info","message":"Rate limit exceeded on a previous run"}`)
		emit(complete(0))
	case "error-event":
		emit(`{"type":"error","message":"review service unavailable"}`)
	case "silent":
		time.Sleep(time.Hour)
	case "chatty":
		for {
			emit(heartbeat)
			time.Sleep(10 * time.Millisecond)
		}
	case "slow-heartbeats":
		for range 3 {
			time.Sleep(time.Second)
			emit(heartbeat)
		}
		emit(complete(0))
	case "slow-stderr":
		for range 3 {
			time.Sleep(time.Second)
			fmt.Fprintln(os.Stderr, "still working")
		}
		emit(complete(0))
	case "child":
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "FAKE_CR_MODE=silent")
		_ = c.Start()
		_ = os.WriteFile(filepath.Join(rec, "child.pid"), []byte(strconv.Itoa(c.Process.Pid)), 0o644)
		time.Sleep(time.Hour)
	case "escaped-stderr":
		c := exec.Command(os.Args[0])
		c.Env = append(os.Environ(), "FAKE_CR_MODE=stderr-holder")
		c.Stderr = os.Stderr
		c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
		_ = c.Start()
		_ = os.WriteFile(filepath.Join(rec, "escaped.pid"), []byte(strconv.Itoa(c.Process.Pid)), 0o644)
		time.Sleep(time.Hour)
	case "stderr-holder":
		time.Sleep(time.Hour)
	default:
		fmt.Fprintln(os.Stderr, "unknown fake mode "+mode)
		return 99
	}
	return 0
}

func run(t *testing.T, mode string, tweak func(*Options)) (Result, string) {
	t.Helper()
	rec := t.TempDir()
	t.Setenv("FAKE_CR_MODE", mode)
	t.Setenv("FAKE_CR_REC", rec)
	o := Options{Bin: os.Args[0], Dir: t.TempDir(), Scope: Scope{Base: "origin/main"}, Timeout: 10 * time.Second,
		Stall: 5 * time.Second, OutDir: t.TempDir(), Poll: 10 * time.Millisecond, Grace: 200 * time.Millisecond}
	if tweak != nil {
		tweak(&o)
	}
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatal(err)
	}
	return res, rec
}

func TestClean(t *testing.T) {
	parentStdin, err := os.CreateTemp(t.TempDir(), "parent-stdin")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := parentStdin.WriteString("must not reach coderabbit\n"); err != nil {
		t.Fatal(err)
	}
	if _, err := parentStdin.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	originalStdin := os.Stdin
	os.Stdin = parentStdin
	t.Cleanup(func() {
		os.Stdin = originalStdin
		_ = parentStdin.Close()
	})

	res, rec := run(t, "clean", nil)
	if res.Status != StatusClean || res.Exit == nil || *res.Exit != 0 || res.Error != "" {
		t.Fatalf("got %+v", res)
	}
	if res.Base != "origin/main" || res.BaseCommit != "abc1234" || res.ReviewType != "committed" || len(res.ReviewedFiles) != 2 {
		t.Fatalf("context not carried: %+v", res)
	}
	if res.Reported == nil || *res.Reported != 0 {
		t.Fatalf("reported = %v", res.Reported)
	}
	// stdin is /dev/null: the fake read it to EOF and got nothing.
	if in, err := os.ReadFile(filepath.Join(rec, "stdin.txt")); err != nil || len(in) != 0 {
		t.Fatalf("stdin = %q, %v", in, err)
	}
	raw, _ := os.ReadFile(res.Events)
	if !strings.Contains(string(raw), "not json: progress bar") {
		t.Fatalf("raw log lost the non-JSON line: %s", raw)
	}
}

func TestFindings(t *testing.T) {
	res, _ := run(t, "findings", nil)
	if res.Status != StatusFindings || len(res.Findings) != 2 {
		t.Fatalf("got %+v", res)
	}
	f := res.Findings[0]
	if f.Severity != "major" || f.File != "a.go" || f.Text != "Nil map write in Save." || string(f.Suggestions) != `["use make"]` {
		t.Fatalf("finding = %+v", f)
	}
	if !reflect.DeepEqual(res.Severities, map[string]int{"major": 1, "minor": 1}) {
		t.Fatalf("severities = %v", res.Severities)
	}
}

func TestNotAResult(t *testing.T) {
	for mode, want := range map[string]string{
		"mismatch":    "reports 2 findings but the stream carried 1",
		"no-count":    "no findings count",
		"no-complete": "without a complete event",
	} {
		res, _ := run(t, mode, nil)
		if res.Status != StatusNoResult || !strings.Contains(res.Error, want) {
			t.Errorf("%s: got %s %q, want no_result %q", mode, res.Status, res.Error, want)
		}
	}
}

func TestFailures(t *testing.T) {
	cases := []struct {
		mode   string
		status Status
		want   string
	}{
		{"fail", StatusFailed, "not authenticated"},
		{"bad-outcome", StatusFailed, "sandbox error"},
		{"error-event", StatusFailed, "review service unavailable"},
		{"ratelimit", StatusRateLimited, "coderabbit exited 1"},
	}
	for _, c := range cases {
		res, _ := run(t, c.mode, nil)
		if res.Status != c.status || !strings.Contains(res.Error, c.want) {
			t.Errorf("%s: got %s %q, want %s %q", c.mode, res.Status, res.Error, c.status, c.want)
		}
	}
}

// A rate-limit message names the outcome only when there was no result.
func TestRateLimitTextDoesNotOverrideAResult(t *testing.T) {
	if res, _ := run(t, "ratelimit-on-success", nil); res.Status != StatusClean {
		t.Fatalf("got %s %q", res.Status, res.Error)
	}
}

func TestRateLimitTextOverridesEveryNoResult(t *testing.T) {
	for mode, reason := range map[string]string{
		"ratelimit-no-count": "no findings count",
		"ratelimit-mismatch": "reports 2 findings but the stream carried 1",
	} {
		res, _ := run(t, mode, nil)
		if res.Status != StatusRateLimited || !strings.Contains(res.Error, reason) {
			t.Errorf("%s: got %s %q, want rate_limited with reason %q", mode, res.Status, res.Error, reason)
		}
	}
}

func TestStallKillsSilentRun(t *testing.T) {
	res, _ := run(t, "silent", func(o *Options) { o.Stall = 150 * time.Millisecond })
	if res.Status != StatusStalled || res.Exit != nil {
		t.Fatalf("got %+v", res)
	}
}

func TestHeartbeatsResetStall(t *testing.T) {
	// 3 heartbeats 1s apart run 3s in total, twice the 1.5s stall window; the
	// window leaves room for a slow (race-instrumented) start.
	res, _ := run(t, "slow-heartbeats", func(o *Options) { o.Stall = 1500 * time.Millisecond })
	if res.Status != StatusClean {
		t.Fatalf("got %s %q", res.Status, res.Error)
	}
}

func TestStderrResetsStall(t *testing.T) {
	res, _ := run(t, "slow-stderr", func(o *Options) { o.Stall = 1500 * time.Millisecond })
	if res.Status != StatusClean {
		t.Fatalf("got %s %q", res.Status, res.Error)
	}
}

func TestTimeoutKillsWholeProcessGroup(t *testing.T) {
	// 2s leaves a slow start room to spawn the child before the kill.
	res, rec := run(t, "child", func(o *Options) { o.Timeout = 2 * time.Second; o.Stall = 0 })
	if res.Status != StatusTimeout {
		t.Fatalf("got %+v", res)
	}
	b, err := os.ReadFile(filepath.Join(rec, "child.pid"))
	if err != nil {
		t.Fatal(err)
	}
	pid, _ := strconv.Atoi(string(b))
	assertDead(t, pid)
}

func TestTimeoutOfChattyRun(t *testing.T) {
	if res, _ := run(t, "chatty", func(o *Options) { o.Timeout = 200 * time.Millisecond }); res.Status != StatusTimeout {
		t.Fatalf("got %+v", res)
	}
}

func TestEscapedStderrHolderCannotBlockTimeout(t *testing.T) {
	rec := t.TempDir()
	t.Setenv("FAKE_CR_MODE", "escaped-stderr")
	t.Setenv("FAKE_CR_REC", rec)
	type outcome struct {
		res Result
		err error
	}
	dir, out := t.TempDir(), t.TempDir()
	done := make(chan outcome, 1)
	go func() {
		res, err := Run(context.Background(), Options{
			Bin: os.Args[0], Dir: dir, Scope: Scope{Base: "origin/main"},
			Timeout: 500 * time.Millisecond, Stall: 0, OutDir: out,
			Poll: 10 * time.Millisecond, Grace: 100 * time.Millisecond,
		})
		done <- outcome{res: res, err: err}
	}()

	killEscaped := func() {
		b, err := os.ReadFile(filepath.Join(rec, "escaped.pid"))
		if err != nil {
			return
		}
		pid, err := strconv.Atoi(string(b))
		if err != nil || pid <= 0 {
			return
		}
		_ = syscall.Kill(pid, syscall.SIGKILL)
	}
	defer killEscaped()

	select {
	case got := <-done:
		if got.err != nil || got.res.Status != StatusTimeout {
			t.Fatalf("got %+v, %v", got.res, got.err)
		}
	case <-time.After(3 * time.Second):
		killEscaped() // release the old os/exec stderr-copy wait before failing
		select {
		case <-done:
		case <-time.After(time.Second):
		}
		t.Fatal("Run hung after its timeout because an escaped helper held stderr open")
	}
}

func TestCancelInterrupts(t *testing.T) {
	rec := t.TempDir()
	t.Setenv("FAKE_CR_MODE", "silent")
	t.Setenv("FAKE_CR_REC", rec)
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(150*time.Millisecond, cancel)
	res, err := Run(ctx, Options{Bin: os.Args[0], Dir: t.TempDir(), Scope: Scope{Base: "x"}, Timeout: 10 * time.Second, OutDir: t.TempDir(), Poll: 10 * time.Millisecond, Grace: 200 * time.Millisecond})
	if err != nil || res.Status != StatusInterrupted {
		t.Fatalf("got %+v, %v", res, err)
	}
}

func TestMissingBinaryIsAStartError(t *testing.T) {
	_, err := Run(context.Background(), Options{Bin: "/nonexistent/coderabbit", Dir: t.TempDir(), Scope: Scope{Base: "x"}, Timeout: time.Second, OutDir: t.TempDir()})
	if err == nil || !strings.Contains(err.Error(), "starting") {
		t.Fatalf("err = %v", err)
	}
}

func TestArgs(t *testing.T) {
	cases := []struct {
		o    Options
		want string
	}{
		{Options{Scope: Scope{Base: "origin/main"}}, "review --agent --committed --base origin/main"},
		{Options{Deep: true, Scope: Scope{BaseCommit: "abc1234"}}, "review --agent --deep --committed --base-commit abc1234"},
		{Options{Scope: Scope{Uncommitted: true}, Configs: []string{"/a/AGENTS.md", "/b/x.md"}}, "review --agent --uncommitted -c /a/AGENTS.md /b/x.md"},
	}
	for _, c := range cases {
		if got := strings.Join(Args(c.o), " "); got != c.want {
			t.Errorf("Args = %q, want %q", got, c.want)
		}
	}
}

func gitRepo(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	for _, args := range [][]string{
		{"init", "-q", "-b", "trunk"},
		{"-c", "user.email=t@example.com", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "init"},
		{"update-ref", "refs/remotes/origin/trunk", "HEAD"},
	} {
		if out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v %s", args, err, out)
		}
	}
	return dir
}

func TestDefaultBaseFromOriginHEAD(t *testing.T) {
	dir := gitRepo(t)
	if out, err := exec.Command("git", "-C", dir, "symbolic-ref", "refs/remotes/origin/HEAD", "refs/remotes/origin/trunk").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	got, err := DefaultBase(dir, func(string) (string, error) { return "", errors.New("must not be asked") })
	if err != nil || got != "origin/trunk" {
		t.Fatalf("got %q, %v", got, err)
	}
}

func TestDefaultBaseFallsBackToLookup(t *testing.T) {
	dir := gitRepo(t)
	got, err := DefaultBase(dir, func(string) (string, error) { return "trunk", nil })
	if err != nil || got != "origin/trunk" {
		t.Fatalf("got %q, %v", got, err)
	}
	if _, err := DefaultBase(dir, func(string) (string, error) { return "", errors.New("no gh") }); err == nil {
		t.Fatal("a failed lookup must refuse, not guess")
	}
	if _, err := DefaultBase(dir, func(string) (string, error) { return "develop", nil }); err == nil || !strings.Contains(err.Error(), "no local or origin ref") {
		t.Fatalf("missing ref: err = %v", err)
	}
	if _, err := DefaultBase(dir, nil); err == nil {
		t.Fatal("no origin/HEAD and no lookup must refuse")
	}
}

func TestLockRepoCoversWorktrees(t *testing.T) {
	dir := gitRepo(t)
	wt := filepath.Join(t.TempDir(), "wt")
	if out, err := exec.Command("git", "-C", dir, "worktree", "add", "-q", "-b", "side", wt).CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	unlock, _, err := LockRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := LockRepo(wt); err == nil || !strings.Contains(err.Error(), "another agentflow coderabbit run") {
		t.Fatalf("a worktree of the same repo must share the lock: %v", err)
	}
	unlock()
	unlock2, _, err := LockRepo(wt)
	if err != nil {
		t.Fatalf("lock not released: %v", err)
	}
	unlock2()
	if _, _, err := LockRepo(t.TempDir()); err == nil {
		t.Fatal("a non-repo dir must be refused")
	}
}

func assertDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		if out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output(); strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("child %d of the coderabbit process group survived", pid)
}
