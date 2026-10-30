package pr

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// OpenStack reports how pr open --stack placed the PR in a GitHub stack.
type OpenStack struct {
	Parent   int    `json:"parent"`           // the PR of the layer below (its head is the base)
	Number   int    `json:"number,omitempty"` // the stack's number
	Position int    `json:"position,omitempty"`
	Action   string `json:"action,omitempty"` // created, added or already
}

// stackParent is what the pre-push checks learned about the layer below.
type stackParent struct {
	pr    int
	stack *stackInfo // nil: the parent is in no stack yet
}

// checkStackParent runs --stack's preconditions before anything is pushed:
// the base is not the trunk, exactly one open PR has the base as its head
// (the layer below), and that PR is the top of its stack, unless this
// branch's PR already sits right above it (a rerun). Failures are appended
// to reasons; err is a failed read.
func checkStackParent(ctx context.Context, o OpenOptions, base, def string, existing *existingPR, reasons *[]string) (*stackParent, error) {
	if base == def {
		*reasons = append(*reasons, fmt.Sprintf("--stack needs --base: the branch of the layer below (%s is the default branch, the stack's trunk)", def))
		return nil, nil
	}
	parents, err := openPRsForBranch(ctx, o, base)
	if err != nil {
		return nil, err
	}
	if len(parents) != 1 {
		*reasons = append(*reasons, fmt.Sprintf("--stack needs exactly one open PR whose head is %s (the layer below); found %d", base, len(parents)))
		return nil, nil
	}
	p := &stackParent{pr: parents[0].Number}
	p.stack, err = readStack(ctx, o.GH, o.Repo, p.pr)
	if errors.Is(err, errNoStackSchema) {
		*reasons = append(*reasons, "this GitHub has no stacked pull requests")
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if s := p.stack; s != nil && s.Position < s.Size {
		above := s.Entries[s.Position].PR
		if existing == nil || existing.Number != above {
			*reasons = append(*reasons, fmt.Sprintf("PR #%d already has #%d above it in stack #%d (a stack is linear: stack on the top layer)", p.pr, above, s.Number))
		}
	}
	return p, nil
}

// linkStack puts PR number on top of the parent's stack, creating the stack
// when the parent is in none, then reads the PR's stack back. GitHub checks
// that each PR's base is the head of the one below it.
func linkStack(ctx context.Context, o OpenOptions, parent, number int) (*OpenStack, error) {
	st := &OpenStack{Parent: parent}
	mine, err := readStack(ctx, o.GH, o.Repo, number)
	if err != nil {
		return st, err
	}
	if mine != nil {
		st.Number, st.Position = mine.Number, mine.Position
		if mine.Position > 1 && mine.Entries[mine.Position-2].PR == parent {
			st.Action = "already"
			return st, nil
		}
		return st, fmt.Errorf("PR #%d is in stack #%d, but not right above #%d", number, mine.Number, parent)
	}
	ps, err := readStack(ctx, o.GH, o.Repo, parent)
	if err != nil {
		return st, err
	}
	ver := "X-GitHub-Api-Version: " + StackAPIVersion
	var out []byte
	switch {
	case ps == nil:
		out, err = o.GH(ctx, "api", "-X", "POST", fmt.Sprintf("repos/%s/stacks", o.Repo), "-H", ver,
			"-F", fmt.Sprintf("pull_requests[]=%d", parent), "-F", fmt.Sprintf("pull_requests[]=%d", number))
		st.Action = "created"
	case ps.Position == ps.Size:
		out, err = o.GH(ctx, "api", "-X", "POST", fmt.Sprintf("repos/%s/stacks/%d/add", o.Repo, ps.Number), "-H", ver,
			"-F", fmt.Sprintf("pull_requests[]=%d", number))
		st.Action = "added"
	default:
		return st, fmt.Errorf("PR #%d is no longer the top of stack #%d", parent, ps.Number)
	}
	if err != nil {
		return st, fmt.Errorf("%s: %w", st.Action, err)
	}
	var resp struct {
		Number int `json:"number"`
	}
	if json.Unmarshal(out, &resp) != nil || resp.Number <= 0 {
		return st, fmt.Errorf("%s: the response has no stack number", st.Action)
	}
	st.Number = resp.Number
	mine, err = readStack(ctx, o.GH, o.Repo, number)
	if err != nil {
		return st, fmt.Errorf("read-back: %w", err)
	}
	if mine == nil || mine.Number != resp.Number || mine.Position < 2 || mine.Entries[mine.Position-2].PR != parent {
		return st, fmt.Errorf("read-back: PR #%d is not right above #%d in stack #%d", number, parent, resp.Number)
	}
	st.Position = mine.Position
	return st, nil
}
