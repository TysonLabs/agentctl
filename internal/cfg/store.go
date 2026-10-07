package cfg

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/TysonLabs/agentctl/internal/registry"
)

// ErrConflict means the file changed since the caller read it.
var ErrConflict = errors.New("services.toml changed since it was read; reload and try again")

// Store edits one services.toml.
type Store struct {
	Path string
}

// Result reports a completed edit.
type Result struct {
	Version  string
	Warnings []string
}

// Edit runs fn on the current file under an exclusive lock and writes the
// result. If expect is not "", the file must still be at that version (the
// UI's optimistic check); the CLI passes "". fn validates its input before
// any side effect. The new content must pass registry.Parse, the same check
// agentctl runs, or nothing is written.
func (s *Store) Edit(expect string, fn func(d *Doc) error) (*Result, error) {
	if err := os.MkdirAll(filepath.Dir(s.Path), 0o700); err != nil {
		return nil, fmt.Errorf("creating %s: %v", filepath.Dir(s.Path), err)
	}
	unlock, err := lockFile(s.Path + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()

	d, _, err := readDoc(s.Path)
	if err != nil {
		return nil, err
	}
	if expect != "" && expect != d.Version {
		return nil, ErrConflict
	}
	if err := fn(d); err != nil {
		return nil, err
	}
	out, err := renderTree(d.Tree)
	if err != nil {
		return nil, err
	}
	if _, err := registry.Parse(s.Path, out); err != nil {
		return nil, fmt.Errorf("refusing to write an invalid registry: %v", err)
	}
	if err := writeAtomic(s.Path, out); err != nil {
		return nil, err
	}
	res := &Result{Version: VersionOf(out)}
	if d.hadComments {
		res.Warnings = append(res.Warnings, "comments in "+s.Path+" were not kept")
	}
	for _, f := range d.onSaved {
		if err := f(); err != nil {
			res.Warnings = append(res.Warnings, err.Error())
		}
	}
	return res, nil
}

// writeAtomic replaces path with data, mode 0600, through a synced temp file
// in the same directory, so a crash leaves the old file or the new one. A
// symlinked path is written through to its target, keeping the link.
func writeAtomic(path string, data []byte) error {
	if real, err := filepath.EvalSymlinks(path); err == nil {
		path = real
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("creating %s: %v", dir, err)
	}
	f, err := os.CreateTemp(dir, ".services.toml.*")
	if err != nil {
		return fmt.Errorf("writing %s: %v", path, err)
	}
	tmp := f.Name()
	defer os.Remove(tmp) // no-op after the rename
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %v", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %v", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return fmt.Errorf("writing %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		return fmt.Errorf("writing %s: %v", path, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		return fmt.Errorf("writing %s: %v", path, err)
	}
	if d, err := os.Open(dir); err == nil {
		_ = d.Sync()
		d.Close()
	}
	return nil
}
