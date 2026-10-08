package pr

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
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
	MergeUnconfirmed MergeStatus = "unconfirmed" // gh accepted the merge but the PR never read back as merged
	MergeGHError     MergeStatus = "gh_error"    // gh failed; nothing was merged
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
	Sync      *Sync       `json:"sync,omitempty"`
	Reasons   []string    `json:"reasons"`
	Next      string      `json:"next,omitempty"`
	CheckedAt string      `json:"checked_at"`
	Error     string      `json:"error,omitempty"`
}

// BranchRe is the branch-name grammar accepted for --sync-branch: it goes
// into a REST path, so every segment starts with a letter, digit or "_"
// (no leading "-" or "."), and no "..", no empty segment, no spaces.
var BranchRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]*(/[A-Za-z0-9_][A-Za-z0-9._-]*)*$`)

var shaRe = regexp.MustCompile(`^[0-9a-f]{40}$`)

// prState is one read of the fields readiness depends on.
type prState struct {
	State       string `json:"state"`
	IsDraft     bool   `json:"isDraft"`
	HeadRefOid  string `json:"headRefOid"`
	BaseRefName string `json:"baseRefName"`
	Mergeable   string `json:"mergeable"`
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
	if st.IsDraft {
		res.Reasons = append(res.Reasons, "PR is a draft")
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
	open, complete, err := countOpenThreads(ctx, o)
	if ctx.Err() != nil {
		return finish(MergeInterrupted, "", "interrupted")
	}
	switch {
	case err != nil:
		return finish(MergeGHError, "", err.Error())
	case !complete:
		res.Reasons = append(res.Reasons, "review threads could not all be read (GitHub reported more pages)")
	case open > 0:
		res.Reasons = append(res.Reasons, fmt.Sprintf("%d unresolved review thread(s)", open))
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

	// gh can fail after GitHub merged (a dropped response), so a failed call
	// still gets one read back; a successful one is re-read until confirmed.
	wait := o.ConfirmWait
	if mergeErr != nil {
		wait = 0
	}
	merged, err := confirmMerged(dctx, o, wait)
	switch {
	case merged != "":
		res.MergeSHA = merged
	case mergeErr != nil:
		return finish(MergeGHError, "", mergeErr.Error())
	case err != nil:
		return finish(MergeUnconfirmed, "check the PR on GitHub before verifying a deploy", err.Error())
	default:
		return finish(MergeUnconfirmed, "check the PR on GitHub (a merge queue may hold it) before verifying a deploy", "PR did not read back as merged")
	}

	if o.SyncBranch == "" {
		return finish(MergeMerged, "verify the deploy with the merge_sha", "")
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
		st, err := readPR(ctx, o, "state,isDraft,headRefOid,baseRefName,mergeable")
		if err != nil || st.Mergeable != "UNKNOWN" || !time.Now().Before(deadline) {
			return st, err
		}
		select {
		case <-ctx.Done():
			return st, ctx.Err()
		case <-time.After(o.MergeableInterval):
		}
	}
}

// confirmMerged re-reads the PR until it is MERGED with a merge commit, up
// to wait, and returns that commit's sha ("" if it never was).
func confirmMerged(ctx context.Context, o MergeOptions, wait time.Duration) (string, error) {
	deadline := time.Now().Add(wait)
	var lastErr error
	for {
		st, err := readPR(ctx, o, "state,mergeCommit")
		lastErr = err
		if err == nil && st.State == "MERGED" && st.MergeCommit != nil {
			if sha := strings.ToLower(st.MergeCommit.Oid); shaRe.MatchString(sha) {
				return sha, nil
			}
		}
		if !time.Now().Before(deadline) {
			return "", lastErr
		}
		time.Sleep(o.ConfirmInterval)
	}
}

func readPR(ctx context.Context, o MergeOptions, fields string) (prState, error) {
	var st prState
	out, err := o.GH(ctx, "pr", "view", fmt.Sprint(o.PR), "-R", o.Repo, "--json", fields)
	if err != nil {
		return st, err
	}
	if err := json.Unmarshal(out, &st); err != nil {
		return st, fmt.Errorf("gh pr view: %w", err)
	}
	return st, nil
}

const allThreadsQuery = `query($o:String!,$n:String!,$p:Int!,$endCursor:String){repository(owner:$o,name:$n){pullRequest(number:$p){` +
	`reviewThreads(first:100,after:$endCursor){pageInfo{hasNextPage endCursor} nodes{isResolved}}}}}`

// countOpenThreads counts unresolved review threads from ANY author (unlike
// parseThreads, which keeps CodeRabbit's only) and reports whether the last
// page said there are no more.
func countOpenThreads(ctx context.Context, o MergeOptions) (int, bool, error) {
	owner, name, _ := strings.Cut(o.Repo, "/")
	out, err := o.GH(ctx, "api", "graphql", "--paginate", "--slurp", "-F", "o="+owner, "-F", "n="+name, "-F", fmt.Sprintf("p=%d", o.PR), "-f", "query="+allThreadsQuery)
	if err != nil {
		return 0, false, err
	}
	type page struct {
		Data struct {
			Repository *struct {
				PullRequest *struct {
					ReviewThreads *struct {
						PageInfo *struct {
							HasNextPage bool `json:"hasNextPage"`
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
		return 0, false, fmt.Errorf("gh api graphql: no pages")
	}
	open, complete := 0, false
	for _, p := range pages {
		if len(p.Errors) > 0 {
			return 0, false, fmt.Errorf("gh api graphql: %s", p.Errors[0].Message)
		}
		if p.Data.Repository == nil || p.Data.Repository.PullRequest == nil || p.Data.Repository.PullRequest.ReviewThreads == nil {
			return 0, false, fmt.Errorf("gh api graphql: response has no reviewThreads")
		}
		rt := p.Data.Repository.PullRequest.ReviewThreads
		if rt.PageInfo == nil || rt.Nodes == nil {
			return 0, false, fmt.Errorf("gh api graphql: incomplete reviewThreads response")
		}
		for _, n := range rt.Nodes {
			// A thread whose state is missing counts as open: unknown is not resolved.
			if n == nil || n.IsResolved == nil || !*n.IsResolved {
				open++
			}
		}
		complete = !rt.PageInfo.HasNextPage
	}
	return open, complete, nil
}

// syncBranch fast-forwards o.SyncBranch to the base branch's current head on
// GitHub. force=false makes GitHub refuse anything but a fast-forward (422).
func syncBranch(ctx context.Context, o MergeOptions, base string) *Sync {
	s := &Sync{Branch: o.SyncBranch}
	out, err := o.GH(ctx, "api", fmt.Sprintf("repos/%s/git/ref/heads/%s", o.Repo, base))
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
	_, err = o.GH(ctx, "api", "-X", "PATCH", fmt.Sprintf("repos/%s/git/refs/heads/%s", o.Repo, o.SyncBranch), "-f", "sha="+s.SHA, "-F", "force=false")
	switch {
	case err == nil:
		s.Status = "synced"
	case strings.Contains(err.Error(), "422") || strings.Contains(strings.ToLower(err.Error()), "not a fast forward"):
		s.Status, s.Error = "refused", "not a fast-forward of "+base+": "+err.Error()
	default:
		s.Status, s.Error = "failed", err.Error()
	}
	return s
}
