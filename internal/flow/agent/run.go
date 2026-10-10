// Package agent runs a coding-agent CLI (Codex or Claude Code) once,
// non-interactively, without the failure modes that make hand-typed
// invocations hang or lie:
//
//   - stdin is never inherited: it is /dev/null or the prompt file, so the
//     agent can never block on, or silently read, the caller's stdin.
//   - the sandbox is always pinned explicitly by the backend (read-only
//     unless Write), never left to the user's config.
//   - a hard timeout and a stall detector kill the whole process group, not
//     just the direct child.
//   - "no final answer" is never reported as success.
//
// Liveness comes from two sources because neither is enough alone: the
// agent's JSON event stream on stdout is silent while the model reasons, and
// its session log only exists once the session has started.
//
// The backends (codex.go, claude.go) differ only in the argv they build, how
// they read events, and where their session logs live.
package agent

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

	"github.com/TysonLabs/agentctl/internal/flow/proc"
)

// Status is the outcome of one run. Every run ends in exactly one.
type Status string

const (
	StatusOK           Status = "ok"            // codex exited 0 and wrote a non-empty final answer
	StatusNoAnswer     Status = "no_answer"     // codex exited 0 but the final answer is missing or empty
	StatusFailed       Status = "codex_failed"  // codex exited non-zero or reported a failed turn
	StatusClaudeFailed Status = "claude_failed" // claude exited non-zero or reported an error result
	StatusRateLimited  Status = "rate_limited"  // codex failed on a usage or rate limit: wait, then retry
	StatusTimeout      Status = "timeout"       // killed: the run exceeded Options.Timeout
	StatusStalled      Status = "stalled"       // killed: no activity for Options.Stall
	StatusInterrupted  Status = "interrupted"   // killed: the caller's context was cancelled

	// statusEndedAfterResult is internal and never reported: agentflow ended
	// a process that outlived its final result, and the stream decides the
	// outcome.
	statusEndedAfterResult Status = "ended_after_result"
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
	Agent        Backend       // Codex (default) or Claude
	Bin          string        // agent executable (default "codex" / "claude")
	Dir          string        // repository / working root
	Prompt       string        // full prompt; empty means native `codex exec review` over Scope (Codex only)
	Scope        Scope         // used natively only when Prompt is empty
	Write        bool          // workspace-write sandbox instead of read-only
	Model        string        // optional model
	MaxBudgetUSD float64       // Claude only: spending cap for the run (0 = none)
	Timeout      time.Duration // hard cap on the whole run
	Stall        time.Duration // kill after this long without any activity
	OutDir       string        // artifacts directory (created if missing)
	Home         string        // agent state dir with session logs (default per backend)
	Poll         time.Duration // watch interval (default 2s)
	Grace        time.Duration // SIGTERM → SIGKILL delay, and how long a process may outlive its final result (default 5s)
}

// ReviewerNote tells a prompt-driven agent that it is the sub-agent, not the
// author: do the one task and stop. Without it, an agent that reads the repo's
// AGENTS.md or CLAUDE.md may try to start a review of its own work.
const ReviewerNote = "You were started by agentflow as a sub-agent for one task: the prompt below. " +
	"Do that task, report, and stop. Do not start other agents or reviews (agentflow, codex, claude), " +
	"do not run commands in the background, " +
	"and do not commit, push, merge or open pull requests."

// Usage is the token usage reported at the end of the run.
type Usage struct {
	InputTokens       int `json:"input_tokens"`
	CachedInputTokens int `json:"cached_input_tokens"`
	OutputTokens      int `json:"output_tokens"`
}

// Result is the machine-readable summary of a run.
type Result struct {
	Status    Status   `json:"status"`
	Agent     string   `json:"agent"`
	Exit      *int     `json:"-"` // rendered as "<agent>_exit"; null when agentflow killed the run
	DurationS float64  `json:"duration_s"`
	Mode      string   `json:"mode"` // "exec" or "review"
	Sandbox   string   `json:"sandbox"`
	Dir       string   `json:"dir"`
	ThreadID  string   `json:"thread_id,omitempty"` // Codex thread id or Claude session id
	Final     string   `json:"final"`               // path of the final answer (may not exist unless ok)
	Prompt    string   `json:"prompt"`              // path of the prompt sent ("" in review mode)
	Events    string   `json:"events"`              // path of the --json event stream
	Stderr    string   `json:"stderr"`
	Rollout   string   `json:"rollout,omitempty"` // the agent's session log
	Usage     *Usage   `json:"usage,omitempty"`
	CostUSD   *float64 `json:"cost_usd,omitempty"` // Claude reports it
	Error     string   `json:"error,omitempty"`
	Args      []string `json:"args"`

	// Protocol is the built-in protocol the prompt used ("fix" or "review").
	// With one, "findings" is always present: the parsed final message, or
	// null when the run failed or the message did not follow the format (a
	// warning then says why). Without one, both are omitted.
	Protocol string     `json:"protocol,omitempty"`
	Findings *[]Finding `json:"findings,omitempty"`
	Warnings []string   `json:"warnings,omitempty"`
}

// SetFindings fills Protocol and Findings from the final answer of a run
// that used protocol p. It never changes Status: a final message that does
// not parse is a warning, not a failed run.
func (r *Result) SetFindings(p Protocol) {
	if p == ProtocolNone {
		return
	}
	r.Protocol = string(p)
	r.Findings = new([]Finding) // points at a nil slice: renders as null
	if r.Status != StatusOK {
		return
	}
	text, err := os.ReadFile(r.Final)
	if err != nil {
		r.Warnings = append(r.Warnings, "findings not parsed: "+err.Error())
		return
	}
	list, err := ParseFindings(string(text))
	if err != nil {
		r.Warnings = append(r.Warnings, "findings not parsed: the final message does not follow the protocol format: "+err.Error())
		return
	}
	r.Findings = &list
}

// MarshalJSON adds the agent's exit code as "codex_exit" or "claude_exit".
func (r Result) MarshalJSON() ([]byte, error) {
	type plain Result
	if r.Agent == Claude.name() {
		return json.Marshal(struct {
			plain
			ClaudeExit *int `json:"claude_exit"`
		}{plain(r), r.Exit})
	}
	return json.Marshal(struct {
		plain
		CodexExit *int `json:"codex_exit"`
	}{plain(r), r.Exit})
}

var rateLimitRe = regexp.MustCompile(`(?i)usage limit|rate limit|too many requests|\b429\b`)

// Run executes the agent once and always returns a Result for a run that started.
// The error is non-nil only when the run could not be started at all.
func Run(ctx context.Context, o Options) (Result, error) {
	o = withDefaults(o)
	b := o.Agent
	res := Result{Agent: b.name(), Dir: o.Dir, Sandbox: sandbox(o.Write)}
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
		if err := os.WriteFile(res.Prompt, []byte(b.wrapPrompt(o.Prompt)), 0o644); err != nil {
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
		if b.needsPrompt() {
			return res, fmt.Errorf("%s needs a prompt: it has no built-in reviewer", b.name())
		}
		res.Mode = "review"
	}
	res.Args = b.args(o, res.Final, isGitRepo(o.Dir))

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

	st := streamState{backend: b, ended: make(chan struct{})}
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
	endedAfterResult := killed == statusEndedAfterResult
	if endedAfterResult {
		killed = "" // proc.KillGroup already reaped the whole group
	} else if killed == "" {
		// cmd.Wait only reaps the direct child. Do not leave helpers from a
		// normally exiting codex process running in its process group.
		proc.CleanupGroup(cmd.Process.Pid, o.Grace)
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
	res.CostUSD = st.costUSD()
	if b.answerInStream() {
		// The answer arrives in the event stream rather than in a file the
		// agent writes; save it where Final points.
		if a := st.answerText(); a != "" {
			if err := os.WriteFile(res.Final, []byte(a), 0o644); err != nil {
				st.setFailure("writing the final answer: " + err.Error())
			}
		}
	}

	if killed != "" {
		res.Status = killed
		res.Error = killMessage(killed, o)
		return res, nil
	}
	// An exit code from a process agentflow ended means nothing: Exit stays
	// null and the stream alone decides the status.
	code := 0
	if !endedAfterResult {
		code = exitCode(waitErr)
		res.Exit = &code
	}
	failure := st.failure()
	switch {
	case code != 0 || failure != "":
		msg := failure
		if msg == "" {
			msg = lastLine(res.Stderr)
		}
		res.Status = b.failed()
		stderrText, _ := os.ReadFile(res.Stderr)
		if rateLimitRe.MatchString(msg) || rateLimitRe.Match(stderrText) {
			res.Status = StatusRateLimited
		}
		res.Error = msg
	case !st.completed() || !nonEmpty(res.Final):
		res.Status = StatusNoAnswer
		if !st.completed() {
			res.Error = b.name() + " exited 0 without a completed turn"
		} else {
			res.Error = b.name() + " exited 0 but wrote no final answer"
		}
	default:
		res.Status = StatusOK
	}
	return res, nil
}

func withDefaults(o Options) Options {
	if o.Agent == nil {
		o.Agent = Codex
	}
	if o.Bin == "" {
		o.Bin = o.Agent.defaultBin()
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
	if o.Home == "" {
		o.Home = o.Agent.defaultHome()
	}
	return o
}

func sandbox(write bool) string {
	if write {
		return "workspace-write"
	}
	return "read-only"
}

func isGitRepo(dir string) bool {
	cmd := exec.Command("git", "-C", dir, "rev-parse", "--is-inside-work-tree")
	out, err := cmd.Output()
	return err == nil && strings.TrimSpace(string(out)) == "true"
}

// watch waits for the process, killing its group on timeout, stall or
// cancellation, or a grace period after the stream reported the end of the
// turn. It returns the kill reason ("" if the agent exited by itself) and the
// agent's wait error.
func watch(ctx context.Context, o Options, pid int, start time.Time, st *streamState, waitCh <-chan error) (Status, error) {
	tick := time.NewTicker(o.Poll)
	defer tick.Stop()
	deadline := time.NewTimer(o.Timeout)
	defer deadline.Stop()
	lastActivity := start
	activity := st.activityCount()
	ended := st.ended // closed by endTurn; set to nil once handled
	var linger <-chan time.Time
	for {
		var reason Status
		select {
		case err := <-waitCh:
			return "", err
		case <-ended:
			ended = nil
			t := time.NewTimer(o.Grace)
			defer t.Stop()
			linger = t.C
		case <-linger:
			reason = statusEndedAfterResult
		case <-ctx.Done():
			reason = StatusInterrupted
		case <-deadline.C:
			reason = StatusTimeout
		case <-tick.C:
			st.refreshRollout(o.Home)
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

// streamState is what the stdout reader learns from the agent's events.
type streamState struct {
	backend  Backend
	activity atomic.Uint64

	mu       sync.Mutex
	thread   string
	rollout  string
	rollSize int64
	rollMod  time.Time
	use      *Usage
	cost     *float64
	answer   string
	fail     string
	complete bool

	// ended is closed when the backend reports the end of the task's turn
	// (endTurn). Backends that never call endTurn run until the agent exits.
	ended     chan struct{}
	turnEnded bool
}

// endTurn records that the task's turn is over; called with s.mu held.
func (s *streamState) endTurn() {
	if !s.turnEnded {
		s.turnEnded = true
		close(s.ended)
	}
}

func (s *streamState) touch()                { s.activity.Add(1) }
func (s *streamState) activityCount() uint64 { return s.activity.Load() }

// consume copies every stdout line to the events file and records what it
// means. Non-JSON lines still count as activity.
func (s *streamState) consume(r *os.File, sink *os.File) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for sc.Scan() {
		line := sc.Bytes()
		s.touch()
		if _, err := sink.Write(append(append([]byte{}, line...), '\n')); err != nil {
			s.setFailure("writing " + s.backend.name() + " event stream: " + err.Error())
		}
		s.mu.Lock()
		s.backend.handle(line, s)
		s.mu.Unlock()
	}
	if err := sc.Err(); err != nil {
		s.setFailure("reading " + s.backend.name() + " event stream: " + err.Error())
	}
}

// refreshRollout finds the session log for this run's thread and treats its
// growth as activity: agents write reasoning there while stdout is silent.
func (s *streamState) refreshRollout(home string) {
	s.mu.Lock()
	thread := s.thread
	s.mu.Unlock()
	if thread == "" || home == "" {
		return
	}
	// When several files match, the lexically last is the newest (Codex
	// names are rollout-<timestamp>-<thread>.jsonl).
	matches, _ := filepath.Glob(s.backend.sessionGlob(home, thread))
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
func (s *streamState) costUSD() *float64     { s.mu.Lock(); defer s.mu.Unlock(); return s.cost }
func (s *streamState) answerText() string    { s.mu.Lock(); defer s.mu.Unlock(); return s.answer }
