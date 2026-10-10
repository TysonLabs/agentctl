package namedlock

import (
	"bufio"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestHelperHold is not a test: run as a subprocess with NAMEDLOCK_HELPER_HOLD
// set, it takes that lock, prints "held", and blocks until killed.
func TestHelperHold(t *testing.T) {
	name := os.Getenv("NAMEDLOCK_HELPER_HOLD")
	if name == "" {
		t.Skip("helper process only")
	}
	l, err := Acquire(context.Background(), name, NewInfo("helper", "/"), nil)
	if err != nil {
		os.Exit(3)
	}
	_ = l
	os.Stdout.WriteString("held\n")
	select {}
}

func isolate(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("TMPDIR", d)
	return d
}

// holdInSubprocess starts a process that holds name and returns it once it
// reports the lock held.
func holdInSubprocess(t *testing.T, name string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperHold$")
	cmd.Env = append(os.Environ(), "NAMEDLOCK_HELPER_HOLD="+name)
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cmd.Process.Kill(); _ = cmd.Wait() })
	line, err := bufio.NewReader(out).ReadString('\n')
	if err != nil || strings.TrimSpace(line) != "held" {
		t.Fatalf("helper did not report the lock held: %q %v", line, err)
	}
	return cmd
}

func TestValidateName(t *testing.T) {
	for _, ok := range []string{"a", "cargo-build", "A.b_c-1", strings.Repeat("x", 64)} {
		if err := ValidateName(ok); err != nil {
			t.Errorf("ValidateName(%q) = %v, want ok", ok, err)
		}
	}
	for _, bad := range []string{"", strings.Repeat("x", 65), "a/b", "../x", "a b", "a:b", "é"} {
		if err := ValidateName(bad); err == nil {
			t.Errorf("ValidateName(%q) = nil, want error", bad)
		}
	}
	isolate(t)
	if _, err := TryAcquire("a/b", Info{}); err == nil {
		t.Error("TryAcquire accepted an invalid name")
	}
	if _, err := Status("../x"); err == nil {
		t.Error("Status accepted an invalid name")
	}
}

func TestConventionPaths(t *testing.T) {
	d := isolate(t)
	lp, ip := Paths("build")
	if lp != filepath.Join(d, "agentflow-lock-build.lock") || ip != filepath.Join(d, "agentflow-lock-build.json") {
		t.Fatalf("paths %s %s do not follow the shared convention", lp, ip)
	}
}

func TestAcquireWritesAndReleaseRemovesHolderRecord(t *testing.T) {
	isolate(t)
	info := NewInfo("cargo build", "/src")
	l, err := Acquire(context.Background(), "x", info, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, ip := Paths("x")
	data, err := os.ReadFile(ip)
	if err != nil {
		t.Fatalf("no holder record while held: %v", err)
	}
	for _, want := range []string{`"pid":`, `"cmd":"cargo build"`, `"dir":"/src"`, `"since":"`} {
		if !strings.Contains(string(data), want) {
			t.Errorf("holder record %s lacks %s", data, want)
		}
	}
	if _, err := time.Parse(time.RFC3339, info.Since); err != nil {
		t.Errorf("since %q is not RFC3339: %v", info.Since, err)
	}
	st, err := Status("x")
	if err != nil || !st.Held || st.Holder == nil || st.Holder.PID != os.Getpid() || !st.HolderAlive {
		t.Fatalf("status while held = %+v, %v", st, err)
	}
	if _, err := TryAcquire("x", info); !errors.Is(err, ErrBusy) {
		t.Fatalf("second TryAcquire = %v, want ErrBusy", err)
	}
	if err := l.Release(); err != nil {
		t.Fatal(err)
	}
	if err := l.Release(); err != nil {
		t.Fatalf("second Release = %v, want no-op", err)
	}
	if _, err := os.Stat(ip); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("holder record still present after release: %v", err)
	}
	st, err = Status("x")
	if err != nil || st.Held || st.Holder != nil {
		t.Fatalf("status after release = %+v, %v", st, err)
	}
}

func TestStatusNeverCreatesLockFile(t *testing.T) {
	isolate(t)
	st, err := Status("never")
	if err != nil || st.Held {
		t.Fatalf("status = %+v, %v", st, err)
	}
	lp, _ := Paths("never")
	if _, err := os.Stat(lp); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("Status created %s", lp)
	}
}

func TestAcquireTimesOutAndAbandonedWaitDoesNotKeepLock(t *testing.T) {
	isolate(t)
	first, err := TryAcquire("x", NewInfo("first", "/"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := Acquire(ctx, "x", NewInfo("second", "/"), nil); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire = %v, want deadline exceeded", err)
	}
	if err := first.Release(); err != nil {
		t.Fatal(err)
	}
	// The abandoned flock goroutine may win the lock now; it must let go.
	ctx2, cancel2 := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel2()
	third, err := Acquire(ctx2, "x", NewInfo("third", "/"), nil)
	if err != nil {
		t.Fatalf("lock not free after the abandoned wait: %v", err)
	}
	third.Release()
}

func TestLockReleasedWhenHolderIsKilled(t *testing.T) {
	isolate(t)
	h := holdInSubprocess(t, "k")
	st, err := Status("k")
	if err != nil || !st.Held || st.Holder == nil || st.Holder.PID != h.Process.Pid || !st.HolderAlive {
		t.Fatalf("status with live holder = %+v, %v", st, err)
	}
	if err := h.Process.Kill(); err != nil { // SIGKILL: no cleanup runs
		t.Fatal(err)
	}
	_ = h.Wait()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	l, err := Acquire(ctx, "k", NewInfo("next", "/"), nil)
	if err != nil {
		t.Fatalf("lock not released after SIGKILL: %v", err)
	}
	l.Release()
}

func TestStatusReportsStaleRecordAndDeadHolder(t *testing.T) {
	isolate(t)
	h := holdInSubprocess(t, "s")
	pid := h.Process.Pid
	_ = h.Process.Kill()
	_ = h.Wait()
	// The SIGKILLed holder left its record behind.
	st, err := Status("s")
	if err != nil {
		t.Fatal(err)
	}
	if st.Held || st.Holder == nil || st.Holder.PID != pid || st.HolderAlive {
		t.Fatalf("status = %+v, want free with a stale, dead holder record", st)
	}
	all, err := List()
	if err != nil || len(all) != 1 || all[0].Name != "s" {
		t.Fatalf("List = %+v, %v", all, err)
	}
}

func TestListFindsLocksAndSkipsForeignFiles(t *testing.T) {
	d := isolate(t)
	a, _ := TryAcquire("a", NewInfo("x", "/"))
	defer a.Release()
	b, _ := TryAcquire("b", NewInfo("x", "/"))
	b.Release()
	for _, f := range []string{"agentflow-lock-bad name.lock", "agentflow-lessons-1.lock", "other.json"} {
		_ = os.WriteFile(filepath.Join(d, f), nil, 0o600)
	}
	all, err := List()
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != 2 || all[0].Name != "a" || !all[0].Held || all[1].Name != "b" || all[1].Held {
		t.Fatalf("List = %+v", all)
	}
}

func TestOnHolderReportsOnceThenOnChange(t *testing.T) {
	isolate(t)
	old := watchInterval
	watchInterval = 20 * time.Millisecond
	defer func() { watchInterval = old }()

	first, err := TryAcquire("w", NewInfo("first", "/"))
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var seen []string
	onHolder := func(h Holder) {
		mu.Lock()
		defer mu.Unlock()
		if h.Info == nil {
			seen = append(seen, "unknown")
			return
		}
		seen = append(seen, h.Info.Cmd)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	_, err = Acquire(ctx, "w", NewInfo("waiter", "/"), onHolder)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire = %v", err)
	}
	mu.Lock()
	if strings.Join(seen, ",") != "first" {
		t.Fatalf("reports = %v, want exactly one for the unchanged holder", seen)
	}
	seen = nil
	mu.Unlock()

	// A new holder record (same lock still held) is a change.
	_, ip := Paths("w")
	go func() {
		time.Sleep(100 * time.Millisecond)
		_ = writeInfo(ip, NewInfo("second", "/"))
	}()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel2()
	_, _ = Acquire(ctx2, "w", NewInfo("waiter", "/"), onHolder)
	mu.Lock()
	got := strings.Join(seen, ",")
	mu.Unlock()
	if got != "first,second" {
		t.Fatalf("reports = %s, want first,second", got)
	}
	first.Release()
}

func TestOnHolderUnknownAfterGrace(t *testing.T) {
	isolate(t)
	old := watchInterval
	watchInterval = 20 * time.Millisecond
	defer func() { watchInterval = old }()
	first, err := TryAcquire("u", NewInfo("first", "/"))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Release()
	_, ip := Paths("u")
	_ = os.Remove(ip) // a holder that writes no record
	var n int
	var mu sync.Mutex
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	_, _ = Acquire(ctx, "u", Info{}, func(h Holder) {
		mu.Lock()
		defer mu.Unlock()
		if h.Info != nil {
			t.Errorf("unexpected holder %+v", h.Info)
		}
		n++
	})
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Fatalf("unknown-holder reports = %d, want 1", n)
	}
}
