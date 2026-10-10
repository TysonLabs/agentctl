//go:build darwin || freebsd || netbsd || openbsd || linux

package cli

import (
	"syscall"
	"unsafe"
)

// isTerminal reports whether fd is a terminal: the termios read succeeds only
// on a tty, unlike a character-device test (/dev/null, /dev/zero pass that).
func isTerminal(fd uintptr) bool {
	var t syscall.Termios
	_, _, errno := syscall.Syscall(syscall.SYS_IOCTL, fd, ioctlGetTermios, uintptr(unsafe.Pointer(&t)))
	return errno == 0
}
