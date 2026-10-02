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
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	return list, nil
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
	if r, err := filepath.EvalSymlinks(p); err == nil {
		return r
	}
	return filepath.Clean(p)
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
		remote, branch, ok := strings.Cut(into, "/")
		if !ok {
			return Target{Remote: "origin", Branch: into}, nil
		}
		return Target{Remote: remote, Branch: branch}, nil
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

	via, err := mergeProof(ctx, env, w, t)
	if err != nil {
		return c, err
	}
	if via == "" {
		refuse("not merged: %s is not contained in %s and no merged PR has exactly this head", short(w.Head), t.Ref())
	}
	c.MergedVia = via
	c.OK = len(c.Refusals) == 0
	return c, nil
}

// mergeProof returns how the tip is known to be merged, or "".
func mergeProof(ctx context.Context, env Env, w Worktree, t Target) (string, error) {
	cmd := exec.CommandContext(ctx, env.Git, "merge-base", "--is-ancestor", w.Head, t.Ref())
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
	num, err := mergedPR(ctx, env, w.Path, w.Branch, w.Head)
	if err != nil || num == "" {
		return "", err
	}
	return "PR #" + num, nil
}

// mergedPR returns the number of a merged PR whose head is exactly
// branch@head, or "" (also when gh is unavailable or there is no branch).
func mergedPR(ctx context.Context, env Env, dir, branch, head string) (string, error) {
	if branch == "" || env.GH == "" {
		return "", nil
	}
	out, err := run(ctx, dir, env.GH, "pr", "list", "--head", branch, "--state", "merged",
		"--json", "number,headRefOid", "--jq", `.[] | "\(.number) \(.headRefOid)"`)
	if err != nil {
		return "", err
	}
	for _, line := range nonEmptyLines(out) {
		num, oid, _ := strings.Cut(strings.TrimSpace(line), " ")
		if strings.EqualFold(oid, head) {
			return num, nil
		}
	}
	return "", nil
}

// longLived names branches that are never deleted, whatever the proof.
var longLived = map[string]bool{
	"main": true, "master": true, "trunk": true, "develop": true, "development": true,
	"dev": true, "staging": true, "production": true, "prod": true,
}

// keepBranch says why a branch must survive its worktree, or "".
func keepBranch(branch string, t Target) string {
	switch {
	case branch == "":
		return ""
	case branch == t.Branch:
		return "it is the merge target"
	case longLived[branch], strings.HasPrefix(branch, "release/"), strings.HasPrefix(branch, "hotfix/"):
		return "it looks long-lived"
	}
	return ""
}

// cwdUsers lists processes ("name (pid N)") whose working directory is
// inside dir.
func cwdUsers(ctx context.Context, env Env, dir string) ([]string, error) {
	cmd := exec.CommandContext(ctx, env.Lsof, "-d", "cwd", "-Fpcn")
	out, err := cmd.Output()
	var ee *exec.ExitError
	if err != nil && !(errors.As(err, &ee) && len(out) > 0) {
		// lsof exits 1 when some processes can't be read; keep what it found.
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
	r.FreedBytes = diskUsage(c.Path)
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
	// The merge was proven above; `-d` would still refuse a squash-merged
	// branch, so delete only if the ref still points at the proven head.
	if _, err := run(ctx, repoDir, env.Git, "update-ref", "-d", "refs/heads/"+c.Branch, c.Head); err != nil {
		r.addNote("local branch not deleted: " + err.Error())
	} else {
		r.BranchDeleted = true
	}
	if !deleteRemote {
		return r, nil
	}
	// A remote branch goes only when it was the head of a merged PR: being
	// contained in the target proves nothing about whether others use it.
	pr, err := mergedPR(ctx, env, repoDir, c.Branch, c.Head)
	if err != nil {
		r.addNote("remote branch kept: " + err.Error())
		return r, nil
	}
	if pr == "" {
		r.addNote("remote branch kept: no merged PR has it as head")
		return r, nil
	}
	out, err := run(ctx, repoDir, env.Git, "ls-remote", "--heads", t.Remote, "refs/heads/"+c.Branch)
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
		if _, err := run(ctx, repoDir, env.Git, "push", "--quiet", t.Remote, "--delete", c.Branch); err != nil {
			r.addNote("remote branch not deleted: " + err.Error())
		} else {
			r.RemoteDeleted = true
		}
	}
	return r, nil
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
