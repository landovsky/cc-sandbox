// Package worktree builds a session's worktree from {repo, base, branch} on the machine that runs it (R-WT, docs/r-wt-scope.md,
// direct-mode path). It exists so that a remote or local start can name a repository instead of a path: the daemon fetches it
// as the OWNER (the user hostd runs as: that user's own git configuration for access; nothing is written into the clone, and
// nothing of it ever enters the fence), keeps ONE bare clone per repo in the owner's state directory (never under a worktree
// root, so a start naming it as workdir.path cannot mount it), and adds a worktree for the session under the worktree root.
//
// Every git call is one argv, never a shell, with hostile-repository switches on the command line (they win over any config): hooks
// off, fsmonitor off, only the https and ssh transports, submodules and LFS filters off, object checking on, no prompts.
// Values reach git only after the C5 rules of docs/m2c-remote-start.md: charset (start.ParseSpec), `git check-ref-format`, a fully
// qualified ref, the base resolved to a commit id by `rev-parse --verify --end-of-options`, and only that id handed to `worktree add`.
package worktree

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"
)

// Error is a refusal the caller turns into bad_spec with a FIXED message (no repo text, no OS text, no paths).
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func refuse(m string) error { return &Error{m} }

// Builder holds the configuration. The zero value is not usable: StateDir and Root are required.
type Builder struct {
	StateDir   string        // <StateDir>/clones/<owner>/<repo>.git
	Root       string        // an absolute worktree root: sessions' worktrees are created directly under it
	Git        string        // default /usr/bin/git
	Timeout    time.Duration // per fetch (default 120 s); other git calls get 30 s
	MaxCloneMB int64         // refuse (and delete) a clone bigger than this (default 2048)
	Protocols  []string      // transports allowed (default https, ssh)
	Remote     func(repo string) string
	Env        []string // extra environment (tests)
}

var (
	repoRe  = regexp.MustCompile(`^github\.com/([A-Za-z0-9][A-Za-z0-9._-]{0,99})/([A-Za-z0-9][A-Za-z0-9._-]{0,99})$`)
	ownerRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	slugRe  = regexp.MustCompile(`[^a-z0-9]+`)
	shaRe   = regexp.MustCompile(`^[0-9a-f]{40,64}$`)
)

func (b *Builder) git() string {
	if b.Git != "" {
		return b.Git
	}
	return "/usr/bin/git"
}

func (b *Builder) remote(repo string) string {
	if b.Remote != nil {
		return b.Remote(repo)
	}
	return "https://" + repo + ".git"
}

// hardened is the command-line configuration every call carries (it beats the owner's config files).
func (b *Builder) hardened() []string {
	protos := b.Protocols
	if len(protos) == 0 {
		protos = []string{"https", "ssh"}
	}
	a := []string{
		"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "core.untrackedCache=false",
		"-c", "protocol.allow=never", "-c", "protocol.ext.allow=never", "-c", "submodule.recurse=false",
		"-c", "transfer.fsckObjects=true", "-c", "fetch.fsckObjects=true", "-c", "receive.fsckObjects=true",
		"-c", "gc.auto=0", "-c", "maintenance.auto=false", "-c", "credential.interactive=false",
		"-c", "filter.lfs.process=", "-c", "filter.lfs.smudge=", "-c", "filter.lfs.clean=", "-c", "filter.lfs.required=false",
		"-c", "advice.detachedHead=false", "-c", "core.symlinks=true",
	}
	for _, p := range protos {
		a = append(a, "-c", "protocol."+p+".allow=always")
	}
	return a
}

// env is the owner's own environment for access (HOME and the agent socket) and nothing else of the daemon's.
func (b *Builder) env() []string {
	e := []string{
		"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_ASKPASS=/bin/false",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=yes", "GIT_PAGER=cat", "GIT_OPTIONAL_LOCKS=0",
	}
	for _, k := range []string{"HOME", "SSH_AUTH_SOCK", "XDG_CONFIG_HOME"} {
		if v := os.Getenv(k); v != "" {
			e = append(e, k+"="+v)
		}
	}
	return append(e, b.Env...)
}

func (b *Builder) run(d time.Duration, dir string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	c := exec.CommandContext(ctx, b.git(), append(b.hardened(), args...)...)
	c.Dir, c.Env = dir, b.env()
	c.WaitDelay = time.Second
	c.Cancel = func() error { return c.Process.Signal(syscall.SIGTERM) }
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v", args[0], err)
	}
	return strings.TrimSpace(out.String()), nil
}

func (b *Builder) fetchTimeout() time.Duration {
	if b.Timeout > 0 {
		return b.Timeout
	}
	return 120 * time.Second
}

func (b *Builder) maxMB() int64 {
	if b.MaxCloneMB > 0 {
		return b.MaxCloneMB
	}
	return 2048
}

// lock takes the per-clone flock (one fetch or worktree add at a time per repo).
func lock(path string) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func dirMB(p string) int64 {
	var n int64
	filepath.Walk(p, func(_ string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			n += fi.Size()
		}
		return nil
	})
	return n >> 20
}

// Prepare returns the absolute path of a new worktree of repo at base on a NEW branch. owner is the session owner (the clone is
// theirs), label names the worktree directory. Any failure removes what it created.
func (b *Builder) Prepare(owner, label, repo, base, branch string) (string, error) {
	m := repoRe.FindStringSubmatch(repo)
	if m == nil || !ownerRe.MatchString(owner) || b.StateDir == "" || !filepath.IsAbs(b.Root) {
		return "", refuse("the repository is not acceptable")
	}
	if strings.HasPrefix(base, "-") || strings.HasPrefix(branch, "-") {
		return "", refuse("the base or branch is not a valid ref name")
	}
	if _, err := b.run(30*time.Second, "", "check-ref-format", "refs/heads/"+base); err != nil {
		return "", refuse("the base is not a valid ref name")
	}
	if _, err := b.run(30*time.Second, "", "check-ref-format", "--branch", branch); err != nil {
		return "", refuse("the branch is not a valid ref name")
	}
	clones := filepath.Join(b.StateDir, "clones", owner)
	if err := os.MkdirAll(clones, 0o700); err != nil {
		return "", refuse("the clone cache is not writable")
	}
	clone := filepath.Join(clones, m[1]+"__"+m[2]+".git")
	unlock, err := lock(clone + ".lock")
	if err != nil {
		return "", refuse("the clone cache is not writable")
	}
	defer unlock()

	if _, err := os.Stat(clone); os.IsNotExist(err) {
		if _, err := b.run(30*time.Second, "", "init", "--bare", "-q", "--", clone); err != nil {
			return "", refuse("the repository could not be set up")
		}
		if _, err := b.run(30*time.Second, clone, "remote", "add", "origin", b.remote(repo)); err != nil {
			os.RemoveAll(clone)
			return "", refuse("the repository could not be set up")
		}
	}
	// fetch ONLY the base branch (full history of that one ref), into a namespace of ours
	ref := "refs/remotes/origin/" + base
	if _, err := b.run(b.fetchTimeout(), clone, "fetch", "--no-tags", "--no-recurse-submodules", "--prune", "origin", "+refs/heads/"+base+":"+ref); err != nil {
		return "", refuse("the repository or branch could not be fetched")
	}
	if mb := dirMB(clone); mb > b.maxMB() {
		os.RemoveAll(clone)
		return "", refuse("the repository is larger than this machine allows")
	}
	sha, err := b.run(30*time.Second, clone, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil || !shaRe.MatchString(sha) {
		return "", refuse("the base does not name a commit")
	}
	if _, err := b.run(30*time.Second, clone, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return "", refuse("the branch already exists")
	}
	path, err := b.newPath(label)
	if err != nil {
		return "", refuse("the worktree root is not writable")
	}
	// `-b <branch>` cannot follow `--`; the name passed check-ref-format --branch and the charset and cannot start with '-'.
	// The path follows `--`; the commit-ish is the resolved id, never a name.
	if _, err := b.run(b.fetchTimeout(), clone, "worktree", "add", "-b", branch, "--", path, sha); err != nil {
		os.RemoveAll(path)
		b.run(30*time.Second, clone, "worktree", "prune")
		return "", refuse("the worktree could not be created")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		b.Remove(path)
		return "", refuse("the worktree could not be created")
	}
	return real, nil
}

// newPath makes a fresh, unused directory name directly under the root (the directory itself is made by `worktree add`).
func (b *Builder) newPath(label string) (string, error) {
	if st, err := os.Stat(b.Root); err != nil || !st.IsDir() {
		return "", fmt.Errorf("root")
	}
	slug := strings.Trim(slugRe.ReplaceAllString(strings.ToLower(label), "-"), "-")
	if len(slug) > 40 {
		slug = slug[:40]
	}
	if slug == "" {
		slug = "session"
	}
	for i := 0; i < 5; i++ {
		var r [3]byte
		rand.Read(r[:])
		p := filepath.Join(b.Root, slug+"-"+hex.EncodeToString(r[:]))
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no free name")
}

// Remove deletes a worktree this builder made (and its registration). Only a path directly under Root is ever touched.
func (b *Builder) Remove(path string) error {
	if filepath.Dir(filepath.Clean(path)) != filepath.Clean(b.Root) {
		return refuse("not a worktree of this builder")
	}
	gd, err := b.run(30*time.Second, path, "rev-parse", "--git-common-dir")
	os.RemoveAll(path)
	if err == nil && filepath.IsAbs(gd) {
		b.run(30*time.Second, gd, "worktree", "prune")
	}
	return nil
}

// Prune removes the builder's worktrees (and their branches) that nothing needs any more: `needed(path)` says whether a
// session still uses or recently used the path; a worktree it does not need and that is untouched for longer than olderThan goes.
// Only worktrees directly under Root, registered in a clone of this state dir, are ever considered. Returns how many were removed.
func (b *Builder) Prune(olderThan time.Duration, needed func(path string) bool, now time.Time) int {
	removed := 0
	clones, _ := filepath.Glob(filepath.Join(b.StateDir, "clones", "*", "*.git"))
	for _, clone := range clones {
		out, err := b.run(30*time.Second, clone, "worktree", "list", "--porcelain")
		if err != nil {
			continue
		}
		var path, branch string
		flush := func() {
			if path != "" && filepath.Dir(path) == filepath.Clean(b.Root) && !needed(path) {
				if fi, err := os.Stat(path); err == nil && now.Sub(fi.ModTime()) > olderThan {
					if b.Remove(path) == nil {
						removed++
						if strings.HasPrefix(branch, "refs/heads/") {
							b.run(30*time.Second, clone, "branch", "-D", strings.TrimPrefix(branch, "refs/heads/"))
						}
					}
				}
			}
			path, branch = "", ""
		}
		for _, l := range strings.Split(out, "\n") {
			switch {
			case strings.HasPrefix(l, "worktree "):
				flush()
				path = strings.TrimPrefix(l, "worktree ")
			case strings.HasPrefix(l, "branch "):
				branch = strings.TrimPrefix(l, "branch ")
			}
		}
		flush()
		b.run(30*time.Second, clone, "worktree", "prune")
	}
	return removed
}
