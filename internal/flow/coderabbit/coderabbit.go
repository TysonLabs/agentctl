// Package coderabbit runs the CodeRabbit CLI's local review once
// (`coderabbit review --agent`), without the failure modes of a hand-typed
// run:
//
//   - stdin is never inherited (it is /dev/null), so the CLI cannot block on
//     or read the caller's stdin.
//   - a hard timeout and a stall detector kill the whole process group. The
//     CLI prints heartbeat lines while it reviews, so silence means a hang.
//   - one run per repository at a time: stale runs that stack up starve later
//     ones at the "connecting" phase.
//   - success needs the stream's "complete" event, and its findings count
//     must match the findings actually read. Anything else is not "clean".
//
// The stream is JSON lines: review_context, status, heartbeat, finding and a
// final complete event. Finding text is untrusted review data: it is stored
// and reported, never acted on.
package coderabbit

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/proc"
)

// Status is the outcome of one run. Every run ends in exactly one.
type Status string

const (
	StatusClean       Status = "clean"        // review completed, no findings
	StatusFindings    Status = "findings"     // review completed with findings
	StatusFailed      Status = "failed"       // the CLI exited non-zero or reported an error
	StatusNoResult    Status = "no_result"    // exit 0 without a usable complete event
	StatusRateLimited Status = "rate_limited" // the CLI hit a rate or usage limit
	StatusTimeout     Status = "timeout"      // killed: the run exceeded Options.Timeout
	StatusStalled     Status = "stalled"      // killed: no output for Options.Stall
	StatusInterrupted Status = "interrupted"  // killed: the caller's context was cancelled
)

// Scope selects what to review. Exactly one of Base, BaseCommit and
// Uncommitted is set.
type Scope struct {
	Base        string // review committed changes against this branch or ref
	BaseCommit  string // review committed changes against this commit
	Uncommitted bool   // review uncommitted changes
}

// Options configures one run.
type Options struct {
	Bin     string        // coderabbit executable (default "coderabbit")
	Dir     string        // repository working tree
	Scope   Scope         //
	Deep    bool          // use the full pull request review policy
	Configs []string      // absolute paths passed with -c
	Timeout time.Duration // hard cap on the whole run
	Stall   time.Duration // kill after this long without output (0 disables)
	OutDir  string        // artifacts directory (created if missing)
	Poll    time.Duration // watch interval (default 2s)
	Grace   time.Duration // SIGTERM → SIGKILL delay (default 5s)
}

// Finding is one review finding. Text is untrusted data.
type Finding struct {
	Severity    string          `json:"severity"`
	File        string          `json:"file"`
	Text        string          `json:"text"`
	Suggestions json.RawMessage `json:"suggestions,omitempty"`
}

// Result is the machine-readable summary of a run.
type Result struct {
	Status        Status         `json:"status"`
	Exit          *int           `json:"coderabbit_exit"` // null when agentflow killed the run
	DurationS     float64        `json:"duration_s"`
	Dir           string         `json:"dir"`
	ReviewType    string         `json:"review_type,omitempty"`
	Base          string         `json:"base,omitempty"`
	BaseCommit    string         `json:"base_commit,omitempty"`
	ReviewedFiles []string       `json:"reviewed_files"`
	Findings      []Finding      `json:"findings"`
	Severities    map[string]int `json:"severity_counts"`
	Reported      *int           `json:"reported_findings"` // the complete event's count; null if none
	Out           string         `json:"out"`
	Events        string         `json:"events"`
	Stderr        string         `json:"stderr"`
	Error         string         `json:"error,omitempty"`
	Args          []string       `json:"args"`
}

var rateLimitRe = regexp.MustCompile(`(?i)usage limit|rate limit|too many requests|\b429\b|exceeds? (the )?included|out of (review )?credits`)

// Args builds the CodeRabbit argv for o. --deep comes before the scope
// flags and the -c list comes last: --deep takes an optional value and -c is
// variadic, so each must be followed by an option or nothing.
func Args(o Options) []string {
	args := []string{"review", "--agent"}
	if o.Deep {
		args = append(args, "--deep")
	}
	switch {
	case o.Scope.Uncommitted:
		args = append(args, "--uncommitted")
	case o.Scope.BaseCommit != "":
		args = append(args, "--committed", "--base-commit", o.Scope.BaseCommit)
	default:
		args = append(args, "--committed", "--base", o.Scope.Base)
	}
	if len(o.Configs) > 0 {
		args = append(args, "-c")
		args = append(args, o.Configs...)
	}
	return args
}

// LockRepo takes the per-repository run lock. It lives in the repository's
// common git dir, so every worktree of one repository shares it. The
// returned func releases it.
func LockRepo(dir string) (func(), string, error) {
	out, err := exec.Command("git", "-C", dir, "rev-parse", "--git-common-dir").Output()
	if err != nil {
		return nil, "", fmt.Errorf("%s is not inside a git repository", dir)
	}
	common := strings.TrimSpace(string(out))
	if !filepath.IsAbs(common) {
		common = filepath.Join(dir, common)
	}
	path := filepath.Join(common, "agentflow-coderabbit.lock")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return nil, path, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, path, fmt.Errorf("another agentflow coderabbit run is reviewing this repository (lock %s)", path)
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, path, nil
}

// DefaultBase returns the remote-tracking ref of the repository's default
// branch (for example "origin/main"). It reads origin/HEAD, then asks
// lookup (gh) for the default branch name. It never guesses a name.
func DefaultBase(dir string, lookup func(dir string) (string, error)) (string, error) {
	out, err := exec.Command("git", "-C", dir, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD").Output()
	if err == nil {
		if ref := strings.TrimSpace(string(out)); strings.HasPrefix(ref, "refs/remotes/") {
			return strings.TrimPrefix(ref, "refs/remotes/"), nil
		}
	}
	if lookup == nil {
		return "", errors.New("origin/HEAD is not set: pass --base (or run `git remote set-head origin --auto`)")
	}
	name, err := lookup(dir)
	if err != nil || name == "" {
		return "", fmt.Errorf("cannot derive the default branch (origin/HEAD is not set and gh gave none): pass --base")
	}
	for _, ref := range []string{"origin/" + name, name} {
		if exec.Command("git", "-C", dir, "rev-parse", "--verify", "--quiet", ref+"^{commit}").Run() == nil {
			return ref, nil
		}
	}
	return "", fmt.Errorf("default branch %q has no local or origin ref: fetch it or pass --base", name)
}

func withDefaults(o Options) Options {
	if o.Bin == "" {
		o.Bin = "coderabbit"
	}
	if o.Poll <= 0 {
		o.Poll = 2 * time.Second
	}
	if o.Grace <= 0 {
		o.Grace = 5 * time.Second
	}
	return o
}

// Run executes the review once and always returns a Result for a run that
// started. The error is non-nil only when the run could not start at all.
func Run(ctx context.Context, o Options) (Result, error) {
	o = withDefaults(o)
	res := Result{Dir: o.Dir, Out: o.OutDir, ReviewedFiles: []string{}, Findings: []Finding{}, Severities: map[string]int{}}
	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return res, err
	}
	res.Events = filepath.Join(o.OutDir, "events.jsonl")
	res.Stderr = filepath.Join(o.OutDir, "stderr.log")
	res.Args = Args(o)

	events, err := os.Create(res.Events)
	if err != nil {
		return res, err
	}
	defer events.Close()
	stderrFile, err := os.Create(res.Stderr)
	if err != nil {
		return res, err
	}
	defer stderrFile.Close()

	st := &stream{}
	pr, pw, err := os.Pipe()
	if err != nil {
		return res, err
	}
	er, ew, err := os.Pipe()
	if err != nil {
		pr.Close()
		pw.Close()
		return res, err
	}
	cmd := exec.Command(o.Bin, res.Args...)
	cmd.Dir = o.Dir
	// cmd.Stdin stays nil: os/exec connects /dev/null.
	cmd.Stdout = pw
	// Give os/exec a file rather than activityWriter directly. A non-file
	// writer makes cmd.Wait wait for os/exec's internal copy pipe, which an
	// escaped descendant can hold open forever after the process group dies.
	cmd.Stderr = ew
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		er.Close()
		ew.Close()
		return res, fmt.Errorf("starting %s: %w", o.Bin, err)
	}
	pw.Close() // the child holds the write end now
	ew.Close()

	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		st.consume(pr, events)
	}()
	go func() {
		defer readers.Done()
		defer er.Close()
		_, _ = io.Copy(activityWriter{w: stderrFile, st: st}, er)
	}()
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	killed, waitErr := watch(ctx, o, cmd.Process.Pid, st, waitCh)
	res.DurationS = time.Since(start).Round(10 * time.Millisecond).Seconds()
	if killed == "" {
		proc.CleanupGroup(cmd.Process.Pid, o.Grace)
	}
	// A grandchild that left the group could hold either pipe open forever.
	readersDone := make(chan struct{})
	go func() {
		readers.Wait()
		close(readersDone)
	}()
	select {
	case <-readersDone:
	case <-time.After(o.Grace):
		pr.Close()
		er.Close()
		<-readersDone
	}
	pr.Close()
	er.Close()

	st.fill(&res)
	if killed != "" {
		res.Status = killed
		res.Error = killMessage(killed, o)
		return res, nil
	}
	code := exitCode(waitErr)
	res.Exit = &code
	stderrText, _ := os.ReadFile(res.Stderr)
	res.Status, res.Error = decide(code, st, string(stderrText))
	return res, nil
}

// decide maps a run that ended by itself to a status. Only a complete event
// with a matching findings count is a result; a rate limit is named only for
// a run that produced no result.
func decide(code int, st *stream, stderrText string) (Status, string) {
	st.mu.Lock()
	defer st.mu.Unlock()
	limited := func(s Status, msg string) (Status, string) {
		if st.rateLimited || rateLimitRe.MatchString(msg) || rateLimitRe.MatchString(st.messages) || rateLimitRe.MatchString(stderrText) {
			return StatusRateLimited, msg
		}
		return s, msg
	}
	switch {
	case code != 0:
		msg := st.errorMsg
		if msg == "" {
			msg = st.recoverableMsg
		}
		if msg == "" {
			msg = lastLine(stderrText)
		}
		if msg == "" {
			msg = fmt.Sprintf("coderabbit exited %d", code)
		}
		return limited(StatusFailed, msg)
	case st.errorMsg != "":
		return limited(StatusFailed, st.errorMsg)
	case st.complete == nil:
		return limited(StatusNoResult, "coderabbit exited 0 without a complete event")
	case st.complete.Status != "review_completed" || (st.complete.Outcome != "" && st.complete.Outcome != "completed"):
		msg := fmt.Sprintf("review ended with status %q, outcome %q", st.complete.Status, st.complete.Outcome)
		if st.complete.Message != "" {
			msg += ": " + st.complete.Message
		}
		return limited(StatusFailed, msg)
	case st.complete.Findings == nil:
		return limited(StatusNoResult, "the complete event has no findings count")
	case *st.complete.Findings != len(st.findings):
		return limited(StatusNoResult, fmt.Sprintf("the complete event reports %d findings but the stream carried %d", *st.complete.Findings, len(st.findings)))
	case len(st.findings) == 0:
		return StatusClean, ""
	default:
		return StatusFindings, ""
	}
}

// watch waits for the process, killing its group on timeout, stall or
// cancellation. It returns the kill reason ("" if the CLI exited by itself)
// and the CLI's wait error.
func watch(ctx context.Context, o Options, pid int, st *stream, waitCh <-chan error) (Status, error) {
	tick := time.NewTicker(o.Poll)
	defer tick.Stop()
	deadline := time.NewTimer(o.Timeout)
	defer deadline.Stop()
	lastActivity := time.Now()
	activity := st.activity.Load()
	for {
		var reason Status
		select {
		case err := <-waitCh:
			return "", err
		case <-ctx.Done():
			reason = StatusInterrupted
		case <-deadline.C:
			reason = StatusTimeout
		case <-tick.C:
			if current := st.activity.Load(); current != activity {
				activity = current
				lastActivity = time.Now()
			}
			if o.Stall > 0 && time.Since(lastActivity) > o.Stall {
				reason = StatusStalled
			}
		}
		if reason == "" {
			continue
		}
		// Prefer a natural exit that completed before the kill decision.
		select {
		case err := <-waitCh:
			return "", err
		default:
		}
		naturalExit, waitErr := proc.KillGroup(pid, o.Grace, waitCh)
		if naturalExit {
			return "", waitErr
		}
		return reason, waitErr
	}
}

func killMessage(s Status, o Options) string {
	switch s {
	case StatusTimeout:
		return fmt.Sprintf("killed after the %s timeout", o.Timeout)
	case StatusStalled:
		return fmt.Sprintf("killed after %s with no output", o.Stall)
	default:
		return "interrupted"
	}
}

// event is one line of the --agent stream; fields are a union over types.
type event struct {
	Type          string          `json:"type"`
	Status        string          `json:"status"`
	Phase         string          `json:"phase"`
	Message       string          `json:"message"`
	Error         json.RawMessage `json:"error"`
	ErrorType     string          `json:"errorType"`
	Recoverable   *bool           `json:"recoverable"`
	BaseBranch    string          `json:"baseBranch"`
	BaseCommit    string          `json:"baseCommit"`
	ReviewType    string          `json:"reviewType"`
	Severity      string          `json:"severity"`
	FileName      string          `json:"fileName"`
	Instructions  string          `json:"codegenInstructions"`
	Suggestions   json.RawMessage `json:"suggestions"`
	Findings      *int            `json:"findings"`
	ReviewedFiles []string        `json:"reviewedFiles"`
	Outcome       string          `json:"outcome"`
}

// stream accumulates what the reader goroutine parsed.
type stream struct {
	activity atomic.Int64 // bumped on every stdout line and stderr write

	mu       sync.Mutex
	context  *event
	findings []Finding
	complete *event
	errorMsg string // the last non-recoverable error event
	// recoverableMsg is the last error event marked recoverable: it fails a
	// run only when no valid result follows (a nonzero exit or no complete).
	recoverableMsg string
	rateLimited    bool   // an error event typed rate_limit, whatever its message says
	messages       string // every status/complete/error message, for rate-limit detection
}

func (s *stream) consume(r io.Reader, raw io.Writer) {
	br := bufio.NewReaderSize(r, 64*1024)
	for {
		line, err := br.ReadBytes('\n')
		if len(line) > 0 {
			s.activity.Add(1)
			_, _ = raw.Write(line)
			s.handle(line)
		}
		if err != nil {
			return
		}
	}
}

func (s *stream) handle(line []byte) {
	var ev event
	if json.Unmarshal(line, &ev) != nil {
		return // non-JSON lines stay in the raw log only
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if ev.Message != "" {
		s.messages += ev.Message + "\n"
	}
	switch ev.Type {
	case "review_context":
		e := ev
		s.context = &e
	case "finding":
		s.findings = append(s.findings, Finding{Severity: ev.Severity, File: ev.FileName, Text: ev.Instructions, Suggestions: ev.Suggestions})
	case "complete":
		e := ev
		s.complete = &e
	case "error":
		msg := ev.Message
		if msg == "" && len(ev.Error) > 0 {
			msg = string(ev.Error)
		}
		if msg == "" {
			msg = "coderabbit reported an error event"
		}
		if ev.ErrorType == "rate_limit" {
			s.rateLimited = true
		}
		if ev.Recoverable != nil && *ev.Recoverable {
			s.recoverableMsg = msg
		} else {
			s.errorMsg = msg
		}
		s.messages += msg + "\n"
	}
}

func (s *stream) fill(res *Result) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.context != nil {
		res.ReviewType, res.Base, res.BaseCommit = s.context.ReviewType, s.context.BaseBranch, s.context.BaseCommit
	}
	res.Findings = append(res.Findings, s.findings...)
	for _, f := range s.findings {
		sev := f.Severity
		if sev == "" {
			sev = "unknown"
		}
		res.Severities[sev]++
	}
	if s.complete != nil {
		res.Reported = s.complete.Findings
		if s.complete.ReviewedFiles != nil {
			res.ReviewedFiles = s.complete.ReviewedFiles
		}
	}
}

// activityWriter counts stderr writes as liveness.
type activityWriter struct {
	w  io.Writer
	st *stream
}

func (a activityWriter) Write(p []byte) (int, error) {
	a.st.activity.Add(1)
	return a.w.Write(p)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if c := ee.ExitCode(); c >= 0 {
			return c
		}
	}
	return -1
}

func lastLine(s string) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}
