// paths.go checks the paths `side serve` is given before it trusts anything under them. It exists because the
// sidecar's state (the relay's replay ledger, the session journal, the routes allowlist, the extra CA) decides what
// the fence may do, so each must live where only the sidecar can write, and a launcher typo that points one at a
// fence-writable mount (/work, the outbox) would hand the fence that decision.
package side

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// DefaultFenceWritable are the mounts the fence container can write in a pod (internal/k8s/spec.go).
var DefaultFenceWritable = []string{"/work", "/run/ccsbx/out", "/home/agent"}

// resolve returns the path with symlinks followed as far as it exists, so a link out of a private
// directory into a fence-writable one cannot hide the real place.
func resolve(p string) string {
	p = filepath.Clean(p)
	for q := p; q != "/" && q != "."; q = filepath.Dir(q) {
		if r, err := filepath.EvalSymlinks(q); err == nil {
			return filepath.Join(r, strings.TrimPrefix(p, q))
		}
	}
	return p
}

// underAny reports whether p is one of roots or below one.
func underAny(p string, roots []string) (string, bool) {
	rp := resolve(p)
	for _, r := range roots {
		for _, cand := range []string{filepath.Clean(r), resolve(r)} {
			if rp == cand || strings.HasPrefix(rp, cand+string(filepath.Separator)) {
				return r, true
			}
		}
	}
	return "", false
}

// ownDir makes dir (0700) if it is missing and requires it to be a plain directory of uid with no group or
// other access: the same bar as the control directory. In a pod the volume root is 0777 and root-owned, so the
// sidecar's directory is one level below the mount, made here.
func ownDir(dir string, uid int) error {
	if !filepath.IsAbs(dir) || filepath.Clean(dir) != dir {
		return fmt.Errorf("%q must be a clean absolute path", dir)
	}
	if err := os.Mkdir(dir, 0o700); err == nil {
		if err := os.Chmod(dir, 0o700); err != nil { // not subject to the umask
			return err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return err
	}
	return checkControlDirAs(dir, uid)
}

// trustedFile requires a regular file (not a link) that nobody but its owner can write, owned by uid or root.
func trustedFile(path string, uid int) error {
	fi, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s has mode %o; group and others must not be able to write it", path, fi.Mode().Perm())
	}
	if !ownedBy(fi, uid) && !ownedBy(fi, 0) {
		return fmt.Errorf("%s is owned by neither uid %d nor root", path, uid)
	}
	return nil
}

// writePublic writes a file the FENCE must be able to read (a reply, a marker in in/): created O_EXCL as a temp file
// in the same directory, set to 0644 explicitly (the process umask is not trusted to be anything), then renamed over
// the final name, so the fence never sees half of it. Private files of ours are never written through here.
func writePublic(path string, data []byte) error {
	tmp := filepath.Join(filepath.Dir(path), "."+filepath.Base(path)+".tmp")
	os.Remove(tmp)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Chmod(0o644)
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}
