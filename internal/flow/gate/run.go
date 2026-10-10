package gate

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/proc"
	"github.com/TysonLabs/agentctl/internal/gatespec"
)

// Status is a whole run's outcome.
type Status string

const (
	StatusPassed      Status = "passed"      // every selected step passed or was cached
	StatusFailed      Status = "failed"      // a step failed
	StatusTimeout     Status = "timeout"     // a step timed out (and none failed)
	StatusInterrupted Status = "interrupted" // SIGINT/SIGTERM
	StatusError       Status = "error"       // usage or precondition
	StatusMissing     Status = "missing"     // --check: a step has no passing receipt
)

// Step statuses.
const (
	StepPassed      = "passed"
	StepFailed      = "failed"
	StepTimeout     = "timeout"
	StepSkipped     = "skipped"
	StepCached      = "cached"
	StepInterrupted = "interrupted"
)

// Options configures one gate run or check.
type Options struct {
	ConfigPath string
	Project    string   // "" resolves from Dir's repo
	Dir        string   // checkout to run in
	DirGiven   bool     // Dir came from --dir
	Only       []string // run only these steps
	Force      bool     // ignore receipts
	OutDir     string   // step logs
	Stderr     io.Writer
	KillGrace  time.Duration // SIGTERM -> SIGKILL grace (default 5s)
}

// StepResult is one step's line in the result.
type StepResult struct {
	Name   string  `json:"name"`
	Status string  `json:"status"`
	Secs   float64 `json:"secs"`
	Exit   *int    `json:"exit"`
	Log    string  `json:"log,omitempty"`
	At     string  `json:"at,omitempty"` // cached: when the receipt was recorded
}

// LockInfo reports the named lock.
type LockInfo struct {
	Name       string      `json:"name"`
	Path       string      `json:"path"`
	WaitedSecs float64     `json:"waited_secs"`
	WaitedFor  *LockHolder `json:"waited_for,omitempty"` // the holder when this run started waiting
}

// Result is the JSON agentflow gate prints.
type Result struct {
	Status      Status       `json:"status"`
	Project     string       `json:"project,omitempty"`
	Dir         string       `json:"dir,omitempty"`
	Head        string       `json:"head,omitempty"`
	Tree        string       `json:"tree,omitempty"`
	Dirty       bool         `json:"dirty"`
	OK          bool         `json:"ok"`
	Steps       []StepResult `json:"steps"`
	LogDir      string       `json:"log_dir,omitempty"`
	Receipts    string       `json:"receipts,omitempty"`
	Lock        *LockInfo    `json:"lock,omitempty"`
	TreeChanged bool         `json:"tree_changed,omitempty"` // a step changed the work tree; it and later steps have no receipt
	Warnings    []string     `json:"warnings,omitempty"`
	Error       string       `json:"error,omitempty"`
}

func (o *Options) logf(format string, a ...any) {
	if o.Stderr != nil {
		fmt.Fprintf(o.Stderr, "agentflow gate: "+format+"\n", a...)
	}
}

func selectSteps(steps []gatespec.Step, only []string) ([]gatespec.Step, error) {
	if len(only) == 0 {
		return steps, nil
	}
	want := map[string]bool{}
	for _, n := range only {
		want[n] = true
	}
	var out []gatespec.Step
	for _, s := range steps {
		if want[s.Name] {
			out = append(out, s)
			delete(want, s.Name)
		}
	}
	if len(want) > 0 {
		var unknown []string
		for n := range want {
			unknown = append(unknown, n)
		}
		return nil, fmt.Errorf("--only names unknown step(s): %s", strings.Join(unknown, ", "))
	}
	return out, nil
}

// Run resolves the project, takes its lock, and runs the steps in order.
func Run(ctx context.Context, o Options) *Result {
	res := &Result{Steps: []StepResult{}}
	fail := func(err error) *Result {
		res.Status, res.Error = StatusError, err.Error()
		return res
	}
	interruptedResult := func(message string) *Result {
		res.Status, res.Error = StatusInterrupted, message
		return res
	}
	if ctx.Err() != nil {
		return interruptedResult("interrupted")
	}
	if o.KillGrace <= 0 {
		o.KillGrace = 5 * time.Second
	}
	t, err := resolve(ctx, o.ConfigPath, o.Project, o.Dir, o.DirGiven)
	if err != nil {
		if ctx.Err() != nil {
			return interruptedResult("interrupted")
		}
		return fail(err)
	}
	res.Project, res.Dir, res.LogDir = t.project, t.root, o.OutDir
	steps, err := selectSteps(t.spec.Steps, o.Only)
	if err != nil {
		return fail(err)
	}

	if t.spec.Lock != "" {
		lockPath, _ := LockPaths(t.spec.Lock)
		res.Lock = &LockInfo{Name: t.spec.Lock, Path: lockPath}
		me := LockHolder{PID: os.Getpid(), Cmd: "agentflow gate --project " + t.project, Dir: t.root}
		release, waited, err := AcquireLock(ctx, t.spec.Lock, me, func(h *LockHolder) {
			res.Lock.WaitedFor = h
			if h != nil {
				o.logf("waiting for lock %s, held by pid %d (%s in %s) since %s", t.spec.Lock, h.PID, h.Cmd, h.Dir, h.Since)
			} else {
				o.logf("waiting for lock %s (holder unknown)", t.spec.Lock)
			}
		})
		res.Lock.WaitedSecs = secs(waited)
		if err != nil {
			if ctx.Err() != nil {
				res.Status, res.Error = StatusInterrupted, "interrupted while waiting for lock "+t.spec.Lock
				return res
			}
			return fail(err)
		}
		defer release()
	}

	// The tree is read under the lock: it is the state these steps test.
	ts, err := workTree(ctx, t.root)
	if err != nil {
		if ctx.Err() != nil {
			res.Status, res.Error = StatusInterrupted, "interrupted"
			return res
		}
		return fail(fmt.Errorf("hashing the work tree: %v", err))
	}
	res.Head, res.Tree, res.Dirty = ts.head, ts.tree, ts.dirty
	res.Receipts = receiptsPath(t.common)
	rf, err := readReceipts(res.Receipts)
	if err != nil {
		res.Warnings = append(res.Warnings, err.Error()+"; running every step")
		o.logf("warning: %v; running every step", err)
		rf = emptyReceipts()
	}
	if err := os.MkdirAll(o.OutDir, 0o700); err != nil {
		return fail(err)
	}

	env := append(cleanEnv(os.Environ()), "AGENTFLOW_GATE_PROJECT="+t.project)
	stopped, interrupted, anyFailed, anyTimeout := false, false, false, false
	unproven := false // the tree may differ from ts.tree: write no more receipts
	for _, st := range steps {
		sr := StepResult{Name: st.Name}
		switch {
		case stopped:
			sr.Status = StepSkipped
		case ctx.Err() != nil:
			sr.Status = StepInterrupted
			interrupted, stopped = true, true
		case !o.Force && rf.passed(t.project, ts.tree, st.Name, st.Run):
			r, _ := rf.lookup(t.project, ts.tree, st.Name, runHash(st.Run))
			sr.Status, sr.Secs, sr.Exit, sr.At = StepCached, r.Secs, r.Exit, r.At.UTC().Format(time.RFC3339)
			o.logf("%s: cached (passed on this tree at %s)", st.Name, sr.At)
		default:
			sr.Log = filepath.Join(o.OutDir, st.Name+".log")
			o.logf("%s: running (timeout %s, log %s)", st.Name, st.Timeout, sr.Log)
			start := time.Now()
			status, exit, err := runStep(ctx, t.root, st, sr.Log, append(append([]string{}, env...), "AGENTFLOW_GATE_STEP="+st.Name), o.KillGrace)
			sr.Status, sr.Exit, sr.Secs = status, exit, secs(time.Since(start))
			if err != nil {
				res.Warnings = append(res.Warnings, st.Name+": "+err.Error())
			}
			o.logf("%s: %s in %.1fs", st.Name, status, sr.Secs)
			// A receipt claims the step ran on ts.tree. If this step (or an
			// earlier one) changed the work tree, or the tree cannot be read
			// again, that claim is unproven and no receipt is written.
			if status != StepInterrupted && !unproven {
				after, err := rehash(ctx, t.root)
				switch {
				case err != nil && ctx.Err() != nil:
					unproven, interrupted, stopped = true, true, true
				case err != nil:
					// Unknown is not unchanged: no later step may claim ts.tree.
					unproven = true
					res.Warnings = append(res.Warnings, "re-hashing the work tree after "+st.Name+": "+err.Error()+"; no receipt recorded for it or later steps")
					o.logf("warning: re-hashing the work tree after %s: %v; no receipt recorded for it or later steps", st.Name, err)
				case after.tree != ts.tree:
					unproven = true
					res.TreeChanged = true
					res.Warnings = append(res.Warnings, st.Name+" changed the work tree; no receipt recorded for it or later steps")
					o.logf("warning: %s changed the work tree; no receipt recorded for it or later steps", st.Name)
				default:
					r := Receipt{RunHash: runHash(st.Run), OK: status == StepPassed, Status: status, Exit: exit, Secs: sr.Secs, At: time.Now().UTC(), Head: ts.head}
					if err := recordReceipt(res.Receipts, t.project, ts.tree, st.Name, r); err != nil {
						res.Warnings = append(res.Warnings, "recording the receipt of "+st.Name+": "+err.Error())
						o.logf("warning: recording the receipt of %s: %v", st.Name, err)
					}
				}
			}
			switch status {
			case StepInterrupted:
				interrupted, stopped = true, true
			case StepFailed:
				anyFailed = true
				stopped = st.StopOnFail
			case StepTimeout:
				anyTimeout = true
				stopped = st.StopOnFail
			}
		}
		res.Steps = append(res.Steps, sr)
	}

	switch {
	case interrupted:
		res.Status = StatusInterrupted
	case anyFailed:
		res.Status = StatusFailed
	case anyTimeout:
		res.Status = StatusTimeout
	default:
		res.Status, res.OK = StatusPassed, true
	}
	return res
}

// rehash reads the work tree after a step; a variable so tests can fail it.
var rehash = workTree

func secs(d time.Duration) float64 { return math.Round(d.Seconds()*10) / 10 }

// runStep runs one step with /bin/sh -c in root: stdin is /dev/null, stdout
// and stderr go to logPath, and the whole process group is killed on timeout
// or interrupt.
func runStep(ctx context.Context, root string, st gatespec.Step, logPath string, env []string, grace time.Duration) (string, *int, error) {
	logf, err := os.OpenFile(logPath, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		return StepFailed, nil, err
	}
	defer logf.Close()
	devnull, err := os.Open(os.DevNull)
	if err != nil {
		return StepFailed, nil, err
	}
	defer devnull.Close()
	cmd := exec.Command("/bin/sh", "-c", st.Run)
	cmd.Dir = root
	cmd.Env = env
	cmd.Stdin = devnull
	cmd.Stdout, cmd.Stderr = logf, logf
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		fmt.Fprintf(logf, "agentflow gate: cannot start: %v\n", err)
		return StepFailed, nil, err
	}
	pid := cmd.Process.Pid
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	timer := time.NewTimer(st.Timeout)
	defer timer.Stop()
	select {
	case err := <-waitCh:
		proc.CleanupGroup(pid, grace)
		status, exit := completedStep(err)
		return status, exit, nil
	case <-timer.C:
		if status, exit, natural := stopStep(pid, grace, waitCh); natural {
			return status, exit, nil
		}
		fmt.Fprintf(logf, "\nagentflow gate: timed out after %s; killed the process group\n", st.Timeout)
		return StepTimeout, nil, nil
	case <-ctx.Done():
		if status, exit, natural := stopStep(pid, grace, waitCh); natural {
			return status, exit, nil
		}
		fmt.Fprintf(logf, "\nagentflow gate: interrupted; killed the process group\n")
		return StepInterrupted, nil, nil
	}
}

// stopStep ends a process selected for timeout or interruption, preferring
// a natural exit that won the boundary race. KillGroup repeats that check
// immediately before signaling, closing the gap after the non-blocking read.
func stopStep(pid int, grace time.Duration, waitCh <-chan error) (status string, exit *int, natural bool) {
	select {
	case err := <-waitCh:
		proc.CleanupGroup(pid, grace)
		status, exit = completedStep(err)
		return status, exit, true
	default:
	}
	natural, err := proc.KillGroup(pid, grace, waitCh)
	if !natural {
		return "", nil, false
	}
	status, exit = completedStep(err)
	return status, exit, true
}

func completedStep(err error) (string, *int) {
	code := exitCode(err)
	if err == nil {
		return StepPassed, &code
	}
	return StepFailed, &code
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode() // -1 when killed by a signal
	}
	return -1
}
