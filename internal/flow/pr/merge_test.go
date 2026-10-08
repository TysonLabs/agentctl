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

// mergeFake answers the gh calls Merge makes and records every call.
type mergeFake struct {
	mu        sync.Mutex
	state     string // default OPEN
	draft     bool
	mergeable []string // one per pr view read; the last repeats. default MERGEABLE
	threads   string   // raw GraphQL response; default: one resolved thread, complete
	mergeErr  error
	merged    bool // set by a successful pr merge
	neverMerg bool // pr merge succeeds but the PR never reads back as merged
	syncErr   error
	views     int
	calls     [][]string
}

func (f *mergeFake) gh(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	switch {
	case args[0] == "pr" && args[1] == "view":
		st := f.state
		if st == "" {
			st = "OPEN"
		}
		if f.merged && !f.neverMerg {
			return json.Marshal(map[string]any{"state": "MERGED", "mergeCommit": map[string]any{"oid": strings.ToUpper(mergeSHA)}})
		}
		m := "MERGEABLE"
		if len(f.mergeable) > 0 {
			i := f.views
			if i >= len(f.mergeable) {
				i = len(f.mergeable) - 1
			}
			m = f.mergeable[i]
		}
		f.views++
		return json.Marshal(map[string]any{"state": st, "isDraft": f.draft, "headRefOid": strings.ToUpper(head), "baseRefName": "main", "mergeable": m, "mergeCommit": nil})
	case args[0] == "api" && args[1] == "graphql":
		if f.threads != "" {
			return []byte(f.threads), nil
		}
		return []byte(`[{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[{"isResolved":true}]}}}}}]`), nil
	case args[0] == "pr" && args[1] == "merge":
		if f.mergeErr != nil {
			return nil, f.mergeErr
		}
		f.merged = true
		return nil, nil
	case args[0] == "api" && args[1] == "repos/o/r/git/ref/heads/main":
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
		{"paging incomplete", &mergeFake{threads: `[{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":true},"nodes":[{"isResolved":true}]}}}}}]`}, nil, "more pages"},
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

func TestMergeGHFailureMergesNothing(t *testing.T) {
	f := &mergeFake{mergeErr: errors.New("gh pr merge: Head branch was modified")}
	r := runMerge(f, nil)
	if r.Status != MergeGHError || !strings.Contains(r.Error, "Head branch was modified") || r.MergeSHA != "" {
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

func TestMergeInterruptedBeforeMerge(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &mergeFake{}
	r := Merge(ctx, MergeOptions{Repo: "o/r", PR: 7, Method: "merge", GH: f.gh})
	if r.Status != MergeInterrupted || len(f.called("pr", "merge")) != 0 {
		t.Fatalf("got %+v", r)
	}
}
