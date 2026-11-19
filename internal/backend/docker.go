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
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

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
}

// Names derived from the sid and gen, so cleanup targets exactly what this run created.
func (s Spec) Container() string { return fmt.Sprintf("ccsbx-%s-g%d", s.Sid, s.Gen) }
func (s Spec) Network() string   { return fmt.Sprintf("ccsbx-net-%s-g%d", s.Sid, s.Gen) }

// GenDir is <state>/s/<sid>/g<gen>.
func (s Spec) GenDir() string {
	return filepath.Join(s.StateDir, "s", s.Sid, "g"+strconv.Itoa(s.Gen))
}

// HomeDir is the per-node home, shared by every generation of the same label.
func (s Spec) HomeDir() string {
	h := sha256.Sum256([]byte(s.Label))
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
		"--mount", bind(s.Workdir, s.Workdir, false),
	}
	if s.GitDir != "" {
		a = append(a, "--mount", bind(s.GitDir, s.GitDir, true))
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
func (s Spec) Prepare() error {
	if !s.NotEvid {
		// helper mode: leaves must be 10001:10001 0700, made by ccsbx-root (gate M). Direct mode's
		// 0777 leaves are not acceptable there, so refuse until that exists.
		return fmt.Errorf("helper mode needs ccsbx-root to create 10001-owned 0700 generation dirs (not built); the 0777 leaves are direct-mode only")
	}
	if _, err := CheckWorkdir(s.Workdir, s.WorktreeRoot, s.StateDir); err != nil {
		return err
	}
	if s.GitDir != "" {
		if err := CheckPath(s.GitDir); err != nil {
			return err
		}
	}
	g := s.GenDir()
	if err := os.MkdirAll(filepath.Dir(g), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(g, 0o700); err != nil { // fails if it exists: one outbox per generation
		return fmt.Errorf("generation dir %s: %w", g, err)
	}
	if err := s.prepareQuarantine(); err != nil {
		return err
	}
	for _, d := range []string{"out", "in", "managed"} {
		p := filepath.Join(g, d)
		if err := os.Mkdir(p, 0o700); err != nil {
			return err
		}
		// 0777 because uid 10001 (fence) must write out/ and read in/ and managed/; the 0700
		// parent keeps other host users out. A mount cannot change this in direct mode.
		if err := os.Chmod(p, 0o777); err != nil {
			return err
		}
	}
	if err := os.MkdirAll(s.HomeDir(), 0o700); err != nil {
		return err
	}
	return os.Chmod(s.HomeDir(), 0o777)
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
func ResolveGitDir(workdir string) (string, error) {
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
		return p, nil
	}
	b, err := os.ReadFile(p)
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
	back, err := os.ReadFile(filepath.Join(t, "gitdir"))
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
	return t, nil
}

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
	rr, err := filepath.EvalSymlinks(root)
	if err != nil {
		return "", fmt.Errorf("worktree root: %w", err)
	}
	if rr == "/" {
		return "", fmt.Errorf("worktree root / would allow the whole host")
	}
	if !strings.HasPrefix(rw, rr+string(filepath.Separator)) {
		return "", fmt.Errorf("workdir %s is not inside the worktree root %s", rw, rr)
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
