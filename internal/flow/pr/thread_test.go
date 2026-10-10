package pr

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"unicode/utf8"
)

const bodyThreads = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
 {"id":"PRRT_open","isResolved":false,"comments":{"totalCount":3,"nodes":[{"author":{"login":"coderabbitai"},"path":"a.go","line":12,"url":"https://x/1","body":"**Title.**\r\nred \u001b[31mtext\u001b[0m and \u202eevil"}]}}
]}}}}}`

func TestWaitBodiesAddsSanitizedBodyAndReplies(t *testing.T) {
	r := Wait(context.Background(), Options{Repo: "o/r", PR: 7, Once: true, Bodies: true, GH: (&fake{summary: covered(head), threads: bodyThreads}).gh})
	if r.Status != StatusOpenThreads || len(r.OpenThreads) != 1 {
		t.Fatalf("got %+v", r)
	}
	th := r.OpenThreads[0]
	want := "**Title.**\nred \\x1b[31mtext\\x1b[0m and \\u202eevil"
	if th.Body == nil || *th.Body != want || th.BodyTruncated || th.Replies == nil || *th.Replies != 2 {
		t.Errorf("body %v replies %v truncated %v; want %q, 2", th.Body, th.Replies, th.BodyTruncated, want)
	}
}

func TestWaitWithoutBodiesKeepsTheJSONShape(t *testing.T) {
	r := Wait(context.Background(), Options{Repo: "o/r", PR: 7, Once: true, GH: (&fake{summary: covered(head), threads: bodyThreads}).gh})
	b, _ := json.Marshal(r.OpenThreads[0])
	if string(b) != `{"id":"PRRT_open","path":"a.go","line":12,"url":"https://x/1","excerpt":"**Title.** red \u001b[31mtext\u001b[0m and `+"\u202e"+`evil"}` {
		t.Errorf("thread JSON changed without --bodies: %s", b)
	}
}

func TestWaitBodiesIncludesEmptyBody(t *testing.T) {
	threads := strings.Replace(bodyThreads, `"body":"**Title.**\r\nred \u001b[31mtext\u001b[0m and \u202eevil"`, `"body":""`, 1)
	r := Wait(context.Background(), Options{Repo: "o/r", PR: 7, Once: true, Bodies: true, GH: (&fake{summary: covered(head), threads: threads}).gh})
	if r.Status != StatusOpenThreads || len(r.OpenThreads) != 1 {
		t.Fatalf("got %+v", r)
	}
	b, _ := json.Marshal(r.OpenThreads[0])
	if !strings.Contains(string(b), `"body":""`) {
		t.Errorf("--bodies omitted an empty body: %s", b)
	}
}

func TestSafeBodyCaps(t *testing.T) {
	s, cut := SafeBody(strings.Repeat("x", MaxBodyRunes+50))
	if !cut || utf8.RuneCountInString(s) > MaxBodyRunes || !strings.Contains(s, "[truncated: ") {
		t.Errorf("cut %v, %d runes", cut, utf8.RuneCountInString(s))
	}
}

// threadGH answers the review-thread query with resp, recording the call.
func threadGH(resp string, err error, calls *[]string) GH {
	return func(_ context.Context, args ...string) ([]byte, error) {
		*calls = append(*calls, strings.Join(args, " "))
		if err != nil {
			return nil, err
		}
		return []byte(resp), nil
	}
}

const threadResp = `{"data":{"node":{"__typename":"PullRequestReviewThread","isResolved":false,"isOutdated":true,"path":"a.go","line":null,"originalLine":9,
 "pullRequest":{"number":7,"repository":{"nameWithOwner":"Acme/Repo"}},
 "comments":{"pageInfo":{"hasNextPage":false},"nodes":[
  {"author":{"login":"coderabbitai"},"createdAt":"2026-10-01T10:00:00Z","url":"https://x/c1","body":"Finding\u001b]0;pwned\u0007 body"},
  {"author":null,"createdAt":"2026-10-01T11:00:00Z","url":"https://x/c2","body":"Fixed in abc: done"}]}}}}`

func TestReadThread(t *testing.T) {
	var calls []string
	r := ReadThread(context.Background(), ThreadOptions{Thread: "PRRT_x", Repo: "acme/repo", GH: threadGH(threadResp, nil, &calls)})
	if r.Status != ThreadOK || r.Repo != "Acme/Repo" || r.PR != 7 || r.Path != "a.go" || r.Line != 9 || !r.Outdated || r.Resolved || !r.Complete {
		t.Fatalf("got %+v", r)
	}
	if len(r.Comments) != 2 || r.Comments[0].Body != `Finding\x1b]0;pwned\x07 body` || r.Comments[1].Author != "ghost" {
		t.Errorf("comments %+v", r.Comments)
	}
	if len(calls) != 1 || !strings.HasPrefix(calls[0], "api graphql -F t=PRRT_x") || strings.Contains(calls[0], "mutation") {
		t.Errorf("pr thread must make one read-only query: %v", calls)
	}
}

func TestReadThreadRefusesAndFails(t *testing.T) {
	var calls []string
	cases := []struct {
		name string
		o    ThreadOptions
		want ThreadStatus
		msg  string
	}{
		{"bad id", ThreadOptions{Thread: "123"}, ThreadRefused, "not a review-thread node id"},
		{"other repo", ThreadOptions{Thread: "PRRT_x", Repo: "o/other", GH: threadGH(threadResp, nil, &calls)}, ThreadRefused, "belongs to Acme/Repo"},
		{"not a thread", ThreadOptions{Thread: "PRRT_x", GH: threadGH(`{"data":{"node":{"__typename":"Issue"}}}`, nil, &calls)}, ThreadRefused, "is a Issue"},
		{"no node", ThreadOptions{Thread: "PRRT_x", GH: threadGH(`{"data":{"node":null}}`, nil, &calls)}, ThreadRefused, "no node"},
		{"gh error", ThreadOptions{Thread: "PRRT_x", GH: threadGH("", errors.New("gh api: HTTP 502"), &calls)}, ThreadGHError, "502"},
		{"graphql error", ThreadOptions{Thread: "PRRT_x", GH: threadGH(`{"errors":[{"message":"boom"}]}`, nil, &calls)}, ThreadGHError, "boom"},
		{"incomplete", ThreadOptions{Thread: "PRRT_x", GH: threadGH(`{"data":{"node":{"__typename":"PullRequestReviewThread"}}}`, nil, &calls)}, ThreadGHError, "incomplete"},
	}
	for _, c := range cases {
		r := ReadThread(context.Background(), c.o)
		if r.Status != c.want || !strings.Contains(r.Error, c.msg) || len(r.Comments) != 0 {
			t.Errorf("%s: got %+v, want %s %q", c.name, r, c.want, c.msg)
		}
	}
}

func TestReadThreadInterrupted(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	gh := func(ctx context.Context, _ ...string) ([]byte, error) {
		cancel()
		<-ctx.Done()
		return nil, ctx.Err()
	}
	r := ReadThread(ctx, ThreadOptions{Thread: "PRRT_x", GH: gh})
	if r.Status != ThreadInterrupted {
		t.Fatalf("got %+v", r)
	}
}

func TestReadThreadMoreThanAPage(t *testing.T) {
	var calls []string
	resp := strings.Replace(threadResp, `"hasNextPage":false`, `"hasNextPage":true`, 1)
	if r := ReadThread(context.Background(), ThreadOptions{Thread: "PRRT_x", GH: threadGH(resp, nil, &calls)}); r.Status != ThreadOK || r.Complete {
		t.Fatalf("a thread with more comments must say comments_complete false: %+v", r)
	}
}
