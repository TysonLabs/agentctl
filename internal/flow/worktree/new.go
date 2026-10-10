package worktree

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// WorktreesDir is where new worktrees go, relative to the main checkout.
// sweep lists every registered worktree wherever it is, but one fixed place
// keeps them findable and out of other checkouts.
const WorktreesDir = ".claude/worktrees"

// NewOptions describes a worktree to create.
type NewOptions struct {
	Branch    string   // the new branch (normal mode)
	From      string   // base ref; "" = the remote's default branch
	Scratch   bool     // detached throwaway worktree instead of a branch
	CloneDirs []string // directories to APFS-clone from the main checkout
	Now       time.Time
}

// Created reports what New made.
type Created struct {
	Path    string      `json:"path"`
	Branch  string      `json:"branch,omitempty"`
	Scratch bool        `json:"scratch,omitempty"`
	Base    string      `json:"base"`
	BaseSHA string      `json:"base_sha"`
	Cloned  []string    `json:"cloned"`
	Skipped []CloneSkip `json:"clone_skipped,omitempty"`
	Notes   []string    `json:"notes,omitempty"`
}

// CloneSkip is a --clone-dir that was not cloned, and why.
type CloneSkip struct {
	Dir    string `json:"dir"`
	Reason string `json:"reason"`
}

// RefusedError lists every reason New refused. errors.Is(err, ErrRefused).
type RefusedError struct{ Reasons []string }

func (e *RefusedError) Error() string { return strings.Join(e.Reasons, "; ") }
func (e *RefusedError) Unwrap() error { return ErrRefused }

// MainCheckout is the repository's main working tree (first in the list).
func MainCheckout(list []Worktree) (Worktree, error) {
	if len(list) == 0 || !list[0].Main {
		return Worktree{}, errors.New("git worktree list returned no main working tree")
	}
	if list[0].Bare {
		return Worktree{}, &RefusedError{[]string{"the repository is bare: there is no main checkout to put .claude/worktrees in"}}
	}
	return list[0], nil
}

// Slug turns a branch name into one directory name: feat/x -> feat-x.
func Slug(branch string) string {
	var b strings.Builder
	for _, r := range branch {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '.', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	return strings.TrimLeft(b.String(), ".-")
}

// New fetches the remote, then creates a worktree under the main checkout's
// .claude/worktrees: on a new branch, or (Scratch) detached and marked as
// throwaway. It never reuses an existing branch or path.
func New(ctx context.Context, env Env, repoDir string, opt NewOptions) (Created, error) {
	env = env.withDefaults()
	var c Created
	if opt.Now.IsZero() {
		opt.Now = time.Now()
	}
	list, err := List(ctx, env, repoDir)
	if err != nil {
		return c, err
	}
	main, err := MainCheckout(list)
	if err != nil {
		return c, err
	}
	var refusals []string
	refuse := func(format string, a ...any) { refusals = append(refusals, fmt.Sprintf(format, a...)) }

	if !opt.Scratch {
		if opt.Branch == "" || strings.HasPrefix(opt.Branch, "-") {
			return c, &RefusedError{[]string{fmt.Sprintf("%q is not a usable branch name", opt.Branch)}}
		}
		if _, err := run(ctx, main.Path, env.Git, "check-ref-format", "--branch", opt.Branch); err != nil {
			return c, &RefusedError{[]string{fmt.Sprintf("%q is not a valid branch name", opt.Branch)}}
		}
		if Slug(opt.Branch) == "" {
			return c, &RefusedError{[]string{fmt.Sprintf("%q gives an empty directory name", opt.Branch)}}
		}
	}
	for _, d := range opt.CloneDirs {
		if err := validCloneDir(d); err != nil {
			refuse("--clone-dir %q: %v", d, err)
		}
	}
	if len(refusals) > 0 {
		return c, &RefusedError{refusals}
	}

	// The base must be fresh: fetch before resolving anything.
	target, targetErr := DefaultTarget(ctx, env, main.Path, "")
	remote := "origin"
	if targetErr == nil {
		remote = target.Remote
	}
	if _, err := run(ctx, main.Path, env.Git, "fetch", "--quiet", remote); err != nil {
		return c, fmt.Errorf("fetch %s: %w", remote, err)
	}
	c.Base = opt.From
	if c.Base == "" {
		if targetErr != nil {
			return c, &RefusedError{[]string{targetErr.Error() + " (or pass the base explicitly)"}}
		}
		c.Base = target.Ref()
	}
	if strings.HasPrefix(c.Base, "-") {
		return c, &RefusedError{[]string{fmt.Sprintf("base %q is not a ref", c.Base)}}
	}
	sha, err := resolveCommit(ctx, env, main.Path, c.Base)
	if err != nil || sha == "" {
		return c, &RefusedError{[]string{fmt.Sprintf("base %q does not name a commit (after fetching %s)", c.Base, remote)}}
	}
	c.BaseSHA = sha

	root := filepath.Join(main.Path, filepath.FromSlash(WorktreesDir))
	if opt.Scratch {
		c.Scratch = true
		c.Path, err = scratchPath(root)
		if err != nil {
			return c, err
		}
	} else {
		c.Branch = opt.Branch
		c.Path = filepath.Join(root, Slug(opt.Branch))
		if exists(ctx, env, main.Path, "refs/heads/"+opt.Branch) {
			refuse("branch %s already exists: pick a new name, or check it out with git worktree add", opt.Branch)
		}
		if exists(ctx, env, main.Path, "refs/remotes/"+remote+"/"+opt.Branch) {
			refuse("%s/%s already exists: someone may own that branch; pick a new name", remote, opt.Branch)
		}
		if _, err := os.Lstat(c.Path); err == nil {
			refuse("path %s already exists", c.Path)
		} else if !errors.Is(err, fs.ErrNotExist) {
			return c, err
		}
	}
	if len(refusals) > 0 {
		return c, &RefusedError{refusals}
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return c, err
	}

	if opt.Scratch {
		// Locked at creation, so no other session's sweep can treat the new
		// tree as an ordinary removable one before the marker exists.
		reason := scratchLockPrefix + " created " + opt.Now.UTC().Format(time.RFC3339) + "; remove with: agentflow worktree done " + c.Path
		if _, err := run(ctx, main.Path, env.Git, "worktree", "add", "--quiet", "--detach", "--lock", "--reason", reason, c.Path, sha); err != nil {
			return c, err
		}
		if err := writeScratchMarker(ctx, env, main.Path, c.Path, ScratchMarker{
			Created: opt.Now.UTC(), PID: os.Getpid(), PPID: os.Getppid(), Base: c.Base, BaseSHA: sha,
		}); err != nil {
			// The tree was made a moment ago by this call and holds nothing.
			_, rmErr := run(ctx, main.Path, env.Git, "worktree", "remove", "--force", "--force", c.Path)
			if rmErr != nil {
				return c, fmt.Errorf("write scratch marker: %v; and the unmarked worktree %s could not be removed: %v", err, c.Path, rmErr)
			}
			return c, fmt.Errorf("write scratch marker: %w", err)
		}
	} else {
		// --no-track: the base is usually origin/main, and an upstream of
		// origin/main would make a bare `git push` target main.
		if _, err := run(ctx, main.Path, env.Git, "worktree", "add", "--quiet", "--no-track", "-b", opt.Branch, c.Path, sha); err != nil {
			return c, err
		}
	}

	c.Cloned = []string{}
	for _, d := range opt.CloneDirs {
		if reason := cloneDir(ctx, main.Path, c.Path, d); reason != "" {
			c.Skipped = append(c.Skipped, CloneSkip{Dir: d, Reason: reason})
		} else {
			c.Cloned = append(c.Cloned, d)
		}
	}
	if !ignored(ctx, env, main.Path, c.Path) {
		c.Notes = append(c.Notes, WorktreesDir+"/ is not ignored, so the main checkout's `git status` shows it; add it to .gitignore or .git/info/exclude")
	}
	return c, nil
}

func exists(ctx context.Context, env Env, dir, ref string) bool {
	_, err := run(ctx, dir, env.Git, "rev-parse", "--verify", "--quiet", "--end-of-options", ref)
	return err == nil
}

func ignored(ctx context.Context, env Env, dir, path string) bool {
	rel, err := filepath.Rel(dir, path)
	if err != nil {
		return false
	}
	_, err = run(ctx, dir, env.Git, "check-ignore", "-q", "--", filepath.ToSlash(rel)+"/")
	return err == nil
}

func scratchPath(root string) (string, error) {
	for range 8 {
		b := make([]byte, 4)
		if _, err := rand.Read(b); err != nil {
			return "", err
		}
		p := filepath.Join(root, "scratch-"+hex.EncodeToString(b))
		if _, err := os.Lstat(p); errors.Is(err, fs.ErrNotExist) {
			return p, nil
		}
	}
	return "", errors.New("could not pick an unused scratch directory name")
}

// validCloneDir accepts a plain relative directory inside the checkout.
func validCloneDir(d string) error {
	if d == "" || filepath.IsAbs(d) {
		return errors.New("want a directory relative to the checkout root")
	}
	clean := filepath.Clean(d)
	if clean == "." {
		return errors.New("cannot clone the whole checkout")
	}
	for _, part := range strings.Split(filepath.ToSlash(clean), "/") {
		if part == ".." || part == ".git" {
			return fmt.Errorf("may not contain %q", part)
		}
	}
	if strings.HasPrefix(filepath.ToSlash(clean)+"/", WorktreesDir+"/") || strings.HasPrefix(WorktreesDir+"/", filepath.ToSlash(clean)+"/") {
		return errors.New("may not overlap " + WorktreesDir)
	}
	return nil
}

// cloneCommand is the copy-on-write copy; tests may replace it.
var cloneCommand = func(ctx context.Context, src, dst string) error {
	out, err := exec.CommandContext(ctx, "cp", "-c", "-R", src, dst).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// cloneDir copy-on-write clones main/d to wt/d, returning why it did not
// ("" on success). It never falls back to a full copy: build directories
// can be tens of GB.
func cloneDir(ctx context.Context, main, wt, d string) string {
	if runtime.GOOS != "darwin" {
		return "copy-on-write cloning (cp -c) is only supported on macOS; the build starts cold"
	}
	src := filepath.Join(main, filepath.Clean(d))
	dst := filepath.Join(wt, filepath.Clean(d))
	fi, err := os.Lstat(src)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return "not present in the main checkout"
	case err != nil:
		return err.Error()
	case !fi.IsDir():
		return "not a directory in the main checkout"
	}
	if _, err := os.Lstat(dst); err == nil {
		return "already exists in the new worktree (tracked content); not overwritten"
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err.Error()
	}
	// A tracked symlink on the way would put the clone (or the directories
	// made for it) outside the worktree: resolve the deepest existing parent
	// before creating anything.
	existing := filepath.Dir(dst)
	for {
		if _, err := os.Lstat(existing); err == nil {
			break
		}
		existing = filepath.Dir(existing)
	}
	if parent, err := filepath.EvalSymlinks(existing); err != nil {
		return err.Error()
	} else if root := canonical(wt); parent != root && !pathWithin(root, parent) {
		return "its parent directory resolves outside the new worktree; not cloned"
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err.Error()
	}
	if err := cloneCommand(ctx, src, dst); err != nil {
		// dst did not exist before, so whatever is there now is a partial clone.
		_ = os.RemoveAll(dst)
		return "clone failed (no full copy was made): " + err.Error()
	}
	return ""
}
