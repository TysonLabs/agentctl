package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// Backend is one agent CLI. Its methods are unexported: the set of agents is
// closed (Codex, Claude), and every guarantee lives in this package.
type Backend interface {
	name() string
	defaultBin() string
	defaultHome() string
	failed() Status
	// needsPrompt: the agent has no built-in reviewer for a bare scope.
	needsPrompt() bool
	args(o Options, finalPath string, inRepo bool) []string
	// wrapPrompt returns the prompt file's contents for a user prompt.
	wrapPrompt(prompt string) string
	// handle reads one stdout line; called with s.mu held.
	handle(line []byte, s *streamState)
	// sessionGlob matches the session log for a thread/session id.
	sessionGlob(home, id string) string
	// answerInStream: the final answer arrives in an event, not a file.
	answerInStream() bool
}

// The agents agentflow can run.
var (
	Codex  Backend = codexBackend{}
	Claude Backend = claudeBackend{}
)

type codexBackend struct{}

func (codexBackend) name() string         { return "codex" }
func (codexBackend) defaultBin() string   { return "codex" }
func (codexBackend) failed() Status       { return StatusFailed }
func (codexBackend) needsPrompt() bool    { return false }
func (codexBackend) answerInStream() bool { return false }

func (codexBackend) defaultHome() string {
	if h := os.Getenv("CODEX_HOME"); h != "" {
		return h
	}
	if home, err := os.UserHomeDir(); err == nil {
		return filepath.Join(home, ".codex")
	}
	return ""
}

func (codexBackend) wrapPrompt(prompt string) string { return ReviewerNote + "\n\n" + prompt }

func (codexBackend) sessionGlob(home, id string) string {
	return filepath.Join(home, "sessions", "*", "*", "*", "rollout-*-"+id+".jsonl")
}

// args assembles the codex argv. The sandbox goes through -c because
// `exec review` has no --sandbox flag and otherwise inherits the user's
// config default (seen: workspace-write).
func (codexBackend) args(o Options, finalPath string, inRepo bool) []string {
	args := []string{"exec"}
	if o.Prompt == "" {
		args = append(args, "review")
		switch {
		case o.Scope.Base != "":
			args = append(args, "--base", o.Scope.Base)
		case o.Scope.Commit != "":
			args = append(args, "--commit", o.Scope.Commit)
		case o.Scope.Uncommitted:
			args = append(args, "--uncommitted")
		}
	} else {
		// `exec review` takes no -C; it reviews the working directory, which
		// cmd.Dir already sets. Plain exec gets -C as well, for its prompt.
		args = append(args, "-C", o.Dir)
	}
	args = append(args, "--json", "-c", fmt.Sprintf("sandbox_mode=%q", sandbox(o.Write)), "-o", finalPath)
	if !inRepo {
		args = append(args, "--skip-git-repo-check")
	}
	if o.Model != "" {
		args = append(args, "-m", o.Model)
	}
	if o.Prompt != "" {
		args = append(args, "-") // prompt from stdin, which is the prompt file
	}
	return args
}

type codexEvent struct {
	Type     string `json:"type"`
	ThreadID string `json:"thread_id"`
	Message  string `json:"message"`
	Error    *struct {
		Message string `json:"message"`
	} `json:"error"`
	Usage *Usage `json:"usage"`
}

func (codexBackend) handle(line []byte, s *streamState) {
	var ev codexEvent
	if json.Unmarshal(line, &ev) != nil {
		return
	}
	switch ev.Type {
	case "thread.started":
		s.thread = ev.ThreadID
	case "turn.completed":
		s.use = ev.Usage
		s.complete = true
	case "turn.failed":
		if ev.Error != nil && strings.TrimSpace(ev.Error.Message) != "" {
			s.fail = ev.Error.Message
		} else {
			s.fail = "turn failed"
		}
	case "error":
		if strings.TrimSpace(ev.Message) != "" {
			s.fail = ev.Message
		} else {
			s.fail = "codex reported an error"
		}
	}
}
