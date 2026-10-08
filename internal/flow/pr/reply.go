package pr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// ReplyStatus is the outcome of one reply. Every reply ends in exactly one.
type ReplyStatus string

const (
	ReplyDone        ReplyStatus = "done"         // the thread carries the reply and is resolved
	ReplyRefused     ReplyStatus = "refused"      // a check failed; nothing was posted
	ReplyNotResolved ReplyStatus = "not_resolved" // the reply may be posted, but resolution is not verified: run it again
	ReplyGHError     ReplyStatus = "gh_error"     // gh failed before anything was posted
	ReplyInterrupted ReplyStatus = "interrupted"  // cancelled before anything was posted
)

// ReplyOptions configures one reply. Exactly one of Fixed (with Note) and
// Keep is set.
type ReplyOptions struct {
	Thread string // GraphQL node id of the review thread (PRRT_…)
	Repo   string // optional owner/name the thread must belong to
	Fixed  string // commit that fixes the finding
	Note   string // what the fix changed
	Keep   string // why the code stays as it is
	GH     GH
}

// ReplyResult is the machine-readable summary.
type ReplyResult struct {
	Status    ReplyStatus `json:"status"`
	Thread    string      `json:"thread"`
	Repo      string      `json:"repo,omitempty"`
	PR        int         `json:"pr,omitempty"`
	Body      string      `json:"body"`
	Replied   bool        `json:"replied"`  // a reply was posted by this run
	Resolved  bool        `json:"resolved"` // the thread was resolved by this run
	ReplyURL  string      `json:"reply_url,omitempty"`
	Next      string      `json:"next,omitempty"`
	CheckedAt string      `json:"checked_at"`
	Error     string      `json:"error,omitempty"`
}

var (
	threadIDRe = regexp.MustCompile(`^PRRT_[A-Za-z0-9_-]+$`)
	shaRe      = regexp.MustCompile(`^[0-9a-fA-F]{7,40}$`)
)

// ReplyBody is the comment text for o: "Fixed in <sha>: <note>" or
// "Keeping as-is: <reason>".
func ReplyBody(o ReplyOptions) string {
	if o.Fixed != "" {
		return "Fixed in " + o.Fixed + ": " + strings.TrimSpace(o.Note)
	}
	return "Keeping as-is: " + strings.TrimSpace(o.Keep)
}

// ValidateReply checks o's own fields, before any gh call.
func ValidateReply(o ReplyOptions) error {
	switch {
	case !threadIDRe.MatchString(o.Thread):
		return fmt.Errorf("thread id %q is not a review-thread node id (PRRT_…, as agentflow pr wait prints it)", o.Thread)
	case (o.Fixed == "") == (o.Keep == ""):
		return fmt.Errorf("give exactly one of --fixed SHA (with --note) and --keep REASON")
	case o.Fixed != "" && !shaRe.MatchString(o.Fixed):
		return fmt.Errorf("--fixed %q is not a commit sha (7-40 hex characters)", o.Fixed)
	case o.Fixed != "" && strings.TrimSpace(o.Note) == "":
		return fmt.Errorf("--fixed needs --note: say what the fix changed")
	case o.Fixed == "" && o.Note != "":
		return fmt.Errorf("--note goes with --fixed; put the reason in --keep")
	case o.Keep != "" && strings.TrimSpace(o.Keep) == "":
		return fmt.Errorf("--keep needs a reason")
	}
	return nil
}

const threadQuery = `query($t:ID!){node(id:$t){__typename ... on PullRequestReviewThread{` +
	`isResolved viewerCanReply pullRequest{number repository{nameWithOwner}} ` +
	`comments(last:100){pageInfo{hasPreviousPage} nodes{viewerDidAuthor body url}}}}}`

const replyMutation = `mutation($t:ID!,$b:String!){addPullRequestReviewThreadReply(input:{pullRequestReviewThreadId:$t,body:$b}){comment{id url}}}`

const resolveMutation = `mutation($t:ID!){resolveReviewThread(input:{threadId:$t}){thread{isResolved}}}`

// threadState is what Reply needs to know about a thread.
type threadState struct {
	repo        string
	pr          int
	resolved    bool
	canReply    bool
	hasOurReply bool // the viewer already posted exactly this body
	ourReplyURL string
	complete    bool // every comment was read (no earlier page)
}

var errNotReviewThread = errors.New("not a pull request review thread")

// Reply posts the reply to a review thread, then resolves it. A thread that
// already carries the viewer's identical reply is not replied to again, so a
// run that failed to resolve can simply be repeated.
func Reply(ctx context.Context, o ReplyOptions) ReplyResult {
	res := ReplyResult{Thread: o.Thread, Body: ReplyBody(o)}
	finish := func(s ReplyStatus, next, msg string) ReplyResult {
		res.Status, res.Next, res.Error = s, next, msg
		res.CheckedAt = time.Now().UTC().Format(time.RFC3339)
		return res
	}
	if err := ValidateReply(o); err != nil {
		return finish(ReplyRefused, "", err.Error())
	}
	st, err := readThread(ctx, o.GH, o.Thread, res.Body)
	if ctx.Err() != nil {
		return finish(ReplyInterrupted, "", "interrupted")
	}
	if err != nil {
		if errors.Is(err, errNotReviewThread) {
			return finish(ReplyRefused, "", err.Error())
		}
		return finish(ReplyGHError, "", err.Error())
	}
	res.Repo, res.PR = st.repo, st.pr
	if o.Repo != "" && !strings.EqualFold(o.Repo, st.repo) {
		return finish(ReplyRefused, "", fmt.Sprintf("thread belongs to %s, not --repo %s", st.repo, o.Repo))
	}

	resolve := func(rctx context.Context) ReplyResult {
		out, callErr := o.GH(rctx, "api", "graphql", "-F", "t="+o.Thread, "-f", "query="+resolveMutation)
		if resolveErr := parseResolve(out, callErr); resolveErr != nil {
			// The mutation can land even when gh loses the response. Read the
			// thread back before claiming that it remains unresolved.
			verified, verifyErr := readThread(rctx, o.GH, o.Thread, res.Body)
			if verifyErr == nil && verified.resolved {
				res.Resolved = true
				return finish(ReplyDone, "", "")
			}
			msg := resolveErr.Error()
			if verifyErr != nil {
				msg += "; could not verify the thread state: " + verifyErr.Error()
			}
			return finish(ReplyNotResolved, "run the same command again: it resolves without replying twice", msg)
		}
		res.Resolved = true
		return finish(ReplyDone, "", "")
	}
	resolveDetached := func() ReplyResult {
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		return resolve(rctx)
	}

	if st.hasOurReply {
		if st.resolved {
			return finish(ReplyDone, "", "") // a repeated run: nothing left to do
		}
		// The original run checked the commit before it posted. Do not let a
		// later force-push or transient commit lookup strand a replied thread.
		return resolveDetached()
	}
	if o.Fixed != "" {
		// Fail closed on a sha GitHub can't see: an unpushed or mistyped fix
		// would leave a reply that points at nothing.
		if _, err := o.GH(ctx, "api", fmt.Sprintf("repos/%s/commits/%s", st.repo, o.Fixed), "--jq", ".sha"); err != nil {
			if ctx.Err() != nil {
				return finish(ReplyInterrupted, "", "interrupted")
			}
			if missingCommit(err) {
				return finish(ReplyRefused, "push the fix commit first", fmt.Sprintf("commit %s is not in %s: %v", o.Fixed, st.repo, err))
			}
			return finish(ReplyGHError, "", fmt.Sprintf("checking commit %s in %s: %v", o.Fixed, st.repo, err))
		}
	}
	if !st.complete {
		// An earlier page could hold our reply; posting again would duplicate it.
		return finish(ReplyRefused, "reply and resolve this thread by hand", "the thread has more than 100 comments; cannot rule out an existing reply")
	}
	if !st.canReply {
		return finish(ReplyRefused, "", "GitHub says you cannot reply to this thread (locked, or no write access)")
	}
	if ctx.Err() != nil {
		return finish(ReplyInterrupted, "", "interrupted")
	}
	out, callErr := o.GH(ctx, "api", "graphql", "-F", "t="+o.Thread, "-f", "b="+res.Body, "-f", "query="+replyMutation)
	url, replyErr := parseReply(out, callErr)
	if replyErr != nil {
		// A timeout, interrupt, or lost response does not prove that the
		// mutation failed. Re-read with a live context so a landed reply can
		// still be resolved and an absent reply can be reported truthfully.
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 60*time.Second)
		defer cancel()
		verified, verifyErr := readThread(rctx, o.GH, o.Thread, res.Body)
		if verifyErr != nil {
			return finish(ReplyNotResolved, "run the same command again: it will not post a second reply", replyErr.Error()+"; could not verify whether the reply landed: "+verifyErr.Error())
		}
		if !verified.hasOurReply {
			if ctx.Err() != nil {
				return finish(ReplyInterrupted, "", "interrupted")
			}
			return finish(ReplyGHError, "run the same command again: it will not post a second reply", replyErr.Error())
		}
		res.Replied, res.ReplyURL = true, verified.ourReplyURL
		if verified.resolved {
			return finish(ReplyDone, "", "")
		}
		return resolve(rctx)
	}
	res.Replied, res.ReplyURL = true, url
	if st.resolved {
		return finish(ReplyDone, "", "")
	}
	// The reply is posted: from here an interrupt must not strand the thread
	// half-handled, so the resolve gets its own deadline.
	return resolveDetached()
}

func missingCommit(err error) bool {
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "http 404") || strings.Contains(s, "http 422") || strings.Contains(s, "no commit found for sha")
}

func readThread(ctx context.Context, gh GH, id, body string) (threadState, error) {
	var st threadState
	out, err := gh(ctx, "api", "graphql", "-F", "t="+id, "-f", "query="+threadQuery)
	if err != nil {
		return st, err
	}
	var doc struct {
		Data struct {
			Node *struct {
				Typename       string `json:"__typename"`
				IsResolved     bool   `json:"isResolved"`
				ViewerCanReply bool   `json:"viewerCanReply"`
				PullRequest    *struct {
					Number     int `json:"number"`
					Repository struct {
						NameWithOwner string `json:"nameWithOwner"`
					} `json:"repository"`
				} `json:"pullRequest"`
				Comments *struct {
					PageInfo *struct {
						HasPreviousPage bool `json:"hasPreviousPage"`
					} `json:"pageInfo"`
					Nodes []struct {
						ViewerDidAuthor bool   `json:"viewerDidAuthor"`
						Body            string `json:"body"`
						URL             string `json:"url"`
					} `json:"nodes"`
				} `json:"comments"`
			} `json:"node"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return st, fmt.Errorf("gh api graphql: %w", err)
	}
	if len(doc.Errors) > 0 {
		return st, fmt.Errorf("gh api graphql: %s", doc.Errors[0].Message)
	}
	n := doc.Data.Node
	if n == nil {
		return st, fmt.Errorf("no node with id %s", id)
	}
	if n.Typename != "PullRequestReviewThread" {
		return st, fmt.Errorf("%w: %s is a %s", errNotReviewThread, id, n.Typename)
	}
	if n.PullRequest == nil || n.Comments == nil || n.Comments.PageInfo == nil || n.PullRequest.Repository.NameWithOwner == "" {
		return st, fmt.Errorf("gh api graphql: incomplete review thread response")
	}
	st.repo, st.pr = n.PullRequest.Repository.NameWithOwner, n.PullRequest.Number
	st.resolved, st.canReply = n.IsResolved, n.ViewerCanReply
	st.complete = !n.Comments.PageInfo.HasPreviousPage
	for _, c := range n.Comments.Nodes {
		if c.ViewerDidAuthor && sameBody(c.Body, body) {
			st.hasOurReply = true
			st.ourReplyURL = c.URL
		}
	}
	return st, nil
}

func parseReply(out []byte, err error) (string, error) {
	if err != nil {
		return "", err
	}
	var doc struct {
		Data struct {
			Add *struct {
				Comment *struct {
					ID  string `json:"id"`
					URL string `json:"url"`
				} `json:"comment"`
			} `json:"addPullRequestReviewThreadReply"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return "", fmt.Errorf("gh api graphql: %w", err)
	}
	if len(doc.Errors) > 0 {
		return "", fmt.Errorf("gh api graphql: %s", doc.Errors[0].Message)
	}
	if doc.Data.Add == nil || doc.Data.Add.Comment == nil || doc.Data.Add.Comment.ID == "" {
		return "", fmt.Errorf("gh api graphql: the reply mutation returned no comment")
	}
	return doc.Data.Add.Comment.URL, nil
}

func parseResolve(out []byte, err error) error {
	if err != nil {
		return err
	}
	var doc struct {
		Data struct {
			Resolve *struct {
				Thread *struct {
					IsResolved bool `json:"isResolved"`
				} `json:"thread"`
			} `json:"resolveReviewThread"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return fmt.Errorf("gh api graphql: %w", err)
	}
	if len(doc.Errors) > 0 {
		return fmt.Errorf("gh api graphql: %s", doc.Errors[0].Message)
	}
	if doc.Data.Resolve == nil || doc.Data.Resolve.Thread == nil || !doc.Data.Resolve.Thread.IsResolved {
		return fmt.Errorf("gh api graphql: the thread is still unresolved")
	}
	return nil
}

// sameBody reports whether a stored comment is our reply. GitHub may store a
// body with CRLF line ends or trimmed edges; a byte compare would then miss
// our own reply and a rerun would post it a second time.
func sameBody(stored, ours string) bool {
	norm := func(s string) string { return strings.TrimSpace(strings.ReplaceAll(s, "\r\n", "\n")) }
	return norm(stored) == norm(ours)
}
