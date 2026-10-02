package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// Claude Code has no OS sandbox flag like codex's sandbox_mode, so the
// backend pins the tool list instead (verified live on 2.1.288):
//
//   - --restricted drops Bash and the other code-running tools, ignores
//     user/project/local settings (so no hooks or permissive defaults), and
//     confines the file tools to the working directory; --strict-mcp-config
//     drops MCP servers.
//   - read-only: --tools Read,Grep,Glob, an allowlist, because the default
//     set also has tools that create worktrees, message other sessions or
//     upload content.
//   - write (fix mode): Edit, Write and Bash, with Bash inside Claude Code's
//     sandbox: writes confined to the working directory, no network.
const (
	claudeReadOnlyTools = "Read,Grep,Glob"
	claudeWriteTools    = "Read,Grep,Glob,Edit,Write,Bash"
	claudeSandbox       = `{"sandbox":{"enabled":true,"autoAllowBashIfSandboxed":true,"allowUnsandboxedCommands":false}}`
)

type claudeBackend struct{}

func (claudeBackend) name() string         { return "claude" }
func (claudeBackend) defaultBin() string   { return "claude" }
func (claudeBackend) failed() Status       { return StatusClaudeFailed }
func (claudeBackend) needsPrompt() bool    { return true }
func (claudeBackend) answerInStream() bool { return true }

func (claudeBackend) defaultHome() string {
	if h := os.Getenv("CLAUDE_CONFIG_DIR"); h != "" {
		return h
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".claude")
	}
	return ""
}

// The reviewer note goes in the system prompt (--append-system-prompt).
func (claudeBackend) wrapPrompt(prompt string) string { return prompt }

func (claudeBackend) sessionGlob(home, id string) string {
	return filepath.Join(home, "projects", "*", id+".jsonl")
}

// args assembles the claude argv. The prompt is read from stdin, which is
// the prompt file; the answer arrives in the final "result" event.
func (claudeBackend) args(o Options, _ string, _ bool) []string {
	args := []string{"-p", "--output-format", "stream-json", "--verbose",
		"--restricted", "--strict-mcp-config", "--append-system-prompt", ReviewerNote}
	if o.Write {
		args = append(args, "--tools", claudeWriteTools, "--permission-mode", "acceptEdits", "--settings", claudeSandbox)
	} else {
		args = append(args, "--tools", claudeReadOnlyTools)
	}
	if o.Model != "" {
		args = append(args, "--model", o.Model)
	}
	if o.MaxBudgetUSD > 0 {
		args = append(args, "--max-budget-usd", strconv.FormatFloat(o.MaxBudgetUSD, 'f', -1, 64))
	}
	return args
}

type claudeEvent struct {
	Type      string   `json:"type"`
	Subtype   string   `json:"subtype"`
	SessionID string   `json:"session_id"`
	IsError   bool     `json:"is_error"`
	Result    *string  `json:"result"`
	CostUSD   *float64 `json:"total_cost_usd"`
	Usage     *struct {
		InputTokens     int `json:"input_tokens"`
		CacheReadTokens int `json:"cache_read_input_tokens"`
		OutputTokens    int `json:"output_tokens"`
	} `json:"usage"`
}

func (claudeBackend) handle(line []byte, s *streamState) {
	var ev claudeEvent
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	if s.thread == "" && ev.SessionID != "" {
		s.thread = ev.SessionID
	}
	if ev.Type != "result" {
		return
	}
	s.cost = ev.CostUSD
	if ev.Usage != nil {
		s.use = &Usage{InputTokens: ev.Usage.InputTokens, CachedInputTokens: ev.Usage.CacheReadTokens, OutputTokens: ev.Usage.OutputTokens}
	}
	text := ""
	if ev.Result != nil {
		text = *ev.Result
	}
	if ev.Subtype == "success" && !ev.IsError {
		s.answer = text
		s.complete = true
		return
	}
	// error_max_turns, error_during_execution, a budget stop, or an API
	// error reported as a result: never an answer.
	// Keep the complete result so classification can see a rate-limit marker
	// even when Claude puts it after a generic first line such as "API Error".
	msg := strings.TrimSpace(text)
	s.fail = strings.TrimSpace(fmt.Sprintf("result %s: %s", ev.Subtype, msg))
}
