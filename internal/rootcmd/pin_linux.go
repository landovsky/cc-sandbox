// pin_linux.go opens a directory so that the thing checked is the thing mounted. Root validates a
// workdir or gitdir that the session's owner can rename or replace at any moment (the owner
// owns every parent), so a path checked now may not be the path docker resolves later. Here the
// directory is opened component by component with O_NOFOLLOW, an fd is held, the checks run on
// the fd, and the mount is made from the fd: the kernel resolves the inode, not a name.

//go:build linux

package rootcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// oPath is O_PATH (not in package syscall).
const oPath = 0x200000

// umountNoFollow is UMOUNT_NOFOLLOW (not in package syscall): never unmount through a symlink.
const umountNoFollow = 0x8

// openDirNoFollow opens the absolute, clean path as an O_PATH directory fd, walking from "/" one
// component at a time with O_NOFOLLOW. A symlink at any component fails the open (ELOOP).
func openDirNoFollow(path string) (*os.File, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("%q is not a clean absolute path", path)
	}
	fd, err := syscall.Open("/", oPath|syscall.O_DIRECTORY|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, err
	}
	for _, c := range strings.Split(strings.TrimPrefix(path, "/"), "/") {
		if c == "" {
			continue
		}
		next, err := syscall.Openat(fd, c, oPath|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		syscall.Close(fd)
		if err != nil {
			return nil, fmt.Errorf("%s: %w (a symlink or a non-directory in the path)", path, err)
		}
		fd = next
	}
	return os.NewFile(uintptr(fd), path), nil
}

// realMounter binds an open directory onto a root-owned mountpoint with nosuid,nodev (and
// read-only when asked). Reading /proc/self/fd/N follows the fd, not a path that could change.
type realMounter struct{}

func (realMounter) Bind(src *os.File, target string, readonly bool) error {
	p := fmt.Sprintf("/proc/self/fd/%d", src.Fd())
	if err := syscall.Mount(p, target, "", syscall.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s: %w", target, err)
	}
	flags := uintptr(syscall.MS_REMOUNT | syscall.MS_BIND | syscall.MS_NOSUID | syscall.MS_NODEV)
	if readonly {
		flags |= syscall.MS_RDONLY
	}
	if err := syscall.Mount("", target, "", flags, ""); err != nil {
		syscall.Unmount(target, syscall.MNT_DETACH|umountNoFollow)
		return fmt.Errorf("remount %s: %w", target, err)
	}
	return nil
}

func (realMounter) Unmount(target string) error {
	if err := syscall.Unmount(target, syscall.MNT_DETACH|umountNoFollow); err != nil && err != syscall.EINVAL && err != syscall.ENOENT {
		return err
	}
	return nil
}

// RealMounter is the production Mounter.
func RealMounter() Mounter { return realMounter{} }

// OpenDir is the production directory opener.
func OpenDir(path string) (*os.File, error) { return openDirNoFollow(path) }
