// `ccsbx steering-check`: lists the steering files (instructions, harness settings, hooks, CI,
// MCP config) that changed since a base, plus the ones that sit in the worktree uncommitted or
// ignored, and exits 3 if there are any. It exists because an agent-written change to such a file
// is input for review, never trusted for the next session (invariant 25). It reads git only through
// the hardened host runner (internal/gitroute), never through the worktree's own .git.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/gitroute"
	"github.com/landovsky/cc-sandbox/internal/start"
)

func steeringCheckCmd() *cli.Command {
	return &cli.Command{
		Name:    "steering-check",
		Summary: "list steering files (CLAUDE.md, .claude/, .mcp.json, hooks, CI) changed since a base; exit 3 if any (H1)",
		Recipes: `  ccsbx steering-check --base main                        # in a normal checkout
  ccsbx steering-check --worktree /path/wt --gitdir /host/owned.git --base main   # a fenced session's worktree
  Reads git only through the hardened host runner: no hooks, no filters, no .git redirect.
  Exit 0: none changed. Exit 3: steering files found (one path per line, then the Steering-Files
  trailer value). Exit 1: git itself failed (not a verdict). Matching ignores case; covered: CLAUDE.md,
  CLAUDE.local.md, AGENTS.md, .mcp.json, .claude/, .cursor/, .github/, .vscode/, .husky/, hooks, mise and
  npm config, .gitattributes, .gitmodules (see internal/gitroute/steering.go). Also reported: a symlink
  or gitlink that is new or changed, edits behind a steering symlink (.claude -> docs/notes), and
  steering files that are untracked, modified or git-ignored in the worktree (CLAUDE.local.md,
  .claude/settings.local.json). Paths with odd characters are printed quoted.
  Needs git >= 2.41 (Apple's /usr/bin/git 2.39 is refused): ccsbx steering-check --git /opt/homebrew/bin/git
  Exit 2: refused (old git, a fenced session's own .git, an unsafe gitdir).`,
		Setup: func(f *cli.Flags) {
			f.String("worktree", ".", "worktree to check")
			f.String("gitdir", "", "host-owned gitdir (default <worktree>/.git, developer-repo mode: the agent can write that one)")
			f.String("state", defaultState(), "daemon state dir; a worktree named by a session record is never read through its own .git")
			f.String("git", "", "git binary (default /usr/bin/git; must be >= 2.41)")
			f.String("timeout", "60s", "time limit per git process")
			f.String("base", "main", "base ref; files changed since the merge base with HEAD are checked")
		},
		Run: func(f *cli.Flags, _ []string, stdout, stderr io.Writer) int {
			get := func(n string) string { return f.Lookup(n).Value.String() }
			wt, err := filepath.Abs(get("worktree"))
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx steering-check:", err)
				return cli.ExitFail
			}
			to, err := time.ParseDuration(get("timeout"))
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx steering-check: --timeout:", err)
				return cli.ExitUsage
			}
			r := &gitroute.Runner{GitDir: get("gitdir"), Worktree: wt, Git: get("git"), Timeout: to}
			if r.GitDir == "" {
				if sid, fenced := start.SessionForWorkdir(get("state"), wt); fenced {
					fmt.Fprintf(stderr, "ccsbx steering-check: %s is the worktree of session %s; its .git is agent-writable, so pass the host-owned --gitdir\n", wt, sid)
					return cli.ExitRefused
				}
				r.GitDir, r.AllowGitDirInWorktree = filepath.Join(wt, ".git"), true
				if st, err := os.Lstat(r.GitDir); err != nil || !st.IsDir() { // not a symlink either
					fmt.Fprintln(stderr, "ccsbx steering-check: no .git directory; pass --gitdir (a linked worktree's gitdir is not found automatically)")
					return cli.ExitFail
				}
			}
			st, err := r.SteeringSince(get("base"))
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx steering-check:", err)
				if errors.Is(err, gitroute.ErrRefused) {
					return cli.ExitRefused
				}
				return cli.ExitFail
			}
			if len(st) == 0 {
				return cli.ExitOK
			}
			for _, p := range st {
				fmt.Fprintln(stdout, gitroute.Q(p))
			}
			fmt.Fprintln(stdout, gitroute.Trailers("-", st)[len("Fenced-Session: -\n"):])
			return cli.ExitFlagged
		},
	}
}
