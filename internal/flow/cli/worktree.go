package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/TysonLabs/agentctl/internal/flow/worktree"
)

const worktreeUsage = `agentflow worktree done <branch|path> [flags]
agentflow worktree sweep [flags]

done removes one finished worktree and its branch. It never forces, and it
refuses (listing every reason) unless all of these hold:
  - merged: the tip is contained in the fetched --into branch, or GitHub
    shows a PR merged into it whose branch and head are exact matches
    (squash merges count)
  - clean: no modified or untracked files (ignored build output such as
    target/ is deleted with the worktree)
  - unused: not locked, no process has its working directory inside, and no
    registered worktree or other Git repository is nested beneath it
  - removable without force: no initialized submodules
  - not the repository's main working tree

sweep lists every worktree that passes the same checks; with --yes it
removes them and prunes records of worktrees whose directories are gone.

Branches: a long-lived branch (the merge target, main, master, develop,
development, staging, production, release/*, hotfix/*) is never deleted.
The remote branch is deleted only when a merged PR from that same repository
had exactly that branch and head; being merged by ancestry alone keeps it.

  --repo DIR        any checkout of the repository (default: current dir)
  --into REF        branch merges must reach (default: origin's HEAD, e.g.
                    origin/main); REMOTE/BRANCH or BRANCH (origin assumed)
  --dry-run         done: check only, change nothing
  --yes             sweep: remove the worktrees that pass (default: list only)
  --keep-remote     don't delete the remote branch (by default it is deleted
                    when it still points at the merged head)
  --format json|text  JSON result (default) or a text summary

Output: a JSON result on stdout. Sweep records per-worktree errors and keeps
going. Exit codes: 0 removed (or would be, or sweep finished) · 1 usage ·
2 refused · 3 git/gh error. An unavailable or incomplete lsof check is a
safety refusal, not an external-command error.
`

func runWorktree(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprint(stdout, worktreeUsage)
		return 0
	}
	sub := args[0]
	if sub != "done" && sub != "sweep" {
		fmt.Fprintf(stderr, "agentflow worktree: unknown subcommand %q\n\n%s", sub, worktreeUsage)
		return 1
	}
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow worktree "+sub+": "+format+"\n", a...)
		return code
	}
	fs := flag.NewFlagSet("agentflow worktree "+sub, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		repo, into           string
		dryRun, yes, keepRem bool
	)
	fs.StringVar(&repo, "repo", "", "")
	fs.StringVar(&into, "into", "", "")
	fs.BoolVar(&keepRem, "keep-remote", false, "")
	format := formatFlag(fs)
	if sub == "done" {
		fs.BoolVar(&dryRun, "dry-run", false, "")
	} else {
		fs.BoolVar(&yes, "yes", false, "")
	}
	pos, err := parseInterleaved(fs, args[1:])
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, worktreeUsage)
			return 0
		}
		return fail(1, "%v (see: agentflow worktree --help)", err)
	}
	if sub == "done" && len(pos) != 1 {
		return fail(1, "want exactly one branch or worktree path, got %d", len(pos))
	}
	if sub == "sweep" && len(pos) != 0 {
		return fail(1, "sweep takes no positional arguments")
	}
	if repo == "" {
		repo, _ = os.Getwd()
	}
	if !gitWorkTree(ctx, repo) {
		return fail(1, "%s is not inside a git checkout (use --repo)", repo)
	}

	env := worktree.Env{Git: os.Getenv("AGENTFLOW_GIT"), GH: os.Getenv("AGENTFLOW_GH"), Lsof: os.Getenv("AGENTFLOW_LSOF")}
	list, err := worktree.List(ctx, env, repo)
	if err != nil {
		return fail(3, "%v", err)
	}
	target, err := worktree.DefaultTarget(ctx, env, repo, into)
	if err != nil {
		return fail(1, "%v", err)
	}

	var picked []worktree.Worktree
	if sub == "done" {
		w, err := worktree.Find(list, repo, pos[0])
		if err != nil {
			return fail(1, "%v", err)
		}
		picked = []worktree.Worktree{w}
	} else {
		for _, w := range list {
			if !w.Main {
				picked = append(picked, w)
			}
		}
	}
	if err := worktree.Fetch(ctx, env, repo, target); err != nil {
		return fail(3, "cannot fetch %s to prove merges: %v", target.Ref(), err)
	}

	type entry struct {
		worktree.Check
		Removed *worktree.Removed `json:"removed,omitempty"`
		Error   string            `json:"error,omitempty"`
	}
	var entries []entry
	var freed int64
	removed, eligible := 0, 0
	hadError := false
	for _, w := range picked {
		c, err := worktree.Inspect(ctx, env, w, target)
		if err != nil {
			if sub == "done" {
				return fail(3, "%s: %v", w.Path, err)
			}
			entries = append(entries, entry{
				Check: worktree.Check{Path: w.Path, Branch: w.Branch, Head: w.Head},
				Error: err.Error(),
			})
			hadError = true
			continue
		}
		e := entry{Check: c}
		if c.OK {
			eligible++
			if (sub == "done" && !dryRun) || (sub == "sweep" && yes) {
				r, err := worktree.Remove(ctx, env, repo, c, target, !keepRem)
				if err != nil {
					if sub == "done" {
						if errors.Is(err, worktree.ErrRefused) {
							return fail(2, "%s: %v", w.Path, err)
						}
						return fail(3, "%s: %v", w.Path, err)
					}
					e.Error = err.Error()
					hadError = true
					if errors.Is(err, worktree.ErrRefused) {
						e.OK = false
						e.Refusals = append(e.Refusals, err.Error())
						eligible--
					}
					entries = append(entries, e)
					continue
				}
				e.Removed = &r
				removed++
				freed += r.FreedBytes
			}
		}
		entries = append(entries, e)
	}

	pruned := 0
	var sweepError string
	if sub == "sweep" && yes {
		for _, e := range entries {
			if e.Prunable {
				pruned++
			}
		}
		if pruned > 0 {
			if err := worktree.Prune(ctx, env, repo); err != nil {
				sweepError = err.Error()
				hadError = true
				pruned = 0
			}
		}
	}

	code := 0
	switch {
	case sub == "done" && !entries[0].OK:
		code = 2
	case hadError:
		code = 3
	}
	textEntries := make([]worktreeEntry, len(entries))
	for i, e := range entries {
		textEntries[i] = worktreeEntry{Check: e.Check, Removed: e.Removed, Error: e.Error}
	}
	var out []byte
	var text func() string
	if sub == "done" {
		text = func() string { return worktreeDoneText(textEntries[0], target.Ref(), dryRun, code) }
		out, _ = json.MarshalIndent(struct {
			entry
			Into   string `json:"into"`
			DryRun bool   `json:"dry_run,omitempty"`
		}{entries[0], target.Ref(), dryRun}, "", "  ")
	} else {
		text = func() string {
			return worktreeSweepText(textEntries, target.Ref(), eligible, removed, pruned, freed, sweepError, yes, code)
		}
		out, _ = json.MarshalIndent(struct {
			Into       string  `json:"into"`
			Removable  int     `json:"removable"`
			Removed    int     `json:"removed"`
			Pruned     int     `json:"pruned_stale_records"`
			FreedBytes int64   `json:"freed_bytes"`
			Worktrees  []entry `json:"worktrees"`
			Error      string  `json:"error,omitempty"`
		}{target.Ref(), eligible, removed, pruned, freed, entries, sweepError}, "", "  ")
	}
	emit(stdout, *format, append(out, '\n'), text)
	if code == 2 {
		for _, r := range entries[0].Refusals {
			fmt.Fprintf(stderr, "agentflow worktree done: refused: %s\n", r)
		}
	} else if code == 3 {
		fmt.Fprintln(stderr, "agentflow worktree sweep: one or more worktrees could not be processed")
	}
	return code
}
