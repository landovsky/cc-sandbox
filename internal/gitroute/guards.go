// guards.go flags what the owner's OWN git would trust inside a worktree a fence has written:
// files that core.hooksPath or an include.path of the repository's (or the owner's global) config
// point into (Husky's .husky/pre-commit is the common case), include files that live in the
// worktree at all, and nested repositories (sub/.git, a directory or file) whose config and hooks
// are the fence's. H1 keeps the host's git away from all of that; the owner's next `git commit` in
// the same worktree is not covered by H1, so `ccsbx steering-check` reports these as steering and
// exits 3, and exits 4 when it could not look everywhere.
//
// Known limits, stated in --help and the runbook: exit 0 does not make hooks safe when a hook
// calls code the fence can edit and git ignores (node_modules/.bin/...), and command-valued config
// keys that name a script in the worktree (filter.*.clean, diff.*.textconv, core.fsmonitor,
// core.editor, alias.*=!...) are not read.
package gitroute

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

// maxConfigRead bounds what is read from `git config`, while it is read.
const maxConfigRead = 64 << 10

var guardKeyRe = regexp.MustCompile(`^(core\.hookspath|include\.path|includeif\..*\.path)$`)

// ErrConfigTooBig: a config's relevant output is larger than steering-check will read.
var ErrConfigTooBig = errors.New("repository config output is larger than 64 KiB")

// ErrIncomplete: the check could not look everywhere (the walk cap was hit); its result must not be
// taken as "nothing found".
var ErrIncomplete = errors.New("incomplete: the worktree walk hit its cap")

// Guards is what the configs say git will read or run: Prefixes are repo-relative paths whose
// CHANGES matter ("." is the whole tree); Forced are in-worktree files that are flagged whether
// changed or not (an included config the agent can write is always for review).
type Guards struct{ Prefixes, Forced []string }

func (r *Runner) home() string {
	if r.Home != "" {
		return r.Home
	}
	h, _ := os.UserHomeDir()
	return h
}

// configGuards reads core.hooksPath and include.path / includeIf.*.path from the gitdir's config
// files and the owner's global config, then, one level down, from every included file. A path that
// resolves inside the worktree is returned; one outside is not the fence's to write (but the
// included file it names is still read for its own hooksPath).
func (r *Runner) configGuards() (Guards, error) {
	var g Guards
	wt, err := filepath.EvalSymlinks(r.Worktree)
	if err != nil {
		return g, err
	}
	home := r.home()
	var files []string
	dirs := []string{r.GitDir}
	if cd, err := os.ReadFile(filepath.Join(r.GitDir, "commondir")); err == nil {
		c := strings.TrimSpace(string(cd))
		if !filepath.IsAbs(c) {
			c = filepath.Join(r.GitDir, c)
		}
		dirs = append(dirs, filepath.Clean(c))
	}
	for _, d := range dirs {
		files = append(files, filepath.Join(d, "config"), filepath.Join(d, "config.worktree"))
	}
	if home != "" { // the owner's global config, which every worktree of the owner inherits
		files = append(files, filepath.Join(home, ".gitconfig"), filepath.Join(home, ".config", "git", "config"))
	}
	inside := func(p string) (string, bool) {
		p = filepath.Clean(p)
		if rv, err := filepath.EvalSymlinks(p); err == nil {
			p = rv
		} else if dv, err := filepath.EvalSymlinks(filepath.Dir(p)); err == nil {
			p = filepath.Join(dv, filepath.Base(p))
		}
		rel, err := filepath.Rel(wt, p)
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			return "", false
		}
		return filepath.ToSlash(rel), true
	}
	resolve := func(base, v string) string {
		if v == "~" || strings.HasPrefix(v, "~/") {
			v = filepath.Join(home, strings.TrimPrefix(v, "~"))
		}
		if !filepath.IsAbs(v) {
			v = filepath.Join(base, v)
		}
		return filepath.Clean(v)
	}
	var read func(f string, depth int) error
	read = func(f string, depth int) error {
		if _, err := os.Stat(f); err != nil {
			return nil
		}
		vals, err := r.configValues(f)
		if err != nil {
			return err
		}
		for _, kv := range vals {
			if kv[1] == "" {
				continue
			}
			if kv[0] == "core.hookspath" {
				if rel, ok := inside(resolve(wt, kv[1])); ok { // hooks run from the worktree
					g.Prefixes = append(g.Prefixes, rel)
				}
				continue
			}
			inc := resolve(filepath.Dir(f), kv[1]) // include.path is relative to the config file
			if rel, ok := inside(inc); ok {
				g.Forced = append(g.Forced, rel)
				g.Prefixes = append(g.Prefixes, rel)
			}
			if depth < 1 { // one level of included files
				if err := read(inc, depth+1); err != nil {
					return err
				}
			}
		}
		return nil
	}
	for _, f := range files {
		if err := read(f, 0); err != nil {
			return g, err
		}
	}
	return g, nil
}

// configValues returns the (lowercased key, value) pairs of one config file that matter, read
// without following includes, with a clean environment and a bound enforced WHILE reading.
func (r *Runner) configValues(cfg string) ([][2]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout())
	defer cancel()
	c := exec.CommandContext(ctx, r.git(), "config", "--file", cfg, "--no-includes", "-z", "--get-regexp", `^(core\.hookspath|include\.path|includeif\..*\.path)$`)
	c.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=/nonexistent", "PATH=/usr/bin:/bin"}
	pipe, err := c.StdoutPipe()
	if err != nil {
		return nil, err
	}
	if err := c.Start(); err != nil {
		return nil, err
	}
	b, rerr := io.ReadAll(io.LimitReader(pipe, maxConfigRead+1))
	if len(b) > maxConfigRead {
		cancel() // stop git; the rest is never read
		c.Wait()
		return nil, ErrConfigTooBig
	}
	werr := c.Wait()
	if ee, ok := werr.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return nil, nil // no match
	}
	if rerr != nil || werr != nil {
		return nil, fmt.Errorf("reading %s: %v %v", cfg, rerr, werr)
	}
	var out [][2]string
	for _, rec := range strings.Split(string(b), "\x00") {
		if k, v, ok := strings.Cut(rec, "\n"); ok && guardKeyRe.MatchString(k) {
			if strings.HasPrefix(k, "includeif.") {
				k = "include.path"
			}
			out = append(out, [2]string{k, v})
		}
	}
	return out, nil
}

// underAny returns the files equal to, or inside, one of the guard paths.
func underAny(files, guards []string) []string {
	var out []string
	seen := map[string]bool{}
	for _, f := range files {
		for _, g := range guards {
			if g == "." || f == g || strings.HasPrefix(f, g+"/") {
				if !seen[f] {
					seen[f] = true
					out = append(out, f)
				}
			}
		}
	}
	return out
}

// maxNestedWalk bounds the walk for nested repositories; reaching it is ErrIncomplete.
var maxNestedWalk = 500000

// dotGitFold: macOS volumes are case-insensitive by default, so `.GIT` is a repository there.
var dotGitFold = runtime.GOOS == "darwin"

// isDotGit: git's own name for the entry.
func isDotGit(name string) bool {
	if dotGitFold {
		return strings.EqualFold(name, ".git")
	}
	return name == ".git"
}

// SetNestedWalkCap lowers the entry cap of the nested-repository walk and returns the undo. It
// exists for tests of the incomplete verdict (500k files are not made in a test).
func SetNestedWalkCap(n int) func() {
	old := maxNestedWalk
	maxNestedWalk = n
	return func() { maxNestedWalk = old }
}

// NestedRepos lists what the owner's git would treat as a repository the fence can write:
//   - `.git` entries (directory or file) below the worktree root that HEAD does not record as a
//     submodule;
//   - a recorded submodule whose `.git` is not a FILE naming a gitdir under this repository's
//     modules/ (the fence swapped in its own repository);
//   - directories it could not read ("<dir> (unreadable)").
//
// The walk does not follow symlinks. A `node_modules` that git ignores is not entered; one that is
// not ignored is walked like any other directory. Hitting the entry cap returns what was found and
// ErrIncomplete.
func (r *Runner) NestedRepos() ([]string, error) {
	root, err := filepath.EvalSymlinks(r.Worktree)
	if err != nil {
		return nil, err
	}
	known := map[string]string{}
	if _, err := r.Run("rev-parse", "--verify", "HEAD"); err == nil {
		if known, err = r.gitlinks("HEAD"); err != nil {
			return nil, err
		}
	}
	modules := []string{filepath.Join(r.GitDir, "modules") + "/"}
	if cd, err := os.ReadFile(filepath.Join(r.GitDir, "commondir")); err == nil {
		c := strings.TrimSpace(string(cd))
		if !filepath.IsAbs(c) {
			c = filepath.Join(r.GitDir, c)
		}
		modules = append(modules, filepath.Join(filepath.Clean(c), "modules")+"/")
	}
	var out []string
	n := 0
	incomplete := false
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if err != nil {
			if p != root && d != nil && d.IsDir() {
				out = append(out, rel+"/ (unreadable)")
				return fs.SkipDir
			}
			if p == root {
				return err
			}
			return nil
		}
		if p == root {
			return nil
		}
		if n++; n > maxNestedWalk {
			incomplete = true
			return filepath.SkipAll
		}
		if isDotGit(d.Name()) {
			if rel == ".git" { // the worktree's own (or the file naming the gitdir)
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			parent := strings.TrimSuffix(rel, "/"+d.Name())
			if _, sub := known[parent]; sub {
				if !r.submoduleDotGitOK(p, d, modules) {
					out = append(out, rel)
				}
			} else {
				out = append(out, rel)
			}
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() && d.Name() == "node_modules" {
			// git never looks inside an ignored node_modules; an unignored one is walked
			if _, err := r.exec("check-ignore", "-q", "--", rel+"/x"); err == nil {
				return fs.SkipDir
			}
		}
		return nil
	})
	if err == nil && incomplete {
		err = ErrIncomplete
	}
	return out, err
}

// submoduleDotGitOK: a recorded submodule's .git must be a regular file whose `gitdir:` resolves
// under this repository's modules/ directory.
func (r *Runner) submoduleDotGitOK(p string, d fs.DirEntry, modules []string) bool {
	if d.IsDir() || d.Type()&fs.ModeSymlink != 0 {
		return false
	}
	b, err := os.ReadFile(p)
	if err != nil || len(b) > 4096 {
		return false
	}
	t, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return false
	}
	t = strings.TrimSpace(t)
	if !filepath.IsAbs(t) {
		t = filepath.Join(filepath.Dir(p), t)
	}
	t = filepath.Clean(t) + "/"
	for _, m := range modules {
		if strings.HasPrefix(t, m) {
			return true
		}
	}
	return false
}
