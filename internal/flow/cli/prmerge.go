package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/pr"
	"github.com/TysonLabs/agentctl/internal/flow/worktree"
)

var prMergeExitCodes = map[pr.MergeStatus]int{
	pr.MergeMerged:      0,
	pr.MergeGHError:     1,
	pr.MergeRefused:     2,
	pr.MergeSyncFailed:  3,
	pr.MergeUnconfirmed: 4,
	pr.MergeInterrupted: 130,
}

var fullSHARe = regexp.MustCompile(`^[0-9a-fA-F]{40}$`)

func runPRMerge(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fail := func(format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow pr merge: "+format+"\n", a...)
		return 1
	}
	fs := flag.NewFlagSet("agentflow pr merge", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	o := pr.MergeOptions{
		MergeableWait: 30 * time.Second, MergeableInterval: 3 * time.Second,
		ConfirmWait: 20 * time.Second, ConfirmInterval: 2 * time.Second,
	}
	fs.StringVar(&o.Repo, "repo", "", "")
	fs.StringVar(&o.Method, "method", "merge", "")
	fs.StringVar(&o.Head, "head", "", "")
	fs.BoolVar(&o.Admin, "admin", false, "")
	fs.StringVar(&o.SyncBranch, "sync-branch", "", "")
	teardown := fs.Bool("teardown", false, "")
	format := formatFlag(fs)
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, prUsage)
			return 0
		}
		return fail("%v (see: agentflow pr --help)", err)
	}
	if len(pos) != 1 {
		return fail("want exactly one PR number, got %d", len(pos))
	}
	n, err := strconv.Atoi(strings.TrimPrefix(pos[0], "#"))
	if err != nil || n <= 0 {
		return fail("PR number %q is not a positive integer", pos[0])
	}
	o.PR = n
	switch o.Method {
	case "merge", "squash", "rebase":
	default:
		return fail("--method %q is not merge, squash or rebase", o.Method)
	}
	if o.Head != "" && !fullSHARe.MatchString(o.Head) {
		return fail("--head %q is not a full 40-character commit sha", o.Head)
	}
	if o.SyncBranch != "" && !pr.ValidBranch(o.SyncBranch) {
		return fail("--sync-branch %q is not a plain branch name", o.SyncBranch)
	}
	bin := os.Getenv("AGENTFLOW_GH")
	if bin == "" {
		if bin, err = exec.LookPath("gh"); err != nil {
			return fail("gh not found: put it on PATH or set AGENTFLOW_GH")
		}
	}
	o.GH = pr.GHCLI(bin, 60*time.Second)
	if o.Repo == "" {
		// Never assume a repo: merging the same number in the wrong repo is
		// not undoable.
		out, err := o.GH(ctx, "repo", "view", "--json", "nameWithOwner")
		if err != nil {
			if ctx.Err() != nil {
				fmt.Fprintln(stderr, "agentflow pr merge: interrupted")
				return prMergeExitCodes[pr.MergeInterrupted]
			}
			return fail("no --repo and the current directory has no GitHub repo: %v", err)
		}
		var rv struct {
			NameWithOwner string `json:"nameWithOwner"`
		}
		if json.Unmarshal(out, &rv) != nil || rv.NameWithOwner == "" {
			return fail("no --repo and gh repo view gave no name")
		}
		o.Repo = rv.NameWithOwner
	}
	owner, name, _ := strings.Cut(o.Repo, "/")
	if !repoRe.MatchString(o.Repo) || owner == "." || owner == ".." || name == "." || name == ".." {
		return fail("--repo %q is not OWNER/NAME", o.Repo)
	}

	res := pr.Merge(ctx, o)
	var td *teardownResult
	if *teardown {
		td = teardownMerged(ctx, res)
	}
	out, _ := json.MarshalIndent(struct {
		pr.MergeResult
		Teardown *teardownResult `json:"teardown,omitempty"`
	}{res, td}, "", "  ")
	code, ok := prMergeExitCodes[res.Status]
	if !ok {
		code = 1
	}
	emit(stdout, *format, append(out, '\n'), func() string { return mergeText(res, td, code) })
	if td != nil && (td.Status == teardownRefused || td.Status == teardownError) {
		fmt.Fprintf(stderr, "agentflow pr merge: teardown %s: %s\n", td.Status, td.Reason)
	}
	if !ok {
		fmt.Fprintf(stderr, "agentflow pr merge: unknown result status %q\n", res.Status)
		return code
	}
	if code != 0 {
		msg := res.Next
		switch {
		case res.Error != "":
			msg = res.Error
		case len(res.Reasons) > 0:
			msg = strings.Join(res.Reasons, "; ")
		case res.Sync != nil && res.Sync.Error != "":
			msg = res.Sync.Error
		}
		fmt.Fprintf(stderr, "agentflow pr merge: %s: %s\n", res.Status, msg)
	}
	return code
}

// Teardown statuses. A teardown never changes the merge's exit code: the PR
// is merged either way; the JSON and stderr say what happened to its worktree.
const (
	teardownRemoved = "removed" // the head branch's worktree and branch are gone
	teardownRefused = "refused" // worktree done refused (reason says why); nothing removed
	teardownNone    = "none"    // no worktree of this checkout has the head branch
	teardownSkipped = "skipped" // not merged, head branch unknown or not in a checkout
	teardownError   = "error"   // worktree done could not run or failed
)

type teardownResult struct {
	Branch string          `json:"branch,omitempty"`
	Status string          `json:"status"`
	Reason string          `json:"reason,omitempty"`
	Done   json.RawMessage `json:"done,omitempty"` // worktree done's own JSON
}

// teardownMerged runs worktree done for a merged PR's head branch in the
// current directory's checkout, so the session that merges also removes the
// worktree (subagents that made it have ended by then). Run it from the main
// checkout: a process inside the worktree makes done refuse.
func teardownMerged(ctx context.Context, res pr.MergeResult) *teardownResult {
	td := &teardownResult{Branch: res.HeadBranch}
	if res.Status != pr.MergeMerged && res.Status != pr.MergeSyncFailed {
		td.Status, td.Reason = teardownSkipped, "the PR is not confirmed merged"
		return td
	}
	if res.HeadBranch == "" {
		td.Status, td.Reason = teardownSkipped, "GitHub did not report the head branch"
		return td
	}
	dir, err := os.Getwd()
	if err != nil || !gitWorkTree(ctx, dir) {
		td.Status, td.Reason = teardownSkipped, "the current directory is not a git checkout"
		return td
	}
	list, err := worktree.List(ctx, worktreeEnv(), dir)
	if err != nil {
		td.Status, td.Reason = teardownError, err.Error()
		return td
	}
	found := false
	for _, w := range list {
		if !w.Main && w.Branch == res.HeadBranch {
			found = true
			break
		}
	}
	if !found {
		td.Status = teardownNone
		return td
	}
	var out, errOut bytes.Buffer
	code := runWorktree(ctx, []string{"done", res.HeadBranch, "--repo", dir, "--format", "json"}, &out, &errOut)
	if json.Valid(out.Bytes()) {
		td.Done = json.RawMessage(bytes.TrimSpace(out.Bytes()))
	}
	switch code {
	case 0:
		td.Status = teardownRemoved
	case 2:
		td.Status, td.Reason = teardownRefused, strings.TrimSpace(errOut.String())
	default:
		td.Status, td.Reason = teardownError, strings.TrimSpace(errOut.String())
	}
	return td
}
