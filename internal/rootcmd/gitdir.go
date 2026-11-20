// gitdir.go derives the gitdir ccsbx-root mounts read-only into a fence. It exists because the
// gitdir was the one mount field root took from the record on trust: a client that sent an empty
// gitdir for a plain repository left `<workdir>/.git` writable inside the fence, and a fence can
// plant hooks there that the owner's next git command runs as the owner. Root now looks at the
// worktree's `.git` itself, through the pinned workdir descriptor, and the record must agree.
package rootcmd

import (
	"path/filepath"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/backend"
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
	derived, p, err := e.gitdirOf(rec.Uid, wd, wpin, home)
	if err != nil {
		return "", nil, err
	}
	if rec.GitDir != derived {
		p.close()
		return "", nil, refuse("the record's gitdir %q does not match the one derived from %s/.git (%q)", rec.GitDir, wd, derived)
	}
	return derived, p, nil
}

// gitdirOf derives the gitdir of a worktree from its own .git, through the pinned descriptor, for a worktree
// whose owner is uid; "" and nil when it has no .git. It compares nothing with a record: deriveGitDir does,
// and worktree-check (which has no record) asks only whether there is one.
func (e *env) gitdirOf(uid int, wd string, wpin *pinned, home string) (string, *pinned, error) {
	kind, content, child, err := dotGit(wpin.f)
	if err != nil {
		return "", nil, refuse("%s/.git: %v", wd, err)
	}
	switch kind {
	case "none":
		return "", nil, nil
	case "dir":
		defer func() {
			if child != nil {
				child.Close()
			}
		}()
		derived := filepath.Join(wd, ".git")
		if !hasRegularChild(child, "HEAD") {
			return "", nil, refuse("%s is not a git directory (no HEAD)", derived)
		}
		p, err := pinFile(child, derived, uid)
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
	// (a linked worktree's gitdir also has a `commondir` file naming the main repository's directory; it is
	// deliberately NOT followed or mounted: only this worktree's own gitdir is, read-only, so it cannot widen
	// the fence. Do not "fix" that into a second mount.)
	derived, err := e.cfg.gitdir(filepath.Clean(target), uid, home)
	if err != nil {
		return "", nil, err
	}
	// The shape hostd accepts, and nothing wider: <repo>/.git/worktrees/<name> under an allowed root. A linked
	// worktree of a bare repository (<bare>/worktrees/<name>) was accepted here and refused by hostd's
	// backend.CheckGitDirLexical, so a start hostd would not send could still be run by hand; both sides now use the
	// one check, and refusing the bare layout is the choice.
	if err := backend.CheckGitDirLexical(derived, wd, strings.Join(e.cfg.AllowedRoots, ",")); err != nil {
		return "", nil, refuse("%v", err)
	}
	gp, err := pin(e.d.OpenDir, derived, uid)
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
