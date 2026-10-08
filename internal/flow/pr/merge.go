package pr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
)

// MergeStatus is the outcome of one merge attempt. Every attempt ends in
// exactly one.
type MergeStatus string

const (
	MergeMerged      MergeStatus = "merged"      // merged (and synced, if asked)
	MergeRefused     MergeStatus = "refused"     // not ready: reasons listed, nothing changed
	MergeSyncFailed  MergeStatus = "sync_failed" // merged, but the sync branch was not fast-forwarded
	MergeUnconfirmed MergeStatus = "unconfirmed" // the merge call ran but its outcome was not confirmed
	MergeGHError     MergeStatus = "gh_error"    // gh failed before the merge call; nothing was merged
	MergeInterrupted MergeStatus = "interrupted" // cancelled before the merge started
)

// MergeOptions configures one merge.
type MergeOptions struct {
	Repo       string // owner/name
	PR         int
	Method     string // merge, squash or rebase
	Head       string // optional: the head sha the caller expects
	Admin      bool   // pass --admin to gh pr merge
	SyncBranch string // optional: branch to fast-forward to the base after the merge

	// MergeableWait bounds how long an UNKNOWN mergeable state is re-read
	// (GitHub computes it lazily) before the merge is refused.
	MergeableWait     time.Duration
	MergeableInterval time.Duration
	// ConfirmWait bounds how long the merged state is re-read after gh
	// accepts the merge.
	ConfirmWait     time.Duration
	ConfirmInterval time.Duration
	GH              GH
}

// Sync reports the fast-forward of the sync branch.
type Sync struct {
	Branch string `json:"branch"`
	SHA    string `json:"sha,omitempty"`
	Status string `json:"status"` // synced, refused (not a fast-forward), failed
	Error  string `json:"error,omitempty"`
}

// MergeResult is the machine-readable summary.
type MergeResult struct {
	Status    MergeStatus `json:"status"`
	Repo      string      `json:"repo"`
	PR        int         `json:"pr"`
	Head      string      `json:"head,omitempty"`
	Base      string      `json:"base,omitempty"`
	Method    string      `json:"method"`
	MergeSHA  string      `json:"merge_sha,omitempty"`
	AutoMerge bool        `json:"auto_merge,omitempty"` // GitHub holds an auto-merge request for the PR
	Sync      *Sync       `json:"sync,omitempty"`
	Reasons   []string    `json:"reasons"`
	Next      string      `json:"next,omitempty"`
	CheckedAt string      `json:"checked_at"`
	Error     string      `json:"error,omitempty"`
}

// branchRe is the character grammar for --sync-branch: it goes into a REST
// path, so every segment starts with a letter, digit or "_" (no leading "-"
// or "."), with no empty segment and no spaces. ValidBranch adds the rest.
var branchRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)*$`)

// ValidBranch reports whether name is a branch name --sync-branch accepts:
// branchRe's grammar and no "..", which git refuses in a ref name.
func ValidBranch(name string) bool {
	return branchRe.MatchString(name) && !strings.Contains(name, "..")
}

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// prState is one read of the fields readiness and confirmation depend on.
type prState struct {
	State               string `json:"state"`
	IsDraft             *bool  `json:"isDraft"`
	HeadRefOid          string `json:"headRefOid"`
	BaseRefName         string `json:"baseRefName"`
	Mergeable           string `json:"mergeable"`
	IsMergeQueueEnabled *bool  `json:"isMergeQueueEnabled"`
	AutoMergeRequest    *struct {
		EnabledAt string `json:"enabledAt"`
	} `json:"autoMergeRequest"`
	MergeCommit *struct {
		Oid string `json:"oid"`
	} `json:"mergeCommit"`
}

// Merge checks that the PR is ready, merges it pinned to the head it just
// read, reports the merge commit, and optionally fast-forwards a sync branch.
// Readiness is read immediately before the merge, never reused from an
// earlier check: a review can land on the last push while a check is read.
func Merge(ctx context.Context, o MergeOptions) MergeResult {
	res := MergeResult{Repo: o.Repo, PR: o.PR, Method: o.Method, Reasons: []string{}}
	finish := func(s MergeStatus, next, msg string) MergeResult {
		res.Status, res.Next, res.Error = s, next, msg
		res.CheckedAt = time.Now().UTC().Format(time.RFC3339)
		return res
	}
	if !shaRe.MatchString(strings.ToLower(o.Head)) && o.Head != "" {
		return finish(MergeGHError, "", "--head must be a full 40-character commit sha")
	}

	// Read the potentially paginated thread list first, then read the PR
	// fields immediately before the merge. In particular, do not merge from a
	// PR snapshot that predates the slower pagination request.
	open, complete, err := countOpenThreads(ctx, o)
	if ctx.Err() != nil {
		return finish(MergeInterrupted, "", "interrupted")
	}
	if err != nil {
		return finish(MergeGHError, "", err.Error())
	}
	if !complete {
		res.Reasons = append(res.Reasons, "review threads could not all be read (GitHub reported an incomplete page set)")
	}
	if open > 0 {
		res.Reasons = append(res.Reasons, fmt.Sprintf("%d unresolved review thread(s)", open))
	}
	if len(res.Reasons) > 0 {
		return finish(MergeRefused, "fix the reasons listed, then run again", "")
	}

	st, err := readMergeable(ctx, o)
	if ctx.Err() != nil {
		return finish(MergeInterrupted, "", "interrupted")
	}
	if err != nil {
		return finish(MergeGHError, "", err.Error())
	}
	res.Head, res.Base = strings.ToLower(st.HeadRefOid), st.BaseRefName

	if st.State != "OPEN" {
		res.Reasons = append(res.Reasons, "PR is "+strings.ToLower(st.State))
	}
	if st.BaseRefName == "" {
		res.Reasons = append(res.Reasons, "PR base branch is unknown")
	}
	// On a merge-queue base, gh pr merge queues the PR or enables auto-merge
	// instead of merging, even without --auto: neither is a merge to report.
	if st.IsMergeQueueEnabled == nil {
		res.Reasons = append(res.Reasons, "whether the base branch uses a merge queue is unknown")
	} else if *st.IsMergeQueueEnabled {
		res.Reasons = append(res.Reasons, "the base branch uses a merge queue (gh would queue the PR or enable auto-merge, not merge it)")
	}
	if st.IsDraft == nil {
		res.Reasons = append(res.Reasons, "PR draft state is unknown")
	} else if *st.IsDraft {
		res.Reasons = append(res.Reasons, "PR is a draft")
	}
	if !shaRe.MatchString(res.Head) {
		res.Reasons = append(res.Reasons, "PR head is not a full commit sha")
	}
	if o.Head != "" && !strings.EqualFold(o.Head, res.Head) {
		res.Reasons = append(res.Reasons, fmt.Sprintf("PR head is %s, not the expected %s", res.Head, strings.ToLower(o.Head)))
	}
	switch st.Mergeable {
	case "MERGEABLE":
	case "CONFLICTING":
		res.Reasons = append(res.Reasons, "PR has merge conflicts with "+st.BaseRefName)
	default:
		res.Reasons = append(res.Reasons, fmt.Sprintf("GitHub has not computed mergeability (mergeable=%s)", st.Mergeable))
	}
	if o.SyncBranch != "" && o.SyncBranch == st.BaseRefName {
		res.Reasons = append(res.Reasons, "--sync-branch is the PR's base branch")
	}
	if len(res.Reasons) > 0 {
		return finish(MergeRefused, "fix the reasons listed, then run again", "")
	}
	if ctx.Err() != nil {
		return finish(MergeInterrupted, "", "interrupted")
	}

	// Point of no return: from here the merge may land, so the rest runs
	// detached from cancellation and still reports what happened.
	dctx := context.WithoutCancel(ctx)
	args := []string{"pr", "merge", fmt.Sprint(o.PR), "-R", o.Repo, "--" + o.Method, "--match-head-commit", res.Head}
	if o.Admin {
		args = append(args, "--admin")
	}
	_, mergeErr := o.GH(dctx, args...)

	// Once gh pr merge has run, a failure no longer proves nothing merged (a
	// dropped response, a late merge), so every outcome is read back for the
	// full window and only a positive confirmation counts as merged.
	c := confirmMerged(dctx, o, res.Head)
	res.AutoMerge = c.autoMerge
	if c.sha != "" {
		res.MergeSHA = c.sha
		if c.base != "" {
			res.Base = c.base
		}
	} else {
		var msgs []string
		if mergeErr != nil {
			msgs = append(msgs, mergeErr.Error())
		}
		if c.err != nil {
			msgs = append(msgs, "confirmation failed: "+c.err.Error())
		} else {
			msgs = append(msgs, "PR did not read back as merged at "+res.Head)
		}
		next := "check the PR on GitHub before verifying a deploy"
		if c.autoMerge {
			next = "GitHub holds an auto-merge request for the PR: disable it or let it land, then check the PR before verifying a deploy"
		}
		return finish(MergeUnconfirmed, next, strings.Join(msgs, "; "))
	}
	mergedBase := c.base

	if o.SyncBranch == "" {
		return finish(MergeMerged, "verify the deploy with the merge_sha", "")
	}
	if mergedBase == "" {
		res.Sync = &Sync{Branch: o.SyncBranch, Status: "failed", Error: "merged PR response had no base branch"}
		return finish(MergeSyncFailed, "the merge landed; sync "+o.SyncBranch+" by hand (never force), then verify the deploy", "")
	}
	if o.SyncBranch == mergedBase {
		res.Sync = &Sync{Branch: o.SyncBranch, Status: "refused", Error: "--sync-branch is the merged PR's base branch"}
		return finish(MergeSyncFailed, "the merge landed; no branch sync was attempted", "")
	}
	res.Sync = syncBranch(dctx, o, res.Base)
	if res.Sync.Status != "synced" {
		return finish(MergeSyncFailed, "the merge landed; sync "+o.SyncBranch+" by hand (never force), then verify the deploy", "")
	}
	return finish(MergeMerged, "verify the deploy with the merge_sha", "")
}

// readMergeable reads the PR, re-reading while GitHub reports mergeable as
// UNKNOWN, up to MergeableWait. A persistent UNKNOWN is returned as read and
// refused by the caller.
func readMergeable(ctx context.Context, o MergeOptions) (prState, error) {
	deadline := time.Now().Add(o.MergeableWait)
	for {
		st, err := readPR(ctx, o)
		if err != nil || st.Mergeable != "UNKNOWN" || !time.Now().Before(deadline) {
			return st, err
		}
		delay := min(o.MergeableInterval, time.Until(deadline))
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(delay):
		}
	}
}

// confirmation is what the read-back after the merge call showed.
type confirmation struct {
	sha, base string // set only when the PR read back MERGED at the pinned head
	autoMerge bool   // the last read showed an auto-merge request
	err       error  // the last read's error, if it failed
}

// confirmMerged re-reads the PR for up to ConfirmWait until it is MERGED
// with a merge commit AND its head is the pinned one. A PR merged at any
// other head is not this merge.
func confirmMerged(ctx context.Context, o MergeOptions, pinned string) confirmation {
	deadline := time.Now().Add(o.ConfirmWait)
	var c confirmation
	for {
		st, err := readPR(ctx, o)
		c.err = err
		if err == nil {
			c.autoMerge = st.AutoMergeRequest != nil
			switch st.State {
			case "MERGED":
				sha := ""
				if st.MergeCommit != nil {
					sha = strings.ToLower(st.MergeCommit.Oid)
				}
				switch {
				case !strings.EqualFold(st.HeadRefOid, pinned):
					c.err = fmt.Errorf("PR read back as merged at head %s, not the pinned %s", strings.ToLower(st.HeadRefOid), pinned)
					return c // a merged PR's head no longer changes
				case shaRe.MatchString(sha):
					c.sha, c.base = sha, st.BaseRefName
					return c
				default:
					c.err = fmt.Errorf("PR read back as merged without a valid merge commit sha")
				}
			case "OPEN", "CLOSED":
			default:
				c.err = fmt.Errorf("PR read back with unknown state %q", st.State)
			}
		}
		if !time.Now().Before(deadline) {
			return c
		}
		time.Sleep(min(o.ConfirmInterval, time.Until(deadline)))
	}
}

const prQuery = `query($o:String!,$n:String!,$p:Int!){repository(owner:$o,name:$n){pullRequest(number:$p){` +
	`state isDraft headRefOid baseRefName mergeable isMergeQueueEnabled autoMergeRequest{enabledAt} mergeCommit{oid}}}}`

// readPR reads the PR through GraphQL: gh pr view --json does not offer
// isMergeQueueEnabled.
func readPR(ctx context.Context, o MergeOptions) (prState, error) {
	var st prState
	owner, name, _ := strings.Cut(o.Repo, "/")
	out, err := o.GH(ctx, "api", "graphql", "-f", "o="+owner, "-f", "n="+name, "-F", fmt.Sprintf("p=%d", o.PR), "-f", "query="+prQuery)
	if err != nil {
		return st, err
	}
	var doc struct {
		Data struct {
			Repository *struct {
				PullRequest *prState `json:"pullRequest"`
			} `json:"repository"`
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
	if doc.Data.Repository == nil || doc.Data.Repository.PullRequest == nil {
		return st, fmt.Errorf("gh api graphql: response has no pullRequest")
	}
	return *doc.Data.Repository.PullRequest, nil
}

const allThreadsQuery = `query($o:String!,$n:String!,$p:Int!,$endCursor:String){repository(owner:$o,name:$n){pullRequest(number:$p){` +
	`reviewThreads(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{isResolved}}}}}`

// countOpenThreads counts unresolved review threads from ANY author (unlike
// parseThreads, which keeps CodeRabbit's only) and reports whether the last
// page said there are no more.
func countOpenThreads(ctx context.Context, o MergeOptions) (int, bool, error) {
	owner, name, _ := strings.Cut(o.Repo, "/")
	out, err := o.GH(ctx, "api", "graphql", "--paginate", "--slurp", "-f", "o="+owner, "-f", "n="+name, "-F", fmt.Sprintf("p=%d", o.PR), "-f", "query="+allThreadsQuery)
	if err != nil {
		return 0, false, err
	}
	type page struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads *struct {
						PageInfo *struct {
							HasNextPage *bool   `json:"hasNextPage"`
							EndCursor   *string `json:"endCursor"`
						} `json:"pageInfo"`
						Nodes []*struct {
							IsResolved *bool `json:"isResolved"`
						} `json:"nodes"`
					} `json:"reviewThreads"`
				} `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	var pages []page
	if trimmed := bytes.TrimSpace(out); len(trimmed) > 0 && trimmed[0] == '[' {
		err = json.Unmarshal(trimmed, &pages)
	} else {
		var one page
		err = json.Unmarshal(out, &one)
		pages = []page{one}
	}
	if err != nil {
		return 0, false, fmt.Errorf("gh api graphql: %w", err)
	}
	if len(pages) == 0 {
		return 0, false, nil
	}
	open, complete := 0, false
	for i, p := range pages {
		if len(p.Errors) > 0 {
			return 0, false, fmt.Errorf("gh api graphql: %s", p.Errors[0].Message)
		}
		if p.Data.Repository == nil || p.Data.Repository.PullRequest == nil {
			return 0, false, fmt.Errorf("gh api graphql: response has no reviewThreads")
		}
		rt := p.Data.Repository.PullRequest.ReviewThreads
		if rt == nil || rt.PageInfo == nil || rt.PageInfo.HasNextPage == nil || rt.Nodes == nil {
			return open, false, nil
		}
		for _, n := range rt.Nodes {
			// A thread whose state is missing counts as open: unknown is not resolved.
			if n == nil || n.IsResolved == nil || !*n.IsResolved {
				open++
			}
		}
		hasNext := *rt.PageInfo.HasNextPage
		if hasNext && (rt.PageInfo.EndCursor == nil || *rt.PageInfo.EndCursor == "") {
			return open, false, nil
		}
		if (!hasNext && i != len(pages)-1) || (hasNext && i == len(pages)-1) {
			return open, false, nil
		}
		complete = !hasNext
	}
	return open, complete, nil
}

// syncBranch fast-forwards o.SyncBranch to the base branch's current head on
// GitHub. force=false makes GitHub refuse anything but a fast-forward (422).
func syncBranch(ctx context.Context, o MergeOptions, base string) *Sync {
	s := &Sync{Branch: o.SyncBranch}
	out, err := o.GH(ctx, "api", fmt.Sprintf("repos/%s/git/ref/heads/%s", o.Repo, escapeRef(base)))
	if err != nil {
		s.Status, s.Error = "failed", err.Error()
		return s
	}
	var ref struct {
		Object struct {
			SHA string `json:"sha"`
		} `json:"object"`
	}
	if err := json.Unmarshal(out, &ref); err != nil || !shaRe.MatchString(strings.ToLower(ref.Object.SHA)) {
		s.Status, s.Error = "failed", "could not read the head of "+base
		return s
	}
	s.SHA = strings.ToLower(ref.Object.SHA)
	_, err = o.GH(ctx, "api", "-X", "PATCH", fmt.Sprintf("repos/%s/git/refs/heads/%s", o.Repo, escapeRef(o.SyncBranch)), "-f", "sha="+s.SHA, "-F", "force=false")
	switch {
	case err == nil:
		s.Status = "synced"
	case strings.Contains(strings.ToLower(err.Error()), "not a fast forward"):
		s.Status, s.Error = "refused", "not a fast-forward of "+base+": "+err.Error()
	default:
		s.Status, s.Error = "failed", err.Error()
	}
	return s
}

// escapeRef preserves the slash-separated ref hierarchy while preventing a
// valid Git ref containing URL metacharacters from changing the REST URL.
func escapeRef(ref string) string {
	parts := strings.Split(ref, "/")
	for i := range parts {
		parts[i] = url.PathEscape(parts[i])
	}
	return strings.Join(parts, "/")
}
