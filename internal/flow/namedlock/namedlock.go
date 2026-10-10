// Package namedlock is agentflow's exclusive, machine-wide named lock. Any
// agentflow command that serializes heavy work (lock run, gate, release)
// shares it, so they interoperate by name.
//
// The convention is fixed, because separately built commands rely on it:
//
//   - The lock is flock(LOCK_EX) on <os.TempDir()>/agentflow-lock-<name>.lock.
//     The kernel releases it when the holder's process dies, even on SIGKILL,
//     so a lock can never leak. The lock file is never deleted: deleting a
//     flock file lets two processes lock two different inodes of one name.
//   - While held, the holder writes <os.TempDir()>/agentflow-lock-<name>.json
//     = {"pid":int,"cmd":string,"dir":string,"since":RFC3339}, after it
//     acquires and removed before it releases. The file is advisory: it can be
//     missing (a holder between acquiring and writing) or stale (a holder
//     that was SIGKILLed). Whether a lock is held comes only from a flock test.
//   - A name matches [A-Za-z0-9._-]{1,64}.
package namedlock

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"syscall"
	"time"
)

const (
	prefix    = "agentflow-lock-"
	lockExt   = ".lock"
	infoExt   = ".json"
	tmpPrefix = ".agentflow-lock-"
)

var nameRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// watchInterval is how often a waiter re-reads the holder file to notice a
// new holder. It reads a small file; it never retries the lock.
var watchInterval = 2 * time.Second

// ValidateName reports whether name is a legal lock name.
func ValidateName(name string) error {
	if !nameRE.MatchString(name) {
		return fmt.Errorf("invalid lock name %q: use 1-64 characters from A-Z a-z 0-9 . _ -", name)
	}
	return nil
}

// Paths returns the lock file and the holder-info file for name.
func Paths(name string) (lockPath, infoPath string) {
	dir := os.TempDir()
	return filepath.Join(dir, prefix+name+lockExt), filepath.Join(dir, prefix+name+infoExt)
}

// Info is the advisory holder record.
type Info struct {
	PID   int    `json:"pid"`
	Cmd   string `json:"cmd"`
	Dir   string `json:"dir"`
	Since string `json:"since"` // RFC3339
}

// NewInfo describes the current process as the holder.
func NewInfo(cmd, dir string) Info {
	return Info{PID: os.Getpid(), Cmd: cmd, Dir: dir, Since: time.Now().Format(time.RFC3339)}
}

// Holder is what a waiter or status reader knows about the current holder.
// Info is nil when no readable holder file exists.
type Holder struct {
	Info  *Info `json:"info"`
	Alive bool  `json:"alive"` // Info.PID exists (false when Info is nil)
}

func (h Holder) same(o Holder) bool {
	if h.Info == nil || o.Info == nil {
		return h.Info == nil && o.Info == nil
	}
	return *h.Info == *o.Info
}

// Lock is a held lock. Release it exactly once; extra calls are no-ops.
type Lock struct {
	name     string
	f        *os.File
	infoPath string
	info     Info
}

// ErrBusy is returned by TryAcquire when another holder has the lock.
var ErrBusy = errors.New("lock is held")

func open(name string) (*os.File, string, error) {
	if err := ValidateName(name); err != nil {
		return nil, "", err
	}
	lp, ip := Paths(name)
	f, err := os.OpenFile(lp, os.O_CREATE|os.O_RDWR|syscall.O_NOFOLLOW, 0o600)
	if err != nil {
		return nil, "", err
	}
	return f, ip, nil
}

func flock(f *os.File, how int) error {
	for {
		err := syscall.Flock(int(f.Fd()), how)
		if !errors.Is(err, syscall.EINTR) {
			return err
		}
	}
}

// TryAcquire takes the lock without waiting. It returns ErrBusy if the lock
// is held.
func TryAcquire(name string, info Info) (*Lock, error) {
	f, ip, err := open(name)
	if err != nil {
		return nil, err
	}
	if err := flock(f, syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrBusy
		}
		return nil, err
	}
	return held(name, f, ip, info), nil
}

// Acquire takes the lock, waiting until it is free or ctx ends. While it
// waits, onHolder (if not nil) is called once with the current holder, and
// again each time the holder changes. The wait is a blocking flock, not a
// retry loop; only the advisory holder file is re-read, every few seconds.
func Acquire(ctx context.Context, name string, info Info, onHolder func(Holder)) (*Lock, error) {
	l, err := TryAcquire(name, info)
	if !errors.Is(err, ErrBusy) {
		return l, err
	}
	f, ip, err := open(name)
	if err != nil {
		return nil, err
	}
	got := make(chan error, 1)
	go func() { got <- flock(f, syscall.LOCK_EX) }()

	var last *Holder
	misses := 0
	report := func() {
		if onHolder == nil {
			return
		}
		h := ReadHolder(name)
		// A holder writes its file just after it acquires: give it one
		// interval before reporting an unknown holder.
		if h.Info == nil && last == nil && misses == 0 {
			misses++
			return
		}
		if last != nil && last.same(h) {
			return
		}
		last = &h
		onHolder(h)
	}
	report()
	tick := time.NewTicker(watchInterval)
	defer tick.Stop()
	for {
		select {
		case err := <-got:
			if err != nil {
				f.Close()
				return nil, err
			}
			return held(name, f, ip, info), nil
		case <-tick.C:
			report()
		case <-ctx.Done():
			// The flock call cannot be interrupted. If it succeeds after we
			// gave up, release at once so an abandoned wait never holds the
			// lock.
			go func() {
				if err := <-got; err == nil {
					_ = flock(f, syscall.LOCK_UN)
				}
				f.Close()
			}()
			return nil, ctx.Err()
		}
	}
}

func held(name string, f *os.File, infoPath string, info Info) *Lock {
	l := &Lock{name: name, f: f, infoPath: infoPath, info: info}
	_ = writeInfo(infoPath, info) // advisory: a failed write never blocks the work
	return l
}

func writeInfo(path string, info Info) error {
	data, err := json.Marshal(info)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), tmpPrefix+"*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(append(data, '\n')); err != nil {
		tmp.Close()
		os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		os.Remove(name)
		return err
	}
	if err := os.Rename(name, path); err != nil {
		os.Remove(name)
		return err
	}
	return nil
}

// Release removes the holder file (while the lock is still held, so it can
// never delete the next holder's file) and then unlocks.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}
	if cur, err := readInfo(l.infoPath); err == nil && cur == l.info {
		_ = os.Remove(l.infoPath)
	}
	err := flock(l.f, syscall.LOCK_UN)
	if cerr := l.f.Close(); err == nil {
		err = cerr
	}
	l.f = nil
	return err
}

func readInfo(path string) (Info, error) {
	var info Info
	data, err := os.ReadFile(path)
	if err != nil {
		return info, err
	}
	if err := json.Unmarshal(data, &info); err != nil {
		return info, err
	}
	if info.PID <= 0 {
		return info, errors.New("holder file has no pid")
	}
	return info, nil
}

// ReadHolder reads name's advisory holder file. It does not test the lock.
func ReadHolder(name string) Holder {
	_, ip := Paths(name)
	info, err := readInfo(ip)
	if err != nil {
		return Holder{}
	}
	return Holder{Info: &info, Alive: pidAlive(info.PID)}
}

func pidAlive(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// State is one lock as lock status reports it.
type State struct {
	Name string `json:"name"`
	// Held comes from a non-blocking flock test, never from the holder file.
	Held bool `json:"held"`
	// Holder is the advisory holder file, if one exists. When Held is false
	// it is stale (its writer died without removing it).
	Holder      *Info  `json:"holder,omitempty"`
	HolderAlive bool   `json:"holder_alive"`
	LockPath    string `json:"lock_path"`
}

// Status tests one lock. It never creates the lock file: a name with no
// lock file is reported as free.
func Status(name string) (State, error) {
	if err := ValidateName(name); err != nil {
		return State{}, err
	}
	lp, _ := Paths(name)
	st := State{Name: name, LockPath: lp}
	h := ReadHolder(name)
	st.Holder, st.HolderAlive = h.Info, h.Alive
	f, err := os.OpenFile(lp, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return st, err
	}
	defer f.Close()
	// A shared non-blocking attempt fails only against an exclusive holder,
	// and two concurrent status calls do not trip over each other.
	switch err := flock(f, syscall.LOCK_SH|syscall.LOCK_NB); {
	case err == nil:
		_ = flock(f, syscall.LOCK_UN)
	case errors.Is(err, syscall.EWOULDBLOCK):
		st.Held = true
	default:
		return st, err
	}
	return st, nil
}

// List reports every lock with a lock or holder file in os.TempDir().
func List() ([]State, error) {
	entries, err := os.ReadDir(os.TempDir())
	if err != nil {
		return nil, err
	}
	seen := map[string]bool{}
	var names []string
	for _, e := range entries {
		n := e.Name()
		if !strings.HasPrefix(n, prefix) {
			continue
		}
		base := strings.TrimPrefix(n, prefix)
		var name string
		switch {
		case strings.HasSuffix(base, lockExt):
			name = strings.TrimSuffix(base, lockExt)
		case strings.HasSuffix(base, infoExt):
			name = strings.TrimSuffix(base, infoExt)
		default:
			continue
		}
		if ValidateName(name) != nil || seen[name] {
			continue
		}
		seen[name] = true
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]State, 0, len(names))
	for _, n := range names {
		st, err := Status(n)
		if err != nil {
			return nil, fmt.Errorf("lock %s: %w", n, err)
		}
		out = append(out, st)
	}
	return out, nil
}
