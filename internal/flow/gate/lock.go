package gate

// Named cross-process locks. The convention is shared with
// `agentflow lock run`, so a gate and any other command holding the same name
// exclude each other: flock(LOCK_EX) on $TMPDIR/agentflow-lock-<name>.lock,
// and while held, holder info in $TMPDIR/agentflow-lock-<name>.json. This
// file is self-contained so it can move to a shared package.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/TysonLabs/agentctl/internal/gatespec"
)

// LockHolder is the holder file's content.
type LockHolder struct {
	PID   int    `json:"pid"`
	Cmd   string `json:"cmd"`
	Dir   string `json:"dir"`
	Since string `json:"since"` // RFC 3339
}

// LockPaths returns the lock file and the holder file of a named lock.
func LockPaths(name string) (lock, holder string) {
	base := filepath.Join(os.TempDir(), "agentflow-lock-"+name)
	return base + ".lock", base + ".json"
}

// lockPoll is how often a waiter retries the lock.
var lockPoll = 200 * time.Millisecond

// AcquireLock takes the named lock, waiting until it is free or ctx ends.
// onWait is called once, with the current holder (nil if unknown), when the
// lock is busy. The returned release removes the holder file and unlocks.
func AcquireLock(ctx context.Context, name string, me LockHolder, onWait func(*LockHolder)) (release func(), waited time.Duration, err error) {
	if err := gatespec.CheckLockName(name); err != nil || name == "" {
		return nil, 0, fmt.Errorf("bad lock name %q", name)
	}
	lockPath, holderPath := LockPaths(name)
	f, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, 0, fmt.Errorf("opening lock %s: %v", lockPath, err)
	}
	start := time.Now()
	notified := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EINTR) {
			f.Close()
			return nil, 0, fmt.Errorf("locking %s: %v", lockPath, err)
		}
		if !notified {
			notified = true
			if onWait != nil {
				onWait(readHolder(holderPath))
			}
		}
		select {
		case <-ctx.Done():
			f.Close()
			return nil, time.Since(start), ctx.Err()
		case <-time.After(lockPoll):
		}
	}
	waited = time.Since(start)
	me.Since = time.Now().UTC().Format(time.RFC3339)
	// The lock works without the holder file; waiters just see less.
	_ = writeHolder(holderPath, me)
	return func() {
		_ = os.Remove(holderPath)
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, waited, nil
}

func readHolder(path string) *LockHolder {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var h LockHolder
	if json.Unmarshal(b, &h) != nil {
		return nil
	}
	return &h
}

func writeHolder(path string, h LockHolder) error {
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".agentflow-lock-*.tmp")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(append(b, '\n')); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}
