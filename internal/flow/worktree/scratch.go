package worktree

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// A scratch worktree is a detached, throwaway tree (a red/green probe, a
// base comparison). It carries two marks:
//   - a marker file in its administrative directory
//     (<git-common-dir>/worktrees/<id>/agentflow-scratch), which is the
//     authority: it lives outside the tree, so nothing done inside the tree
//     can forge or delete it, and git removes it with the worktree;
//   - a lock whose reason starts with scratchLockPrefix, so the ordinary
//     done/sweep checks (and plain `git worktree remove`) refuse it.
//
// Only a marked worktree is removed without the merged/clean checks. The
// unused checks (no process inside, nothing nested) always apply.

const (
	scratchMarkerFile = "agentflow-scratch"
	scratchLockPrefix = "agentflow scratch"
)

// ScratchMarker is the content of the marker file.
type ScratchMarker struct {
	ID      string    `json:"id"`
	Created time.Time `json:"created"`
	PID     int       `json:"pid"`
	PPID    int       `json:"ppid"`
	Base    string    `json:"base"`
	BaseSHA string    `json:"base_sha"`
}

// adminDir finds the administrative directory of the linked worktree at
// path by reading each <common>/worktrees/*/gitdir, so it works even when
// the worktree directory is gone and never trusts the tree's own .git file.
func adminDir(ctx context.Context, env Env, repoDir, path string) (string, error) {
	env = env.withDefaults()
	out, err := run(ctx, repoDir, env.Git, "rev-parse", "--path-format=absolute", "--git-common-dir")
	if err != nil {
		return "", err
	}
	base := filepath.Join(strings.TrimSpace(out), "worktrees")
	entries, err := os.ReadDir(base)
	if errors.Is(err, fs.ErrNotExist) {
		return "", nil
	} else if err != nil {
		return "", err
	}
	want := canonical(path)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		b, err := os.ReadFile(filepath.Join(base, e.Name(), "gitdir"))
		if err != nil {
			continue
		}
		gitdir := strings.TrimSpace(string(b))
		if !filepath.IsAbs(gitdir) {
			gitdir = filepath.Join(base, e.Name(), gitdir)
		}
		if canonical(filepath.Dir(gitdir)) == want {
			return filepath.Join(base, e.Name()), nil
		}
	}
	return "", nil
}

func writeScratchMarker(ctx context.Context, env Env, repoDir, path string, m ScratchMarker) error {
	dir, err := adminDir(ctx, env, repoDir, path)
	if err != nil {
		return err
	}
	if dir == "" {
		return fmt.Errorf("no administrative directory found for %s", path)
	}
	b, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, scratchMarkerFile+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	removeTmp := true
	defer func() {
		if removeTmp {
			_ = os.Remove(tmp)
		}
	}()
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	// Linking publishes without overwriting a marker another actor created.
	if err := os.Link(tmp, filepath.Join(dir, scratchMarkerFile)); err != nil {
		return err
	}
	_ = os.Remove(tmp) // the published marker is authoritative; the temp is not
	removeTmp = false
	return nil
}

// scratchMarkerWriter is replaceable by tests that exercise creation failure.
var scratchMarkerWriter = writeScratchMarker

// ReadScratch returns the scratch marker of w, nil if it has none, or an
// error if a marker exists but cannot be read (callers must refuse then).
func ReadScratch(ctx context.Context, env Env, repoDir string, w Worktree) (*ScratchMarker, error) {
	if w.Main {
		return nil, nil
	}
	dir, err := adminDir(ctx, env, repoDir, w.Path)
	if err != nil || dir == "" {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, scratchMarkerFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var m ScratchMarker
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("unreadable scratch marker: %v", err)
	}
	if len(m.ID) != 32 || !isHexOIDPart(m.ID) || m.Created.IsZero() || m.PID <= 0 || m.PPID < 0 || m.Base == "" || !isHexOID(m.BaseSHA) {
		return nil, errors.New("invalid scratch marker: missing or malformed identity, creation, process, or base fields")
	}
	return &m, nil
}

func isHexOIDPart(s string) bool {
	for _, r := range s {
		if !strings.ContainsRune("0123456789abcdefABCDEF", r) {
			return false
		}
	}
	return s != ""
}

func scratchLockReason(path string, m ScratchMarker) string {
	return scratchLockPrefix + " " + m.ID + " created " + m.Created.UTC().Format(time.RFC3339) + "; remove with: agentflow worktree done " + path
}

// ScratchCheck is the verdict on removing one scratch worktree.
type ScratchCheck struct {
	Path       string   `json:"path"`
	Branch     string   `json:"branch,omitempty"`
	Head       string   `json:"head"`
	Scratch    bool     `json:"scratch"`
	Created    string   `json:"created,omitempty"`
	Age        string   `json:"age,omitempty"`
	DirtyFiles int      `json:"dirty_files"`
	OK         bool     `json:"ok"`
	Refusals   []string `json:"refusals,omitempty"`
	Prunable   bool     `json:"prunable,omitempty"`
	markerID   string
}

// InspectScratch decides whether w can be removed as a scratch worktree:
// it must carry the scratch marker, be at least minAge old, and be unused.
// Uncommitted files and unmerged commits do not block it. It never changes
// anything.
func InspectScratch(ctx context.Context, env Env, repoDir string, w Worktree, now time.Time, minAge time.Duration) (ScratchCheck, error) {
	env = env.withDefaults()
	c := ScratchCheck{Path: w.Path, Branch: w.Branch, Head: w.Head}
	refuse := func(format string, a ...any) { c.Refusals = append(c.Refusals, fmt.Sprintf(format, a...)) }
	if w.Main {
		refuse("this is the repository's main working tree")
		return c, nil
	}
	m, err := ReadScratch(ctx, env, repoDir, w)
	if err != nil {
		refuse("%v: not treated as scratch", err)
		return c, nil
	}
	if m == nil {
		refuse("not marked as an agentflow scratch worktree, so the merged and clean checks apply (agentflow worktree done without scratch)")
		return c, nil
	}
	c.Scratch = true
	c.markerID = m.ID
	c.Created = m.Created.UTC().Format(time.RFC3339)
	age := now.Sub(m.Created)
	c.Age = age.Round(time.Minute).String()
	if w.Locked && w.LockReason != scratchLockReason(w.Path, *m) {
		refuse("locked by someone else (%s)%s", w.LockReason, lockOwnerState(w.LockReason))
	}
	if minAge > 0 && age < minAge {
		refuse("younger than %s (created %s)", minAge, c.Created)
	}
	if _, err := os.Stat(w.Path); errors.Is(err, fs.ErrNotExist) {
		c.Prunable = true
		c.OK = len(c.Refusals) == 0
		return c, nil
	} else if err != nil {
		return c, err
	}
	dirty, err := run(ctx, w.Path, env.Git, "status", "--porcelain", "--untracked-files=all")
	if err != nil {
		return c, err
	}
	c.DirtyFiles = len(nonEmptyLines(dirty))
	contained, err := containedRefusals(ctx, env, w.Path)
	if err != nil {
		return c, err
	}
	c.Refusals = append(c.Refusals, contained...)
	if users := inUseRefusal(ctx, env, w.Path); users != "" {
		refuse("%s", users)
	}
	c.OK = len(c.Refusals) == 0
	return c, nil
}

// RemovedScratch reports what RemoveScratch did.
type RemovedScratch struct {
	FreedBytes int64 `json:"freed_bytes"`
	// DiscardedHead is the commit the tree was at; commits made only there
	// stay recoverable by this sha until git gc.
	DiscardedHead       string `json:"discarded_head,omitempty"`
	DiscardedDirtyFiles int    `json:"discarded_dirty_files"`
}

// RemoveScratch deletes a scratch worktree that InspectScratch passed,
// re-inspecting it first. Uncommitted files in it are discarded.
func RemoveScratch(ctx context.Context, env Env, repoDir string, c ScratchCheck, now time.Time, minAge time.Duration) (RemovedScratch, error) {
	env = env.withDefaults()
	var r RemovedScratch
	if !c.OK || !c.Scratch {
		return r, errors.New("refusing to remove a scratch worktree that did not pass its checks")
	}
	if !c.Prunable {
		r.FreedBytes = diskUsage(c.Path)
	}
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
	fresh, err := InspectScratch(ctx, env, repoDir, *current, now, minAge)
	if err != nil {
		return r, err
	}
	if !fresh.OK {
		return r, fmt.Errorf("%w: safety changed after inspection: %s", ErrRefused, strings.Join(fresh.Refusals, "; "))
	}
	if fresh.markerID != c.markerID {
		return r, fmt.Errorf("%w: scratch worktree identity changed after inspection", ErrRefused)
	}
	r.DiscardedHead, r.DiscardedDirtyFiles = fresh.Head, fresh.DirtyFiles
	if fresh.Prunable {
		if current.Locked {
			if _, err := run(ctx, repoDir, env.Git, "worktree", "unlock", c.Path); err != nil {
				return r, err
			}
		}
		_, err := run(ctx, repoDir, env.Git, "worktree", "prune")
		return r, err
	}
	// Twice --force: once for the expected uncommitted files, once for the
	// scratch lock. Nested repos, worktrees and submodules were refused above.
	if _, err := run(ctx, repoDir, env.Git, "worktree", "remove", "--force", "--force", c.Path); err != nil {
		return r, err
	}
	return r, nil
}
