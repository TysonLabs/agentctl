package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strconv"

	"github.com/TysonLabs/agentctl/internal/flow/branch"
)

const branchUsage = `agentflow branch sync [flags]

sync fetches the base branch and reports how the current branch relates to
it: commits ahead and behind, the incoming commits, and the files changed on
both sides since the merge base (where conflicts will come from). With
--merge it also merges the base into the current branch. It never rebases,
never pushes and never forces.

  --dir DIR             the checkout to sync (default: current directory)
  --base REF            branch to merge from: REMOTE/BRANCH or BRANCH (origin
                        assumed). Default: origin's default branch as the
                        remote reports it, else refs/remotes/origin/HEAD
  --merge               merge the base in with git merge --no-edit --ff
  --abort-on-conflict   with --merge: if the merge stops, abort it and only
                        report the conflicts (default: leave it in progress)
  --max-incoming N      list at most N incoming commits (default 20; the
                        total is always reported)

--merge refuses, listing every reason, when tracked files have uncommitted
changes, a merge, rebase, cherry-pick or revert is in progress, HEAD is
detached, the current branch is the base branch itself, or the merge would
overwrite untracked files. Untracked files alone do not refuse: git never
overwrites them in a merge.

On a conflict the merge is left in progress and "conflicts" lists each path
with its kind (both modified, both added, both deleted, added by us/them,
deleted by us/them). Resolve, git add, git commit (or git merge --abort).

Output: a JSON result on stdout; "next" says what to do when there is
something to do. Exit codes: 0 up to date, merged cleanly, or report only ·
1 usage · 2 refused or the merge stopped (conflicts) · 3 git error ·
130 interrupted.
`

var branchExitCodes = struct{ ok, usage, refused, gitErr, interrupted int }{0, 1, 2, 3, 130}

type branchResult struct {
	branch.Report
	Merge    *branch.MergeResult `json:"merge,omitempty"`
	Refusals []string            `json:"refusals,omitempty"`
	Next     string              `json:"next,omitempty"`
}

func runBranch(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprint(stdout, branchUsage)
		return 0
	}
	if args[0] != "sync" {
		fmt.Fprintf(stderr, "agentflow branch: unknown subcommand %q\n\n%s", args[0], branchUsage)
		return branchExitCodes.usage
	}
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow branch sync: "+format+"\n", a...)
		return code
	}
	fs := flag.NewFlagSet("agentflow branch sync", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		dir, base        string
		merge, abortStop bool
		maxIncoming      int
	)
	fs.StringVar(&dir, "dir", "", "")
	fs.StringVar(&base, "base", "", "")
	fs.BoolVar(&merge, "merge", false, "")
	fs.BoolVar(&abortStop, "abort-on-conflict", false, "")
	fs.IntVar(&maxIncoming, "max-incoming", 20, "")
	if err := fs.Parse(args[1:]); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, branchUsage)
			return 0
		}
		return fail(branchExitCodes.usage, "%v (see: agentflow branch --help)", err)
	}
	if fs.NArg() > 0 {
		return fail(branchExitCodes.usage, "unexpected argument %q", fs.Arg(0))
	}
	if abortStop && !merge {
		return fail(branchExitCodes.usage, "--abort-on-conflict needs --merge")
	}
	if maxIncoming < 0 {
		return fail(branchExitCodes.usage, "--max-incoming must not be negative")
	}
	if dir == "" {
		dir, _ = os.Getwd()
	}
	if !gitWorkTree(ctx, dir) {
		if ctx.Err() != nil {
			return fail(branchExitCodes.interrupted, "interrupted: %v", ctx.Err())
		}
		return fail(branchExitCodes.usage, "%s is not inside a git checkout (use --dir)", dir)
	}
	gitFail := func(err error) int {
		if ctx.Err() != nil {
			return fail(branchExitCodes.interrupted, "interrupted: %v", err)
		}
		return fail(branchExitCodes.gitErr, "%v", err)
	}

	env := branch.Env{Git: os.Getenv("AGENTFLOW_GIT")}
	b, err := branch.ResolveBase(ctx, env, dir, base)
	if errors.Is(err, branch.ErrNoDefault) {
		return fail(branchExitCodes.usage, "%v", err)
	}
	if err != nil {
		return gitFail(err)
	}
	baseSHA, err := branch.Fetch(ctx, env, dir, b)
	if err != nil {
		return gitFail(fmt.Errorf("cannot fetch %s: %w", b.Ref(), err))
	}
	rep, err := branch.Compare(ctx, env, dir, b, baseSHA, maxIncoming)
	if err != nil {
		return gitFail(err)
	}
	res := branchResult{Report: rep}
	code := branchExitCodes.ok
	switch {
	case !merge && !rep.UpToDate:
		res.Next = "agentflow branch sync --merge to merge the " + strconv.Itoa(rep.Behind) + " incoming commit(s)"
	case merge:
		code = branchMerge(ctx, env, dir, b, &res, abortStop, stderr)
		if code == -1 {
			return branchExitCodes.gitErr
		}
	}
	out, _ := json.MarshalIndent(res, "", "  ")
	_, _ = stdout.Write(append(out, '\n'))
	return code
}

// branchMerge runs the refusal checks and the merge, filling res. It
// returns the exit code, or -1 after printing a git error (no JSON).
func branchMerge(ctx context.Context, env branch.Env, dir string, b branch.Base, res *branchResult, abortStop bool, stderr io.Writer) int {
	refusals, err := branch.Refusals(ctx, env, dir, res.Report, b)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintf(stderr, "agentflow branch sync: interrupted: %v\n", err)
			return branchExitCodes.interrupted
		}
		fmt.Fprintf(stderr, "agentflow branch sync: %v\n", err)
		return -1
	}
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "agentflow branch sync: interrupted before the merge started")
		return branchExitCodes.interrupted
	}
	if len(refusals) > 0 {
		res.Refusals = refusals
		res.Next = "fix every refusal, then rerun agentflow branch sync --merge"
		for _, r := range refusals {
			fmt.Fprintf(stderr, "agentflow branch sync: refused: %s\n", r)
		}
		return branchExitCodes.refused
	}
	if res.UpToDate {
		return branchExitCodes.ok
	}
	// Not cancellable: killing git mid-merge leaves index.lock and a half state.
	m, err := branch.Merge(context.WithoutCancel(ctx), env, dir, res.Report, b, abortStop)
	if err != nil {
		if ctx.Err() != nil {
			fmt.Fprintf(stderr, "agentflow branch sync: interrupted after git merge finished: %v\n", err)
			return branchExitCodes.interrupted
		}
		fmt.Fprintf(stderr, "agentflow branch sync: %v\n", err)
		return -1
	}
	res.Merge = &m
	if ctx.Err() != nil {
		fmt.Fprintln(stderr, "agentflow branch sync: interrupted after git merge finished; the merge result is reported")
		return branchExitCodes.interrupted
	}
	switch m.Outcome {
	case branch.Merged:
		return branchExitCodes.ok
	case branch.TrackedChanges:
		res.Refusals = []string{"tracked files changed after the preflight check (git stopped before changing anything): commit or stash them first"}
		res.Next = "commit or stash the tracked files git names in git_output, then rerun agentflow branch sync --merge"
		fmt.Fprintf(stderr, "agentflow branch sync: refused: %s\n", res.Refusals[0])
		return branchExitCodes.refused
	case branch.Overwrite:
		res.Refusals = []string{"the merge would overwrite untracked files (git stopped before changing anything): move them aside first"}
		res.Next = "move or delete the untracked files git names in git_output, then rerun agentflow branch sync --merge"
		fmt.Fprintf(stderr, "agentflow branch sync: refused: %s\n", res.Refusals[0])
		return branchExitCodes.refused
	}
	if m.Aborted {
		res.Next = "the merge was aborted and the branch is unchanged; merge without --abort-on-conflict to resolve the conflicts"
	} else if len(m.Conflicts) > 0 {
		res.Next = "resolve each conflict, git add the files, git commit (or git merge --abort)"
	} else {
		res.Next = "the merge stopped before committing (see git_output); review the staged result and git commit (or git merge --abort)"
	}
	fmt.Fprintf(stderr, "agentflow branch sync: merge stopped with %d conflict(s)\n", len(m.Conflicts))
	return branchExitCodes.refused
}
