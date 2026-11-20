// Package rootcmd is ccsbx-root: the one program tomas and the daemon may run as root through
// sudo (U0). It exists so that the fence can be started, stopped, cleaned up and attached to
// without any caller holding the docker group, and so that every one of those acts is checked by
// root-owned policy, not by the caller.
//
// The model: the caller is hostile. Verbs are fixed (run stop renew gc attach exec-token). A verb
// takes a session id, never a path, an image or a command: those come from the daemon's session
// record, which root re-validates (record.go, paths.go), and from root-owned config and pins.
// The environment is rebuilt; every binary is absolute and root-owned. The caller's identity is
// sudo's SUDO_UID, and a caller may act only on its own sessions (the service uid on all).
package rootcmd

import (
	"fmt"
	"io"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"

	"github.com/landovsky/cc-sandbox/internal/backend"
	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/start"
)

// Backend is what run, stop and gc need from docker. Production wraps backend.Runner; tests pass a
// fake, so the suite needs neither root nor docker.
type Backend interface {
	// Run runs one generation. stage makes the mounts docker will use (see run); it runs after the
	// directories exist and before docker is asked for anything.
	Run(s backend.Spec, files map[string][]byte, stage Stager, stdin io.Reader, stdout, stderr io.Writer) (int, error)
	Stop(s backend.Spec) error
	// Running says whether the generation's container (exact name) is running; read-only.
	Running(s backend.Spec) (bool, error)
	Orphans(runDir, sessionsDir string) ([]backend.Orphan, error)
	Reap(runDir string, o []backend.Orphan) error
}

// Stager is backend.Runner.Stage: it returns the function that undoes the staging.
type Stager = func(s backend.Spec) (undo func(), err error)

// Mounter binds an open directory onto a root-owned mountpoint (nosuid, nodev, and read-only when
// asked) and takes it away again. The real one is in pin_linux.go; tests use a fake that records
// what it was handed, and mount_root_test.go is the root-only test of the real one.
type Mounter interface {
	Bind(src *os.File, target string, readonly bool) error
	Unmount(target string) error
}

// Deps is everything outside the policy: the OS facts and the programs. Production fills it with
// the real ones (Real); tests fill it with fakes.
type Deps struct {
	Euid       int
	Getenv     func(string) string // SUDO_UID and TERM only
	LookupUser func(name string) (uid, gid int, err error)
	// NewBackend builds the backend for a validated docker binary.
	NewBackend func(docker string, m Mounter) Backend
	// DropPrivs becomes uid:gid for good (and proves it cannot get root back); Exec replaces the process.
	DropPrivs func(uid, gid int) error
	Exec      func(path string, argv, env []string) error
	Chown     func(path string, uid, gid int) error
	Stdin     io.Reader
	// OpenDir opens a directory without following any symlink; Mounter stages it; HomeOf is a
	// user's passwd home (for the hidden-directory rule, independent of HOME).
	OpenDir func(path string) (*os.File, error)
	// SameMountNS reports whether this process is in the host's mount namespace (pid 1's). The
	// staging mounts and docker's own view of them only agree there.
	SameMountNS func() (bool, error)
	// Strip clears setuid/setgid bits a fence left in a worktree (backend.StripSetid).
	Strip   func(root *os.File, owner int, logf func(string, ...any)) (int, error)
	Mounter Mounter
	HomeOf  func(uid int) (string, error)
}

// App builds the command table for a loaded config.
func App(cfg *Config, d *Deps, getenv func(string) string) *cli.App {
	e := &env{cfg: cfg, d: d}
	return &cli.App{
		Name: "ccsbx-root", Summary: "the fixed-verb root door for the fence (U0): run stop renew gc attach exec-token",
		Version: "dev", Getenv: getenv, EnvPrefix: "CCSBX_ROOT_",
		Cmds: []*cli.Command{
			e.runCmd(), e.stopCmd(), e.gcCmd(), e.attachCmd(), e.statusCmd(),
			e.notWired("renew", "renew the machine's credentials (needs the signer, U0b)"),
			e.notWired("exec-token", "mint an exec token for one session (needs the signer, U0b)"),
		},
	}
}

type env struct {
	cfg *Config
	d   *Deps
}

// who is the validated caller of a verb.
type who struct {
	uid        int // the caller (SUDO_UID, or 0 for a root login)
	service    int // the service user's uid
	serviceGid int // and gid (the outbox group)
}

func (w who) mayAct(rec *start.Record) bool {
	return w.uid == 0 || w.uid == w.service || w.uid == rec.Uid
}

// prelude checks everything every verb needs before it looks at an argument: the verb is switched
// on, the process is root, the caller is known, the service user exists.
func (e *env) prelude(verb string) (who, error) {
	if !e.cfg.Enabled(verb) {
		return who{}, refuse("verb %s is not enabled in the root config", verb)
	}
	if e.d.Euid != 0 {
		return who{}, refuse("must run as root (sudo -n /usr/local/sbin/ccsbx-root %s ...)", verb)
	}
	w := who{}
	if s := e.d.Getenv("SUDO_UID"); s != "" {
		n, err := strconv.Atoi(s)
		if err != nil || n < 0 || len(s) > 10 {
			return who{}, refuse("SUDO_UID %q is not a uid", s)
		}
		w.uid = n
	}
	uid, gid, err := e.d.LookupUser(e.cfg.ServiceUser)
	if err != nil {
		return who{}, refuse("service user %s: %v", e.cfg.ServiceUser, err)
	}
	w.service, w.serviceGid = uid, gid
	return w, nil
}

// session loads, validates and authorises one session record for the caller.
func (e *env) session(w who, sid string) (*start.Record, error) {
	rec, err := loadRecord(e.cfg.StateDir, sid, w.service, e.cfg.TrustedUID)
	if err != nil {
		return nil, refuse("%v", err)
	}
	if !w.mayAct(rec) {
		return nil, refuse("uid %d may not act on session %s", w.uid, sid)
	}
	if rec.Uid < e.cfg.MinUID || rec.Uid == w.service {
		return nil, refuse("session %s belongs to uid %d, which is not a login user (min_uid %d, not the service user)", sid, rec.Uid, e.cfg.MinUID)
	}
	if rec.Spec.Machine != e.cfg.Machine {
		return nil, refuse("session %s is for machine %q, this is %q", sid, rec.Spec.Machine, e.cfg.Machine)
	}
	return rec, nil
}

// fail turns an error into an exit code and a message.
func fail(stderr io.Writer, verb string, err error) int {
	if r, ok := err.(*Refusal); ok {
		fmt.Fprintf(stderr, "ccsbx-root %s: refused: %s\n", verb, r.Msg)
		return cli.ExitRefused
	}
	fmt.Fprintf(stderr, "ccsbx-root %s: %v\n", verb, err)
	return cli.ExitFail
}

// oneSid checks the positional arguments are exactly one session id.
func oneSid(verb string, args []string, stderr io.Writer) (string, bool) {
	if len(args) != 1 {
		fmt.Fprintf(stderr, "usage: ccsbx-root %s <sid>   (nothing else is accepted)\n", verb)
		return "", false
	}
	if !start.ValidSid(args[0]) || strings.ContainsRune(args[0], 0) {
		fmt.Fprintf(stderr, "ccsbx-root %s: %q is not a session id\n", verb, args[0])
		return "", false
	}
	return args[0], true
}

// Real returns the production Deps. getenv answers only for the few variables main kept
// (SUDO_UID, TERM), because main clears the rest of the environment.
func Real(getenv func(string) string) *Deps {
	return &Deps{
		Euid: os.Geteuid(), Getenv: getenv, LookupUser: lookupUser,
		NewBackend: func(docker string, m Mounter) Backend { return &dockerBackend{docker: docker, mounter: m} },
		DropPrivs:  dropPrivs, Exec: syscall.Exec, Stdin: os.Stdin,
		OpenDir: OpenDir, Mounter: RealMounter(), HomeOf: homeOf, Strip: backend.StripSetid, SameMountNS: sameMountNS,
	}
}

func homeOf(uid int) (string, error) {
	u, err := user.LookupId(strconv.Itoa(uid))
	if err != nil {
		return "", err
	}
	return u.HomeDir, nil
}

func lookupUser(name string) (int, int, error) {
	u, err := user.Lookup(name)
	if err != nil {
		return 0, 0, err
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil || uid <= 0 {
		return 0, 0, fmt.Errorf("user %s has uid %q gid %q", name, u.Uid, u.Gid)
	}
	return uid, gid, nil
}

// sameMountNS compares this process's mount namespace with pid 1's.
func sameMountNS() (bool, error) { return sameNS("/proc/self/ns/mnt", "/proc/1/ns/mnt") }

func sameNS(a, b string) (bool, error) {
	la, err := os.Readlink(a)
	if err != nil {
		return false, err
	}
	lb, err := os.Readlink(b)
	if err != nil {
		return false, err
	}
	return la == lb, nil
}
