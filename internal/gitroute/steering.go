package gitroute

import (
	"path"
	"sort"
	"strings"
)

// steeringPatterns are paths whose content steers a later agent or a host tool: instructions,
// harness settings, MCP config, hooks, CI. An agent-written change to one is input for review,
// never trusted for the next session (invariant 25), so a commit touching one is flagged.
// Patterns are path.Match globs against the full path and the base name; "dir/" matches a tree.
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
		if strings.HasSuffix(pat, "/") {
			// the tree may sit at the root or in a subdirectory (e.g. a package's .claude/)
			if strings.HasPrefix(p, pat) || strings.Contains(p, "/"+pat) {
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
func Steering(files []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, f := range files {
		f = strings.TrimSpace(f)
		if f != "" && IsSteering(f) && !seen[f] {
			seen[f] = true
			out = append(out, f)
		}
	}
	sort.Strings(out)
	return out
}

// Trailers builds the commit trailer block. Steering-Files is present only when a steering file
// changed, so its absence is itself a signal (C8b flags merges that carry it).
func Trailers(session string, steering []string) string {
	t := "Fenced-Session: " + session
	if len(steering) > 0 {
		t += "\nSteering-Files: " + strings.Join(steering, ", ")
	}
	return t
}
