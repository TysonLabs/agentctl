package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/worktree"
)

// runWorktreeNew implements `agentflow worktree new`.
func runWorktreeNew(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow worktree new: "+format+"\n", a...)
		return code
	}
	fs := flag.NewFlagSet("agentflow worktree new", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var (
		repo, from, at string
		scratch        bool
		clones         pathList
	)
	fs.StringVar(&repo, "repo", "", "")
	fs.StringVar(&from, "from", "", "")
	fs.StringVar(&at, "at", "", "")
	fs.BoolVar(&scratch, "scratch", false, "")
	fs.Var(&clones, "clone-dir", "")
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, worktreeUsage)
			return 0
		}
		return fail(1, "%v (see: agentflow worktree --help)", err)
	}
	if from != "" && at != "" {
		return fail(1, "--from and --at are the same option; give one")
	}
	if at != "" {
		from = at
	}
	switch {
	case scratch && len(pos) != 0:
		return fail(1, "--scratch takes no branch: a scratch worktree is detached")
	case !scratch && len(pos) != 1:
		return fail(1, "want exactly one new branch name (or --scratch), got %d", len(pos))
	}
	if repo == "" {
		repo, _ = os.Getwd()
	}
	if !gitWorkTree(ctx, repo) {
		return fail(1, "%s is not inside a git checkout (use --repo)", repo)
	}
	opt := worktree.NewOptions{From: from, Scratch: scratch, CloneDirs: clones}
	if !scratch {
		opt.Branch = pos[0]
	}
	env := worktree.Env{Git: os.Getenv("AGENTFLOW_GIT"), GH: os.Getenv("AGENTFLOW_GH"), Lsof: os.Getenv("AGENTFLOW_LSOF")}
	c, err := worktree.New(ctx, env, repo, opt)
	if err != nil {
		var refused *worktree.RefusedError
		if errors.As(err, &refused) {
			out, _ := json.MarshalIndent(struct {
				Refusals []string `json:"refusals"`
			}{refused.Reasons}, "", "  ")
			_, _ = stdout.Write(append(out, '\n'))
			for _, r := range refused.Reasons {
				fmt.Fprintf(stderr, "agentflow worktree new: refused: %s\n", r)
			}
			return 2
		}
		return fail(3, "%v", err)
	}
	out, _ := json.MarshalIndent(c, "", "  ")
	_, _ = stdout.Write(append(out, '\n'))
	return 0
}

// scratchDone is `worktree done` on a scratch worktree: no merged or clean
// checks, but the marker and unused checks, re-run right before removal.
func scratchDone(ctx context.Context, env worktree.Env, repo string, w worktree.Worktree, dryRun bool, stdout, stderr io.Writer) int {
	now := time.Now()
	c, err := worktree.InspectScratch(ctx, env, repo, w, now, 0)
	if err != nil {
		fmt.Fprintf(stderr, "agentflow worktree done: %s: %v\n", w.Path, err)
		return 3
	}
	e := scratchEntry{ScratchCheck: c}
	code := 0
	if c.OK && !dryRun {
		r, err := worktree.RemoveScratch(ctx, env, repo, c, now, 0)
		switch {
		case errors.Is(err, worktree.ErrRefused):
			e.OK = false
			e.Refusals = append(e.Refusals, err.Error())
		case err != nil:
			fmt.Fprintf(stderr, "agentflow worktree done: %s: %v\n", w.Path, err)
			return 3
		default:
			e.Removed = &r
		}
	}
	out, _ := json.MarshalIndent(struct {
		scratchEntry
		DryRun bool `json:"dry_run,omitempty"`
	}{e, dryRun}, "", "  ")
	_, _ = stdout.Write(append(out, '\n'))
	if !e.OK {
		for _, r := range e.Refusals {
			fmt.Fprintf(stderr, "agentflow worktree done: refused: %s\n", r)
		}
		code = 2
	}
	return code
}

type scratchEntry struct {
	worktree.ScratchCheck
	Removed *worktree.RemovedScratch `json:"removed,omitempty"`
	Error   string                   `json:"error,omitempty"`
}

// sweepScratch inspects (and with yes removes) the scratch worktrees.
func sweepScratch(ctx context.Context, env worktree.Env, repo string, list []worktree.Worktree, minAge time.Duration, yes bool) (entries []scratchEntry, removable, removed int, freed int64, hadError bool) {
	now := time.Now()
	entries = []scratchEntry{}
	for _, w := range list {
		c, err := worktree.InspectScratch(ctx, env, repo, w, now, minAge)
		if err != nil {
			entries = append(entries, scratchEntry{ScratchCheck: worktree.ScratchCheck{Path: w.Path, Head: w.Head, Scratch: true}, Error: err.Error()})
			hadError = true
			continue
		}
		e := scratchEntry{ScratchCheck: c}
		if c.OK {
			removable++
			if yes {
				r, err := worktree.RemoveScratch(ctx, env, repo, c, now, minAge)
				if err != nil {
					e.Error = err.Error()
					hadError = true
					if errors.Is(err, worktree.ErrRefused) {
						e.OK = false
						e.Refusals = append(e.Refusals, err.Error())
						removable--
					}
				} else {
					e.Removed = &r
					removed++
					freed += r.FreedBytes
				}
			}
		}
		entries = append(entries, e)
	}
	return entries, removable, removed, freed, hadError
}

// isScratch says whether w carries a scratch marker, including one that
// cannot be read (which must then be refused, never treated as ordinary).
func isScratch(ctx context.Context, env worktree.Env, repo string, w worktree.Worktree) bool {
	m, err := worktree.ReadScratch(ctx, env, repo, w)
	return m != nil || err != nil
}
