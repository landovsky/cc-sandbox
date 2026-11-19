package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/gitroute"
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
  npm config, .gitattributes, .gitmodules (see internal/gitroute/steering.go).`,
		Setup: func(f *cli.Flags) {
			f.String("worktree", ".", "worktree to check")
			f.String("gitdir", "", "host-owned gitdir (default <worktree>/.git, developer-repo mode: the agent can write that one)")
			f.String("base", "main", "base ref; files changed since the merge base with HEAD are checked")
		},
		Run: func(f *cli.Flags, _ []string, stdout, stderr io.Writer) int {
			get := func(n string) string { return f.Lookup(n).Value.String() }
			wt, err := filepath.Abs(get("worktree"))
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx steering-check:", err)
				return cli.ExitFail
			}
			r := &gitroute.Runner{GitDir: get("gitdir"), Worktree: wt}
			if r.GitDir == "" {
				r.GitDir, r.AllowGitDirInWorktree = filepath.Join(wt, ".git"), true
				if st, err := os.Stat(r.GitDir); err != nil || !st.IsDir() {
					fmt.Fprintln(stderr, "ccsbx steering-check: no .git directory; pass --gitdir (a linked worktree's gitdir is not found automatically)")
					return cli.ExitFail
				}
			}
			files, err := r.ChangedFiles(get("base"))
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx steering-check:", err)
				return cli.ExitFail
			}
			st := gitroute.Steering(files)
			if len(st) == 0 {
				return cli.ExitOK
			}
			for _, p := range st {
				fmt.Fprintln(stdout, p)
			}
			fmt.Fprintln(stdout, gitroute.Trailers("-", st)[len("Fenced-Session: -\n"):])
			return cli.ExitFlagged
		},
	}
}
