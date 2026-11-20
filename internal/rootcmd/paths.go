// The path policy of ccsbx-root: which workdirs and gitdirs may be mounted into a fence. It exists
// because root mounts them, so a path the daemon wrote into a record must be re-checked here,
// against the roots the root-owned config names, with no symlink anywhere in it and the right
// owner, before docker sees it.
package rootcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/landovsky/cc-sandbox/internal/backend"
)

// Refusal is a policy refusal: the verb exits 2 and says why.
type Refusal struct{ Msg string }

func (r *Refusal) Error() string { return r.Msg }

func refuse(format string, a ...any) error { return &Refusal{fmt.Sprintf(format, a...)} }

// cleanPath refuses a path that is not clean and absolute, or has a character that changes how a
// mount spec is read, or has any symlink in it (the resolved path must equal the given one).
func cleanPath(p string) (string, error) {
	if err := backend.CheckPath(p); err != nil {
		return "", refuse("%v", err)
	}
	if !filepath.IsAbs(p) || filepath.Clean(p) != p {
		return "", refuse("path %q must be clean and absolute", p)
	}
	r, err := filepath.EvalSymlinks(p)
	if err != nil {
		return "", refuse("path %q: %v", p, err)
	}
	if r != p {
		return "", refuse("path %q goes through a symlink (it resolves to %q)", p, r)
	}
	return r, nil
}

// insideRoot returns the allowed root that strictly contains p, or "".
func (c *Config) insideRoot(p string) string {
	for _, r := range c.AllowedRoots {
		if strings.HasPrefix(p, r+string(filepath.Separator)) {
			return r
		}
	}
	return ""
}

func ownerUID(p string) (int, error) {
	st, err := os.Lstat(p)
	if err != nil {
		return 0, err
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return 0, fmt.Errorf("%s: no owner information", p)
	}
	return int(sys.Uid), nil
}

// workdir validates a record's workdir and returns it with the root that contains it. It must be
// a real directory, strictly inside an allowed root, with no symlink in its path, owned by the
// session's owner, and not overlap the daemon's or root's state or a fixed fence mount.
func (c *Config) workdir(p string, owner int) (string, string, error) {
	rp, err := cleanPath(p)
	if err != nil {
		return "", "", err
	}
	root := c.insideRoot(rp)
	if root == "" {
		return "", "", refuse("workdir %s is not inside an allowed root (%s)", rp, strings.Join(c.AllowedRoots, ", "))
	}
	st, err := os.Lstat(rp)
	if err != nil || !st.IsDir() {
		return "", "", refuse("workdir %s is not a directory", rp)
	}
	if u, err := ownerUID(rp); err != nil || u != owner {
		return "", "", refuse("workdir %s is not owned by the session's owner (uid %d)", rp, owner)
	}
	for _, s := range []string{c.StateDir, c.RunDir} {
		if rp == s || strings.HasPrefix(rp, s+"/") || strings.HasPrefix(s, rp+"/") {
			return "", "", refuse("workdir %s overlaps %s", rp, s)
		}
	}
	// the hidden-directory and fixed-mount rules of the direct backend, against the same root
	if _, err := backend.CheckWorkdir(rp, root, c.RunDir); err != nil {
		return "", "", refuse("%v", err)
	}
	return rp, root, nil
}

// gitdir validates the gitdir a record carries: same path rules, inside an allowed root, owned by
// the session's owner. (The host-ownership and mode walk runs again in backend.Prepare.)
func (c *Config) gitdir(p string, owner int) (string, error) {
	rp, err := cleanPath(p)
	if err != nil {
		return "", err
	}
	if c.insideRoot(rp) == "" {
		return "", refuse("gitdir %s is not inside an allowed root", rp)
	}
	if u, err := ownerUID(rp); err != nil || u != owner {
		return "", refuse("gitdir %s is not owned by the session's owner (uid %d)", rp, owner)
	}
	return rp, nil
}

// binary validates a program root is about to run: absolute, resolved, owned by root, not
// writable by others, in a directory that is not either.
func (c *Config) binary(p string) (string, error) {
	if err := trustedFile(p, c.TrustedUID, true); err != nil {
		return "", refuse("binary %s: %v", p, err)
	}
	return filepath.EvalSymlinks(p)
}
