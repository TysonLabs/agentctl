package pr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

// stackLayerFake is one PR of a stackFake.
type stackLayerFake struct {
	pr        int
	state     string // OPEN, MERGED, CLOSED
	draft     bool
	mergeable string // default MERGEABLE
	queue     bool
	threads   int // unresolved review threads
	movedHead bool
}

// stackFake answers the gh calls of a stacked merge: PRs 11, 12, 13 by
// default, branches s1..s3 stacked on main. Heads are "<pr>" repeated.
type stackFake struct {
	mu         sync.Mutex
	layers     []*stackLayerFake
	stackNull  bool   // the stack query reports no stack
	stackRaw   string // raw stack query response
	compare    map[string]string
	asyncResp  string // PUT merge-async response
	asyncErr   error
	polls      []string // poll responses, in order; the last repeats
	landed     bool     // every layer up to the target reads back MERGED after the PUT
	landOnly   int      // if > 0, only this many layers land
	mergedBase string
	put        bool
	target     int // index of the requested layer
	calls      [][]string
}

func newStackFake() *stackFake {
	return &stackFake{
		layers:  []*stackLayerFake{{pr: 11, state: "OPEN"}, {pr: 12, state: "OPEN"}, {pr: 13, state: "OPEN"}},
		compare: map[string]string{}, asyncResp: `{"status":"merged"}`, landed: true,
	}
}

func layerHead(pr int) string { return strings.Repeat(fmt.Sprint(pr%10), 40) }
func layerMergeSHA(pr int) string {
	return strings.Repeat(string(rune('a'+pr%10)), 40)
}

func (f *stackFake) layer(pr int) (int, *stackLayerFake) {
	for i, l := range f.layers {
		if l.pr == pr {
			return i, l
		}
	}
	return -1, nil
}

func (f *stackFake) branch(i int) string {
	if i < 0 {
		return "main"
	}
	return fmt.Sprintf("s%d", i+1)
}

func (f *stackFake) gh(_ context.Context, args ...string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, args)
	query, pr := "", 0
	for i, a := range args {
		if strings.HasPrefix(a, "query=") {
			query = a
		}
		if a == "-F" && i+1 < len(args) && strings.HasPrefix(args[i+1], "p=") {
			fmt.Sscanf(args[i+1], "p=%d", &pr)
		}
	}
	switch {
	case args[0] == "api" && args[1] == "graphql" && strings.Contains(query, "stackEntry"):
		if f.stackRaw != "" {
			return []byte(f.stackRaw), nil
		}
		if f.stackNull {
			return []byte(`{"data":{"repository":{"pullRequest":{"stackEntry":null,"stack":null}}}}`), nil
		}
		i, _ := f.layer(pr)
		var nodes []map[string]any
		for j, l := range f.layers {
			nodes = append(nodes, map[string]any{"position": j + 1, "pullRequest": map[string]any{"number": l.pr, "state": f.stateOf(j)}})
		}
		return json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"stackEntry": map[string]any{"position": i + 1},
			"stack": map[string]any{"number": 5, "baseRefName": "main", "size": len(f.layers),
				"entries": map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": nodes}},
		}}}})
	case args[0] == "api" && args[1] == "graphql" && strings.Contains(query, "reviewThreads"):
		_, l := f.layer(pr)
		var nodes []string
		nodes = append(nodes, `{"isResolved":true}`)
		for range l.threads {
			nodes = append(nodes, `{"isResolved":false}`)
		}
		return []byte(`[{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[` + strings.Join(nodes, ",") + `]}}}}}]`), nil
	case args[0] == "api" && args[1] == "graphql":
		i, l := f.layer(pr)
		h := layerHead(l.pr)
		if l.movedHead && f.put {
			h = strings.Repeat("f", 40)
		}
		m := l.mergeable
		if m == "" {
			m = "MERGEABLE"
		}
		doc := map[string]any{"state": f.stateOf(i), "isDraft": l.draft, "headRefOid": h, "headRefName": f.branch(i),
			"baseRefName": f.baseOf(i), "mergeable": m, "isMergeQueueEnabled": l.queue, "autoMergeRequest": nil, "mergeCommit": nil}
		if f.stateOf(i) == "MERGED" {
			base := f.mergedBase
			if base == "" {
				base = "main"
			}
			doc["baseRefName"] = base
			doc["mergeCommit"] = map[string]any{"oid": layerMergeSHA(l.pr)}
		}
		return json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": doc}}})
	case args[0] == "api" && strings.HasPrefix(args[1], "repos/o/r/compare/"):
		key := strings.TrimSuffix(strings.TrimPrefix(args[1], "repos/o/r/compare/"), "?per_page=1")
		if s, ok := f.compare[key]; ok {
			return []byte(s + "\n"), nil
		}
		return []byte("ahead\n"), nil
	case args[0] == "api" && args[1] == "-X" && args[2] == "PUT":
		f.put = true
		if f.asyncErr != nil {
			return nil, f.asyncErr
		}
		return []byte(f.asyncResp), nil
	case args[0] == "api" && strings.Contains(args[1], "/merge-async/"):
		if len(f.polls) == 0 {
			return nil, errors.New("no poll response")
		}
		p := f.polls[0]
		if len(f.polls) > 1 {
			f.polls = f.polls[1:]
		}
		return []byte(p), nil
	case args[0] == "api" && strings.HasPrefix(args[1], "repos/o/r/git/ref/heads/"):
		return []byte(`{"object":{"sha":"` + mergeSHA + `"}}`), nil
	case args[0] == "api" && args[1] == "-X" && args[2] == "PATCH":
		return nil, nil
	}
	return nil, errors.New("unexpected gh call: " + strings.Join(args, " "))
}

// baseOf is layer i's base: the branch of the nearest unmerged layer below
// it, or the trunk (GitHub retargets a layer when the one below merges).
func (f *stackFake) baseOf(i int) string {
	for j := i - 1; j >= 0; j-- {
		if f.layers[j].state != "MERGED" {
			return f.branch(j)
		}
	}
	return "main"
}

// stateOf is layer i's state now: after a landed PUT, every open layer up
// to the requested one (or landOnly of them) reads MERGED.
func (f *stackFake) stateOf(i int) string {
	l := f.layers[i]
	if f.put && f.landed && l.state == "OPEN" && f.target >= 0 && i <= f.target {
		if f.landOnly == 0 || i < f.landOnly {
			return "MERGED"
		}
	}
	return l.state
}

func (f *stackFake) count(prefix string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	n := 0
	for _, c := range f.calls {
		if strings.HasPrefix(strings.Join(c, " "), prefix) {
			n++
		}
	}
	return n
}

func (f *stackFake) putArgs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if len(c) > 2 && c[1] == "-X" && c[2] == "PUT" {
			return c
		}
	}
	return nil
}

func runStackMerge(f *stackFake, pr int, mod func(*MergeOptions)) MergeResult {
	f.target, _ = f.layer(pr)
	o := MergeOptions{Repo: "o/r", PR: pr, Method: "squash", GH: f.gh,
		MergeableWait: 20 * time.Millisecond, MergeableInterval: 5 * time.Millisecond,
		ConfirmWait: 20 * time.Millisecond, ConfirmInterval: 2 * time.Millisecond, StackWait: 40 * time.Millisecond}
	if mod != nil {
		mod(&o)
	}
	return Merge(context.Background(), o)
}

func TestStackMergeMergesEveryLayerUpToTheTarget(t *testing.T) {
	f := newStackFake()
	res := runStackMerge(f, 12, nil)
	if res.Status != MergeMerged {
		t.Fatalf("status = %s (%v %s)", res.Status, res.Reasons, res.Error)
	}
	if res.Stack == nil || len(res.Stack.Layers) != 2 || res.Stack.Layers[0].PR != 11 || res.Stack.Layers[1].PR != 12 {
		t.Fatalf("layers = %+v", res.Stack)
	}
	if res.MergeSHA != layerMergeSHA(12) || res.Head != layerHead(12) || res.HeadBranch != "s2" || res.Base != "main" {
		t.Fatalf("result = %+v", res)
	}
	if res.Stack.Layers[0].MergeSHA != layerMergeSHA(11) || res.Stack.Layers[0].HeadBranch != "s1" {
		t.Fatalf("bottom layer = %+v", res.Stack.Layers[0])
	}
	put := strings.Join(f.putArgs(), " ")
	for _, want := range []string{"repos/o/r/pulls/12/merge-async", "sha=" + layerHead(12), "merge_method=squash",
		"merge_action=direct_merge", "bypass_rules=false", "X-GitHub-Api-Version: " + StackAPIVersion} {
		if !strings.Contains(put, want) {
			t.Errorf("merge-async call %q lacks %q", put, want)
		}
	}
	if f.count("pr merge") != 0 {
		t.Fatal("a stacked PR must never go through gh pr merge")
	}
	// Linearity: main...s1 head, s1 head...s2 head.
	if f.count("api repos/o/r/compare/main..."+layerHead(11)) != 1 || f.count("api repos/o/r/compare/"+layerHead(11)+"..."+layerHead(12)) != 1 {
		t.Fatalf("compare calls missing: %v", f.calls)
	}
}

func TestStackMergeSkipsMergedBottomLayers(t *testing.T) {
	f := newStackFake()
	f.layers[0].state = "MERGED"
	res := runStackMerge(f, 13, nil)
	if res.Status != MergeMerged || len(res.Stack.Layers) != 2 || res.Stack.Layers[0].PR != 12 {
		t.Fatalf("status = %s layers = %+v (%v %s)", res.Status, res.Stack, res.Reasons, res.Error)
	}
}

func TestStackMergeAdminBypassesRules(t *testing.T) {
	f := newStackFake()
	runStackMerge(f, 11, func(o *MergeOptions) { o.Admin = true })
	if !strings.Contains(strings.Join(f.putArgs(), " "), "bypass_rules=true") {
		t.Fatalf("--admin not passed as bypass_rules: %v", f.putArgs())
	}
}

func TestStackMergeRefusesAnyUnreadyLayer(t *testing.T) {
	cases := map[string]struct {
		mod  func(f *stackFake)
		want string
	}{
		"threads below":    {func(f *stackFake) { f.layers[0].threads = 2 }, "#11: 2 unresolved review thread(s)"},
		"draft below":      {func(f *stackFake) { f.layers[0].draft = true }, "#11: PR is a draft"},
		"conflict below":   {func(f *stackFake) { f.layers[0].mergeable = "CONFLICTING" }, "#11: PR has merge conflicts"},
		"queue":            {func(f *stackFake) { f.layers[0].queue = true }, "#11: the base branch uses a merge queue"},
		"closed below":     {func(f *stackFake) { f.layers[0].state = "CLOSED" }, "stack layer #11 is closed"},
		"target merged":    {func(f *stackFake) { f.layers[1].state = "MERGED"; f.layers[0].state = "MERGED" }, "PR #12 is not open"},
		"not linear":       {func(f *stackFake) { f.compare[layerHead(11)+"..."+layerHead(12)] = "diverged" }, "#12: head " + layerHead(12) + " does not contain"},
		"trunk moved":      {func(f *stackFake) { f.compare["main..."+layerHead(11)] = "behind" }, "#11: head " + layerHead(11) + " does not contain main"},
		"merged above gap": {func(f *stackFake) { f.layers[1].state = "MERGED" }, "stack layer #12 is merged above an unmerged layer"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newStackFake()
			c.mod(f)
			res := runStackMerge(f, 12, nil)
			if res.Status != MergeRefused || !strings.Contains(strings.Join(res.Reasons, "; "), c.want) {
				t.Fatalf("status = %s reasons = %v, want %q", res.Status, res.Reasons, c.want)
			}
			if f.putArgs() != nil {
				t.Fatal("a refused stack merge must not call merge-async")
			}
		})
	}
}

func TestStackMergeHeadPinsTheTarget(t *testing.T) {
	f := newStackFake()
	res := runStackMerge(f, 12, func(o *MergeOptions) { o.Head = layerHead(11) })
	if res.Status != MergeRefused || !strings.Contains(strings.Join(res.Reasons, ";"), "not the expected") {
		t.Fatalf("status = %s reasons = %v", res.Status, res.Reasons)
	}
	f = newStackFake()
	res = runStackMerge(f, 12, func(o *MergeOptions) { o.Head = strings.ToUpper(layerHead(12)) })
	if res.Status != MergeMerged {
		t.Fatalf("status = %s (%v)", res.Status, res.Reasons)
	}
}

func TestStackMergeSyncBranchCannotBeTheTrunk(t *testing.T) {
	f := newStackFake()
	res := runStackMerge(f, 12, func(o *MergeOptions) { o.SyncBranch = "main" })
	if res.Status != MergeRefused || !strings.Contains(strings.Join(res.Reasons, ";"), "stack's base branch") {
		t.Fatalf("status = %s reasons = %v", res.Status, res.Reasons)
	}
}

func TestStackMergeSyncsAfterMerge(t *testing.T) {
	f := newStackFake()
	res := runStackMerge(f, 12, func(o *MergeOptions) { o.SyncBranch = "development" })
	if res.Status != MergeMerged || res.Sync == nil || res.Sync.Status != "synced" {
		t.Fatalf("status = %s sync = %+v", res.Status, res.Sync)
	}
	if f.count("api repos/o/r/git/ref/heads/main") != 1 {
		t.Fatal("sync must read the stack base's head")
	}
}

func TestStackMergePollsAPendingMerge(t *testing.T) {
	f := newStackFake()
	f.asyncResp = `{"status":"pending","details":{"uuid":"abc-123"}}`
	f.polls = []string{`{"status":"pending"}`, `{"status":"merged"}`}
	res := runStackMerge(f, 12, nil)
	if res.Status != MergeMerged || res.Stack.AsyncStatus != "merged" {
		t.Fatalf("status = %s stack = %+v (%s)", res.Status, res.Stack, res.Error)
	}
	if f.count("api repos/o/r/pulls/12/merge-async/abc-123") != 2 {
		t.Fatalf("want two polls of the request id: %v", f.calls)
	}
}

func TestStackMergeFailedWithEveryLayerOpenIsRefused(t *testing.T) {
	f := newStackFake()
	f.landed = false
	f.asyncResp = `{"status":"pending","details":{"uuid":"abc"}}`
	f.polls = []string{`{"status":"failed","details":{"message":"required status check missing"}}`}
	res := runStackMerge(f, 12, nil)
	if res.Status != MergeRefused || !strings.Contains(strings.Join(res.Reasons, ";"), "required status check missing") {
		t.Fatalf("status = %s reasons = %v (%s)", res.Status, res.Reasons, res.Error)
	}
}

func TestStackMergeFailedButALayerLandedIsUnconfirmed(t *testing.T) {
	f := newStackFake()
	f.landOnly = 1
	f.asyncResp = `{"status":"failed"}`
	res := runStackMerge(f, 12, nil)
	if res.Status != MergeUnconfirmed {
		t.Fatalf("status = %s (%v %s)", res.Status, res.Reasons, res.Error)
	}
}

func TestStackMergePartialReadBackIsUnconfirmed(t *testing.T) {
	f := newStackFake()
	f.landOnly = 1
	res := runStackMerge(f, 12, nil)
	if res.Status != MergeUnconfirmed || res.MergeSHA != "" || !strings.Contains(res.Error, "#12 did not read back as merged") {
		t.Fatalf("status = %s merge_sha = %q error = %s", res.Status, res.MergeSHA, res.Error)
	}
}

func TestStackMergeLayerMovedAfterPinIsUnconfirmed(t *testing.T) {
	f := newStackFake()
	f.layers[0].movedHead = true
	res := runStackMerge(f, 12, nil)
	if res.Status != MergeUnconfirmed || !strings.Contains(res.Error, "#11") {
		t.Fatalf("status = %s error = %s", res.Status, res.Error)
	}
}

func TestStackMergeMergedIntoOtherBaseIsUnconfirmed(t *testing.T) {
	f := newStackFake()
	f.mergedBase = "s1"
	res := runStackMerge(f, 12, nil)
	if res.Status != MergeUnconfirmed || !strings.Contains(res.Error, "not the stack base") {
		t.Fatalf("status = %s error = %s", res.Status, res.Error)
	}
}

func TestStackMergeEnqueuedIsUnconfirmed(t *testing.T) {
	f := newStackFake()
	f.landed = false
	f.asyncResp = `{"status":"enqueued"}`
	res := runStackMerge(f, 12, nil)
	if res.Status != MergeUnconfirmed || !strings.Contains(res.Next, "merge queue") {
		t.Fatalf("status = %s next = %s", res.Status, res.Next)
	}
}

func TestStackMergeCallErrorThatLandsIsMerged(t *testing.T) {
	f := newStackFake()
	f.asyncErr = errors.New("gh api: HTTP 502")
	res := runStackMerge(f, 12, nil)
	if res.Status != MergeMerged {
		t.Fatalf("status = %s (%s)", res.Status, res.Error)
	}
	f = newStackFake()
	f.asyncErr = errors.New("gh api: HTTP 502")
	f.landed = false
	res = runStackMerge(f, 12, nil)
	if res.Status != MergeUnconfirmed || !strings.Contains(res.Error, "HTTP 502") {
		t.Fatalf("status = %s (%s)", res.Status, res.Error)
	}
}

func TestStackMergeStillPendingIsUnconfirmed(t *testing.T) {
	f := newStackFake()
	f.landed = false
	f.asyncResp = `{"status":"pending","details":{"uuid":"abc"}}`
	f.polls = []string{`{"status":"pending"}`}
	res := runStackMerge(f, 12, nil)
	if res.Status != MergeUnconfirmed || !strings.Contains(res.Error, "still pending") {
		t.Fatalf("status = %s (%s)", res.Status, res.Error)
	}
}

func TestStackMergeUnreadableStackIsGHError(t *testing.T) {
	cases := map[string]string{
		"entry without stack": `{"data":{"repository":{"pullRequest":{"stackEntry":{"position":1},"stack":null}}}}`,
		"fields missing":      `{"data":{"repository":{"pullRequest":{}}}}`,
		"size mismatch": `{"data":{"repository":{"pullRequest":{"stackEntry":{"position":1},"stack":{"number":5,"baseRefName":"main","size":3,` +
			`"entries":{"pageInfo":{"hasNextPage":false},"nodes":[{"position":1,"pullRequest":{"number":12,"state":"OPEN"}}]}}}}}}`,
		"wrong position": `{"data":{"repository":{"pullRequest":{"stackEntry":{"position":1},"stack":{"number":5,"baseRefName":"main","size":1,` +
			`"entries":{"pageInfo":{"hasNextPage":false},"nodes":[{"position":1,"pullRequest":{"number":99,"state":"OPEN"}}]}}}}}}`,
		"unreadable entry PR": `{"data":{"repository":{"pullRequest":{"stackEntry":{"position":1},"stack":{"number":5,"baseRefName":"main","size":1,` +
			`"entries":{"pageInfo":{"hasNextPage":false},"nodes":[{"position":1,"pullRequest":null}]}}}}}}`,
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			f := newStackFake()
			f.stackRaw = raw
			res := runStackMerge(f, 12, nil)
			if res.Status != MergeGHError || f.putArgs() != nil || f.count("pr merge") != 0 {
				t.Fatalf("status = %s (%s)", res.Status, res.Error)
			}
		})
	}
}

func TestMergeWithoutStackSchemaUsesSinglePRPath(t *testing.T) {
	f := &mergeFake{}
	o := MergeOptions{Repo: "o/r", PR: 7, Method: "merge",
		GH: func(ctx context.Context, args ...string) ([]byte, error) {
			for _, a := range args {
				if strings.Contains(a, "stackEntry") {
					return nil, errors.New("gh api: GraphQL: Field 'stackEntry' doesn't exist on type 'PullRequest'")
				}
			}
			return f.gh(ctx, args...)
		},
		MergeableWait: 40 * time.Millisecond, MergeableInterval: 5 * time.Millisecond,
		ConfirmWait: 40 * time.Millisecond, ConfirmInterval: 5 * time.Millisecond}
	res := Merge(context.Background(), o)
	if res.Status != MergeMerged || res.Stack != nil || len(f.called("pr", "merge")) != 1 {
		t.Fatalf("status = %s (%s)", res.Status, res.Error)
	}
}

func TestMergeStackReadErrorIsGHError(t *testing.T) {
	o := MergeOptions{Repo: "o/r", PR: 7, Method: "merge",
		GH: func(context.Context, ...string) ([]byte, error) { return nil, errors.New("gh api: HTTP 500") }}
	res := Merge(context.Background(), o)
	if res.Status != MergeGHError {
		t.Fatalf("status = %s", res.Status)
	}
}
