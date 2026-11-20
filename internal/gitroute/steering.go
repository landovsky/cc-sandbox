// Steering files: the paths whose content steers a later agent or a host tool (instructions,
// harness settings, MCP config, hooks, CI). H1 names them in a commit trailer and in
// `ccsbx steering-check`, because an agent-written change to one is input for review and never
// trusted for the next session (invariant 25). The flag has to hold against the cheap evasions:
// a symlink where the directory should be, edits to a symlink's target, and files that git
// ignores or that were never committed (CLAUDE.local.md, .claude/settings.local.json).
package gitroute

import (
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unicode"
)

// steeringPatterns are paths whose content steers a later agent or a host tool: instructions,
// harness settings, MCP config, hooks, CI. Patterns are path.Match globs against the base name;
// "dir/" matches a whole path segment (the tree, and also a symlink or gitlink named like it).
var steeringPatterns = []string{
	"CLAUDE.md", "CLAUDE.local.md", "AGENTS.md", "GEMINI.md", ".mcp.json", ".gitattributes", ".gitmodules",
	".cursorrules", ".windsurfrules", ".clinerules", ".pre-commit-config.yaml", "lefthook.yml", ".envrc",
	"mise.toml", ".mise.toml", ".tool-versions", ".npmrc",
	".claude/", ".cursor/", ".github/", ".githooks/", ".husky/", ".devcontainer/", ".vscode/",
}

// IsSteering reports whether a repo-relative path is a steering file. Matching ignores case: on a
// case-insensitive filesystem (macOS default) claude.md IS CLAUDE.md.
func IsSteering(p string) bool {
	p = strings.ToLower(strings.TrimPrefix(path.Clean(p), "./"))
	for _, pat := range steeringPatterns {
		pat = strings.ToLower(pat)
		if d, ok := strings.CutSuffix(pat, "/"); ok {
			// a whole path segment: the path IS the dir (a symlink or gitlink at .claude), is in
			// it, or has it as a segment further down (a package's own .claude/)
			if p == d || strings.HasPrefix(p, d+"/") || strings.HasSuffix(p, "/"+d) || strings.Contains(p, "/"+d+"/") {
				return true
			}
			continue
		}
		if path.Base(p) == pat {
			return true
		}
	}
	return false
}

// Steering filters files and returns the steering ones, sorted and deduplicated.
func Steering(files []string) []string { return steeringWith(files, nil) }

// steeringWith also flags a path that is, or lies under, one of targets: the repo-relative
// destinations of steering symlinks (`.claude -> docs/notes`: edits to docs/notes/x are steering).
func steeringWith(files, targets []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f == "" || seen[f] {
			continue
		}
		hit := IsSteering(f)
		c := path.Clean(f)
		for _, t := range targets {
			if c == t || strings.HasPrefix(c, t+"/") {
				hit = true
			}
		}
		if hit {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// Q makes a path safe to print or to put in a trailer: a path with a comma, a quote, a backslash
// or any non-printable character (newline, ESC, bidi controls) is shown as a Go-quoted string,
// so it cannot start a new trailer line, split the list, or write to the terminal.
func Q(p string) string {
	if strings.ContainsAny(p, `",\`) {
		return strconv.Quote(p)
	}
	for _, r := range p {
		if !unicode.IsPrint(r) {
			return strconv.Quote(p)
		}
	}
	return p
}

// Trailers builds the commit trailer block. Steering-Files is present only when a steering file
// changed, so its absence is itself a signal (C8b flags merges that carry it). Paths are quoted
// with Q. Consumers should read them with `%(trailers:key=Steering-Files,valueonly)`.
func Trailers(session string, steering []string) string {
	t := "Fenced-Session: " + session
	if len(steering) > 0 {
		q := make([]string, len(steering))
		for i, s := range steering {
			q[i] = Q(s)
		}
		t += "\nSteering-Files: " + strings.Join(q, ", ")
	}
	return t
}

// steeringTargets finds the symlinks in the worktree (tracked or not, ignored or not) whose own
// path is steering, and returns where they point, relative to the worktree. A target outside the
// worktree is not listed here (Commit refuses those).
func (r *Runner) steeringTargets() ([]string, error) {
	out, err := r.Run("ls-files", "-z", "--cached", "--others")
	if err != nil {
		return nil, err
	}
	var targets []string
	for _, p := range splitZ(out) {
		if !IsSteering(p) {
			continue
		}
		abs := filepath.Join(r.Worktree, p)
		if st, err := os.Lstat(abs); err != nil || st.Mode()&fs.ModeSymlink == 0 {
			continue
		}
		l, err := os.Readlink(abs)
		if err != nil {
			continue
		}
		if !filepath.IsAbs(l) {
			l = filepath.Join(filepath.Dir(abs), l)
		}
		rel, err := filepath.Rel(r.Worktree, filepath.Clean(l))
		if err != nil || rel == ".." || strings.HasPrefix(rel, "../") {
			continue
		}
		targets = append(targets, filepath.ToSlash(rel))
	}
	return targets, nil
}

// SteeringFiles returns the steering files among files, counting symlinks that are named like
// steering and the files behind them.
func (r *Runner) SteeringFiles(files []string) ([]string, error) {
	t, err := r.steeringTargets()
	if err != nil {
		return nil, err
	}
	return steeringWith(files, t), nil
}

// Pending lists what is in the worktree but not in HEAD: modified tracked files, staged changes,
// and every untracked file INCLUDING the ones .gitignore hides (CLAUDE.local.md and
// .claude/settings.local.json are ignored by default and still steer the next session).
func (r *Runner) Pending() ([]string, error) {
	var all []string
	for _, a := range [][]string{
		{"ls-files", "-z", "--others"},
		{"ls-files", "-z", "--modified"},
		{"diff", "--cached", "--name-only", "--no-renames", "-z", "--"},
	} {
		out, err := r.Run(a...)
		if err != nil {
			return nil, fmt.Errorf("pending files: %w", err)
		}
		all = append(all, splitZ(out)...)
	}
	return all, nil
}
