package coderabbit

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/namedlock"
)

func TestParseWait(t *testing.T) {
	cases := []struct {
		in   string
		want time.Duration
		ok   bool
	}{
		{"12 minutes and 13 seconds", 12*time.Minute + 13*time.Second, true},
		{"45 minutes", 45 * time.Minute, true},
		{"1 minute", time.Minute, true},
		{"30 seconds", 30 * time.Second, true},
		{"1 second", time.Second, true},
		{"1 hour, 2 minutes and 3 seconds", time.Hour + 2*time.Minute + 3*time.Second, true},
		{"2 hours", 2 * time.Hour, true},
		{"5 mins 10 secs", 5*time.Minute + 10*time.Second, true},
		{"12m13s", 12*time.Minute + 13*time.Second, true},
		{"90", 90 * time.Second, true},
		{" 7 Minutes ", 7 * time.Minute, true},
		{"", 0, false},
		{"soon", 0, false},
		{"about 5 minutes", 0, false},
		{"5 minutes or more", 0, false},
		{"5 fortnights", 0, false},
	}
	for _, c := range cases {
		got, ok := ParseWait(c.in)
		if ok != c.ok || (ok && got != c.want) {
			t.Errorf("ParseWait(%q) = %s, %v; want %s, %v", c.in, got, ok, c.want, c.ok)
		}
	}
}

// fakeSleep records each wait and never sleeps.
type fakeSleep struct {
	waits []time.Duration
	err   error
}

func (f *fakeSleep) sleep(_ context.Context, d time.Duration) error {
	f.waits = append(f.waits, d)
	return f.err
}

func retryRun(t *testing.T, mode string, r Retry, env map[string]string) (Result, string) {
	t.Helper()
	for k, v := range env {
		t.Setenv(k, v)
	}
	rec := t.TempDir()
	t.Setenv("FAKE_CR_MODE", mode)
	t.Setenv("FAKE_CR_REC", rec)
	o := Options{Bin: os.Args[0], Dir: t.TempDir(), Scope: Scope{Base: "origin/main"}, Timeout: 10 * time.Second,
		Stall: 5 * time.Second, OutDir: t.TempDir(), Poll: 10 * time.Millisecond, Grace: 200 * time.Millisecond}
	res, err := RunRetrying(context.Background(), o, r)
	if err != nil {
		t.Fatal(err)
	}
	return res, rec
}

func runs(t *testing.T, rec string) string {
	t.Helper()
	b, _ := os.ReadFile(filepath.Join(rec, "runs"))
	return strings.TrimSpace(string(b))
}

func TestRateLimitWaitsTheStatedTimeThenRetries(t *testing.T) {
	fs := &fakeSleep{}
	var logs []string
	res, rec := retryRun(t, "ratelimit-then-clean", Retry{Retries: 1, MaxWait: 30 * time.Minute, DefaultWait: 10 * time.Minute,
		Jitter: func() time.Duration { return 7 * time.Second }, Sleep: fs.sleep, Log: func(m string) { logs = append(logs, m) }}, nil)
	if res.Status != StatusClean || res.Attempts != 2 || runs(t, rec) != "2" {
		t.Fatalf("status %s, attempts %d, runs %s; want clean after 2", res.Status, res.Attempts, runs(t, rec))
	}
	want := 12*time.Minute + 20*time.Second // 12m13s + 7s jitter
	if len(fs.waits) != 1 || fs.waits[0] != want {
		t.Fatalf("waits %v, want [%s]", fs.waits, want)
	}
	if res.RateLimitWaitS != want.Seconds() {
		t.Fatalf("rate_limit_wait_s = %v, want %v", res.RateLimitWaitS, want.Seconds())
	}
	if len(logs) != 1 || !strings.Contains(logs[0], "12 minutes and 13 seconds") || !strings.Contains(logs[0], "12m20s") {
		t.Fatalf("logs %q", logs)
	}
	// The first attempt's logs are kept next to the final ones.
	if _, err := os.Stat(filepath.Join(res.Out, "events.attempt1.jsonl")); err != nil {
		t.Fatalf("attempt 1 events not kept: %v", err)
	}
	if b, _ := os.ReadFile(res.Events); strings.Contains(string(b), "rate_limit") {
		t.Fatal("the final events file holds the rate-limited attempt")
	}
}

func TestRateLimitAfterEveryRetryIsRateLimited(t *testing.T) {
	fs := &fakeSleep{}
	res, rec := retryRun(t, "ratelimit-meta", Retry{Retries: 1, MaxWait: 30 * time.Minute, DefaultWait: 10 * time.Minute, Sleep: fs.sleep},
		map[string]string{"FAKE_CR_WAIT": "2 minutes and 5 seconds"})
	_ = rec
	if res.Status != StatusRateLimited || res.Attempts != 2 || len(fs.waits) != 1 || fs.waits[0] != 2*time.Minute+5*time.Second {
		t.Fatalf("status %s, attempts %d, waits %v", res.Status, res.Attempts, fs.waits)
	}
	if res.RateLimitWait != "2 minutes and 5 seconds" || res.RetryAfterS == nil || *res.RetryAfterS != 125 || res.RateLimitGuidance != "connect the repository" {
		t.Fatalf("rate-limit fields: %q %v %q", res.RateLimitWait, res.RetryAfterS, res.RateLimitGuidance)
	}
	if res.RateLimitWaitS != 125 {
		t.Fatalf("rate_limit_wait_s = %v", res.RateLimitWaitS)
	}
}

func TestRateLimitNoRetries(t *testing.T) {
	fs := &fakeSleep{}
	res, _ := retryRun(t, "ratelimit-meta", Retry{Retries: 0, MaxWait: 30 * time.Minute, DefaultWait: 10 * time.Minute, Sleep: fs.sleep},
		map[string]string{"FAKE_CR_WAIT": "1 minute"})
	if res.Status != StatusRateLimited || res.Attempts != 1 || len(fs.waits) != 0 {
		t.Fatalf("status %s, attempts %d, waits %v", res.Status, res.Attempts, fs.waits)
	}
}

func TestRateLimitLongerThanMaxWaitIsNotWaited(t *testing.T) {
	fs := &fakeSleep{}
	res, _ := retryRun(t, "ratelimit-meta", Retry{Retries: 1, MaxWait: 30 * time.Minute, DefaultWait: 10 * time.Minute, Sleep: fs.sleep},
		map[string]string{"FAKE_CR_WAIT": "45 minutes"})
	if res.Status != StatusRateLimited || res.Attempts != 1 || len(fs.waits) != 0 || !strings.Contains(res.Error, "longer than the rate-limit max wait") {
		t.Fatalf("status %s, attempts %d, waits %v, error %q", res.Status, res.Attempts, fs.waits, res.Error)
	}
}

func TestRateLimitWithoutWaitTimeUsesDefaultAndJitterIsCapped(t *testing.T) {
	fs := &fakeSleep{}
	// "ratelimit" names no waitTime in metadata.
	res, _ := retryRun(t, "ratelimit", Retry{Retries: 1, MaxWait: 10*time.Minute + 3*time.Second, DefaultWait: 10 * time.Minute,
		Jitter: func() time.Duration { return 14 * time.Second }, Sleep: fs.sleep}, nil)
	if res.Status != StatusRateLimited || res.Attempts != 2 || len(fs.waits) != 1 || fs.waits[0] != 10*time.Minute+3*time.Second {
		t.Fatalf("status %s, attempts %d, waits %v", res.Status, res.Attempts, fs.waits)
	}
	if res.RetryAfterS != nil {
		t.Fatalf("retry_after_s = %v, want null with no waitTime", *res.RetryAfterS)
	}
}

func TestCancelDuringRateLimitWaitInterrupts(t *testing.T) {
	fs := &fakeSleep{err: context.Canceled}
	res, rec := retryRun(t, "ratelimit-then-clean", Retry{Retries: 1, MaxWait: 30 * time.Minute, DefaultWait: 10 * time.Minute, Sleep: fs.sleep}, nil)
	if res.Status != StatusInterrupted || res.Attempts != 1 || runs(t, rec) != "1" {
		t.Fatalf("status %s, attempts %d, runs %s", res.Status, res.Attempts, runs(t, rec))
	}
}

func TestOtherFailuresAreNotRetried(t *testing.T) {
	fs := &fakeSleep{}
	res, _ := retryRun(t, "fail", Retry{Retries: 3, MaxWait: time.Hour, DefaultWait: time.Minute, Sleep: fs.sleep}, nil)
	if res.Status != StatusFailed || res.Attempts != 1 || len(fs.waits) != 0 {
		t.Fatalf("status %s, attempts %d, waits %v", res.Status, res.Attempts, fs.waits)
	}
}

// A holder that dies (even by SIGKILL) releases the lock with its process:
// a waiter then takes it, whatever the stale holder record says.
func TestWaitRepoTakesOverFromDeadHolder(t *testing.T) {
	dir := gitRepo(t)
	rec := t.TempDir()
	holder := exec.Command(os.Args[0])
	holder.Env = append(os.Environ(), "FAKE_CR_MODE=hold-lock", "FAKE_CR_REC="+rec, "FAKE_CR_DIR="+dir)
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = holder.Process.Kill(); _ = holder.Wait() }()
	waitFor(t, func() bool { _, err := os.Stat(filepath.Join(rec, "locked")); return err == nil })

	holders := make(chan namedlock.Holder, 4)
	got := make(chan error, 1)
	go func() {
		unlock, _, err := WaitRepo(context.Background(), dir, namedlock.NewInfo("test", dir), func(h namedlock.Holder) { holders <- h })
		if err == nil {
			unlock()
		}
		got <- err
	}()
	h := <-holders
	if h.Info == nil || h.Info.PID != holder.Process.Pid || !h.Alive {
		t.Fatalf("holder = %+v, want live pid %d", h, holder.Process.Pid)
	}
	select {
	case err := <-got:
		t.Fatalf("acquired while the holder lives: %v", err)
	default:
	}
	_ = holder.Process.Signal(syscall.SIGKILL)
	select {
	case err := <-got:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the lock was not taken over after the holder died")
	}
}

func TestWaitRepoEndsWithContext(t *testing.T) {
	dir := gitRepo(t)
	unlock, _, err := LockRepo(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, _, err := WaitRepo(ctx, dir, namedlock.NewInfo("test", dir), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want deadline exceeded", err)
	}
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 10s")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
