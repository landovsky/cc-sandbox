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
	Sid, Label string
	Gen        int
	Image      string
	StateDir   string // daemon state dir; per-generation dirs live under it
	Workdir    string // host worktree, mounted rw at the same absolute path (resume is keyed by cwd)
	GitDir     string // host-owned gitdir, mounted ro at its own path; "" if none
	MemoryMB   int
	Pids       int
	Cmd        []string // command in the fence; default ["claude"]
	TTY        bool
	NotEvid    bool // direct mode
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
	return []string{"network", "create", "--internal", "--label", "ccsbx.sid=" + s.Sid, "--label", "ccsbx.gen=" + strconv.Itoa(s.Gen), s.Network()}
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
		"-v", s.Workdir + ":" + s.Workdir,
	}
	if s.GitDir != "" {
		a = append(a, "-v", s.GitDir+":"+s.GitDir+":ro")
	}
	g := s.GenDir()
	a = append(a,
		"-v", filepath.Join(g, "out")+":/run/ccsbx/out",
		"-v", filepath.Join(g, "in")+":/run/ccsbx/in:ro",
		"-v", filepath.Join(g, "managed")+":/etc/claude-code:ro",
		"-v", s.HomeDir()+":/home/agent",
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
	if err := CheckWorkdir(s.Workdir); err != nil {
		return err
	}
	g := s.GenDir()
	if err := os.MkdirAll(filepath.Dir(g), 0o700); err != nil {
		return err
	}
	if err := os.Mkdir(g, 0o700); err != nil { // fails if it exists: one outbox per generation
		return fmt.Errorf("generation dir %s: %w", g, err)
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

// GitDirFor returns the gitdir to mount read-only for a worktree: the `.git` directory itself,
// or the target of a `.git` file ("gitdir: ..."). "" when there is no git metadata.
func GitDirFor(workdir string) string {
	p := filepath.Join(workdir, ".git")
	st, err := os.Lstat(p)
	if err != nil {
		return ""
	}
	if st.IsDir() {
		return p
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return ""
	}
	line := strings.TrimSpace(string(b))
	if rest, ok := strings.CutPrefix(line, "gitdir:"); ok {
		d := strings.TrimSpace(rest)
		if !filepath.IsAbs(d) {
			d = filepath.Join(workdir, d)
		}
		return filepath.Clean(d)
	}
	return ""
}

// reserved are paths the fence mounts or needs itself; a worktree on or above one would shadow
// it (or be shadowed by it) and the mounts would fail or, worse, silently mask each other.
var reserved = []string{"/tmp", "/run", "/etc", "/home/agent", "/proc", "/sys", "/dev"}

// CheckWorkdir refuses a worktree path that overlaps a fixed mount point of the fence.
func CheckWorkdir(w string) error {
	if !filepath.IsAbs(w) || filepath.Clean(w) != w {
		return fmt.Errorf("workdir %q must be a clean absolute path", w)
	}
	if w == "/" {
		return fmt.Errorf("workdir / would mount the whole host")
	}
	for _, r := range reserved {
		if w == r || strings.HasPrefix(w, r+"/") || strings.HasPrefix(r, w+"/") {
			return fmt.Errorf("workdir %s overlaps %s, which the fence mounts itself", w, r)
		}
	}
	return nil
}
