// Package backend runs one fence generation. This file is the docker backend (U1b) in direct
// mode: everything runs as the calling user, who is in the docker group, so invariant 14 is
// waived and every result is marked "not evidence".
//
// Layout per generation, under <state>/s/<sid>/g<gen>/ (parent 0700, so only the owner can walk
// in; the leaves are 0777 because uid 10001 inside the fence is not the owner):
//
//	out/       rw to the fence  -> /run/ccsbx/out   (this generation's outbox, never reused)
//	in/        ro to the fence  -> /run/ccsbx/in    (written by the host before start)
//	managed/   ro to the fence  -> /etc/claude-code (compiled managed settings, U17)
//
// The home directory is per node (label), kept across generations so a resume sees the old
// transcript.
package backend

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

// ClassOf names the home class of a profile: "e2e" (may run unreviewed candidate images) or "prod".
func ClassOf(e2e bool) string {
	if e2e {
		return "e2e"
	}
	return "prod"
}

// FenceUID is the uid and gid inside the fence.
const FenceUID = 10001

// Spec is everything the docker argv depends on. It is a pure value so tests can check the
// flags without a docker daemon.
type Spec struct {
	Sid, Label   string
	Gen          int
	Image        string
	StateDir     string // daemon state dir; per-generation dirs live under it
	Workdir      string // host worktree, mounted rw at the same absolute path (resume is keyed by cwd)
	GitDir       string // gitdir from the daemon record (ResolveGitDir at first start), mounted ro; "" if none
	WorktreeRoot string // the operator-set root every worktree must be inside
	MemoryMB     int
	Pids         int
	Cmd          []string // command in the fence; default ["claude"]
	TTY          bool
	NotEvid      bool // direct mode
	Uid          int  // the host user who owns the session (record uid); the gitdir must be theirs
	// Chown makes a helper-mode directory the fence's; nil means os.Lchown. Tests replace it.
	Chown func(path string, uid, gid int) error
	// Seccomp, when set, is the path of the seccomp profile docker runs the fence with (ccsbx-root
	// sets it to a verified profile that refuses setuid/setgid chmod); "" means docker's default.
	Seccomp string
	// OutGid is the group of the service user (ccsbx) that owns hostd: helper mode makes out/
	// 10001:OutGid 02770 so hostd can read what the fence writes there (the files inherit the group).
	OutGid int
	// ServiceUID is the service user's uid: helper mode makes in/replies theirs (hostd writes its
	// replies there and nowhere else), while in/ itself and managed/ stay root's.
	ServiceUID int
	// Class is "e2e" or "prod": part of the home's key.
	Class string
	// MountWorkdir and MountGitDir, when set, are the host paths docker mounts instead of Workdir and
	// GitDir (the container still sees the original paths). ccsbx-root sets them to staging mounts of
	// directories it opened and checked itself, so a path swapped after the check is never mounted.
	MountWorkdir, MountGitDir string
}

// Names derived from the sid and gen, so cleanup targets exactly what this run created.
func (s Spec) Container() string { return fmt.Sprintf("ccsbx-%s-g%d", s.Sid, s.Gen) }
func (s Spec) Network() string   { return fmt.Sprintf("ccsbx-net-%s-g%d", s.Sid, s.Gen) }

// GenDir is <state>/s/<sid>/g<gen>.
func (s Spec) GenDir() string {
	return filepath.Join(s.StateDir, "s", s.Sid, "g"+strconv.Itoa(s.Gen))
}

// HomeDir is the per-node home, shared by every generation of the same owner, class and label. The
// owner and the class (e2e or production) are part of the key so one owner's session cannot open
// another's transcripts and an unreviewed candidate image never shares a production node's home.
func (s Spec) HomeDir() string {
	h := sha256.Sum256([]byte(fmt.Sprintf("%d|%s|%s", s.Uid, s.Class, s.Label)))
	return filepath.Join(s.StateDir, "home", hex.EncodeToString(h[:6]))
}

// NetworkArgs creates the per-generation internal network: no route out, no gateway.
func (s Spec) NetworkArgs() []string {
	// inhibit_ipv4: the host gets no address on the bridge, so a host service listening on a
	// wildcard address can't be reached at "the gateway" (that address is on-link, and no default
	// route doesn't cover it). Peers on the same network still talk to each other over L2.
	return []string{"network", "create", "--internal", "-o", "com.docker.network.bridge.inhibit_ipv4=true", "--label", "ccsbx.sid=" + s.Sid, "--label", "ccsbx.gen=" + strconv.Itoa(s.Gen), s.Network()}
}

// RunArgs is the fixed `docker run` argv. Nothing in it comes from the agent. The flags are the
// contract T:fence-flags checks, so change them only together with that test.
func (s Spec) RunArgs() []string {
	cmd := s.Cmd
	if len(cmd) == 0 {
		cmd = []string{"claude"}
	}
	a := []string{"run", "--rm", "--init",
		"--name", s.Container(),
		"--label", "ccsbx.sid=" + s.Sid, "--label", "ccsbx.gen=" + strconv.Itoa(s.Gen),
		"--user", fmt.Sprintf("%d:%d", FenceUID, FenceUID),
		"--read-only",
		"--cap-drop", "ALL",
		"--security-opt", "no-new-privileges",
		"--pids-limit", strconv.Itoa(s.Pids),
		"--memory", fmt.Sprintf("%dm", s.MemoryMB), "--memory-swap", fmt.Sprintf("%dm", s.MemoryMB),
		"--network", s.Network(),
		"--dns", "127.0.0.1", // no resolver: names never leave the container (T:no-dns-exfil)
		"--tmpfs", "/tmp:rw,nosuid,nodev,size=512m",
		"--workdir", s.Workdir,
		"--mount", bind(s.mountSrc(s.Workdir, s.MountWorkdir), s.Workdir, false),
	}
	if s.GitDir != "" {
		a = append(a, "--mount", bind(s.mountSrc(s.GitDir, s.MountGitDir), s.GitDir, true))
	}
	g := s.GenDir()
	a = append(a,
		"--mount", bind(filepath.Join(g, "out"), "/run/ccsbx/out", false),
		"--mount", bind(filepath.Join(g, "in"), "/run/ccsbx/in", true),
		"--mount", bind(filepath.Join(g, "managed"), "/etc/claude-code", true),
		"--mount", bind(s.HomeDir(), "/home/agent", false),
		// The environment is explicit and short: no host env is inherited.
		"-e", "HOME=/home/agent", "-e", "CCSBX_SID="+s.Sid, "-e", "CCSBX_GEN="+strconv.Itoa(s.Gen),
	)
	if s.Seccomp != "" {
		a = append(a, "--security-opt", "seccomp="+s.Seccomp)
	}
	if s.TTY {
		a = append(a, "-it")
	} else {
		a = append(a, "-i")
	}
	a = append(a, s.Image)
	return append(a, cmd...)
}

// Prepare creates the per-generation directories and the home. It refuses to reuse a generation
// directory: a second run of the same (sid, gen) would share an outbox (invariant 11).
//
// Direct mode (NotEvid): everything is made by the calling user, the leaves are 0777 because uid
// 10001 in the fence is not the owner, and the 0700 parent keeps other host users out.
// Helper mode (ccsbx-root, running as root): the parents are root-owned 0711, out/ and the home are
// 10001:10001 0700, in/ and managed/ are root-owned 0755 (the fence reads them and can never write
// them, with or without the read-only mount), and nothing is world-writable. Every directory is
// created fresh with Mkdir (never followed through a symlink) before it is chowned.
func (s Spec) Prepare() error {
	if _, err := CheckWorkdir(s.Workdir, s.WorktreeRoot, s.StateDir); err != nil {
		return err
	}
	if s.GitDir != "" {
		if err := CheckPath(s.GitDir); err != nil {
			return err
		}
		// Every generation, not only the first: the gitdir is mounted again and may have changed.
		if err := hostOwnedGitDir(s.GitDir, s.Uid); err != nil {
			return err
		}
	}
	helper := !s.NotEvid
	g := s.GenDir()
	parent := 0o700
	if helper {
		parent = 0o711 // others may traverse by name but not list the session ids
	}
	if err := os.MkdirAll(filepath.Dir(g), os.FileMode(parent)); err != nil {
		return err
	}
	if err := os.Mkdir(g, os.FileMode(parent)); err != nil { // fails if it exists: one outbox per generation
		return fmt.Errorf("generation dir %s: %w", g, err)
	}
	if err := s.prepareQuarantine(); err != nil {
		return err
	}
	for _, d := range []string{"out", "in", "managed"} {
		p := filepath.Join(g, d)
		if helper {
			mode, own, grp := os.FileMode(0o755), 0, 0
			if d == "out" {
				// 10001 writes, the service group can read and traverse but NOT create, rename or
				// delete (02750): new files take the group (setgid), and a file the fence makes with
				// the default umask 022 is group-readable. A file it makes unreadable to the group
				// is unreadable to the daemon, which tolerates that. The daemon still requires every
				// file's uid to be 10001.
				if s.OutGid <= 0 {
					return fmt.Errorf("helper mode needs the service user's gid for the outbox (OutGid)")
				}
				mode, own, grp = os.ModeSetgid|0o750, FenceUID, s.OutGid
			}
			if err := os.Mkdir(p, mode); err != nil {
				return err
			}
			if err := os.Chmod(p, mode); err != nil { // umask must not narrow or widen it
				return err
			}
			if err := s.chown(p, own, grp); err != nil {
				return err
			}
			if d == "in" {
				// in/ is root's (the fence reads it, nobody but root can replace what root wrote
				// there); only in/replies belongs to the daemon, which cannot rename or delete the
				// files in in/ itself. A service uid is required so the directory exists as intended.
				if s.ServiceUID <= 0 || s.OutGid <= 0 {
					return fmt.Errorf("helper mode needs the service user's uid and gid for in/replies")
				}
				rp := filepath.Join(p, "replies")
				if err := os.Mkdir(rp, 0o755); err != nil {
					return err
				}
				if err := os.Chmod(rp, 0o755); err != nil {
					return err
				}
				if err := s.chown(rp, s.ServiceUID, s.OutGid); err != nil {
					return err
				}
			}
			continue
		}
		if err := os.Mkdir(p, 0o700); err != nil {
			return err
		}
		// 0777 because uid 10001 (fence) must write out/ and read in/ and managed/; the 0700
		// parent keeps other host users out. A mount cannot change this in direct mode.
		if err := os.Chmod(p, 0o777); err != nil {
			return err
		}
	}
	if !helper {
		// hostd writes replies ONLY into in/replies and never creates it (in helper mode ccsbx-root
		// does): so direct mode makes it here, before the pane starts.
		if err := os.Mkdir(filepath.Join(g, "in", "replies"), 0o755); err != nil {
			return err
		}
	}
	h := s.HomeDir()
	if helper {
		if err := os.MkdirAll(filepath.Dir(h), 0o711); err != nil {
			return err
		}
		if err := os.Mkdir(h, 0o700); err == nil {
			if err := os.Chmod(h, 0o700); err != nil {
				return err
			}
			return s.chown(h, FenceUID, FenceUID)
		} else if !os.IsExist(err) {
			return err
		}
		return nil // a later generation: the home exists and is the fence's
	}
	if err := os.MkdirAll(h, 0o700); err != nil {
		return err
	}
	return os.Chmod(h, 0o777)
}

func (s Spec) mountSrc(orig, staged string) string {
	if staged != "" {
		return staged
	}
	return orig
}

func (s Spec) chown(path string, uid, gid int) error {
	if s.Chown != nil {
		return s.Chown(path, uid, gid)
	}
	return os.Lchown(path, uid, gid)
}

// Close marks the generation's outbox closed. The directory stays for the host to read.
func (s Spec) Close() error {
	return os.WriteFile(filepath.Join(s.GenDir(), "CLOSED"), []byte("closed\n"), 0o600)
}

// bind is one `--mount` bind spec. Unlike `-v a:b:ro` it has no colon syntax to inject into, and
// readonly is explicit. CheckPath keeps commas and newlines out of every path first.
func bind(src, dst string, ro bool) string {
	m := "type=bind,src=" + src + ",dst=" + dst
	if ro {
		m += ",readonly"
	}
	return m
}

// ResolveGitDir decides, ONCE, at the first start of a session, which gitdir the fence may see
// read-only. It must never be called on a worktree an agent has run in: the worktree's `.git`
// file is agent-writable, and whatever it names would be mounted into the next generation. The
// daemon stores the result in the session record and reuses it for every later generation.
//
// Accepted: no `.git` (returns ""), a real `.git` directory with a HEAD, or the gitdir of a
// linked worktree whose own `gitdir` file points back at this worktree's `.git`. That
// back-reference is something git writes and a random host directory cannot have, so
// `gitdir: /var/run/x` or `gitdir: /etc` fail. A `.git` symlink is refused.
func ResolveGitDir(workdir string, uid int) (string, error) {
	wt, err := filepath.EvalSymlinks(workdir)
	if err != nil {
		return "", err
	}
	p := filepath.Join(wt, ".git")
	st, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	switch {
	case st.Mode()&os.ModeSymlink != 0:
		return "", fmt.Errorf(".git in %s is a symlink", workdir)
	case st.IsDir():
		if !isGitDir(p) {
			return "", fmt.Errorf("%s is not a git directory", p)
		}
		if err := hostOwnedGitDir(p, uid); err != nil {
			return "", err
		}
		return p, nil
	}
	b, err := readSmall(p, maxGitFile)
	if err != nil {
		return "", err
	}
	rest, ok := strings.CutPrefix(strings.TrimSpace(string(b)), "gitdir:")
	if !ok {
		return "", fmt.Errorf("%s is not a gitdir file", p)
	}
	t := strings.TrimSpace(rest)
	if !filepath.IsAbs(t) {
		t = filepath.Join(wt, t)
	}
	t, err = filepath.EvalSymlinks(t)
	if err != nil {
		return "", fmt.Errorf("gitdir target: %w", err)
	}
	if !isGitDir(t) {
		return "", fmt.Errorf("gitdir target %s is not a git directory", t)
	}
	back, err := readSmall(filepath.Join(t, "gitdir"), maxGitFile)
	if err != nil {
		return "", fmt.Errorf("gitdir target %s has no back-reference to a worktree", t)
	}
	bp := strings.TrimSpace(string(back))
	if bd, err := filepath.EvalSymlinks(filepath.Dir(bp)); err != nil || filepath.Join(bd, filepath.Base(bp)) != p {
		return "", fmt.Errorf("gitdir target %s belongs to another worktree", t)
	}
	if err := CheckPath(t); err != nil {
		return "", err
	}
	if err := hostOwnedGitDir(t, uid); err != nil {
		return "", err
	}
	return t, nil
}

// maxGitFile bounds what is read from a `.git` file, a `gitdir` back-reference or a `commondir`: each
// is one short path. Nothing larger is a real git file, and an agent that left a huge or special file
// there must not make the daemon block or read gigabytes while it holds the start lock.
const maxGitFile = 4096

// readSmall reads a small regular file the way a daemon should read one an agent could have
// replaced: no symlink followed (O_NOFOLLOW), no blocking on a FIFO (O_NONBLOCK), judged on the
// opened descriptor (a regular file with one link, at most max bytes) and read with a hard cap.
func readSmall(path string, max int64) ([]byte, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !st.Mode().IsRegular() {
		return nil, fmt.Errorf("%s is not a regular file", path)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || sys.Nlink != 1 {
		return nil, fmt.Errorf("%s has more than one link", path)
	}
	if st.Size() > max {
		return nil, fmt.Errorf("%s is larger than %d bytes: not a git file", path, max)
	}
	b, err := io.ReadAll(io.LimitReader(f, max+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) > max {
		return nil, fmt.Errorf("%s grew past %d bytes while being read", path, max)
	}
	return b, nil
}

// hostOwnedGitDir applies, for a gitdir about to be mounted, the same rule host git applies before
// it runs (internal/gitroute): the gitdir and what git reads from it are owned by the calling host
// user (uid, never the uid of this process: in helper mode that is root) and are not writable by
// the world, by the fence's gid, or by a group the caller is not in. What is checked: the gitdir
// and, for a linked worktree, its commondir and the parent that holds the gitdir; in each, HEAD,
// config, config.worktree, commondir, gitdir, and everything under hooks/ and info/. A symlink
// anywhere in that set is refused, and so is any stat that does not carry an owner (fail closed).
// Known limit: POSIX ACLs and xattrs that grant the fence uid write access do not show in mode bits.
func hostOwnedGitDir(gd string, uid int) error {
	o, err := ownerOf(uid)
	if err != nil {
		return err
	}
	roots := []string{gd}
	switch cd, err := readSmall(filepath.Join(gd, "commondir"), maxGitFile); {
	case err == nil: // a linked worktree
		c := strings.TrimSpace(string(cd))
		if !filepath.IsAbs(c) {
			c = filepath.Join(gd, c)
		}
		c, err = filepath.EvalSymlinks(c)
		if err != nil {
			return fmt.Errorf("commondir of %s: %w", gd, err)
		}
		if !isGitDir(c) {
			return fmt.Errorf("commondir of %s (%s) is not a git directory", gd, c)
		}
		// the parent of the gitdir (.git/worktrees): a writable one allows a swap after the check
		if err := o.checkOne(filepath.Dir(gd)); err != nil {
			return err
		}
		roots = append(roots, c)
	case !os.IsNotExist(err):
		return fmt.Errorf("commondir of %s: %w", gd, err)
	}
	for _, d := range roots {
		for _, p := range []string{d, filepath.Join(d, "HEAD"), filepath.Join(d, "config"), filepath.Join(d, "config.worktree"), filepath.Join(d, "commondir"), filepath.Join(d, "gitdir")} {
			if err := o.checkOne(p); err != nil {
				return err
			}
		}
		for _, sub := range []string{"hooks", "info"} {
			err := filepath.WalkDir(filepath.Join(d, sub), func(p string, _ fs.DirEntry, err error) error {
				if err != nil {
					if os.IsNotExist(err) && p == filepath.Join(d, sub) {
						return nil
					}
					return err
				}
				return o.checkOne(p)
			})
			if err != nil {
				return err
			}
		}
	}
	return nil
}

// owner is who must own a gitdir and which groups may write it.
type owner struct {
	uid  int
	gids map[int]bool
}

func ownerOf(uid int) (owner, error) {
	o := owner{uid: uid, gids: map[int]bool{}}
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return o, fmt.Errorf("uid %d: %w", uid, err)
	}
	gs, err := u.GroupIds()
	if err != nil {
		return o, fmt.Errorf("groups of uid %d: %w", uid, err)
	}
	for _, g := range append(gs, u.Gid) {
		if n, err := strconv.Atoi(g); err == nil {
			o.gids[n] = true
		}
	}
	return o, nil
}

// checkOne lstats p (a missing path is fine: git does not need it) and applies check.
func (o owner) checkOne(p string) error {
	st, err := os.Lstat(p)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	return o.check(p, st)
}

func (o owner) check(p string, st fs.FileInfo) error {
	if st.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s is a symlink", p)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("%s: no owner information on this platform, refusing", p)
	}
	return o.checkMode(p, st.Mode().Perm(), int(sys.Uid), int(sys.Gid))
}

func (o owner) checkMode(p string, perm fs.FileMode, uid, gid int) error {
	if perm&0o002 != 0 {
		return fmt.Errorf("%s is world-writable", p)
	}
	if uid != o.uid {
		return fmt.Errorf("%s is owned by uid %d, not by the host user %d", p, uid, o.uid)
	}
	if perm&0o020 != 0 && (gid == FenceUID || !o.gids[gid]) {
		return fmt.Errorf("%s is writable by group %d, which the host user is not in (or is the fence's)", p, gid)
	}
	return nil
}

// CheckGitDirOwned is the ownership and mode walk of a gitdir for a caller outside this package
// (ccsbx-root runs it on the staged mount it is about to hand to docker).
func CheckGitDirOwned(gd string, uid int) error { return hostOwnedGitDir(gd, uid) }

func isGitDir(d string) bool {
	st, err := os.Lstat(filepath.Join(d, "HEAD"))
	return err == nil && st.Mode().IsRegular()
}

// CheckPath refuses characters that would change how a path is read in a mount spec.
func CheckPath(p string) error {
	if strings.ContainsAny(p, ",:\n\r\x00") {
		return fmt.Errorf("path %q contains a character that is not allowed in a mount (comma, colon, newline, NUL)", p)
	}
	return nil
}

// reserved are paths the fence mounts or needs itself; a worktree on or above one would shadow
// it (or be shadowed by it) and the mounts would fail or, worse, silently mask each other.
var reserved = []string{"/tmp", "/run", "/etc", "/home/agent", "/proc", "/sys", "/dev"}

// CheckWorkdirLexical is CheckWorkdir for a daemon that cannot see inside the owner's home (hostd as
// the service uid: /home/<owner> is 0750): it never touches the filesystem. The CLIENT resolved the
// path (no symlinks, absolute, clean) and the daemon only checks its SHAPE: clean and absolute, free
// of the characters CheckPath refuses, strictly inside one root of the comma-separated list (each a
// clean absolute entry, not "/"), not overlapping the state dir, the run dir or a fixed fence mount.
// ccsbx-root, as root, does the real validation at run time (an fd walk, owners, symlinks), and the
// roots it is given are real paths, so a lexical match here is never looser than the walk there.
func CheckWorkdirLexical(w, roots, stateDir, runDir string) (string, error) {
	if err := CheckPath(w); err != nil {
		return "", err
	}
	if !filepath.IsAbs(w) || filepath.Clean(w) != w {
		return "", fmt.Errorf("workdir %q must be a clean absolute path", w)
	}
	if roots == "" {
		return "", fmt.Errorf("no worktree root is configured (set --worktree-root)")
	}
	inside := false
	for _, r := range strings.Split(roots, ",") {
		if err := CheckPath(r); err != nil {
			return "", fmt.Errorf("worktree root %q: %w", r, err)
		}
		if !filepath.IsAbs(r) || filepath.Clean(r) != r || r == "/" {
			return "", fmt.Errorf("worktree root %q must be a clean absolute path and not /", r)
		}
		if strings.HasPrefix(w, r+string(filepath.Separator)) {
			inside = true
		}
	}
	if !inside {
		return "", fmt.Errorf("workdir %s is not inside the worktree root(s) %s", w, roots)
	}
	overlap := func(a, b string) bool {
		return a == b || strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
	}
	for _, d := range []string{stateDir, runDir} {
		if d != "" && overlap(w, filepath.Clean(d)) {
			return "", fmt.Errorf("workdir %s overlaps the daemon's state or run dir", w)
		}
	}
	for _, r := range reserved {
		if overlap(w, r) {
			return "", fmt.Errorf("workdir %s overlaps %s, which the fence mounts itself", w, r)
		}
	}
	return w, nil
}

// CheckGitDirLexical checks the SHAPE of a client-resolved gitdir without touching the filesystem:
// empty (no .git), exactly <workdir>/.git, or a clean absolute path whose last two segments are
// worktrees/<name> inside a .git directory (a linked worktree's gitdir). ccsbx-root verifies the real
// thing (the back-reference, the owners) at run time.
func CheckGitDirLexical(gd, workdir string) error {
	if gd == "" {
		return nil
	}
	if err := CheckPath(gd); err != nil {
		return err
	}
	if !filepath.IsAbs(gd) || filepath.Clean(gd) != gd {
		return fmt.Errorf("gitdir %q must be a clean absolute path", gd)
	}
	if gd == filepath.Join(workdir, ".git") {
		return nil
	}
	parent, name := filepath.Split(gd)
	parent = filepath.Clean(parent)
	if name == "" || filepath.Base(parent) != "worktrees" || filepath.Base(filepath.Dir(parent)) != ".git" {
		return fmt.Errorf("gitdir %s is neither <workdir>/.git nor <repo>/.git/worktrees/<name>", gd)
	}
	return nil
}

// CheckWorkdir decides whether a worktree path may be mounted rw into a fence. The path is
// resolved through symlinks first (a link `/x/wt -> /run` would otherwise pass a textual check)
// and must be strictly inside root, which the operator sets (CCSBX_WORKTREE_ROOT). It must not be
// the state dir or contain it, must not overlap a fixed mount point, and when root is a home
// directory it must not sit in a hidden directory of it (.ssh, .aws, .claude, .config ...).
func CheckWorkdir(w, root, stateDir string) (string, error) {
	if err := CheckPath(w); err != nil {
		return "", err
	}
	if !filepath.IsAbs(w) || filepath.Clean(w) != w {
		return "", fmt.Errorf("workdir %q must be a clean absolute path", w)
	}
	rw, err := filepath.EvalSymlinks(w)
	if err != nil {
		return "", err
	}
	if root == "" {
		return "", fmt.Errorf("no worktree root is configured (set --worktree-root or CCSBX_WORKTREE_ROOT)")
	}
	// root is a comma-separated list, with the syntax and meaning of ccsbx-root's allowed_roots: each
	// entry a clean absolute path, not "/", resolved through symlinks (to something that is not "/"
	// either); the workdir must be strictly inside one of them. A bad entry is an error even if
	// another would match, as in ccsbx-root (its whole config is refused), so the two never disagree.
	var rr string
	for _, r := range strings.Split(root, ",") {
		if err := CheckPath(r); err != nil {
			return "", fmt.Errorf("worktree root %q: %w", r, err)
		}
		if !filepath.IsAbs(r) || filepath.Clean(r) != r || r == "/" {
			return "", fmt.Errorf("worktree root %q must be a clean absolute path and not /", r)
		}
		res, err := filepath.EvalSymlinks(r)
		if err != nil {
			return "", fmt.Errorf("worktree root %q: %w", r, err)
		}
		if res == "/" {
			return "", fmt.Errorf("worktree root %q resolves to /: it would allow the whole host", r)
		}
		if rr == "" && strings.HasPrefix(rw, res+string(filepath.Separator)) {
			rr = res
		}
	}
	if rr == "" {
		return "", fmt.Errorf("workdir %s is not inside the worktree root(s) %s", rw, root)
	}
	if st, err := filepath.EvalSymlinks(stateDir); err == nil {
		if rw == st || strings.HasPrefix(rw, st+string(filepath.Separator)) || strings.HasPrefix(st, rw+string(filepath.Separator)) {
			return "", fmt.Errorf("workdir %s overlaps the daemon state dir", rw)
		}
	}
	for _, r := range reserved {
		if rw == r || strings.HasPrefix(rw, r+"/") || strings.HasPrefix(r, rw+"/") {
			return "", fmt.Errorf("workdir %s overlaps %s, which the fence mounts itself", rw, r)
		}
	}
	if h, err := os.UserHomeDir(); err == nil {
		if hh, err := filepath.EvalSymlinks(h); err == nil && rr == hh {
			rel := strings.TrimPrefix(rw, rr+string(filepath.Separator))
			if strings.HasPrefix(rel, ".") {
				return "", fmt.Errorf("workdir %s is in a hidden directory of the home (%s)", rw, strings.SplitN(rel, "/", 2)[0])
			}
		}
	}
	return rw, nil
}
