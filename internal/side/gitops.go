// gitops.go is the pod's side of git (plan F3c, with H1's rules): clone the session's repo into the worktree,
// commit what the fence wrote, push the session branch. It exists so a fenced pod can do a day's git work
// without the fence ever holding a git credential, a remote, or a say in which repo or branch is touched.
//
// What the sidecar does and why it is safe to do it here:
//   - The repo, base and branch come from flags in the pod spec (GitConfig), never from a request: a
//     git.commit body carries only a message, a git.push body nothing.
//   - Only the sidecar's own git can reach the door's git routes: it sends a per-process secret (fwd.GitHeader), and
//     the forwarder the fence uses refuses every /git/ path without it. (Git 2.43 cannot speak HTTP over a unix
//     socket, and a second loopback port would be visible to the fence, so the capability rides on the one port.)
//   - Git runs with H1's hardened environment and -c overrides (read off gitroute.Runner, which this package
//     never edits), plus network overrides of its own: only http to the forwarder, no redirects, no proxy, fsck on
//     everything received. The gitdir lives in a sidecar-only directory outside /work, and H1's repo check
//     (gitroute's checkRepo, run by Runner.Cmd) is repeated before EVERY git command, fetch and push included.
//   - Clone is staged: git checks the tree out into a sidecar-only directory, and the files are then created in
//     /work through os.Root with O_EXCL, so nothing the fence does to /work (a directory swapped for a link, say)
//     can send a write outside it. /work must be empty right before the copy, and a clone that finds it written
//     fails instead of merging.
//   - Commit is snapshot-then-commit. H1's Commit wants proof that the fence is stopped, and a pod's sidecar
//     cannot freeze the fence container, so it copies what git would commit (tracked files plus untracked ones
//     that are not ignored, as `git ls-files` lists them) from /work through os.Root into a sidecar-only
//     directory and commits THERE with H1's Runner. A nested .git (file, link or directory) is never copied. The
//     snapshot cannot change under git, which is the whole point of the proof; the audit fact says "snapshot".
//   - Push sends exactly refs/heads/<branch>:refs/heads/<branch> through the forwarder: never a '+', a force,
//     a delete, a tag, main/master/HEAD/trunk (other protected branches are the door's call), or a branch that
//     fails check-ref-format. The door's rules (U16) are the real gate; these are the second lock. A push is
//     refused while a snapshot is being made.
//
// Journal facts (git.cloned, git.committed, git.pushed, git.refused) carry shas and counts, never content.
package side

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/fwd"
	"github.com/landovsky/cc-sandbox/internal/gitname"
	"github.com/landovsky/cc-sandbox/internal/gitroute"
	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/journal"
)

// GitConfig is what the sidecar knows about the session's repository, from the pod spec.
type GitConfig struct {
	Repo   string // owner/name
	Base   string // a branch name, or (when it is exactly 40 or 64 hex digits) a commit id
	Branch string // the session branch: the only ref that is ever pushed
	URL    string // the forwarder's git base, e.g. http://127.0.0.1:8080/egress/git
	GitDir string // sidecar-only, outside Work and Snapshot
	Work   string // the worktree emptyDir (the fence writes it)
	// Snapshot is a sidecar-only directory (one level below its volume, made 0700): the staging area of the clone
	// and the copy of Work that a commit is made from.
	Snapshot string
	Sid      string
	Gen      int
	Journal  Appender
	Gate     func() string // the forwarder's git secret (Server.GitGate)
	Git      string        // git binary (default /usr/bin/git)

	Timeout          time.Duration // per git process (default 2m)
	ListTimeout      time.Duration // the listing of the live worktree, which a fence-planted .git can stall (default 20s)
	MaxFiles         int           // entries a snapshot may hold (default 100000)
	MaxDepth         int           // path components (default 64)
	MaxFileBytes     int64         // one file in the snapshot (default 16 MiB)
	MaxSnapshotBytes int64         // total, counting allocated blocks so a sparse file is not free (default 1 GiB)
	MaxPackBytes     int64         // the forwarder's body cap; http.postBuffer is set just above it (default 32 MiB)
	MaxCommits       int           // a push of this many commits or more is refused (the door's compare-API cap, 250)
	MaxChangedFiles  int           // ... or this many changed files (300)
	MaxFacts         int           // git.refused facts per lifetime (default 50)
	Logf             func(string, ...any)
}

// hexRe: a base of exactly 40 or 64 hex digits is a commit id, never a branch.
var hexRe = regexp.MustCompile(`^([0-9a-f]{40}|[0-9a-f]{64})$`)

// ValidBranch reports whether b may be the session branch (the one definition lives in internal/gitname, shared with the
// launcher so a bad name is refused at the start, not minutes later as "pod not ready").
func ValidBranch(b string) bool { return gitname.Branch(b) == nil }

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

	// test seams
	hookEntry    func(rel string) // called before each snapshot entry is read
	hookSnapshot func()           // called once a snapshot has begun, holding mu
	hookCopy     func()           // called after the checkout is staged, before it is copied into Work
}

// NewGitOps checks the configuration: the repo and branch are well-formed, the branch is not protected and not
// the base, the URL is plain http to a literal loopback address, and the three directories are distinct.
func NewGitOps(cfg GitConfig) (*GitOps, error) {
	if cfg.Base == "" {
		cfg.Base = "main"
	}
	if err := gitname.Pair(cfg.Repo, cfg.Base, cfg.Branch); err != nil {
		return nil, fmt.Errorf("side git: %w", err)
	}
	u, err := url.Parse(cfg.URL)
	if err != nil || u.Scheme != "http" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || strings.HasSuffix(cfg.URL, "/") ||
		u.Port() == "" || u.Path == "" {
		return nil, errors.New("side git: --git-url must be http://<loopback ip>:<port>/<path> with no credentials, query or trailing slash")
	}
	if ip := net.ParseIP(u.Hostname()); ip == nil || !ip.IsLoopback() {
		return nil, errors.New("side git: --git-url must name a literal loopback address (the forwarder)")
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
	if cfg.ListTimeout == 0 {
		cfg.ListTimeout = 20 * time.Second
	}
	if cfg.MaxFiles == 0 {
		cfg.MaxFiles = 100000
	}
	if cfg.MaxDepth == 0 {
		cfg.MaxDepth = 64
	}
	if cfg.MaxFileBytes == 0 {
		cfg.MaxFileBytes = 16 << 20
	}
	if cfg.MaxSnapshotBytes == 0 {
		cfg.MaxSnapshotBytes = 1 << 30
	}
	if cfg.MaxPackBytes == 0 {
		cfg.MaxPackBytes = 32 << 20
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
		over, first := g.facts > g.cfg.MaxFacts, g.facts == g.cfg.MaxFacts+1
		g.factMu.Unlock()
		if over {
			if first { // say once that the rest are not written, as the relay does for sheds
				g.fact("git.facts_suppressed", map[string]any{"after": g.cfg.MaxFacts})
			}
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

type capBuf struct{ b bytes.Buffer }

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

// overrides returns H1's, ours, the post buffer (just above the forwarder's body cap, so a body is sent whole with a
// Content-Length) and the forwarder's git secret.
func (g *GitOps) overrides() []string {
	ov := append([]string(nil), g.ov...)
	ov = append(ov, fmt.Sprintf("http.postBuffer=%d", g.cfg.MaxPackBytes+4096))
	if g.cfg.Gate != nil {
		ov = append(ov, "http.extraHeader="+fwd.GitHeader+": "+g.cfg.Gate())
	}
	return ov
}

// run runs git with H1's environment for r and the overrides, in its own process group, with a timeout. H1's repo
// check runs first, every time, so a gitdir that grew a config include or info/attributes is refused.
func (g *GitOps) run(ctx context.Context, r *gitroute.Runner, args ...string) (string, error) {
	if _, err := r.Cmd("status", "--porcelain"); err != nil {
		return "", fmt.Errorf("the gitdir failed its check: %w", err)
	}
	ctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()
	ov := g.overrides()
	argv := make([]string, 0, 2*len(ov)+len(args))
	for _, o := range ov {
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

// wipeDir empties a sidecar-only directory (not the directory itself, which is ours and 0700).
func wipeDir(dir string) {
	es, _ := os.ReadDir(dir)
	for _, e := range es {
		os.RemoveAll(filepath.Join(dir, e.Name()))
	}
}

// recovered reports whether an earlier run of this sidecar already cloned: the gitdir holds the base and HEAD is the
// session branch. A restarted sidecar (its volumes outlive the container) then carries on without touching /work.
func (g *GitOps) recovered(ctx context.Context) bool {
	if _, err := os.Stat(g.cfg.GitDir + "/HEAD"); err != nil {
		return false
	}
	var err error
	if g.ov, err = overridesOf(g.runner); err != nil {
		return false
	}
	head, err := g.run(ctx, g.runner, "symbolic-ref", "-q", "HEAD")
	if err != nil || strings.TrimSpace(head) != "refs/heads/"+g.cfg.Branch {
		return false
	}
	_, err = g.run(ctx, g.runner, "rev-parse", "--verify", "--end-of-options", "refs/ccsbx/base^{commit}")
	return err == nil
}

// Clone fetches the base into the sidecar's gitdir, stages the checkout in the snapshot directory and creates the
// files in the (empty) worktree through os.Root, once. Call it after the token is loaded. A restart that finds the
// work already done carries on; a failure before anything reached /work leaves nothing behind, so it can be retried.
func (g *GitOps) Clone(ctx context.Context) (err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.cloned.Load() {
		return nil
	}
	if g.recovered(ctx) {
		g.cloned.Store(true)
		return nil
	}
	root, err := os.OpenRoot(g.cfg.Work)
	if err != nil {
		return fmt.Errorf("worktree: %w", err)
	}
	defer root.Close()
	if err := emptyRoot(root); err != nil {
		return err
	}
	touched := false
	defer func() {
		wipeDir(g.cfg.Snapshot)
		if err != nil && !touched {
			wipeDir(g.cfg.GitDir) // nothing reached /work: start clean next time
		}
	}()
	wipeDir(g.cfg.GitDir)
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
	// the checkout is staged in the sidecar's own directory: git never writes into /work
	wipeDir(g.cfg.Snapshot)
	stage := *g.runner
	stage.Worktree = g.cfg.Snapshot
	if _, err := g.run(ctx, &stage, "read-tree", "-u", "--reset", sha); err != nil {
		return err
	}
	if g.hookCopy != nil {
		g.hookCopy()
	}
	touched = true // from here on /work may hold some of it
	n, err := copyInto(root, g.cfg.Snapshot)
	if err != nil {
		return fmt.Errorf("copying the checkout into the worktree: %w", err)
	}
	for _, a := range [][]string{{"update-ref", "refs/heads/" + g.cfg.Branch, sha}, {"symbolic-ref", "HEAD", "refs/heads/" + g.cfg.Branch}} {
		if _, err := g.run(ctx, g.runner, a...); err != nil {
			return err
		}
	}
	g.cloned.Store(true)
	g.fact("git.cloned", map[string]any{"base": sha, "files": n})
	return nil
}

// emptyRoot refuses a worktree that already holds anything.
func emptyRoot(root *os.Root) error {
	d, err := root.Open(".")
	if err != nil {
		return err
	}
	defer d.Close()
	names, err := d.Readdirnames(1)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if len(names) != 0 {
		return errors.New("the worktree is not empty: refusing to check out into a tree the fence may have written")
	}
	return nil
}

// copyInto creates every file, directory and link of src (a sidecar-only tree) in root, through os.Root with
// O_EXCL: nothing can be redirected out of root, and anything the fence put there first makes the copy fail. Files
// get 0664 (0775 when executable) and directories 0775, set on the opened file itself, so the fence's group can
// write what the sidecar created whatever the process umask is.
func copyInto(root *os.Root, src string) (int, error) {
	n := 0
	var walk func(rel string) error
	walk = func(rel string) error {
		d, err := os.Open(filepath.Join(src, rel))
		if err != nil {
			return err
		}
		ents, err := d.ReadDir(-1)
		d.Close()
		if err != nil {
			return err
		}
		sort.Slice(ents, func(i, j int) bool { return ents[i].Name() < ents[j].Name() })
		for _, e := range ents {
			name := filepath.Join(rel, e.Name())
			fi, err := os.Lstat(filepath.Join(src, name))
			if err != nil {
				return err
			}
			n++
			switch m := fi.Mode(); {
			case m.IsDir():
				if err := root.Mkdir(name, 0o775); err != nil {
					return err
				}
				df, err := root.Open(name)
				if err != nil {
					return err
				}
				err = df.Chmod(0o775)
				df.Close()
				if err != nil {
					return err
				}
				if err := walk(name); err != nil {
					return err
				}
			case m&fs.ModeSymlink != 0:
				target, err := os.Readlink(filepath.Join(src, name))
				if err != nil {
					return err
				}
				if err := root.Symlink(target, name); err != nil {
					return err
				}
			case m.IsRegular():
				mode := os.FileMode(0o664)
				if m&0o111 != 0 {
					mode = 0o775
				}
				in, err := os.Open(filepath.Join(src, name))
				if err != nil {
					return err
				}
				out, err := root.OpenFile(name, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
				if err != nil {
					in.Close()
					return err
				}
				_, err = io.Copy(out, in)
				if err == nil {
					err = out.Chmod(mode)
				}
				in.Close()
				if cerr := out.Close(); err == nil {
					err = cerr
				}
				if err != nil {
					return err
				}
			default:
				return fmt.Errorf("%s is not a regular file, directory or link", name)
			}
		}
		return nil
	}
	return n, walk(".")
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
	snap, err := g.snapshot(ctx)
	defer wipeDir(g.cfg.Snapshot)
	if err != nil {
		return g.refused("commit", "snapshot_refused", err.Error())
	}
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
	g.fact("git.committed", map[string]any{"sha": sha, "files": snap.files, "bytes": snap.bytes, "skipped": snap.skipped, "proof": "snapshot"})
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
	files, skipped int
	bytes          int64
}

// listFiles streams `git ls-files -z --cached --others --exclude-standard` run by H1's runner against the live
// worktree with the sidecar's own gitdir, and calls fn for each path. It is what git would commit: ignored trees
// (node_modules, build output) never appear, so they can neither fill the snapshot nor block a commit. Streaming,
// so a tree of millions of entries is never held in memory.
func (g *GitOps) listFiles(ctx context.Context, fn func(p string) error) error {
	c, err := g.runner.Cmd("ls-files", "-z", "--cached", "--others", "--exclude-standard")
	if err != nil {
		return err
	}
	// git decides whether a directory is a nested repository by opening what a .git file inside it names, so a
	// fence-planted .git can point it at a FIFO and stall the listing. The listing therefore has its own, short
	// deadline: the commit is refused, the process group is killed, and the lock is released.
	ctx, cancel := context.WithTimeout(ctx, g.cfg.ListTimeout)
	defer cancel()
	out, err := c.StdoutPipe()
	if err != nil {
		return err
	}
	var errb capBuf
	c.Stderr = &errb
	if err := c.Start(); err != nil {
		return err
	}
	done := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
		case <-done:
		}
	}()
	br := bufio.NewReaderSize(out, 64<<10)
	var ferr error
	for ferr == nil {
		p, err := br.ReadString(0)
		if err != nil {
			if err != io.EOF || p != "" {
				ferr = fmt.Errorf("git ls-files: %v", err)
			}
			break
		}
		if p = p[:len(p)-1]; len(p) > 4096 {
			ferr = errors.New("a path is longer than 4096 bytes")
		} else {
			ferr = fn(p)
		}
	}
	if ferr != nil {
		syscall.Kill(-c.Process.Pid, syscall.SIGKILL)
	}
	close(done)
	io.Copy(io.Discard, out)
	werr := c.Wait()
	if ferr != nil {
		return ferr
	}
	if werr != nil {
		return fmt.Errorf("git ls-files: %w: %s", werr, cleanText(errb.b.String(), 200))
	}
	return nil
}

// snapshot copies what git would commit from Work into Snapshot through os.Root, so no path inside the fence's tree
// can lead the copy outside it. A nested .git (file, link, directory) is skipped, never copied, and counted. Special
// files, hard links, oversized files and a tree beyond the caps refuse the whole snapshot, as H1 refuses them.
func (g *GitOps) snapshot(ctx context.Context) (snapStats, error) {
	var st snapStats
	wipeDir(g.cfg.Snapshot)
	root, err := os.OpenRoot(g.cfg.Work)
	if err != nil {
		return st, err
	}
	defer root.Close()
	if g.hookSnapshot != nil {
		g.hookSnapshot()
	}
	seen := 0
	err = g.listFiles(ctx, func(p string) error {
		if g.hookEntry != nil {
			g.hookEntry(p)
		}
		if seen++; seen > g.cfg.MaxFiles {
			return fmt.Errorf("more than %d entries", g.cfg.MaxFiles)
		}
		parts := strings.Split(p, "/")
		if len(parts) > g.cfg.MaxDepth {
			return fmt.Errorf("%s is nested more than %d levels", p, g.cfg.MaxDepth)
		}
		for _, c := range parts {
			if c == ".git" || c == "" { // a nested repo (file, link or directory), or "dir/" for one
				st.skipped++
				return nil
			}
		}
		fi, err := root.Lstat(p)
		if errors.Is(err, fs.ErrNotExist) {
			return nil // tracked and deleted: the snapshot's absence is the deletion
		}
		if err != nil {
			return fmt.Errorf("%s: %w", p, err)
		}
		dst := filepath.Join(g.cfg.Snapshot, p)
		if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
			return err
		}
		switch m := fi.Mode(); {
		case m.IsDir(): // a gitlink (a submodule's directory): not ours to copy
			st.skipped++
		case m&fs.ModeSymlink != 0:
			target, err := root.Readlink(p)
			if err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			if err := os.Symlink(target, dst); err != nil {
				return err
			}
			st.files++
		case m.IsRegular():
			cost := fi.Size()
			if sys, ok := fi.Sys().(*syscall.Stat_t); ok {
				if sys.Nlink > 1 {
					return fmt.Errorf("%s has more than one hard link", p)
				}
				if alloc := sys.Blocks * 512; alloc > cost {
					cost = alloc // a sparse file costs what it occupies once copied
				}
			}
			if fi.Size() > g.cfg.MaxFileBytes {
				return fmt.Errorf("%s is %d bytes (the cap is %d)", p, fi.Size(), g.cfg.MaxFileBytes)
			}
			if st.bytes += cost; st.bytes > g.cfg.MaxSnapshotBytes {
				return fmt.Errorf("the tree is over %d bytes", g.cfg.MaxSnapshotBytes)
			}
			if err := copyOne(root, p, dst, fi, g.cfg.MaxFileBytes); err != nil {
				return fmt.Errorf("%s: %w", p, err)
			}
			st.files++
		default:
			return fmt.Errorf("%s is not a regular file, directory or link", p)
		}
		return nil
	})
	return st, err
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
