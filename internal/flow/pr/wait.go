// Package pr waits for a pull request's CodeRabbit review of its CURRENT head
// and reports the review threads that are still open.
//
// "Reviewed the head" is read from CodeRabbit's summary comment, which the bot
// edits after every review round: it carries `"coveredCommitId":"<sha>"` (and
// "between <base> and <sha>"). Done means that sha is the PR head and the
// comment shows no "review in progress" marker. Two signals that look useful
// are not: review objects with an empty body (they are CodeRabbit's replies
// to thread replies) and the "CodeRabbit" commit status (it can stay PENDING
// after the review is done). A clean round posts no review object at all.
//
// All GitHub access goes through the gh CLI, so agentflow holds no tokens.
package pr

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

	"github.com/TysonLabs/agentctl/internal/flow/render"
)

// Status is the outcome of one wait. Every wait ends in exactly one.
type Status string

const (
	StatusClean       Status = "clean"        // head reviewed, no open CodeRabbit threads
	StatusOpenThreads Status = "open_threads" // head reviewed, open CodeRabbit threads
	StatusSkipped     Status = "skipped"      // review skipped (draft, non-default base, paused)
	StatusRateLimited Status = "rate_limited" // CodeRabbit is rate-limited for this PR
	StatusClosed      Status = "closed"       // PR closed or merged before its head was reviewed
	StatusWaiting     Status = "waiting"      // --once: head not reviewed yet
	StatusGHError     Status = "gh_error"     // gh failed on the first check (bad repo, PR, auth)
	StatusTimeout     Status = "timeout"      // head never reviewed before the deadline
	StatusInterrupted Status = "interrupted"  // the caller's context was cancelled
)

// GH runs the gh CLI with args and returns its stdout.
type GH func(ctx context.Context, args ...string) ([]byte, error)

// Options configures one wait.
type Options struct {
	Repo     string        // owner/name
	PR       int           // pull request number
	Timeout  time.Duration // overall deadline (ignored with Once)
	Interval time.Duration // delay between checks
	Once     bool          // check once instead of waiting
	Bodies   bool          // add each open thread's full first comment and reply count
	GH       GH
}

// Thread is one open CodeRabbit review thread. ID is the GraphQL node id
// that addPullRequestReviewThreadReply and resolveReviewThread take.
type Thread struct {
	ID      string `json:"id"`
	Path    string `json:"path"`
	Line    int    `json:"line,omitempty"`
	URL     string `json:"url,omitempty"`
	Excerpt string `json:"excerpt"`
	// Set only with Options.Bodies. Body is the first comment, sanitized
	// (control characters escaped) and capped at MaxBodyRunes; Replies counts
	// the comments after it.
	Body          *string `json:"body,omitempty"`
	BodyTruncated bool    `json:"body_truncated,omitempty"`
	Replies       *int    `json:"replies,omitempty"`
}

// Result is the machine-readable summary.
type Result struct {
	Status          Status   `json:"status"`
	Repo            string   `json:"repo"`
	PR              int      `json:"pr"`
	Head            string   `json:"head,omitempty"`
	Reviewed        string   `json:"reviewed,omitempty"` // the commit CodeRabbit last covered
	OpenThreads     []Thread `json:"open_threads"`
	ThreadsComplete *bool    `json:"threads_complete,omitempty"` // set once the head is reviewed; false: GitHub still reported more pages
	Next            string   `json:"next,omitempty"`             // what to do next, one line
	Attempts        int      `json:"attempts"`
	DurationS       float64  `json:"duration_s"`
	CheckedAt       string   `json:"checked_at"`
	Error           string   `json:"error,omitempty"`
}

var (
	inProgressRe = regexp.MustCompile(`(?i)review in progress by coderabbit|Currently processing new changes`)
	skippedRe    = regexp.MustCompile(`(?i)Review skipped|Auto reviews are disabled|Review paused`)
	rateRe       = regexp.MustCompile(`(?i)Rate limit exceeded|rate limited`)
	coveredRe    = regexp.MustCompile(`"coveredCommitId":"([0-9a-f]{40})"|between [0-9a-f]{40} and ([0-9a-f]{40})`)
	summaryRe    = regexp.MustCompile(`(?i)walkthrough|coveredCommitId|review in progress|Review skipped|summarize by coderabbit|Rate limit exceeded|rate limited by coderabbit`)
)

const reReview = "comment `@coderabbitai review` on the PR"

// snapshot is one read of the PR.
type snapshot struct {
	head, state, base string
	draft             bool
	summary           string
	threads           []Thread
	threadsComplete   bool
}

// Wait checks until CodeRabbit has reviewed the PR's head, the deadline
// passes, or ctx is cancelled.
func Wait(ctx context.Context, o Options) Result {
	start := time.Now()
	res := Result{Repo: o.Repo, PR: o.PR, OpenThreads: []Thread{}}
	finish := func(s Status, next, msg string) Result {
		res.Status, res.Next = s, next
		res.DurationS = time.Since(start).Round(10 * time.Millisecond).Seconds()
		res.CheckedAt = time.Now().UTC().Format(time.RFC3339)
		if msg != "" {
			res.Error = msg
		}
		return res
	}
	pollCtx, cancel := ctx, func() {}
	if !o.Once {
		pollCtx, cancel = context.WithTimeout(ctx, o.Timeout)
	}
	defer cancel()
	timedOut := func() Result {
		return finish(StatusTimeout, reReview+" once; if nothing comes, ask a human", res.Error)
	}
	for {
		if ctx.Err() != nil {
			return finish(StatusInterrupted, "", "interrupted")
		}
		if !o.Once && pollCtx.Err() != nil {
			return timedOut()
		}
		res.Attempts++
		snap, err := read(pollCtx, o)
		switch {
		case ctx.Err() != nil:
			return finish(StatusInterrupted, "", "interrupted")
		case !o.Once && pollCtx.Err() != nil:
			return timedOut()
		case err != nil && res.Attempts == 1:
			// A wrong repo, PR number or login fails at once, not after 45 min.
			return finish(StatusGHError, "", err.Error())
		case err != nil:
			res.Error = err.Error() // transient: keep waiting
		default:
			res.Error = ""
			if s, next, done := verdict(snap, &res); done {
				return finish(s, next, "")
			}
		}
		if o.Once {
			return finish(StatusWaiting, "run again later, in the background", res.Error)
		}
		select {
		case <-ctx.Done():
			return finish(StatusInterrupted, "", "interrupted")
		case <-pollCtx.Done():
			return timedOut()
		case <-time.After(o.Interval):
		}
	}
}

// verdict fills res from snap and reports whether the wait is over.
func verdict(snap snapshot, res *Result) (Status, string, bool) {
	res.Head = snap.head
	covered := map[string]bool{}
	for _, m := range coveredRe.FindAllStringSubmatch(snap.summary, -1) {
		sha := m[1] + m[2]
		covered[sha] = true
		res.Reviewed = sha // the last one named is the latest round
	}
	inProgress := inProgressRe.MatchString(snap.summary)
	if covered[snap.head] && !inProgress {
		complete := snap.threadsComplete
		res.OpenThreads, res.ThreadsComplete = snap.threads, &complete
		if len(snap.threads) > 0 || !snap.threadsComplete {
			return StatusOpenThreads, "fix each thread, reply (\"Fixed in <sha>: …\" or \"Keeping as-is: …\"), then resolve it", true
		}
		return StatusClean, "", true
	}
	if snap.state != "OPEN" {
		return StatusClosed, "nothing to wait for: the PR is " + strings.ToLower(snap.state) + " and CodeRabbit never reviewed its head", true
	}
	if inProgress {
		return "", "", false // a round is running: older skip/limit text is stale
	}
	if rateRe.MatchString(snap.summary) {
		return StatusRateLimited, "wait the time stated in the CodeRabbit summary comment, then " + reReview, true
	}
	if snap.draft || skippedRe.MatchString(snap.summary) {
		return StatusSkipped, reReview + " (or mark the PR ready for review)", true
	}
	return "", "", false
}

func read(ctx context.Context, o Options) (snapshot, error) {
	var snap snapshot
	out, err := o.GH(ctx, "pr", "view", fmt.Sprint(o.PR), "-R", o.Repo, "--json", "headRefOid,baseRefName,state,isDraft")
	if err != nil {
		return snap, err
	}
	var pv struct {
		HeadRefOid  string `json:"headRefOid"`
		BaseRefName string `json:"baseRefName"`
		State       string `json:"state"`
		IsDraft     bool   `json:"isDraft"`
	}
	if err := json.Unmarshal(out, &pv); err != nil {
		return snap, fmt.Errorf("gh pr view: %w", err)
	}
	snap.head, snap.base, snap.state, snap.draft = strings.ToLower(pv.HeadRefOid), pv.BaseRefName, pv.State, pv.IsDraft

	out, err = o.GH(ctx, "api", "--paginate", "--slurp", fmt.Sprintf("repos/%s/issues/%d/comments?per_page=100", o.Repo, o.PR))
	if err != nil {
		return snap, err
	}
	var pages [][]struct {
		User struct {
			Login string `json:"login"`
		} `json:"user"`
		Body string `json:"body"`
	}
	if err := json.Unmarshal(out, &pages); err != nil {
		return snap, fmt.Errorf("gh api issue comments: %w", err)
	}
	for _, page := range pages {
		for _, c := range page {
			// The bot keeps one summary comment and edits it; take the last match.
			if strings.EqualFold(c.User.Login, "coderabbitai[bot]") && summaryRe.MatchString(c.Body) {
				snap.summary = c.Body
			}
		}
	}

	owner, name, _ := strings.Cut(o.Repo, "/")
	// --paginate follows reviewThreads by $endCursor; --slurp returns every page.
	out, err = o.GH(ctx, "api", "graphql", "--paginate", "--slurp", "-F", "o="+owner, "-F", "n="+name, "-F", fmt.Sprintf("p=%d", o.PR), "-f", "query="+threadsQuery)
	if err != nil {
		return snap, err
	}
	snap.threads, snap.threadsComplete, err = parseThreads(out, o.Bodies)
	return snap, err
}

const threadsQuery = `query($o:String!,$n:String!,$p:Int!,$endCursor:String){repository(owner:$o,name:$n){pullRequest(number:$p){` +
	`reviewThreads(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{id isResolved ` +
	`comments(first:1){totalCount nodes{author{login} path line originalLine url body}}}}}}}`

// threadsPage is one page of the reviewThreads query.
type threadsPage struct {
	Data struct {
		Repository *struct {
			PullRequest *struct {
				ReviewThreads *struct {
					PageInfo *struct {
						HasNextPage bool `json:"hasNextPage"`
					} `json:"pageInfo"`
					Nodes []struct {
						ID         string `json:"id"`
						IsResolved bool   `json:"isResolved"`
						Comments   struct {
							TotalCount int `json:"totalCount"`
							Nodes      []struct {
								Author struct {
									Login string `json:"login"`
								} `json:"author"`
								Path         string `json:"path"`
								Line         *int   `json:"line"`
								OriginalLine *int   `json:"originalLine"`
								URL          string `json:"url"`
								Body         string `json:"body"`
							} `json:"nodes"`
						} `json:"comments"`
					} `json:"nodes"`
				} `json:"reviewThreads"`
			} `json:"pullRequest"`
		} `json:"repository"`
	} `json:"data"`
	Errors []struct {
		Message string `json:"message"`
	} `json:"errors"`
}

// parseThreads reads gh's slurped pages (a JSON array) or a single page,
// and reports whether the last page says there are no more threads.
func parseThreads(out []byte, bodies bool) ([]Thread, bool, error) {
	var pages []threadsPage
	if trimmed := bytes.TrimSpace(out); len(trimmed) > 0 && trimmed[0] == '[' {
		if err := json.Unmarshal(trimmed, &pages); err != nil {
			return nil, false, fmt.Errorf("gh api graphql: %w", err)
		}
	} else {
		var one threadsPage
		if err := json.Unmarshal(out, &one); err != nil {
			return nil, false, fmt.Errorf("gh api graphql: %w", err)
		}
		pages = []threadsPage{one}
	}
	if len(pages) == 0 {
		return nil, false, fmt.Errorf("gh api graphql: no pages")
	}
	threads := []Thread{}
	complete := false
	for _, doc := range pages {
		if len(doc.Errors) > 0 {
			return nil, false, fmt.Errorf("gh api graphql: %s", doc.Errors[0].Message)
		}
		if doc.Data.Repository == nil || doc.Data.Repository.PullRequest == nil || doc.Data.Repository.PullRequest.ReviewThreads == nil {
			return nil, false, fmt.Errorf("gh api graphql: response has no reviewThreads")
		}
		rt := doc.Data.Repository.PullRequest.ReviewThreads
		if rt.PageInfo == nil || rt.Nodes == nil {
			return nil, false, fmt.Errorf("gh api graphql: incomplete reviewThreads response")
		}
		for _, n := range rt.Nodes {
			if n.IsResolved || len(n.Comments.Nodes) == 0 {
				continue
			}
			c := n.Comments.Nodes[0]
			if !strings.EqualFold(c.Author.Login, "coderabbitai") {
				continue
			}
			t := Thread{ID: n.ID, Path: c.Path, URL: c.URL, Excerpt: excerpt(c.Body)}
			if c.Line != nil {
				t.Line = *c.Line
			} else if c.OriginalLine != nil {
				t.Line = *c.OriginalLine // an outdated thread keeps its original line
			}
			if bodies {
				body, truncated := SafeBody(c.Body)
				t.Body, t.BodyTruncated = &body, truncated
				replies := max(n.Comments.TotalCount-1, 0)
				t.Replies = &replies
			}
			threads = append(threads, t)
		}
		complete = !rt.PageInfo.HasNextPage
	}
	return threads, complete, nil
}

// excerpt keeps the first meaningful line of a thread's opening comment
// (CodeRabbit starts with a severity tag line, then a bold title).
func excerpt(body string) string {
	var parts []string
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "<") || strings.HasPrefix(line, "```") {
			continue
		}
		parts = append(parts, line)
		if len(parts) == 2 {
			break
		}
	}
	s := strings.Join(parts, " ")
	if r := []rune(s); len(r) > 240 {
		s = string(r[:240]) + "…"
	}
	return s
}

// MaxBodyRunes caps a comment body in agentflow's output (JSON and text).
const MaxBodyRunes = 16000

// SafeBody prepares untrusted comment text for output: control characters
// and bidi overrides escaped (render.Block), then capped at MaxBodyRunes with
// a note. It reports whether the body was cut.
func SafeBody(body string) (string, bool) {
	return render.Cap(render.Block(body), MaxBodyRunes)
}

// GHCLI returns a GH that runs the gh binary at bin.
func GHCLI(bin string, perCall time.Duration) GH { return GHCLIIn(bin, "", perCall) }

// GHCLIIn is GHCLI run in dir ("" = the current directory), for gh calls
// that read the repository from the directory's git remotes.
func GHCLIIn(bin, dir string, perCall time.Duration) GH {
	return func(ctx context.Context, args ...string) ([]byte, error) {
		cctx, cancel := context.WithTimeout(ctx, perCall)
		defer cancel()
		cmd := exec.CommandContext(cctx, bin, args...)
		cmd.Dir = dir
		cmd.WaitDelay = time.Second
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		if err := cmd.Run(); err != nil {
			msg := strings.TrimSpace(stderr.String())
			if i := strings.IndexByte(msg, '\n'); i >= 0 {
				msg = msg[:i]
			}
			if msg == "" {
				msg = err.Error()
			}
			var ee *exec.ExitError
			if errors.As(err, &ee) {
				return nil, fmt.Errorf("gh %s: %s", args[0], msg)
			}
			return nil, fmt.Errorf("running gh: %s", msg)
		}
		return stdout.Bytes(), nil
	}
}
