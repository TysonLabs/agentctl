// Package branch reports how the current branch relates to the default
// branch and, on request, merges the default branch into it. It never
// rebases, never pushes, never forces, and refuses to merge over local
// changes. A conflicted merge is left in progress for the caller to resolve
// (or aborted, on request); either way the conflict list is reported.
package branch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
)

// Env holds the external commands, so tests can substitute them.
type Env struct {
	Git string // default "git"
}

func (e Env) git() string {
	if e.Git == "" {
		return "git"
	}
	return e.Git
}

// gitError is a failed git command: its arguments, exit error and stderr.
type gitError struct {
	args   []string
	err    error
	stderr string
}

func (e *gitError) Error() string {
	return fmt.Sprintf("git %s: %v: %s", strings.Join(e.args, " "), e.err, e.stderr)
}

// run runs git in dir with a pinned, non-interactive environment: C locale
// (stable messages), no terminal or editor prompts, verbatim path names.
func (e Env) run(ctx context.Context, dir string, args ...string) (string, error) {
	full := append([]string{"-c", "core.quotePath=false"}, args...)
	cmd := exec.CommandContext(ctx, e.git(), full...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "LC_ALL=C", "LANG=C", "GIT_TERMINAL_PROMPT=0", "GIT_EDITOR=true", "GIT_MERGE_AUTOEDIT=no")
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		return stdout.String(), &gitError{args: args, err: err, stderr: strings.TrimSpace(stderr.String())}
	}
	return stdout.String(), nil
}

// Base is the branch merged from, as a remote-tracking ref.
type Base struct {
	Remote string `json:"-"`
	Branch string `json:"-"`
	Source string `json:"-"` // "--base", "remote HEAD" or "origin/HEAD"
}

// Ref is the remote-tracking ref, e.g. "origin/main".
func (b Base) Ref() string { return b.Remote + "/" + b.Branch }

// ErrNoDefault means origin's default branch could not be derived.
var ErrNoDefault = errors.New("cannot tell origin's default branch: the remote reports no HEAD and refs/remotes/origin/HEAD is unset; pass --base")

// ResolveBase returns the base to merge from. An explicit ref is
// REMOTE/BRANCH (a configured remote) or BRANCH (origin assumed). Otherwise
// the base is origin's default branch as the remote reports it now (a
// clone's refs/remotes/origin/HEAD is set once and goes stale when the
// default branch is renamed), falling back to refs/remotes/origin/HEAD.
// The default branch is never assumed to be main or master.
func ResolveBase(ctx context.Context, env Env, dir, explicit string) (Base, error) {
	if explicit != "" {
		out, err := env.run(ctx, dir, "remote")
		if err != nil {
			return Base{}, err
		}
		remotes := lines(out)
		sort.Slice(remotes, func(i, j int) bool { return len(remotes[i]) > len(remotes[j]) })
		for _, remote := range remotes {
			if branch, ok := strings.CutPrefix(explicit, remote+"/"); ok {
				if branch == "" {
					return Base{}, fmt.Errorf("--base %q has no branch", explicit)
				}
				return Base{Remote: remote, Branch: branch, Source: "--base"}, nil
			}
		}
		return Base{Remote: "origin", Branch: explicit, Source: "--base"}, nil
	}
	out, err := env.run(ctx, dir, "ls-remote", "--symref", "origin", "HEAD")
	if err != nil {
		return Base{}, err
	}
	for _, l := range lines(out) {
		// "ref: refs/heads/main\tHEAD"
		if rest, ok := strings.CutPrefix(l, "ref: refs/heads/"); ok {
			if branch, _, ok := strings.Cut(rest, "\t"); ok && branch != "" {
				return Base{Remote: "origin", Branch: branch, Source: "remote HEAD"}, nil
			}
		}
	}
	out, err = env.run(ctx, dir, "symbolic-ref", "--quiet", "refs/remotes/origin/HEAD")
	if err != nil {
		return Base{}, ErrNoDefault
	}
	if branch, ok := strings.CutPrefix(strings.TrimSpace(out), "refs/remotes/origin/"); ok && branch != "" {
		return Base{Remote: "origin", Branch: branch, Source: "origin/HEAD"}, nil
	}
	return Base{}, ErrNoDefault
}

// Fetch updates the base's remote-tracking ref and returns its commit.
func Fetch(ctx context.Context, env Env, dir string, b Base) (string, error) {
	if _, err := env.run(ctx, dir, "fetch", "--quiet", "--no-tags", b.Remote,
		"+refs/heads/"+b.Branch+":refs/remotes/"+b.Ref()); err != nil {
		return "", err
	}
	return resolve(ctx, env, dir, "refs/remotes/"+b.Ref())
}

func resolve(ctx context.Context, env Env, dir, rev string) (string, error) {
	out, err := env.run(ctx, dir, "rev-parse", "--verify", "--quiet", "--end-of-options", rev+"^{commit}")
	if err != nil {
		return "", fmt.Errorf("cannot resolve %s to a commit: %w", rev, err)
	}
	return strings.TrimSpace(out), nil
}

// Commit is one incoming commit.
type Commit struct {
	SHA     string `json:"sha"`
	Subject string `json:"subject"`
}

// Report is the read-only comparison of HEAD with the base.
type Report struct {
	Dir              string   `json:"dir"`
	Branch           string   `json:"branch"` // "" when HEAD is detached
	Head             string   `json:"head"`
	Base             string   `json:"base"`
	BaseSource       string   `json:"base_source"`
	BaseSHA          string   `json:"base_sha"`
	MergeBase        string   `json:"merge_base"`
	Ahead            int      `json:"ahead"`
	Behind           int      `json:"behind"`
	Incoming         []Commit `json:"incoming"`
	IncomingTotal    int      `json:"incoming_total"`
	OverlappingFiles []string `json:"overlapping_files"`
	UpToDate         bool     `json:"up_to_date"`
}

// Compare builds the report for dir's HEAD against the fetched base commit.
// It lists at most maxIncoming incoming commits, newest first.
func Compare(ctx context.Context, env Env, dir string, b Base, baseSHA string, maxIncoming int) (Report, error) {
	r := Report{Dir: dir, Base: b.Ref(), BaseSource: b.Source, BaseSHA: baseSHA, Incoming: []Commit{}, OverlappingFiles: []string{}}
	head, err := resolve(ctx, env, dir, "HEAD")
	if err != nil {
		return r, err
	}
	r.Head = head
	if out, err := env.run(ctx, dir, "symbolic-ref", "--quiet", "--short", "HEAD"); err == nil {
		r.Branch = strings.TrimSpace(out)
	} else if ge := (*gitError)(nil); !errors.As(err, &ge) || exitCode(ge.err) != 1 {
		return r, err // exit 1 means detached; anything else is a real failure
	}
	out, err := env.run(ctx, dir, "merge-base", head, baseSHA)
	if err != nil {
		if ge := (*gitError)(nil); errors.As(err, &ge) && exitCode(ge.err) == 1 {
			return r, fmt.Errorf("HEAD and %s share no history (no merge base)", b.Ref())
		}
		return r, err
	}
	r.MergeBase = strings.TrimSpace(out)
	out, err = env.run(ctx, dir, "rev-list", "--left-right", "--count", head+"..."+baseSHA)
	if err != nil {
		return r, err
	}
	f := strings.Fields(out)
	if len(f) != 2 {
		return r, fmt.Errorf("git rev-list --count: unexpected output %q", out)
	}
	if r.Ahead, err = strconv.Atoi(f[0]); err != nil {
		return r, fmt.Errorf("git rev-list --count: %v", err)
	}
	if r.Behind, err = strconv.Atoi(f[1]); err != nil {
		return r, fmt.Errorf("git rev-list --count: %v", err)
	}
	r.IncomingTotal = r.Behind
	r.UpToDate = r.Behind == 0
	if r.Behind == 0 {
		return r, nil
	}
	if maxIncoming > 0 {
		out, err = env.run(ctx, dir, "log", "--no-color", "--format=%H%x1f%s", "-n", strconv.Itoa(maxIncoming), head+".."+baseSHA)
		if err != nil {
			return r, err
		}
		for _, l := range lines(out) {
			sha, subject, _ := strings.Cut(l, "\x1f")
			r.Incoming = append(r.Incoming, Commit{SHA: sha, Subject: subject})
		}
	}
	if r.Ahead == 0 {
		return r, nil // nothing changed on our side
	}
	theirs, err := changedFiles(ctx, env, dir, r.MergeBase, baseSHA)
	if err != nil {
		return r, err
	}
	ours, err := changedFiles(ctx, env, dir, r.MergeBase, head)
	if err != nil {
		return r, err
	}
	for p := range ours {
		if theirs[p] {
			r.OverlappingFiles = append(r.OverlappingFiles, p)
		}
	}
	sort.Strings(r.OverlappingFiles)
	return r, nil
}

// changedFiles lists paths changed between two commits. Renames count as
// a delete plus an add, so both the old and the new path are listed.
func changedFiles(ctx context.Context, env Env, dir, from, to string) (map[string]bool, error) {
	out, err := env.run(ctx, dir, "diff", "--name-only", "-z", "--no-renames", "--no-ext-diff", "--no-textconv", from, to, "--")
	if err != nil {
		return nil, err
	}
	m := map[string]bool{}
	for _, p := range strings.Split(out, "\x00") {
		if p != "" {
			m[p] = true
		}
	}
	return m, nil
}

// Refusals lists every reason a merge into dir's HEAD must not start.
// Untracked files do not refuse: git never overwrites them during a merge
// (it stops first), and neither does `git merge --abort`.
func Refusals(ctx context.Context, env Env, dir string, r Report, b Base) ([]string, error) {
	var refusals []string
	if r.Branch == "" {
		refusals = append(refusals, "HEAD is detached: check out the branch to merge into")
	} else if r.Branch == b.Branch {
		refusals = append(refusals, fmt.Sprintf("on the base branch itself (%s): sync it with a fast-forward pull, not a merge", r.Branch))
	}
	for _, s := range []struct{ path, what string }{
		{"MERGE_HEAD", "a merge is in progress (finish it, or git merge --abort)"},
		{"rebase-merge", "a rebase is in progress (finish it, or git rebase --abort)"},
		{"rebase-apply", "a rebase or am is in progress (finish it, or abort it)"},
		{"CHERRY_PICK_HEAD", "a cherry-pick is in progress (finish it, or git cherry-pick --abort)"},
		{"REVERT_HEAD", "a revert is in progress (finish it, or git revert --abort)"},
	} {
		in, err := gitPathExists(ctx, env, dir, s.path)
		if err != nil {
			return nil, err
		}
		if in {
			refusals = append(refusals, s.what)
		}
	}
	out, err := env.run(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=no", "--no-renames", "--ignore-submodules=none")
	if err != nil {
		return nil, err
	}
	var dirty []string
	for _, e := range strings.Split(out, "\x00") {
		if len(e) > 3 {
			dirty = append(dirty, e)
		}
	}
	if len(dirty) > 0 {
		refusals = append(refusals, fmt.Sprintf("%d tracked file(s) have uncommitted changes, e.g. %s: commit or stash them first", len(dirty), dirty[0]))
	}
	return refusals, nil
}

func gitPathExists(ctx context.Context, env Env, dir, name string) (bool, error) {
	out, err := env.run(ctx, dir, "rev-parse", "--git-path", name)
	if err != nil {
		return false, err
	}
	p := strings.TrimSpace(out)
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	_, err = os.Lstat(p)
	if err == nil {
		return true, nil
	}
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	return false, err
}

// Conflict is one unmerged path and how it conflicts.
type Conflict struct {
	Path string `json:"path"`
	Kind string `json:"kind"`
}

var conflictKinds = map[string]string{
	"UU": "both modified",
	"AA": "both added",
	"DD": "both deleted",
	"AU": "added by us",
	"UA": "added by them",
	"DU": "deleted by us",
	"UD": "deleted by them",
}

// Outcome is what a merge attempt did.
type Outcome int

const (
	Merged    Outcome = iota // a merge commit or a fast-forward landed
	Stopped                  // the merge stopped with conflicts or before committing
	Overwrite                // git refused: the merge would overwrite untracked files
)

// MergeResult describes a merge attempt.
type MergeResult struct {
	Outcome     Outcome    `json:"-"`
	Commit      string     `json:"commit,omitempty"`
	FastForward bool       `json:"fast_forward,omitempty"`
	Conflicts   []Conflict `json:"conflicts,omitempty"`
	InProgress  bool       `json:"in_progress"`
	Aborted     bool       `json:"aborted,omitempty"`
	GitOutput   string     `json:"git_output,omitempty"`
}

// Merge merges baseSHA into dir's HEAD (expected to be head) with
// `git merge --no-edit --ff`. It never rebases or pushes. If the merge
// stops, it is left in progress unless abortOnStop, in which case it is
// aborted and HEAD is checked to be back at head. ctx should not be
// cancelled midway: killing git mid-merge leaves a lock file behind.
func Merge(ctx context.Context, env Env, dir string, r Report, b Base, abortOnStop bool) (MergeResult, error) {
	msg := fmt.Sprintf("Merge %s (%s) into %s", b.Ref(), short(r.BaseSHA), r.Branch)
	out, err := env.run(ctx, dir, "merge", "--no-edit", "--ff", "--no-autostash", "-m", msg, r.BaseSHA)
	var ge *gitError
	if err != nil && !errors.As(err, &ge) {
		return MergeResult{}, err
	}
	if err == nil {
		head, err := resolve(ctx, env, dir, "HEAD")
		if err != nil {
			return MergeResult{}, err
		}
		res := MergeResult{Outcome: Merged, Commit: head}
		res.FastForward = head == r.BaseSHA && r.Ahead == 0
		return res, nil
	}
	gitOut := strings.TrimSpace(strings.TrimSpace(out) + "\n" + ge.stderr)
	inProgress, perr := gitPathExists(ctx, env, dir, "MERGE_HEAD")
	if perr != nil {
		return MergeResult{}, perr
	}
	if !inProgress {
		head, herr := resolve(ctx, env, dir, "HEAD")
		if herr != nil {
			return MergeResult{}, herr
		}
		if head != r.Head {
			return MergeResult{}, fmt.Errorf("git merge failed and HEAD moved from %s to %s: %s", short(r.Head), short(head), gitOut)
		}
		if strings.Contains(gitOut, "would be overwritten by merge") {
			return MergeResult{Outcome: Overwrite, GitOutput: gitOut}, nil
		}
		return MergeResult{}, err
	}
	res := MergeResult{Outcome: Stopped, InProgress: true, GitOutput: gitOut}
	if res.Conflicts, err = Conflicts(ctx, env, dir); err != nil {
		return res, err
	}
	if abortOnStop {
		if _, err := env.run(ctx, dir, "merge", "--abort"); err != nil {
			return res, fmt.Errorf("the merge stopped and could not be aborted (it is still in progress): %w", err)
		}
		res.InProgress = false
		res.Aborted = true
		head, err := resolve(ctx, env, dir, "HEAD")
		if err != nil {
			return res, err
		}
		if head != r.Head {
			return res, fmt.Errorf("after git merge --abort, HEAD is %s, not %s", short(head), short(r.Head))
		}
	}
	return res, nil
}

// Conflicts lists the unmerged paths in dir with their kind.
func Conflicts(ctx context.Context, env Env, dir string) ([]Conflict, error) {
	out, err := env.run(ctx, dir, "status", "--porcelain=v1", "-z", "--untracked-files=no", "--no-renames")
	if err != nil {
		return nil, err
	}
	cs := []Conflict{}
	for _, e := range strings.Split(out, "\x00") {
		if len(e) < 4 {
			continue
		}
		if kind, ok := conflictKinds[e[:2]]; ok {
			cs = append(cs, Conflict{Path: e[3:], Kind: kind})
		}
	}
	sort.Slice(cs, func(i, j int) bool { return cs[i].Path < cs[j].Path })
	return cs, nil
}

func exitCode(err error) int {
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		return ee.ExitCode()
	}
	return -1
}

func lines(s string) []string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			out = append(out, l)
		}
	}
	return out
}

func short(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
