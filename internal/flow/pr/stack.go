package pr

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// StackAPIVersion is the REST API version that documents the stack and
// asynchronous merge endpoints. GitHub refuses to merge a stacked PR through
// the synchronous merge endpoint that gh pr merge calls.
const StackAPIVersion = "2026-03-10"

// StackLayer is one PR of a stack merge, bottom first.
type StackLayer struct {
	PR         int    `json:"pr"`
	Position   int    `json:"position"`
	Head       string `json:"head,omitempty"`
	HeadBranch string `json:"head_branch,omitempty"`
	MergeSHA   string `json:"merge_sha,omitempty"`
}

// StackMerge reports a merge of a stacked PR: every unmerged layer from the
// bottom of the stack up to and including the requested PR lands together.
type StackMerge struct {
	Number   int          `json:"number"`
	Base     string       `json:"base"`     // the stack's trunk, where every layer lands
	Position int          `json:"position"` // the requested PR's position (1 = bottom)
	Size     int          `json:"size"`
	Layers   []StackLayer `json:"layers"` // the layers this merge covers, bottom first
	// AsyncStatus is the asynchronous merge's last reported status:
	// pending, merged, enqueued or failed ("" if it was never read).
	AsyncStatus string `json:"async_status,omitempty"`
	AsyncError  string `json:"async_error,omitempty"`
}

// stackInfo is the stack a PR belongs to, as GitHub reports it.
type stackInfo struct {
	Number   int
	Base     string
	Position int // the PR's position
	Size     int
	Entries  []stackEntry // sorted by position, complete (1..Size)
}

type stackEntry struct {
	Position int
	PR       int
	State    string // OPEN, CLOSED or MERGED
}

const stackQuery = `query($o:String!,$n:String!,$p:Int!){repository(owner:$o,name:$n){pullRequest(number:$p){` +
	`stackEntry{position} stack{number baseRefName size entries(first:100){pageInfo{hasNextPage} nodes{position pullRequest{number state}}}}}}}`

// errNoStackSchema marks a GitHub whose GraphQL schema has no stack fields
// (stacks are not available there), as opposed to a failed read.
var errNoStackSchema = fmt.Errorf("GitHub's GraphQL schema has no pull request stacks")

// readStack reads the stack the PR belongs to, or nil if it is in none. A
// partial or inconsistent answer is an error, never "not stacked": merging
// a stacked PR as a single PR would skip the checks on the layers below it.
func readStack(ctx context.Context, gh GH, repo string, pr int) (*stackInfo, error) {
	owner, name, _ := strings.Cut(repo, "/")
	out, err := gh(ctx, "api", "graphql", "-f", "o="+owner, "-f", "n="+name, "-F", fmt.Sprintf("p=%d", pr), "-f", "query="+stackQuery)
	if err != nil {
		if strings.Contains(err.Error(), "doesn't exist on type") {
			return nil, errNoStackSchema
		}
		return nil, err
	}
	var doc struct {
		Data struct {
			Repository *struct {
				PullRequest *map[string]json.RawMessage `json:"pullRequest"`
			} `json:"repository"`
		} `json:"data"`
		Errors []struct {
			Message string `json:"message"`
		} `json:"errors"`
	}
	if err := json.Unmarshal(out, &doc); err != nil {
		return nil, fmt.Errorf("gh api graphql: %w", err)
	}
	if len(doc.Errors) > 0 {
		if strings.Contains(doc.Errors[0].Message, "doesn't exist on type") {
			return nil, errNoStackSchema
		}
		return nil, fmt.Errorf("gh api graphql: %s", doc.Errors[0].Message)
	}
	if doc.Data.Repository == nil || doc.Data.Repository.PullRequest == nil {
		return nil, fmt.Errorf("gh api graphql: response has no pullRequest")
	}
	fields := *doc.Data.Repository.PullRequest
	rawEntry, okEntry := fields["stackEntry"]
	rawStack, okStack := fields["stack"]
	if !okEntry || !okStack {
		return nil, fmt.Errorf("gh api graphql: response has no stack fields")
	}
	isNull := func(r json.RawMessage) bool { return strings.TrimSpace(string(r)) == "null" }
	if isNull(rawEntry) && isNull(rawStack) {
		return nil, nil
	}
	if isNull(rawEntry) || isNull(rawStack) {
		return nil, fmt.Errorf("gh api graphql: the PR's stack and stack entry disagree")
	}
	var entry struct {
		Position *int `json:"position"`
	}
	var stack struct {
		Number      *int    `json:"number"`
		BaseRefName *string `json:"baseRefName"`
		Size        *int    `json:"size"`
		Entries     *struct {
			PageInfo *struct {
				HasNextPage *bool `json:"hasNextPage"`
			} `json:"pageInfo"`
			Nodes []*struct {
				Position    *int `json:"position"`
				PullRequest *struct {
					Number *int    `json:"number"`
					State  *string `json:"state"`
				} `json:"pullRequest"`
			} `json:"nodes"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rawEntry, &entry); err != nil {
		return nil, fmt.Errorf("gh api graphql: stackEntry: %w", err)
	}
	if err := json.Unmarshal(rawStack, &stack); err != nil {
		return nil, fmt.Errorf("gh api graphql: stack: %w", err)
	}
	if entry.Position == nil || stack.Number == nil || stack.BaseRefName == nil || *stack.BaseRefName == "" ||
		stack.Size == nil || stack.Entries == nil || stack.Entries.PageInfo == nil || stack.Entries.PageInfo.HasNextPage == nil {
		return nil, fmt.Errorf("gh api graphql: the stack is missing fields")
	}
	if *stack.Entries.PageInfo.HasNextPage {
		return nil, fmt.Errorf("the stack has more than 100 pull requests")
	}
	si := &stackInfo{Number: *stack.Number, Base: *stack.BaseRefName, Position: *entry.Position, Size: *stack.Size}
	seen := map[int]bool{}
	for _, n := range stack.Entries.Nodes {
		if n == nil || n.Position == nil || n.PullRequest == nil || n.PullRequest.Number == nil || n.PullRequest.State == nil {
			return nil, fmt.Errorf("gh api graphql: a stack entry is missing fields (or its PR is not readable)")
		}
		if seen[*n.Position] {
			return nil, fmt.Errorf("gh api graphql: two stack entries at position %d", *n.Position)
		}
		seen[*n.Position] = true
		si.Entries = append(si.Entries, stackEntry{Position: *n.Position, PR: *n.PullRequest.Number, State: *n.PullRequest.State})
	}
	sort.Slice(si.Entries, func(i, j int) bool { return si.Entries[i].Position < si.Entries[j].Position })
	if len(si.Entries) != si.Size {
		return nil, fmt.Errorf("gh api graphql: the stack reports size %d but lists %d entries", si.Size, len(si.Entries))
	}
	for i, e := range si.Entries {
		if e.Position != i+1 {
			return nil, fmt.Errorf("gh api graphql: stack positions are not 1..%d", si.Size)
		}
	}
	if si.Position < 1 || si.Position > si.Size || si.Entries[si.Position-1].PR != pr {
		return nil, fmt.Errorf("gh api graphql: PR #%d is not at its reported stack position %d", pr, si.Position)
	}
	return si, nil
}

// forPR returns o with the PR number replaced, for reads of another layer.
func (o MergeOptions) forPR(n int) MergeOptions {
	o.PR = n
	return o
}

// mergeStack merges a stacked PR: the layers from the lowest unmerged one up
// to the requested one land together, atomically, through GitHub's
// asynchronous merge endpoint. Every layer passes the same readiness checks
// as a single PR, its head is pinned to the one read, and every layer must
// read back merged at that head.
func mergeStack(ctx context.Context, o MergeOptions, si *stackInfo, res *MergeResult, finish func(MergeStatus, string, string) MergeResult) MergeResult {
	sm := &StackMerge{Number: si.Number, Base: si.Base, Position: si.Position, Size: si.Size, Layers: []StackLayer{}}
	res.Stack = sm
	res.Base = si.Base

	// The layers this merge covers: every one up to the requested PR that
	// has not merged yet. A merged layer can only sit below unmerged ones.
	var layers []stackEntry
	for _, e := range si.Entries[:si.Position] {
		switch e.State {
		case "MERGED":
			if len(layers) > 0 {
				res.Reasons = append(res.Reasons, fmt.Sprintf("stack layer #%d is merged above an unmerged layer", e.PR))
			}
		case "OPEN":
			layers = append(layers, e)
		default:
			res.Reasons = append(res.Reasons, fmt.Sprintf("stack layer #%d is %s", e.PR, strings.ToLower(e.State)))
		}
	}
	if len(layers) == 0 || layers[len(layers)-1].PR != o.PR {
		res.Reasons = append(res.Reasons, fmt.Sprintf("PR #%d is not open", o.PR))
	}
	if len(res.Reasons) > 0 {
		return finish(MergeRefused, "fix the reasons listed, then run again", "")
	}
	for _, l := range layers {
		sm.Layers = append(sm.Layers, StackLayer{PR: l.PR, Position: l.Position})
	}

	// Review threads of every layer first (the slow, paginated reads), then
	// each layer's PR fields, so the merge never runs on a stale snapshot.
	for _, l := range layers {
		open, complete, err := countOpenThreads(ctx, o.forPR(l.PR))
		if ctx.Err() != nil {
			return finish(MergeInterrupted, "", "interrupted")
		}
		if err != nil {
			return finish(MergeGHError, "", fmt.Sprintf("#%d: %v", l.PR, err))
		}
		if !complete {
			res.Reasons = append(res.Reasons, fmt.Sprintf("#%d: review threads could not all be read (GitHub reported an incomplete page set)", l.PR))
		}
		if open > 0 {
			res.Reasons = append(res.Reasons, fmt.Sprintf("#%d: %d unresolved review thread(s)", l.PR, open))
		}
	}
	if len(res.Reasons) > 0 {
		return finish(MergeRefused, "fix the reasons listed, then run again", "")
	}

	for i, l := range layers {
		st, err := readMergeable(ctx, o.forPR(l.PR))
		if ctx.Err() != nil {
			return finish(MergeInterrupted, "", "interrupted")
		}
		if err != nil {
			return finish(MergeGHError, "", fmt.Sprintf("#%d: %v", l.PR, err))
		}
		h := strings.ToLower(st.HeadRefOid)
		sm.Layers[i].Head, sm.Layers[i].HeadBranch = h, st.HeadRefName
		for _, r := range layerReasons(st, h) {
			res.Reasons = append(res.Reasons, fmt.Sprintf("#%d: %s", l.PR, r))
		}
		// Each layer targets the branch of the one below it; the bottom
		// targets the trunk.
		want := si.Base
		if i > 0 {
			want = sm.Layers[i-1].HeadBranch
		}
		if st.BaseRefName != want {
			res.Reasons = append(res.Reasons, fmt.Sprintf("#%d: base is %q, not %q", l.PR, st.BaseRefName, want))
		}
	}
	top := sm.Layers[len(sm.Layers)-1]
	res.Head, res.HeadBranch = top.Head, top.HeadBranch
	if o.Head != "" && !strings.EqualFold(o.Head, top.Head) {
		res.Reasons = append(res.Reasons, fmt.Sprintf("PR head is %s, not the expected %s", top.Head, strings.ToLower(o.Head)))
	}
	if o.SyncBranch != "" && o.SyncBranch == si.Base {
		res.Reasons = append(res.Reasons, "--sync-branch is the stack's base branch")
	}
	if len(res.Reasons) > 0 {
		return finish(MergeRefused, "fix the reasons listed, then run again", "")
	}

	// The history must be linear: each layer contains the trunk or the layer
	// below it. Pinning the top head then pins every layer under it: a push
	// to a lower layer breaks linearity and GitHub refuses the merge.
	prev := si.Base
	for _, l := range sm.Layers {
		ok, err := contains(ctx, o, prev, l.Head)
		if ctx.Err() != nil {
			return finish(MergeInterrupted, "", "interrupted")
		}
		if err != nil {
			return finish(MergeGHError, "", fmt.Sprintf("#%d: %v", l.PR, err))
		}
		if !ok {
			res.Reasons = append(res.Reasons, fmt.Sprintf("#%d: head %s does not contain %s (rebase the stack)", l.PR, l.Head, prev))
		}
		prev = l.Head
	}
	if len(res.Reasons) > 0 {
		return finish(MergeRefused, "rebase the stack (GitHub's \"Rebase stack\" or gh stack), then run again", "")
	}
	if ctx.Err() != nil {
		return finish(MergeInterrupted, "", "interrupted")
	}

	// Point of no return.
	dctx := context.WithoutCancel(ctx)
	status, asyncErr := mergeAsync(dctx, o, top.Head, sm)
	sm.AsyncStatus = status
	if asyncErr != nil {
		sm.AsyncError = asyncErr.Error()
	}

	// GitHub's stack merge is atomic. A "failed" result counts as refused
	// only when every layer still reads back open at its pinned head.
	if status == "failed" {
		if allOpen(dctx, o, sm.Layers) {
			res.Reasons = append(res.Reasons, "GitHub refused the stack merge: "+sm.AsyncError)
			return finish(MergeRefused, "fix what GitHub reported (branch rules, checks, approvals), then run again", "")
		}
		return finish(MergeUnconfirmed, "check every layer of the stack on GitHub before verifying a deploy", "the stack merge failed, but not every layer reads back open")
	}
	if status == "enqueued" {
		return finish(MergeUnconfirmed, "GitHub queued the stack instead of merging it: check the merge queue before verifying a deploy", "the stack was enqueued, not merged")
	}

	var msgs []string
	for i, l := range sm.Layers {
		c := confirmMerged(dctx, o.forPR(l.PR), l.Head)
		res.AutoMerge = res.AutoMerge || c.autoMerge
		if c.sha == "" {
			msg := fmt.Sprintf("#%d did not read back as merged at %s", l.PR, l.Head)
			if c.err != nil {
				msg = fmt.Sprintf("#%d: %v", l.PR, c.err)
			}
			msgs = append(msgs, msg)
			continue
		}
		sm.Layers[i].MergeSHA = c.sha
		if c.base != si.Base {
			msgs = append(msgs, fmt.Sprintf("#%d merged into %q, not the stack base %q", l.PR, c.base, si.Base))
		}
	}
	if len(msgs) > 0 {
		if sm.AsyncError != "" {
			msgs = append([]string{sm.AsyncError}, msgs...)
		}
		return finish(MergeUnconfirmed, "check every layer of the stack on GitHub before verifying a deploy", strings.Join(msgs, "; "))
	}
	res.MergeSHA = sm.Layers[len(sm.Layers)-1].MergeSHA

	if o.SyncBranch == "" {
		return finish(MergeMerged, "verify the deploy with the merge_sha (the top layer's: it contains every layer)", "")
	}
	res.Sync = syncBranch(dctx, o, si.Base)
	if res.Sync.Status != "synced" {
		return finish(MergeSyncFailed, "the merge landed; sync "+o.SyncBranch+" by hand (never force), then verify the deploy", "")
	}
	return finish(MergeMerged, "verify the deploy with the merge_sha (the top layer's: it contains every layer)", "")
}

// layerReasons are the single-PR readiness checks that apply to every layer
// of a stack: open, not a draft, no merge queue, a full head sha, mergeable.
func layerReasons(st prState, head string) []string {
	var r []string
	if st.State != "OPEN" {
		r = append(r, "PR is "+strings.ToLower(st.State))
	}
	if st.BaseRefName == "" {
		r = append(r, "PR base branch is unknown")
	}
	if st.IsMergeQueueEnabled == nil {
		r = append(r, "whether the base branch uses a merge queue is unknown")
	} else if *st.IsMergeQueueEnabled {
		r = append(r, "the base branch uses a merge queue (the stack would be queued, not merged)")
	}
	if st.IsDraft == nil {
		r = append(r, "PR draft state is unknown")
	} else if *st.IsDraft {
		r = append(r, "PR is a draft")
	}
	if !fullSHARe.MatchString(head) {
		r = append(r, "PR head is not a full commit sha")
	}
	switch st.Mergeable {
	case "MERGEABLE":
	case "CONFLICTING":
		r = append(r, "PR has merge conflicts with "+st.BaseRefName)
	default:
		r = append(r, fmt.Sprintf("GitHub has not computed mergeability (mergeable=%s)", st.Mergeable))
	}
	return r
}

// contains reports whether head contains base (base is an ancestor of head
// or the same commit), from GitHub's compare endpoint.
func contains(ctx context.Context, o MergeOptions, base, head string) (bool, error) {
	out, err := o.GH(ctx, "api", fmt.Sprintf("repos/%s/compare/%s...%s?per_page=1", o.Repo, escapeRef(base), head), "--jq", ".status")
	if err != nil {
		return false, err
	}
	switch s := strings.TrimSpace(string(out)); s {
	case "ahead", "identical":
		return true, nil
	case "behind", "diverged":
		return false, nil
	default:
		return false, fmt.Errorf("compare %s...%s: unknown status %q", base, head, s)
	}
}

var asyncUUIDRe = regexp.MustCompile(`^[0-9A-Za-z-]{1,64}$`)

// asyncResult is the asynchronous merge's response and poll result.
type asyncResult struct {
	Status  string `json:"status"`
	UUID    string `json:"uuid"`
	Message string `json:"message"`
	Details *struct {
		UUID    string `json:"uuid"`
		Message string `json:"message"`
		Reason  string `json:"reason"`
	} `json:"details"`
}

func (a asyncResult) uuid() string {
	if a.Details != nil && a.Details.UUID != "" {
		return a.Details.UUID
	}
	return a.UUID
}

func (a asyncResult) message() string {
	var parts []string
	if a.Message != "" {
		parts = append(parts, a.Message)
	}
	if a.Details != nil {
		for _, s := range []string{a.Details.Message, a.Details.Reason} {
			if s != "" {
				parts = append(parts, s)
			}
		}
	}
	return strings.Join(parts, ": ")
}

// mergeAsync requests the stack merge through the asynchronous endpoint,
// pinned to the top layer's head, and polls its result for up to StackWait.
// It returns the last status it saw (pending, merged, enqueued, failed, or
// "" if no response could be read) and any error.
func mergeAsync(ctx context.Context, o MergeOptions, head string, sm *StackMerge) (string, error) {
	path := fmt.Sprintf("repos/%s/pulls/%d/merge-async", o.Repo, sm.Layers[len(sm.Layers)-1].PR)
	args := []string{"api", "-X", "PUT", path, "-H", "X-GitHub-Api-Version: " + StackAPIVersion,
		"-f", "sha=" + head, "-f", "merge_method=" + o.Method, "-f", "merge_action=direct_merge",
		"-F", fmt.Sprintf("bypass_rules=%t", o.Admin)}
	out, err := o.GH(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("merge-async: %w", err)
	}
	var a asyncResult
	if err := json.Unmarshal(out, &a); err != nil {
		return "", fmt.Errorf("merge-async: unreadable response: %w", err)
	}
	deadline := time.Now().Add(o.StackWait)
	for {
		switch a.Status {
		case "merged", "enqueued":
			return a.Status, nil
		case "failed":
			msg := a.message()
			if msg == "" {
				msg = "no reason given"
			}
			return a.Status, fmt.Errorf("%s", msg)
		case "pending":
		default:
			return a.Status, fmt.Errorf("merge-async: unknown status %q", a.Status)
		}
		id := a.uuid()
		if !asyncUUIDRe.MatchString(id) {
			return a.Status, fmt.Errorf("merge-async: pending without a readable request id")
		}
		if !time.Now().Before(deadline) {
			return a.Status, fmt.Errorf("merge-async: still pending after %s", o.StackWait)
		}
		time.Sleep(min(o.ConfirmInterval, time.Until(deadline)))
		out, err := o.GH(ctx, "api", path+"/"+id, "-H", "X-GitHub-Api-Version: "+StackAPIVersion)
		if err != nil {
			return a.Status, fmt.Errorf("merge-async poll: %w", err)
		}
		var next asyncResult
		if err := json.Unmarshal(out, &next); err != nil {
			return a.Status, fmt.Errorf("merge-async poll: unreadable response: %w", err)
		}
		if next.uuid() == "" {
			if next.Details == nil {
				next.UUID = id
			} else {
				next.Details.UUID = id
			}
		}
		a = next
	}
}

// allOpen reports whether every layer reads back OPEN at its pinned head:
// the only state in which a failed stack merge proves nothing landed.
func allOpen(ctx context.Context, o MergeOptions, layers []StackLayer) bool {
	for _, l := range layers {
		st, err := readPR(ctx, o.forPR(l.PR))
		if err != nil || st.State != "OPEN" || !strings.EqualFold(st.HeadRefOid, l.Head) {
			return false
		}
	}
	return true
}
