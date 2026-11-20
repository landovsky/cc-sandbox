// sweep_linux.go is the helper-mode sweep of the per-node home (invariant 25), done by ccsbx-root
// itself with directory fds. It exists so the session image never runs as container root with
// extra capabilities: that would turn any unreviewed image into host root.

//go:build linux

package backend

import (
	"fmt"
	"strings"
	"syscall"
)

// oPath is O_PATH (not in package syscall): an fd that names an entry without opening it, so a
// symlink can be examined as a link.
const oPath = 0x200000

// sweepHomeGo is the helper-mode sweep: the same moves as sweepScript, done by this process (root)
// and not by a container. Every step goes through a directory fd and O_NOFOLLOW, because the home
// is written by the fence: a path it made a symlink, or a directory it replaced, must never be
// followed. A steering path whose parent is not a plain directory is moved whole (the parent, as
// the link or file it is). The quarantine is root-owned 0700 and nothing is made readable.
func (s Spec) sweepHomeGo() ([]string, error) {
	home, err := syscall.Open(s.HomeDir(), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("sweep: open home: %w", err)
	}
	defer syscall.Close(home)
	quar, err := syscall.Open(s.QuarantineDir(), syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("sweep: open quarantine: %w", err)
	}
	defer syscall.Close(quar)
	var moved []string
	for _, rel := range steeringHome {
		got, err := sweepOne(home, quar, rel)
		if err != nil {
			return moved, err
		}
		if got != "" {
			moved = append(moved, got)
		}
	}
	return moved, nil
}

// sweepOne moves rel (or its first non-directory ancestor) from the home fd to the quarantine fd.
func sweepOne(home, quar int, rel string) (string, error) {
	parts := strings.Split(rel, "/")
	hd, qd := home, quar
	var opened []int
	defer func() {
		for _, fd := range opened {
			syscall.Close(fd)
		}
	}()
	for i, name := range parts {
		// is there anything at name, and is it a plain directory (when more follows)?
		pfd, err := syscall.Openat(hd, name, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err == syscall.ENOENT {
			return "", nil
		}
		if err != nil {
			return "", fmt.Errorf("sweep %s: %w", rel, err)
		}
		var st syscall.Stat_t
		err = syscall.Fstat(pfd, &st)
		syscall.Close(pfd)
		if err != nil {
			return "", fmt.Errorf("sweep %s: %w", rel, err)
		}
		last := i == len(parts)-1
		if last || st.Mode&syscall.S_IFMT != syscall.S_IFDIR {
			// move this entry, whatever it is, as itself (a symlink is moved as a link)
			if err := syscall.Renameat(hd, name, qd, name); err != nil {
				return "", fmt.Errorf("sweep %s: %w", rel, err)
			}
			return strings.Join(parts[:i+1], "/"), nil
		}
		// a plain directory: descend in both trees (creating it in the quarantine)
		nh, err := syscall.Openat(hd, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return "", fmt.Errorf("sweep %s: %w", rel, err)
		}
		opened = append(opened, nh)
		if err := syscall.Mkdirat(qd, name, 0o700); err != nil && err != syscall.EEXIST {
			return "", fmt.Errorf("sweep %s: %w", rel, err)
		}
		nq, err := syscall.Openat(qd, name, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
		if err != nil {
			return "", fmt.Errorf("sweep %s: %w", rel, err)
		}
		opened = append(opened, nq)
		hd, qd = nh, nq
	}
	return "", nil
}
