package pr

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// ThreadStatus is the outcome of one thread read.
type ThreadStatus string

const (
	ThreadOK          ThreadStatus = "ok"          // the thread was read
	ThreadRefused     ThreadStatus = "refused"     // not a review thread, or not in --repo
	ThreadGHError     ThreadStatus = "gh_error"    // gh failed
	ThreadInterrupted ThreadStatus = "interrupted" // the caller's context was cancelled
)

// ThreadOptions configures one read.
type ThreadOptions struct {
	Thread string // GraphQL node id of the review thread (PRRT_…)
	Repo   string // optional owner/name the thread must belong to
	GH     GH
}

// Comment is one comment of a review thread. Body is sanitized and capped
// like Thread.Body (see SafeBody).
type Comment struct {
	Author        string `json:"author"`
	CreatedAt     string `json:"created_at"`
	URL           string `json:"url,omitempty"`
	Body          string `json:"body"`
	BodyTruncated bool   `json:"body_truncated,omitempty"`
}

// ThreadResult is the machine-readable result of agentflow pr thread.
type ThreadResult struct {
	Status   ThreadStatus `json:"status"`
	Thread   string       `json:"thread"`
	Repo     string       `json:"repo,omitempty"`
	PR       int          `json:"pr,omitempty"`
	Path     string       `json:"path,omitempty"`
	Line     int          `json:"line,omitempty"`
	Resolved bool         `json:"resolved"`
	Outdated bool         `json:"outdated"`
	Comments []Comment    `json:"comments"`
	// Complete is false when GitHub reported more comments than the first
	// 100 that were read.
	Complete  bool   `json:"comments_complete"`
	CheckedAt string `json:"checked_at"`
	Error     string `json:"error,omitempty"`
}

// ValidThreadID reports whether id looks like a review-thread node id.
func ValidThreadID(id string) bool { return threadIDRe.MatchString(id) }

const threadCommentsQuery = `query($t:ID!){node(id:$t){__typename ... on PullRequestReviewThread{` +
	`isResolved isOutdated path line originalLine pullRequest{number repository{nameWithOwner}} ` +
	`comments(first:100){pageInfo{hasNextPage} nodes{author{login} createdAt url body}}}}}`

// ReadThread reads one review thread's comments. It never writes.
func ReadThread(ctx context.Context, o ThreadOptions) ThreadResult {
	res := ThreadResult{Thread: o.Thread, Comments: []Comment{}}
	finish := func(s ThreadStatus, msg string) ThreadResult {
		res.Status, res.Error = s, msg
		res.CheckedAt = time.Now().UTC().Format(time.RFC3339)
		if s != ThreadOK {
			// A refused or failed read reports no partial thread.
			res.Comments, res.Complete = []Comment{}, false
		}
		return res
	}
	if !ValidThreadID(o.Thread) {
		return finish(ThreadRefused, fmt.Sprintf("thread id %q is not a review-thread node id (PRRT_…, as agentflow pr wait prints it)", o.Thread))
	}
	out, err := o.GH(ctx, "api", "graphql", "-F", "t="+o.Thread, "-f", "query="+threadCommentsQuery)
	if ctx.Err() != nil {
		return finish(ThreadInterrupted, "interrupted")
	}
	if err != nil {
		return finish(ThreadGHError, err.Error())
	}
	var doc struct {
		Data struct {
			Node *struct {
				Typename     string `json:"__typename"`
				IsResolved   bool   `json:"isResolved"`
				IsOutdated   bool   `json:"isOutdated"`
				Path         string `json:"path"`
				Line         *int   `json:"line"`
				OriginalLine *int   `json:"originalLine"`
				PullRequest  *struct {
					Number     int `json:"number"`
					Repository struct {
						NameWithOwner string `json:"nameWithOwner"`
					} `json:"repository"`
				} `json:"pullRequest"`
				Comments *struct {
					PageInfo *struct {
						HasNextPage bool `json:"hasNextPage"`
					} `json:"pageInfo"`
					Nodes []struct {
						Author *struct {
							Login string `json:"login"`
						} `json:"author"`
						CreatedAt string `json:"createdAt"`
						URL       string `json:"url"`
						Body      string `json:"body"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"node"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return finish(ThreadGHError, fmt.Sprintf("gh api graphql: %v", err))
	}
	if len(doc.Errors) > 0 {
		return finish(ThreadGHError, "gh api graphql: "+doc.Errors[0].Message)
	}
	n := doc.Data.Node
	if n == nil {
		return finish(ThreadRefused, "no node with id "+o.Thread)
	}
	if n.Typename != "PullRequestReviewThread" {
		return finish(ThreadRefused, fmt.Sprintf("%s: %s is a %s", errNotReviewThread, o.Thread, n.Typename))
	}
	if n.PullRequest == nil || n.Comments == nil || n.Comments.PageInfo == nil || n.PullRequest.Repository.NameWithOwner == "" {
		return finish(ThreadGHError, "gh api graphql: incomplete review thread response")
	}
	res.Repo, res.PR = n.PullRequest.Repository.NameWithOwner, n.PullRequest.Number
	if o.Repo != "" && !strings.EqualFold(o.Repo, res.Repo) {
		return finish(ThreadRefused, fmt.Sprintf("thread belongs to %s, not --repo %s", res.Repo, o.Repo))
	}
	res.Path, res.Resolved, res.Outdated = n.Path, n.IsResolved, n.IsOutdated
	if n.Line != nil {
		res.Line = *n.Line
	} else if n.OriginalLine != nil {
		res.Line = *n.OriginalLine
	}
	for _, c := range n.Comments.Nodes {
		cm := Comment{CreatedAt: c.CreatedAt, URL: c.URL, Author: "ghost"} // GitHub's name for a deleted account
		if c.Author != nil && c.Author.Login != "" {
			cm.Author = c.Author.Login
		}
		cm.Body, cm.BodyTruncated = SafeBody(c.Body)
		res.Comments = append(res.Comments, cm)
	}
	res.Complete = !n.Comments.PageInfo.HasNextPage
	return finish(ThreadOK, "")
}
