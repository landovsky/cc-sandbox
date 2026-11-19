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
