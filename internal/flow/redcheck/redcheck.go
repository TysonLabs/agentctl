// Package redcheck proves that a test added with a fix fails without the
// fix. It builds the change's "after" state in a temporary detached
// worktree, reverts the source files to the "before" state while keeping the
// test files at "after", and runs the test there (it must fail: red). Then
// it restores the source files and runs the test again (it must pass:
// green). The user's checkout is never written: the uncommitted scope is
// snapshotted through a temporary index, and the temporary worktree is
// always removed.
package redcheck

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/proc"
)

// Status is the outcome of a redcheck run.
type Status string

const (
	StatusRed          Status = "red"          // fails without the fix (and passes with it, when checked)
	StatusNotRed       Status = "not_red"      // passes without the fix
	StatusInconclusive Status = "inconclusive" // reverting the source would revert tests too, or nothing to revert
	StatusGreenFailed  Status = "green_failed" // fails with the fix too
	StatusError        Status = "error"        // precondition, git or start failure
	StatusTimeout      Status = "timeout"
	StatusInterrupted  Status = "interrupted"
)

// AllStatuses lists every status, for exit-code table tests.
var AllStatuses = []Status{StatusRed, StatusNotRed, StatusInconclusive, StatusGreenFailed, StatusError, StatusTimeout, StatusInterrupted}

// Scope picks the change: exactly one field is set.
type Scope struct {
	Commit      string // the commit vs its first parent
	Base        string // HEAD vs merge-base(Base, HEAD)
	Uncommitted bool   // HEAD plus staged, unstaged and untracked changes vs HEAD
}

// Options configure a run.
type Options struct {
	Dir         string // any directory inside the repository
	Test        string // shell command, run with /bin/sh -c at the worktree root
	Scope       Scope
	TestPaths   []string // extra test patterns, added to DefaultTestPatterns
	Keep        []string // patterns of source files to keep at "after" (fixtures)
	NoGreen     bool
	NoSplice    bool
	CloneTarget string // directory to clone (copy-on-write) into the worktree
	Timeout     time.Duration
	Grace       time.Duration // SIGTERM → SIGKILL grace (default 5s)
	OutDir      string        // logs go here (must exist)
	Git         string        // git binary (default "git")
}

// TestRun is one run of the test command.
type TestRun struct {
	Exit     int     `json:"exit"` // 128+N when killed by signal N; -1 when killed by redcheck
	Signal   string  `json:"signal,omitempty"`
	Secs     float64 `json:"secs"`
	Log      string  `json:"log"`
	TimedOut bool    `json:"timed_out,omitempty"`
}

// SourceTest is a reverted source file whose change also touches tests.
type SourceTest struct {
	Path    string   `json:"path"`
	Reasons []string `json:"reasons"`
}

// Result is the JSON result.
type Result struct {
	Status       Status       `json:"status"`
	Test         string       `json:"test"`
	Scope        string       `json:"scope"`
	Before       string       `json:"before"`
	After        string       `json:"after"`
	Red          *TestRun     `json:"red"`
	Green        *TestRun     `json:"green"`
	Reverted     []string     `json:"reverted"`
	Spliced      []string     `json:"spliced"`
	KeptTests    []string     `json:"kept_tests"`
	Kept         []string     `json:"kept"`
	TestInSource []SourceTest `json:"test_in_source,omitempty"`
	Worktree     string       `json:"worktree,omitempty"` // the temporary worktree (removed on exit)
	CleanupError string       `json:"cleanup_error,omitempty"`
	Notes        []string     `json:"notes,omitempty"`
	Error        string       `json:"error,omitempty"`
	Next         string       `json:"next,omitempty"`
}

type change struct {
	status byte // git name-status: A, M, D, T
	path   string
}

type runner struct {
	o   Options
	top string
	res *Result
}

// Run performs the check. It always returns a Result; a failure before or
// around the test runs is StatusError with Error set.
func Run(ctx context.Context, o Options) Result {
	if o.Git == "" {
		o.Git = "git"
	}
	if o.Grace <= 0 {
		o.Grace = 5 * time.Second
	}
	res := Result{Test: o.Test, Reverted: []string{}, Spliced: []string{}, KeptTests: []string{}, Kept: []string{}}
	r := &runner{o: o, res: &res}
	if err := r.run(ctx); err != nil {
		res.Status = StatusError
		if ctx.Err() != nil {
			res.Status = StatusInterrupted
		}
		res.Error = err.Error()
	}
	return res
}

func (r *runner) run(ctx context.Context) error {
	top, err := r.git(ctx, r.o.Dir, nil, "rev-parse", "--show-toplevel")
	if err != nil {
		return fmt.Errorf("%s is not inside a git checkout: %v", r.o.Dir, err)
	}
	r.top = strings.TrimSpace(top)

	tmp, err := os.MkdirTemp("", "agentflow-redcheck-")
	if err != nil {
		return err
	}
	if resolved, err := filepath.EvalSymlinks(tmp); err == nil {
		tmp = resolved
	}
	wt := filepath.Join(tmp, "wt")
	added := false
	defer func() { r.cleanup(tmp, wt, added) }()

	if err := r.resolveScope(ctx, tmp); err != nil {
		return err
	}
	changes, err := r.diff(ctx)
	if err != nil {
		return err
	}
	if len(changes) == 0 {
		return fmt.Errorf("no changes between %s (before) and %s (after)", short(r.res.Before), short(r.res.After))
	}
	revert, splices, err := r.classify(ctx, changes)
	if err != nil {
		return err
	}
	if len(r.res.TestInSource) > 0 {
		r.res.Status = StatusInconclusive
		r.res.Next = "reverting these source files would also revert test code, so a red result would not prove the test. " +
			"Move the test into its own test file, or pass --keep <file> (that keeps the file's fix too, so the check no longer covers it)."
		return nil
	}
	if len(revert) == 0 {
		r.res.Status = StatusInconclusive
		r.res.Next = "the change has no source files to revert (only test or --keep files), so there is no fix to prove the test against"
		return nil
	}

	added = true // before the add: an interrupted add can leave a partial record
	if _, err := r.git(ctx, r.top, nil, "worktree", "add", "--detach", wt, r.res.After); err != nil {
		return fmt.Errorf("creating the temporary worktree: %v", err)
	}
	r.res.Worktree = wt
	if r.o.CloneTarget != "" {
		r.cloneTarget(ctx, wt)
	}
	if v := os.Getenv("CARGO_TARGET_DIR"); v != "" {
		r.res.Notes = append(r.res.Notes, "CARGO_TARGET_DIR is set ("+v+"): cargo builds there, not in the temporary worktree")
	}

	// Red: before's source, after's tests.
	if err := r.apply(ctx, wt, revert, splices, true); err != nil {
		return err
	}
	red, kill, err := r.runTest(ctx, wt, "red")
	if err != nil {
		return err
	}
	r.res.Red = red
	if kill != "" {
		r.res.Status = kill
		r.res.Next = "the red run (without the fix) did not finish; see " + red.Log
		return nil
	}
	if red.Exit == 126 || red.Exit == 127 {
		return fmt.Errorf("the test command could not run (exit %d, see %s)", red.Exit, red.Log)
	}
	if red.Exit == 0 {
		r.res.Status = StatusNotRed
		r.res.Next = "the test passes without the fix, so it does not prove the fix. Check that --test runs the new test and that 'reverted' lists the fix."
		return nil
	}
	r.res.Status = StatusRed
	if r.o.NoGreen {
		r.res.Notes = append(r.res.Notes, "green run skipped (--no-green): a test that always fails also looks red")
		return nil
	}

	// Green: the full after state.
	if err := r.apply(ctx, wt, revert, splices, false); err != nil {
		return err
	}
	green, kill, err := r.runTest(ctx, wt, "green")
	if err != nil {
		return err
	}
	r.res.Green = green
	switch {
	case kill != "":
		r.res.Status = kill
		r.res.Next = "the green run (with the fix) did not finish; see " + green.Log
	case green.Exit != 0:
		r.res.Status = StatusGreenFailed
		r.res.Next = "the test fails with the fix too, so the red run proves nothing; see " + green.Log
	}
	return nil
}

// resolveScope sets Before, After and Scope.
func (r *runner) resolveScope(ctx context.Context, tmp string) error {
	s := r.o.Scope
	rev := func(ref string) (string, error) {
		out, err := r.git(ctx, r.top, nil, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
		if err != nil {
			return "", fmt.Errorf("%q is not a commit", ref)
		}
		return strings.TrimSpace(out), nil
	}
	var err error
	switch {
	case s.Commit != "":
		r.res.Scope = "commit " + s.Commit
		if r.res.After, err = rev(s.Commit); err != nil {
			return err
		}
		if r.res.Before, err = rev(r.res.After + "^1"); err != nil {
			return fmt.Errorf("commit %s has no parent to compare against", short(r.res.After))
		}
	case s.Base != "":
		r.res.Scope = "base " + s.Base
		if r.res.After, err = rev("HEAD"); err != nil {
			return err
		}
		if _, err = rev(s.Base); err != nil {
			return err
		}
		out, err := r.git(ctx, r.top, nil, "merge-base", s.Base, "HEAD")
		if err != nil {
			return fmt.Errorf("no merge base between %s and HEAD", s.Base)
		}
		r.res.Before = strings.TrimSpace(out)
		if dirty, _ := r.git(ctx, r.top, nil, "status", "--porcelain", "--untracked-files=no"); strings.TrimSpace(dirty) != "" {
			r.res.Notes = append(r.res.Notes, "the checkout has uncommitted changes; --base checks committed HEAD only (use --uncommitted to include them)")
		}
	case s.Uncommitted:
		r.res.Scope = "uncommitted"
		if r.res.Before, err = rev("HEAD"); err != nil {
			return err
		}
		if r.res.After, err = r.snapshot(ctx, tmp); err != nil {
			return err
		}
	default:
		return errors.New("no scope: pass --commit, --base or --uncommitted")
	}
	return nil
}

// snapshot records HEAD plus every staged, unstaged and untracked
// (non-ignored) change as a commit, through a temporary index, so neither
// the user's index nor their working tree is touched.
func (r *runner) snapshot(ctx context.Context, tmp string) (string, error) {
	env := []string{"GIT_INDEX_FILE=" + filepath.Join(tmp, "snapshot.index")}
	if _, err := r.git(ctx, r.top, env, "read-tree", "HEAD"); err != nil {
		return "", err
	}
	if _, err := r.git(ctx, r.top, env, "add", "-A", "--", "."); err != nil {
		return "", fmt.Errorf("snapshotting the working tree: %v", err)
	}
	tree, err := r.git(ctx, r.top, env, "write-tree")
	if err != nil {
		return "", err
	}
	env = append(env,
		"GIT_AUTHOR_NAME=agentflow redcheck", "GIT_AUTHOR_EMAIL=redcheck@agentflow.invalid",
		"GIT_COMMITTER_NAME=agentflow redcheck", "GIT_COMMITTER_EMAIL=redcheck@agentflow.invalid")
	out, err := r.git(ctx, r.top, env, "commit-tree", strings.TrimSpace(tree), "-p", r.res.Before, "-m", "agentflow redcheck snapshot of uncommitted changes")
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(out), nil
}

func (r *runner) diff(ctx context.Context) ([]change, error) {
	out, err := r.git(ctx, r.top, nil, "diff", "--no-renames", "--no-ext-diff", "--name-status", "-z", r.res.Before, r.res.After, "--")
	if err != nil {
		return nil, err
	}
	fields := strings.Split(strings.TrimSuffix(out, "\x00"), "\x00")
	var changes []change
	for i := 0; i+1 < len(fields); i += 2 {
		if fields[i] == "" {
			continue
		}
		changes = append(changes, change{status: fields[i][0], path: fields[i+1]})
	}
	return changes, nil
}

// classify sorts the changed files into kept tests, kept files and source
// files to revert (or splice), and records source files whose own diff
// touches tests.
func (r *runner) classify(ctx context.Context, changes []change) ([]change, map[string]string, error) {
	testPats := append(append([]string{}, DefaultTestPatterns...), r.o.TestPaths...)
	var revert []change
	splices := map[string]string{}
	keepUsed := map[string]bool{}
	for _, c := range changes {
		switch {
		case matchAny(testPats, c.path):
			r.res.KeptTests = append(r.res.KeptTests, c.path)
			continue
		case matchAny(r.o.Keep, c.path):
			r.res.Kept = append(r.res.Kept, c.path)
			for _, k := range r.o.Keep {
				if Match(k, c.path) {
					keepUsed[k] = true
				}
			}
			continue
		}
		revert = append(revert, c)
		if markerFor(c.path) == nil {
			continue
		}
		before, err := r.blob(ctx, r.res.Before, c.path, c.status != 'A')
		if err != nil {
			return nil, nil, err
		}
		after, err := r.blob(ctx, r.res.After, c.path, c.status != 'D')
		if err != nil {
			return nil, nil, err
		}
		v := judgeSource(c.path, c.status, before, after, !r.o.NoSplice)
		switch {
		case len(v.reasons) > 0:
			r.res.TestInSource = append(r.res.TestInSource, SourceTest{Path: c.path, Reasons: v.reasons})
		case v.splice != "":
			splices[c.path] = v.splice
			r.res.Spliced = append(r.res.Spliced, c.path)
		}
	}
	for _, c := range revert {
		if _, ok := splices[c.path]; !ok {
			r.res.Reverted = append(r.res.Reverted, c.path)
		}
	}
	for _, k := range r.o.Keep {
		if !keepUsed[k] {
			r.res.Notes = append(r.res.Notes, fmt.Sprintf("--keep %s matched no changed source file", k))
		}
	}
	sort.Strings(r.res.Spliced)
	return revert, splices, nil
}

func (r *runner) blob(ctx context.Context, commit, p string, exists bool) (string, error) {
	if !exists {
		return "", nil
	}
	return r.git(ctx, r.top, nil, "cat-file", "blob", commit+":"+p)
}

// apply sets the source files to the red state (before's source; spliced
// files get before's code with after's inline tests) or back to the green
// state (after).
func (r *runner) apply(ctx context.Context, wt string, revert []change, splices map[string]string, red bool) error {
	commit := r.res.After
	if red {
		commit = r.res.Before
	}
	var checkout []string
	for _, c := range revert {
		gone := (red && c.status == 'A') || (!red && c.status == 'D')
		switch {
		case gone:
			if err := os.Remove(filepath.Join(wt, filepath.FromSlash(c.path))); err != nil && !errors.Is(err, os.ErrNotExist) {
				return err
			}
		case red && splices[c.path] != "":
			if err := os.WriteFile(filepath.Join(wt, filepath.FromSlash(c.path)), []byte(splices[c.path]), 0o644); err != nil {
				return err
			}
		default:
			checkout = append(checkout, c.path)
		}
	}
	if len(checkout) == 0 {
		return nil
	}
	list := filepath.Join(filepath.Dir(wt), "pathspec")
	if err := os.WriteFile(list, []byte(strings.Join(checkout, "\x00")+"\x00"), 0o600); err != nil {
		return err
	}
	if _, err := r.git(ctx, wt, nil, "checkout", commit, "--pathspec-from-file="+list, "--pathspec-file-nul"); err != nil {
		return fmt.Errorf("setting the %s state: %v", map[bool]string{true: "red", false: "green"}[red], err)
	}
	return nil
}

// runTest runs the test command in the worktree. kill is StatusTimeout or
// StatusInterrupted when redcheck ended the run, "" when it exited itself.
func (r *runner) runTest(ctx context.Context, wt, phase string) (*TestRun, Status, error) {
	logPath := filepath.Join(r.o.OutDir, phase+".log")
	f, err := os.Create(logPath)
	if err != nil {
		return nil, "", err
	}
	defer f.Close()
	cmd := exec.Command("/bin/sh", "-c", r.o.Test)
	cmd.Dir = wt
	cmd.Env = append(cleanEnv(), "AGENTFLOW_REDCHECK_PHASE="+phase)
	// cmd.Stdin stays nil: os/exec connects /dev/null. A file (not a pipe)
	// for output, so Wait never blocks on an escaped descendant.
	cmd.Stdout, cmd.Stderr = f, f
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	start := time.Now()
	if err := cmd.Start(); err != nil {
		return nil, "", fmt.Errorf("starting the test command: %v", err)
	}
	pid := cmd.Process.Pid
	waitCh := make(chan error, 1)
	go func() { waitCh <- cmd.Wait() }()
	deadline := time.NewTimer(r.o.Timeout)
	defer deadline.Stop()

	var kill Status
	var waitErr error
	select {
	case waitErr = <-waitCh:
	case <-ctx.Done():
		kill = StatusInterrupted
	case <-deadline.C:
		kill = StatusTimeout
	}
	if kill != "" {
		select {
		case waitErr = <-waitCh: // it finished just before the kill decision
			kill = ""
		default:
			natural, err := proc.KillGroup(pid, r.o.Grace, waitCh)
			waitErr = err
			if natural {
				kill = ""
			}
		}
	}
	if kill == "" {
		proc.CleanupGroup(pid, r.o.Grace)
	}
	run := &TestRun{Log: logPath, Secs: time.Since(start).Round(10 * time.Millisecond).Seconds()}
	if kill != "" {
		run.Exit = -1
		run.TimedOut = kill == StatusTimeout
		fmt.Fprintf(f, "\nagentflow redcheck: %s run %s after %s\n", phase, map[bool]string{true: "timed out", false: "interrupted"}[run.TimedOut], time.Since(start).Round(time.Millisecond))
		return run, kill, nil
	}
	run.Exit, run.Signal = exitStatus(waitErr)
	return run, "", nil
}

func exitStatus(err error) (int, string) {
	if err == nil {
		return 0, ""
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal()), ws.Signal().String()
		}
		return ee.ExitCode(), ""
	}
	return -1, err.Error()
}

// cloneTarget copy-on-write clones the build directory into the worktree,
// at the same relative path when it lies inside the checkout. Without a
// clone-capable cp it skips with a note: a full copy could be huge.
func (r *runner) cloneTarget(ctx context.Context, wt string) {
	src, err := filepath.Abs(r.o.CloneTarget)
	if err != nil {
		r.res.Notes = append(r.res.Notes, "--clone-target: "+err.Error())
		return
	}
	if fi, err := os.Stat(src); err != nil || !fi.IsDir() {
		r.res.Notes = append(r.res.Notes, "--clone-target "+src+" is not a directory; skipped")
		return
	}
	rel := filepath.Base(src)
	if top, err := filepath.EvalSymlinks(r.top); err == nil {
		if s, err := filepath.EvalSymlinks(src); err == nil {
			if p, err := filepath.Rel(top, s); err == nil && p != "." && !strings.HasPrefix(p, "..") {
				rel = p
			}
		}
	}
	dest := filepath.Join(wt, rel)
	if _, err := os.Lstat(dest); err == nil {
		r.res.Notes = append(r.res.Notes, "--clone-target: "+rel+" already exists in the worktree; skipped")
		return
	}
	var args []string
	switch runtime.GOOS {
	case "darwin":
		args = []string{"-c", "-R", src, dest}
	case "linux":
		args = []string{"-R", "--reflink=always", src, dest}
	default:
		r.res.Notes = append(r.res.Notes, "--clone-target: no copy-on-write cp on "+runtime.GOOS+"; skipped")
		return
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		r.res.Notes = append(r.res.Notes, "--clone-target: "+err.Error())
		return
	}
	if out, err := exec.CommandContext(ctx, "cp", args...).CombinedOutput(); err != nil {
		_ = os.RemoveAll(dest)
		r.res.Notes = append(r.res.Notes, fmt.Sprintf("--clone-target: copy-on-write clone failed (%v: %s); skipped", err, strings.TrimSpace(string(out))))
		return
	}
	r.res.Notes = append(r.res.Notes, "--clone-target: cloned "+src+" to "+rel)
}

// cleanup removes the temporary worktree and its record, even after an
// interrupt (it does not use the run's context).
func (r *runner) cleanup(tmp, wt string, added bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	var errs []string
	if added {
		// Its error is checked below through the worktree list: after an
		// interrupted add there may be nothing to remove.
		_, _ = r.git(ctx, r.top, nil, "worktree", "remove", "--force", wt)
	}
	if err := os.RemoveAll(tmp); err != nil {
		errs = append(errs, err.Error())
	}
	if added {
		if _, err := r.git(ctx, r.top, nil, "worktree", "prune"); err != nil {
			errs = append(errs, err.Error())
		}
		if out, err := r.git(ctx, r.top, nil, "worktree", "list", "--porcelain"); err == nil && strings.Contains(out, "worktree "+wt+"\n") {
			errs = append(errs, "the temporary worktree is still registered: "+wt)
		}
	}
	if len(errs) > 0 {
		r.res.CleanupError = strings.Join(errs, "; ")
	}
}

// git runs git with hooks disabled, literal pathspecs and the environment
// cleaned of variables that would redirect it to another repository.
func (r *runner) git(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	full := append([]string{"-c", "core.hooksPath=/dev/null", "--literal-pathspecs"}, args...)
	cmd := exec.CommandContext(ctx, r.o.Git, full...)
	cmd.Dir = dir
	cmd.Env = append(cleanEnv(), env...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// cleanEnv drops the variables that point git at a specific repository or
// index (set when agentflow runs from a git hook), so neither git nor the
// test command can reach the user's checkout through them.
func cleanEnv() []string {
	drop := map[string]bool{
		"GIT_DIR": true, "GIT_WORK_TREE": true, "GIT_INDEX_FILE": true, "GIT_COMMON_DIR": true,
		"GIT_OBJECT_DIRECTORY": true, "GIT_ALTERNATE_OBJECT_DIRECTORIES": true, "GIT_PREFIX": true,
	}
	var env []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if !drop[k] {
			env = append(env, kv)
		}
	}
	return env
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
