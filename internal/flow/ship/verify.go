// Package ship checks that a deployed service is running an expected commit.
//
// It reads the service's /agent/version through agentctl (a separate
// binary), so agentflow never holds service tokens or speaks HTTP itself.
// Deploys restart the service, so transient fetch failures (HTTP errors,
// timeouts) are retried until the deadline; a response that is readable but
// carries no recognizable commit is a contract problem and fails at once.
package ship

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// Status is the outcome of one verify. Every verify ends in exactly one.
type Status string

const (
	StatusDeployed    Status = "deployed"       // the running commit is (or descends from) the expected one
	StatusNotDeployed Status = "not_deployed"   // --once: it is not, yet
	StatusUnreadable  Status = "unreadable"     // /agent/version answered without a recognizable commit
	StatusAgentctl    Status = "agentctl_error" // agentctl refused (unknown or unwired service, bad config)
	StatusTimeout     Status = "timeout"        // never matched before the deadline
	StatusInterrupted Status = "interrupted"    // the caller's context was cancelled
)

// Fetch returns /agent/version for a service. exitCode is agentctl's:
// 0 ok · 1 usage/config · 2 HTTP >= 400 · 3 transport.
type Fetch func(ctx context.Context, service string) (body []byte, exitCode int, err error)

// Options configures one verify.
type Options struct {
	Service  string        // service.env, as registered with agentctl
	Expected string        // lowercase hex commit, 7..40 chars
	Repo     string        // optional git checkout for the "contains" ancestry check
	Timeout  time.Duration // overall deadline (ignored with Once)
	Interval time.Duration // delay between polls
	Once     bool          // check once instead of waiting
	Contains bool          // accept a running descendant of Expected
	Fetch    Fetch
}

// Result is the machine-readable summary.
type Result struct {
	Status    Status  `json:"status"`
	Service   string  `json:"service"`
	Expected  string  `json:"expected"`
	Running   string  `json:"running,omitempty"`
	Match     string  `json:"match,omitempty"` // "exact" or "contains" (running descends from expected)
	StartedAt string  `json:"started_at,omitempty"`
	Attempts  int     `json:"attempts"`
	DurationS float64 `json:"duration_s"`
	Error     string  `json:"error,omitempty"`
}

var hexRe = regexp.MustCompile(`^[0-9a-f]{7,40}$`)

// IsHexSHA reports whether s looks like an abbreviated or full commit id.
func IsHexSHA(s string) bool { return hexRe.MatchString(strings.ToLower(s)) }

// Verify polls until the service runs the expected commit, the deadline
// passes, or ctx is cancelled.
func Verify(ctx context.Context, o Options) Result {
	start := time.Now()
	res := Result{Service: o.Service, Expected: o.Expected}
	finish := func(s Status, msg string) Result {
		res.Status, res.DurationS = s, time.Since(start).Round(10*time.Millisecond).Seconds()
		if msg != "" {
			res.Error = msg
		}
		return res
	}
	pollCtx := ctx
	cancel := func() {}
	if !o.Once {
		pollCtx, cancel = context.WithTimeout(ctx, o.Timeout)
	}
	defer cancel()
	timedOut := func() Result {
		if res.Error == "" {
			res.Error = fmt.Sprintf("still running %s after %s", res.Running, o.Timeout)
		}
		return finish(StatusTimeout, "")
	}
	for {
		select {
		case <-ctx.Done():
			return finish(StatusInterrupted, "interrupted")
		default:
		}
		if !o.Once {
			select {
			case <-pollCtx.Done():
				return timedOut()
			default:
			}
		}
		res.Attempts++
		body, code, err := o.Fetch(pollCtx, o.Service)
		switch {
		case ctx.Err() != nil:
			return finish(StatusInterrupted, "interrupted")
		case !o.Once && pollCtx.Err() != nil:
			return timedOut()
		case err != nil:
			return finish(StatusAgentctl, err.Error())
		case code == 1:
			return finish(StatusAgentctl, strings.TrimSpace(string(body)))
		case retryableAgentctl(code, body):
			// The service is restarting or unreachable: expected mid-deploy.
			res.Error = fmt.Sprintf("agentctl exit %d: %s", code, firstLine(body))
		case code != 0:
			return finish(StatusAgentctl, fmt.Sprintf("agentctl exit %d: %s", code, firstLine(body)))
		default:
			running, started, perr := ParseVersion(body)
			if perr != nil {
				return finish(StatusUnreadable, perr.Error())
			}
			res.Running, res.StartedAt, res.Error = running, started, ""
			// Exact matching stays textual (prefix, 7+ hex): requiring both ids
			// in a local clone would time out on an unfetched merge commit.
			if Matches(o.Expected, running) {
				if ctx.Err() != nil {
					return finish(StatusInterrupted, "interrupted")
				}
				if !o.Once && pollCtx.Err() != nil {
					return timedOut()
				}
				res.Match = "exact"
				return finish(StatusDeployed, "")
			}
			if o.Contains && o.Repo != "" && isAncestor(pollCtx, o.Repo, o.Expected, running) {
				if ctx.Err() != nil {
					return finish(StatusInterrupted, "interrupted")
				}
				if !o.Once && pollCtx.Err() != nil {
					return timedOut()
				}
				res.Match = "contains"
				return finish(StatusDeployed, "")
			}
		}
		if o.Once {
			if res.Error == "" {
				res.Error = fmt.Sprintf("running %s, not %s", res.Running, o.Expected)
			}
			return finish(StatusNotDeployed, "")
		}
		select {
		case <-ctx.Done():
			return finish(StatusInterrupted, "interrupted")
		case <-pollCtx.Done():
			return timedOut()
		case <-time.After(o.Interval):
		}
	}
}

var httpStatusRe = regexp.MustCompile(`\bHTTP ([0-9]{3})\b`)

func retryableAgentctl(code int, body []byte) bool {
	if code == 3 {
		return true
	}
	if code != 2 {
		return false
	}
	m := httpStatusRe.FindSubmatch(body)
	if m == nil {
		return false
	}
	status := int(m[1][0]-'0')*100 + int(m[1][1]-'0')*10 + int(m[1][2]-'0')
	return status == 404 || status == 408 || status == 425 || status == 429 || (status >= 500 && status <= 599)
}

// Matches reports whether two commit ids name the same commit: one is a
// prefix of the other and the shorter has at least 7 hex digits. Builds
// stamp short SHAs while callers often hold full ones.
func Matches(a, b string) bool {
	a, b = strings.ToLower(a), strings.ToLower(b)
	if !IsHexSHA(a) || !IsHexSHA(b) {
		return false
	}
	if len(a) > len(b) {
		a, b = b, a
	}
	return strings.HasPrefix(b, a)
}

// Fields known to carry a source commit in /agent/version. Generic fields such
// as "sha" and "revision" are deliberately not trusted: they may identify a
// config, image, or schema rather than the running source.
var commitFields = []string{"git_commit", "commit", "git_sha"}

var commitLineRe = regexp.MustCompile(`(?im)^[\t ]*git commit[\t ]*:[\t ]*([0-9a-f]{7,40})[\t ]*$`)

// ParseVersion extracts the running commit (and started_at, if present)
// from an /agent/version body.
func ParseVersion(body []byte) (commit, startedAt string, err error) {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return "", "", fmt.Errorf("/agent/version is not a JSON object: %s", firstLine(body))
	}
	if s, ok := doc["started_at"].(string); ok {
		startedAt = s
	}
	type candidate struct {
		source, commit string
	}
	var candidates []candidate
	for _, f := range commitFields {
		v, present := doc[f]
		if !present {
			continue
		}
		s, ok := v.(string)
		if !ok || !IsHexSHA(s) {
			return "", startedAt, fmt.Errorf("/agent/version field %q is not a commit", f)
		}
		candidates = append(candidates, candidate{f, strings.ToLower(s)})
	}
	// Free-text build banners, e.g. "rustpbx 0.4.4\nGit Commit: f316cb0f".
	if version, ok := doc["version"].(string); ok {
		for _, m := range commitLineRe.FindAllStringSubmatch(version, -1) {
			candidates = append(candidates, candidate{"version", strings.ToLower(m[1])})
		}
	}
	if len(candidates) == 0 {
		return "", startedAt, fmt.Errorf("/agent/version has no commit (looked for %s or a \"Git Commit:\" line in version)", strings.Join(commitFields, ", "))
	}
	commit = candidates[0].commit
	for _, c := range candidates[1:] {
		// Without a repository here, prefix agreement cannot prove that two
		// separately reported abbreviations identify the same object.
		if commit != c.commit {
			return "", startedAt, fmt.Errorf("/agent/version has conflicting commits in %s and %s", candidates[0].source, c.source)
		}
	}
	return commit, startedAt, nil
}

// isAncestor reports whether expected is an ancestor of running in repo. A
// newer deploy that already contains the expected commit counts as deployed.
// Any git failure (e.g. running is not in the local clone) is a plain "no".
func isAncestor(ctx context.Context, repo, expected, running string) bool {
	if running == "" {
		return false
	}
	cmd := exec.CommandContext(ctx, "git", "-C", repo, "merge-base", "--is-ancestor", expected, running)
	return cmd.Run() == nil
}

// AgentctlFetch returns a Fetch that runs `<bin> get <service> /agent/version`.
func AgentctlFetch(bin string, perCall time.Duration) Fetch {
	return func(ctx context.Context, service string) ([]byte, int, error) {
		cctx, cancel := context.WithTimeout(ctx, perCall)
		defer cancel()
		cmd := exec.CommandContext(cctx, bin, "get", service, "/agent/version")
		// After a kill, don't wait on a straggler that still holds our pipes.
		cmd.WaitDelay = time.Second
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		if err == nil {
			return stdout.Bytes(), 0, nil
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() > 0 {
			out := stderr.Bytes()
			if len(bytes.TrimSpace(out)) == 0 {
				out = stdout.Bytes()
			}
			return out, ee.ExitCode(), nil
		}
		if cctx.Err() != nil && ctx.Err() == nil {
			return []byte("agentctl call timed out"), 3, nil // treat as transport
		}
		return nil, 0, fmt.Errorf("running %s: %w", bin, err)
	}
}

func firstLine(b []byte) string {
	s := strings.TrimSpace(string(b))
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		s = s[:i]
	}
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
