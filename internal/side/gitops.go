// gitops.go is the pod's side of git (plan F3c, with H1's rules): clone the session's repo into the worktree,
// commit what the fence wrote, push the session branch. It exists so a fenced pod can do a day's git work
// without the fence ever holding a git credential, a remote, or a say in which repo or branch is touched.
//
// What the sidecar does and why it is safe to do it here:
//   - The repo, base and branch come from flags in the pod spec (GitConfig), never from a request: a
//     git.commit body carries only a message, a git.push body nothing.
//   - Git runs with H1's hardened environment and -c overrides (read off gitroute.Runner, which this package
//     never edits), plus network overrides of its own: only http to the forwarder, no redirects, no proxy, no
//     credentials, fsck on everything received. The gitdir lives in a sidecar-only directory outside /work.
//   - Clone happens once, into an EMPTY worktree, before the fence may write (a planted symlink in /work would
//     otherwise steer the checkout); a non-empty worktree is refused.
//   - Commit is snapshot-then-commit. H1's Commit wants proof that the fence is stopped, and a pod's sidecar
//     cannot freeze the fence container, so it copies /work (never following a link out of it) into a
//     sidecar-only directory and commits THERE with H1's Runner. The snapshot cannot change under git, which is
//     the whole point of the proof. The audit fact says so ("proof":"snapshot").
//   - Push sends exactly refs/heads/<branch>:refs/heads/<branch> through the forwarder: never a '+', a force,
//     a delete, a tag, main/master/HEAD, or a branch that fails check-ref-format. The door's rules (U16) are
//     the real gate; these are the second lock. A push is refused while a snapshot is being made.
//
// Journal facts (git.cloned, git.committed, git.pushed, git.refused) carry shas and counts, never content.
package side

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/gitroute"
	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/journal"
)

// GitConfig is what the sidecar knows about the session's repository, from the pod spec.
type GitConfig struct {
	Repo   string // owner/name
	Base   string // a branch name, or a full commit id
	Branch string // the session branch: the only ref that is ever pushed
	URL    string // the forwarder's git base, e.g. http://127.0.0.1:8080/egress/git
	GitDir string // sidecar-only, outside Work and Snapshot
	Work   string // the worktree emptyDir (the fence writes it)
	// Snapshot is a sidecar-only directory (one level below its volume, made 0700) that commit copies Work into.
	Snapshot string
	Sid      string
	Gen      int
	Journal  Appender
	Git      string // git binary (default /usr/bin/git)

	Timeout          time.Duration // per git process (default 2m)
	MaxFiles         int           // snapshot entry cap (default 100000)
	MaxFileBytes     int64         // one file in the snapshot (default 16 MiB: the sidecar holds it in memory while copying)
	MaxSnapshotBytes int64         // total (default 1 GiB)
	MaxCommits       int           // a push of this many commits or more is refused (the door's compare-API cap, 250)
	MaxChangedFiles  int           // ... or this many changed files (300)
	MaxFacts         int           // git.refused facts per lifetime (default 50)
	Logf             func(string, ...any)
}

var (
	repoRe   = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,99}/[A-Za-z0-9][A-Za-z0-9._-]{0,99}$`)
	branchRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,99}$`)
	hexRe    = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)
	// protected: never pushed, whatever the door would say
	protectedBranches = map[string]bool{"main": true, "master": true, "head": true, "trunk": true}
)

// ValidBranch reports whether b may be the session branch: a plain ref name that is not protected.
func ValidBranch(b string) bool {
	if !branchRe.MatchString(b) || strings.Contains(b, "..") || strings.HasSuffix(b, "/") || strings.HasSuffix(b, ".lock") ||
		strings.Contains(b, "//") || strings.Contains(b, "/.") {
		return false
	}
	return !protectedBranches[strings.ToLower(b)] && !strings.HasPrefix(strings.ToLower(b), "refs/")
}

// GitOps runs the pod's git. Create it with NewGitOps; it handles git.commit and git.push through Forward.
type GitOps struct {
	cfg     GitConfig
	mu      sync.Mutex // one git operation at a time; a second one is "busy", never queued
	cloned  atomic.Bool
	ov      []string // H1's -c overrides plus ours
	runner  *gitroute.Runner
	facts   int
	factMu  sync.Mutex
	failLog appendFailures
}

// NewGitOps checks the configuration: the repo and branch are well-formed, the branch is not protected and not
// the base, the URL is plain http, and the three directories are distinct and absolute.
func NewGitOps(cfg GitConfig) (*GitOps, error) {
	if !repoRe.MatchString(cfg.Repo) || strings.Contains(cfg.Repo, "..") || strings.HasSuffix(strings.Split(cfg.Repo, "/")[1], ".git") {
		return nil, fmt.Errorf("side git: %q is not owner/name", cfg.Repo)
	}
	if !ValidBranch(cfg.Branch) {
		return nil, fmt.Errorf("side git: %q is not a pushable session branch (plain name, not main/master/HEAD)", cfg.Branch)
	}
	if !hexRe.MatchString(cfg.Base) && (!branchRe.MatchString(cfg.Base) || strings.Contains(cfg.Base, "..") || strings.HasSuffix(cfg.Base, ".lock")) {
		return nil, fmt.Errorf("side git: base %q is neither a branch name nor a full commit id", cfg.Base)
	}
	if cfg.Base == cfg.Branch {
		return nil, errors.New("side git: the session branch must differ from the base")
	}
	if !strings.HasPrefix(cfg.URL, "http://127.0.0.1:") && !strings.HasPrefix(cfg.URL, "http://localhost:") && !strings.HasPrefix(cfg.URL, "http://[::1]:") {
		return nil, errors.New("side git: --git-url must be http on loopback (the forwarder)")
	}
	if strings.ContainsAny(cfg.URL, "?#@ ") || strings.HasSuffix(cfg.URL, "/") {
		return nil, errors.New("side git: --git-url must have no query, fragment, credentials or trailing slash")
	}
	dirs := []string{cfg.GitDir, cfg.Work, cfg.Snapshot}
	for i, d := range dirs {
		if d == "" || !path.IsAbs(d) || path.Clean(d) != d {
			return nil, fmt.Errorf("side git: %q must be a clean absolute path", d)
		}
		for _, e := range dirs[i+1:] {
			if d == e || strings.HasPrefix(d, e+"/") || strings.HasPrefix(e, d+"/") {
				return nil, fmt.Errorf("side git: %s and %s must not be inside each other", d, e)
			}
		}
	}
	if cfg.Sid == "" || cfg.Gen < 1 {
		return nil, errors.New("side git: sid and gen are required")
	}
	if cfg.Git == "" {
		cfg.Git = "/usr/bin/git"
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 2 * time.Minute
	}
	if cfg.MaxFiles == 0 {
		cfg.MaxFiles = 100000
	}
	if cfg.MaxFileBytes == 0 {
		cfg.MaxFileBytes = 16 << 20
	}
	if cfg.MaxSnapshotBytes == 0 {
		cfg.MaxSnapshotBytes = 1 << 30
	}
	if cfg.MaxCommits == 0 {
		cfg.MaxCommits = 250
	}
	if cfg.MaxChangedFiles == 0 {
		cfg.MaxChangedFiles = 300
	}
	if cfg.MaxFacts == 0 {
		cfg.MaxFacts = 50
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	g := &GitOps{cfg: cfg}
	g.runner = &gitroute.Runner{GitDir: cfg.GitDir, Worktree: cfg.Work, Git: cfg.Git, Timeout: cfg.Timeout,
		Author: "ccsbx", Email: "ccsbx@localhost", MaxFiles: cfg.MaxFiles, MaxFileBytes: cfg.MaxFileBytes}
	return g, nil
}

func (g *GitOps) repoURL() string { return g.cfg.URL + "/" + g.cfg.Repo + ".git" }

func (g *GitOps) fact(typ string, data map[string]any) {
	if g.cfg.Journal == nil {
		return
	}
	if typ == "git.refused" {
		g.factMu.Lock()
		g.facts++
		over := g.facts > g.cfg.MaxFacts
		g.factMu.Unlock()
		if over {
			return
		}
	}
	b, _ := json.Marshal(data)
	if _, err := g.cfg.Journal.Append(journal.Event{Sid: g.cfg.Sid, Gen: g.cfg.Gen, Src: "host", Type: typ, Data: b}); err != nil {
		g.failLog.log(g.cfg.Logf, "side git", err)
	}
}

// netOverrides are ours on top of H1's: what makes network git safe against a hostile remote or a misrouted URL.
var netOverrides = []string{
	"protocol.allow=never", "protocol.http.allow=always",
	"http.followRedirects=false", "http.proxy=", "http.sslVerify=true", "http.extraHeader=", "http.userAgent=ccsbx-side",
	"http.postBuffer=1073741824", // a body over this would go chunked; the forwarder wants a Content-Length
	"transfer.fsckObjects=true", "fetch.fsckObjects=true", "receive.fsckObjects=true",
	"fetch.recurseSubmodules=false", "push.recurseSubmodules=no", "push.default=nothing", "push.followTags=false",
	"core.fsmonitor=false", "core.hooksPath=/dev/null", "core.protectNTFS=true", "core.protectHFS=true",
	"core.autocrlf=false", "core.safecrlf=false",
}

// overridesOf reads H1's -c overrides off the command gitroute builds for a harmless read, so this package
// follows gitroute without copying or editing its list.
func overridesOf(r *gitroute.Runner) ([]string, error) {
	c, err := r.Cmd("status", "--porcelain")
	if err != nil {
		return nil, err
	}
	var ov []string
	for i := 1; i+1 < len(c.Args) && c.Args[i] == "-c"; i += 2 { // the command is never run: only its argv is read
		ov = append(ov, c.Args[i+1])
	}
	if len(ov) == 0 {
		return nil, errors.New("gitroute gave no overrides")
	}
	return append(ov, netOverrides...), nil
}

type capBuf struct {
	b bytes.Buffer
}

func (c *capBuf) Write(p []byte) (int, error) {
	if room := 64<<10 - c.b.Len(); room > 0 {
		if len(p) > room {
			c.b.Write(p[:room])
		} else {
			c.b.Write(p)
		}
	}
	return len(p), nil
}

// run runs git with H1's environment for r and the overrides, in its own process group, with a timeout.
func (g *GitOps) run(ctx context.Context, r *gitroute.Runner, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()
	argv := make([]string, 0, 2*len(g.ov)+len(args))
	for _, o := range g.ov {
		argv = append(argv, "-c", o)
	}
	argv = append(argv, args...)
	c := exec.CommandContext(ctx, g.cfg.Git, argv...)
	c.Dir = r.Worktree
	c.Env = append(r.Env(), "GIT_ALLOW_PROTOCOL=http")
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.Cancel = func() error { return syscall.Kill(-c.Process.Pid, syscall.SIGKILL) }
	c.WaitDelay = 2 * time.Second
	var out, errb capBuf
	c.Stdout, c.Stderr, c.Stdin = &out, &errb, nil
	if err := c.Run(); err != nil {
		return out.b.String(), fmt.Errorf("git %s: %w: %s", args[0], err, cleanText(errb.b.String(), 200))
	}
	return out.b.String(), nil
}

// cleanText keeps one line of printable text, so what a remote or a path says can be shown without control bytes.
func cleanText(s string, max int) string {
	s = strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return ' '
		}
		return r
	}, s)
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > max {
		s = s[:max] + "…"
	}
	return s
}

// Clone fetches the base into the sidecar's gitdir and checks it out into the (empty) worktree, once.
// Call it after the token is loaded and before the fence may write.
func (g *GitOps) Clone(ctx context.Context) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cloned.Load() {
		return nil
	}
	es, err := os.ReadDir(g.cfg.Work)
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	if len(es) != 0 {
		return errors.New("the worktree is not empty: refusing to check out into a tree the fence may have written")
	}
	if _, err := os.Stat(g.cfg.GitDir + "/HEAD"); err == nil {
		return errors.New("the gitdir is already initialised")
	}
	// init has no gitdir to validate yet and must not record the worktree in the config
	initEnv := []string{}
	for _, e := range g.runner.Env() {
		if !strings.HasPrefix(e, "GIT_WORK_TREE=") {
			initEnv = append(initEnv, e)
		}
	}
	ic := exec.CommandContext(ctx, g.cfg.Git, "-c", "init.templateDir=", "-c", "core.hooksPath=/dev/null", "init", "-q", "--initial-branch=ccsbx-init")
	ic.Dir, ic.Env, ic.SysProcAttr = "/", initEnv, &syscall.SysProcAttr{Setpgid: true}
	if out, err := ic.CombinedOutput(); err != nil {
		return fmt.Errorf("git init: %w: %s", err, cleanText(string(out), 200))
	}
	if g.ov, err = overridesOf(g.runner); err != nil {
		return err
	}
	src := "refs/heads/" + g.cfg.Base
	if hexRe.MatchString(g.cfg.Base) {
		src = g.cfg.Base
	}
	if _, err := g.run(ctx, g.runner, "fetch", "-q", "--no-tags", "--no-recurse-submodules", "--no-write-fetch-head", "--end-of-options", g.repoURL(), src+":refs/ccsbx/base"); err != nil {
		return err
	}
	sha, err := g.run(ctx, g.runner, "rev-parse", "--verify", "--end-of-options", "refs/ccsbx/base^{commit}")
	if err != nil {
		return err
	}
	sha = strings.TrimSpace(sha)
	if !hexRe.MatchString(sha) {
		return errors.New("the base is not a commit")
	}
	for _, a := range [][]string{{"update-ref", "refs/heads/" + g.cfg.Branch, sha}, {"symbolic-ref", "HEAD", "refs/heads/" + g.cfg.Branch}} {
		if _, err := g.run(ctx, g.runner, a...); err != nil {
			return err
		}
	}
	// what the sidecar writes into /work must be writable by the fence through the pod's shared group (fsGroup),
	// which needs group write: the one place a umask of 002 is wanted. Everything else of ours stays 077.
	old := syscall.Umask(0o002)
	_, err = g.run(ctx, g.runner, "read-tree", "-u", "--reset", sha)
	syscall.Umask(old)
	if err != nil {
		return err
	}
	ls, _ := g.run(ctx, g.runner, "ls-files", "-z")
	g.cloned.Store(true)
	g.fact("git.cloned", map[string]any{"base": sha, "files": strings.Count(ls, "\x00")})
	return nil
}

func refuse(code, why string) hostd.Reply {
	r := hostd.Reply{Status: "refused", Result: map[string]string{"code": code}}
	if why != "" {
		r.Result = map[string]string{"code": code, "why": cleanText(why, 200)}
	}
	return r
}

func (g *GitOps) refused(op, code, why string) hostd.Reply {
	g.fact("git.refused", map[string]any{"op": op, "code": code})
	return refuse(code, why)
}

// Forward handles git.commit and git.push; any other type is refused (compose it with the bus by type).
func (g *GitOps) Forward(ctx context.Context, q hostd.Request) hostd.Reply {
	switch q.Type {
	case "git.commit":
		return g.commit(ctx, q)
	case "git.push":
		return g.push(ctx, q)
	}
	return refuse("unsupported_in_pod", "")
}

func (g *GitOps) commit(ctx context.Context, q hostd.Request) hostd.Reply {
	var b struct {
		Type    string `json:"type"` // the request file names its own type; the relay has checked it
		Message string `json:"message"`
	}
	dec := json.NewDecoder(bytes.NewReader(q.Body))
	dec.DisallowUnknownFields()
	if dec.Decode(&b) != nil || dec.More() || strings.TrimSpace(b.Message) == "" || len(b.Message) > 8<<10 || strings.ContainsRune(b.Message, 0) {
		return g.refused("commit", "bad_request", "a git.commit carries only a message (at most 8 KiB)")
	}
	if !g.cloned.Load() {
		return g.refused("commit", "not_ready", "")
	}
	if !g.mu.TryLock() {
		return g.refused("commit", "busy", "")
	}
	defer g.mu.Unlock()
	snap, err := g.snapshot()
	if err != nil {
		g.clearSnapshot()
		return g.refused("commit", "snapshot_refused", err.Error())
	}
	defer g.clearSnapshot()
	r := *g.runner
	r.Worktree = g.cfg.Snapshot
	sha, err := r.Commit(g.cfg.Sid, b.Message, gitroute.StoppedFunc(func() error { return nil })) // the snapshot is the proof
	if err != nil {
		if errors.Is(err, gitroute.ErrRefused) || strings.Contains(err.Error(), "nothing to commit") {
			return g.refused("commit", "commit_refused", err.Error())
		}
		g.cfg.Logf("side git: commit failed: %v", err)
		return hostd.Reply{Status: "error", Result: map[string]string{"code": "commit_failed"}}
	}
	g.fact("git.committed", map[string]any{"sha": sha, "files": snap.files, "bytes": snap.bytes, "proof": "snapshot"})
	return hostd.Reply{Status: "ok", Result: map[string]any{"sha": sha, "files": snap.files}}
}

func (g *GitOps) push(ctx context.Context, q hostd.Request) hostd.Reply {
	var m map[string]json.RawMessage
	if json.Unmarshal(q.Body, &m) != nil {
		return g.refused("push", "bad_request", "a git.push body is a JSON object")
	}
	delete(m, "type") // the request file names its own type; anything else would be the fence choosing something
	if len(m) != 0 {
		return g.refused("push", "bad_request", "a git.push carries nothing: the branch and the remote are not the fence's to name")
	}
	if !g.cloned.Load() {
		return g.refused("push", "not_ready", "")
	}
	if !g.mu.TryLock() { // a snapshot or another push is in progress
		return g.refused("push", "busy", "")
	}
	defer g.mu.Unlock()
	br := "refs/heads/" + g.cfg.Branch
	if out, err := g.run(ctx, g.runner, "check-ref-format", "--branch", g.cfg.Branch); err != nil || strings.TrimSpace(out) == "" {
		return g.refused("push", "bad_branch", "")
	}
	head, err := g.run(ctx, g.runner, "symbolic-ref", "-q", "HEAD")
	if err != nil || strings.TrimSpace(head) != br {
		return g.refused("push", "bad_head", "HEAD is not the session branch")
	}
	count, err := g.run(ctx, g.runner, "rev-list", "--count", "refs/ccsbx/base.."+br)
	n := 0
	fmt.Sscanf(strings.TrimSpace(count), "%d", &n)
	if err != nil || n == 0 {
		return g.refused("push", "nothing_to_push", "")
	}
	if n >= g.cfg.MaxCommits {
		return g.refused("push", "too_many_commits", fmt.Sprintf("%d commits (the door refuses %d or more)", n, g.cfg.MaxCommits))
	}
	names, err := g.run(ctx, g.runner, "diff", "--name-only", "-z", "--no-renames", "refs/ccsbx/base", br)
	if err != nil {
		return hostd.Reply{Status: "error", Result: map[string]string{"code": "push_failed"}}
	}
	if files := strings.Count(names, "\x00"); files >= g.cfg.MaxChangedFiles {
		return g.refused("push", "too_many_files", fmt.Sprintf("%d changed files (the door refuses %d or more)", files, g.cfg.MaxChangedFiles))
	}
	sha, _ := g.run(ctx, g.runner, "rev-parse", "--verify", "--end-of-options", br)
	// exactly one refspec, no '+', no remote name: the URL is the forwarder's and nothing is stored in the config
	if _, err := g.run(ctx, g.runner, "push", "-q", "--no-verify", "--no-recurse-submodules", "--no-signed", "--atomic", "--end-of-options", g.repoURL(), br+":"+br); err != nil {
		g.fact("git.refused", map[string]any{"op": "push", "code": "push_rejected"})
		return hostd.Reply{Status: "error", Result: map[string]string{"code": "push_rejected", "why": cleanText(err.Error(), 200)}}
	}
	sha = strings.TrimSpace(sha)
	g.fact("git.pushed", map[string]any{"sha": sha, "commits": n, "files": strings.Count(names, "\x00")})
	return hostd.Reply{Status: "ok", Result: map[string]any{"pushed": true, "sha": sha, "commits": n}}
}

type snapStats struct {
	files int
	bytes int64
}

// clearSnapshot empties the snapshot directory (not the directory itself, which is ours and 0700).
func (g *GitOps) clearSnapshot() {
	es, _ := os.ReadDir(g.cfg.Snapshot)
	for _, e := range es {
		os.RemoveAll(path.Join(g.cfg.Snapshot, e.Name()))
	}
}

// snapshot copies Work into Snapshot through os.Root, so no path inside the fence's tree can lead the copy
// outside it (a symlink that stays inside is recreated as a symlink, one that leaves is refused by Root). Special
// files, hard links, oversized files and a tree beyond the caps refuse the whole snapshot, as H1 refuses them.
func (g *GitOps) snapshot() (snapStats, error) {
	var st snapStats
	g.clearSnapshot()
	root, err := os.OpenRoot(g.cfg.Work)
	if err != nil {
		return st, err
	}
	defer root.Close()
	var walk func(rel string) error
	walk = func(rel string) error {
		d, err := root.Open(rel)
		if err != nil {
			return err
		}
		ents, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			return err
		}
		for _, e := range ents {
			name := path.Join(rel, e.Name())
			if rel == "." {
				name = e.Name()
			}
			if st.files++; st.files > g.cfg.MaxFiles {
				return fmt.Errorf("more than %d entries", g.cfg.MaxFiles)
			}
			fi, err := root.Lstat(name)
			if err != nil {
				return err
			}
			dst := path.Join(g.cfg.Snapshot, name)
			switch m := fi.Mode(); {
			case m.IsDir():
				if e.Name() == ".git" {
					return fmt.Errorf("%s: a .git inside the worktree", name)
				}
				if err := os.Mkdir(dst, 0o700); err != nil {
					return err
				}
				if err := walk(name); err != nil {
					return err
				}
			case m&fs.ModeSymlink != 0:
				target, err := root.Readlink(name)
				if err != nil {
					return err
				}
				if err := os.Symlink(target, dst); err != nil {
					return err
				}
			case m.IsRegular():
				if fi.Size() > g.cfg.MaxFileBytes {
					return fmt.Errorf("%s is %d bytes (the cap is %d)", name, fi.Size(), g.cfg.MaxFileBytes)
				}
				if sys, ok := fi.Sys().(*syscall.Stat_t); ok && sys.Nlink > 1 {
					return fmt.Errorf("%s has more than one hard link", name)
				}
				if st.bytes += fi.Size(); st.bytes > g.cfg.MaxSnapshotBytes {
					return fmt.Errorf("the tree is over %d bytes", g.cfg.MaxSnapshotBytes)
				}
				if err := copyOne(root, name, dst, fi, g.cfg.MaxFileBytes); err != nil {
					return fmt.Errorf("%s: %w", name, err)
				}
			default:
				return fmt.Errorf("%s is not a regular file, directory or link", name)
			}
		}
		return nil
	}
	return st, walk(".")
}

// copyOne copies one regular file. It opens O_NONBLOCK and checks the OPENED file, so a file swapped for a FIFO
// after the Lstat cannot hang the sidecar.
func copyOne(root *os.Root, name, dst string, lfi os.FileInfo, max int64) error {
	f, err := root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() || !os.SameFile(fi, lfi) {
		return errors.New("changed while it was being copied")
	}
	mode := os.FileMode(0o600)
	if fi.Mode()&0o111 != 0 {
		mode = 0o700
	}
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	n, err := io.Copy(out, io.LimitReader(f, max+1))
	if cerr := out.Close(); err == nil {
		err = cerr
	}
	if err == nil && n > max {
		err = errors.New("grew past the size cap while it was being copied")
	}
	return err
}

// Route sends each request type to its handler: msg.* to the bus, git.* to the git handler; anything else is
// refused. Either may be nil, in which case its types are refused as unavailable.
func Route(bus, git func(context.Context, hostd.Request) hostd.Reply) func(context.Context, hostd.Request) hostd.Reply {
	return func(ctx context.Context, q hostd.Request) hostd.Reply {
		switch {
		case strings.HasPrefix(q.Type, "msg.") && bus != nil:
			return bus(ctx, q)
		case strings.HasPrefix(q.Type, "git.") && git != nil:
			return git(ctx, q)
		}
		return refuse("unsupported_in_pod", "")
	}
}
