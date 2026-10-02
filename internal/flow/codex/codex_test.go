package codex

import (
	"context"
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

// The test binary doubles as a fake codex: with FAKE_CODEX_MODE set it acts
// out one codex behaviour instead of running tests. FAKE_CODEX_REC names a
// directory where it records its argv, its stdin and any child pid.
func TestMain(m *testing.M) {
	switch mode := os.Getenv("FAKE_CODEX_MODE"); mode {
	case "":
		os.Exit(m.Run())
	case "sleeper":
		time.Sleep(time.Hour)
		os.Exit(0)
	default:
		os.Exit(fakeCodex(mode))
	}
}

func fakeCodex(mode string) int {
	rec := os.Getenv("FAKE_CODEX_REC")
	args := os.Args[1:]
	_ = os.WriteFile(filepath.Join(rec, "args.txt"), []byte(strings.Join(args, "\n")), 0o644)
	// Reading stdin to EOF is exactly what hangs when stdin is left open.
	in, _ := io.ReadAll(os.Stdin)
	_ = os.WriteFile(filepath.Join(rec, "stdin.txt"), in, 0o644)

	final := ""
	for i, a := range args {
		if a == "-o" && i+1 < len(args) {
			final = args[i+1]
		}
	}
	emit := func(s string) { fmt.Println(s) }
	thread := "thread-" + mode
	emit(`{"type":"thread.started","thread_id":"` + thread + `"}`)
	emit(`{"type":"turn.started"}`)
	switch mode {
	case "ok":
		_ = os.WriteFile(final, []byte("no findings\n"), 0o644)
		emit(`{"type":"turn.completed","usage":{"input_tokens":10,"cached_input_tokens":4,"output_tokens":2}}`)
		return 0
	case "empty":
		emit(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":0}}`)
		return 0
	case "whitespace":
		_ = os.WriteFile(final, []byte("  \n\n"), 0o644)
		return 0
	case "fail":
		emit(`{"type":"turn.failed","error":{"message":"model refused"}}`)
		return 1
	case "fail-exit0":
		_ = os.WriteFile(final, []byte("partial\n"), 0o644)
		emit(`{"type":"turn.failed","error":{"message":"stream disconnected"}}`)
		return 0
	case "ratelimit":
		emit(`{"type":"error","message":"You've hit your usage limit. Try again in 2 hours."}`)
		return 1
	case "ratelimit-stderr":
		fmt.Fprintln(os.Stderr, "HTTP 429: retry later")
		emit(`{"type":"turn.failed","error":{"message":"request failed"}}`)
		return 1
	case "stderr-only":
		fmt.Fprintln(os.Stderr, "Reading additional input from stdin...")
		fmt.Fprintln(os.Stderr, "Error: Not inside a trusted directory")
		return 1
	case "hang":
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "FAKE_CODEX_MODE=sleeper")
		_ = child.Start()
		_ = os.WriteFile(filepath.Join(rec, "child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		for {
			emit(`{"type":"item.completed","item":{"type":"command_execution"}}`)
			time.Sleep(100 * time.Millisecond)
		}
	case "silent":
		time.Sleep(time.Hour)
	case "chatty":
		for range 30 {
			time.Sleep(100 * time.Millisecond)
			emit(`{"type":"item.completed","item":{"type":"agent_message"}}`)
		}
		_ = os.WriteFile(final, []byte("done\n"), 0o644)
		emit(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
		return 0
	case "rollout":
		// No stdout for 3s, but the session log keeps growing.
		home := os.Getenv("FAKE_CODEX_HOME")
		day := filepath.Join(home, "sessions", "2026", "10", "02")
		_ = os.MkdirAll(day, 0o755)
		log := filepath.Join(day, "rollout-2026-10-02T00-00-00-"+thread+".jsonl")
		for i := range 30 {
			time.Sleep(100 * time.Millisecond)
			f, _ := os.OpenFile(log, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
			_, _ = fmt.Fprintln(f, i)
			_ = f.Close()
			// Size growth, rather than a trustworthy filesystem clock, must be
			// enough to reset the stall timer.
			past := time.Unix(1, 0)
			_ = os.Chtimes(log, past, past)
		}
		_ = os.WriteFile(final, []byte("done\n"), 0o644)
		emit(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
		return 0
	case "future-rollout":
		// A future-dated mtime must not suppress stall detection forever.
		home := os.Getenv("FAKE_CODEX_HOME")
		day := filepath.Join(home, "sessions", "2026", "10", "02")
		_ = os.MkdirAll(day, 0o755)
		log := filepath.Join(day, "rollout-2026-10-02T00-00-00-"+thread+".jsonl")
		_ = os.WriteFile(log, []byte("one event"), 0o644)
		future := time.Now().Add(time.Hour)
		_ = os.Chtimes(log, future, future)
		time.Sleep(time.Hour)
	case "leaky-exit":
		child := exec.Command(os.Args[0])
		child.Env = append(os.Environ(), "FAKE_CODEX_MODE=sleeper")
		child.Stdout = os.Stdout // keep the agentflow stdout pipe open
		if err := child.Start(); err != nil {
			return 98
		}
		_ = os.WriteFile(filepath.Join(rec, "child.pid"), []byte(strconv.Itoa(child.Process.Pid)), 0o644)
		_ = os.WriteFile(final, []byte("done\n"), 0o644)
		emit(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
		return 0
	case "final-without-completion":
		_ = os.WriteFile(final, []byte("partial answer\n"), 0o644)
		return 0
	case "midline":
		_ = os.WriteFile(final, []byte("done\n"), 0o644)
		fmt.Print(`{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}`)
		return 0
	}
	return 99
}

type harness struct {
	t   *testing.T
	rec string
	out string
}

func newHarness(t *testing.T, mode string) *harness {
	t.Helper()
	h := &harness{t: t, rec: t.TempDir(), out: t.TempDir()}
	t.Setenv("FAKE_CODEX_MODE", mode)
	t.Setenv("FAKE_CODEX_REC", h.rec)
	return h
}

func (h *harness) opts() Options {
	return Options{
		Bin:       os.Args[0],
		Dir:       h.t.TempDir(),
		Prompt:    "review this",
		Timeout:   20 * time.Second,
		Stall:     10 * time.Second,
		OutDir:    h.out,
		CodexHome: h.t.TempDir(),
		Poll:      50 * time.Millisecond,
		Grace:     500 * time.Millisecond,
	}
}

func (h *harness) recorded(name string) string {
	b, _ := os.ReadFile(filepath.Join(h.rec, name))
	return string(b)
}

func run(t *testing.T, o Options) Result {
	t.Helper()
	res, err := Run(context.Background(), o)
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	return res
}

func TestOKExecMode(t *testing.T) {
	h := newHarness(t, "ok")
	o := h.opts()
	res := run(t, o)
	if res.Status != StatusOK || res.CodexExit == nil || *res.CodexExit != 0 {
		t.Fatalf("got %+v", res)
	}
	if res.Mode != "exec" || res.ThreadID != "thread-ok" || res.Usage == nil || res.Usage.InputTokens != 10 {
		t.Errorf("summary not parsed: %+v", res)
	}
	if got := h.recorded("stdin.txt"); got != "review this" {
		t.Errorf("codex stdin = %q, want the prompt file", got)
	}
	args := h.recorded("args.txt")
	for _, want := range []string{"exec", "--json", `sandbox_mode="read-only"`, "-C\n" + o.Dir, "--skip-git-repo-check", "-"} {
		if !strings.Contains(args, want) {
			t.Errorf("argv missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "review") {
		t.Errorf("exec mode must not use the review subcommand:\n%s", args)
	}
	if b, _ := os.ReadFile(res.Final); string(b) != "no findings\n" {
		t.Errorf("final = %q", b)
	}
}

func TestBuildArgsForCodex0159(t *testing.T) {
	tests := []struct {
		name   string
		o      Options
		inRepo bool
		want   []string
	}{
		{
			name:   "exec",
			o:      Options{Dir: "/repo", Prompt: "p"},
			inRepo: true,
			want:   []string{"exec", "-C", "/repo", "--json", "-c", `sandbox_mode="read-only"`, "-o", "/out/final.md", "-"},
		},
		{
			name: "review",
			o: Options{
				Scope: Scope{Base: "main"},
				Write: true,
				Model: "gpt-test",
			},
			want: []string{"exec", "review", "--base", "main", "--json", "-c", `sandbox_mode="workspace-write"`, "-o", "/out/final.md", "--skip-git-repo-check", "-m", "gpt-test"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := buildArgs(tt.o, "/out/final.md", tt.inRepo); !reflect.DeepEqual(got, tt.want) {
				t.Fatalf("argv = %#v, want %#v", got, tt.want)
			}
		})
	}
}

func TestReviewModeClosesStdinAndPinsSandbox(t *testing.T) {
	h := newHarness(t, "ok")
	o := h.opts()
	o.Prompt = ""
	o.Scope = Scope{Base: "main"}
	o.Write = true
	o.Timeout = 5 * time.Second
	// The real hang: the caller's stdin is a pipe nobody ever closes. If it
	// reached codex, the fake would block reading it until the timeout.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer pr.Close()
	defer pw.Close()
	saved := os.Stdin
	os.Stdin = pr
	defer func() { os.Stdin = saved }()
	res := run(t, o)
	if res.Status != StatusOK || res.Mode != "review" {
		t.Fatalf("got %+v", res)
	}
	if got := h.recorded("stdin.txt"); got != "" {
		t.Errorf("review-mode stdin = %q, want empty (/dev/null)", got)
	}
	args := h.recorded("args.txt")
	for _, want := range []string{"exec\nreview\n--base\nmain", `sandbox_mode="workspace-write"`} {
		if !strings.Contains(args, want) {
			t.Errorf("argv missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "-C\n") || strings.HasSuffix(args, "\n-") {
		t.Errorf("review mode takes neither -C nor a stdin prompt:\n%s", args)
	}
}

func TestNoGitCheckSkipOnlyOutsideRepo(t *testing.T) {
	h := newHarness(t, "ok")
	o := h.opts()
	o.Dir = gitRepo(t)
	run(t, o)
	if strings.Contains(h.recorded("args.txt"), "--skip-git-repo-check") {
		t.Error("--skip-git-repo-check passed inside a git repo")
	}
}

func TestEmptyAnswerIsNotOK(t *testing.T) {
	for _, mode := range []string{"empty", "whitespace"} {
		t.Run(mode, func(t *testing.T) {
			h := newHarness(t, mode)
			res := run(t, h.opts())
			if res.Status != StatusNoAnswer {
				t.Fatalf("status = %s, want no_answer", res.Status)
			}
		})
	}
}

func TestFinalWithoutCompletedTurnIsNotOK(t *testing.T) {
	h := newHarness(t, "final-without-completion")
	res := run(t, h.opts())
	if res.Status != StatusNoAnswer || !strings.Contains(res.Error, "completed turn") {
		t.Fatalf("got %+v, want no_answer for an incomplete turn", res)
	}
}

func TestCompletedEventWithoutTrailingNewlineIsRead(t *testing.T) {
	h := newHarness(t, "midline")
	if res := run(t, h.opts()); res.Status != StatusOK {
		t.Fatalf("got %+v, want ok", res)
	}
}

func TestStaleFinalFromEarlierRunIsRemoved(t *testing.T) {
	h := newHarness(t, "empty")
	if err := os.WriteFile(filepath.Join(h.out, "final.md"), []byte("old answer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if res := run(t, h.opts()); res.Status != StatusNoAnswer {
		t.Fatalf("status = %s: an old final.md was read as this run's answer", res.Status)
	}
}

func TestFailures(t *testing.T) {
	cases := []struct {
		mode, wantErr string
		want          Status
	}{
		{"fail", "model refused", StatusFailed},
		{"fail-exit0", "stream disconnected", StatusFailed},
		{"ratelimit", "usage limit", StatusRateLimited},
		{"ratelimit-stderr", "request failed", StatusRateLimited},
		{"stderr-only", "Not inside a trusted directory", StatusFailed},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			h := newHarness(t, c.mode)
			res := run(t, h.opts())
			if res.Status != c.want || !strings.Contains(res.Error, c.wantErr) {
				t.Fatalf("got status=%s error=%q, want %s containing %q", res.Status, res.Error, c.want, c.wantErr)
			}
		})
	}
}

func TestTimeoutKillsWholeProcessGroup(t *testing.T) {
	h := newHarness(t, "hang")
	o := h.opts()
	o.Timeout = 1 * time.Second
	start := time.Now()
	res := run(t, o)
	if res.Status != StatusTimeout || res.CodexExit != nil {
		t.Fatalf("got %+v", res)
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Errorf("took %s to enforce a 1s timeout", d)
	}
	pid, err := strconv.Atoi(h.recorded("child.pid"))
	if err != nil {
		t.Fatalf("child pid not recorded: %v", err)
	}
	assertDead(t, pid)
}

func TestStallKillsSilentRun(t *testing.T) {
	h := newHarness(t, "silent")
	o := h.opts()
	o.Stall = 700 * time.Millisecond
	res := run(t, o)
	if res.Status != StatusStalled {
		t.Fatalf("status = %s, want stalled", res.Status)
	}
}

func TestActivityResetsStall(t *testing.T) {
	h := newHarness(t, "chatty")
	o := h.opts()
	o.Stall = 1500 * time.Millisecond // half the run, far longer than any gap
	if res := run(t, o); res.Status != StatusOK {
		t.Fatalf("status = %s (%s), want ok: stdout events must count as activity", res.Status, res.Error)
	}
}

func TestRolloutGrowthCountsAsActivity(t *testing.T) {
	h := newHarness(t, "rollout")
	o := h.opts()
	t.Setenv("FAKE_CODEX_HOME", o.CodexHome)
	// A stale file with the same suffix must not capture the watcher and hide
	// the rollout created by this run.
	staleDir := filepath.Join(o.CodexHome, "sessions", "2020", "01", "01")
	if err := os.MkdirAll(staleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	stale := filepath.Join(staleDir, "rollout-old-thread-rollout.jsonl")
	if err := os.WriteFile(stale, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(stale, old, old); err != nil {
		t.Fatal(err)
	}
	o.Stall = 1500 * time.Millisecond
	res := run(t, o)
	if res.Status != StatusOK {
		t.Fatalf("status = %s (%s), want ok: a growing session log must count as activity", res.Status, res.Error)
	}
	if !strings.HasSuffix(res.Rollout, "thread-rollout.jsonl") {
		t.Errorf("rollout = %q", res.Rollout)
	}
}

func TestFutureRolloutMtimeDoesNotDisableStall(t *testing.T) {
	h := newHarness(t, "future-rollout")
	o := h.opts()
	t.Setenv("FAKE_CODEX_HOME", o.CodexHome)
	o.Stall = 500 * time.Millisecond
	o.Timeout = 3 * time.Second
	if res := run(t, o); res.Status != StatusStalled {
		t.Fatalf("got %+v, want stalled", res)
	}
}

func TestNormalExitKillsStdoutHoldingDescendant(t *testing.T) {
	h := newHarness(t, "leaky-exit")
	o := h.opts()
	o.Grace = 300 * time.Millisecond
	res := run(t, o)
	if res.Status != StatusOK {
		t.Fatalf("got %+v, want ok", res)
	}
	pid, err := strconv.Atoi(h.recorded("child.pid"))
	if err != nil {
		t.Fatalf("child pid not recorded: %v", err)
	}
	assertDead(t, pid)
}

func TestCancelInterruptsAndKills(t *testing.T) {
	h := newHarness(t, "hang")
	o := h.opts()
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(500*time.Millisecond, cancel)
	res, err := Run(ctx, o)
	if err != nil {
		t.Fatal(err)
	}
	if res.Status != StatusInterrupted {
		t.Fatalf("status = %s, want interrupted", res.Status)
	}
	pid, _ := strconv.Atoi(h.recorded("child.pid"))
	assertDead(t, pid)
}

func TestMissingBinaryIsAStartError(t *testing.T) {
	o := (&harness{t: t, out: t.TempDir()}).opts()
	o.Bin = filepath.Join(t.TempDir(), "no-such-codex")
	if _, err := Run(context.Background(), o); err == nil {
		t.Fatal("want an error for a missing codex binary")
	}
}

func assertDead(t *testing.T, pid int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if err := syscall.Kill(pid, 0); err != nil {
			return
		}
		// A killed child of a dead parent may linger as a zombie until init
		// reaps it; check its state rather than mere existence.
		if out, _ := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output(); strings.HasPrefix(strings.TrimSpace(string(out)), "Z") {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
	t.Fatalf("child %d of the codex process group survived", pid)
}
