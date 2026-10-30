package pr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
)

// openStackFake wraps openFake with the layer below (PR #5, head
// "development") and GitHub's stacks. stack holds the stack's PRs bottom
// first; nil = #5 is in no stack. The PR under test is #7.
type openStackFake struct {
	*openFake
	parents   int   // open PRs whose head is development; default 1
	noParent  bool  // no open PR has development as its head
	stack     []int // the stack's PRs, bottom first
	stackNum  int
	linkErr   error
	noSchema  bool
	skipLink  bool // the create/add call succeeds but the PR is not linked
	linkCalls [][]string
}

func (g *openStackFake) gh(ctx context.Context, args ...string) ([]byte, error) {
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
	case args[0] == "pr" && args[1] == "list" && len(args) > 5 && args[5] == "development":
		var out []map[string]any
		n := g.parents
		if n == 0 {
			n = 1
		}
		if g.noParent {
			n = 0
		}
		for i := range n {
			out = append(out, map[string]any{"number": 5 + 100*i, "url": "u", "baseRefName": "main", "headRefName": "development",
				"isCrossRepository": false, "headRepositoryOwner": map[string]any{"login": "o"}})
		}
		return json.Marshal(out)
	case args[0] == "api" && args[1] == "graphql" && strings.Contains(query, "stackEntry"):
		if g.noSchema {
			return nil, errors.New("gh api: GraphQL: Field 'stackEntry' doesn't exist on type 'PullRequest'")
		}
		pos := 0
		for i, n := range g.stack {
			if n == pr {
				pos = i + 1
			}
		}
		if pos == 0 {
			return []byte(`{"data":{"repository":{"pullRequest":{"stackEntry":null,"stack":null}}}}`), nil
		}
		var nodes []map[string]any
		for i, n := range g.stack {
			nodes = append(nodes, map[string]any{"position": i + 1, "pullRequest": map[string]any{"number": n, "state": "OPEN"}})
		}
		return json.Marshal(map[string]any{"data": map[string]any{"repository": map[string]any{"pullRequest": map[string]any{
			"stackEntry": map[string]any{"position": pos},
			"stack": map[string]any{"number": g.stackNum, "baseRefName": "main", "size": len(g.stack),
				"entries": map[string]any{"pageInfo": map[string]any{"hasNextPage": false}, "nodes": nodes}}}}}})
	case args[0] == "api" && args[1] == "-X" && args[2] == "POST" && strings.HasPrefix(args[3], "repos/o/r/stacks"):
		g.linkCalls = append(g.linkCalls, args)
		if g.linkErr != nil {
			return nil, g.linkErr
		}
		var prs []int
		for i, a := range args {
			if a == "-F" && i+1 < len(args) && strings.HasPrefix(args[i+1], "pull_requests[]=") {
				var n int
				fmt.Sscanf(args[i+1], "pull_requests[]=%d", &n)
				prs = append(prs, n)
			}
		}
		if args[3] == "repos/o/r/stacks" {
			g.stackNum = 9
			if !g.skipLink {
				g.stack = prs
			}
		} else if !g.skipLink {
			g.stack = append(g.stack, prs...)
		}
		return json.Marshal(map[string]any{"number": g.stackNum})
	}
	return g.openFake.gh(ctx, args...)
}

func openStack(t *testing.T, mod func(*openStackFake)) (*openStackFake, *openFixture, OpenResult) {
	t.Helper()
	f := newOpenFixture(t)
	g := &openStackFake{openFake: &openFake{f: f}}
	if mod != nil {
		mod(g)
	}
	o := f.opts(g.openFake)
	o.GH, o.Base, o.Stack = g.gh, "development", true
	return g, f, Open(context.Background(), o)
}

func TestOpenStackCreatesAStackWithTheLayerBelow(t *testing.T) {
	g, _, res := openStack(t, nil)
	if res.Status != OpenCreated || res.Stack == nil {
		t.Fatalf("status = %s stack = %+v (%v %s)", res.Status, res.Stack, res.Reasons, res.Error)
	}
	if *res.Stack != (OpenStack{Parent: 5, Number: 9, Position: 2, Action: "created"}) {
		t.Fatalf("stack = %+v", res.Stack)
	}
	call := strings.Join(g.linkCalls[0], " ")
	for _, want := range []string{"repos/o/r/stacks", "pull_requests[]=5", "pull_requests[]=7", "X-GitHub-Api-Version: " + StackAPIVersion} {
		if !strings.Contains(call, want) {
			t.Errorf("create call %q lacks %q", call, want)
		}
	}
	if g.created["base"] != "development" {
		t.Fatalf("PR base = %v", g.created["base"])
	}
}

func TestOpenStackAddsOnTopOfAnExistingStack(t *testing.T) {
	g, _, res := openStack(t, func(g *openStackFake) { g.stack, g.stackNum = []int{4, 5}, 3 })
	if res.Status != OpenCreated || *res.Stack != (OpenStack{Parent: 5, Number: 3, Position: 3, Action: "added"}) {
		t.Fatalf("status = %s stack = %+v (%s)", res.Status, res.Stack, res.Error)
	}
	if call := strings.Join(g.linkCalls[0], " "); !strings.Contains(call, "repos/o/r/stacks/3/add") || strings.Contains(call, "pull_requests[]=5") {
		t.Fatalf("add call = %q", call)
	}
}

func TestOpenStackRerunIsAlready(t *testing.T) {
	g, _, res := openStack(t, func(g *openStackFake) {
		g.stack, g.stackNum = []int{5, 7}, 3
		g.existing = existingPR7("development")
	})
	if res.Status != OpenExisting || res.Stack == nil || res.Stack.Action != "already" || len(g.linkCalls) != 0 {
		t.Fatalf("status = %s stack = %+v calls = %v (%v %s)", res.Status, res.Stack, g.linkCalls, res.Reasons, res.Error)
	}
}

func TestOpenStackRefusalsPushNothing(t *testing.T) {
	cases := map[string]struct {
		mod  func(*openStackFake)
		base string
		want string
	}{
		"trunk base":    {nil, "main", "--stack needs --base"},
		"no parent":     {func(g *openStackFake) { g.noParent = true }, "", "found 0"},
		"two parents":   {func(g *openStackFake) { g.parents = 2 }, "", "found 2"},
		"parent capped": {func(g *openStackFake) { g.stack, g.stackNum = []int{5, 6}, 3 }, "", "#5 already has #6 above it"},
		"no schema":     {func(g *openStackFake) { g.noSchema = true }, "", "no stacked pull requests"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			f := newOpenFixture(t)
			g := &openStackFake{openFake: &openFake{f: f}}
			if c.mod != nil {
				c.mod(g)
			}
			o := f.opts(g.openFake)
			o.GH, o.Base, o.Stack = g.gh, "development", true
			if c.base != "" {
				o.Base = c.base
			}
			res := Open(context.Background(), o)
			if res.Status != OpenRefused || !strings.Contains(strings.Join(res.Reasons, "; "), c.want) {
				t.Fatalf("status = %s reasons = %v, want %q", res.Status, res.Reasons, c.want)
			}
			if len(f.pushes()) != 0 || len(g.linkCalls) != 0 {
				t.Fatal("a refused --stack must push and link nothing")
			}
		})
	}
}

func TestOpenStackLinkFailureIsUnverified(t *testing.T) {
	_, _, res := openStack(t, func(g *openStackFake) { g.linkErr = errors.New("gh api: HTTP 422") })
	if res.Status != OpenUnverified || res.Number != 7 || !strings.Contains(res.Error, "HTTP 422") {
		t.Fatalf("status = %s number = %d error = %s", res.Status, res.Number, res.Error)
	}
	_, _, res = openStack(t, func(g *openStackFake) { g.skipLink = true })
	if res.Status != OpenUnverified || !strings.Contains(res.Error, "read-back") {
		t.Fatalf("status = %s error = %s", res.Status, res.Error)
	}
}

func TestOpenStackPRInAnotherStackIsUnverified(t *testing.T) {
	_, _, res := openStack(t, func(g *openStackFake) {
		g.stack, g.stackNum = []int{8, 7}, 4
		g.existing = existingPR7("development")
	})
	if res.Status != OpenUnverified || !strings.Contains(res.Error, "not right above #5") {
		t.Fatalf("status = %s error = %s", res.Status, res.Error)
	}
}
