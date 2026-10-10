package cli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/TysonLabs/agentctl/internal/flow/agent"
	"github.com/TysonLabs/agentctl/internal/flow/coderabbit"
	"github.com/TysonLabs/agentctl/internal/flow/lessons"
	"github.com/TysonLabs/agentctl/internal/flow/pr"
	"github.com/TysonLabs/agentctl/internal/flow/ship"
	"github.com/TysonLabs/agentctl/internal/flow/worktree"
)

// Every command that prints a JSON result takes --format; an unknown value
// is a usage error (exit 1) before anything runs.
func TestFormatFlagOnEverySubcommand(t *testing.T) {
	t.Setenv("AGENTFLOW_GH", "/nonexistent/gh")
	t.Setenv("AGENTFLOW_LESSONS_DIR", t.TempDir())
	cmds := [][]string{
		{"codex", "--prompt", "p"},
		{"claude", "--prompt", "p"},
		{"coderabbit"},
		{"ship", "verify", "x.prod", "--sha", "abc1234"},
		{"ship", "announce", "x.prod"},
		{"pr", "wait", "7"},
		{"pr", "reply", "PRRT_x", "--keep", "r"},
		{"pr", "merge", "7"},
		{"pr", "thread", "PRRT_x"},
		{"worktree", "done", "b"},
		{"worktree", "sweep"},
		{"lessons", "bump", "--used", "l1"},
		{"lessons", "retire"},
		{"lessons", "stats"},
	}
	for _, c := range cmds {
		for _, bad := range [][]string{{"--format", "yaml"}, {"--format=TEXT"}, {"--format", ""}} {
			args := append(append([]string{}, c...), bad...)
			code, out, errOut := run(t, args...)
			if code != 1 || out != "" || !strings.Contains(errOut, "must be json or text") {
				t.Errorf("%v: exit %d stdout %q stderr %q; want 1 and a --format usage error", args, code, out, errOut)
			}
		}
	}
	// brief prints Markdown, not a JSON result: it has no --format.
	if code, _, errOut := run(t, "lessons", "brief", "--topics", "x", "--format", "text"); code != 1 || !strings.Contains(errOut, "not defined") {
		t.Errorf("lessons brief --format: exit %d %q", code, errOut)
	}
}

// Without --format, and with --format json, the output is the same bytes.
func TestJSONUnchangedWithoutFormat(t *testing.T) {
	dir := lessonsFixture(t)
	t.Setenv("AGENTFLOW_LESSONS_DIR", dir)
	for _, args := range [][]string{
		{"lessons", "stats"},
		{"lessons", "retire", "--today", "2027-01-01"},
	} {
		c1, plain, _ := run(t, args...)
		c2, asJSON, _ := run(t, append(args, "--format", "json")...)
		c3, text, _ := run(t, append(args, "--format", "text")...)
		if plain != asJSON || c1 != c2 || c1 != c3 || !json.Valid([]byte(plain)) {
			t.Errorf("%v: --format json changed the output or exit (%d %d %d)\n%s\n%s", args, c1, c2, c3, plain, asJSON)
		}
		if json.Valid([]byte(text)) || !strings.HasPrefix(text, strings.Join(args[:2], " ")+": ok (exit 0)\n") {
			t.Errorf("%v --format text: %q", args, text)
		}
	}
	want := "{\n  \"lessons\": 1,\n  \"inbox\": 0,\n  \"archived\": 0,\n  \"used_zero\": 0,\n  \"misled_nonzero\": 0,\n" +
		"  \"retire_candidates\": 0,\n  \"retire_kept_linked\": 0,\n  \"false_positives\": 0\n}\n"
	if _, out, _ := run(t, "lessons", "stats"); out != want {
		t.Errorf("lessons stats JSON changed:\n%s", out)
	}
	code, out, _ := run(t, "lessons", "bump", "--used", "l1", "--format", "text")
	if code != 0 || out != "lessons bump: ok (exit 0)\nchanges: 1\n- l1 Used 2 -> 3 (Concurrency.md)\n" {
		t.Errorf("bump text: exit %d %q", code, out)
	}
}

// --format text still saves the JSON result under --out.
func TestTextFormatStillWritesResultJSON(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(t.TempDir(), "claude")
	script := "#!/bin/sh\ncat > /dev/null\n" +
		`echo '{"type":"system","subtype":"init","session_id":"s1"}'` + "\n" +
		`echo '{"type":"result","subtype":"success","is_error":false,"result":"No findings.","session_id":"s1","total_cost_usd":0.1}'` + "\n"
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_CLAUDE", fake)
	out := t.TempDir()
	code, stdout, errOut := run(t, "claude", "--dir", dir, "--prompt", "p", "--out", out, "--format", "text")
	if code != 0 || !strings.HasPrefix(stdout, "claude: ok (exit 0)\nfinal: "+filepath.Join(out, "final.md")+"\nclaude_exit: 0\n") {
		t.Fatalf("exit %d\nstdout %s\nstderr %s", code, stdout, errOut)
	}
	if !strings.Contains(stdout, "\njson: "+filepath.Join(out, "result.json")+"\n") || !strings.Contains(stdout, "\ncost_usd: 0.1\n") {
		t.Errorf("text misses json path or cost:\n%s", stdout)
	}
	saved, err := os.ReadFile(filepath.Join(out, "result.json"))
	var res struct{ Status string }
	if err != nil || json.Unmarshal(saved, &res) != nil || res.Status != "ok" {
		t.Errorf("result.json not saved in text mode: %v %s", err, saved)
	}
}

// fakeGH installs a gh stub that answers pr view, the issue comments, the
// open-threads query and the single-thread query from fixture files.
func fakeGH(t *testing.T, threads, thread string) {
	t.Helper()
	d := t.TempDir()
	const sha = "1111111111111111111111111111111111111111"
	files := map[string]string{
		"view.json":     `{"headRefOid":"` + sha + `","baseRefName":"main","state":"OPEN","isDraft":false}`,
		"comments.json": `[[{"user":{"login":"coderabbitai[bot]"},"body":"<!-- walkthrough_start -->\n\"coveredCommitId\":\"` + sha + `\""}]]`,
		"threads.json":  threads,
		"thread.json":   thread,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(d, name), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	script := "#!/bin/sh\ncd '" + d + "'\n" + `case "$1 $2" in
"pr view") cat view.json ;;
"api graphql") case "$*" in *reviewThreads*) cat threads.json ;; *) cat thread.json ;; esac ;;
"api "*) cat comments.json ;;
*) echo "unexpected gh $*" >&2; exit 9 ;;
esac
`
	gh := filepath.Join(d, "gh")
	if err := os.WriteFile(gh, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_GH", gh)
}

const cliThreads = `{"data":{"repository":{"pullRequest":{"reviewThreads":{"pageInfo":{"hasNextPage":false},"nodes":[
 {"id":"PRRT_a","isResolved":false,"comments":{"totalCount":2,"nodes":[{"author":{"login":"coderabbitai"},"path":"a.go","line":3,"url":"https://x/1","body":"**Nil map.**\n\nUse make(\u001b[31m).\n"}]}}]}}}}}`

const cliThread = `{"data":{"node":{"__typename":"PullRequestReviewThread","isResolved":false,"isOutdated":false,"path":"a.go","line":3,"originalLine":3,
 "pullRequest":{"number":7,"repository":{"nameWithOwner":"o/r"}},
 "comments":{"pageInfo":{"hasNextPage":false},"nodes":[{"author":{"login":"coderabbitai"},"createdAt":"2026-10-01T10:00:00Z","url":"https://x/1","body":"**Nil map.**\r\nUse make.\u202e"}]}}}}`

func TestPRWaitBodiesText(t *testing.T) {
	fakeGH(t, cliThreads, cliThread)
	code, out, errOut := run(t, "pr", "wait", "7", "--repo", "o/r", "--once", "--bodies", "--format", "text")
	if code != 10 || !strings.HasPrefix(out, "pr wait: open_threads (exit 10)\nrepo: o/r\npr: 7\n") {
		t.Fatalf("exit %d\n%s\n%s", code, out, errOut)
	}
	for _, want := range []string{
		"\n- PRRT_a a.go:3 **Nil map.** Use make(\\x1b[31m).\n    replies: 1\n    url: https://x/1\n    **Nil map.**\n\n    Use make(\\x1b[31m).\n",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("missing %q in:\n%s", want, out)
		}
	}
	if strings.ContainsRune(out, 0x1b) {
		t.Error("a raw escape character reached the output")
	}
	// JSON: body and replies only with --bodies.
	_, plain, _ := run(t, "pr", "wait", "7", "--repo", "o/r", "--once")
	_, bodies, _ := run(t, "pr", "wait", "7", "--repo", "o/r", "--once", "--bodies")
	if strings.Contains(plain, `"body"`) || strings.Contains(plain, `"replies"`) {
		t.Errorf("JSON without --bodies grew fields:\n%s", plain)
	}
	if !strings.Contains(bodies, `"body": "**Nil map.**\n\nUse make(\\x1b[31m).\n"`) || !strings.Contains(bodies, `"replies": 1`) {
		t.Errorf("JSON with --bodies:\n%s", bodies)
	}
}

func TestPRThread(t *testing.T) {
	fakeGH(t, cliThreads, cliThread)
	code, out, errOut := run(t, "pr", "thread", "PRRT_a", "--format", "text")
	want := "pr thread: ok (exit 0)\nthread: PRRT_a\nrepo: o/r\npr: 7\npath: a.go:3\nresolved: false\noutdated: false\n" +
		"comments: 1\ncomments_complete: true\n- coderabbitai 2026-10-01T10:00:00Z\n    **Nil map.**\n    Use make.\\u202e\n"
	if code != 0 || out != want {
		t.Fatalf("exit %d stderr %q\ngot:\n%s\nwant:\n%s", code, errOut, out, want)
	}
	code, out, _ = run(t, "pr", "thread", "PRRT_a", "--repo", "o/r")
	var res pr.ThreadResult
	if code != 0 || json.Unmarshal([]byte(out), &res) != nil || len(res.Comments) != 1 || res.Comments[0].Author != "coderabbitai" {
		t.Errorf("json: exit %d %s", code, out)
	}
	if code, _, errOut := run(t, "pr", "thread", "PRRT_a", "--repo", "o/other"); code != 2 || !strings.Contains(errOut, "belongs to o/r") {
		t.Errorf("other repo: exit %d %q, want 2", code, errOut)
	}
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"pr", "thread"}, "exactly one thread id"},
		{[]string{"pr", "thread", "123"}, "not a review-thread node id"},
		{[]string{"pr", "thread", "PRRT_a", "--repo", "bad"}, "not OWNER/NAME"},
	} {
		if code, _, errOut := run(t, c.args...); code != 1 || !strings.Contains(errOut, c.want) {
			t.Errorf("%v: exit %d %q", c.args, code, errOut)
		}
	}
}

func TestThreadExitCodesCoverEveryStatus(t *testing.T) {
	all := []pr.ThreadStatus{pr.ThreadOK, pr.ThreadGHError, pr.ThreadRefused, pr.ThreadInterrupted}
	seen := map[int]bool{}
	for _, s := range all {
		code, ok := threadExitCodes[s]
		if !ok || seen[code] {
			t.Errorf("%s: code %d ok %v (duplicate or missing)", s, code, ok)
		}
		seen[code] = true
	}
	if len(threadExitCodes) != len(all) {
		t.Errorf("threadExitCodes has %d entries", len(threadExitCodes))
	}
}

// Golden renderings: small fixtures, one per command.
func TestTextRenderers(t *testing.T) {
	zero, two := 0, 2
	cost := 0.25
	done := true
	cases := []struct {
		name, got, want string
	}{
		{"codex", agentText("codex", agent.Result{Status: agent.StatusOK, Exit: &zero, Mode: "exec", Sandbox: "read-only", Dir: "/r",
			Final: "/o/final.md", Stderr: "/o/stderr.log", DurationS: 1.5, CostUSD: &cost,
			Usage: &agent.Usage{InputTokens: 10, CachedInputTokens: 4, OutputTokens: 3}}, 0, "/o"),
			"codex: ok (exit 0)\nfinal: /o/final.md\ncodex_exit: 0\nmode: exec\nsandbox: read-only\ndir: /r\nduration_s: 1.5\n" +
				"tokens: input 10 (cached 4), output 3\ncost_usd: 0.25\nstderr: /o/stderr.log\njson: /o/result.json\n"},
		{"claude killed", agentText("claude", agent.Result{Status: agent.StatusStalled, Error: "no activity"}, 125, ""),
			"claude: stalled (exit 125)\nclaude_exit: none (killed by agentflow)\nduration_s: 0\nerror: no activity\n"},
		{"coderabbit", coderabbitText(coderabbit.Result{Status: coderabbit.StatusFindings, Base: "origin/main", Out: "/o",
			ReviewedFiles: []string{"a.go"}, Severities: map[string]int{"minor": 1, "major": 1},
			Findings: []coderabbit.Finding{{Severity: "major", File: "a.go", Text: "Bug.\nDetail \x1b[1m"}, {File: "b.go", Text: "Nit."}}}, 10),
			"coderabbit: findings (exit 10)\nbase: origin/main\nfindings: 2 (major 1, minor 1)\nreviewed_files: 1\nduration_s: 0\njson: /o/result.json\n" +
				"- [major] a.go\n    Bug.\n    Detail \\x1b[1m\n- [unknown] b.go\n    Nit.\n"},
		{"ship verify", verifyText(ship.Result{Status: ship.StatusTimeout, Service: "s.prod", Expected: "abc", Running: "def", Attempts: 3, DurationS: 90, CheckedAt: "T", Error: "timed out"}, 124),
			"ship verify: timeout (exit 124)\nservice: s.prod\nexpected: abc\nrunning: def\nattempts: 3\nduration_s: 90\nchecked_at: T\nerror: timed out\n"},
		{"ship announce", announceText(ship.AnnounceResult{Status: ship.AnnounceDryRun, Service: "s.prod", SHA: "abc", Channel: "#c", Payload: json.RawMessage(`{"text":"x"}`)}, 0),
			"ship announce: dry_run (exit 0)\nservice: s.prod\nsha: abc\nchannel: #c\npayload: (dry run; the Slack payload follows)\n    {\"text\":\"x\"}\n"},
		{"pr wait", waitText(pr.Result{Status: pr.StatusOpenThreads, Repo: "o/r", PR: 7, Head: "h", Reviewed: "h", ThreadsComplete: &done, Next: "fix",
			Attempts: 1, CheckedAt: "T", OpenThreads: []pr.Thread{{ID: "PRRT_1", Path: "a.go", Line: 3, URL: "u", Excerpt: "Nil map."}}}, 10),
			"pr wait: open_threads (exit 10)\nrepo: o/r\npr: 7\nhead: h\nreviewed: h\nopen_threads: 1\nthreads_complete: true\nnext: fix\n" +
				"attempts: 1\nduration_s: 0\nchecked_at: T\n- PRRT_1 a.go:3 Nil map.\n    url: u\n"},
		{"pr reply", replyText(pr.ReplyResult{Status: pr.ReplyNotResolved, Thread: "PRRT_1", Repo: "o/r", PR: 7, Body: "Keeping as-is: x", Replied: true, Next: "run again"}, 3),
			"pr reply: not_resolved (exit 3)\nthread: PRRT_1\nrepo: o/r\npr: 7\nreplied: true\nresolved: false\nbody: Keeping as-is: x\nnext: run again\n"},
		{"pr merge", mergeText(pr.MergeResult{Status: pr.MergeRefused, Repo: "o/r", PR: 7, Method: "merge", Reasons: []string{"draft", "2 unresolved threads"}}, 2),
			"pr merge: refused (exit 2)\nrepo: o/r\npr: 7\nmethod: merge\n- refused: draft\n- refused: 2 unresolved threads\n"},
		{"pr merge sync", mergeText(pr.MergeResult{Status: pr.MergeMerged, Repo: "o/r", PR: 7, Method: "squash", MergeSHA: "m", Sync: &pr.Sync{Branch: "development", SHA: "m", Status: "synced"}}, 0),
			"pr merge: merged (exit 0)\nrepo: o/r\npr: 7\nmethod: squash\nmerge_sha: m\nsync: development synced m\n"},
		{"pr thread refused", threadText(pr.ThreadResult{Status: pr.ThreadRefused, Thread: "PRRT_1", Error: "not a thread"}, 2),
			"pr thread: refused (exit 2)\nthread: PRRT_1\nerror: not a thread\n"},
		{"worktree done", worktreeDoneText(worktreeEntry{Check: worktree.Check{Path: "/w", Branch: "b", Head: "h", Refusals: []string{"dirty", "in use"}}}, "origin/main", false, 2),
			"worktree done: refused (exit 2)\ninto: origin/main\n- [refused] /w\n    branch: b\n    refused: dirty\n    refused: in use\n"},
		{"worktree done dry", worktreeDoneText(worktreeEntry{Check: worktree.Check{Path: "/w", Branch: "b", OK: true, MergedVia: "PR #3"}}, "origin/main", true, 0),
			"worktree done: removable (dry run) (exit 0)\ninto: origin/main\n- [removable] /w\n    branch: b\n    merged_via: PR #3\n"},
		{"worktree sweep", worktreeSweepText([]worktreeEntry{
			{Check: worktree.Check{Path: "/a", Branch: "a", OK: true}, Removed: &worktree.Removed{FreedBytes: 9, BranchDeleted: true}},
			{Check: worktree.Check{Path: "/b"}, Error: "git failed"}}, "origin/main", 1, 1, 0, 9, "", true, 3),
			"worktree sweep: error (exit 3)\ninto: origin/main\nremovable: 1\nremoved: 1\npruned_stale_records: 0\nfreed_bytes: 9\n" +
				"- [removed] /a\n    branch: a\n    freed_bytes: 9\n    branch_deleted: true\n    remote_deleted: false\n- [error] /b\n    error: git failed\n"},
		{"lessons retire", retireText([]lessons.Candidate{{Lesson: lessons.Lesson{ID: "l2", Title: "T", File: "X.md"}, Reason: "unused", Linked: true}}, false),
			"lessons retire: ok (exit 0)\napplied: false\ncandidates: 1\n- l2 T: unused (X.md) [kept: linked from principles]\n"},
		{"lessons stats", statsText(lessons.Stats{Lessons: 5, Inbox: 2}),
			"lessons stats: ok (exit 0)\nlessons: 5\ninbox: 2\narchived: 0\nused_zero: 0\nmisled_nonzero: 0\nretire_candidates: 0\nretire_kept_linked: 0\nfalse_positives: 0\n"},
		{"thread replies", waitText(pr.Result{Status: pr.StatusClean, OpenThreads: []pr.Thread{{ID: "PRRT_2", Path: "p", Replies: &two, Body: "b"}}}, 0),
			"pr wait: clean (exit 0)\npr: 0\nopen_threads: 1\nattempts: 0\nduration_s: 0\n- PRRT_2 p\n    replies: 2\n    b\n"},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s:\ngot:\n%s\nwant:\n%s", c.name, c.got, c.want)
		}
	}
}
