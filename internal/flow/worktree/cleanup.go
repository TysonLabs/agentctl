package worktree

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
	"unicode/utf8"
)

// CleanupKey is the git config key (multi-valued, one command per value)
// that names extra cleanup commands `done` runs after it removed a
// worktree, e.g. to drop a per-worktree test database. It is read from the
// repository's own config, never from a tracked file, so a branch cannot
// add commands that run on someone else's machine.
const CleanupKey = "agentflow.worktreeCleanup"

// DefaultCleanupTimeout bounds each cleanup command.
const DefaultCleanupTimeout = 2 * time.Minute

const cleanupOutputMax = 2048

// CleanupResult reports one cleanup command.
type CleanupResult struct {
	Command    []string `json:"command"`
	OK         bool     `json:"ok"`
	ExitCode   int      `json:"exit_code"` // -1 when it did not exit on its own
	TimedOut   bool     `json:"timed_out,omitempty"`
	Error      string   `json:"error,omitempty"`
	Output     string   `json:"output,omitempty"` // the last 2 KB of stdout+stderr
	DurationMS int64    `json:"duration_ms"`
}

// CleanupCommands returns the configured cleanup commands (none is not an
// error).
func CleanupCommands(ctx context.Context, env Env, repoDir string) ([]string, error) {
	env = env.withDefaults()
	cmd := exec.CommandContext(ctx, env.Git, "config", "--local", "--get-all", CleanupKey)
	cmd.Dir = repoDir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	err := cmd.Run()
	var ee *exec.ExitError
	if errors.As(err, &ee) && ee.ExitCode() == 1 && strings.TrimSpace(stderr.String()) == "" {
		return nil, nil // key not set
	}
	if err != nil {
		return nil, fmt.Errorf("git config --local --get-all %s: %v: %s", CleanupKey, err, strings.TrimSpace(stderr.String()))
	}
	return nonEmptyLines(stdout.String()), nil
}

// SplitCommand splits a configured command into argv: words separated by
// whitespace, with '...' and "..." quoting (a backslash escapes " and \
// inside double quotes). No shell is involved.
func SplitCommand(s string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == '\'':
			j := strings.IndexByte(s[i+1:], '\'')
			if j < 0 {
				return nil, errors.New("unterminated ' quote")
			}
			cur.WriteString(s[i+1 : i+1+j])
			i += j + 1
			inWord = true
		case c == '"':
			i++
			for ; i < len(s) && s[i] != '"'; i++ {
				if s[i] == '\\' && i+1 < len(s) && (s[i+1] == '"' || s[i+1] == '\\') {
					i++
				}
				cur.WriteByte(s[i])
			}
			if i >= len(s) {
				return nil, errors.New("unterminated \" quote")
			}
			inWord = true
		case c == ' ' || c == '\t':
			if inWord {
				argv = append(argv, cur.String())
				cur.Reset()
				inWord = false
			}
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		argv = append(argv, cur.String())
	}
	if len(argv) == 0 {
		return nil, errors.New("empty command")
	}
	return argv, nil
}

// CleanupVars are the placeholder values for one removed worktree.
func CleanupVars(path, branch string) map[string]string {
	slug := Slug(branch)
	return map[string]string{
		"{branch}":     branch,
		"{slug}":       slug,
		"{slug_snake}": strings.ReplaceAll(strings.ReplaceAll(slug, "-", "_"), ".", "_"),
		"{dir}":        filepath.Base(path),
		"{path}":       path,
	}
}

// RunCleanup runs each command in dir, without a shell, with placeholders
// substituted per argument (so a value can never split into more words).
// Each command gets stdin /dev/null, its own process group, and timeout;
// a failure is recorded and the next command still runs.
func RunCleanup(ctx context.Context, dir string, commands []string, vars map[string]string, timeout time.Duration) []CleanupResult {
	if timeout <= 0 {
		timeout = DefaultCleanupTimeout
	}
	pairs := make([]string, 0, 2*len(vars))
	for k, v := range vars {
		pairs = append(pairs, k, v)
	}
	repl := strings.NewReplacer(pairs...)
	results := make([]CleanupResult, 0, len(commands))
	for _, line := range commands {
		argv, err := SplitCommand(line)
		if err != nil {
			results = append(results, CleanupResult{Command: []string{line}, ExitCode: -1, Error: "not run: " + err.Error()})
			continue
		}
		for i := range argv {
			argv[i] = repl.Replace(argv[i])
		}
		results = append(results, runCleanupCommand(ctx, dir, argv, vars, timeout))
	}
	return results
}

func runCleanupCommand(ctx context.Context, dir string, argv []string, vars map[string]string, timeout time.Duration) (r CleanupResult) {
	r = CleanupResult{Command: argv, ExitCode: -1}
	start := time.Now()
	defer func() { r.DurationMS = time.Since(start).Milliseconds() }()
	bin := argv[0]
	if strings.ContainsRune(bin, filepath.Separator) && !filepath.IsAbs(bin) {
		bin = filepath.Join(dir, bin) // relative to the main checkout, not to agentflow's cwd
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	cmd := exec.CommandContext(cctx, bin, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(),
		"AGENTFLOW_WORKTREE_PATH="+vars["{path}"],
		"AGENTFLOW_WORKTREE_BRANCH="+vars["{branch}"],
		"AGENTFLOW_WORKTREE_SLUG="+vars["{slug}"])
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 2 * time.Second
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	r.Output = tail(strings.TrimSpace(out.String()), cleanupOutputMax)
	var ee *exec.ExitError
	switch {
	case cctx.Err() == context.DeadlineExceeded:
		r.TimedOut = true
		r.Error = fmt.Sprintf("timed out after %s; its process group was killed", timeout)
	case err == nil:
		r.OK, r.ExitCode = true, 0
	case errors.As(err, &ee):
		r.ExitCode = ee.ExitCode()
		r.Error = err.Error()
	default:
		r.Error = err.Error()
	}
	return r
}

func tail(s string, max int) string {
	if len(s) <= max {
		return s
	}
	i := len(s) - max
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return "…" + s[i:]
}
