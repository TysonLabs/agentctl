// Package codex runs `codex exec` non-interactively without the failure modes
// that make hand-typed invocations hang or lie:
//
//   - stdin is never inherited: it is /dev/null or the prompt file, so codex
//     can never block on "Reading additional input from stdin...".
//   - the sandbox is always pinned explicitly (`-c sandbox_mode=...`), which
//     also covers `codex exec review`, a subcommand that has no --sandbox flag.
//   - a hard timeout and a stall detector kill the whole process group, not
//     just the direct child.
//   - "no final answer" is never reported as success.
//
// Liveness comes from two sources because neither is enough alone: the
// `--json` event stream on stdout is silent while the model reasons, and
// codex's session rollout file (~/.codex/sessions/.../rollout-*-<thread>.jsonl)
// only exists once the thread has started.
package codex

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// Status is the outcome of one run. Every run ends in exactly one.
type Status string

const (
	StatusOK          Status = "ok"           // codex exited 0 and wrote a non-empty final answer
	StatusNoAnswer    Status = "no_answer"    // codex exited 0 but the final answer is missing or empty
	StatusFailed      Status = "codex_failed" // codex exited non-zero or reported a failed turn
	StatusRateLimited Status = "rate_limited" // codex failed on a usage or rate limit: wait, then retry
	StatusTimeout     Status = "timeout"      // killed: the run exceeded Options.Timeout
	StatusStalled     Status = "stalled"      // killed: no activity for Options.Stall
	StatusInterrupted Status = "interrupted"  // killed: the caller's context was cancelled
)

// Scope selects a diff to review. At most one of Base, Commit and Uncommitted
// is set; Paths optionally narrows the diff to pathspecs.
type Scope struct {
	Base        string
	Commit      string
	Uncommitted bool
	Paths       []string
}

// IsSet reports whether any scope was requested.
func (s Scope) IsSet() bool { return s.Base != "" || s.Commit != "" || s.Uncommitted }

// Options configures one run.
type Options struct {
	Bin       string        // codex executable (default "codex")
	Dir       string        // repository / working root
	Prompt    string        // full prompt; empty means native `codex exec review` over Scope
	Scope     Scope         // used natively only when Prompt is empty
	Write     bool          // workspace-write sandbox instead of read-only
	Model     string        // optional -m
	Timeout   time.Duration // hard cap on the whole run
	Stall     time.Duration // kill after this long without any activity
	OutDir    string        // artifacts directory (created if missing)
	CodexHome string        // where rollouts live (default $CODEX_HOME or ~/.codex)
	Poll      time.Duration // watch interval (default 2s)
	Grace     time.Duration // SIGTERM → SIGKILL delay (default 5s)
}

// Usage is the token usage from the final turn.completed event.
type Usage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}

// Result is the machine-readable summary of a run.
type Result struct {
	Status    Status   `json:"status"`
	CodexExit *int     `json:"codex_exit"` // null when agentflow killed the run
	DurationS float64  `json:"duration_s"`
	Mode      string   `json:"mode"` // "exec" or "review"
	Sandbox   string   `json:"sandbox"`
	Dir       string   `json:"dir"`
	ThreadID  string   `json:"thread_id,omitempty"`
	Final     string   `json:"final"`  // path of the final answer (may not exist unless ok)
	Prompt    string   `json:"prompt"` // path of the prompt sent ("" in review mode)
	Events    string   `json:"events"` // path of the --json event stream
	Stderr    string   `json:"stderr"`
	Rollout   string   `json:"rollout,omitempty"`
	Usage     *Usage   `json:"usage,omitempty"`
	Error     string   `json:"error,omitempty"`
	Args      []string `json:"args"`
}

var rateLimitRe = regexp.MustCompile(`(?i)usage limit|rate limit|too many requests|\b429\b`)

// Run executes codex once and always returns a Result for a run that started.
// The error is non-nil only when the run could not be started at all.
func Run(ctx context.Context, o Options) (Result, error) {
	o = withDefaults(o)
	res := Result{Dir: o.Dir, Sandbox: sandbox(o.Write)}
	if err := os.MkdirAll(o.OutDir, 0o755); err != nil {
		return res, err
	}
	res.Final = filepath.Join(o.OutDir, "final.md")
	res.Events = filepath.Join(o.OutDir, "events.jsonl")
	res.Stderr = filepath.Join(o.OutDir, "stderr.log")
	// A final answer left over from an earlier run in the same OutDir must
	// not read as this run's answer.
	if err := os.Remove(res.Final); err != nil && !errors.Is(err, os.ErrNotExist) {
		return res, err
	}

	var stdin *os.File // nil → os/exec connects /dev/null
	if o.Prompt != "" {
		res.Mode = "exec"
		res.Prompt = filepath.Join(o.OutDir, "prompt.md")
		if err := os.WriteFile(res.Prompt, []byte(o.Prompt), 0o644); err != nil {
			return res, err
		}
		f, err := os.Open(res.Prompt)
		if err != nil {
			return res, err
		}
		defer f.Close()
		stdin = f
	} else {
		if !o.Scope.IsSet() {
			return res, errors.New("nothing to run: give a prompt or a review scope")
		}
		res.Mode = "review"
	}
	res.Args = buildArgs(o, res.Final, isGitRepo(o.Dir))

	events, err := os.Create(res.Events)
	if err != nil {
		return res, err
	}
	defer events.Close()
	stderr, err := os.Create(res.Stderr)
	if err != nil {
		return res, err
	}
	defer stderr.Close()

	pr, pw, err := os.Pipe()
	if err != nil {
		return res, err
	}
	cmd := exec.Command(o.Bin, res.Args...)
	cmd.Dir = o.Dir
	if stdin != nil {
		cmd.Stdin = stdin
	}
	cmd.Stdout = pw
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	start := time.Now()
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return res, fmt.Errorf("starting %s: %w", o.Bin, err)
	}
	pw.Close() // the child holds the write end now

	var st streamState
	st.touch()
	readerDone := make(chan struct{})
	go func() {
		defer close(readerDone)
		defer pr.Close()
		st.consume(pr, events)
	}()

	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()

	killed, waitErr := watch(ctx, o, cmd.Process.Pid, start, &st, waitCh)
	res.DurationS = time.Since(start).Round(10 * time.Millisecond).Seconds()
	if killed == "" {
		// cmd.Wait only reaps the direct child. Do not leave helpers from a
		// normally exiting codex process running in its process group.
		cleanupGroup(cmd.Process.Pid, o.Grace)
	}

	// The pipe closes when every holder of the write end exits. A grandchild
	// that left the process group could keep it open forever: give the reader
	// a moment, then cut it loose.
	select {
	case <-readerDone:
	case <-time.After(o.Grace):
		pr.Close()
		<-readerDone
	}
	pr.Close()

	res.ThreadID = st.threadID()
	res.Rollout = st.rolloutPath()
	res.Usage = st.usage()

	if killed != "" {
		res.Status = killed
		res.Error = killMessage(killed, o)
		return res, nil
	}
	code := exitCode(waitErr)
	res.CodexExit = &code
	failure := st.failure()
	switch {
	case code != 0 || failure != "":
		msg := failure
		if msg == "" {
			msg = lastLine(res.Stderr)
		}
		res.Status = StatusFailed
		stderrText, _ := os.ReadFile(res.Stderr)
		if rateLimitRe.MatchString(msg) || rateLimitRe.Match(stderrText) {
			res.Status = StatusRateLimited
		}
		res.Error = msg
	case !st.completed() || !nonEmpty(res.Final):
		res.Status = StatusNoAnswer
		if !st.completed() {
			res.Error = "codex exited 0 without a completed turn"
		} else {
			res.Error = "codex exited 0 but wrote no final answer"
		}
	default:
		res.Status = StatusOK
	}
	return res, nil
}

func withDefaults(o Options) Options {
	if o.Bin == "" {
		o.Bin = "codex"
	}
	if o.Dir == "" {
		o.Dir, _ = os.Getwd()
	}
	if o.Poll == 0 {
		o.Poll = 2 * time.Second
	}
	if o.Grace == 0 {
		o.Grace = 5 * time.Second
	}
	if o.CodexHome == "" {
		o.CodexHome = os.Getenv("CODEX_HOME")
	}
	if o.CodexHome == "" {
		if home, err := os.UserHomeDir(); err == nil {
			o.CodexHome = filepath.Join(home, ".codex")
		}
	}
	return o
}

func sandbox(write bool) string {
	if write {
		return "workspace-write"
	}
	return "read-only"
}

// buildArgs assembles the codex argv. The sandbox goes through -c because
// `exec review` has no --sandbox flag and otherwise inherits the user's
// config default (seen: workspace-write).
func buildArgs(o Options, finalPath string, inRepo bool) []string {
	args := []string{"exec"}
	if o.Prompt == "" {
		args = append(args, "review")
		switch {
		case o.Scope.Base != "":
			args = append(args, "--base", o.Scope.Base)
		case o.Scope.Commit != "":
			args = append(args, "--commit", o.Scope.Commit)
		case o.Scope.Uncommitted:
			args = append(args, "--uncommitted")
		}
	} else {
		// `exec review` takes no -C; it reviews the working directory, which
		// cmd.Dir already sets. Plain exec gets -C as well, for its prompt.
		args = append(args, "-C", o.Dir)
	}
	args = append(args, "--json", "-c", fmt.Sprintf("sandbox_mode=%q", sandbox(o.Write)), "-o", finalPath)
	if !inRepo {
		args = append(args, "--skip-git-repo-check")
	}
	if o.Model != "" {
		args = append(args, "-m", o.Model)
	}
	if o.Prompt != "" {
		args = append(args, "-") // prompt from stdin, which is the prompt file
	}
	return args
}

func isGitRepo(dir string) bool {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// watch waits for the process, killing its group on timeout, stall or
// cancellation. It returns the kill reason ("" if codex exited by itself)
// and codex's wait error.
func watch(ctx context.Context, o Options, pid int, start time.Time, st *streamState, waitCh <-chan error) (Status, error) {
	tick := time.NewTicker(o.Poll)
	defer tick.Stop()
	deadline := time.NewTimer(o.Timeout)
	defer deadline.Stop()
	lastActivity := start
	activity := st.activityCount()
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
			st.refreshRollout(o.CodexHome)
			if current := st.activityCount(); current != activity {
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
		// Prefer a natural exit that completed before the kill decision. This
		// avoids reporting a boundary-time success as a timeout or stall.
		select {
		case err := <-waitCh:
			return "", err
		default:
		}
		return reason, killGroup(pid, o.Grace, waitCh)
	}
}

// killGroup sends SIGTERM to the whole process group, then SIGKILL after
// grace if codex is still alive. It always reaps the process.
func killGroup(pid int, grace time.Duration, waitCh <-chan error) error {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case err := <-waitCh:
		_ = syscall.Kill(-pid, syscall.SIGKILL) // stragglers in the group
		return err
	case <-time.After(grace):
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return <-waitCh
}

// cleanupGroup terminates descendants left in codex's process group after the
// direct child exits normally. It is bounded even if a descendant ignores
// SIGTERM.
func cleanupGroup(pid int, grace time.Duration) {
	if err := syscall.Kill(-pid, 0); err != nil {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if err := syscall.Kill(-pid, 0); err != nil {
				return
			}
		case <-deadline.C:
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			return
		}
	}
}

func killMessage(s Status, o Options) string {
	switch s {
	case StatusTimeout:
		return fmt.Sprintf("killed after the %s timeout", o.Timeout)
	case StatusStalled:
		return fmt.Sprintf("killed after %s with no events and no session-log growth", o.Stall)
	default:
		return "interrupted"
	}
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

func nonEmpty(path string) bool {
	b, err := os.ReadFile(path)
	return err == nil && strings.TrimSpace(string(b)) != ""
}

func lastLine(path string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	return strings.TrimSpace(lines[len(lines)-1])
}

// streamState is what the stdout reader learns from codex's --json events.
type streamState struct {
	activity atomic.Uint64

	mu       sync.Mutex
	thread   string
	rollout  string
	rollSize int64
	rollMod  time.Time
	use      *Usage
	fail     string
	complete bool
}

func (s *streamState) touch()                { s.activity.Add(1) }
func (s *streamState) activityCount() uint64 { return s.activity.Load() }

type event struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage *Usage `json:"usage"`
}

// consume copies every stdout line to the events file and records what it
// means. Non-JSON lines still count as activity.
func (s *streamState) consume(r *os.File, sink *os.File) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		s.touch()
		if _, err := sink.Write(append(append([]byte{}, line...), '\n')); err != nil {
			s.setFailure("writing codex event stream: " + err.Error())
		}
		var ev event
		if json.Unmarshal(line, &ev) != nil {
			continue
		}
		s.mu.Lock()
		switch ev.Type {
		case "thread.started":
			s.thread = ev.ThreadID
		case "turn.completed":
			s.use = ev.Usage
			s.complete = true
		case "turn.failed":
			if ev.Error != nil && strings.TrimSpace(ev.Error.Message) != "" {
				s.fail = ev.Error.Message
			} else {
				s.fail = "turn failed"
			}
		case "error":
			if strings.TrimSpace(ev.Message) != "" {
				s.fail = ev.Message
			} else {
				s.fail = "codex reported an error"
			}
		}
		s.mu.Unlock()
	}
	if err := sc.Err(); err != nil {
		s.setFailure("reading codex event stream: " + err.Error())
	}
}

// refreshRollout finds the session log for this run's thread and treats its
// growth as activity: codex writes reasoning there while stdout is silent.
func (s *streamState) refreshRollout(codexHome string) {
	s.mu.Lock()
	thread := s.thread
	s.mu.Unlock()
	if thread == "" || codexHome == "" {
		return
	}
	// Names are rollout-<timestamp>-<thread>.jsonl, so the lexically last
	// match is the newest file for this thread.
	matches, _ := filepath.Glob(filepath.Join(codexHome, "sessions", "*", "*", "*", "rollout-*-"+thread+".jsonl"))
	var path string
	for _, match := range matches {
		if match > path {
			path = match
		}
	}
	if path == "" {
		return
	}
	fi, err := os.Stat(path)
	if err != nil {
		return
	}
	s.mu.Lock()
	changed := path != s.rollout || fi.Size() != s.rollSize || !fi.ModTime().Equal(s.rollMod)
	s.rollout = path
	s.rollSize = fi.Size()
	s.rollMod = fi.ModTime()
	s.mu.Unlock()
	if changed {
		// Use our monotonic clock for liveness. Filesystem mtimes may be stale
		// or future-dated and are only change indicators.
		s.touch()
	}
}

func (s *streamState) setFailure(msg string) { s.mu.Lock(); defer s.mu.Unlock(); s.fail = msg }
func (s *streamState) threadID() string      { s.mu.Lock(); defer s.mu.Unlock(); return s.thread }
func (s *streamState) rolloutPath() string   { s.mu.Lock(); defer s.mu.Unlock(); return s.rollout }
func (s *streamState) usage() *Usage         { s.mu.Lock(); defer s.mu.Unlock(); return s.use }
func (s *streamState) failure() string       { s.mu.Lock(); defer s.mu.Unlock(); return s.fail }
func (s *streamState) completed() bool       { s.mu.Lock(); defer s.mu.Unlock(); return s.complete }
