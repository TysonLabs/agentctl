package agent

import (
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// Protocol is a built-in review protocol that wraps the caller's brief, so a
// brief only has to say what changed and what it must guarantee. The final
// message format is fixed, so ParseFindings can read it into the JSON result.
type Protocol string

const (
	ProtocolNone   Protocol = ""
	ProtocolFix    Protocol = "fix"    // find defects and fix them in the working tree (needs Write)
	ProtocolReview Protocol = "review" // find defects and report them; edit nothing (read-only)
)

// ParseProtocol accepts "", "fix" or "review".
func ParseProtocol(s string) (Protocol, error) {
	switch p := Protocol(s); p {
	case ProtocolNone, ProtocolFix, ProtocolReview:
		return p, nil
	}
	return "", fmt.Errorf("--protocol must be fix or review, not %q", s)
}

// ProtocolPrompt holds what WrapBrief needs besides the brief.
type ProtocolPrompt struct {
	Mode     Protocol
	TestCmds []string // fix mode: the targeted tests to run
	Lessons  bool     // a learned-checks section follows: explain the [learned lNNN] tag
}

const fixHeader = `## Review protocol: fix mode

You review a change that another agent wrote. Find the real defects in it and fix them.

Rules:
- A real defect is wrong behaviour, a broken guarantee, a security hole or a missed case. Skip style and taste.
- Fix each defect in the working tree.
- Do not commit, stage, push or open a pull request.
- Edit only files inside this repository.
- Add no new dependency unless the fix needs it. If you add one, say why in FIX.
- For each fix, add one test that fails without the fix and passes with it.
- Run the targeted tests for the code you changed.
- Tests that bind loopback sockets (a local HTTP or TCP server) fail in this sandbox. That failure is not a defect. Name the test in TEST; the caller runs it.
- If you choose not to fix something, list it under "Not fixed" with the reason.
`

const reviewHeader = `## Review protocol: review mode

You review a change that another agent wrote. Find the real defects in it and report them.

Rules:
- A real defect is wrong behaviour, a broken guarantee, a security hole or a missed case. Skip style and taste.
- Do not edit, create or delete any file.
- For each defect, give the failing scenario, the fix you suggest and a test that would catch it.
- If you see a problem that you judge is not worth fixing, list it under "Not fixed" with the reason.
`

const lessonsRule = "- For a finding from the learned-checks section, put its tag at the end of the heading line, for example `### F2 [Minor] a/b.go:7 [learned l12]`.\n"

// The example uses placeholders that do not parse, so an answer that copies
// it unchanged is reported as malformed rather than as a finding.
const outputFormat = `## Output format

Your final message must have this shape. A program reads it.

` + "```text" + `
## Findings

### F1 [<severity>] <path>:<line>
WHY: <the failing scenario: the input or steps, and the wrong result>
FIX: <FIXTEXT>
TEST: <TESTTEXT>

## Not fixed

- <what, and why you left it>
` + "```" + `

- Number the findings F1, F2, F3 and so on, the most severe first.
- <severity> is Blocker, Major or Minor.
- <path> is relative to the repository root. <line> is a line number.
- WHY, FIX and TEST each start a new line. A field can continue on the lines below it.
- If you find no defects, write "None." under "## Findings".
- If you left nothing unfixed, write "None." under "## Not fixed".
`

// WrapBrief returns the protocol header, the caller's brief and the output
// format, in that order. The caller appends the learned-checks section and
// then the diff (BuildPrompt), so the size cap counts all of it.
func WrapBrief(p ProtocolPrompt, brief string) string {
	var b strings.Builder
	fix, test := "<the change you suggest>", "<a test that would fail on this defect>"
	if p.Mode == ProtocolFix {
		b.WriteString(fixHeader)
		if len(p.TestCmds) > 0 {
			b.WriteString("\nThe targeted tests to run:\n")
			for _, c := range p.TestCmds {
				b.WriteString("- `" + c + "`\n")
			}
		}
		fix, test = "<what you changed>", "<the test you added; it fails without the fix>"
	} else {
		b.WriteString(reviewHeader)
	}
	if p.Lessons {
		b.WriteString(lessonsRule)
	}
	b.WriteString("\n## Brief\n\n")
	if brief = strings.TrimSpace(brief); brief != "" {
		b.WriteString(brief + "\n")
	} else {
		b.WriteString("No brief was given. Review the diff on its own terms.\n")
	}
	b.WriteString("\n")
	b.WriteString(strings.NewReplacer("<FIXTEXT>", fix, "<TESTTEXT>", test).Replace(outputFormat))
	return b.String()
}

// Finding is one parsed block of a protocol final message.
type Finding struct {
	ID       string `json:"id"`       // "F1"
	Severity string `json:"severity"` // Blocker, Major or Minor
	File     string `json:"file"`
	Line     int    `json:"line"`
	Learned  string `json:"learned,omitempty"` // "l12" when tagged [learned l12]
	Why      string `json:"why"`
	Fix      string `json:"fix"`
	Test     string `json:"test"`
}

var (
	findingsHeadRe = regexp.MustCompile(`^##\s+Findings\s*$`)
	sectionRe      = regexp.MustCompile(`^##\s`)
	blockStartRe   = regexp.MustCompile(`^###\s+F\d`)
	// ### F1 [Major] path/to/file.go:42 [learned l12]   (a line range 42-48 keeps 42)
	blockHeadRe = regexp.MustCompile(`^###\s+(F\d+)\s+\[([A-Za-z]+)\]\s+(\S(?:.*\S)?):(\d+)(?:-\d+)?(?:\s+\[learned\s+(l\d+)\])?\s*$`)
	fieldRe     = regexp.MustCompile(`^(WHY|FIX|TEST):\s?(.*)$`)
	noneRe      = regexp.MustCompile(`(?i)^\s*none\.?\s*$`)
)

var severities = map[string]string{"blocker": "Blocker", "major": "Major", "minor": "Minor"}

// ParseFindings reads a protocol final message. It returns an empty, non-nil
// slice for "None." and an error when the message does not follow the format.
// A single malformed block fails the whole parse: a partial list would read
// as a complete one.
func ParseFindings(text string) ([]Finding, error) {
	lines := strings.Split(strings.ReplaceAll(text, "\r\n", "\n"), "\n")
	start := -1
	for i, l := range lines {
		if findingsHeadRe.MatchString(strings.TrimSpace(l)) {
			if start >= 0 {
				return nil, errors.New(`more than one "## Findings" heading`)
			}
			start = i + 1
		}
	}
	if start < 0 {
		return nil, errors.New(`no "## Findings" heading`)
	}
	findings := []Finding{}
	var cur *Finding
	var field *string
	sawNone := false
	inFence := false
	seen := map[string]bool{}
	finish := func() error {
		if cur == nil {
			return nil
		}
		cur.Why, cur.Fix, cur.Test = strings.TrimSpace(cur.Why), strings.TrimSpace(cur.Fix), strings.TrimSpace(cur.Test)
		for _, f := range []struct{ name, val string }{{"WHY", cur.Why}, {"FIX", cur.Fix}, {"TEST", cur.Test}} {
			if f.val == "" {
				return fmt.Errorf("%s has no %s", cur.ID, f.name)
			}
		}
		findings = append(findings, *cur)
		cur, field = nil, nil
		return nil
	}
	for _, raw := range lines[start:] {
		l := strings.TrimRight(raw, " \t")
		trimmed := strings.TrimSpace(l)
		if strings.HasPrefix(trimmed, "```") {
			inFence = !inFence
		}
		if !inFence && sectionRe.MatchString(trimmed) {
			break // the next section ("## Not fixed") ends the findings
		}
		if !inFence && blockStartRe.MatchString(trimmed) {
			if err := finish(); err != nil {
				return nil, err
			}
			m := blockHeadRe.FindStringSubmatch(trimmed)
			if m == nil {
				return nil, fmt.Errorf("malformed finding heading %q (want: ### F1 [Major] path:line)", trimmed)
			}
			sev, ok := severities[strings.ToLower(m[2])]
			if !ok {
				return nil, fmt.Errorf("%s: severity %q is not Blocker, Major or Minor", m[1], m[2])
			}
			line, err := strconv.Atoi(m[4])
			if err != nil || line < 1 {
				return nil, fmt.Errorf("%s: line %q is not a line number", m[1], m[4])
			}
			if seen[m[1]] {
				return nil, fmt.Errorf("duplicate finding id %s", m[1])
			}
			seen[m[1]] = true
			cur = &Finding{ID: m[1], Severity: sev, File: m[3], Line: line, Learned: m[5]}
			continue
		}
		if cur == nil {
			switch {
			case trimmed == "":
			case noneRe.MatchString(trimmed):
				sawNone = true
			default:
				return nil, fmt.Errorf("text outside a finding block: %q", trimmed)
			}
			continue
		}
		if m := fieldRe.FindStringSubmatch(trimmed); m != nil && !inFence {
			switch m[1] {
			case "WHY":
				field = &cur.Why
			case "FIX":
				field = &cur.Fix
			default:
				field = &cur.Test
			}
			if *field != "" {
				return nil, fmt.Errorf("%s has %s twice", cur.ID, m[1])
			}
			*field = m[2]
			continue
		}
		if field == nil {
			if trimmed == "" {
				continue
			}
			return nil, fmt.Errorf("%s: text before WHY: %q", cur.ID, trimmed)
		}
		*field += "\n" + l
	}
	if inFence {
		return nil, errors.New("unclosed code fence in the findings")
	}
	if err := finish(); err != nil {
		return nil, err
	}
	if len(findings) == 0 && !sawNone {
		return nil, errors.New(`no findings and no "None." under "## Findings"`)
	}
	if len(findings) > 0 && sawNone {
		return nil, errors.New(`both findings and "None." under "## Findings"`)
	}
	return findings, nil
}
