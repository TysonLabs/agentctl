package cli

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	iofs "io/fs"
	"os"
	"strings"
	"time"

	"github.com/TysonLabs/agentctl/internal/flow/worktree"
)

const worktreeUsage = `agentflow worktree new <branch> [--from REF] [--clone-dir DIR]... [--owner X] [--repo DIR]
agentflow worktree new --scratch [--at REF] [--clone-dir DIR]... [--owner X] [--repo DIR]
agentflow worktree done <branch|path> [flags]
agentflow worktree sweep [flags]
agentflow worktree adopt <branch|path> [--owner X] [--repo DIR]

new fetches origin, then creates <main checkout>/.claude/worktrees/<slug> on
a NEW branch (feat/x -> feat-x) from --from, else origin's default branch
(origin/HEAD, e.g. origin/main). The branch gets no upstream, so a bare
` + "`git push`" + ` can never target main. It refuses (listing every reason) if the
branch exists locally or on origin, or the path exists. Its JSON is
{path, branch, base, base_sha, cloned, clone_skipped, owner, notes}.

  --from REF / --at REF  the base commit (the same option; any ref or sha)
  --clone-dir DIR        repeatable: copy-on-write clone this directory (e.g.
                         target, node_modules) from the main checkout with
                         cp -c -R so builds start warm. macOS (APFS) only;
                         elsewhere, or if cloning fails, it is skipped with
                         a reason and never copied in full.
  --scratch              a detached throwaway worktree, .claude/worktrees/
                         scratch-<random>, for probes and base comparisons.
                         It is locked ("agentflow scratch ...") and marked in
                         its git admin directory.
  --owner X              the creator to record (default: $AGENTFLOW_OWNER,
                         else the base name of $CLAUDE_JOB_DIR, else
                         $CLAUDE_CODE_SESSION_ID, else $CODEX_SESSION_ID)

Owner: new records who created the worktree (owner, source, time) in its
git admin directory, outside the tree. adopt stamps the same record on a
worktree made another way (plain git worktree add); it refuses one that
another owner already holds and is a no-op for the same owner. Its JSON is
{path, branch, owner: {owner, source, via, recorded}, unchanged}. done and
sweep print each worktree's owner ("unknown" in text when none).

done on a scratch worktree removes it WITHOUT the merged and clean checks
(uncommitted files and commits in it are discarded; the JSON's
discarded_head keeps the sha recoverable until git gc), but only when the
scratch marker is present and the unused checks below pass. sweep lists
scratch worktrees under "scratch"; with --yes it removes those older than
--scratch-age (default 24h) that pass. Unmarked worktrees always get the
full checks.

done removes one finished worktree and its branch. It never forces, and it
refuses (listing every reason) unless all of these hold:
  - merged: GitHub shows a MERGED PR of origin's repository whose head
    branch and head commit are exactly the worktree's branch and tip; it
    counts as merged into that PR's base, whatever the base is (a
    non-default base, a retargeted stacked PR, a squash merge). Without
    such a PR, only --into proves a merge, by ancestry: the tip must be
    contained in the fetched --into branch. No default branch is assumed.
    The refusal names an OPEN or CLOSED PR for the branch if there is one.
    A gh failure or timeout is a refusal (pr_error), never a pass.
  - clean: no modified or untracked files (ignored build output such as
    target/ is deleted with the worktree, and so are untracked files under
    .claude/, agent briefs and notes, listed in scratch_files and
    scratch_deleted; a modified tracked .claude/ file still refuses)
  - unused: not locked, no process has its working directory inside, and no
    registered worktree or other Git repository is nested beneath it
  - removable without force: no initialized submodules
  - not the repository's main working tree

After a removal, done runs the repository's cleanup commands, if any (see
Cleanup hook). sweep lists every worktree with its owner, its PR (number,
state, base) and its verdict; with --yes it removes those that pass (only
those of --owner, if given). Records whose directory is gone are listed
under "stale"; sweep --yes prunes them with git worktree prune (locked
ones are kept) and lists what it pruned in stale_pruned.

Branches: a long-lived branch (the merge target or proving PR's base,
origin's default branch, main, master, develop, development, staging,
production, release/*, hotfix/*) is never deleted. The remote branch is
deleted only when a merged PR from that same repository had exactly that
branch and head; being merged by ancestry alone keeps it.

Cleanup hook (opt-in, per repository): each value of the local git config
key agentflow.worktreeCleanup is one command done runs after it removed a
worktree, e.g.
  git config --add agentflow.worktreeCleanup 'scripts/drop-test-db.sh {slug_snake}'
It runs without a shell (words split on spaces; '...' and "..." quote), in
the main checkout, stdin closed, its own process group, each bounded by
--cleanup-timeout. Placeholders, substituted inside each word: {branch},
{slug} (feat/x -> feat-x), {slug_snake} (feat_x), {dir} (the worktree
directory name), {path}. The same values are in AGENTFLOW_WORKTREE_PATH,
_BRANCH and _SLUG. A failure is reported in cleanup[] and on stderr; it
never undoes the removal or changes the exit code. done runs it for scratch
worktrees too ({branch} is empty there). Not run by sweep.

  --repo DIR        any checkout of the repository (default: current dir)
  --into REF        prove merges by ancestry into this branch as well
                    (no default); REMOTE/BRANCH or BRANCH (origin assumed)
  --dry-run         done: check only, change nothing
  --no-cleanup      done: skip the cleanup commands
  --cleanup-timeout DUR  done: bound on each cleanup command (default 2m)
  --yes             sweep: remove the worktrees that pass (default: list only)
  --owner X         sweep: with --yes, remove only worktrees owned by X
                    (others are refused "owned by ..."); new/adopt: the owner
  --scratch-age DUR sweep: minimum age of a scratch worktree to remove
                    (default 24h)
  --keep-remote     don't delete the remote branch (by default it is deleted
                    when it still points at the merged head)
  --format json|text  JSON result (default) or a text summary

Output: a JSON result on stdout. Per worktree: path, branch, head, ok,
merged_via ("PR #N into BASE" or "contained in REMOTE/BRANCH"), pr_number,
pr_state, pr_base, pr_error, owner, refusals, keep_branch, prunable, and
after removal removed {freed_bytes, branch_deleted, remote_deleted, note}.
done adds into ("" without --into), dry_run and cleanup[] ({command, ok,
exit_code, timed_out, error, output, duration_ms}). sweep adds into,
owner_filter, removable, removed, pruned_stale_records, stale[] ({path,
branch, head, owner, locked}), stale_pruned[] (paths), freed_bytes, and
scratch. Sweep records per-worktree errors and keeps going. Exit codes:
0 created/removed/adopted (or would be, or sweep finished) · 1 usage ·
2 refused · 3 git error. An unavailable or incomplete lsof check, and an
unavailable or failing gh, are safety refusals, not external-command errors.
`

func runWorktree(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 || args[0] == "--help" || args[0] == "-h" || args[0] == "help" {
		fmt.Fprint(stdout, worktreeUsage)
		return 0
	}
	sub := args[0]
	switch sub {
	case "new":
		return runWorktreeNew(ctx, args[1:], stdout, stderr)
	case "adopt":
		return runWorktreeAdopt(ctx, args[1:], stdout, stderr)
	case "done", "sweep":
	default:
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
		repo, into, ownerFilter       string
		dryRun, yes, keepRem, noClean bool
		scratchAge, cleanupTimeout    time.Duration
	)
	fs.StringVar(&repo, "repo", "", "")
	fs.StringVar(&into, "into", "", "")
	fs.BoolVar(&keepRem, "keep-remote", false, "")
	format := formatFlag(fs)
	if sub == "done" {
		fs.BoolVar(&dryRun, "dry-run", false, "")
		fs.BoolVar(&noClean, "no-cleanup", false, "")
		fs.DurationVar(&cleanupTimeout, "cleanup-timeout", worktree.DefaultCleanupTimeout, "")
	} else {
		fs.BoolVar(&yes, "yes", false, "")
		fs.DurationVar(&scratchAge, "scratch-age", 24*time.Hour, "")
		fs.StringVar(&ownerFilter, "owner", "", "")
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
	if ownerFilter != "" {
		if err := worktree.ValidOwner(ownerFilter); err != nil {
			return fail(1, "--owner: %v", err)
		}
	}
	if cleanupTimeout < 0 {
		return fail(1, "--cleanup-timeout must not be negative")
	}
	if repo == "" {
		repo, _ = os.Getwd()
	}
	if !gitWorkTree(ctx, repo) {
		return fail(1, "%s is not inside a git checkout (use --repo)", repo)
	}

	env := worktreeEnv()
	list, err := worktree.List(ctx, env, repo)
	if err != nil {
		return fail(3, "%v", err)
	}
	// Keep a surviving repository directory for all work after a removal.
	// The caller may have invoked done from (or with --repo pointing at) the
	// very linked worktree being removed.
	operationRepo := repo
	if len(list) > 0 && list[0].Main {
		operationRepo = list[0].Path
	}
	// Scratch worktrees (agentflow worktree new --scratch) skip the merge
	// and clean checks; only their marker admits them to that path.
	var scratch []worktree.Worktree
	stale := []staleEntry{}
	if sub == "done" {
		if w, err := worktree.Find(list, repo, pos[0]); err == nil && isScratch(ctx, env, repo, w) {
			return scratchDone(ctx, env, operationRepo, w, dryRun, noClean, cleanupTimeout, stdout, stderr)
		}
	} else {
		var ordinary []worktree.Worktree
		for _, w := range list {
			if !w.Main && dirGone(w.Path) {
				stale = append(stale, staleEntry{Path: w.Path, Branch: w.Branch, Head: w.Head,
					Owner: ownerOf(ctx, env, operationRepo, w), Locked: w.Locked})
			}
			if isScratch(ctx, env, repo, w) {
				scratch = append(scratch, w)
			} else {
				ordinary = append(ordinary, w)
			}
		}
		list = ordinary
	}
	target, err := worktree.ResolveTarget(ctx, env, repo, into)
	if err != nil {
		if errors.Is(err, worktree.ErrRemote) {
			return fail(3, "%v", err)
		}
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
		owner := ownerOf(ctx, env, operationRepo, w)
		c, err := worktree.Inspect(ctx, env, w, target)
		c.Owner = owner
		if err != nil {
			if sub == "done" {
				return fail(3, "%s: %v", w.Path, err)
			}
			entries = append(entries, entry{
				Check: worktree.Check{Path: w.Path, Branch: w.Branch, Head: w.Head, Owner: owner},
				Error: err.Error(),
			})
			hadError = true
			continue
		}
		if sub == "sweep" && ownerFilter != "" && !dirGone(w.Path) && owner != ownerFilter {
			c.OK = false
			c.Refusals = append(c.Refusals, fmt.Sprintf("owned by %s, not %s (--owner)", ownerOrUnknown(owner), ownerFilter))
		}
		e := entry{Check: c}
		if c.OK {
			eligible++
			if (sub == "done" && !dryRun) || (sub == "sweep" && yes) {
				r, err := worktree.Remove(ctx, env, operationRepo, c, target, !keepRem)
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

	var scratchEntries []scratchEntry
	scratchRemovable, scratchRemoved := 0, 0
	if sub == "sweep" {
		var sFreed int64
		var sErr bool
		scratchEntries, scratchRemovable, scratchRemoved, sFreed, sErr = sweepScratch(ctx, env, operationRepo, scratch, scratchAge, yes, ownerFilter)
		freed += sFreed
		hadError = hadError || sErr
	}

	stalePruned := []string{}
	var sweepError string
	if sub == "sweep" && yes && len(stale) > 0 {
		if pruned, err := pruneStale(ctx, env, operationRepo, stale); err != nil {
			sweepError = err.Error()
			hadError = true
		} else {
			stalePruned = pruned
		}
	}

	// Cleanup commands run after a successful done removal only.
	var cleanup []worktree.CleanupResult
	if sub == "done" && entries[0].Removed != nil && !noClean {
		cleanup = runCleanupHook(ctx, env, operationRepo, entries[0].Path, entries[0].Branch, cleanupTimeout, stderr)
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
		text = func() string { return worktreeDoneText(textEntries[0], target.Ref(), dryRun, cleanup, code) }
		out, _ = json.MarshalIndent(struct {
			entry
			Into    string                   `json:"into"`
			DryRun  bool                     `json:"dry_run,omitempty"`
			Cleanup []worktree.CleanupResult `json:"cleanup,omitempty"`
		}{entries[0], target.Ref(), dryRun, cleanup}, "", "  ")
	} else {
		text = func() string {
			return worktreeSweepText(textEntries, target.Ref(), eligible, removed, stale, stalePruned, freed, sweepError, yes, code)
		}
		out, _ = json.MarshalIndent(struct {
			Into             string         `json:"into"`
			OwnerFilter      string         `json:"owner_filter,omitempty"`
			Removable        int            `json:"removable"`
			Removed          int            `json:"removed"`
			Pruned           int            `json:"pruned_stale_records"`
			FreedBytes       int64          `json:"freed_bytes"`
			Worktrees        []entry        `json:"worktrees"`
			Stale            []staleEntry   `json:"stale"`
			StalePruned      []string       `json:"stale_pruned"`
			ScratchAge       string         `json:"scratch_age"`
			ScratchRemovable int            `json:"scratch_removable"`
			ScratchRemoved   int            `json:"scratch_removed"`
			Scratch          []scratchEntry `json:"scratch"`
			Error            string         `json:"error,omitempty"`
		}{target.Ref(), ownerFilter, eligible, removed, len(stalePruned), freed, entries, stale, stalePruned,
			scratchAge.String(), scratchRemovable, scratchRemoved, scratchEntries, sweepError}, "", "  ")
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

func worktreeEnv() worktree.Env {
	return worktree.Env{Git: os.Getenv("AGENTFLOW_GIT"), GH: os.Getenv("AGENTFLOW_GH"), Lsof: os.Getenv("AGENTFLOW_LSOF")}
}

// staleEntry is a worktree record whose directory is gone.
type staleEntry struct {
	Path   string `json:"path"`
	Branch string `json:"branch,omitempty"`
	Head   string `json:"head"`
	Owner  string `json:"owner,omitempty"`
	Locked bool   `json:"locked,omitempty"` // git worktree prune keeps it
}

func dirGone(path string) bool {
	_, err := os.Stat(path)
	return errors.Is(err, iofs.ErrNotExist)
}

// pruneStale runs git worktree prune and returns the stale records it
// dropped (git decides the scope: missing directories, not locked).
func pruneStale(ctx context.Context, env worktree.Env, repo string, stale []staleEntry) ([]string, error) {
	if err := worktree.Prune(ctx, env, repo); err != nil {
		return nil, err
	}
	after, err := worktree.List(ctx, env, repo)
	if err != nil {
		return nil, err
	}
	still := map[string]bool{}
	for _, w := range after {
		still[w.Path] = true
	}
	pruned := []string{}
	for _, s := range stale {
		if !still[s.Path] {
			pruned = append(pruned, s.Path)
		}
	}
	return pruned, nil
}

// ownerOf reads w's owner; an unreadable record reads as unknown.
func ownerOf(ctx context.Context, env worktree.Env, repo string, w worktree.Worktree) string {
	rec, err := worktree.ReadOwner(ctx, env, repo, w)
	if err != nil || rec == nil {
		return ""
	}
	return rec.Owner
}

func ownerOrUnknown(owner string) string {
	if owner == "" {
		return "unknown"
	}
	return owner
}

// runCleanupHook runs the repository's configured cleanup commands for a
// removed worktree, in the main checkout. Failures are reported, never fatal.
func runCleanupHook(ctx context.Context, env worktree.Env, repo, path, branch string, timeout time.Duration, stderr io.Writer) []worktree.CleanupResult {
	cmds, err := worktree.CleanupCommands(ctx, env, repo)
	if err != nil {
		fmt.Fprintf(stderr, "agentflow worktree done: cleanup not run: %v\n", err)
		return []worktree.CleanupResult{{Command: []string{"git", "config", "--get-all", worktree.CleanupKey}, ExitCode: -1, Error: "cleanup not run: " + err.Error()}}
	}
	if len(cmds) == 0 {
		return nil
	}
	dir := repo
	if list, err := worktree.List(ctx, env, repo); err == nil {
		if main, err := worktree.MainCheckout(list); err == nil {
			dir = main.Path
		}
	}
	results := worktree.RunCleanup(ctx, dir, cmds, worktree.CleanupVars(path, branch), timeout)
	for _, r := range results {
		if !r.OK {
			fmt.Fprintf(stderr, "agentflow worktree done: cleanup failed (the worktree stays removed): %s: %s\n", strings.Join(r.Command, " "), r.Error)
		}
	}
	return results
}

// runWorktreeAdopt implements `agentflow worktree adopt`.
func runWorktreeAdopt(ctx context.Context, args []string, stdout, stderr io.Writer) int {
	fail := func(code int, format string, a ...any) int {
		fmt.Fprintf(stderr, "agentflow worktree adopt: "+format+"\n", a...)
		return code
	}
	fs := flag.NewFlagSet("agentflow worktree adopt", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	var repo, ownerFlag string
	fs.StringVar(&repo, "repo", "", "")
	fs.StringVar(&ownerFlag, "owner", "", "")
	format := formatFlag(fs)
	pos, err := parseInterleaved(fs, args)
	if err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fmt.Fprint(stdout, worktreeUsage)
			return 0
		}
		return fail(1, "%v (see: agentflow worktree --help)", err)
	}
	if len(pos) != 1 {
		return fail(1, "want exactly one branch or worktree path, got %d", len(pos))
	}
	owner, source := worktree.ResolveOwner(ownerFlag, os.Getenv)
	if owner == "" {
		return fail(1, "no owner: pass --owner X (or set AGENTFLOW_OWNER)")
	}
	if err := worktree.ValidOwner(owner); err != nil {
		return fail(1, "%v", err)
	}
	if repo == "" {
		repo, _ = os.Getwd()
	}
	if !gitWorkTree(ctx, repo) {
		return fail(1, "%s is not inside a git checkout (use --repo)", repo)
	}
	env := worktreeEnv()
	list, err := worktree.List(ctx, env, repo)
	if err != nil {
		return fail(3, "%v", err)
	}
	w, err := worktree.Find(list, repo, pos[0])
	if err != nil {
		return fail(1, "%v", err)
	}
	a, err := worktree.Adopt(ctx, env, repo, w, owner, source, time.Now())
	if err != nil {
		var refused *worktree.RefusedError
		if errors.As(err, &refused) {
			out, _ := json.MarshalIndent(struct {
				Path     string   `json:"path"`
				Refusals []string `json:"refusals"`
			}{w.Path, refused.Reasons}, "", "  ")
			emit(stdout, *format, append(out, '\n'), func() string { return worktreeAdoptText(a, refused.Reasons, 2) })
			for _, r := range refused.Reasons {
				fmt.Fprintf(stderr, "agentflow worktree adopt: refused: %s\n", r)
			}
			return 2
		}
		return fail(3, "%v", err)
	}
	out, _ := json.MarshalIndent(a, "", "  ")
	emit(stdout, *format, append(out, '\n'), func() string { return worktreeAdoptText(a, nil, 0) })
	return 0
}
