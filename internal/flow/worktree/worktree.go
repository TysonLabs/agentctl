// Package worktree removes finished git worktrees safely: only after
// proving the branch is merged, the tree has no work that would be lost,
// and nobody is using it. It never forces anything. Ignored files (build
// output such as target/) are deleted with the worktree, and so are untracked
// files under .claude/ (agent scratch: briefs, notes); any other untracked or
// modified file makes it refuse.
//
// Merge proof is either "GitHub shows a merged PR whose head branch and
// head are exactly this branch and tip" (into whatever base that PR has;
// squash and rebase merges leave no ancestry), or, only when the caller
// names the target explicitly (--into), "the tip is contained in the
// fetched target branch". It never guesses a target branch.
package worktree

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"
)

// Worktree is one entry of `git worktree list --porcelain`.
type Worktree struct {
	Path       string
	Head       string
	Branch     string // short name; "" when detached
	Locked     bool
	LockReason string
	Main       bool // the repository's main working tree
	Bare       bool // a bare repository's entry: no working tree
}

// Env holds the external commands, so tests can substitute them.
type Env struct {
	Git  string // default "git"
	GH   string // default "gh"; "" after defaults means unavailable
	Lsof string // default "lsof"
	// GHTimeout bounds each gh call (default DefaultGHTimeout).
	GHTimeout time.Duration
}

func (e Env) withDefaults() Env {
	if e.Git == "" {
		e.Git = "git"
	}
	if e.GH == "" {
		if p, err := exec.LookPath("gh"); err == nil {
			e.GH = p
		}
	}
	if e.Lsof == "" {
		e.Lsof = "lsof"
	}
	return e
}

func run(ctx context.Context, dir, bin string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Dir = dir
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), fmt.Errorf("%s %s: %v: %s", filepath.Base(bin), strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}

// List returns the worktrees of the repository containing dir.
func List(ctx context.Context, env Env, dir string) ([]Worktree, error) {
	env = env.withDefaults()
	out, err := run(ctx, dir, env.Git, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, err
	}
	var list []Worktree
	var cur *Worktree
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		line := sc.Text()
		key, val, _ := strings.Cut(line, " ")
		switch key {
		case "worktree":
			list = append(list, Worktree{Path: val, Main: len(list) == 0})
			cur = &list[len(list)-1]
		case "HEAD":
			if cur != nil {
				cur.Head = val
			}
		case "branch":
			if cur != nil {
				cur.Branch = strings.TrimPrefix(val, "refs/heads/")
			}
		case "bare":
			if cur != nil {
				cur.Bare = true
			}
		case "locked":
			if cur != nil {
				cur.Locked, cur.LockReason = true, val
			}
		}
	}
	return list, sc.Err()
}

// Find resolves a worktree by path (absolute or relative to dir), by
// branch name, or by the worktree directory's base name.
func Find(list []Worktree, dir, target string) (Worktree, error) {
	abs := target
	if !filepath.IsAbs(abs) {
		abs = filepath.Join(dir, target)
	}
	abs = canonical(abs)
	var matches []Worktree
	for _, w := range list {
		if canonical(w.Path) == abs || (w.Branch != "" && w.Branch == target) || filepath.Base(w.Path) == target {
			matches = append(matches, w)
		}
	}
	switch len(matches) {
	case 0:
		return Worktree{}, fmt.Errorf("no worktree matches %q (see: git worktree list)", target)
	case 1:
		return matches[0], nil
	default:
		return Worktree{}, fmt.Errorf("%q matches %d worktrees; pass the path", target, len(matches))
	}
}

func canonical(p string) string {
	p = filepath.Clean(p)
	cur := p
	var suffix []string
	for {
		if r, err := filepath.EvalSymlinks(cur); err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				r = filepath.Join(r, suffix[i])
			}
			return r
		}
		parent := filepath.Dir(cur)
		if parent == cur {
			return p
		}
		suffix = append(suffix, filepath.Base(cur))
		cur = parent
	}
}

// Target is the branch merges are checked against by ancestry, e.g.
// "origin/main". Branch is "" when the caller named none (--into absent):
// then only a merged PR proves a merge. Default is origin's HEAD branch,
// used only to protect it from deletion, never to judge "merged".
type Target struct {
	Remote  string
	Branch  string
	Default string
}

// Ref is "REMOTE/BRANCH", or "" when no target branch was named.
func (t Target) Ref() string {
	if t.Branch == "" {
		return ""
	}
	return t.Remote + "/" + t.Branch
}

// ResolveTarget is the target for done and sweep: the --into branch if
// given, else none (Branch "") on origin. Either way Default is origin's
// current HEAD branch, used only to protect it from deletion.
func ResolveTarget(ctx context.Context, env Env, dir, into string) (Target, error) {
	env = env.withDefaults()
	t := Target{Remote: "origin"}
	if into != "" {
		var err error
		if t, err = DefaultTarget(ctx, env, dir, into); err != nil {
			return Target{}, err
		}
	}
	out, err := run(ctx, dir, env.Git, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return Target{}, fmt.Errorf("%w: resolve origin's default branch for deletion protection: %v", ErrRemote, err)
	}
	for _, line := range nonEmptyLines(out) {
		if rest, ok := strings.CutPrefix(line, "ref: refs/heads/"); ok {
			if branch, ref, ok := strings.Cut(rest, "\t"); ok && branch != "" && ref == "HEAD" {
				t.Default = branch
				break
			}
		}
	}
	if t.Default == "" {
		return Target{}, fmt.Errorf("%w: cannot tell origin's default branch from `git ls-remote --symref origin HEAD`; refusing because it could be deleted", ErrRemote)
	}
	return t, nil
}

// DefaultTarget is origin's HEAD branch (origin/main), or the one named.
func DefaultTarget(ctx context.Context, env Env, dir, into string) (Target, error) {
	env = env.withDefaults()
	if into != "" {
		out, err := run(ctx, dir, env.Git, "remote")
		if err != nil {
			return Target{}, err
		}
		remotes := nonEmptyLines(out)
		sort.Slice(remotes, func(i, j int) bool { return len(remotes[i]) > len(remotes[j]) })
		for _, remote := range remotes {
			if branch, ok := strings.CutPrefix(into, remote+"/"); ok {
				if branch == "" {
					return Target{}, fmt.Errorf("--into %q has no branch", into)
				}
				return Target{Remote: remote, Branch: branch}, nil
			}
		}
		return Target{Remote: "origin", Branch: into}, nil
	}
	out, err := run(ctx, dir, env.Git, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
	if err != nil {
		return Target{}, errors.New("cannot tell origin's default branch (refs/remotes/origin/HEAD is unset): pass --into, or run `git remote set-head origin --auto`")
	}
	ref := strings.TrimPrefix(strings.TrimSpace(out), "refs/remotes/")
	remote, branch, _ := strings.Cut(ref, "/")
	return Target{Remote: remote, Branch: branch}, nil
}

// Fetch updates the target branch's remote-tracking ref (nothing to do
// without a target branch).
func Fetch(ctx context.Context, env Env, dir string, t Target) error {
	env = env.withDefaults()
	if t.Branch == "" {
		return nil
	}
	_, err := run(ctx, dir, env.Git, "fetch", "--quiet", t.Remote, "+refs/heads/"+t.Branch+":refs/remotes/"+t.Ref())
	return err
}

// Check is the verdict on one worktree.
type Check struct {
	Path      string   `json:"path"`
	Branch    string   `json:"branch,omitempty"`
	Head      string   `json:"head"`
	OK        bool     `json:"ok"`
	MergedVia string   `json:"merged_via,omitempty"` // "PR #123 into development" or "contained in origin/main"
	Refusals  []string `json:"refusals,omitempty"`
	Prunable  bool     `json:"prunable,omitempty"` // directory gone: only a stale record is left
	// KeepBranch says why the branch survives removal ("" = it is deleted).
	KeepBranch string `json:"keep_branch,omitempty"`
	// The PR found for the branch: the merged proof, else the one that
	// explains the refusal (open, closed, or merged at another head).
	PRNumber int    `json:"pr_number,omitempty"`
	PRState  string `json:"pr_state,omitempty"`
	PRBase   string `json:"pr_base,omitempty"`
	// PRError says why GitHub could not be checked (gh missing, failed or
	// timed out). It never counts as a pass.
	PRError string `json:"pr_error,omitempty"`
	// Owner is the recorded creator (worktree new / adopt); "" = unknown.
	Owner string `json:"owner,omitempty"`
	// ScratchFiles are untracked files under .claude/ (agent briefs and
	// notes, paths relative to the worktree). They do not block removal and
	// are deleted with the worktree.
	ScratchFiles []string `json:"scratch_files,omitempty"`

	prProof    bool   // PRNumber is a merged PR with exactly this branch and head
	prHeadRepo string // that PR's head repository (owner/name), "" = unknown
}

// ErrRemote means the remote could not be read (an external-command error,
// not a usage error).
var ErrRemote = errors.New("remote unavailable")

// ErrRefused means safety changed after Inspect and removal was not attempted.
var ErrRefused = errors.New("worktree removal refused")

// Inspect decides whether w can be removed. It never changes anything.
// target must have been fetched already.
func Inspect(ctx context.Context, env Env, w Worktree, t Target) (Check, error) {
	env = env.withDefaults()
	c := Check{Path: w.Path, Branch: w.Branch, Head: w.Head, KeepBranch: keepBranch(w.Branch, t, "")}
	refuse := func(format string, a ...any) { c.Refusals = append(c.Refusals, fmt.Sprintf(format, a...)) }

	if w.Main {
		refuse("this is the repository's main working tree")
		return c, nil
	}
	if w.Locked {
		refuse("locked (%s)%s: another session may own it; if it is yours, `git worktree unlock` it first", w.LockReason, lockOwnerState(w.LockReason))
	}
	if _, err := os.Stat(w.Path); errors.Is(err, fs.ErrNotExist) {
		refuse("the worktree directory is gone; only a stale record is left (`git worktree prune`, or sweep --yes, removes it)")
		c.Prunable = !w.Locked
		return c, nil
	} else if err != nil {
		return c, err
	}

	dirty, err := run(ctx, w.Path, env.Git, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return c, err
	}
	root, err := os.OpenRoot(w.Path)
	if err != nil {
		return c, err
	}
	defer root.Close()
	var lost []string
	for _, l := range nonEmptyLines(dirty) {
		if f, ok := scratchFile(l); ok {
			fi, err := root.Lstat(filepath.FromSlash(f))
			switch {
			case err == nil && fi.Mode().IsRegular():
				c.ScratchFiles = append(c.ScratchFiles, f)
			case err == nil, errors.Is(err, fs.ErrNotExist):
				lost = append(lost, l)
			default:
				return c, err
			}
		} else {
			lost = append(lost, l)
		}
	}
	if len(lost) > 0 {
		refuse("%d modified or untracked file(s) would be lost, e.g. %s", len(lost), strings.TrimSpace(lost[0]))
	}
	contained, err := containedRefusals(ctx, env, w.Path)
	if err != nil {
		return c, err
	}
	c.Refusals = append(c.Refusals, contained...)

	if err := mergeProof(ctx, env, w, t, &c); err != nil {
		return c, err
	}
	c.KeepBranch = keepBranch(w.Branch, t, c.PRBase)

	// Keep this last so Remove's second Inspect leaves the smallest possible
	// window for another process to enter after the in-use check.
	if users := inUseRefusal(ctx, env, w.Path); users != "" {
		refuse("%s", users)
	}
	c.OK = len(c.Refusals) == 0
	return c, nil
}

// scratchDir holds agent scratch (briefs, notes) that a finished worktree may
// lose: Claude Code sessions write it, and it is never part of the change.
const scratchDir = ".claude/"

// scratchFile reports whether a `git status --porcelain` line is an untracked
// regular-file candidate under scratchDir, and returns its path. Git quotes
// paths containing spaces and other special characters, so decode its
// C-style quoting before classifying the path. Inspect verifies the file type.
// A directory entry ("?? dir/", e.g. a nested repository) never counts.
func scratchFile(line string) (string, bool) {
	f, ok := strings.CutPrefix(line, "?? ")
	if !ok {
		return "", false
	}
	if strings.HasPrefix(f, `"`) {
		var err error
		if f, err = strconv.Unquote(f); err != nil {
			return "", false
		}
	}
	if !strings.HasPrefix(f, scratchDir) || strings.HasSuffix(f, "/") ||
		len(f) == len(scratchDir) || strings.Contains(f, "/../") || strings.HasSuffix(f, "/..") {
		return "", false
	}
	return f, true
}

// containedRefusals lists what removing the tree at path would delete that
// its git status does not show: registered worktrees, initialized
// submodules and other Git repositories nested beneath it.
func containedRefusals(ctx context.Context, env Env, path string) ([]string, error) {
	var refusals []string
	nested, err := nestedWorktrees(ctx, env, path)
	if err != nil {
		return nil, err
	}
	if len(nested) > 0 {
		refusals = append(refusals, "contains registered worktree(s) that would be deleted: "+strings.Join(nested, ", "))
	}
	subs, err := initializedSubmodules(ctx, env, path)
	if err != nil {
		return nil, err
	}
	if len(subs) > 0 {
		refusals = append(refusals, "contains initialized submodule(s), which git cannot remove without --force: "+strings.Join(subs, ", "))
	}
	repos, err := nestedRepositories(path, append(append([]string(nil), nested...), subs...))
	if err != nil {
		return nil, err
	}
	if len(repos) > 0 {
		refusals = append(refusals, "contains nested Git repository/repositories that would be deleted: "+strings.Join(repos, ", "))
	}
	return refusals, nil
}

// inUseRefusal returns a refusal when a process has its working directory
// inside path, or when that cannot be checked (fail closed).
func inUseRefusal(ctx context.Context, env Env, path string) string {
	users, err := cwdUsers(ctx, env, path)
	if err != nil {
		return fmt.Sprintf("cannot check whether a process is using it (%v)", err)
	}
	if len(users) == 0 {
		return ""
	}
	verb := "has its working directory"
	if len(users) > 1 {
		verb = "have their working directories"
	}
	return fmt.Sprintf("in use: %s %s inside; leave it first (a Claude Code session: ExitWorktree or end it), then run done from the main checkout", strings.Join(users, ", "), verb)
}

func nestedWorktrees(ctx context.Context, env Env, root string) ([]string, error) {
	list, err := List(ctx, env, root)
	if err != nil {
		return nil, err
	}
	root = canonical(root)
	var nested []string
	for _, other := range list {
		path := canonical(other.Path)
		if path == root || !pathWithin(root, path) {
			continue
		}
		if _, err := os.Stat(other.Path); err == nil {
			nested = append(nested, other.Path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
	}
	return nested, nil
}

// nestedRepositories finds Git metadata belonging to repositories beneath the
// worktree. A nested repository can be ignored by the parent, in which case a
// plain `git worktree remove` recursively deletes it without warning.
func nestedRepositories(root string, excluded []string) ([]string, error) {
	root = filepath.Clean(root)
	skip := make(map[string]bool, len(excluded))
	for _, path := range excluded {
		if !filepath.IsAbs(path) {
			path = filepath.Join(root, path)
		}
		skip[canonical(path)] = true
	}
	var nested []string
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Name() == ".git" {
			if filepath.Dir(path) == root {
				if d.IsDir() {
					return filepath.SkipDir
				}
				return nil
			}
			ok, err := isGitMetadata(path, d)
			if err != nil {
				return err
			}
			if ok && !skip[canonical(filepath.Dir(path))] {
				nested = append(nested, filepath.Dir(path))
			}
			if ok && d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		// A bare repository has its metadata at its root instead of beneath a
		// .git entry. Looking only at HEAD keeps this check linear in tree size.
		if d.Name() == "HEAD" && !d.IsDir() && filepath.Dir(path) != root {
			ok, err := isGitDir(filepath.Dir(path))
			if err != nil {
				return err
			}
			if ok && !skip[canonical(filepath.Dir(path))] {
				nested = append(nested, filepath.Dir(path))
			}
		}
		return nil
	})
	return nested, err
}

func isGitMetadata(path string, d fs.DirEntry) (bool, error) {
	if d.IsDir() {
		return isGitDir(path)
	}
	if !d.Type().IsRegular() {
		return false, nil
	}
	f, err := os.Open(path)
	if err != nil {
		return false, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return false, err
	}
	if len(b) > 4096 {
		return false, nil
	}
	target, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir: ")
	if !ok || target == "" {
		return false, nil
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(filepath.Dir(path), target)
	}
	return isGitDir(filepath.Clean(target))
}

func isGitDir(path string) (bool, error) {
	head, err := os.ReadFile(filepath.Join(path, "HEAD"))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	headText := strings.TrimSpace(string(head))
	if !strings.HasPrefix(headText, "ref: refs/") && !isHexOID(headText) {
		return false, nil
	}
	for _, marker := range []string{"objects", "commondir"} {
		if _, err := os.Stat(filepath.Join(path, marker)); err == nil {
			return true, nil
		} else if !errors.Is(err, fs.ErrNotExist) {
			return false, err
		}
	}
	return false, nil
}

func isHexOID(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return true
}

func initializedSubmodules(ctx context.Context, env Env, dir string) ([]string, error) {
	out, err := run(ctx, dir, env.Git, "submodule", "status", "--recursive")
	if err != nil {
		return nil, err
	}
	var initialized []string
	for _, line := range nonEmptyLines(out) {
		if line[0] == '-' { // An uninitialized submodule does not block worktree removal.
			continue
		}
		fields := strings.Fields(line[1:])
		if len(fields) > 1 {
			initialized = append(initialized, fields[1])
		}
	}
	return initialized, nil
}

func pathWithin(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != "." && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

func resolveCommit(ctx context.Context, env Env, dir, ref string) (string, error) {
	out, err := run(ctx, dir, env.Git, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	return strings.TrimSpace(out), err
}

// mergeProof sets c.MergedVia (or adds the "not merged" refusal) and the
// PR fields. A merged PR whose head branch and head are exactly w's proves
// the merge into that PR's base, whatever the base. Ancestry is checked
// only against an explicitly named target (t.Branch). A gh failure is
// recorded in c.PRError and never proves anything.
func mergeProof(ctx context.Context, env Env, w Worktree, t Target, c *Check) error {
	refuse := func(format string, a ...any) { c.Refusals = append(c.Refusals, fmt.Sprintf(format, a...)) }
	var detail string
	if w.Branch != "" {
		prs, err := branchPRs(ctx, env, w.Path, t.Remote, w.Branch)
		if err != nil {
			c.PRError = err.Error()
		} else {
			proof, related, why := prVerdict(prs, w.Head)
			switch {
			case proof != nil:
				c.PRNumber, c.PRState, c.PRBase = proof.Number, proof.State, proof.Base
				c.prProof, c.prHeadRepo = true, proof.HeadRepo
				c.MergedVia = fmt.Sprintf("PR #%d into %s", proof.Number, proof.Base)
				return nil
			case related != nil:
				c.PRNumber, c.PRState, c.PRBase = related.Number, related.State, related.Base
				detail = why
			}
		}
	}
	if t.Branch != "" {
		targetHead, err := resolveCommit(ctx, env, w.Path, t.Ref())
		if err != nil {
			return fmt.Errorf("resolve fetched target %s: %w", t.Ref(), err)
		}
		cmd := exec.CommandContext(ctx, env.Git, "merge-base", "--is-ancestor", w.Head, targetHead)
		cmd.Dir = w.Path
		err = cmd.Run()
		var ee *exec.ExitError
		switch {
		case err == nil:
			c.MergedVia = "contained in " + t.Ref()
			return nil
		case errors.As(err, &ee) && ee.ExitCode() == 1:
			// Not an ancestor.
		default:
			return fmt.Errorf("git merge-base: %v", err)
		}
	}
	// Not proven. Say why, naming the PR that explains it if there is one.
	var why []string
	if t.Branch != "" {
		why = append(why, fmt.Sprintf("not merged: %s is not contained in %s", short(w.Head), t.Ref()))
	} else {
		why = append(why, "not merged")
	}
	switch {
	case c.PRError != "":
		why = append(why, "GitHub could not be checked for a merged PR ("+c.PRError+")")
	case detail != "":
		why = append(why, detail)
	case w.Branch == "":
		why = append(why, "a detached HEAD has no PR to check")
	default:
		why = append(why, fmt.Sprintf("GitHub has no merged PR from branch %s with head %s", w.Branch, short(w.Head)))
	}
	if t.Branch == "" {
		why = append(why, "to prove the merge by ancestry instead, pass --into <branch> (no default branch is assumed)")
	}
	refuse("%s", strings.Join(why, "; "))
	return nil
}

var scpRemoteRe = regexp.MustCompile(`^(?:[^@/]+@)?([^:/]+):/?(.+)$`)

// githubRemote returns gh's HOST/OWNER/REPO form and the repository owner.
// Local/file remotes return ok=false; callers still pass their raw URL to gh
// so repository selection is always explicit and a real gh fails closed.
func githubRemote(ctx context.Context, env Env, dir, remote string) (repo, owner string, ok bool) {
	env = env.withDefaults()
	out, err := run(ctx, dir, env.Git, "remote", "get-url", remote)
	if err != nil {
		return "", "", false
	}
	raw := strings.TrimSpace(out)
	var host, path string
	if m := scpRemoteRe.FindStringSubmatch(raw); m != nil && !strings.Contains(raw, "://") {
		host, path = m[1], m[2]
	} else if i := strings.Index(raw, "://"); i >= 0 {
		rest := raw[i+3:]
		if at := strings.LastIndex(strings.SplitN(rest, "/", 2)[0], "@"); at >= 0 {
			rest = rest[at+1:]
		}
		host, path, ok = strings.Cut(rest, "/")
		if !ok || strings.EqualFold(host, "file") {
			return "", "", false
		}
	} else {
		return "", "", false
	}
	parts := strings.Split(strings.TrimSuffix(strings.Trim(path, "/"), ".git"), "/")
	if len(parts) != 2 || host == "" || parts[0] == "" || parts[1] == "" {
		return "", "", false
	}
	return host + "/" + parts[0] + "/" + parts[1], parts[0], true
}

// longLived names branches that are never deleted, whatever the proof.
var longLived = map[string]bool{
	"main": true, "master": true, "trunk": true, "develop": true, "development": true,
	"dev": true, "staging": true, "production": true, "prod": true,
}

// keepBranch says why a branch must survive its worktree, or "". prBase
// is the base of the PR that proved the merge ("" = none).
func keepBranch(branch string, t Target, prBase string) string {
	lower := strings.ToLower(branch)
	switch {
	case branch == "":
		return ""
	case branch == t.Branch, branch == prBase:
		return "it is the merge target"
	case branch == t.Default:
		return "it is the remote's default branch"
	case longLived[lower], strings.HasPrefix(lower, "release/"), strings.HasPrefix(lower, "hotfix/"):
		return "it looks long-lived"
	}
	return ""
}

// cwdIgnored names processes that may sit in a worktree without using it.
// Claude Code runs `caffeinate -i -t 300` in the session's directory and
// starts a new one every 5 minutes while the session works, so a session
// that merged from inside the worktree always had one there. caffeinate
// holds no file in it, and removing its working directory harms nothing.
var cwdIgnored = map[string]bool{"caffeinate": true}

// cwdUsers lists processes ("name (pid N)") whose working directory is
// inside dir, except the ones in cwdIgnored.
func cwdUsers(ctx context.Context, env Env, dir string) ([]string, error) {
	cmd := exec.CommandContext(ctx, env.Lsof, "-d", "cwd", "-Fpcn")
	out, err := cmd.Output()
	if err != nil {
		// Partial output cannot prove that lsof saw every process.
		return nil, err
	}
	root := canonical(dir)
	self := os.Getpid()
	var users []string
	var pid int
	var name string
	for _, line := range nonEmptyLines(string(out)) {
		switch line[0] {
		case 'p':
			pid, _ = strconv.Atoi(line[1:])
			name = ""
		case 'c':
			name = line[1:]
		case 'n':
			p := line[1:]
			if pid == self || cwdIgnored[name] {
				continue
			}
			if p == root || strings.HasPrefix(p, root+string(filepath.Separator)) ||
				canonical(p) == root || strings.HasPrefix(canonical(p), root+string(filepath.Separator)) {
				users = append(users, fmt.Sprintf("%s (pid %d)", name, pid))
			}
		}
	}
	return users, nil
}

var lockPidRe = regexp.MustCompile(`\bpid (\d+)\b`)

// lockOwnerState says whether the pid named in a lock reason is alive.
func lockOwnerState(reason string) string {
	m := lockPidRe.FindStringSubmatch(reason)
	if m == nil {
		return ""
	}
	pid, _ := strconv.Atoi(m[1])
	if err := syscall.Kill(pid, 0); err == nil || errors.Is(err, syscall.EPERM) {
		return "; its owner pid " + m[1] + " is running"
	}
	return "; its owner pid " + m[1] + " is gone, so the lock is probably stale"
}

// Prune drops records of worktrees whose directories are gone (git skips
// locked ones). It never touches a directory that exists.
func Prune(ctx context.Context, env Env, repoDir string) error {
	env = env.withDefaults()
	_, err := run(ctx, repoDir, env.Git, "worktree", "prune")
	return err
}

// Removed reports what Remove did.
type Removed struct {
	FreedBytes    int64 `json:"freed_bytes"`
	BranchDeleted bool  `json:"branch_deleted"`
	RemoteDeleted bool  `json:"remote_deleted"`
	// ScratchDeleted lists the untracked .claude/ files deleted with it.
	ScratchDeleted []string `json:"scratch_deleted,omitempty"`
	Note           string   `json:"note,omitempty"`
}

// Remove deletes a worktree that Inspect passed, then its branches. It
// never forces: if git still refuses, that error is returned.
func Remove(ctx context.Context, env Env, repoDir string, c Check, t Target, deleteRemote bool) (Removed, error) {
	env = env.withDefaults()
	var r Removed
	if !c.OK {
		return r, errors.New("refusing to remove a worktree that did not pass its checks")
	}
	// This can be slow for large ignored build trees, so do it before the
	// final safety inspection rather than widening the check/remove race.
	r.FreedBytes = diskUsage(c.Path)
	list, err := List(ctx, env, repoDir)
	if err != nil {
		return r, err
	}
	var current *Worktree
	for i := range list {
		if canonical(list[i].Path) == canonical(c.Path) {
			current = &list[i]
			break
		}
	}
	if current == nil {
		return r, fmt.Errorf("%w: worktree record disappeared", ErrRefused)
	}
	if current.Head != c.Head || current.Branch != c.Branch {
		return r, fmt.Errorf("%w: HEAD or branch changed after inspection", ErrRefused)
	}
	fresh, err := Inspect(ctx, env, *current, t)
	if err != nil {
		return r, err
	}
	if !fresh.OK {
		return r, fmt.Errorf("%w: safety changed after inspection: %s", ErrRefused, strings.Join(fresh.Refusals, "; "))
	}
	root, err := os.OpenRoot(c.Path)
	if err != nil {
		return r, err
	}
	defer root.Close()
	// git worktree remove refuses untracked files, so delete the scratch the
	// fresh inspection found. A file that appears after it still makes git
	// refuse below.
	for _, f := range fresh.ScratchFiles {
		name := filepath.FromSlash(f)
		p := filepath.Join(c.Path, name)
		if !pathWithin(c.Path, p) {
			return r, fmt.Errorf("%w: scratch path %s escapes the worktree", ErrRefused, f)
		}
		fi, err := root.Lstat(name)
		if errors.Is(err, fs.ErrNotExist) {
			continue
		}
		if err != nil {
			return r, err
		}
		if !fi.Mode().IsRegular() {
			return r, fmt.Errorf("%w: scratch path %s is not a file", ErrRefused, f)
		}
		if err := root.Remove(name); err != nil && !errors.Is(err, fs.ErrNotExist) {
			return r, err
		}
		r.ScratchDeleted = append(r.ScratchDeleted, f)
	}
	if _, err := run(ctx, repoDir, env.Git, "worktree", "remove", c.Path); err != nil {
		return r, err
	}
	_, _ = run(ctx, repoDir, env.Git, "worktree", "prune")
	if c.Branch == "" {
		return r, nil
	}
	if keep := firstNonEmpty(fresh.KeepBranch, c.KeepBranch); keep != "" {
		r.addNote("branch " + c.Branch + " kept: " + keep)
		return r, nil
	}
	if branchWorktree, err := worktreeForBranch(ctx, env, repoDir, c.Branch); err != nil {
		r.addNote("local branch not deleted: " + err.Error())
		return r, nil
	} else if branchWorktree != "" {
		r.addNote("local branch kept: now checked out at " + branchWorktree)
		return r, nil
	}
	// The merge was proven above; `-d` would still refuse a squash-merged
	// branch, so delete only if the ref still points at the proven head.
	if _, err := run(ctx, repoDir, env.Git, "update-ref", "--no-deref", "-d", "refs/heads/"+c.Branch, c.Head); err != nil {
		r.addNote("local branch not deleted: " + err.Error())
	} else {
		r.BranchDeleted = true
	}
	if !deleteRemote {
		return r, nil
	}
	// A remote branch goes only when it was the head of a merged PR: being
	// contained in the target proves nothing about whether others use it.
	// fresh is the re-inspection just before removal, so its PR proof is
	// the latest GitHub answer.
	if !fresh.prProof {
		why := "no merged PR has it as head"
		if fresh.PRError != "" {
			why = "GitHub could not be checked (" + fresh.PRError + ")"
		}
		r.addNote("remote branch kept: " + why)
		return r, nil
	}
	if repo, _, ok := githubRemote(ctx, env, repoDir, t.Remote); ok && !prHeadMatchesRemote(fresh.prHeadRepo, repo) {
		r.addNote("remote branch kept: merged PR head belongs to a different or unknown repository")
		return r, nil
	}
	remoteURL, err := matchingFetchPushURL(ctx, env, repoDir, t.Remote)
	if err != nil {
		r.addNote("remote branch kept: " + err.Error())
		return r, nil
	}
	out, err := run(ctx, repoDir, env.Git, "ls-remote", "--heads", remoteURL, "refs/heads/"+c.Branch)
	if err != nil {
		r.addNote("remote branch not checked: " + err.Error())
		return r, nil
	}
	remoteHead, _, _ := strings.Cut(strings.TrimSpace(out), "\t")
	switch {
	case remoteHead == "":
		// Already gone (e.g. deleted by the PR merge).
	case !strings.EqualFold(remoteHead, c.Head):
		r.addNote("remote branch kept: it points at " + short(remoteHead) + ", not the merged head")
	default:
		lease := "--force-with-lease=refs/heads/" + c.Branch + ":" + c.Head
		if _, err := run(ctx, repoDir, env.Git, "push", "--quiet", lease, remoteURL, ":refs/heads/"+c.Branch); err != nil {
			r.addNote("remote branch not deleted: " + err.Error())
		} else {
			r.RemoteDeleted = true
		}
	}
	return r, nil
}

func matchingFetchPushURL(ctx context.Context, env Env, repoDir, remote string) (string, error) {
	fetch, err := run(ctx, repoDir, env.Git, "remote", "get-url", remote)
	if err != nil {
		return "", err
	}
	push, err := run(ctx, repoDir, env.Git, "remote", "get-url", "--push", "--all", remote)
	if err != nil {
		return "", err
	}
	fetchURL := strings.TrimSpace(fetch)
	pushURLs := nonEmptyLines(push)
	if len(pushURLs) != 1 || pushURLs[0] != fetchURL {
		return "", fmt.Errorf("remote %s does not have one push URL identical to its fetch URL", remote)
	}
	return fetchURL, nil
}

func prHeadMatchesRemote(headRepo, ghRepo string) bool {
	_, nameWithOwner, ok := strings.Cut(ghRepo, "/")
	return ok && headRepo != "" && strings.EqualFold(headRepo, nameWithOwner)
}

func worktreeForBranch(ctx context.Context, env Env, repoDir, branch string) (string, error) {
	list, err := List(ctx, env, repoDir)
	if err != nil {
		return "", err
	}
	for _, w := range list {
		if w.Branch == branch {
			return w.Path, nil
		}
	}
	return "", nil
}

func firstNonEmpty(a ...string) string {
	for _, s := range a {
		if s != "" {
			return s
		}
	}
	return ""
}

func (r *Removed) addNote(s string) {
	if r.Note != "" {
		r.Note += "; "
	}
	r.Note += s
}

// diskUsage sums file sizes under dir (best effort, without following links).
func diskUsage(dir string) int64 {
	var total int64
	_ = filepath.WalkDir(dir, func(_ string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total
}

func nonEmptyLines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.TrimSpace(l) != "" {
			out = append(out, l)
		}
	}
	return out
}

func short(sha string) string {
	if len(sha) > 8 {
		return sha[:8]
	}
	return sha
}
