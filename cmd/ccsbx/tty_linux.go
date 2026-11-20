// tty_linux.go answers "is this fd a terminal" on Linux. It exists because the standard library
// has no such call and the repo takes no third-party modules; tty_darwin.go is the macOS twin.

package main

import (
	"syscall"
	"unsafe"
)

// isTerminal is a real isatty: /dev/null is a character device but not a terminal.
func isTerminal(fd uintptr) bool {
	var t syscall.Termios
	_, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, syscall.TCGETS, uintptr(unsafe.Pointer(&t)))
	return e == 0
}
