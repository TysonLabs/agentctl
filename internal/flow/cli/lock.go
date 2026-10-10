package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/namedlock"
)

const lockUsage = `agentflow lock run --name N [--wait DUR] [--dir D] -- CMD [ARGS...]
agentflow lock status [--name N]

run takes the exclusive named lock N, runs CMD, and releases the lock when
CMD exits. Use it to serialize heavy jobs (builds, test suites) across
parallel agents instead of a mkdir/sleep loop:

  agentflow lock run --name cargo-build -- cargo build --release

  --name N     lock name: 1-64 characters from A-Z a-z 0-9 . _ -
  --wait DUR   give up (exit 75, CMD not run) if the lock is not free within
               DUR; 0 means try once (default: wait as long as it takes)
  --dir D      working directory for CMD (default: current directory)

  CMD runs directly, with no shell; pass "sh -c '...'" for one. stdout and
  stderr are CMD's own. stdin is passed through, except a terminal: CMD runs
  in its own process group, where reading the terminal would stop it, so it
  gets /dev/null instead. SIGINT, SIGTERM, SIGHUP and SIGQUIT are forwarded
  to CMD's process group. While waiting, one stderr line names the holder
  (pid, cmd, dir, since), and another only if the holder changes.

  The lock is flock(2): the kernel releases it when agentflow exits, even on
  SIGKILL. A SIGKILLed agentflow cannot forward the kill, so CMD may outlive
  the lock; kill the process group instead. Processes CMD leaves behind after
  a normal exit are not killed.

status prints {"locks":[...]} on stdout: every lock with a file in the temp
dir (or just N), each with held (a non-blocking flock test), the advisory
holder record, and whether the holder pid is alive. A holder record on a lock
that is not held is stale.

Exit codes: run: CMD's exit code (128+signal if a signal killed it) ·
1 usage · 75 lock not acquired within --wait · 126 CMD could not start ·
127 CMD not found · 130 interrupted while waiting. status: 0 ok · 1 usage
or error.
`

// lockExitCodes: run passes CMD's code through, so its own codes are the
// sysexits/shell conventions that a build command rarely uses.
var lockExitCodes = struct{ ok, usage, timeout, cannotRun, notFound, interrupted int }{0, 1, 75, 126, 127, 130}

// lockStdin is CMD's stdin source; tests replace it.
var lockStdin = os.Stdin

func runLock(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprint(stdout, lockUsage)
		return 0
	}
	switch args[0] {
	case "run":
		return runLockRun(args[1:], stdout, stderr)
	case "status":
		return runLockStatus(args[1:], stdout, stderr)
	}
	fmt.Fprintf(stderr, "agentflow lock: unknown subcommand %q\n\n%s", args[0], lockUsage)
	return lockExitCodes.usage
}

func runLockRun(args []string, stdout, stderr io.Writer) int {
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow lock run: "+format+"\n", a...)
		return code
	}
	fs := flag.NewFlagSet("agentflow lock run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		name, dir string
		wait      time.Duration
	)
	fs.StringVar(&name, "name", "", "")
	fs.StringVar(&dir, "dir", "", "")
	fs.DurationVar(&wait, "wait", 0, "")
	// Flags end at "--" or at the first non-flag, so CMD's own flags are
	// never parsed here.
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stderr, lockUsage)
			return lockExitCodes.usage
		}
		return fail(lockExitCodes.usage, "%v (see: agentflow lock --help)", err)
	}
	waitSet := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "wait" {
			waitSet = true
		}
	})
	cmdArgs := fs.Args()
	if name == "" {
		return fail(lockExitCodes.usage, "--name is required")
	}
	if err := namedlock.ValidateName(name); err != nil {
		return fail(lockExitCodes.usage, "%v", err)
	}
	if len(cmdArgs) == 0 {
		return fail(lockExitCodes.usage, "no command: agentflow lock run --name N -- CMD [ARGS...]")
	}
	if wait < 0 {
		return fail(lockExitCodes.usage, "--wait must not be negative")
	}
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return fail(lockExitCodes.usage, "%v", err)
		}
		dir = wd
	}
	if abs, err := filepath.Abs(dir); err == nil {
		dir = abs
	}
	if fi, err := os.Stat(dir); err != nil || !fi.IsDir() {
		return fail(lockExitCodes.usage, "--dir %s is not a directory", dir)
	}
	path, err := exec.LookPath(cmdArgs[0])
	if err != nil && !strings.Contains(cmdArgs[0], "/") {
		if commandExistsInPath(cmdArgs[0]) {
			return fail(lockExitCodes.cannotRun, "%s: command cannot be run", cmdArgs[0])
		}
		return fail(lockExitCodes.notFound, "%s: command not found", cmdArgs[0])
	}
	if err != nil {
		path = cmdArgs[0] // a path: let Start report the precise error
	}

	// Own the signals from here on: before the command starts they cancel the
	// wait; after, they are forwarded to its process group.
	sigs := make(chan os.Signal, 8)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	defer signal.Stop(sigs)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var timedOut bool
	if waitSet {
		var tcancel context.CancelFunc
		ctx, tcancel = context.WithTimeout(ctx, wait)
		defer tcancel()
	}
	interrupted := make(chan struct{})
	watcherDone := make(chan struct{})
	go func() {
		defer close(watcherDone)
		select {
		case <-sigs:
			close(interrupted)
			cancel()
		case <-ctx.Done():
		}
	}()

	info := namedlock.NewInfo(shellJoin(cmdArgs), dir)
	onHolder := func(h namedlock.Holder) {
		fmt.Fprintf(stderr, "agentflow lock run: waiting for lock %q: %s\n", name, describeHolder(h))
	}
	var l *namedlock.Lock
	if waitSet && wait == 0 {
		l, err = namedlock.TryAcquire(name, info)
		if errors.Is(err, namedlock.ErrBusy) {
			onHolder(namedlock.ReadHolder(name))
			timedOut = true
		}
	} else {
		l, err = namedlock.Acquire(ctx, name, info, onHolder)
	}
	// Stop the interrupt watcher and wait for it, so a signal is either seen
	// here or left queued in sigs for the forwarder, never dropped.
	cancel()
	<-watcherDone
	select {
	case <-interrupted:
		if err == nil {
			_ = l.Release()
		}
		return fail(lockExitCodes.interrupted, "interrupted while waiting for lock %q; command not run", name)
	default:
	}
	if timedOut || errors.Is(err, context.DeadlineExceeded) {
		return fail(lockExitCodes.timeout, "lock %q not acquired within %s; command not run", name, wait)
	}
	if err != nil {
		return fail(lockExitCodes.usage, "lock %q: %v", name, err)
	}
	defer l.Release()

	cmd := exec.Command(path, cmdArgs[1:]...)
	cmd.Args[0] = cmdArgs[0]
	cmd.Dir = dir
	cmd.Stdin = childStdin()
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	// Only matters when stdout/stderr are not *os.File (tests): a leftover
	// grandchild holding a pipe must not keep Wait from returning.
	cmd.WaitDelay = 5 * time.Second
	if err := cmd.Start(); err != nil {
		if errors.Is(err, exec.ErrNotFound) || errors.Is(err, os.ErrNotExist) {
			return fail(lockExitCodes.notFound, "%v", err)
		}
		return fail(lockExitCodes.cannotRun, "%v", err)
	}
	pid := cmd.Process.Pid
	done := make(chan struct{})
	go func() {
		for {
			select {
			case s := <-sigs:
				_ = syscall.Kill(-pid, s.(syscall.Signal))
			case <-done:
				return
			}
		}
	}()
	werr := cmd.Wait()
	close(done)
	if ws, ok := cmd.ProcessState.Sys().(syscall.WaitStatus); ok {
		if ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return ws.ExitStatus()
	}
	if werr != nil {
		return fail(lockExitCodes.cannotRun, "%v", werr)
	}
	return cmd.ProcessState.ExitCode()
}

// childStdin passes stdin through unless it is a terminal (CMD is in a
// background process group there, and a read would stop it with SIGTTIN).
func childStdin() io.Reader {
	if lockStdin == nil {
		return nil
	}
	if isTerminal(lockStdin.Fd()) {
		return nil // exec opens /dev/null
	}
	return lockStdin
}

// commandExistsInPath tells "not executable" (126) from "not found" (127)
// after exec.LookPath, which reports both as not found.
func commandExistsInPath(name string) bool {
	if name == "" {
		return false
	}
	for _, dir := range filepath.SplitList(os.Getenv("PATH")) {
		if dir == "" {
			dir = "."
		}
		// A regular file that LookPath rejected is not executable (126);
		// a directory of that name is skipped, as a shell does.
		if fi, err := os.Stat(filepath.Join(dir, name)); err == nil && fi.Mode().IsRegular() {
			return true
		}
	}
	return false
}

func describeHolder(h namedlock.Holder) string {
	if h.Info == nil {
		return "held by an unknown holder (no holder record)"
	}
	alive := "alive"
	if !h.Alive {
		alive = "not alive: record is stale"
	}
	return fmt.Sprintf("held by pid %d (%s) since %s in %s: %s", h.Info.PID, alive, h.Info.Since, h.Info.Dir, h.Info.Cmd)
}

// shellJoin renders argv for the holder record, quoting where a reader would
// otherwise misread the word boundaries. The record is capped: it is a label.
func shellJoin(argv []string) string {
	parts := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && !strings.ContainsAny(a, " \t\n'\"\\$`;&|<>()*?[]{}~#!") {
			parts[i] = a
			continue
		}
		parts[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	s := strings.Join(parts, " ")
	const max = 512
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

func runLockStatus(args []string, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow lock status: "+format+"\n", a...)
		return lockExitCodes.usage
	}
	fs := flag.NewFlagSet("agentflow lock status", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var name string
	fs.StringVar(&name, "name", "", "")
	if err := fs.Parse(args); err != nil {
		return fail("%v (see: agentflow lock --help)", err)
	}
	if fs.NArg() > 0 {
		return fail("unexpected argument %q", fs.Arg(0))
	}
	var locks []namedlock.State
	if name != "" {
		st, err := namedlock.Status(name)
		if err != nil {
			return fail("%v", err)
		}
		locks = []namedlock.State{st}
	} else {
		var err error
		if locks, err = namedlock.List(); err != nil {
			return fail("%v", err)
		}
	}
	out, _ := json.MarshalIndent(struct {
		Locks []namedlock.State `json:"locks"`
	}{locks}, "", "  ")
	_, _ = stdout.Write(append(out, '\n'))
	return lockExitCodes.ok
}
