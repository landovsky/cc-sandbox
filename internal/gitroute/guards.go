// guards.go flags what the owner's OWN git would trust inside a worktree a fence has written:
// files that core.hooksPath or an include.path of the repository's config point into (Husky's
// .husky/pre-commit is the common case), and nested repositories (sub/.git, a directory or a file)
// whose config and hooks are the fence's. H1 keeps the host's git away from all of that; the owner's
// next `git commit` in the same worktree is not covered by H1, so `ccsbx steering-check` reports
// these as steering and exits 3. A known limit, documented: do not run your own git in a worktree a
// fence wrote to before running steering-check.
package gitroute

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// maxConfigRead bounds what is read from `git config`.
const maxConfigRead = 64 << 10

var guardKeyRe = regexp.MustCompile(`^(core\.hookspath|include\.path|includeif\..*\.path)$`)

// configGuards lists the repo-relative paths that the repository's config makes git read or run:
// core.hooksPath (relative to the worktree) and every include.path / includeIf.*.path (relative to
// the config file they are in), when they point inside the worktree. "." means the whole tree.
func (r *Runner) configGuards() ([]string, error) {
	wt, err := filepath.EvalSymlinks(r.Worktree)
	if err != nil {
		return nil, err
	}
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
	var out []string
	for _, f := range files {
		if _, err := os.Stat(f); err != nil {
			continue
		}
		vals, err := r.configValues(f)
		if err != nil {
			return nil, err
		}
		for _, kv := range vals {
			base := filepath.Dir(f) // include.path is relative to the config file
			if kv[0] == "core.hookspath" {
				base = wt // hooks run from the worktree
			}
			v := kv[1]
			if v == "" || strings.HasPrefix(v, "~") {
				continue
			}
			if !filepath.IsAbs(v) {
				v = filepath.Join(base, v)
			}
			v = filepath.Clean(v)
			if rv, err := filepath.EvalSymlinks(v); err == nil {
				v = rv
			} else if dv, err := filepath.EvalSymlinks(filepath.Dir(v)); err == nil {
				v = filepath.Join(dv, filepath.Base(v))
			}
			rel, err := filepath.Rel(wt, v)
			if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
				continue // outside the worktree: the fence cannot write it
			}
			out = append(out, filepath.ToSlash(rel))
		}
	}
	return out, nil
}

// configValues returns the (lowercased key, value) pairs of one config file that matter, read
// without following includes, bounded, with a clean environment.
func (r *Runner) configValues(cfg string) ([][2]string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), r.timeout())
	defer cancel()
	c := exec.CommandContext(ctx, r.git(), "config", "--file", cfg, "--no-includes", "-z", "--get-regexp", `^(core\.hookspath|include\.path|includeif\..*\.path)$`)
	c.Env = []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "HOME=/nonexistent", "PATH=/usr/bin:/bin"}
	b, err := c.Output()
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return nil, nil // no match
	}
	if err != nil {
		return nil, err
	}
	if len(b) > maxConfigRead {
		return nil, ErrConfigTooBig
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

// ErrConfigTooBig: the repository config is larger than steering-check will read.
var ErrConfigTooBig = errConfig("repository config output is larger than 64 KiB")

type errConfig string

func (e errConfig) Error() string { return string(e) }

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

// maxNestedWalk bounds the walk for nested repositories.
const maxNestedWalk = 500000

// NestedRepos lists `.git` entries (directory or file) below the worktree root, which are
// repositories the fence can write, except submodules the HEAD tree already has. The walk does
// not follow symlinks and does not enter node_modules (its nested repositories are package
// manager checkouts the owner's git never runs from).
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
	var out []string
	n := 0
	err = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // unreadable: nothing to say about it
		}
		if p == root {
			return nil
		}
		if n++; n > maxNestedWalk {
			return filepath.SkipAll
		}
		rel, _ := filepath.Rel(root, p)
		rel = filepath.ToSlash(rel)
		if d.Name() == ".git" {
			if rel == ".git" { // the worktree's own (or the file naming the gitdir)
				if d.IsDir() {
					return fs.SkipDir
				}
				return nil
			}
			if _, sub := known[strings.TrimSuffix(rel, "/.git")]; !sub {
				out = append(out, rel)
			}
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() && d.Name() == "node_modules" {
			return fs.SkipDir
		}
		return nil
	})
	return out, err
}
