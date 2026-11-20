// tty_darwin.go answers "is this fd a terminal" on macOS. It exists because the standard library
// has no such call and the repo takes no third-party modules; tty_linux.go is the Linux twin.

package main

import (
	"syscall"
	"unsafe"
)

// isTerminal is a real isatty: /dev/null is a character device but not a terminal.
func isTerminal(fd uintptr) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TIOCGETA, uintptr(unsafe.Pointer(&t)))
	return e == 0
}
