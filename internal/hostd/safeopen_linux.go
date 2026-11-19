//go:build linux && (amd64 || arm64)

package hostd

import (
	"fmt"
	"syscall"
	"unsafe"
)

const (
	sysOpenat2       = 437 // the same number on amd64 and arm64
	resolveNoMagic   = 0x02
	resolveNoSymlink = 0x04
	resolveBeneath   = 0x08
)

type openHow struct{ flags, mode, resolve uint64 }

// openBeneath opens rel under the directory fd root with openat2: no symlink anywhere on the
// path (RESOLVE_NO_SYMLINKS), no /proc magic links, and nothing that climbs out of root
// (RESOLVE_BENEATH). The kernel does this in one step, so there is no check-then-open race
// for the fence to win. O_NONBLOCK keeps a FIFO from hanging the daemon.
func openBeneath(root int, rel string, flags int) (int, error) {
	p, err := syscall.BytePtrFromString(rel)
	if err != nil {
		return -1, err
	}
	how := openHow{flags: uint64(flags | syscall.O_CLOEXEC | syscall.O_NONBLOCK), resolve: resolveBeneath | resolveNoSymlink | resolveNoMagic}
	for {
		fd, _, e := syscall.Syscall6(sysOpenat2, uintptr(root), uintptr(unsafe.Pointer(p)), uintptr(unsafe.Pointer(&how)), unsafe.Sizeof(how), 0, 0)
		if e == 0 {
			return int(fd), nil
		}
		if e == syscall.EINTR {
			continue
		}
		if e == syscall.ENOSYS {
			return -1, fmt.Errorf("openat2 needs Linux 5.6+")
		}
		return -1, e
	}
}
