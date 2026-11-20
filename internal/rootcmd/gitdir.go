// gitdir.go derives the gitdir ccsbx-root mounts read-only into a fence. It exists because the
// gitdir was the one mount field root took from the record on trust: a client that sent an empty
// gitdir for a plain repository left `<workdir>/.git` writable inside the fence, and a fence can
// plant hooks there that the owner's next git command runs as the owner. Root now looks at the
// worktree's `.git` itself, through the pinned workdir descriptor, and the record must agree.
package rootcmd

import (
	"path/filepath"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/start"
)

// deriveGitDir returns the gitdir path to mount (read-only) and its pinned descriptor, or "" and nil
// when the workdir has no `.git`. The record's GitDir must equal what is derived: an empty field for
// a repository, a different path, or a path for a worktree without `.git` are all refused.
//
//	.git is a directory  -> that directory (plain repository)
//	.git is a file       -> the `gitdir:` target: a host-owned directory with a HEAD whose own
//	                        `gitdir` file points back at this worktree's .git (a linked worktree)
//	no .git              -> none
func (e *env) deriveGitDir(rec *start.Record, wd string, wpin *pinned, home string) (string, *pinned, error) {
	kind, content, child, err := dotGit(wpin.f)
	if err != nil {
		return "", nil, refuse("%s/.git: %v", wd, err)
	}
	mismatch := func(derived string) error {
		return refuse("the record's gitdir %q does not match the one derived from %s/.git (%q)", rec.GitDir, wd, derived)
	}
	switch kind {
	case "none":
		if rec.GitDir != "" {
			return "", nil, mismatch("")
		}
		return "", nil, nil
	case "dir":
		defer func() {
			if child != nil {
				child.Close()
			}
		}()
		derived := filepath.Join(wd, ".git")
		if rec.GitDir != derived {
			return "", nil, mismatch(derived)
		}
		p, err := pinFile(child, derived, rec.Uid)
		if err != nil {
			return "", nil, err
		}
		child = nil // owned by p now
		return derived, p, nil
	}
	// a .git file: "gitdir: <path>" (relative to the worktree, or absolute)
	target, ok := strings.CutPrefix(strings.TrimSpace(string(content)), "gitdir:")
	if !ok {
		return "", nil, refuse("%s/.git is a file that does not start with \"gitdir:\"", wd)
	}
	target = strings.TrimSpace(target)
	if target == "" || strings.ContainsAny(target, "\x00\n\r") {
		return "", nil, refuse("%s/.git names no usable gitdir", wd)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(wd, target)
	}
	derived, err := e.cfg.gitdir(filepath.Clean(target), rec.Uid, home)
	if err != nil {
		return "", nil, err
	}
	if rec.GitDir != derived {
		return "", nil, mismatch(derived)
	}
	gp, err := pin(e.d.OpenDir, derived, rec.Uid)
	if err != nil {
		return "", nil, err
	}
	// a git directory, and one that belongs to THIS worktree: its own `gitdir` file (written by git,
	// host-owned: the ownership walk runs on the staged mount) names this worktree's .git
	if !hasRegularChild(gp.f, "HEAD") {
		gp.close()
		return "", nil, refuse("gitdir %s is not a git directory (no HEAD)", derived)
	}
	back, err := readChild(gp.f, "gitdir")
	if err != nil {
		gp.close()
		return "", nil, refuse("gitdir %s has no back-reference to a worktree: %v", derived, err)
	}
	bp := strings.TrimSpace(string(back))
	if d, err := filepath.EvalSymlinks(filepath.Dir(bp)); !filepath.IsAbs(bp) || err != nil || filepath.Join(d, filepath.Base(bp)) != filepath.Join(wd, ".git") {
		gp.close()
		return "", nil, refuse("gitdir %s belongs to another worktree (it names %q)", derived, bp)
	}
	return derived, gp, nil
}
