// Package worktree removes finished git worktrees safely: only after
// proving the branch is merged, the tree has no work that would be lost,
// and nobody is using it. It never forces anything. Ignored files (build
// output such as target/) are deleted with the worktree; untracked or
// modified files make it refuse.
//
// Merge proof is either "the branch tip is contained in the fetched
// target branch" or "GitHub shows a merged PR whose head is exactly this
// tip" (squash and rebase merges leave no ancestry).
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
)

// Worktree is one entry of `git worktree list --porcelain`.
type Worktree struct {
	Path       string
	Head       string
	Branch     string // short name; "" when detached
	Locked     bool
	LockReason string
	Main       bool // the repository's main working tree
}

// Env holds the external commands, so tests can substitute them.
type Env struct {
	Git  string // default "git"
	GH   string // default "gh"; "" after defaults means unavailable
	Lsof string // default "lsof"
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

// Target is the branch merges are checked against, e.g. "origin/main".
type Target struct {
	Remote string
	Branch string
}

func (t Target) Ref() string { return t.Remote + "/" + t.Branch }

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

// Fetch updates the target branch's remote-tracking ref.
func Fetch(ctx context.Context, env Env, dir string, t Target) error {
	env = env.withDefaults()
	_, err := run(ctx, dir, env.Git, "fetch", "--quiet", t.Remote, "+refs/heads/"+t.Branch+":refs/remotes/"+t.Ref())
	return err
}

// Check is the verdict on one worktree.
type Check struct {
	Path      string   `json:"path"`
	Branch    string   `json:"branch,omitempty"`
	Head      string   `json:"head"`
	OK        bool     `json:"ok"`
	MergedVia string   `json:"merged_via,omitempty"` // "contained in origin/main" or "PR #123"
	Refusals  []string `json:"refusals,omitempty"`
	Prunable  bool     `json:"prunable,omitempty"` // directory gone: only a stale record is left
	// KeepBranch says why the branch survives removal ("" = it is deleted).
	KeepBranch string `json:"keep_branch,omitempty"`
}

// ErrRefused means safety changed after Inspect and removal was not attempted.
var ErrRefused = errors.New("worktree removal refused")

// Inspect decides whether w can be removed. It never changes anything.
// target must have been fetched already.
func Inspect(ctx context.Context, env Env, w Worktree, t Target) (Check, error) {
	env = env.withDefaults()
	c := Check{Path: w.Path, Branch: w.Branch, Head: w.Head, KeepBranch: keepBranch(w.Branch, t)}
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
	if lines := nonEmptyLines(dirty); len(lines) > 0 {
		refuse("%d modified or untracked file(s) would be lost, e.g. %s", len(lines), strings.TrimSpace(lines[0]))
	}
	nested, err := nestedWorktrees(ctx, env, w.Path)
	if err != nil {
		return c, err
	}
	if len(nested) > 0 {
		refuse("contains registered worktree(s) that would be deleted: %s", strings.Join(nested, ", "))
	}
	subs, err := initializedSubmodules(ctx, env, w.Path)
	if err != nil {
		return c, err
	}
	if len(subs) > 0 {
		refuse("contains initialized submodule(s), which git cannot remove without --force: %s", strings.Join(subs, ", "))
	}
	repos, err := nestedRepositories(w.Path, append(append([]string(nil), nested...), subs...))
	if err != nil {
		return c, err
	}
	if len(repos) > 0 {
		refuse("contains nested Git repository/repositories that would be deleted: %s", strings.Join(repos, ", "))
	}

	targetHead, err := resolveCommit(ctx, env, w.Path, t.Ref())
	if err != nil {
		return c, fmt.Errorf("resolve fetched target %s: %w", t.Ref(), err)
	}
	via, err := mergeProof(ctx, env, w, t, targetHead)
	if err != nil {
		return c, err
	}
	if via == "" {
		refuse("not merged: %s is not contained in %s and no merged PR has exactly this head", short(w.Head), t.Ref())
	}
	c.MergedVia = via

	// Keep this last so Remove's second Inspect leaves the smallest possible
	// window for another process to enter after the in-use check.
	users, err := cwdUsers(ctx, env, w.Path)
	if err != nil {
		refuse("cannot check whether a process is using it (%v)", err)
	} else if len(users) > 0 {
		verb := "has its working directory"
		if len(users) > 1 {
			verb = "have their working directories"
		}
		refuse("in use: %s %s inside", strings.Join(users, ", "), verb)
	}
	c.OK = len(c.Refusals) == 0
	return c, nil
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

// mergeProof returns how the tip is known to be merged, or "".
func mergeProof(ctx context.Context, env Env, w Worktree, t Target, targetHead string) (string, error) {
	cmd := exec.CommandContext(ctx, env.Git, "merge-base", "--is-ancestor", w.Head, targetHead)
	cmd.Dir = w.Path
	err := cmd.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return "contained in " + t.Ref(), nil
	case errors.As(err, &ee) && ee.ExitCode() == 1:
		// Not an ancestor; maybe squash- or rebase-merged.
	default:
		return "", fmt.Errorf("git merge-base: %v", err)
	}
	pr, err := mergedPR(ctx, env, w.Path, t, w.Branch, w.Head)
	if err != nil || pr.Number == "" {
		return "", err
	}
	return "PR #" + pr.Number, nil
}

type prProof struct {
	Number   string
	HeadRepo string
}

// mergedPR returns a merged PR whose head is exactly branch@head, or an
// empty proof (also when gh is unavailable or there is no branch).
func mergedPR(ctx context.Context, env Env, dir string, t Target, branch, head string) (prProof, error) {
	if branch == "" || env.GH == "" {
		return prProof{}, nil
	}
	args := []string{"pr", "list", "--head", branch, "--base", t.Branch, "--state", "merged",
		"--json", "number,headRefOid,headRepository", "--jq", `.[] | "\(.number) \(.headRefOid) \(.headRepository.nameWithOwner // \"\")"`}
	repo, _, ok := githubRemote(ctx, env, dir, t.Remote)
	if !ok {
		out, err := run(ctx, dir, env.Git, "remote", "get-url", t.Remote)
		if err != nil {
			return prProof{}, err
		}
		repo = strings.TrimSpace(out)
	}
	args = append(args, "--repo", repo)
	out, err := run(ctx, dir, env.GH, args...)
	if err != nil {
		return prProof{}, err
	}
	for _, line := range nonEmptyLines(out) {
		fields := strings.Fields(line)
		if len(fields) >= 2 && strings.EqualFold(fields[1], head) {
			proof := prProof{Number: fields[0]}
			if len(fields) >= 3 {
				proof.HeadRepo = fields[2]
			}
			return proof, nil
		}
	}
	return prProof{}, nil
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

// keepBranch says why a branch must survive its worktree, or "".
func keepBranch(branch string, t Target) string {
	lower := strings.ToLower(branch)
	switch {
	case branch == "":
		return ""
	case branch == t.Branch:
		return "it is the merge target"
	case longLived[lower], strings.HasPrefix(lower, "release/"), strings.HasPrefix(lower, "hotfix/"):
		return "it looks long-lived"
	}
	return ""
}

// cwdUsers lists processes ("name (pid N)") whose working directory is
// inside dir.
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
			if pid == self {
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
	FreedBytes    int64  `json:"freed_bytes"`
	BranchDeleted bool   `json:"branch_deleted"`
	RemoteDeleted bool   `json:"remote_deleted"`
	Note          string `json:"note,omitempty"`
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
	if _, err := run(ctx, repoDir, env.Git, "worktree", "remove", c.Path); err != nil {
		return r, err
	}
	_, _ = run(ctx, repoDir, env.Git, "worktree", "prune")
	if c.Branch == "" {
		return r, nil
	}
	if c.KeepBranch != "" || keepBranch(c.Branch, t) != "" {
		r.addNote("branch " + c.Branch + " kept: " + keepBranch(c.Branch, t))
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
	pr, err := mergedPR(ctx, env, repoDir, t, c.Branch, c.Head)
	if err != nil {
		r.addNote("remote branch kept: " + err.Error())
		return r, nil
	}
	if pr.Number == "" {
		r.addNote("remote branch kept: no merged PR has it as head")
		return r, nil
	}
	if repo, _, ok := githubRemote(ctx, env, repoDir, t.Remote); ok && !prHeadMatchesRemote(pr.HeadRepo, repo) {
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
