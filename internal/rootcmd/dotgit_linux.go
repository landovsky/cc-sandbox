// dotgit_linux.go looks at a worktree's `.git` through the already-open (pinned) worktree
// descriptor, so that what root learns about the repository is about the directory it is going to
// mount and not about a path the owner can swap. It exists because the gitdir is the one mount field
// root used to take from the record on trust: root derives it itself from what is really there.

//go:build linux

package rootcmd

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// maxDotGit bounds what is read from a `.git` file or a back-reference file.
const maxDotGit = 4096

// dotGit says what <wd>/.git is: "none", "dir" (child is its O_PATH descriptor) or "file" (content
// is its bytes, at most maxDotGit). A symlink or any other kind is an error.
func dotGit(wd *os.File) (kind string, content []byte, child *os.File, err error) {
	fd, err := syscall.Openat(int(wd.Fd()), ".git", oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err == syscall.ENOENT {
		return "none", nil, nil, nil
	}
	if err != nil {
		return "", nil, nil, fmt.Errorf(".git: %w", err)
	}
	var st syscall.Stat_t
	if err := syscall.Fstat(fd, &st); err != nil {
		syscall.Close(fd)
		return "", nil, nil, err
	}
	switch st.Mode & syscall.S_IFMT {
	case syscall.S_IFDIR:
		return "dir", nil, os.NewFile(uintptr(fd), ".git"), nil
	case syscall.S_IFREG:
		syscall.Close(fd)
		b, err := readChild(wd, ".git")
		return "file", b, nil, err
	}
	syscall.Close(fd)
	return "", nil, nil, fmt.Errorf(".git is a symlink or not a plain file or directory")
}

// readChild reads the regular file `name` directly inside dir (an open directory), without
// following a link, at most maxDotGit bytes; a larger file is an error.
func readChild(dir *os.File, name string) ([]byte, error) {
	fd, err := syscall.Openat(int(dir.Fd()), name, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	st, err := f.Stat()
	if err != nil || !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", name)
	}
	if st.Size() > maxDotGit {
		return nil, fmt.Errorf("%s is larger than %d bytes", name, maxDotGit)
	}
	return io.ReadAll(io.LimitReader(f, maxDotGit+1))
}

// hasRegularChild reports whether dir has a regular file `name` (not a link).
func hasRegularChild(dir *os.File, name string) bool {
	fd, err := syscall.Openat(int(dir.Fd()), name, oPath|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return false
	}
	defer syscall.Close(fd)
	var st syscall.Stat_t
	return syscall.Fstat(fd, &st) == nil && st.Mode&syscall.S_IFMT == syscall.S_IFREG
}
