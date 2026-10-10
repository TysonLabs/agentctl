package cli

import (
	"bytes"
	"encoding/json"
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

// TestLockHelperAgentflow is not a test: run as a subprocess with
// AGENTFLOW_LOCK_HELPER=1, it is agentflow, taking its arguments after "--".
func TestLockHelperAgentflow(t *testing.T) {
	if os.Getenv("AGENTFLOW_LOCK_HELPER") != "1" {
		t.Skip("helper process only")
	}
	args := os.Args
	for i, a := range args {
		if a == "--" {
			args = args[i+1:]
			break
		}
	}
	os.Exit(Run(args, os.Stdout, os.Stderr))
}

// agentflowCmd builds an agentflow subprocess (the test binary as helper).
func agentflowCmd(args ...string) *exec.Cmd {
	cmd := exec.Command(os.Args[0], append([]string{"-test.run=^TestLockHelperAgentflow$", "--"}, args...)...)
	cmd.Env = append(os.Environ(), "AGENTFLOW_LOCK_HELPER=1")
	return cmd
}

func lockIsolate(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	t.Setenv("TMPDIR", d)
	return d
}

func waitFile(t *testing.T, path string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("%s never appeared", path)
}

func exitCode(t *testing.T, err error) int {
	t.Helper()
	var ee *exec.ExitError
	if err == nil {
		return 0
	}
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	t.Fatalf("unexpected error: %v", err)
	return -1
}

func TestLockRunSerializesConcurrentRuns(t *testing.T) {
	d := lockIsolate(t)
	log := filepath.Join(d, "log")
	script := func(id string) string {
		return "echo start-" + id + " >> " + log + "; touch " + filepath.Join(d, "started-"+id) + "; sleep 0.5; echo end-" + id + " >> " + log
	}
	a := agentflowCmd("lock", "run", "--name", "build", "--", "sh", "-c", script("a"))
	if err := a.Start(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, filepath.Join(d, "started-a"))
	var bErr bytes.Buffer
	b := agentflowCmd("lock", "run", "--name", "build", "--", "sh", "-c", script("b"))
	b.Stderr = &bErr
	if err := b.Start(); err != nil {
		t.Fatal(err)
	}
	if err := a.Wait(); err != nil {
		t.Fatal(err)
	}
	if err := b.Wait(); err != nil {
		t.Fatalf("b: %v (%s)", err, bErr.String())
	}
	got, _ := os.ReadFile(log)
	if strings.Join(strings.Fields(string(got)), ",") != "start-a,end-a,start-b,end-b" {
		t.Fatalf("runs overlapped:\n%s", got)
	}
	if n := strings.Count(bErr.String(), "waiting for lock"); n != 1 {
		t.Fatalf("waiter printed %d holder lines, want 1:\n%s", n, bErr.String())
	}
	if !strings.Contains(bErr.String(), "sh -c") || !strings.Contains(bErr.String(), "(alive)") {
		t.Fatalf("holder line does not name the holder:\n%s", bErr.String())
	}
}

func TestLockRunWaitTimeoutDoesNotRunCommand(t *testing.T) {
	d := lockIsolate(t)
	held, err := namedlock.TryAcquire("busy", namedlock.NewInfo("other job", "/elsewhere"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	marker := filepath.Join(d, "ran")
	for _, wait := range []string{"0", "300ms"} {
		var stdout, stderr bytes.Buffer
		code := Run([]string{"lock", "run", "--name", "busy", "--wait", wait, "--", "touch", marker}, &stdout, &stderr)
		if code != 75 {
			t.Fatalf("--wait %s: exit %d, want 75 (%s)", wait, code, stderr.String())
		}
		if _, err := os.Stat(marker); err == nil {
			t.Fatalf("--wait %s: command ran without the lock", wait)
		}
		if n := strings.Count(stderr.String(), "waiting for lock"); n != 1 || !strings.Contains(stderr.String(), "other job") {
			t.Fatalf("--wait %s: want one line naming the holder:\n%s", wait, stderr.String())
		}
		if !strings.Contains(stderr.String(), "not acquired within") {
			t.Fatalf("--wait %s: no timeout message:\n%s", wait, stderr.String())
		}
	}
}

func TestLockRunHolderLinePrintedOnceWhileHolderUnchanged(t *testing.T) {
	lockIsolate(t)
	held, err := namedlock.TryAcquire("slow", namedlock.NewInfo("long build", "/"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	var stdout, stderr bytes.Buffer
	// Longer than two holder-file re-reads (every 2s).
	code := Run([]string{"lock", "run", "--name", "slow", "--wait", "4500ms", "--", "true"}, &stdout, &stderr)
	if code != 75 {
		t.Fatalf("exit %d, want 75", code)
	}
	if n := strings.Count(stderr.String(), "waiting for lock"); n != 1 {
		t.Fatalf("printed %d holder lines, want 1:\n%s", n, stderr.String())
	}
}

func TestLockRunExitCodePropagation(t *testing.T) {
	lockIsolate(t)
	for _, tc := range []struct {
		args []string
		want int
	}{
		{[]string{"true"}, 0},
		{[]string{"sh", "-c", "exit 7"}, 7},
		{[]string{"sh", "-c", "exit 75"}, 75},
		{[]string{"sh", "-c", "kill -9 $$"}, 128 + 9},
		{[]string{"no-such-command-agentflow-test"}, 127},
		{[]string{"./no/such/path"}, 127},
	} {
		var stdout, stderr bytes.Buffer
		code := Run(append([]string{"lock", "run", "--name", "codes", "--"}, tc.args...), &stdout, &stderr)
		if code != tc.want {
			t.Errorf("%v: exit %d, want %d (%s)", tc.args, code, tc.want, stderr.String())
		}
	}
	// The lock is free again after every run.
	if st, err := namedlock.Status("codes"); err != nil || st.Held || st.Holder != nil {
		t.Fatalf("lock not released: %+v %v", st, err)
	}
}

func TestLockRunPassesOutputDirAndStdin(t *testing.T) {
	d := lockIsolate(t)
	cmd := agentflowCmd("lock", "run", "--name", "io", "--dir", d, "--", "sh", "-c", "pwd; cat; echo err >&2; ls \"$TMPDIR\" | grep -c agentflow-lock-io.json")
	cmd.Stdin = strings.NewReader("from-stdin\n")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%v: %s", err, stderr.String())
	}
	real, _ := filepath.EvalSymlinks(d)
	out := stdout.String()
	if !strings.Contains(out, "from-stdin") || (!strings.Contains(out, d) && !strings.Contains(out, real)) {
		t.Fatalf("stdout = %q", out)
	}
	if !strings.HasSuffix(strings.TrimSpace(out), "1") {
		t.Fatalf("holder record missing while the command ran: %q", out)
	}
	if strings.TrimSpace(stderr.String()) != "err" {
		t.Fatalf("stderr = %q", stderr.String())
	}
}

func TestLockRunForwardsSIGTERMToChildGroup(t *testing.T) {
	d := lockIsolate(t)
	ready, got := filepath.Join(d, "ready"), filepath.Join(d, "got")
	script := `trap 'echo term > ` + got + `; exit 0' TERM; touch ` + ready + `; while :; do sleep 0.05; done`
	cmd := agentflowCmd("lock", "run", "--name", "sig", "--", "sh", "-c", script)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	waitFile(t, ready)
	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if code := exitCode(t, err); code != 0 {
			t.Fatalf("exit %d, want the child's 0 (%s)", code, stderr.String())
		}
	case <-time.After(10 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("agentflow did not exit after SIGTERM")
	}
	if b, err := os.ReadFile(got); err != nil || strings.TrimSpace(string(b)) != "term" {
		t.Fatalf("child did not get SIGTERM: %q %v", b, err)
	}
}

func TestLockRunInterruptedWhileWaiting(t *testing.T) {
	d := lockIsolate(t)
	held, err := namedlock.TryAcquire("int", namedlock.NewInfo("holder", "/"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	marker := filepath.Join(d, "ran")
	cmd := agentflowCmd("lock", "run", "--name", "int", "--", "touch", marker)
	errPath := filepath.Join(d, "stderr")
	errFile, err := os.Create(errPath)
	if err != nil {
		t.Fatal(err)
	}
	defer errFile.Close()
	cmd.Stderr = errFile
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	stderr := func() string { b, _ := os.ReadFile(errPath); return string(b) }
	deadline := time.Now().Add(10 * time.Second)
	for !strings.Contains(stderr(), "waiting for lock") && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	_ = cmd.Process.Signal(syscall.SIGINT)
	if code := exitCode(t, cmd.Wait()); code != 130 {
		t.Fatalf("exit %d, want 130 (%s)", code, stderr())
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("command ran after an interrupt")
	}
}

func TestLockRunReleasedAfterHolderSIGKILL(t *testing.T) {
	d := lockIsolate(t)
	ready := filepath.Join(d, "ready")
	holder := agentflowCmd("lock", "run", "--name", "kill9", "--", "sh", "-c", "touch "+ready+"; exec sleep 30")
	holder.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := holder.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = syscall.Kill(-holder.Process.Pid, syscall.SIGKILL) }()
	waitFile(t, ready)
	if st, _ := namedlock.Status("kill9"); !st.Held {
		t.Fatal("lock not held by the running holder")
	}
	_ = holder.Process.Kill() // SIGKILL agentflow itself
	_ = holder.Wait()
	var stdout, stderr bytes.Buffer
	if code := Run([]string{"lock", "run", "--name", "kill9", "--wait", "10s", "--", "true"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d after the holder was SIGKILLed (%s)", code, stderr.String())
	}
}

func TestLockRunUsage(t *testing.T) {
	lockIsolate(t)
	for _, args := range [][]string{
		{"run", "--", "true"},
		{"run", "--name", "a/b", "--", "true"},
		{"run", "--name", strings.Repeat("x", 65), "--", "true"},
		{"run", "--name", "ok"},
		{"run", "--name", "ok", "--wait", "-1s", "--", "true"},
		{"run", "--name", "ok", "--dir", "/no/such/dir", "--", "true"},
		{"run", "--bogus", "--", "true"},
		{"status", "extra"},
		{"status", "--name", "bad name"},
		{"frobnicate"},
	} {
		var stdout, stderr bytes.Buffer
		if code := Run(append([]string{"lock"}, args...), &stdout, &stderr); code != 1 {
			t.Errorf("%v: exit %d, want 1", args, code)
		}
	}
}

func TestLockRunDoesNotParseCommandFlags(t *testing.T) {
	lockIsolate(t)
	var stdout, stderr bytes.Buffer
	// No "--": flags end at the first non-flag, so --name here is sh's.
	if code := Run([]string{"lock", "run", "--name", "f", "sh", "-c", "exit 3", "--name"}, &stdout, &stderr); code != 3 {
		t.Fatalf("exit %d, want 3 (%s)", code, stderr.String())
	}
}

func TestLockStatus(t *testing.T) {
	lockIsolate(t)
	held, err := namedlock.TryAcquire("busy", namedlock.NewInfo("job", "/w"))
	if err != nil {
		t.Fatal(err)
	}
	defer held.Release()
	free, _ := namedlock.TryAcquire("free", namedlock.NewInfo("x", "/"))
	free.Release()

	var stdout, stderr bytes.Buffer
	if code := Run([]string{"lock", "status"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d: %s", code, stderr.String())
	}
	var res struct {
		Locks []struct {
			Name   string `json:"name"`
			Held   bool   `json:"held"`
			Holder *struct {
				PID int    `json:"pid"`
				Cmd string `json:"cmd"`
			} `json:"holder"`
			HolderAlive bool `json:"holder_alive"`
		} `json:"locks"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &res); err != nil {
		t.Fatalf("%v: %s", err, stdout.String())
	}
	if len(res.Locks) != 2 {
		t.Fatalf("locks = %+v", res.Locks)
	}
	b, f := res.Locks[0], res.Locks[1]
	if b.Name != "busy" || !b.Held || b.Holder == nil || b.Holder.PID != os.Getpid() || b.Holder.Cmd != "job" || !b.HolderAlive {
		t.Errorf("busy = %+v", b)
	}
	if f.Name != "free" || f.Held || f.Holder != nil || f.HolderAlive {
		t.Errorf("free = %+v", f)
	}

	stdout.Reset()
	if code := Run([]string{"lock", "status", "--name", "unknown"}, &stdout, &stderr); code != 0 {
		t.Fatalf("exit %d", code)
	}
	if !strings.Contains(stdout.String(), `"held": false`) || !strings.Contains(stdout.String(), `"name": "unknown"`) {
		t.Fatalf("status --name unknown = %s", stdout.String())
	}

	stdout.Reset()
	lockIsolate(t)
	if code := Run([]string{"lock", "status"}, &stdout, &stderr); code != 0 || !strings.Contains(stdout.String(), `"locks": []`) {
		t.Fatalf("empty status = %d %s", code, stdout.String())
	}
}

func TestShellJoin(t *testing.T) {
	got := shellJoin([]string{"sh", "-c", "echo 'hi' && ls", ""})
	if got != `sh -c 'echo '\''hi'\'' && ls' ''` {
		t.Fatalf("shellJoin = %s", got)
	}
	if long := shellJoin([]string{strings.Repeat("a", 600)}); len(long) > 520 {
		t.Fatalf("not capped: %d bytes", len(long))
	}
}
