// Package worktree builds a session's worktree from {repo, base, branch} on the machine that runs it (R-WT, docs/r-wt-scope.md,
// direct-mode path). It exists so that a remote or local start can name a repository instead of a path: the daemon fetches it
// as the OWNER (the user hostd runs as: that user's own git configuration for access; nothing is written into the clone, and
// nothing of it ever enters the fence), keeps ONE bare clone per repo in the owner's state directory (never under a worktree
// root, so a start naming it as workdir.path cannot mount it), and adds a worktree for the session under the worktree root.
//
// Every git call is one argv, never a shell, with hostile-repository switches on the command line (they win over any config): hooks
// off, fsmonitor off, only the https and ssh transports, submodules and LFS filters off, object checking on, no prompts. The owner's
// git configuration is used for the FETCH only (credentials, url rewrites); every other call runs with no global config at all, so
// a hostile .gitattributes cannot pick one of the owner's filter drivers to run on a checkout. Values reach git only after the C5 rules of
// docs/m2c-remote-start.md: charset (start.ParseSpec), `git check-ref-format`, a fully qualified ref, the base resolved to a commit id
// by `rev-parse --verify --end-of-options`, and only that id handed to `worktree add`.
//
// Disk is bounded three ways: the clone (polled WHILE a fetch runs, which is then killed), the checkout (the tree is measured with
// `ls-tree -l` before anything is written), and the age of finished sessions' worktrees (Prune). A clone with live worktrees is never
// deleted: an oversized fetch into it is rolled back instead.
package worktree

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Error is a refusal the caller turns into bad_spec with a FIXED message (no repo text, no OS text, no paths).
type Error struct{ Msg string }

func (e *Error) Error() string { return e.Msg }

func refuse(m string) error { return &Error{m} }

// Limits are the tunables (hostd and `ccsbx start` expose each as a flag).
type Limits struct {
	FetchTimeout time.Duration // per fetch (default 120 s)
	MaxCloneMB   int64         // a clone may not grow past this (default 2048); the fetch is killed when it does
	MaxCheckout  int64         // MiB of file content a checkout may write (default 1024)
	MaxFiles     int           // entries a checkout may write (default 100000)
	LockWait     time.Duration // how long a start waits for a busy clone (default 60 s)
}

// Builder holds the configuration. The zero value is not usable: StateDir and Root are required.
type Builder struct {
	StateDir  string // <StateDir>/clones/<owner>/<github-owner>/<repo>.git
	Root      string // an absolute worktree root: sessions' worktrees are created directly under it (symlinks are resolved once)
	Git       string // default /usr/bin/git
	Limits    Limits
	Protocols []string // transports allowed (default https, ssh)
	Remote    func(repo string) string
	Env       []string // extra environment (tests)
}

// Handle is what Prepare made: everything Remove needs, so nothing is ever read back from the (agent-writable) worktree.
type Handle struct {
	Clone, Path, Branch string
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

func (l Limits) fetchTimeout() time.Duration {
	if l.FetchTimeout > 0 {
		return l.FetchTimeout
	}
	return 120 * time.Second
}

func (l Limits) cloneMB() int64 {
	if l.MaxCloneMB > 0 {
		return l.MaxCloneMB
	}
	return 2048
}

func (l Limits) checkoutMB() int64 {
	if l.MaxCheckout > 0 {
		return l.MaxCheckout
	}
	return 1024
}

func (l Limits) maxFiles() int {
	if l.MaxFiles > 0 {
		return l.MaxFiles
	}
	return 100000
}

func (l Limits) lockWait() time.Duration {
	if l.LockWait > 0 {
		return l.LockWait
	}
	return 60 * time.Second
}

// root is the worktree root with symlinks resolved (every comparison is between real paths).
func (b *Builder) root() string {
	if r, err := filepath.EvalSymlinks(b.Root); err == nil {
		return r
	}
	return filepath.Clean(b.Root)
}

// hardened is the command-line configuration every call carries (it beats any config file).
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

// env: with owner=true the owner's own configuration and agent (HOME, the agent socket) are visible: the FETCH only. Otherwise there is no global
// config at all and an empty HOME, so nothing of the owner's (filters, textconv, aliases) can be selected by the repository's content.
func (b *Builder) env(owner bool) []string {
	e := []string{
		"PATH=/usr/bin:/bin", "LANG=C.UTF-8", "GIT_TERMINAL_PROMPT=0", "GIT_CONFIG_NOSYSTEM=1", "GIT_ASKPASS=/bin/false",
		"GIT_SSH_COMMAND=ssh -o BatchMode=yes -o StrictHostKeyChecking=yes", "GIT_PAGER=cat", "GIT_OPTIONAL_LOCKS=0",
	}
	if owner {
		for _, k := range []string{"HOME", "SSH_AUTH_SOCK", "XDG_CONFIG_HOME"} {
			if v := os.Getenv(k); v != "" {
				e = append(e, k+"="+v)
			}
		}
	} else {
		e = append(e, "HOME=/nonexistent", "GIT_CONFIG_GLOBAL=/dev/null")
	}
	return append(e, b.Env...)
}

// run runs git with the owner's config hidden. runFetch runs it as the owner and kills it when watch says so.
func (b *Builder) run(d time.Duration, dir string, args ...string) (string, error) {
	return b.exec(d, dir, false, nil, args...)
}

func (b *Builder) exec(d time.Duration, dir string, owner bool, watch func() bool, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), d)
	defer cancel()
	c := exec.CommandContext(ctx, b.git(), append(b.hardened(), args...)...)
	c.Dir, c.Env = dir, b.env(owner)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.WaitDelay = 4 * time.Second
	cancelGroup, stopKill := groupCancel(c)
	c.Cancel = cancelGroup
	defer stopKill()
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if watch != nil {
		done := make(chan struct{})
		defer close(done)
		go func() {
			t := time.NewTicker(500 * time.Millisecond)
			defer t.Stop()
			for {
				select {
				case <-done:
					return
				case <-t.C:
					if watch() {
						cancel()
						return
					}
				}
			}
		}()
	}
	if err := c.Run(); err != nil {
		return "", fmt.Errorf("git %s: %v", args[0], err)
	}
	return strings.TrimSpace(out.String()), nil
}

// groupCancel stops the WHOLE process group (git-remote-https, ssh and index-pack must not outlive a timeout and keep writing into the
// clone): SIGTERM first, so git removes its own lock files, and SIGKILL to the group two seconds later if anything is left.
// The second return stops the pending SIGKILL: call it once the command has been waited for, so a process group id that was reused
// in the meantime is never killed.
func groupCancel(c *exec.Cmd) (func() error, func()) {
	var mu sync.Mutex
	var timer *time.Timer
	cancel := func() error {
		pid := c.Process.Pid
		mu.Lock()
		timer = time.AfterFunc(2*time.Second, func() { syscall.Kill(-pid, syscall.SIGKILL) })
		mu.Unlock()
		return syscall.Kill(-pid, syscall.SIGTERM)
	}
	stop := func() {
		mu.Lock()
		if timer != nil {
			timer.Stop()
		}
		mu.Unlock()
	}
	return cancel, stop
}

// staleLocks removes git lock files a killed process left in the clone. The caller holds the clone's flock, so no git of ours is running
// in it; a lock file older than staleAfter belongs to nobody. (A host git commit through the host route may hold one for a moment:
// only old ones go.)
func staleLocks(clone string, staleAfter time.Duration) {
	now := time.Now()
	filepath.Walk(clone, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() && strings.HasSuffix(p, ".lock") && now.Sub(fi.ModTime()) > staleAfter {
			os.Remove(p)
		}
		return nil
	})
}

// lock takes the per-clone flock, waiting at most wait (a hung fetch must not hold every start of that repo for ever).
func lock(path string, wait time.Duration) (func(), error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err == nil {
			return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
		}
		if time.Now().After(deadline) {
			f.Close()
			return nil, fmt.Errorf("busy")
		}
		time.Sleep(100 * time.Millisecond)
	}
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

func (b *Builder) cloneDir(owner string, m []string) string {
	return filepath.Join(b.StateDir, "clones", owner, m[1], m[2]+".git") // nested: injective, unlike a joined name
}

// Prepare returns a handle to a new worktree of repo at base on a NEW branch. owner is the session owner (the clone is
// theirs), label names the worktree directory. Any failure removes what it created, the branch included.
func (b *Builder) Prepare(owner, label, repo, base, branch string) (*Handle, error) {
	m := repoRe.FindStringSubmatch(repo)
	if m == nil || !ownerRe.MatchString(owner) || b.StateDir == "" || !filepath.IsAbs(b.Root) {
		return nil, refuse("the repository is not acceptable")
	}
	if strings.HasPrefix(base, "-") || strings.HasPrefix(branch, "-") {
		return nil, refuse("the base or branch is not a valid ref name")
	}
	if _, err := b.run(30*time.Second, "", "check-ref-format", "refs/heads/"+base); err != nil {
		return nil, refuse("the base is not a valid ref name")
	}
	if _, err := b.run(30*time.Second, "", "check-ref-format", "--branch", branch); err != nil {
		return nil, refuse("the branch is not a valid ref name")
	}
	clone := b.cloneDir(owner, m)
	if err := os.MkdirAll(filepath.Dir(clone), 0o700); err != nil {
		return nil, refuse("the clone cache is not writable")
	}
	unlock, err := lock(clone+".lock", b.Limits.lockWait())
	if err != nil {
		return nil, refuse("the repository is busy: try again")
	}
	defer unlock()

	created := false
	if _, err := os.Stat(clone); os.IsNotExist(err) {
		created = true
		if _, err := b.run(30*time.Second, "", "init", "--bare", "-q", "--", clone); err != nil {
			os.RemoveAll(clone)
			return nil, refuse("the repository could not be set up")
		}
	}
	// the origin is ours to set every time: never trust what an earlier life of the clone left
	want := b.remote(repo)
	if cur, err := b.run(30*time.Second, clone, "config", "--get", "remote.origin.url"); err != nil {
		if _, err := b.run(30*time.Second, clone, "remote", "add", "origin", want); err != nil {
			b.dropIfNew(clone, created)
			return nil, refuse("the repository could not be set up")
		}
	} else if cur != want {
		if _, err := b.run(30*time.Second, clone, "remote", "set-url", "origin", want); err != nil {
			b.dropIfNew(clone, created)
			return nil, refuse("the repository could not be set up")
		}
	}
	// fetch ONLY the base branch (full history of that one ref), into a namespace of ours; the size of the clone is polled
	// while it runs, and the fetch is killed when it passes the cap
	ref := "refs/remotes/origin/" + base
	staleLocks(clone, 10*time.Minute)
	oldSha, herr := b.run(30*time.Second, clone, "rev-parse", "--verify", "--quiet", "--end-of-options", ref+"^{commit}")
	if herr != nil {
		oldSha = ""
	}
	packsBefore := packSet(clone)
	fetchStart := time.Now()
	over := func() bool { return dirMB(clone) > b.Limits.cloneMB() }
	if _, err := b.exec(b.Limits.fetchTimeout(), clone, true, over, "-c", "fetch.unpackLimit=1", "-c", "transfer.unpackLimit=1", "fetch", "--no-tags", "--no-recurse-submodules", "--prune", "origin", "+refs/heads/"+base+":"+ref); err != nil {
		tooBig := over()
		b.rollbackFetch(clone, created, ref, oldSha, packsBefore, fetchStart)
		if tooBig {
			return nil, refuse("the repository is larger than this machine allows")
		}
		return nil, refuse("the repository or branch could not be fetched")
	}
	if over() {
		b.rollbackFetch(clone, created, ref, oldSha, packsBefore, fetchStart)
		return nil, refuse("the repository is larger than this machine allows")
	}
	sha, err := b.run(30*time.Second, clone, "rev-parse", "--verify", "--end-of-options", ref+"^{commit}")
	if err != nil || !shaRe.MatchString(sha) {
		return nil, refuse("the base does not name a commit")
	}
	// the checkout is measured BEFORE anything is written: a tree of highly compressible or heavily shared blobs is small as a
	// clone and enormous as files
	if err := b.checkTree(clone, sha); err != nil {
		return nil, err
	}
	if _, err := b.run(30*time.Second, clone, "show-ref", "--verify", "--quiet", "refs/heads/"+branch); err == nil {
		return nil, refuse("the branch already exists")
	}
	path, err := b.newPath(label)
	if err != nil {
		return nil, refuse("the worktree root is not writable")
	}
	h := &Handle{Clone: clone, Path: path, Branch: branch}
	// `-b <branch>` cannot follow `--`; the name passed check-ref-format --branch and the charset and cannot start with '-'.
	// The path follows `--`; the commit-ish is the resolved id, never a name.
	if _, err := b.run(b.Limits.fetchTimeout(), clone, "worktree", "add", "-b", branch, "--", path, sha); err != nil {
		b.removeLocked(h)
		return nil, refuse("the worktree could not be created")
	}
	real, err := filepath.EvalSymlinks(path)
	if err != nil {
		b.removeLocked(h)
		return nil, refuse("the worktree could not be created")
	}
	h.Path = real
	// remember the branch this worktree was made for (the session may switch branches; cleanup deletes THIS one)
	os.MkdirAll(filepath.Join(clone, "ccsbx-wt"), 0o700)
	os.WriteFile(filepath.Join(clone, "ccsbx-wt", filepath.Base(real)), []byte(branch), 0o600)
	return h, nil
}

// checkTree refuses a commit whose checkout would write more than the caps (ls-tree -l sizes every blob, a shared blob once per path).
// The listing is STREAMED and counted as it arrives: a tiny repository of nested shared trees names billions of paths, so git is killed (its
// whole process group) the moment a limit is passed, and the bytes read are capped too. Nothing is ever held whole in memory.
func (b *Builder) checkTree(clone, sha string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c := exec.CommandContext(ctx, b.git(), append(b.hardened(), "ls-tree", "-r", "-l", "--full-tree", "-z", "--end-of-options", sha)...)
	c.Dir, c.Env = clone, b.env(false)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	c.WaitDelay = 4 * time.Second
	cancelGroup, stopKill := groupCancel(c)
	c.Cancel = cancelGroup
	defer stopKill()
	out, err := c.StdoutPipe()
	if err != nil {
		return refuse("the tree could not be measured")
	}
	if err := c.Start(); err != nil {
		return refuse("the tree could not be measured")
	}
	rd := bufio.NewReaderSize(out, 64<<10)
	var total, bytesRead int64
	files := 0
	verdict := error(nil)
	for verdict == nil {
		rec, err := rd.ReadString(0)
		bytesRead += int64(len(rec))
		if len(rec) > 0 && rec[len(rec)-1] == 0 {
			meta, _, ok := strings.Cut(rec[:len(rec)-1], "\t")
			f := strings.Fields(meta)
			switch {
			case !ok || len(f) != 4:
				verdict = refuse("the tree could not be measured")
			case f[1] == "commit": // a submodule entry: never fetched, never checked out
			default:
				files++
				if n, perr := strconv.ParseInt(f[3], 10, 64); perr == nil {
					total += n
				}
				if files > b.Limits.maxFiles() || total>>20 > b.Limits.checkoutMB() {
					verdict = refuse("the checkout is larger than this machine allows")
				}
			}
		}
		if verdict == nil && bytesRead > 256<<20 {
			verdict = refuse("the tree is larger than this machine allows")
		}
		if err != nil {
			break
		}
	}
	if verdict != nil {
		cancel() // kills the group; the rest of the listing is never read
		c.Wait()
		return verdict
	}
	if err := c.Wait(); err != nil {
		return refuse("the tree could not be measured")
	}
	return nil
}

// dropIfNew deletes a clone this call created and could not make usable; an existing clone is never deleted.
func (b *Builder) dropIfNew(clone string, created bool) {
	if created {
		os.RemoveAll(clone)
	}
}

// packSet lists the pack files of a clone (names only).
func packSet(clone string) map[string]bool {
	m := map[string]bool{}
	ents, _ := os.ReadDir(filepath.Join(clone, "objects", "pack"))
	for _, e := range ents {
		m[e.Name()] = true
	}
	return m
}

// rollbackFetch undoes a fetch that failed or went over the cap. A clone this call created is deleted. An existing clone (it may have live
// worktrees, and a host git commit may be writing objects that nothing references yet) keeps everything it had: the ref goes back to what
// it was (or is dropped if there was none), and ONLY the pack files this fetch created are deleted, together with its temporary files.
// Nothing can reference those packs yet, and no pruning of unreferenced objects is ever done here: that is git's own expiry, in Prune.
func (b *Builder) rollbackFetch(clone string, created bool, ref, oldSha string, before map[string]bool, since time.Time) {
	if created {
		os.RemoveAll(clone)
		return
	}
	if oldSha != "" {
		b.run(30*time.Second, clone, "update-ref", ref, oldSha)
	} else {
		b.run(30*time.Second, clone, "update-ref", "-d", ref)
	}
	ents, _ := os.ReadDir(filepath.Join(clone, "objects", "pack"))
	for _, e := range ents {
		if !before[e.Name()] {
			os.Remove(filepath.Join(clone, "objects", "pack", e.Name()))
		}
	}
	// the lock files THIS killed fetch can have left (ours to remove: nothing of ours runs in the clone now)
	for _, l := range []string{"packed-refs.lock", "config.lock", ref + ".lock"} {
		p := filepath.Join(clone, l)
		if fi, err := os.Stat(p); err == nil && !fi.ModTime().Before(since.Add(-2*time.Second)) { // made by THIS fetch, not by something else
			os.Remove(p)
		}
	}
}

// gc reclaims unreachable objects older than git's own expiry (two weeks by default). It never uses --prune=now: an object a host git
// commit has just written and not yet referenced must survive.
func (b *Builder) gc(clone string) {
	b.run(b.Limits.fetchTimeout(), clone, "gc", "-q")
}

// newPath makes a fresh, unused directory name directly under the REAL root (the directory itself is made by `worktree add`).
func (b *Builder) newPath(label string) (string, error) {
	root := b.root()
	if st, err := os.Stat(root); err != nil || !st.IsDir() {
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
		p := filepath.Join(root, slug+"-"+hex.EncodeToString(r[:]))
		if _, err := os.Lstat(p); os.IsNotExist(err) {
			return p, nil
		}
	}
	return "", fmt.Errorf("no free name")
}

// Remove deletes the worktree a Handle names, its registration and its branch, under the clone's lock. The clone is the one
// Prepare recorded, never one read back from the worktree (the agent can rewrite the worktree's .git pointer).
func (b *Builder) Remove(h *Handle) error {
	if h == nil || filepath.Dir(filepath.Clean(h.Path)) != b.root() {
		return refuse("not a worktree of this builder")
	}
	// wait as long as the longest holder can run (a fetch, a worktree add, a gc): giving up would leave the tree AND the branch behind
	unlock, err := lock(h.Clone+".lock", 3*b.Limits.fetchTimeout()+b.Limits.lockWait())
	if err != nil {
		return refuse("the repository is busy")
	}
	defer unlock()
	b.removeLocked(h)
	return nil
}

// RemoveSoon removes like Remove but never makes the caller wait long: it tries the clone's lock for 200 ms and, if the clone is busy
// (a slow fetch of another start), finishes the removal in the background with the full wait. The branch name is free as soon as it runs.
func (b *Builder) RemoveSoon(h *Handle, onErr func(error)) {
	if h == nil || filepath.Dir(filepath.Clean(h.Path)) != b.root() {
		if onErr != nil {
			onErr(refuse("not a worktree of this builder"))
		}
		return
	}
	if unlock, err := lock(h.Clone+".lock", 200*time.Millisecond); err == nil {
		b.removeLocked(h)
		unlock()
		return
	}
	bgRemovals.Add(1)
	go func() {
		defer bgRemovals.Done()
		if err := b.Remove(h); err != nil && onErr != nil {
			onErr(err)
		}
	}()
}

// bgRemovals counts removals still running in the background (tests wait for them).
var bgRemovals sync.WaitGroup

// WaitBackground waits for removals started by RemoveSoon (shutdown, tests).
func WaitBackground() { bgRemovals.Wait() }

// removeLocked: the caller holds the clone's lock.
func (b *Builder) removeLocked(h *Handle) {
	os.RemoveAll(h.Path)
	os.Remove(filepath.Join(h.Clone, "ccsbx-wt", filepath.Base(h.Path)))
	b.run(30*time.Second, h.Clone, "worktree", "prune")
	if h.Branch != "" {
		b.run(30*time.Second, h.Clone, "branch", "-D", "--", h.Branch)
	}
}

// Prune removes the builder's worktrees (and their branches) that nothing needs any more. It takes each clone's lock; for an old enough worktree
// it calls guard(path, remove), which decides with fresh records and calls remove() only if nothing needs the path, all under the caller's lock. Only worktrees directly under the real Root, registered in a clone of this
// state dir, are considered. Returns how many were removed.
func (b *Builder) Prune(olderThan time.Duration, guard func(path string, remove func()), now time.Time) int {
	removed := 0
	clones, _ := filepath.Glob(filepath.Join(b.StateDir, "clones", "*", "*", "*.git"))
	root := b.root()
	for _, clone := range clones {
		unlock, err := lock(clone+".lock", 5*time.Second)
		if err != nil {
			continue // busy: next hour
		}
		out, err := b.run(30*time.Second, clone, "worktree", "list", "--porcelain")
		if err != nil {
			unlock()
			continue
		}
		var path, branch string
		flush := func() {
			if path != "" && filepath.Dir(path) == root {
				if fi, err := os.Stat(path); err == nil && now.Sub(fi.ModTime()) > olderThan {
					b0 := ""
					if saved, err := os.ReadFile(filepath.Join(clone, "ccsbx-wt", filepath.Base(path))); err == nil {
						b0 = strings.TrimSpace(string(saved)) // the branch it was made for, not whatever it is on now
					} else if strings.HasPrefix(branch, "refs/heads/") {
						b0 = strings.TrimPrefix(branch, "refs/heads/")
					}
					p0 := path
					// guard decides AND removes under the caller's own lock (the daemon's): a session resumed between a check and a
					// removal cannot lose its tree
					guard(p0, func() { b.removeLocked(&Handle{Clone: clone, Path: p0, Branch: b0}); removed++ })
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
		if removed > 0 {
			b.gc(clone)
		}
		unlock()
	}
	return removed
}
