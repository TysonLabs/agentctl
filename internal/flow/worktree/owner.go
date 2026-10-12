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
	"unicode"
	"unicode/utf8"
)

// The owner record says which session or job created (or adopted) a
// worktree. Like the scratch marker it lives in the worktree's Git admin
// directory (<git-common-dir>/worktrees/<id>/agentflow-owner): outside the
// tree, readable when the tree is gone, and removed by git with the record.
// It is informational: done removes any owner's worktree on request; sweep
// --owner limits removal to one owner.
const ownerFile = "agentflow-owner"

// OwnerRecord is the content of the owner file.
type OwnerRecord struct {
	Owner    string    `json:"owner"`
	Source   string    `json:"source,omitempty"` // where the name came from: --owner, AGENTFLOW_OWNER, CLAUDE_JOB_DIR, ...
	Via      string    `json:"via"`              // "new" or "adopt"
	Recorded time.Time `json:"recorded"`
}

// ownerEnv lists, in order, the environment variables an owner is derived
// from when --owner is not given. CLAUDE_JOB_DIR contributes its base name.
var ownerEnv = []string{"AGENTFLOW_OWNER", "CLAUDE_JOB_DIR", "CLAUDE_CODE_SESSION_ID", "CODEX_SESSION_ID"}

// ResolveOwner returns the owner name and its source: flag if set, else the
// first usable variable of ownerEnv. "" means no owner is known.
func ResolveOwner(flag string, getenv func(string) string) (owner, source string) {
	if flag != "" {
		return flag, "--owner"
	}
	for _, k := range ownerEnv {
		v := strings.TrimSpace(getenv(k))
		if k == "CLAUDE_JOB_DIR" && v != "" {
			v = filepath.Base(filepath.Clean(v))
			if v == "." || v == string(filepath.Separator) {
				v = ""
			}
		}
		if v != "" {
			return v, k
		}
	}
	return "", ""
}

// ValidOwner accepts 1-128 printable characters without spaces.
func ValidOwner(s string) error {
	if !utf8.ValidString(s) {
		return errors.New("an owner must be valid UTF-8")
	}
	if s == "" || utf8.RuneCountInString(s) > 128 {
		return errors.New("an owner is 1-128 characters")
	}
	for _, r := range s {
		if !unicode.IsPrint(r) || unicode.IsSpace(r) {
			return fmt.Errorf("owner %q contains a space or control character", s)
		}
	}
	return nil
}

// ReadOwner returns w's owner record, nil if it has none, or an error if
// one exists but cannot be read.
func ReadOwner(ctx context.Context, env Env, repoDir string, w Worktree) (*OwnerRecord, error) {
	if w.Main {
		return nil, nil
	}
	dir, err := adminDir(ctx, env, repoDir, w.Path)
	if err != nil || dir == "" {
		return nil, err
	}
	b, err := os.ReadFile(filepath.Join(dir, ownerFile))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	} else if err != nil {
		return nil, err
	}
	var rec OwnerRecord
	if err := json.Unmarshal(b, &rec); err != nil {
		return nil, fmt.Errorf("unreadable owner record: %v", err)
	}
	if ValidOwner(rec.Owner) != nil {
		return nil, errors.New("invalid owner record: missing or malformed owner")
	}
	return &rec, nil
}

// writeOwner publishes rec for the worktree at path. With replace false it
// never overwrites an existing record.
func writeOwner(ctx context.Context, env Env, repoDir, path string, rec OwnerRecord, replace bool) error {
	dir, err := adminDir(ctx, env, repoDir, path)
	if err != nil {
		return err
	}
	if dir == "" {
		return fmt.Errorf("no administrative directory found for %s", path)
	}
	b, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return err
	}
	suffix, err := randomHex(4)
	if err != nil {
		return err
	}
	tmp := filepath.Join(dir, ownerFile+"."+suffix+".tmp")
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	defer func() { _ = os.Remove(tmp) }()
	if _, err := f.Write(append(b, '\n')); err != nil {
		_ = f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	final := filepath.Join(dir, ownerFile)
	if replace {
		return os.Rename(tmp, final)
	}
	return os.Link(tmp, final) // fails if a record already exists
}

// Adopted reports what Adopt did.
type Adopted struct {
	Path          string      `json:"path"`
	Branch        string      `json:"branch,omitempty"`
	Owner         OwnerRecord `json:"owner"`
	Unchanged     bool        `json:"unchanged,omitempty"` // it already had this owner
	PreviousOwner string      `json:"previous_owner,omitempty"`
}

// Adopt stamps an owner on a worktree created without one (for example by
// plain `git worktree add`). It refuses the main working tree and a
// worktree another owner already holds; the same owner again is a no-op.
func Adopt(ctx context.Context, env Env, repoDir string, w Worktree, owner, source string, now time.Time) (Adopted, error) {
	env = env.withDefaults()
	a := Adopted{Path: w.Path, Branch: w.Branch}
	if w.Main {
		return a, &RefusedError{[]string{"this is the repository's main working tree: it has no owner record"}}
	}
	if err := ValidOwner(owner); err != nil {
		return a, &RefusedError{[]string{err.Error()}}
	}
	prev, err := ReadOwner(ctx, env, repoDir, w)
	if err != nil {
		return a, &RefusedError{[]string{err.Error() + ": not overwritten (remove it by hand if it is junk)"}}
	}
	if prev != nil {
		if prev.Owner == owner {
			a.Owner, a.Unchanged = *prev, true
			return a, nil
		}
		return a, &RefusedError{[]string{fmt.Sprintf("already owned by %s (recorded %s via %s): not reassigned",
			prev.Owner, prev.Recorded.UTC().Format(time.RFC3339), prev.Via)}}
	}
	a.Owner = OwnerRecord{Owner: owner, Source: source, Via: "adopt", Recorded: now.UTC()}
	if err := writeOwner(ctx, env, repoDir, w.Path, a.Owner, false); err != nil {
		if errors.Is(err, fs.ErrExist) {
			return a, &RefusedError{[]string{"another process recorded an owner at the same time: run adopt again to see it"}}
		}
		return a, err
	}
	return a, nil
}
