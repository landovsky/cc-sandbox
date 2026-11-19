// Package gitroute is H1: every git command the host runs on an agent-writable worktree.
//
// The worktree is hostile input (invariants 24 and 25). A repo can carry hooks, a `.git` file
// redirecting to an attacker-chosen directory, `.gitattributes` naming filter drivers, and
// `.gitmodules` pointing at `ext::` transports. So the host never lets git discover anything:
//
//   - GIT_DIR and GIT_WORK_TREE are always set, so the worktree's `.git` is never read;
//   - the gitdir must be outside the worktree (it is host-owned, read-only to the fence);
//   - attributes are read from the empty tree, never from the worktree's .gitattributes;
//   - system and global config are off, and a short fixed list of `-c` overrides turns off hooks,
//     fsmonitor, external diff and credential helpers, and external/file transports;
//   - the environment is rebuilt from scratch, nothing from the caller's is inherited;
//   - only a short list of verbs runs: no push, fetch, clone, config, remote, submodule.
//
// Push happens only through the forwarder's fwd.sock (P2, not here).
package gitroute

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// emptyTree is git's well-known empty tree (SHA-1 repos).
const emptyTree = "4b825dc642cb6eb9a060e54bf8d69288fbee4904"

// Runner runs hardened git against one host-owned gitdir and one worktree.
type Runner struct {
	GitDir   string // host-owned gitdir, outside Worktree
	Worktree string
	// AllowGitDirInWorktree is for a developer's own repo (steering-check on a normal checkout).
	// A fenced session's gitdir is never inside its worktree.
	AllowGitDirInWorktree bool
	Git                   string // git binary, default "git"
	Author, Email         string // commit identity; default "ccsbx" / "ccsbx@localhost"
	Path                  string // PATH for git's own helpers, default /usr/bin:/bin
}

// ErrRefused is returned for a verb or flag the host will not run.
var ErrRefused = errors.New("refused")

// verbs are the only git commands the host runs on a worktree.
var verbs = map[string]bool{
	"status": true, "diff": true, "log": true, "show": true, "rev-parse": true, "rev-list": true,
	"add": true, "commit": true, "ls-files": true, "ls-tree": true, "cat-file": true,
	"diff-tree": true, "merge-base": true, "branch": true, "symbolic-ref": true, "write-tree": true,
}

// forbiddenFlags are refused anywhere in the argument list: each re-enables something the
// overrides turn off, or reaches outside the gitdir.
var forbiddenFlags = []string{
	"--ext-diff", "--textconv", "--recurse-submodules", "--upload-pack", "--receive-pack", "--exec",
	"--git-dir", "--work-tree", "--exec-path", "--template", "-c", "--config", "--no-index",
	"--output", "-o", "--output-directory",
}

// overrides are the fixed `-c` settings, placed before the verb. Command-line config beats the
// repo's own config, so a poisoned gitdir config can't undo them.
var overrides = []string{
	"core.hooksPath=/dev/null",
	"core.fsmonitor=false",
	"core.untrackedCache=false",
	"core.sshCommand=/bin/false",
	"core.pager=cat",
	"core.editor=/bin/false",
	"core.attributesFile=/dev/null",
	"core.excludesFile=/dev/null",
	"diff.external=",
	"diff.ignoreSubmodules=all",
	"status.submoduleSummary=false",
	"submodule.recurse=false",
	"credential.helper=",
	"protocol.ext.allow=never",
	"protocol.file.allow=never",
	"commit.gpgsign=false",
	"gc.auto=0",
	"maintenance.auto=false",
	"advice.detachedHead=false",
}

// Env is the complete environment of every host git process. Nothing is inherited.
func (r *Runner) Env() []string {
	p := r.Path
	if p == "" {
		p = "/usr/bin:/bin"
	}
	name, email := r.Author, r.Email
	if name == "" {
		name = "ccsbx"
	}
	if email == "" {
		email = "ccsbx@localhost"
	}
	return []string{
		"GIT_AUTHOR_NAME=" + name, "GIT_AUTHOR_EMAIL=" + email,
		"GIT_COMMITTER_NAME=" + name, "GIT_COMMITTER_EMAIL=" + email,
		"GIT_DIR=" + r.GitDir,
		"GIT_WORK_TREE=" + r.Worktree,
		"GIT_CONFIG_NOSYSTEM=1",
		"GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_TERMINAL_PROMPT=0",
		"GIT_OPTIONAL_LOCKS=0",
		"GIT_CEILING_DIRECTORIES=" + filepath.Dir(r.Worktree),
		"GIT_ATTR_NOSYSTEM=1",
		// Attributes come from the empty tree, so a worktree .gitattributes can't select a filter
		// or diff driver that the host's own config happens to define (git-lfs, say).
		"GIT_ATTR_SOURCE=" + emptyTree,
		"GIT_EXTERNAL_DIFF=/bin/false",
		"GIT_ASKPASS=/bin/false",
		"GIT_SSH_COMMAND=/bin/false",
		"HOME=/nonexistent",
		"LC_ALL=C",
		"PATH=" + p,
	}
}

// Check validates the paths and verb without running anything.
func (r *Runner) Check(args []string) error {
	if !filepath.IsAbs(r.GitDir) || !filepath.IsAbs(r.Worktree) {
		return fmt.Errorf("%w: gitdir and worktree must be absolute", ErrRefused)
	}
	gd, err := filepath.EvalSymlinks(r.GitDir)
	if err != nil {
		return err
	}
	wt, err := filepath.EvalSymlinks(r.Worktree)
	if err != nil {
		return err
	}
	if !r.AllowGitDirInWorktree && (gd == wt || strings.HasPrefix(gd, wt+string(filepath.Separator))) {
		return fmt.Errorf("%w: gitdir %s is inside the worktree, so the agent could write it", ErrRefused, r.GitDir)
	}
	if st, err := os.Stat(filepath.Join(r.GitDir, "HEAD")); err != nil || st.IsDir() {
		return fmt.Errorf("%w: %s is not a git directory", ErrRefused, r.GitDir)
	}
	if len(args) == 0 {
		return fmt.Errorf("%w: no git verb", ErrRefused)
	}
	if !verbs[args[0]] {
		return fmt.Errorf("%w: git %s is not a host verb", ErrRefused, args[0])
	}
	for _, a := range args[1:] {
		if a == "--" {
			break
		}
		for _, f := range forbiddenFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return fmt.Errorf("%w: flag %s", ErrRefused, a)
			}
		}
	}
	return nil
}

// Cmd builds the process without starting it (tests inspect it).
func (r *Runner) Cmd(args ...string) (*exec.Cmd, error) {
	if err := r.Check(args); err != nil {
		return nil, err
	}
	g := r.Git
	if g == "" {
		g = "git"
	}
	argv := []string{}
	for _, o := range overrides {
		argv = append(argv, "-c", o)
	}
	argv = append(argv, args[0])
	// --no-ext-diff keeps diff drivers off even if a config override were missed.
	if args[0] == "diff" || args[0] == "show" || args[0] == "log" {
		argv = append(argv, "--no-ext-diff", "--no-textconv")
	}
	if args[0] == "commit" {
		argv = append(argv, "--no-verify", "--no-gpg-sign")
	}
	argv = append(argv, args[1:]...)
	c := exec.Command(g, argv...)
	c.Dir = r.Worktree
	c.Env = r.Env()
	return c, nil
}

// Run runs a host verb and returns stdout, or an error carrying stderr.
func (r *Runner) Run(args ...string) (string, error) {
	c, err := r.Cmd(args...)
	if err != nil {
		return "", err
	}
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return out.String(), fmt.Errorf("git %s: %v: %s", args[0], err, strings.TrimSpace(errb.String()))
	}
	return out.String(), nil
}
