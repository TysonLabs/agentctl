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
	target, err := resolveStorePath(s.Path)
	if err != nil {
		return nil, err
	}
	unlock, err := lockFile(target + ".lock")
	if err != nil {
		return nil, err
	}
	defer unlock()

	d, data, err := readDoc(target)
	if err != nil {
		return nil, err
	}
	d.Path = s.Path
	if expect != "" && expect != d.Version {
		return nil, ErrConflict
	}
	if data != nil {
		if _, err := registry.Parse(s.Path, data); err != nil {
			return nil, fmt.Errorf("refusing to edit an invalid registry: %v", err)
		}
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
	commit, cleanup, err := prepareAtomic(target, out)
	if err != nil {
		return nil, err
	}
	defer cleanup()
	for _, f := range d.beforeSave {
		if err := f(); err != nil {
			return nil, err
		}
	}
	if err := commit(); err != nil {
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

// resolveStorePath returns one canonical write and lock identity. In
// particular, a symlink and its target must not acquire different locks.
// A broken symlink is rejected rather than replaced by a regular file.
func resolveStorePath(path string) (string, error) {
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolving %s: %v", path, err)
	}
	if real, err := filepath.EvalSymlinks(abs); err == nil {
		return real, nil
	}
	fi, lerr := os.Lstat(abs)
	switch {
	case lerr == nil:
		if fi.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("resolving symlink %s: its target is unavailable", path)
		}
		return "", fmt.Errorf("resolving %s: path is unavailable", path)
	case !errors.Is(lerr, os.ErrNotExist):
		return "", fmt.Errorf("resolving %s: %v", path, lerr)
	}
	if realDir, err := filepath.EvalSymlinks(filepath.Dir(abs)); err == nil {
		return filepath.Join(realDir, filepath.Base(abs)), nil
	}
	return abs, nil
}

// prepareAtomic writes and syncs a mode-0600 temporary file, returning the
// final atomic rename. Preparing before Keychain callbacks ensures ordinary
// file errors cannot leave a changed credential behind an unchanged file.
func prepareAtomic(path string, data []byte) (commit func() error, cleanup func(), err error) {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, nil, fmt.Errorf("creating %s: %v", dir, err)
	}
	f, err := os.CreateTemp(dir, ".services.toml.*")
	if err != nil {
		return nil, nil, fmt.Errorf("writing %s: %v", path, err)
	}
	tmp := f.Name()
	cleanup = func() { _ = os.Remove(tmp) }
	if err := f.Chmod(0o600); err != nil {
		f.Close()
		cleanup()
		return nil, nil, fmt.Errorf("writing %s: %v", path, err)
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		cleanup()
		return nil, nil, fmt.Errorf("writing %s: %v", path, err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		cleanup()
		return nil, nil, fmt.Errorf("writing %s: %v", path, err)
	}
	if err := f.Close(); err != nil {
		cleanup()
		return nil, nil, fmt.Errorf("writing %s: %v", path, err)
	}
	commit = func() error {
		if err := os.Rename(tmp, path); err != nil {
			return fmt.Errorf("writing %s: %v", path, err)
		}
		if d, err := os.Open(dir); err == nil {
			_ = d.Sync()
			d.Close()
		}
		return nil
	}
	return commit, cleanup, nil
}
