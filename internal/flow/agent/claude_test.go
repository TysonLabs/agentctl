package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// fakeClaude acts out one `claude -p --output-format stream-json` behaviour.
// FAKE_CODEX_REC is reused as the directory where it records argv and stdin.
func fakeClaude(mode string) int {
	rec := os.Getenv("FAKE_CODEX_REC")
	_ = os.WriteFile(filepath.Join(rec, "args.txt"), []byte(strings.Join(os.Args[1:], "\n")), 0o644)
	in, _ := io.ReadAll(os.Stdin)
	_ = os.WriteFile(filepath.Join(rec, "stdin.txt"), in, 0o644)

	sid := "sess-" + mode
	emit := func(s string) { fmt.Println(s) }
	result := func(subtype string, isError bool, text string) {
		b, _ := json.Marshal(map[string]any{
			"type": "result", "subtype": subtype, "is_error": isError, "result": text, "session_id": sid,
			"total_cost_usd": 0.42, "usage": map[string]int{"input_tokens": 10, "cache_read_input_tokens": 7, "output_tokens": 3},
		})
		emit(string(b))
	}
	emit(`{"type":"system","subtype":"init","session_id":"` + sid + `","tools":["Read","Grep","Glob"]}`)
	emit(`{"type":"assistant","session_id":"` + sid + `","message":{"content":[{"type":"text","text":"working"}]}}`)
	switch mode {
	case "ok":
		result("success", false, "No findings.\n")
		return 0
	case "empty":
		result("success", false, "  \n")
		return 0
	case "no-result":
		return 0
	case "max-turns":
		result("error_max_turns", true, "")
		return 0
	case "api-error":
		result("success", true, "API Error: 500 internal error")
		return 1
	case "ratelimit":
		result("success", true, "API Error: 429 rate limit exceeded, retry later")
		return 1
	case "session-log":
		// Silent on stdout for 3s while the session log grows.
		day := filepath.Join(os.Getenv("FAKE_CODEX_HOME"), "projects", "-tmp-repo")
		_ = os.MkdirAll(day, 0o755)
		log := filepath.Join(day, sid+".jsonl")
		for i := range 30 {
			time.Sleep(100 * time.Millisecond)
			_ = os.WriteFile(log, []byte(strings.Repeat("x", i+1)), 0o644)
		}
		result("success", false, "done\n")
		return 0
	}
	return 99
}

func claudeHarness(t *testing.T, mode string) (*harness, Options) {
	t.Helper()
	t.Setenv("FAKE_CODEX_MODE", "")
	h := &harness{t: t, rec: t.TempDir(), out: t.TempDir()}
	t.Setenv("FAKE_CLAUDE_MODE", mode)
	t.Setenv("FAKE_CODEX_REC", h.rec)
	o := h.opts()
	o.Agent = Claude
	return h, o
}

func TestClaudeOKReadOnly(t *testing.T) {
	h, o := claudeHarness(t, "ok")
	o.Model = "opus"
	o.MaxBudgetUSD = 2.5
	res := run(t, o)
	if res.Status != StatusOK || res.Agent != "claude" || res.ThreadID != "sess-ok" {
		t.Fatalf("got %+v", res)
	}
	if res.CostUSD == nil || *res.CostUSD != 0.42 || res.Usage == nil || res.Usage.CachedInputTokens != 7 {
		t.Errorf("cost/usage not parsed: %+v %+v", res.CostUSD, res.Usage)
	}
	if b, _ := os.ReadFile(res.Final); string(b) != "No findings.\n" {
		t.Errorf("final = %q, want the result text", b)
	}
	if got := h.recorded("stdin.txt"); got != "review this" {
		t.Errorf("stdin = %q, want the bare prompt (the note goes in the system prompt)", got)
	}
	args := strings.Split(h.recorded("args.txt"), "\n")
	want := []string{"-p", "--output-format", "stream-json", "--verbose", "--restricted", "--strict-mcp-config",
		"--append-system-prompt", ReviewerNote, "--tools", "Read,Grep,Glob", "--model", "opus", "--max-budget-usd", "2.5"}
	if !reflect.DeepEqual(args, want) {
		t.Errorf("argv:\n got %q\nwant %q", args, want)
	}
}

func TestClaudeWriteModeIsSandboxed(t *testing.T) {
	h, o := claudeHarness(t, "ok")
	o.Write = true
	if res := run(t, o); res.Status != StatusOK || res.Sandbox != "workspace-write" {
		t.Fatalf("got %+v", res)
	}
	args := h.recorded("args.txt")
	for _, want := range []string{"--restricted", "--tools\nRead,Grep,Glob,Edit,Write,Bash", "--permission-mode\nacceptEdits",
		`"sandbox":{"enabled":true`, `"allowUnsandboxedCommands":false`} {
		if !strings.Contains(args, want) {
			t.Errorf("argv missing %q:\n%s", want, args)
		}
	}
	if strings.Contains(args, "bypassPermissions") || strings.Contains(args, "dangerously") {
		t.Errorf("write mode must never bypass permissions:\n%s", args)
	}
}

func TestClaudeReadOnlyHasNoWriteTools(t *testing.T) {
	for _, tool := range []string{"Edit", "Write", "Bash", "NotebookEdit", "Task", "Agent"} {
		for _, a := range Claude.args(Options{}, "", true) {
			for _, listed := range strings.Split(a, ",") {
				if listed == tool {
					t.Errorf("read-only argv grants %s", tool)
				}
			}
		}
	}
}

func TestClaudeFailures(t *testing.T) {
	cases := []struct {
		mode    string
		want    Status
		wantErr string
	}{
		{"empty", StatusNoAnswer, "no final answer"},
		{"no-result", StatusNoAnswer, "without a completed turn"},
		{"max-turns", StatusClaudeFailed, "error_max_turns"},
		{"api-error", StatusClaudeFailed, "API Error: 500"},
		{"ratelimit", StatusRateLimited, "429"},
	}
	for _, c := range cases {
		t.Run(c.mode, func(t *testing.T) {
			_, o := claudeHarness(t, c.mode)
			res := run(t, o)
			if res.Status != c.want || !strings.Contains(res.Error, c.wantErr) {
				t.Fatalf("got status=%s error=%q, want %s containing %q", res.Status, res.Error, c.want, c.wantErr)
			}
		})
	}
}

func TestClaudeErrorResultIsNeverTheAnswer(t *testing.T) {
	_, o := claudeHarness(t, "api-error")
	res := run(t, o)
	if _, err := os.Stat(res.Final); err == nil {
		t.Errorf("an error result was written as the final answer")
	}
}

func TestClaudeSessionLogCountsAsActivity(t *testing.T) {
	_, o := claudeHarness(t, "session-log")
	t.Setenv("FAKE_CODEX_HOME", o.Home)
	o.Stall = 1500 * time.Millisecond
	res := run(t, o)
	if res.Status != StatusOK || !strings.HasSuffix(res.Rollout, "sess-session-log.jsonl") {
		t.Fatalf("got %+v", res)
	}
}

func TestClaudeNeedsAPrompt(t *testing.T) {
	_, o := claudeHarness(t, "ok")
	o.Prompt = ""
	o.Scope = Scope{Base: "main"}
	if _, err := Run(context.Background(), o); err == nil || !strings.Contains(err.Error(), "needs a prompt") {
		t.Fatalf("err = %v", err)
	}
}

func TestResultJSONNamesTheAgentsExit(t *testing.T) {
	zero := 0
	for _, c := range []struct{ agent, want, not string }{{"codex", `"codex_exit":0`, "claude_exit"}, {"claude", `"claude_exit":0`, "codex_exit"}} {
		b, err := json.Marshal(Result{Status: StatusOK, Agent: c.agent, Exit: &zero})
		if err != nil || !strings.Contains(string(b), c.want) || strings.Contains(string(b), c.not) {
			t.Errorf("%s: %s %v", c.agent, b, err)
		}
	}
	b, _ := json.Marshal(Result{Status: StatusTimeout, Agent: "codex"})
	if !strings.Contains(string(b), `"codex_exit":null`) {
		t.Errorf("a killed run must report a null exit: %s", b)
	}
}
