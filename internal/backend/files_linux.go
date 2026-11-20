// files_linux.go writes the files a generation starts with (session.json, scope.json, brief.md,
// the managed settings) without following links. It exists because root writes them into
// directories that other uids may have touched: a symlink planted where a file is about to be
// created would make a plain write land on the link's target, as root. Each file is made with
// openat on the directory's descriptor, O_CREAT|O_EXCL|O_NOFOLLOW: it must not already exist.

//go:build linux

package backend

import (
	"fmt"
	"os"
	"strings"
	"syscall"
)

// writeGenFile writes name (one directory below genDir, e.g. "in/brief.md") with mode 0644.
func writeGenFile(genDir, name string, data []byte) error {
	dir, file, ok := strings.Cut(name, "/")
	if !ok || dir == "" || file == "" || strings.ContainsAny(file, "/\x00") || dir == ".." || file == ".." || dir == "." || file == "." {
		return fmt.Errorf("generation file %q: want <dir>/<name>", name)
	}
	g, err := syscall.Open(genDir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("generation file %q: %w", name, err)
	}
	defer syscall.Close(g)
	d, err := syscall.Openat(g, dir, syscall.O_RDONLY|syscall.O_DIRECTORY|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("generation file %q: %w", name, err)
	}
	defer syscall.Close(d)
	fd, err := syscall.Openat(d, file, syscall.O_WRONLY|syscall.O_CREAT|syscall.O_EXCL|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o644)
	if err != nil {
		return fmt.Errorf("generation file %q: %w (it must not exist, and a symlink is never followed)", name, err)
	}
	f := os.NewFile(uintptr(fd), name)
	defer f.Close()
	if _, err := f.Write(data); err != nil {
		return err
	}
	return f.Chmod(0o644) // the umask must not decide who can read what the fence is given
}
