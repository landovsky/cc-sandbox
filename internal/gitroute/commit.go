// The two things the host does with git on a fenced worktree besides reading: list what changed
// since a base (ChangedFiles) and make a commit (Commit). The commit carries a Fenced-Session
// trailer and, when a steering file changed, a Steering-Files trailer, both written by the host
// from its own view of the staged change, never from the agent's message (invariants 24 and 25).
//
// PRECONDITION (the live-fence race blocks F3 and C22): Commit must run while the fence is not
// writing to the worktree. Git checks a path with lstat and then opens it, and open follows
// symlinks, so a fence that swaps a file for a symlink in that window can get a host file staged.
// The scan below refuses symlinks that point outside the worktree, special files and oversized
// trees among what git would add, but it cannot close the race itself. So Commit takes a Stopped
// proof from the owner of the session record and refuses without one. Reads that return worktree
// content are host-only: see RunForFence.
package gitroute

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

var sessionRe = regexp.MustCompile(`^[A-Za-z0-9/_.:-]{1,120}$`)

// Change is one entry of `git diff --raw`.
type Change struct{ OldMode, NewMode, Status, Path string }

// changes parses `git diff --raw -z` output: ":old new oldsha newsha S\0path\0".
func changes(out string) []Change {
	var cs []Change
	f := splitZ(out)
	for i := 0; i+1 < len(f); i += 2 {
		m := strings.Fields(strings.TrimPrefix(f[i], ":"))
		if len(m) < 5 {
			continue
		}
		cs = append(cs, Change{OldMode: m[0], NewMode: m[1], Status: m[4], Path: f[i+1]})
	}
	return cs
}

// ChangedFiles lists files changed between base and HEAD (three-dot: since the merge base).
func (r *Runner) ChangedFiles(base string) ([]string, error) {
	cs, err := r.changedSince(base)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, c := range cs {
		out = append(out, c.Path)
	}
	return out, nil
}

func (r *Runner) changedSince(base string) ([]Change, error) {
	if strings.HasPrefix(base, "-") {
		return nil, fmt.Errorf("%w: base %q", ErrRefused, base)
	}
	out, err := r.Run("diff", "--raw", "--no-renames", "-z", base+"...HEAD", "--")
	if err != nil {
		return nil, err
	}
	return changes(out), nil
}

// SteeringSince returns what steering-check reports: steering files changed since base, symlinks
// and gitlinks that changed (a link can point a steering name anywhere), edits behind a steering
// symlink, and steering files that are in the worktree but not in HEAD, ignored ones included.
func (r *Runner) SteeringSince(base string) ([]string, error) {
	// first: what the configs say git will read or run. An include file inside the worktree is
	// flagged whether it changed or not, and does not depend on git's own change detection.
	g, err := r.configGuards()
	if err != nil {
		return nil, err
	}
	st, links, err := r.steeringSince(base, g)
	if err != nil && !errors.Is(err, ErrIncomplete) {
		// With an include in the config, H1 refuses to run git at all. The included files that
		// live in the worktree are already the finding; report them rather than a bare refusal.
		if errors.Is(err, ErrRefused) && strings.Contains(err.Error(), "include") && len(g.Forced) > 0 {
			return withLinks(nil, g.Forced), nil
		}
		return nil, err
	}
	return withLinks(append(st, g.Forced...), links), err
}

func (r *Runner) steeringSince(base string, g Guards) (st, links []string, err error) {
	cs, err := r.changedSince(base)
	if err != nil {
		return nil, nil, err
	}
	links = linkPaths(cs)
	files := append([]string{}, links...)
	for _, c := range cs {
		files = append(files, c.Path)
	}
	// gitlinks are invisible to diff here (submodule changes are ignored on purpose): compare trees
	if mb, err := r.Run("merge-base", base, "HEAD"); err == nil {
		old, e1 := r.gitlinks(strings.TrimSpace(mb))
		cur, e2 := r.gitlinks("HEAD")
		if e1 != nil || e2 != nil {
			return nil, nil, fmt.Errorf("reading gitlinks: %v %v", e1, e2)
		}
		for p, sha := range cur {
			if old[p] != sha {
				links = append(links, p)
				files = append(files, p)
			}
		}
	}
	pend, err := r.Pending()
	if err != nil {
		return nil, nil, err
	}
	st, err = r.SteeringFiles(append(files, pend...))
	if err != nil {
		return nil, nil, err
	}
	// what the owner's own git would run: changes under core.hooksPath and include.path, nested repos
	extra := underAny(append(append([]string{}, files...), pend...), g.Prefixes)
	nested, nerr := r.NestedRepos() // ErrIncomplete comes with what was found
	if nerr != nil && !errors.Is(nerr, ErrIncomplete) {
		return nil, nil, nerr
	}
	return append(st, extra...), append(append(links, extra...), nested...), nerr
}

// withLinks adds the changed symlink and gitlink paths to a steering list: a link can point a
// steering name anywhere, so every new or changed one is shown for review.
func withLinks(st, links []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, p := range append(append([]string{}, st...), links...) {
		if !seen[p] {
			seen[p] = true
			out = append(out, p)
		}
	}
	sort.Strings(out)
	return out
}

// linkPaths are the paths of changes whose new mode is a symlink or a gitlink.
func linkPaths(cs []Change) []string {
	var out []string
	for _, c := range cs {
		if c.NewMode == "120000" || c.NewMode == "160000" {
			out = append(out, c.Path)
		}
	}
	return out
}

// gitlinks maps path -> object id for every 160000 entry of a tree-ish.
func (r *Runner) gitlinks(rev string) (map[string]string, error) {
	m := map[string]string{}
	out, err := r.Run("ls-tree", "-r", "-z", rev)
	if err != nil {
		return m, err
	}
	for _, e := range splitZ(out) { // "<mode> <type> <sha>\t<path>"
		meta, p, ok := strings.Cut(e, "\t")
		if f := strings.Fields(meta); ok && len(f) == 3 && f[0] == "160000" {
			m[p] = f[2]
		}
	}
	return m, nil
}

// indexGitlinks is the same for the staged index.
func (r *Runner) indexGitlinks() (map[string]string, error) {
	m := map[string]string{}
	out, err := r.Run("ls-files", "-s", "-z")
	if err != nil {
		return m, err
	}
	for _, e := range splitZ(out) { // "<mode> <sha> <stage>\t<path>"
		meta, p, ok := strings.Cut(e, "\t")
		if f := strings.Fields(meta); ok && len(f) == 3 && f[0] == "160000" {
			m[p] = f[1]
		}
	}
	return m, nil
}

// Stopped is proof, from whoever owns the session record, that no fence generation is running
// against this worktree. Commit refuses without it, so F3/C22 cannot call Commit on a live fence
// by accident. It is a seam, not a lock: the owner must not hand out a proof it has not checked.
//
// Hold is called once, before anything is staged: it returns only if no generation is running, and
// the owner KEEPS the answer true until release is called, which Commit does when it is finished
// (the daemon holds its session lock for that long, so a resume cannot start a fence in between).
type Stopped interface {
	Hold() (release func(), err error)
}

// StoppedFunc adapts a function that only checks (and holds nothing) to Stopped. Tests use it;
// the daemon's proof (start.StoppedProof) holds its lock.
type StoppedFunc func() error

func (f StoppedFunc) Hold() (func(), error) { return func() {}, f() }

// Commit stages everything and commits with the Fenced-Session and Steering-Files trailers. The
// trailers come from the host's view of the staged change, never from the agent's message.
// No hooks run, the identity is the host's, and nothing is pushed. See the package comment above
// for the precondition (a quiet fence). A refusal leaves nothing staged.
func (r *Runner) Commit(session, message string, proof Stopped) (string, error) {
	if proof == nil {
		return "", fmt.Errorf("%w: commit needs proof that the fence is stopped (the worktree is still writable by the agent)", ErrRefused)
	}
	release, err := proof.Hold()
	if err != nil {
		return "", fmt.Errorf("%w: the fence is not stopped: %v", ErrRefused, err)
	}
	defer release() // held until the commit is made (or refused)
	if !sessionRe.MatchString(session) {
		return "", fmt.Errorf("%w: session id %q", ErrRefused, session)
	}
	if strings.TrimSpace(message) == "" {
		return "", fmt.Errorf("%w: empty commit message", ErrRefused)
	}
	// An agent-written message may try to forge a trailer; drop any trailer-looking line for ours.
	message = stripForgedTrailers(message)
	if err := r.scanPending(); err != nil {
		return "", err
	}
	if _, err := r.exec("add", "-A", "--", "."); err != nil {
		return "", err
	}
	cs, err := r.staged()
	if err == nil {
		err = r.checkStaged(cs)
	}
	if err != nil {
		r.unstage()
		return "", err
	}
	if len(cs) == 0 {
		return "", fmt.Errorf("nothing to commit")
	}
	var paths []string
	for _, c := range cs {
		paths = append(paths, c.Path)
	}
	st, err := r.SteeringFiles(paths)
	st = withLinks(st, linkPaths(cs))
	if err != nil {
		r.unstage()
		return "", err
	}
	if _, err := r.exec("commit", "-m", message, "-m", Trailers(session, st)); err != nil {
		return "", err
	}
	sha, err := r.Run("rev-parse", "HEAD")
	return strings.TrimSpace(sha), err
}

func (r *Runner) staged() ([]Change, error) {
	out, err := r.Run("diff", "--cached", "--raw", "--no-renames", "-z", "--")
	return changes(out), err
}

// checkStaged refuses what the host must not record: a path with a control character (it would
// forge a trailer line or write to a terminal), and a gitlink that HEAD does not already have
// (an embedded repo the agent made: recording it reads that repo, and its HEAD id would land in
// a pushed commit).
func (r *Runner) checkStaged(cs []Change) error {
	for _, c := range cs {
		for _, ch := range c.Path {
			if ch < 0x20 || ch == 0x7f {
				return fmt.Errorf("%w: staged path %q has a control character", ErrRefused, c.Path)
			}
		}
	}
	idx, err := r.indexGitlinks()
	if err != nil {
		return err
	}
	if len(idx) == 0 {
		return nil
	}
	head := map[string]string{}
	if _, err := r.Run("rev-parse", "--verify", "HEAD"); err == nil {
		if head, err = r.gitlinks("HEAD"); err != nil {
			return err
		}
	}
	for p, sha := range idx {
		if head[p] != sha {
			return fmt.Errorf("%w: %q is a nested repository (new or moved gitlink); the host does not record embedded repos", ErrRefused, p)
		}
	}
	return nil
}

// unstage puts the index back to HEAD (or empty) after a refusal.
func (r *Runner) unstage() {
	if _, err := r.Run("rev-parse", "--verify", "HEAD"); err == nil {
		r.exec("read-tree", "HEAD")
	} else {
		r.exec("read-tree", "--empty")
	}
}

// scanPending checks what `git add -A` would read: untracked files that .gitignore does not hide,
// and tracked files that changed. Ignored trees (.venv, node_modules, build output) are never
// opened by add, so they are not scanned and cannot block a commit. Each entry is lstat'ed, never
// followed: a symlink that resolves outside the worktree is refused, so is a file that is not
// regular (a FIFO would hang git), and a list over the entry or size caps. Racy by nature (see the
// package comment): it protects against a quiet fence only.
func (r *Runner) scanPending() error {
	root, err := filepath.EvalSymlinks(r.Worktree)
	if err != nil {
		return err
	}
	maxFiles, maxBytes := r.MaxFiles, r.MaxFileBytes
	if maxFiles <= 0 {
		maxFiles = 200000
	}
	if maxBytes <= 0 {
		maxBytes = 256 << 20
	}
	var list []string
	for _, a := range [][]string{{"ls-files", "-z", "--others", "--exclude-standard"}, {"ls-files", "-z", "--modified"}} {
		out, err := r.Run(a...)
		if err != nil {
			return err
		}
		list = append(list, splitZ(out)...)
	}
	if len(list) > maxFiles {
		return fmt.Errorf("%w: %d changed entries in the worktree (cap %d)", ErrRefused, len(list), maxFiles)
	}
	for _, rel := range list {
		p := filepath.Join(root, rel)
		st, err := os.Lstat(p)
		switch {
		case os.IsNotExist(err):
			continue // a deletion
		case err != nil:
			return fmt.Errorf("%w: cannot scan %q: %v", ErrRefused, rel, err)
		case st.Mode()&fs.ModeSymlink != 0:
			if err := checkLink(root, p); err != nil {
				return err
			}
		case st.IsDir():
			// a nested repository or an empty dir: gitlinks are judged after staging
		case st.Mode().IsRegular():
			if st.Size() > maxBytes {
				return fmt.Errorf("%w: %q is %d bytes (cap %d)", ErrRefused, rel, st.Size(), maxBytes)
			}
		default:
			return fmt.Errorf("%w: %q is not a regular file, directory or symlink", ErrRefused, rel)
		}
	}
	return nil
}

// checkLink refuses a symlink whose final target is outside root. A dangling link is judged by its
// text.
func checkLink(root, p string) error {
	inside := func(t string) bool { return t == root || strings.HasPrefix(t, root+string(filepath.Separator)) }
	if t, err := filepath.EvalSymlinks(p); err == nil {
		if !inside(t) {
			return fmt.Errorf("%w: symlink %q points outside the worktree (%q)", ErrRefused, p, t)
		}
		return nil
	}
	l, err := os.Readlink(p)
	if err != nil {
		return err
	}
	if !filepath.IsAbs(l) {
		l = filepath.Join(filepath.Dir(p), l)
	}
	if !inside(filepath.Clean(l)) {
		return fmt.Errorf("%w: dangling symlink %q points outside the worktree", ErrRefused, p)
	}
	return nil
}

// forgedRe matches a line that looks like one of our trailers, with git's leniency about blanks
// before the separator ("Fenced-Session : x").
var forgedRe = regexp.MustCompile(`(?mi)^\s*(Fenced-Session|Steering-Files)\s*[:=].*\n?`)

// dividerRe is a patch divider line: `git interpret-trailers --parse` stops looking at it, so a
// message containing one could make a parser read the agent's own paragraph as the trailer block.
var dividerRe = regexp.MustCompile(`(?m)^---`)

func stripForgedTrailers(m string) string {
	m = forgedRe.ReplaceAllString(m, "")
	m = dividerRe.ReplaceAllString(m, "- --")
	return strings.TrimSpace(m)
}

func splitZ(s string) []string {
	var out []string
	for _, p := range strings.Split(s, "\x00") {
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}
