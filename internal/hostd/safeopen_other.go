//go:build !(linux && (amd64 || arm64))

// The fallback of safeopen.go where openat2 does not exist (the Mac): weaker, and said so in the
// docs; it exists so the daemon builds everywhere, and refuses symlinks it can see with O_NOFOLLOW.
package hostd

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
)

// openBeneath without openat2 (the Mac): every component is lstat'ed and must not be a
// symlink, and the final open uses O_NOFOLLOW. This is weaker than the Linux path (a
// component can be swapped between the lstat and the open), which is why the Mac's hostd is
// not an evidence path; the fstat checks that follow still apply.
func openBeneath(root int, rel string, flags int) (int, error) {
	// root is a directory fd; resolve its path through /dev/fd-less means: the caller passes the
	// path through rootPath for this build.
	base := rootPath(root)
	if base == "" {
		return -1, syscall.ENOTSUP
	}
	cur := base
	for _, part := range strings.Split(rel, "/") {
		if part == "" || part == "." || part == ".." {
			return -1, syscall.EINVAL
		}
		cur = filepath.Join(cur, part)
		st, err := os.Lstat(cur)
		if err != nil {
			return -1, err
		}
		if st.Mode()&os.ModeSymlink != 0 {
			return -1, syscall.ELOOP
		}
	}
	return syscall.Open(cur, flags|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
}
