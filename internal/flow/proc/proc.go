// Package proc holds the process-group plumbing shared by agentflow's
// runners: every runner starts its child with Setpgid, so ending the group
// ends the child's helpers too.
package proc

import (
	"errors"
	"syscall"
	"time"
)

// KillGroup sends SIGTERM to the whole process group, then SIGKILL after
// grace if the process is still alive. It always reaps the process. The bool
// reports that the direct child had already been reaped before signaling, so
// its exit was natural even if descendants still needed cleanup.
func KillGroup(pid int, grace time.Duration, waitCh <-chan error) (bool, error) {
	naturalExit := errors.Is(syscall.Kill(pid, 0), syscall.ESRCH)
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	select {
	case err := <-waitCh:
		_ = syscall.Kill(-pid, syscall.SIGKILL) // stragglers in the group
		return naturalExit, err
	case <-time.After(grace):
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return naturalExit, <-waitCh
}

// CleanupGroup terminates descendants left in the process group after the
// direct child exits normally. It is bounded even if a descendant ignores
// SIGTERM.
func CleanupGroup(pid int, grace time.Duration) {
	if err := syscall.Kill(-pid, 0); err != nil {
		return
	}
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	deadline := time.NewTimer(grace)
	defer deadline.Stop()
	tick := time.NewTicker(20 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-tick.C:
			if err := syscall.Kill(-pid, 0); err != nil {
				return
			}
		case <-deadline.C:
			_ = syscall.Kill(-pid, syscall.SIGKILL)
			return
		}
	}
}
