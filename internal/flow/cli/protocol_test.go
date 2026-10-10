package cli

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const protocolFinal = "## Findings\n\n### F1 [Major] new.go:1\nWHY: w\nFIX: f\nTEST: t\n\n## Not fixed\n\nNone.\n"

// protocolRepo is a git repo with one untracked file, so --uncommitted has a diff.
func protocolRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	for _, args := range [][]string{{"init", "-q"}, {"-c", "user.email=t@e", "-c", "user.name=t", "commit", "-q", "--allow-empty", "-m", "x"}} {
		if out, err := exec.Command("git", append([]string{"-C", repo}, args...)...).CombinedOutput(); err != nil {
			t.Fatalf("git: %v %s", err, out)
		}
	}
	if err := os.WriteFile(filepath.Join(repo, "new.go"), []byte("package x // FRESH\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	return repo
}

// fakeAgent installs a shell fake for backend that saves its stdin to the
// returned path and answers with final.
func fakeAgent(t *testing.T, backend, final string) string {
	t.Helper()
	tmp := t.TempDir()
	stdinCopy := filepath.Join(tmp, "stdin.txt")
	answer := filepath.Join(tmp, "answer.md")
	if err := os.WriteFile(answer, []byte(final), 0o644); err != nil {
		t.Fatal(err)
	}
	var script string
	if backend == "claude" {
		res, _ := json.Marshal(final)
		script = "#!/bin/sh\ncat > " + stdinCopy + "\n" +
			`echo '{"type":"system","subtype":"init","session_id":"s1"}'` + "\n" +
			`cat <<'EOF'` + "\n" +
			`{"type":"result","subtype":"success","is_error":false,"result":` + string(res) + `,"session_id":"s1"}` + "\nEOF\n"
	} else {
		script = "#!/bin/sh\ncat > " + stdinCopy + "\n" +
			`while [ $# -gt 0 ]; do if [ "$1" = -o ]; then final="$2"; fi; shift; done` + "\n" +
			`echo '{"type":"thread.started","thread_id":"t1"}'` + "\n" +
			`cp ` + answer + ` "$final"` + "\n" +
			`echo '{"type":"turn.completed","usage":{"input_tokens":1,"output_tokens":1}}'` + "\n"
	}
	fake := filepath.Join(tmp, backend)
	if err := os.WriteFile(fake, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENTFLOW_"+strings.ToUpper(backend), fake)
	return stdinCopy
}

func TestProtocolPromptOrderAndFindings(t *testing.T) {
	for _, backend := range []string{"codex", "claude"} {
		for _, mode := range []string{"fix", "review"} {
			t.Run(backend+"/"+mode, func(t *testing.T) {
				repo := protocolRepo(t)
				stdinCopy := fakeAgent(t, backend, protocolFinal)
				args := []string{backend, "--dir", repo, "--uncommitted", "--out", t.TempDir(),
					"--prompt", "BRIEF-MARKER: the change", "--protocol", mode,
					"--lessons", "concurrency", "--lessons-dir", lessonsFixture(t)}
				if mode == "fix" {
					args = append(args, "--write", "--test-cmd", "go test ./x", "--test-cmd", "make check")
				}
				code, out, errOut := run(t, args...)
				if code != 0 {
					t.Fatalf("exit %d\nstdout %s\nstderr %s", code, out, errOut)
				}
				sent, _ := os.ReadFile(stdinCopy)
				s := string(sent)
				order := []string{"## Review protocol: " + mode + " mode", "BRIEF-MARKER", "## Output format", "- **l1**", "```diff", "// FRESH"}
				last := -1
				for _, marker := range order {
					i := strings.Index(s, marker)
					if i <= last {
						t.Fatalf("%q at %d, want after %d:\n%s", marker, i, last, s)
					}
					last = i
				}
				if !strings.Contains(s, "[learned l12]") {
					t.Error("the learned tag is not explained with --lessons")
				}
				if mode == "fix" {
					for _, want := range []string{"Do not commit", "- `go test ./x`", "- `make check`"} {
						if !strings.Contains(s, want) {
							t.Errorf("fix prompt missing %q", want)
						}
					}
				} else if !strings.Contains(s, "Do not edit") || strings.Contains(s, "Do not commit") {
					t.Error("review prompt has the wrong rules")
				}
				var res struct {
					Protocol string           `json:"protocol"`
					Findings []map[string]any `json:"findings"`
					Warnings []string         `json:"warnings"`
				}
				if err := json.Unmarshal([]byte(out), &res); err != nil {
					t.Fatal(err)
				}
				if res.Protocol != mode || len(res.Findings) != 1 || res.Findings[0]["file"] != "new.go" || res.Warnings != nil {
					t.Errorf("result: %s", out)
				}
			})
		}
	}
}

func TestProtocolWithoutLessonsOrBrief(t *testing.T) {
	for _, backend := range []string{"codex", "claude"} {
		t.Run(backend, func(t *testing.T) {
			repo := protocolRepo(t)
			stdinCopy := fakeAgent(t, backend, "Looks fine.")
			code, out, errOut := run(t, backend, "--dir", repo, "--uncommitted", "--out", t.TempDir(), "--protocol", "review")
			if code != 0 {
				t.Fatalf("exit %d\nstdout %s\nstderr %s", code, out, errOut)
			}
			s, _ := os.ReadFile(stdinCopy)
			if strings.Contains(string(s), "[learned") || !strings.Contains(string(s), "No brief was given") ||
				strings.Contains(string(s), "Review the diff below") || !strings.Contains(string(s), "```diff") {
				t.Errorf("prompt:\n%s", s)
			}
			// A codex scope with no prompt would be the native reviewer; the protocol makes it exec.
			if !strings.Contains(out, `"mode": "exec"`) {
				t.Errorf("want exec mode: %s", out)
			}
			// A final message that does not parse: findings null, a warning, exit 0.
			if !strings.Contains(out, `"findings": null`) || !strings.Contains(out, "protocol format") {
				t.Errorf("garbage final: %s", out)
			}
		})
	}
}

func TestProtocolFlagErrors(t *testing.T) {
	t.Setenv("AGENTFLOW_CODEX", "/no/such/codex") // must never be reached
	t.Setenv("AGENTFLOW_CLAUDE", "/no/such/claude")
	cases := []struct {
		name    string
		args    []string
		wantErr string
	}{
		{"unknown mode", []string{"codex", "--prompt", "p", "--protocol", "fixes"}, "must be fix or review"},
		{"fix without write", []string{"codex", "--prompt", "p", "--protocol", "fix"}, "needs --write"},
		{"claude fix without write", []string{"claude", "--prompt", "p", "--protocol", "fix"}, "needs --write"},
		{"review with write", []string{"codex", "--prompt", "p", "--protocol", "review", "--write"}, "read-only"},
		{"claude review with write", []string{"claude", "--prompt", "p", "--protocol", "review", "--write"}, "read-only"},
		{"test-cmd without protocol", []string{"codex", "--prompt", "p", "--test-cmd", "go test"}, "--test-cmd needs --protocol fix"},
		{"test-cmd with review", []string{"codex", "--prompt", "p", "--protocol", "review", "--test-cmd", "go test"}, "--test-cmd needs --protocol fix"},
		{"empty test-cmd", []string{"codex", "--prompt", "p", "--protocol", "fix", "--write", "--test-cmd", " "}, "one non-empty line"},
		{"backtick test-cmd", []string{"codex", "--prompt", "p", "--protocol", "fix", "--write", "--test-cmd", "go test `x`"}, "one non-empty line"},
		{"protocol, nothing to review", []string{"codex", "--protocol", "review"}, "give --prompt"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			code, _, errOut := run(t, c.args...)
			if code != 1 || !strings.Contains(errOut, c.wantErr) {
				t.Errorf("code=%d stderr=%q, want 1 and %q", code, errOut, c.wantErr)
			}
		})
	}
}

// The size cap counts the protocol text, not just the caller's brief.
func TestProtocolCountsTowardSizeCap(t *testing.T) {
	t.Setenv("AGENTFLOW_CODEX", "/no/such/codex") // must never be reached
	code, _, errOut := run(t, "codex", "--dir", t.TempDir(), "--prompt", "p", "--protocol", "review", "--max-prompt-bytes", "200")
	if code != 1 || !strings.Contains(errOut, "200-byte cap") {
		t.Errorf("code=%d stderr=%q", code, errOut)
	}
}
