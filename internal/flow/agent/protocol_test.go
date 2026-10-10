package agent

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const goodFinal = `I read the diff and the callers.

## Findings

### F1 [major] internal/x/a.go:42-48
WHY: a nil map write panics when the cache is cold.
  It happens on the first request.
FIX: initialise the map in New.
TEST: TestColdCache in a_test.go; it panics without the fix.

### F2 [Minor] cmd/b c.go:7 [learned l12]
WHY: the error is dropped.
FIX: return it.
TEST: TestErrReturned.

## Not fixed

- The log line is noisy; out of scope.
`

func TestParseFindingsGood(t *testing.T) {
	got, err := ParseFindings(goodFinal)
	if err != nil {
		t.Fatal(err)
	}
	want := []Finding{
		{ID: "F1", Severity: "Major", File: "internal/x/a.go", Line: 42,
			Why:  "a nil map write panics when the cache is cold.\n  It happens on the first request.",
			Fix:  "initialise the map in New.",
			Test: "TestColdCache in a_test.go; it panics without the fix."},
		{ID: "F2", Severity: "Minor", File: "cmd/b c.go", Line: 7, Learned: "l12",
			Why: "the error is dropped.", Fix: "return it.", Test: "TestErrReturned."},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got  %+v\nwant %+v", got, want)
	}
}

func TestParseFindingsNone(t *testing.T) {
	got, err := ParseFindings("## Findings\n\nNone.\n\n## Not fixed\n\nNone.\n")
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || len(got) != 0 {
		t.Errorf("got %#v, want an empty non-nil slice", got)
	}
	b, _ := json.Marshal(got)
	if string(b) != "[]" {
		t.Errorf("marshals as %s, want []", b)
	}
}

func TestParseFindingsFencedFieldText(t *testing.T) {
	text := "## Findings\n\n### F1 [Blocker] a.go:1\nWHY: input\n```go\n### F9 not a heading\nWHY: inside a fence\n## Not a section\n```\nFIX: f\nTEST: t\n"
	got, err := ParseFindings(text)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || !strings.Contains(got[0].Why, "### F9 not a heading") || got[0].Fix != "f" {
		t.Errorf("got %+v", got)
	}
}

func TestParseFindingsRejects(t *testing.T) {
	cases := map[string]string{
		"garbage":            "Looks fine to me, no real issues.",
		"no heading":         "### F1 [Major] a.go:1\nWHY: w\nFIX: f\nTEST: t\n",
		"partial: no TEST":   "## Findings\n\n### F1 [Major] a.go:1\nWHY: w\nFIX: f\nTEST: t\n\n### F2 [Major] b.go:2\nWHY: w\nFIX: f\n",
		"empty WHY":          "## Findings\n\n### F1 [Major] a.go:1\nWHY:\nFIX: f\nTEST: t\n",
		"bad severity":       "## Findings\n\n### F1 [High] a.go:1\nWHY: w\nFIX: f\nTEST: t\n",
		"no line":            "## Findings\n\n### F1 [Major] a.go\nWHY: w\nFIX: f\nTEST: t\n",
		"line zero":          "## Findings\n\n### F1 [Major] a.go:0\nWHY: w\nFIX: f\nTEST: t\n",
		"copied placeholder": "## Findings\n\n### F1 [<severity>] <path>:<line>\nWHY: w\nFIX: f\nTEST: t\n",
		"duplicate id":       "## Findings\n\n### F1 [Major] a.go:1\nWHY: w\nFIX: f\nTEST: t\n### F1 [Minor] b.go:2\nWHY: w\nFIX: f\nTEST: t\n",
		"field twice":        "## Findings\n\n### F1 [Major] a.go:1\nWHY: w\nWHY: again\nFIX: f\nTEST: t\n",
		"text before WHY":    "## Findings\n\n### F1 [Major] a.go:1\nstray\nWHY: w\nFIX: f\nTEST: t\n",
		"stray text":         "## Findings\n\nSome prose instead of blocks.\n",
		"empty section":      "## Findings\n\n## Not fixed\n\nNone.\n",
		"none and findings":  "## Findings\n\nNone.\n\n### F1 [Major] a.go:1\nWHY: w\nFIX: f\nTEST: t\n",
		"two headings":       "## Findings\n\nNone.\n\n## Findings\n\nNone.\n",
		"unclosed fence":     "## Findings\n\n### F1 [Major] a.go:1\nWHY: w\n```\nFIX: f\nTEST: t\n",
	}
	for name, text := range cases {
		t.Run(name, func(t *testing.T) {
			if got, err := ParseFindings(text); err == nil || got != nil {
				t.Errorf("got %+v, %v; want nil and an error", got, err)
			}
		})
	}
}

func TestWrapBrief(t *testing.T) {
	fix := WrapBrief(ProtocolPrompt{Mode: ProtocolFix, TestCmds: []string{"go test ./x"}}, "BRIEF\n")
	for _, want := range []string{"fix mode", "Do not commit", "inside this repository", "no new dependency",
		"fails without the fix", "loopback sockets", `"Not fixed"`, "- `go test ./x`", "FIX: <what you changed>"} {
		if !strings.Contains(fix, want) {
			t.Errorf("fix protocol missing %q", want)
		}
	}
	h, br, f := strings.Index(fix, "## Review protocol"), strings.Index(fix, "BRIEF"), strings.Index(fix, "## Output format")
	if !(h == 0 && h < br && br < f) {
		t.Errorf("order: header %d, brief %d, format %d", h, br, f)
	}
	review := WrapBrief(ProtocolPrompt{Mode: ProtocolReview}, "")
	for _, want := range []string{"review mode", "Do not edit", "No brief was given", "FIX: <the change you suggest>"} {
		if !strings.Contains(review, want) {
			t.Errorf("review protocol missing %q", want)
		}
	}
	for _, s := range []string{fix, review} {
		if strings.Contains(s, "[learned") {
			t.Error("learned tag explained without --lessons")
		}
	}
	if !strings.Contains(WrapBrief(ProtocolPrompt{Mode: ProtocolReview, Lessons: true}, "b"), "[learned l12]") {
		t.Error("learned tag not explained with --lessons")
	}
	if _, err := ParseProtocol("fixes"); err == nil {
		t.Error("ParseProtocol accepted an unknown mode")
	}
}

func TestSetFindings(t *testing.T) {
	final := filepath.Join(t.TempDir(), "final.md")
	if err := os.WriteFile(final, []byte(goodFinal), 0o644); err != nil {
		t.Fatal(err)
	}
	render := func(r Result) map[string]any {
		b, err := json.Marshal(r)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		return m
	}

	plain := Result{Status: StatusOK, Agent: "codex", Final: final}
	plain.SetFindings(ProtocolNone)
	if m := render(plain); m["findings"] != nil || m["protocol"] != nil {
		t.Errorf("no protocol: findings/protocol must be omitted: %v", m)
	} else if _, ok := m["findings"]; ok {
		t.Error("no protocol: findings key present")
	}

	ok := Result{Status: StatusOK, Agent: "codex", Final: final}
	ok.SetFindings(ProtocolFix)
	if m := render(ok); m["protocol"] != "fix" || len(m["findings"].([]any)) != 2 || m["warnings"] != nil {
		t.Errorf("parsed: %v", m)
	}

	failed := Result{Status: StatusFailed, Agent: "codex", Final: final}
	failed.SetFindings(ProtocolReview)
	m := render(failed)
	if v, present := m["findings"]; !present || v != nil || m["warnings"] != nil {
		t.Errorf("failed run: want findings null and no warning: %v", m)
	}

	if err := os.WriteFile(final, []byte("All good."), 0o644); err != nil {
		t.Fatal(err)
	}
	bad := Result{Status: StatusOK, Agent: "codex", Final: final}
	bad.SetFindings(ProtocolReview)
	m = render(bad)
	if v, present := m["findings"]; !present || v != nil || bad.Status != StatusOK {
		t.Errorf("garbage: want findings null and status ok: %v", m)
	}
	if len(bad.Warnings) != 1 || !strings.Contains(bad.Warnings[0], "protocol format") {
		t.Errorf("garbage: warnings %q", bad.Warnings)
	}
}
