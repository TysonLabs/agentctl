package codex

import (
	"bytes"
	"errors"
	"fmt"
	"os/exec"
	"slices"
	"strings"
)

// ErrEmptyDiff means the requested scope has no changes. It is never a clean
// review: there was nothing to look at.
var ErrEmptyDiff = errors.New("the requested scope has no changes to review")

// TooLargeError means the prompt exceeds the size cap. Large single prompts
// stall codex in pure reasoning; split the review with --path instead.
type TooLargeError struct{ Size, Max int }

func (e *TooLargeError) Error() string {
	return fmt.Sprintf("prompt is %d bytes, over the %d-byte cap: split the review with --path (one pass per area) or raise --max-prompt-bytes", e.Size, e.Max)
}

// BuildPrompt returns the prompt to send. Without a scope it is the user's
// prompt unchanged. With a scope, the scoped diff is inlined below it, since
// `codex exec review` cannot combine a scope with custom instructions.
func BuildPrompt(dir, userPrompt string, s Scope, maxBytes int) (string, error) {
	prompt := userPrompt
	if s.IsSet() {
		diff, label, err := scopedDiff(dir, s)
		if err != nil {
			return "", err
		}
		if strings.TrimSpace(diff) == "" {
			return "", ErrEmptyDiff
		}
		var b strings.Builder
		b.WriteString(strings.TrimRight(userPrompt, "\n"))
		b.WriteString("\n\n## Diff under review (" + label + ")\n\n")
		b.WriteString("Review only the changes in this diff. Other files in the repository exist and compile; ")
		b.WriteString("read them only to check a claim about this diff, and say which file you checked.\n\n")
		b.WriteString("```diff\n")
		b.WriteString(diff)
		if !strings.HasSuffix(diff, "\n") {
			b.WriteString("\n")
		}
		b.WriteString("```\n")
		prompt = b.String()
	}
	if maxBytes > 0 && len(prompt) > maxBytes {
		return "", &TooLargeError{Size: len(prompt), Max: maxBytes}
	}
	return prompt, nil
}

// scopedDiff renders the diff for a scope, with renames detected (-M) so a
// moved file is not shown as a delete plus an add.
func scopedDiff(dir string, s Scope) (diff, label string, err error) {
	base := []string{"-C", dir, "diff", "--no-color", "--no-ext-diff", "-M"}
	var rangeArgs []string
	switch {
	case s.Base != "":
		rangeArgs = []string{s.Base + "...HEAD"}
		label = s.Base + "...HEAD"
	case s.Commit != "":
		rangeArgs = []string{s.Commit + "^!"}
		label = "commit " + s.Commit
	case s.Uncommitted:
		rangeArgs = []string{"HEAD"}
		label = "uncommitted changes"
	}
	args := append(append(base, rangeArgs...), "--")
	args = append(args, s.Paths...)
	out, err := git(args...)
	if err != nil {
		return "", "", err
	}
	if len(s.Paths) > 0 {
		label += " limited to " + strings.Join(s.Paths, " ")
	}
	if !s.Uncommitted {
		return out, label, nil
	}
	// `git diff HEAD` omits untracked files; new files are often the point.
	lsArgs := append([]string{"-C", dir, "ls-files", "--others", "--exclude-standard", "-z", "--"}, s.Paths...)
	list, err := git(lsArgs...)
	if err != nil {
		return "", "", err
	}
	var b strings.Builder
	b.WriteString(out)
	for _, f := range strings.Split(list, "\x00") {
		if f == "" {
			continue
		}
		// --no-index exits 1 when the files differ, which they always do here.
		d, err := git("-C", dir, "diff", "--no-color", "--no-ext-diff", "--no-index", "--", "/dev/null", f)
		var ee *exec.ExitError
		if err != nil && !(errors.As(err, &ee) && ee.ExitCode() == 1) {
			return "", "", err
		}
		b.WriteString(d)
	}
	return b.String(), label, nil
}

// git runs git and returns stdout. On failure the error carries stderr.
func git(args ...string) (string, error) {
	cmd := exec.Command("git", args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 1 && slices.Contains(args, "--no-index") {
			return stdout.String(), err
		}
		return "", fmt.Errorf("git %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
