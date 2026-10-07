package pr

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const (
	head  = "1111111111111111111111111111111111111111"
	older = "2222222222222222222222222222222222222222"
	base  = "3333333333333333333333333333333333333333"
)

type fake struct {
	state, summary string
	draft          bool
	threads        string // raw GraphQL response; default: none
	pages          [][]map[string]any
	failFirst      int // calls (of all kinds) that fail before success
	calls          atomic.Int32
}

func covered(sha string) string {
	return `<!-- walkthrough_start -->` + "\n" + `"coveredCommitId":"` + sha + `"` + "\nreviewing files that changed from the base of the PR and between " + base + " and " + sha
}

func bot(body string) map[string]any {
	return map[string]any{"user": map[string]any{"login": "coderabbitai[bot]"}, "body": body}
}

func (f *fake) gh(_ context.Context, args ...string) ([]byte, error) {
	if int(f.calls.Add(1)) <= f.failFirst {
		return nil, errors.New("gh api: HTTP 502")
	}
	switch {
	case args[0] == "pr" && args[1] == "view":
		st := f.state
		if st == "" {
			st = "OPEN"
		}
		return json.Marshal(map[string]any{"headRefOid": strings.ToUpper(head), "baseRefName": "main", "state": st, "isDraft": f.draft})
	case args[0] == "api" && args[1] == "graphql":
		if f.threads != "" {
			return []byte(f.threads), nil
		}
		return []byte(`{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[]}}}}}`), nil
	case args[0] == "api":
		pages := f.pages
		if pages == nil {
			pages = [][]map[string]any{{map[string]any{"user": map[string]any{"login": "someone"}, "body": "lgtm"}, bot(f.summary)}}
		}
		return json.Marshal(pages)
	}
	return nil, errors.New("unexpected gh call: " + strings.Join(args, " "))
}

func run(f *fake, once bool) Result {
	return Wait(context.Background(), Options{Repo: "o/r", PR: 7, Once: once, Timeout: 300 * time.Millisecond, Interval: 5 * time.Millisecond, GH: f.gh})
}

func TestReviewedHeadClean(t *testing.T) {
	r := run(&fake{summary: covered(head) + "\nNo actionable comments were generated in the recent review."}, true)
	if r.Status != StatusClean || r.Head != head || r.Reviewed != head || len(r.OpenThreads) != 0 || r.ThreadsComplete == nil || !*r.ThreadsComplete {
		t.Fatalf("got %+v", r)
	}
}

const threeThreads = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
 {"id":"PRRT_open","isResolved":false,"comments":{"nodes":[{"author":{"login":"coderabbitai"},"path":"a.go","line":12,"originalLine":10,"url":"https://x/1","body":"_⚠️ Potential issue_ | _🟠 Major_\n\n<details>\n**Nil map write in Save.**\nmore"}]}},
 {"id":"PRRT_outdated","isResolved":false,"comments":{"nodes":[{"author":{"login":"coderabbitai"},"path":"b.go","line":null,"originalLine":40,"url":"https://x/2","body":"Old point"}]}},
 {"id":"PRRT_done","isResolved":true,"comments":{"nodes":[{"author":{"login":"coderabbitai"},"path":"c.go","line":3,"url":"https://x/3","body":"fixed"}]}},
 {"id":"PRRT_human","isResolved":false,"comments":{"nodes":[{"author":{"login":"teammate"},"path":"d.go","line":5,"url":"https://x/4","body":"question"}]}}
]}}}}}`

func TestReviewedHeadOpenThreads(t *testing.T) {
	r := run(&fake{summary: covered(head), threads: threeThreads}, true)
	if r.Status != StatusOpenThreads || len(r.OpenThreads) != 2 {
		t.Fatalf("got %+v", r)
	}
	a, b := r.OpenThreads[0], r.OpenThreads[1]
	if a.ID != "PRRT_open" || a.Path != "a.go" || a.Line != 12 || a.URL != "https://x/1" {
		t.Errorf("thread a = %+v", a)
	}
	if a.Excerpt != "_⚠️ Potential issue_ | _🟠 Major_ **Nil map write in Save.**" {
		t.Errorf("excerpt = %q", a.Excerpt)
	}
	if b.ID != "PRRT_outdated" || b.Line != 40 {
		t.Errorf("outdated thread should keep its original line: %+v", b)
	}
	if r.Next == "" {
		t.Error("next is empty")
	}
}

func TestTruncatedThreadListIsNeverClean(t *testing.T) {
	threads := `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":true},"nodes":[]}}}}}`
	r := run(&fake{summary: covered(head), threads: threads}, true)
	if r.Status != StatusOpenThreads || r.ThreadsComplete == nil || *r.ThreadsComplete {
		t.Fatalf("got %+v", r)
	}
}

func TestOlderRoundIsNotTheHead(t *testing.T) {
	r := run(&fake{summary: covered(older)}, true)
	if r.Status != StatusWaiting || r.Reviewed != older {
		t.Fatalf("got %+v", r)
	}
}

func TestInProgressWaits(t *testing.T) {
	for _, s := range []string{
		covered(head) + "\nCurrently processing new changes in this PR.",
		"Review in progress by coderabbit.ai\nReview skipped earlier: Rate limit exceeded",
	} {
		if r := run(&fake{summary: s}, true); r.Status != StatusWaiting {
			t.Errorf("summary %q: got %+v", s, r)
		}
	}
}

func TestReviewedHeadOverridesStaleLimitAndSkipText(t *testing.T) {
	summary := "Rate limit exceeded\nReview skipped earlier\n" + covered(head)
	if r := run(&fake{summary: summary}, true); r.Status != StatusClean {
		t.Fatalf("got %+v", r)
	}
}

func TestSkippedRateLimitedClosed(t *testing.T) {
	cases := []struct {
		f    *fake
		want Status
	}{
		{&fake{summary: "## Review skipped\nAuto reviews are disabled on base/target branches other than the default branch."}, StatusSkipped},
		{&fake{draft: true}, StatusSkipped},
		{&fake{summary: "> [!WARNING]\n> ## Rate limit exceeded\n> wait 12 minutes"}, StatusRateLimited},
		{&fake{state: "MERGED"}, StatusClosed},
		{&fake{state: "MERGED", summary: "Rate limit exceeded"}, StatusClosed},
		{&fake{state: "CLOSED", summary: "Review skipped", draft: true}, StatusClosed},
		{&fake{state: "MERGED", summary: covered(head), threads: threeThreads}, StatusOpenThreads}, // merged, still reviewed
	}
	for i, c := range cases {
		if r := run(c.f, true); r.Status != c.want {
			t.Errorf("case %d: got %s (%+v), want %s", i, r.Status, r, c.want)
		}
	}
}

func TestSummaryOnALaterPage(t *testing.T) {
	var page1 []map[string]any
	for i := 0; i < 100; i++ {
		page1 = append(page1, map[string]any{"user": map[string]any{"login": "someone"}, "body": "comment"})
	}
	page1[3] = bot(covered(older)) // an older summary early on
	f := &fake{pages: [][]map[string]any{page1, {bot(covered(head))}}}
	var sawPaginatedComments, sawBoundedThreads bool
	gh := func(ctx context.Context, args ...string) ([]byte, error) {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "/issues/7/comments?per_page=100") {
			sawPaginatedComments = strings.Contains(joined, "--paginate") && strings.Contains(joined, "--slurp")
		}
		if len(args) > 1 && args[0] == "api" && args[1] == "graphql" {
			sawBoundedThreads = strings.Contains(joined, "reviewThreads(first:100)") && strings.Contains(joined, "pageInfo{hasNextPage}")
		}
		return f.gh(ctx, args...)
	}
	r := Wait(context.Background(), Options{Repo: "o/r", PR: 7, Once: true, GH: gh})
	if r.Status != StatusClean || !sawPaginatedComments || !sawBoundedThreads {
		t.Fatalf("got %+v", r)
	}
}

func TestOnlyCodeRabbitBotCommentCanBeTheSummary(t *testing.T) {
	f := &fake{pages: [][]map[string]any{{
		{"user": map[string]any{"login": "coderabbit-helper"}, "body": covered(head)},
		bot(covered(older)),
	}}}
	if r := run(f, true); r.Status != StatusWaiting || r.Reviewed != older {
		t.Fatalf("got %+v", r)
	}
}

func TestOnlyCodeRabbitCanOpenACodeRabbitThread(t *testing.T) {
	threads := `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
 {"id":"PRRT_impostor","isResolved":false,"comments":{"nodes":[{"author":{"login":"coderabbit-helper"},"path":"a.go","line":12,"url":"https://x/1","body":"not the bot"}]}}
]}}}}}`
	r := run(&fake{summary: covered(head), threads: threads}, true)
	if r.Status != StatusClean || len(r.OpenThreads) != 0 {
		t.Fatalf("got %+v", r)
	}
}

func TestFirstGHErrorFailsAtOnce(t *testing.T) {
	r := run(&fake{failFirst: 1}, false)
	if r.Status != StatusGHError || r.Attempts != 1 || !strings.Contains(r.Error, "502") {
		t.Fatalf("got %+v", r)
	}
}

func TestLaterGHErrorIsRetried(t *testing.T) {
	f := &fake{summary: covered(older)}
	r := Wait(context.Background(), Options{Repo: "o/r", PR: 7, Timeout: time.Second, Interval: time.Millisecond, GH: func(ctx context.Context, args ...string) ([]byte, error) {
		n := f.calls.Load()
		if n >= 3 && n < 6 { // the whole second check fails
			f.calls.Add(1)
			return nil, errors.New("gh api: HTTP 502")
		}
		if n >= 6 {
			f.summary = covered(head)
		}
		return f.gh(ctx, args...)
	}})
	if r.Status != StatusClean || r.Attempts < 3 || r.Error != "" {
		t.Fatalf("got %+v", r)
	}
}

func TestTimeout(t *testing.T) {
	r := Wait(context.Background(), Options{Repo: "o/r", PR: 7, Timeout: 40 * time.Millisecond, Interval: 5 * time.Millisecond, GH: (&fake{summary: covered(older)}).gh})
	if r.Status != StatusTimeout || r.Attempts < 2 || r.Next == "" {
		t.Fatalf("got %+v", r)
	}
}

func TestTimeoutCancelsInflightGH(t *testing.T) {
	gh := func(ctx context.Context, _ ...string) ([]byte, error) {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	start := time.Now()
	r := Wait(context.Background(), Options{Repo: "o/r", PR: 7, Timeout: 30 * time.Millisecond, Interval: time.Millisecond, GH: gh})
	if r.Status != StatusTimeout || time.Since(start) > 250*time.Millisecond {
		t.Fatalf("got %+v after %s", r, time.Since(start))
	}
}

func TestLateCleanDoesNotBeatDeadline(t *testing.T) {
	f := &fake{summary: covered(head)}
	gh := func(ctx context.Context, args ...string) ([]byte, error) {
		if args[0] == "pr" {
			time.Sleep(50 * time.Millisecond) // deliberately ignores cancellation
		}
		return f.gh(ctx, args...)
	}
	r := Wait(context.Background(), Options{Repo: "o/r", PR: 7, Timeout: 20 * time.Millisecond, Interval: time.Millisecond, GH: gh})
	if r.Status != StatusTimeout {
		t.Fatalf("got %+v", r)
	}
}

func TestInterrupted(t *testing.T) {
	for _, duringCall := range []bool{false, true} {
		ctx, cancel := context.WithCancel(context.Background())
		gh := (&fake{}).gh
		if duringCall {
			gh = func(callCtx context.Context, _ ...string) ([]byte, error) {
				<-callCtx.Done()
				return nil, callCtx.Err()
			}
			time.AfterFunc(20*time.Millisecond, cancel)
		} else {
			cancel()
		}
		r := Wait(ctx, Options{Repo: "o/r", PR: 7, Timeout: time.Second, Interval: time.Millisecond, GH: gh})
		if r.Status != StatusInterrupted {
			t.Errorf("duringCall=%v: got %+v", duringCall, r)
		}
		cancel()
	}
}

func TestGraphQLErrorsAreErrors(t *testing.T) {
	r := run(&fake{summary: covered(head), threads: `{"errors":[{"message":"Could not resolve to a PullRequest"}]}`}, true)
	if r.Status != StatusGHError || !strings.Contains(r.Error, "Could not resolve") {
		t.Fatalf("got %+v", r)
	}
}

func TestMissingGraphQLThreadDataIsAnError(t *testing.T) {
	for _, response := range []string{
		`{}`,
		`{"data":{"repository":null}}`,
		`{"data":{"repository":{"pullRequest":null}}}`,
		`{"data":{"repository":{"pullRequest":{"reviewThreads":{}}}}}`,
	} {
		r := run(&fake{summary: covered(head), threads: response}, true)
		if r.Status != StatusGHError || !strings.Contains(r.Error, "reviewThreads") {
			t.Errorf("response %s: got %+v", response, r)
		}
	}
}
