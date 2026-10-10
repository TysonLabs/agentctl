package gate

import (
	"context"
	"fmt"
	"regexp"
	"strings"
	"time"
)

// CheckStep is one step's line in a --check result.
type CheckStep struct {
	Name   string `json:"name"`
	Status string `json:"status"` // passed · failed · stale (run changed) · missing
	At     string `json:"at,omitempty"`
}

// CheckResult is the JSON of agentflow gate --check.
type CheckResult struct {
	Status   Status      `json:"status"`
	Project  string      `json:"project,omitempty"`
	Dir      string      `json:"dir,omitempty"`
	Tree     string      `json:"tree,omitempty"`
	Dirty    *bool       `json:"dirty,omitempty"` // only for the work tree
	OK       bool        `json:"ok"`
	Steps    []CheckStep `json:"steps"`
	Missing  []string    `json:"missing"`
	Receipts string      `json:"receipts,omitempty"`
	Error    string      `json:"error,omitempty"`
}

var treeHashRe = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// Check reports whether every configured step has a passing receipt, with
// the current run string, for one tree: tree, else rev's tree, else the
// work tree as it is now. It writes no receipt and runs no step.
func Check(ctx context.Context, o Options, tree, rev string) *CheckResult {
	res := &CheckResult{Steps: []CheckStep{}, Missing: []string{}}
	fail := func(err error) *CheckResult {
		res.Status, res.Error = StatusError, err.Error()
		return res
	}
	t, err := resolve(ctx, o.ConfigPath, o.Project, o.Dir, o.DirGiven)
	if err != nil {
		return fail(err)
	}
	res.Project, res.Dir = t.project, t.root
	switch {
	case tree != "":
		tree = strings.ToLower(tree)
		if !treeHashRe.MatchString(tree) {
			return fail(fmt.Errorf("--tree %q is not a full tree hash", tree))
		}
		res.Tree = tree
	case rev != "":
		if res.Tree, err = revTree(ctx, t.root, rev); err != nil {
			return fail(err)
		}
	default:
		ts, err := workTree(ctx, t.root)
		if err != nil {
			return fail(fmt.Errorf("hashing the work tree: %v", err))
		}
		res.Tree, res.Dirty = ts.tree, &ts.dirty
	}
	res.Receipts = receiptsPath(t.common)
	rf, err := readReceipts(res.Receipts)
	if err != nil {
		return fail(err)
	}
	for _, st := range t.spec.Steps {
		cs := CheckStep{Name: st.Name, Status: "missing"}
		if r, ok := rf.lookup(t.project, res.Tree, st.Name); ok {
			cs.At = r.At.UTC().Format(time.RFC3339)
			switch {
			case r.RunHash != runHash(st.Run):
				cs.Status = "stale"
			case r.OK:
				cs.Status = "passed"
			default:
				cs.Status = "failed"
			}
		}
		if cs.Status != "passed" {
			res.Missing = append(res.Missing, st.Name)
		}
		res.Steps = append(res.Steps, cs)
	}
	res.OK = len(res.Missing) == 0
	if res.OK {
		res.Status = StatusPassed
	} else {
		res.Status = StatusMissing
	}
	return res
}
