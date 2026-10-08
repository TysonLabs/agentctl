package pr

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"
)

const mergeSHA = "4444444444444444444444444444444444444444"

// mergeFake answers the gh calls Merge makes and records every call. The PR
// fields come from one document served both as `gh pr view --json` and as the
// GraphQL pullRequest query, so a test means the same thing on either path.
type mergeFake struct {
	mu                sync.Mutex
	state             string // default OPEN
	base              string // default main
	baseAfterMerge    string
	draft             bool
	omitDraft         bool
	omitHead          bool
	omitBase          bool
	queue             string   // isMergeQueueEnabled: "" false, "true", "omit"
	autoMerge         bool     // autoMergeRequest set after a merge attempt
	mergedHead        string   // headRefOid once merged; default head
	mergeLands        bool     // pr merge errors, but the merge lands anyway
	mergedAfter       int      // read-backs after the merge call before MERGED shows
	mergeable         []string // one per readiness read; the last repeats. default MERGEABLE
	threads           string   // raw GraphQL response; default: one resolved thread, complete
	draftAfterThreads bool
	mergeErr          error
	confirmErr        error
	merged            bool // set by a successful pr merge
	neverMerg         bool // pr merge succeeds but the PR never reads back as merged
	syncErr           error
	mergeAttempted    bool
	readbacks         int
	views             int
	calls             [][]string
}

// prDoc is the PR as GitHub would report it now.
func (f *mergeFake) prDoc() map[string]any {
	base := f.base
	if base == "" {
		base = "main"
	}
	if f.mergeAttempted {
		f.readbacks++
		if (f.merged || f.mergeLands) && !f.neverMerg && f.readbacks > f.mergedAfter {
			if f.baseAfterMerge != "" {
				base = f.baseAfterMerge
			}
			h := f.mergedHead
			if h == "" {
				h = head
			}
			return map[string]any{"state": "MERGED", "isDraft": false, "headRefOid": strings.ToUpper(h), "baseRefName": base,
				"mergeable": "UNKNOWN", "isMergeQueueEnabled": false, "autoMergeRequest": nil,
				"mergeCommit": map[string]any{"oid": strings.ToUpper(mergeSHA)}}
		}
	}
	st := f.state
	if st == "" {
		st = "OPEN"
	}
	m := "MERGEABLE"
	if len(f.mergeable) > 0 && !f.mergeAttempted {
		i := f.views
		if i >= len(f.mergeable) {
			i = len(f.mergeable) - 1
		}
		m = f.mergeable[i]
		f.views++
	} else if !f.mergeAttempted {
		f.views++
	}
	pv := map[string]any{"state": st, "mergeable": m, "mergeCommit": nil, "autoMergeRequest": nil}
	if f.mergeAttempted && f.autoMerge {
		pv["autoMergeRequest"] = map[string]any{"enabledAt": "2026-10-08T00:00:00Z"}
	}
	if !f.omitBase {
		pv["baseRefName"] = base
	}
	if !f.omitDraft {
		pv["isDraft"] = f.draft
	}
	if !f.omitHead {
		pv["headRefOid"] = strings.ToUpper(head)
	}
	switch f.queue {
	case "":
		pv["isMergeQueueEnabled"] = false
	case "true":
		pv["isMergeQueueEnabled"] = true
	}
	return pv
}

func (f *mergeFake) gh(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	query := ""
	for _, a := range args {
		if strings.HasPrefix(a, "query=") {
			query = a
		}
	}
	switch {
	case args[0] == "pr" && args[1] == "view":
		if f.mergeAttempted && f.confirmErr != nil {
			return nil, f.confirmErr
		}
		return json.Marshal(f.prDoc())
	case args[0] == "api" && args[1] == "graphql" && strings.Contains(query, "reviewThreads"):
		if f.draftAfterThreads {
			f.draft = true
		}
		if f.threads != "" {
			return []byte(f.threads), nil
		}
		return []byte(`[{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[{"isResolved":true}]}}}}}]`), nil
	case args[0] == "api" && args[1] == "graphql":
		if f.mergeAttempted && f.confirmErr != nil {
			return nil, f.confirmErr
		}
		return json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": f.prDoc()}}})
	case args[0] == "pr" && args[1] == "merge":
		f.mergeAttempted = true
		if f.mergeErr != nil {
			return nil, f.mergeErr
		}
		f.merged = true
		return nil, nil
	case args[0] == "api" && strings.HasPrefix(args[1], "repos/o/r/git/ref/heads/"):
		return []byte(`{"object":{"sha":"` + mergeSHA + `"}}`), nil
	case args[0] == "api" && args[1] == "-X":
		return nil, f.syncErr
	}
	return nil, errors.New("unexpected gh call: " + strings.Join(args, " "))
}

func (f *mergeFake) called(prefix ...string) [][]string {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out [][]string
	for _, c := range f.calls {
		if len(c) >= len(prefix) && strings.Join(c[:len(prefix)], " ") == strings.Join(prefix, " ") {
			out = append(out, c)
		}
	}
	return out
}

func runMerge(f *mergeFake, mod func(*MergeOptions)) MergeResult {
	o := MergeOptions{Repo: "o/r", PR: 7, Method: "merge", GH: f.gh,
		MergeableWait: 40 * time.Millisecond, MergeableInterval: 5 * time.Millisecond,
		ConfirmWait: 40 * time.Millisecond, ConfirmInterval: 5 * time.Millisecond}
	if mod != nil {
		mod(&o)
	}
	return Merge(context.Background(), o)
}

func TestMergeHappyPath(t *testing.T) {
	f := &mergeFake{}
	r := runMerge(f, nil)
	if r.Status != MergeMerged || r.MergeSHA != mergeSHA || r.Head != head || r.Base != "main" || r.Sync != nil {
		t.Fatalf("got %+v", r)
	}
	m := f.called("pr", "merge")
	if len(m) != 1 {
		t.Fatalf("want one pr merge call, got %v", m)
	}
	got := strings.Join(m[0], " ")
	if !strings.Contains(got, "--merge --match-head-commit "+head) || strings.Contains(got, "--delete-branch") || strings.Contains(got, "--auto") || strings.Contains(got, "--admin") {
		t.Fatalf("pr merge args: %s", got)
	}
}

func TestMergeMethodAndAdmin(t *testing.T) {
	f := &mergeFake{}
	r := runMerge(f, func(o *MergeOptions) { o.Method, o.Admin = "squash", true })
	got := strings.Join(f.called("pr", "merge")[0], " ")
	if r.Status != MergeMerged || !strings.Contains(got, "--squash") || !strings.HasSuffix(got, "--admin") {
		t.Fatalf("status %s, args %s", r.Status, got)
	}
}

func TestMergeRefusals(t *testing.T) {
	cases := []struct {
		name string
		f    *mergeFake
		mod  func(*MergeOptions)
		want string
	}{
		{"closed", &mergeFake{state: "CLOSED"}, nil, "PR is closed"},
		{"draft", &mergeFake{draft: true}, nil, "draft"},
		{"head moved", &mergeFake{}, func(o *MergeOptions) { o.Head = older }, "not the expected"},
		{"conflicts", &mergeFake{mergeable: []string{"CONFLICTING"}}, nil, "merge conflicts"},
		{"unknown stays", &mergeFake{mergeable: []string{"UNKNOWN"}}, nil, "not computed mergeability"},
		{"open thread", &mergeFake{threads: `[{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[{"isResolved":true},{"isResolved":false}]}}}}}]`}, nil, "1 unresolved"},
		{"thread state missing", &mergeFake{threads: `[{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[{}]}}}}}]`}, nil, "1 unresolved"},
		{"paging incomplete", &mergeFake{threads: `[{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":true},"nodes":[{"isResolved":true}]}}}}}]`}, nil, "incomplete page set"},
		{"paging state missing", &mergeFake{threads: `[{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{},"nodes":[]}}}}}]`}, nil, "incomplete page set"},
		{"thread nodes missing", &mergeFake{threads: `[{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false}}}}}}]`}, nil, "incomplete page set"},
		{"draft state missing", &mergeFake{omitDraft: true}, nil, "draft state is unknown"},
		{"head missing", &mergeFake{omitHead: true}, nil, "head is not a full commit sha"},
		{"sync is base", &mergeFake{}, func(o *MergeOptions) { o.SyncBranch = "main" }, "base branch"},
	}
	for _, c := range cases {
		r := runMerge(c.f, c.mod)
		if r.Status != MergeRefused || !strings.Contains(strings.Join(r.Reasons, "; "), c.want) {
			t.Errorf("%s: got %s %v", c.name, r.Status, r.Reasons)
		}
		if m := c.f.called("pr", "merge"); len(m) != 0 {
			t.Errorf("%s: merged despite refusal: %v", c.name, m)
		}
	}
}

func TestMergeReadsPRFieldsAfterReviewThreads(t *testing.T) {
	f := &mergeFake{draftAfterThreads: true}
	r := runMerge(f, nil)
	if r.Status != MergeRefused || !strings.Contains(strings.Join(r.Reasons, "; "), "PR is a draft") {
		t.Fatalf("got %+v", r)
	}
	if len(f.called("pr", "merge")) != 0 {
		t.Fatal("merged using PR fields read before review-thread pagination")
	}
}

func TestMergeHeadMatchIsCaseInsensitive(t *testing.T) {
	r := runMerge(&mergeFake{}, func(o *MergeOptions) { o.Head = strings.ToUpper(head) })
	if r.Status != MergeMerged {
		t.Fatalf("got %+v", r)
	}
}

func TestMergeWaitsOutUnknownMergeable(t *testing.T) {
	f := &mergeFake{mergeable: []string{"UNKNOWN", "UNKNOWN", "MERGEABLE"}}
	r := runMerge(f, nil)
	if r.Status != MergeMerged || f.views < 3 {
		t.Fatalf("status %s after %d reads", r.Status, f.views)
	}
}

func TestMergeGHFailureWithFailedReadbackIsUnconfirmed(t *testing.T) {
	f := &mergeFake{
		mergeErr:   errors.New("gh pr merge: request timed out"),
		confirmErr: errors.New("gh pr view: connection reset"),
	}
	r := runMerge(f, nil)
	if r.Status != MergeUnconfirmed || !strings.Contains(r.Error, "confirmation failed") || r.MergeSHA != "" {
		t.Fatalf("got %+v", r)
	}
}

func TestMergeUnconfirmed(t *testing.T) {
	r := runMerge(&mergeFake{neverMerg: true}, nil)
	if r.Status != MergeUnconfirmed || r.MergeSHA != "" {
		t.Fatalf("got %+v", r)
	}
}

func TestMergeSync(t *testing.T) {
	f := &mergeFake{}
	r := runMerge(f, func(o *MergeOptions) { o.SyncBranch = "development" })
	if r.Status != MergeMerged || r.Sync == nil || r.Sync.Status != "synced" || r.Sync.SHA != mergeSHA {
		t.Fatalf("got %+v sync %+v", r, r.Sync)
	}
	p := f.called("api", "-X", "PATCH")
	if len(p) != 1 {
		t.Fatalf("want one PATCH, got %v", p)
	}
	got := strings.Join(p[0], " ")
	if !strings.Contains(got, "repos/o/r/git/refs/heads/development") || !strings.Contains(got, "-F force=false") || !strings.Contains(got, "sha="+mergeSHA) {
		t.Fatalf("PATCH args: %s", got)
	}
}

func TestMergeSyncUsesConfirmedBase(t *testing.T) {
	f := &mergeFake{baseAfterMerge: "release"}
	r := runMerge(f, func(o *MergeOptions) { o.SyncBranch = "development" })
	if r.Status != MergeMerged || r.Base != "release" {
		t.Fatalf("got %+v", r)
	}
	reads := f.called("api", "repos/o/r/git/ref/heads/release")
	if len(reads) != 1 {
		t.Fatalf("base-head reads: %v", f.calls)
	}
}

func TestMergeSyncEscapesBaseRefInRESTPath(t *testing.T) {
	f := &mergeFake{base: "release#next"}
	r := runMerge(f, func(o *MergeOptions) { o.SyncBranch = "development" })
	if r.Status != MergeMerged {
		t.Fatalf("got %+v", r)
	}
	reads := f.called("api", "repos/o/r/git/ref/heads/release%23next")
	if len(reads) != 1 {
		t.Fatalf("base-head reads: %v", f.calls)
	}
}

func TestMergeSyncRefusesConfirmedBase(t *testing.T) {
	f := &mergeFake{baseAfterMerge: "development"}
	r := runMerge(f, func(o *MergeOptions) { o.SyncBranch = "development" })
	if r.Status != MergeSyncFailed || r.Sync == nil || r.Sync.Status != "refused" {
		t.Fatalf("got %+v", r)
	}
	if len(f.called("api", "-X", "PATCH")) != 0 {
		t.Fatal("updated the merged PR's base branch")
	}
}

func TestMergeSyncNotFastForward(t *testing.T) {
	f := &mergeFake{syncErr: errors.New("gh api: Update is not a fast forward (HTTP 422)")}
	r := runMerge(f, func(o *MergeOptions) { o.SyncBranch = "development" })
	if r.Status != MergeSyncFailed || r.MergeSHA != mergeSHA || r.Sync.Status != "refused" {
		t.Fatalf("got %+v sync %+v", r, r.Sync)
	}
}

func TestMergeSyncOtherFailure(t *testing.T) {
	f := &mergeFake{syncErr: errors.New("gh api: HTTP 502")}
	r := runMerge(f, func(o *MergeOptions) { o.SyncBranch = "development" })
	if r.Status != MergeSyncFailed || r.Sync.Status != "failed" {
		t.Fatalf("got %+v sync %+v", r, r.Sync)
	}
}

func TestMergeSyncUnrelated422IsFailure(t *testing.T) {
	f := &mergeFake{syncErr: errors.New("gh api: Reference does not exist (HTTP 422)")}
	r := runMerge(f, func(o *MergeOptions) { o.SyncBranch = "development" })
	if r.Status != MergeSyncFailed || r.Sync.Status != "failed" {
		t.Fatalf("got %+v sync %+v", r, r.Sync)
	}
}

func TestMergeGraphQLStringVariablesAreRawFields(t *testing.T) {
	f := &mergeFake{}
	r := runMerge(f, func(o *MergeOptions) { o.Repo = "true/null" })
	if r.Status != MergeMerged {
		t.Fatalf("got %+v", r)
	}
	calls := f.called("api", "graphql")
	if len(calls) < 2 { // the thread count and at least one PR read
		t.Fatalf("GraphQL calls: %v", calls)
	}
	for _, c := range calls {
		if got := strings.Join(c, " "); !strings.Contains(got, "-f o=true -f n=null -F p=7") {
			t.Fatalf("GraphQL args use typed fields for string variables: %s", got)
		}
	}
}

func TestMergePollingRespectsDeadlines(t *testing.T) {
	t.Run("mergeable", func(t *testing.T) {
		f := &mergeFake{mergeable: []string{"UNKNOWN"}}
		start := time.Now()
		r := runMerge(f, func(o *MergeOptions) {
			o.MergeableWait = 10 * time.Millisecond
			o.MergeableInterval = 500 * time.Millisecond
		})
		if elapsed := time.Since(start); r.Status != MergeRefused || elapsed >= 250*time.Millisecond {
			t.Fatalf("status %s after %s", r.Status, elapsed)
		}
	})
	t.Run("confirmation", func(t *testing.T) {
		f := &mergeFake{neverMerg: true}
		start := time.Now()
		r := runMerge(f, func(o *MergeOptions) {
			o.ConfirmWait = 10 * time.Millisecond
			o.ConfirmInterval = 500 * time.Millisecond
		})
		if elapsed := time.Since(start); r.Status != MergeUnconfirmed || elapsed >= 250*time.Millisecond {
			t.Fatalf("status %s after %s", r.Status, elapsed)
		}
	})
}

func TestMergeInterruptedBeforeMerge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &mergeFake{}
	r := Merge(ctx, MergeOptions{Repo: "o/r", PR: 7, Method: "merge", GH: f.gh})
	if r.Status != MergeInterrupted || len(f.called("pr", "merge")) != 0 {
		t.Fatalf("got %+v", r)
	}
}

// Confirmation-round fixes (C1–C4).

func TestMergeRefusesMergeQueue(t *testing.T) {
	for _, q := range []string{"true", "omit"} {
		f := &mergeFake{queue: q}
		r := runMerge(f, nil)
		if r.Status != MergeRefused || !strings.Contains(strings.Join(r.Reasons, "; "), "merge queue") || len(f.called("pr", "merge")) != 0 {
			t.Errorf("queue=%s: got %s %v, merge calls %d", q, r.Status, r.Reasons, len(f.called("pr", "merge")))
		}
	}
}

func TestMergeFailureWithAutoMergeIsUnconfirmed(t *testing.T) {
	f := &mergeFake{mergeErr: errors.New("gh pr merge: HTTP 502"), autoMerge: true}
	r := runMerge(f, nil)
	if r.Status != MergeUnconfirmed || !r.AutoMerge || r.MergeSHA != "" {
		t.Fatalf("got %+v", r)
	}
}

func TestMergeFailureThatLandsLateIsMerged(t *testing.T) {
	f := &mergeFake{mergeErr: errors.New("gh pr merge: HTTP 502"), mergeLands: true, mergedAfter: 2}
	r := runMerge(f, nil)
	if r.Status != MergeMerged || r.MergeSHA != mergeSHA {
		t.Fatalf("got %+v", r)
	}
}

func TestMergeFailureNeverMergedIsUnconfirmedNotGHError(t *testing.T) {
	f := &mergeFake{mergeErr: errors.New("gh pr merge: Head branch was modified")}
	r := runMerge(f, nil)
	if r.Status != MergeUnconfirmed || !strings.Contains(r.Error, "Head branch was modified") || r.MergeSHA != "" {
		t.Fatalf("got %+v", r)
	}
}

func TestMergeReadBackWithOtherHeadIsUnconfirmed(t *testing.T) {
	f := &mergeFake{mergedHead: older}
	r := runMerge(f, nil)
	if r.Status != MergeUnconfirmed || r.MergeSHA != "" {
		t.Fatalf("got %+v", r)
	}
}

func TestMergeRefusesMissingBase(t *testing.T) {
	f := &mergeFake{omitBase: true}
	r := runMerge(f, nil)
	if r.Status != MergeRefused || !strings.Contains(strings.Join(r.Reasons, "; "), "base branch") || len(f.called("pr", "merge")) != 0 {
		t.Fatalf("got %s %v", r.Status, r.Reasons)
	}
}
