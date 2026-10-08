package pr

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

type replyFake struct {
	typename     string // default PullRequestReviewThread
	resolved     bool
	cantReply    bool
	morePages    bool
	comments     []map[string]any
	repo         string // default o/r
	missingSHA   bool
	commitErr    error
	replyErr     error
	resolveErr   error
	replyLands   bool     // the reply lands even though replyErr is returned
	resolveLands bool     // the resolve lands even though resolveErr is returned
	stillOpen    bool     // resolve returns isResolved:false
	onReply      func()   // runs inside the reply mutation
	calls        []string // "read", "commit", "reply", "resolve"
	replyBody    string
	resolveCtxOK bool // the resolve call's context was live
}

func (f *replyFake) gh(ctx context.Context, args ...string) ([]byte, error) {
	joined := strings.Join(args, " ")
	switch {
	case args[0] == "api" && strings.HasPrefix(args[1], "repos/") && strings.Contains(args[1], "/commits/"):
		f.calls = append(f.calls, "commit")
		if f.commitErr != nil {
			return nil, f.commitErr
		}
		if f.missingSHA {
			return nil, errors.New("gh api: HTTP 422: No commit found for SHA")
		}
		return []byte("abc\n"), nil
	case strings.Contains(joined, "addPullRequestReviewThreadReply"):
		f.calls = append(f.calls, "reply")
		for _, a := range args {
			if strings.HasPrefix(a, "b=") {
				f.replyBody = strings.TrimPrefix(a, "b=")
			}
		}
		if f.onReply != nil {
			f.onReply()
		}
		if f.replyErr != nil {
			if f.replyLands {
				f.comments = append(f.comments, map[string]any{"viewerDidAuthor": true, "body": f.replyBody, "url": "https://x/c1"})
			}
			return nil, f.replyErr
		}
		return []byte(`{"data":{"addPullRequestReviewThreadReply":{"comment":{"id":"PRRC_1","url":"https://x/c1"}}}}`), nil
	case strings.Contains(joined, "resolveReviewThread"):
		f.calls = append(f.calls, "resolve")
		f.resolveCtxOK = ctx.Err() == nil
		if f.resolveErr != nil {
			if f.resolveLands {
				f.resolved = true
			}
			return nil, f.resolveErr
		}
		return json.Marshal(map[string]any{"data": map[string]any{"resolveReviewThread": map[string]any{"thread": map[string]any{"isResolved": !f.stillOpen}}}})
	case args[0] == "api" && args[1] == "graphql":
		f.calls = append(f.calls, "read")
		tn, repo := f.typename, f.repo
		if tn == "" {
			tn = "PullRequestReviewThread"
		}
		if repo == "" {
			repo = "o/r"
		}
		comments := f.comments
		if comments == nil {
			comments = []map[string]any{{"viewerDidAuthor": false, "body": "**Major** finding"}}
		}
		return json.Marshal(map[string]any{"data": map[string]any{"node": map[string]any{
			"__typename": tn, "isResolved": f.resolved, "viewerCanReply": !f.cantReply,
			"pullRequest": map[string]any{"number": 7, "repository": map[string]any{"nameWithOwner": repo}},
			"comments":    map[string]any{"pageInfo": map[string]any{"hasPreviousPage": f.morePages}, "nodes": comments},
		}}})
	}
	return nil, errors.New("unexpected gh call: " + joined)
}

const thread = "PRRT_kwDOabc123"

func fixed(f *replyFake) ReplyOptions {
	return ReplyOptions{Thread: thread, Fixed: "0123abc", Note: "guard the nil map", GH: f.gh}
}

func callsAre(t *testing.T, f *replyFake, want ...string) {
	t.Helper()
	if strings.Join(f.calls, ",") != strings.Join(want, ",") {
		t.Fatalf("gh calls %v, want %v", f.calls, want)
	}
}

func TestReplyFixedRepliesThenResolves(t *testing.T) {
	f := &replyFake{}
	r := Reply(context.Background(), fixed(f))
	if r.Status != ReplyDone || !r.Replied || !r.Resolved || r.Repo != "o/r" || r.PR != 7 || r.ReplyURL != "https://x/c1" {
		t.Fatalf("got %+v", r)
	}
	callsAre(t, f, "read", "commit", "reply", "resolve")
	if f.replyBody != "Fixed in 0123abc: guard the nil map" {
		t.Fatalf("posted %q", f.replyBody)
	}
}

func TestReplyKeepSkipsCommitCheck(t *testing.T) {
	f := &replyFake{}
	r := Reply(context.Background(), ReplyOptions{Thread: thread, Keep: "the caller already holds the lock", GH: f.gh})
	if r.Status != ReplyDone || f.replyBody != "Keeping as-is: the caller already holds the lock" {
		t.Fatalf("got %+v, posted %q", r, f.replyBody)
	}
	callsAre(t, f, "read", "reply", "resolve")
}

func TestReplyRerunDoesNotReplyTwice(t *testing.T) {
	ours := map[string]any{"viewerDidAuthor": true, "body": "Fixed in 0123abc: guard the nil map"}
	f := &replyFake{comments: []map[string]any{{"viewerDidAuthor": false, "body": "finding"}, ours}}
	r := Reply(context.Background(), fixed(f))
	if r.Status != ReplyDone || r.Replied || !r.Resolved {
		t.Fatalf("got %+v", r)
	}
	callsAre(t, f, "read", "resolve")
}

func TestReplyStoredBodyNormalizationStillCountsAsOurs(t *testing.T) {
	cases := []struct{ note, stored string }{
		{"guard the nil map", "Fixed in 0123abc: guard the nil map\n"},             // trailing newline
		{"first line\nsecond line", "Fixed in 0123abc: first line\r\nsecond line"}, // CRLF
	}
	for _, c := range cases {
		f := &replyFake{comments: []map[string]any{{"viewerDidAuthor": true, "body": c.stored}}}
		o := fixed(f)
		o.Note = c.note
		r := Reply(context.Background(), o)
		if r.Status != ReplyDone || r.Replied || !r.Resolved {
			t.Fatalf("stored %q: got %+v", c.stored, r)
		}
		callsAre(t, f, "read", "resolve")
	}
}
func TestReplyRerunDoesNotRecheckCommit(t *testing.T) {
	ours := map[string]any{"viewerDidAuthor": true, "body": "Fixed in 0123abc: guard the nil map"}
	f := &replyFake{comments: []map[string]any{ours}, missingSHA: true}
	r := Reply(context.Background(), fixed(f))
	if r.Status != ReplyDone || r.Replied || !r.Resolved {
		t.Fatalf("got %+v", r)
	}
	callsAre(t, f, "read", "resolve")
}

func TestReplyAlreadyDoneIsANoOp(t *testing.T) {
	ours := map[string]any{"viewerDidAuthor": true, "body": "Fixed in 0123abc: guard the nil map"}
	f := &replyFake{resolved: true, comments: []map[string]any{ours}}
	r := Reply(context.Background(), fixed(f))
	if r.Status != ReplyDone || r.Replied || r.Resolved {
		t.Fatalf("got %+v", r)
	}
	callsAre(t, f, "read")
}

func TestReplyResolvedWithoutReplyStillGetsTheReply(t *testing.T) {
	f := &replyFake{resolved: true}
	r := Reply(context.Background(), fixed(f))
	if r.Status != ReplyDone || !r.Replied || r.Resolved {
		t.Fatalf("got %+v", r)
	}
	callsAre(t, f, "read", "commit", "reply")
}

func TestReplySameTextBySomeoneElseIsNotOurs(t *testing.T) {
	theirs := map[string]any{"viewerDidAuthor": false, "body": "Fixed in 0123abc: guard the nil map"}
	f := &replyFake{comments: []map[string]any{theirs}}
	r := Reply(context.Background(), fixed(f))
	if r.Status != ReplyDone || !r.Replied {
		t.Fatalf("got %+v", r)
	}
	callsAre(t, f, "read", "commit", "reply", "resolve")
}

func TestReplyRefusalsPostNothing(t *testing.T) {
	cases := []struct {
		name string
		f    *replyFake
		repo string
		want string
	}{
		{"repo mismatch", &replyFake{repo: "other/repo"}, "o/r", "belongs to other/repo"},
		{"unknown commit", &replyFake{missingSHA: true}, "", "is not in o/r"},
		{"more than 100 comments", &replyFake{morePages: true}, "", "more than 100 comments"},
		{"cannot reply", &replyFake{cantReply: true}, "", "cannot reply"},
	}
	for _, c := range cases {
		o := fixed(c.f)
		o.Repo = c.repo
		r := Reply(context.Background(), o)
		if r.Status != ReplyRefused || !strings.Contains(r.Error, c.want) {
			t.Errorf("%s: got %+v, want refused with %q", c.name, r, c.want)
		}
		for _, call := range c.f.calls {
			if call == "reply" || call == "resolve" {
				t.Errorf("%s: posted (%v)", c.name, c.f.calls)
			}
		}
	}
}

func TestReplyRepoMatchIsCaseInsensitive(t *testing.T) {
	f := &replyFake{repo: "Owner/Repo"}
	o := fixed(f)
	o.Repo = "owner/repo"
	if r := Reply(context.Background(), o); r.Status != ReplyDone {
		t.Fatalf("got %+v", r)
	}
}

func TestReplyNotAThread(t *testing.T) {
	f := &replyFake{typename: "IssueComment"}
	r := Reply(context.Background(), fixed(f))
	if r.Status != ReplyRefused || !strings.Contains(r.Error, "not a pull request review thread") {
		t.Fatalf("got %+v", r)
	}
	callsAre(t, f, "read")
}

func TestReplyCommitLookupGHErrorIsNotMissingCommit(t *testing.T) {
	f := &replyFake{commitErr: errors.New("gh api: HTTP 502: upstream failure")}
	r := Reply(context.Background(), fixed(f))
	if r.Status != ReplyGHError || r.Replied || r.Resolved {
		t.Fatalf("got %+v", r)
	}
	callsAre(t, f, "read", "commit")
}

func TestReplyMutationErrorDoesNotResolve(t *testing.T) {
	f := &replyFake{replyErr: errors.New("gh api: HTTP 502")}
	r := Reply(context.Background(), fixed(f))
	if r.Status != ReplyGHError || r.Replied || !strings.Contains(r.Next, "again") {
		t.Fatalf("got %+v", r)
	}
	callsAre(t, f, "read", "commit", "reply", "read")
}

func TestReplyMutationErrorThatLandedIsReconciled(t *testing.T) {
	f := &replyFake{replyErr: errors.New("gh api: HTTP 502"), replyLands: true}
	r := Reply(context.Background(), fixed(f))
	if r.Status != ReplyDone || !r.Replied || !r.Resolved || r.ReplyURL != "https://x/c1" {
		t.Fatalf("got %+v", r)
	}
	callsAre(t, f, "read", "commit", "reply", "read", "resolve")
}

func TestReplyResolveFailureIsNotResolved(t *testing.T) {
	for _, f := range []*replyFake{{resolveErr: errors.New("gh api: HTTP 502")}, {stillOpen: true}} {
		r := Reply(context.Background(), fixed(f))
		if r.Status != ReplyNotResolved || !r.Replied || r.Resolved {
			t.Fatalf("got %+v", r)
		}
	}
}

func TestReplyResolveErrorThatLandedIsReconciled(t *testing.T) {
	f := &replyFake{resolveErr: errors.New("gh api: HTTP 502"), resolveLands: true}
	r := Reply(context.Background(), fixed(f))
	if r.Status != ReplyDone || !r.Replied || !r.Resolved {
		t.Fatalf("got %+v", r)
	}
	callsAre(t, f, "read", "commit", "reply", "resolve", "read")
}

func TestReplyInterruptAfterUnacknowledgedPostStillResolves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &replyFake{replyErr: context.Canceled, replyLands: true, onReply: cancel}
	r := Reply(ctx, fixed(f))
	if r.Status != ReplyDone || !r.Replied || !r.Resolved || !f.resolveCtxOK {
		t.Fatalf("got %+v, resolve ctx live %v", r, f.resolveCtxOK)
	}
	callsAre(t, f, "read", "commit", "reply", "read", "resolve")
}

func TestReplyInterruptAfterPostingStillResolves(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	f := &replyFake{onReply: cancel}
	o := fixed(f)
	o.GH = f.gh
	r := Reply(ctx, o)
	if r.Status != ReplyDone || !r.Resolved || !f.resolveCtxOK {
		t.Fatalf("got %+v, resolve ctx live %v", r, f.resolveCtxOK)
	}
}

func TestReplyInterruptBeforePostingPostsNothing(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	f := &replyFake{}
	if r := Reply(ctx, fixed(f)); r.Status != ReplyInterrupted {
		t.Fatalf("got %+v", r)
	}
	for _, call := range f.calls {
		if call == "reply" || call == "resolve" {
			t.Fatalf("posted after interrupt: %v", f.calls)
		}
	}
}

func TestValidateReply(t *testing.T) {
	ok := []ReplyOptions{
		{Thread: thread, Fixed: "0123abc", Note: "x"},
		{Thread: thread, Fixed: strings.Repeat("a", 40), Note: "x"},
		{Thread: thread, Keep: "reason"},
	}
	for _, o := range ok {
		if err := ValidateReply(o); err != nil {
			t.Errorf("%+v: %v", o, err)
		}
	}
	bad := []struct {
		o    ReplyOptions
		want string
	}{
		{ReplyOptions{Thread: "PRRC_comment", Keep: "r"}, "not a review-thread node id"},
		{ReplyOptions{Thread: "PRRT_a b", Keep: "r"}, "not a review-thread node id"},
		{ReplyOptions{Thread: thread}, "exactly one"},
		{ReplyOptions{Thread: thread, Fixed: "0123abc", Note: "x", Keep: "r"}, "exactly one"},
		{ReplyOptions{Thread: thread, Fixed: "012", Note: "x"}, "not a commit sha"},
		{ReplyOptions{Thread: thread, Fixed: "0123abz", Note: "x"}, "not a commit sha"},
		{ReplyOptions{Thread: thread, Fixed: "0123abc"}, "needs --note"},
		{ReplyOptions{Thread: thread, Fixed: "0123abc", Note: "  "}, "needs --note"},
		{ReplyOptions{Thread: thread, Keep: "r", Note: "x"}, "--note goes with --fixed"},
		{ReplyOptions{Thread: thread, Keep: " "}, "needs a reason"},
	}
	for _, c := range bad {
		if err := ValidateReply(c.o); err == nil || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%+v: got %v, want %q", c.o, err, c.want)
		}
	}
}
