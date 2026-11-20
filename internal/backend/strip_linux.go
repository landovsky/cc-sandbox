// strip_linux.go clears setuid and setgid bits that a fence left on files in its worktree. The
// fence writes as uid 10001; a setuid-10001 file there, run on the host by its owner, would make
// the owner uid 10001, whose outboxes and homes every fence shares. The fence's seccomp profile
// already refuses to set the bits; this is the second layer, run by ccsbx-root at stop, at gc and
// before each new generation's mounts.

//go:build linux

package backend

import (
	"fmt"
	"os"
	"syscall"
)

const (
	maxStripEntries = 2_000_000
	maxStripDepth   = 64
)

// StripSetid walks root (an open directory) with directory fds and O_NOFOLLOW and clears S_ISUID
// and S_ISGID on every regular file not owned by owner. It never follows a symlink, never leaves
// root's filesystem, and changes the mode on the opened file (not on a path that could be swapped
// meanwhile). logf is told about each file changed. It returns how many.
func StripSetid(root *os.File, owner int, logf func(format string, args ...any)) (int, error) {
	return stripSetid(root, owner, FenceUID, logf)
}

// stripSetid is StripSetid with the fence's id as a parameter (tests cannot create files owned by 10001).
func stripSetid(root *os.File, owner, fence int, logf func(format string, args ...any)) (int, error) {
	var rst syscall.Stat_t
	if err := syscall.Fstat(int(root.Fd()), &rst); err != nil {
		return 0, err
	}
	dfd, err := syscall.Openat(int(root.Fd()), ".", syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return 0, fmt.Errorf("strip: %w", err)
	}
	n, seen := 0, 0
	err = stripDir(dfd, "", 0, rst.Dev, owner, fence, logf, &n, &seen)
	return n, err
}

// stripDir takes ownership of dfd and closes it.
func stripDir(dfd int, rel string, depth int, dev uint64, owner, fence int, logf func(string, ...any), n, seen *int) error {
	d := os.NewFile(uintptr(dfd), rel)
	defer d.Close()
	if depth > maxStripDepth {
		return fmt.Errorf("strip: %q is deeper than %d", rel, maxStripDepth)
	}
	names, err := d.Readdirnames(-1)
	if err != nil {
		return fmt.Errorf("strip %q: %w", rel, err)
	}
	for _, name := range names {
		if *seen++; *seen > maxStripEntries {
			return fmt.Errorf("strip: more than %d entries", maxStripEntries)
		}
		child := name
		if rel != "" {
			child = rel + "/" + name
		}
		pfd, err := syscall.Openat(dfd, name, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			continue // gone meanwhile
		}
		var st syscall.Stat_t
		err = syscall.Fstat(pfd, &st)
		syscall.Close(pfd)
		if err != nil {
			continue
		}
		switch st.Mode & syscall.S_IFMT {
		case syscall.S_IFDIR:
			if uint64(st.Dev) != dev {
				continue // another filesystem: not ours to walk
			}
			sub, err := syscall.Openat(dfd, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
			if err != nil {
				continue
			}
			if err := stripDir(sub, child, depth+1, dev, owner, fence, logf, n, seen); err != nil {
				return err
			}
		case syscall.S_IFREG:
			// Only what the fence could have made: owned by the fence's uid or gid. A hardlink in the
			// worktree to some other setuid file (/usr/bin/sudo, with fs.protected_hardlinks=0) is the
			// SAME inode, and clearing its bit would clear it everywhere.
			if st.Mode&(syscall.S_ISUID|syscall.S_ISGID) == 0 || int(st.Uid) == owner || (int(st.Uid) != fence && int(st.Gid) != fence) {
				continue
			}
			ffd, err := syscall.Openat(dfd, name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
			if err != nil {
				continue
			}
			var fst syscall.Stat_t
			if syscall.Fstat(ffd, &fst) == nil && fst.Ino == st.Ino && fst.Dev == st.Dev && fst.Mode&syscall.S_IFMT == syscall.S_IFREG {
				if err := syscall.Fchmod(ffd, fst.Mode&^(syscall.S_ISUID|syscall.S_ISGID)&0o7777); err != nil {
					syscall.Close(ffd)
					return fmt.Errorf("strip %q: %w", child, err)
				}
				*n++
				if logf != nil {
					logf("cleared setuid/setgid on %q (uid %d, mode %#o)", child, fst.Uid, fst.Mode&0o7777)
				}
			}
			syscall.Close(ffd)
		}
	}
	return nil
}
