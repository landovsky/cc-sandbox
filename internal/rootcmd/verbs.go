// The verbs of ccsbx-root: run, stop, gc, attach, and the two that are not wired yet. They exist as
// separate fixed verbs, rather than a general "run docker" door, so that what root can be asked to
// do is a list a reviewer can read in one sitting. Each takes
// at most one session id and a flag or two; a flag nobody listed is a usage error (exit 64), a
// policy refusal is exit 2.
package rootcmd

import (
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"

	"github.com/landovsky/cc-sandbox/internal/backend"
	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/pins"
	"github.com/landovsky/cc-sandbox/internal/seccomp"
	"github.com/landovsky/cc-sandbox/internal/settings"
	"github.com/landovsky/cc-sandbox/internal/start"
	"github.com/landovsky/cc-sandbox/profiles"
)

func (e *env) runCmd() *cli.Command {
	return &cli.Command{
		Name:    "run",
		Summary: "run one generation of a session in the fence, in the foreground (helper mode: 10001-owned outbox)",
		Recipes: `  sudo -n /usr/local/sbin/ccsbx-root run s0k1dry66qg          # what the pane of 'ccsbx start' runs
  sudo -n /usr/local/sbin/ccsbx-root run --tty s0k1dry66qg    # with a terminal
  Takes only the sid. The image, workdir, gitdir, caps and managed settings come from the
  daemon's record, re-checked here: the image must be a pinned digest (pins.toml, or a candidate
  digest for an e2e-* profile), the workdir strictly inside an allowed root with no symlink in
  its path and owned by the session's owner. Refusals exit 2 and say why.`,
		Setup: func(f *cli.Flags) { f.Bool("tty", false, "give the container a terminal (the caller's stdin is one)") },
		Run: func(f *cli.Flags, args []string, stdout, stderr io.Writer) int {
			sid, ok := oneSid("run", args, stderr)
			if !ok {
				return cli.ExitUsage
			}
			code, err := e.run(sid, f.Lookup("tty").Value.String() == "true", stdout, stderr)
			if err != nil {
				return fail(stderr, "run", err)
			}
			return code
		},
	}
}

// Harness arguments root will pass to claude. The record is less trusted than root and an
// argument can switch the harness's own safety off, so the list is short and parsed per flag:
// a value never starts with a dash (it cannot be read as another flag), and every value is passed
// as `--flag=value` so it cannot be split. Any other token, wherever it appears, is refused.
var (
	modelRe  = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)
	resumeRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9-]{0,63}$`)
)

func harnessArgs(in []string) ([]string, error) {
	var out []string
	bad := func(a string) ([]string, error) {
		return nil, refuse("harness argument %q is not allowed (allowed: --continue, --model NAME, --resume ID)", a)
	}
	for i := 0; i < len(in); i++ {
		a := in[i]
		name, val, hasVal := strings.Cut(a, "=")
		var re *regexp.Regexp
		switch name {
		case "--continue":
			if hasVal {
				return bad(a)
			}
			out = append(out, "--continue")
			continue
		case "--model":
			re = modelRe
		case "--resume":
			re = resumeRe
		default:
			return bad(a)
		}
		if !hasVal {
			if i+1 >= len(in) {
				return bad(a)
			}
			i++
			val = in[i]
		}
		if !re.MatchString(val) {
			return bad(a + " " + val)
		}
		out = append(out, name+"="+val)
	}
	return out, nil
}

func (e *env) run(sid string, tty bool, stdout, stderr io.Writer) (int, error) {
	w, err := e.prelude("run")
	if err != nil {
		return 0, err
	}
	// The staging mounts and docker's view of them agree only in the host's mount namespace. Inside
	// a sandboxed unit (a private /home, say) the workdir would be missing or empty, and the fence
	// would silently start on nothing: refuse instead.
	if err := e.hostNS(); err != nil {
		return 0, err
	}
	rec, err := e.session(w, sid)
	if err != nil {
		return 0, err
	}
	if rec.Ended != nil {
		return 0, refuse("session %s has ended", sid)
	}
	p, err := profiles.Load(rec.Spec.Profile)
	if err != nil {
		return 0, refuse("%v", err)
	}
	if m, b := p.Allows(rec.Spec.Machine, rec.Spec.Billing); !m || !b {
		return 0, refuse("profile %s is not approved for machine %s with billing %s", p.Name, rec.Spec.Machine, rec.Spec.Billing)
	}
	// the image: a pinned digest, or a candidate digest of ours for an e2e-* profile. Never a tag,
	// never the dev channel (that is direct mode only).
	if err := trustedPins(e.cfg.PinsFile, e.cfg.TrustedUID); err != nil {
		return 0, refuse("pins: %v", err)
	}
	pl, err := pins.Load(e.cfg.PinsFile)
	if err != nil {
		return 0, refuse("pins: %v", err)
	}
	image := rec.Reply.Pin.Image
	if _, err := pins.Check(pl, nil, image, p, false); err != nil {
		return 0, refuse("%v", err)
	}
	home, err := e.d.HomeOf(rec.Uid)
	if err != nil {
		return 0, refuse("no home directory for uid %d: %v", rec.Uid, err)
	}
	wd, root, err := e.cfg.workdir(rec.Spec.Workdir.Path, rec.Uid, home)
	if err != nil {
		return 0, err
	}
	// From here on the directories are held open: what is checked is what is mounted.
	wpin, err := pin(e.d.OpenDir, wd, rec.Uid)
	if err != nil {
		return 0, err
	}
	defer wpin.close()
	// The gitdir is not taken from the record: root derives it from the worktree's own .git, through
	// the pinned descriptor, and the record has to agree (see gitdir.go).
	gd, gpin, err := e.deriveGitDir(rec, wd, wpin, home)
	if err != nil {
		return 0, err
	}
	defer gpin.close()
	extra, err := harnessArgs(rec.Spec.HarnessArgs)
	if err != nil {
		return 0, err
	}
	docker, err := e.cfg.binary(e.cfg.Docker)
	if err != nil {
		return 0, err
	}
	hasLayer, err := backend.ImageHasLayer(docker, image) // the image is a pinned digest here; its label is part of what the digest names
	if err != nil {
		return 0, err
	}
	if !hasLayer { // a production pin without the label runs with no reporting: say so in root's log and to the caller, never silently
		e.logf("run %s: image %s carries no client layer (no %s label): no hooks, statusLine or ccf MCP server in this session", sid, image, backend.LayerLabel)
		fmt.Fprintf(stderr, "ccsbx-root run: image %s carries no client layer: this session has no hooks, statusLine or ccf MCP server\n", image)
	}
	res, err := settings.Compile(p, settings.Params{BaseURL: e.cfg.BaseURL, FetchURL: e.cfg.FetchURL, NoCCF: !hasLayer})
	if err != nil {
		return 0, err
	}
	if err := e.ensureRunDir(); err != nil {
		return 0, err
	}
	// The session's lock, held for as long as the fence runs: a commit (which holds it while the host
	// reads the worktree) and a fence exclude each other, and neither waits.
	lf, err := e.lockFile(rec.Sid, w.serviceGid)
	if err != nil {
		return 0, err
	}
	defer lf.Close()
	if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return 0, refuse("session %s is busy: a commit is in progress or a generation is running", rec.Sid)
	}
	seccompFile, err := e.seccomp()
	if err != nil {
		return 0, err
	}
	spec := backend.Spec{
		Sid: rec.Sid, Label: rec.Label, Gen: rec.Gen, Image: image, StateDir: e.cfg.RunDir,
		Workdir: wd, GitDir: gd, Uid: rec.Uid, WorktreeRoot: root,
		MemoryMB: p.MemoryMB, Pids: p.Pids, Cmd: append([]string{"claude"}, extra...), TTY: tty,
		NotEvid: false, Chown: e.d.Chown, Class: backend.ClassOf(p.E2E), Seccomp: seccompFile, OutGid: w.serviceGid, ServiceUID: w.service,
	}
	// docker mounts these root-owned staging paths, not the owner's paths
	spec.MountWorkdir = filepath.Join(spec.GenDir(), "wt")
	if gd != "" {
		spec.MountGitDir = filepath.Join(spec.GenDir(), "gd")
	}
	stage := func(s backend.Spec) (func(), error) { return e.stage(s, wpin, gpin) }
	files := rec.FenceFiles(e.cfg.Machine, res.Settings, res.MCP)
	code, err := e.d.NewBackend(docker, e.d.Mounter).Run(spec, files, stage, e.d.Stdin, stdout, stderr)
	e.revokeRelay(rec.Sid, rec.Gen) // the generation is over: its relay credential goes with it
	return code, err
}

// stage makes the two mountpoints under the generation directory and binds the pinned
// directories onto them: nosuid, nodev, the gitdir read-only. The gitdir's ownership and mode
// walk then runs on the staged mount, which is the inode that will be mounted into the fence.
func (e *env) stage(s backend.Spec, w, g *pinned) (func(), error) {
	// before the new generation sees the worktree: whatever the last one left with a setid bit
	if _, err := e.d.Strip(w.f, s.Uid, e.logf); err != nil {
		return nil, fmt.Errorf("clearing setid bits in the workdir: %w", err)
	}
	var undo []func()
	rollback := func() {
		for i := len(undo) - 1; i >= 0; i-- {
			undo[i]()
		}
	}
	bind := func(p *pinned, target string, ro bool) error {
		if err := os.Mkdir(target, 0o755); err != nil {
			return err
		}
		if err := e.d.Mounter.Bind(p.f, target, ro); err != nil {
			os.Remove(target)
			return err
		}
		undo = append(undo, func() { e.d.Mounter.Unmount(target); os.Remove(target) })
		return nil
	}
	if err := bind(w, s.MountWorkdir, false); err != nil {
		return nil, err
	}
	if g != nil {
		if err := bind(g, s.MountGitDir, true); err != nil {
			rollback()
			return nil, err
		}
		if err := backend.CheckGitDirOwned(s.MountGitDir, s.Uid); err != nil {
			rollback()
			return nil, refuse("%v", err)
		}
	}
	return rollback, nil
}

// ensureRunDir makes root's own tree: a real directory, root-owned, nobody else's to write.
func (e *env) ensureRunDir() error {
	if err := os.MkdirAll(e.cfg.RunDir, 0o755); err != nil {
		return err
	}
	return trustedFile(e.cfg.RunDir, e.cfg.TrustedUID, false)
}

func trustedPins(path string, uid int) error {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil // no pins: nothing production can run, candidates only
	}
	return trustedFile(path, uid, true)
}

func (e *env) stopCmd() *cli.Command {
	return &cli.Command{
		Name:    "stop",
		Summary: "remove the container and network of one session's current generation",
		Recipes: `  sudo -n /usr/local/sbin/ccsbx-root stop s0k1dry66qg
  Takes only the sid. Removes exactly ccsbx-<sid>-g<gen> and its network, nothing else.`,
		Run: func(_ *cli.Flags, args []string, stdout, stderr io.Writer) int {
			sid, ok := oneSid("stop", args, stderr)
			if !ok {
				return cli.ExitUsage
			}
			w, err := e.prelude("stop")
			if err != nil {
				return fail(stderr, "stop", err)
			}
			// a pod session is stopped by deleting its pod (by name and UID): the same as pod-stop, which
			// the owner may run; docker's mount-namespace rule does not apply to it
			if prec, perr := e.sessionAny(w, sid); perr == nil && prec.Backend == "k8s" {
				if err := e.podStop(sid, 0, stdout); err != nil {
					return fail(stderr, "stop", err)
				}
				return cli.ExitOK
			}
			if err := e.hostNS(); err != nil {
				return fail(stderr, "stop", err)
			}
			rec, err := e.session(w, sid)
			if err != nil {
				return fail(stderr, "stop", err)
			}
			docker, err := e.cfg.binary(e.cfg.Docker)
			if err != nil {
				return fail(stderr, "stop", err)
			}
			if err := e.d.NewBackend(docker, e.d.Mounter).Stop(backend.Spec{Sid: rec.Sid, Gen: rec.Gen, StateDir: e.cfg.RunDir}); err != nil {
				return fail(stderr, "stop", err)
			}
			fmt.Fprintf(stdout, "stopped %s g%d\n", rec.Sid, rec.Gen)
			e.revokeRelay(rec.Sid, rec.Gen)
			if err := e.stripSession(rec); err != nil {
				fmt.Fprintf(stderr, "ccsbx-root stop: the container is gone, but the workdir was not cleaned of setid bits: %v\n", err)
				return cli.ExitFail
			}
			return cli.ExitOK
		},
	}
}

func (e *env) statusCmd() *cli.Command {
	return &cli.Command{
		Name:    "status",
		Summary: "print running or stopped for one session's current and previous generation (changes nothing but making the session's lock file)",
		Recipes: `  sudo -n /usr/local/sbin/ccsbx-root status -- s0k1dry66qg
  Takes only the sid. Prints "running" if the container of the record's generation, or of the one
  before it, is running (or its session lock is held), else "stopped"; exit 1 (and no answer) if
  docker cannot be asked. It stops, starts and removes nothing; the one thing it writes is the
  session's empty lock file under the run dir, so the daemon (which may only open it read-only) can
  take the lock. The daemon uses it before it lets the host read a worktree (the stopped proof).`,
		Run: func(_ *cli.Flags, args []string, stdout, stderr io.Writer) int {
			sid, ok := oneSid("status", args, stderr)
			if !ok {
				return cli.ExitUsage
			}
			w, err := e.prelude("status")
			if err == nil {
				err = e.hostNS()
			}
			if err != nil {
				return fail(stderr, "status", err)
			}
			rec, err := e.session(w, sid)
			if err != nil {
				return fail(stderr, "status", err)
			}
			docker, err := e.cfg.binary(e.cfg.Docker)
			if err != nil {
				return fail(stderr, "status", err)
			}
			// the session's lock file is made here if it is not there yet (root's, in root's
			// directory) so the daemon can take the lock on it without creating anything; a held
			// lock (a fence, or a commit) is "running"
			if err := e.ensureRunDir(); err != nil {
				return fail(stderr, "status", err)
			}
			lf, err := e.lockFile(rec.Sid, w.serviceGid)
			if err != nil {
				return fail(stderr, "status", err)
			}
			defer lf.Close()
			if err := syscall.Flock(int(lf.Fd()), syscall.LOCK_SH|syscall.LOCK_NB); err != nil {
				fmt.Fprintln(stdout, "running")
				return cli.ExitOK
			}
			syscall.Flock(int(lf.Fd()), syscall.LOCK_UN)
			b := e.d.NewBackend(docker, e.d.Mounter)
			for g := rec.Gen; g >= 1 && g >= rec.Gen-1; g-- {
				running, err := b.Running(backend.Spec{Sid: rec.Sid, Gen: g})
				if err != nil {
					return fail(stderr, "status", err)
				}
				if running {
					fmt.Fprintln(stdout, "running")
					return cli.ExitOK
				}
			}
			fmt.Fprintln(stdout, "stopped")
			return cli.ExitOK
		},
	}
}

func (e *env) gcCmd() *cli.Command {
	return &cli.Command{
		Name:    "gc",
		Summary: "remove containers and networks of generations whose run died",
		Recipes: `  sudo -n /usr/local/sbin/ccsbx-root gc --dry-run
  sudo -n /usr/local/sbin/ccsbx-root gc
  Only resources labelled ccsbx.sid whose sid has a daemon record, and only when the run that owns
  the generation is gone. Takes no sid.`,
		Setup: func(f *cli.Flags) { f.Bool("dry-run", false, "list orphans, remove nothing") },
		Run: func(f *cli.Flags, args []string, stdout, stderr io.Writer) int {
			if len(args) != 0 {
				fmt.Fprintln(stderr, "usage: ccsbx-root gc [--dry-run]   (no arguments)")
				return cli.ExitUsage
			}
			if _, err := e.prelude("gc"); err != nil {
				return fail(stderr, "gc", err)
			}
			if err := e.hostNS(); err != nil {
				return fail(stderr, "gc", err)
			}
			docker, err := e.cfg.binary(e.cfg.Docker)
			if err != nil {
				return fail(stderr, "gc", err)
			}
			b := e.d.NewBackend(docker, e.d.Mounter)
			orphans, err := b.Orphans(e.cfg.RunDir, e.cfg.StateDir+"/sessions")
			if err != nil {
				return fail(stderr, "gc", err)
			}
			for _, o := range orphans {
				fmt.Fprintf(stdout, "orphan %s %s (sid %s g%d)\n", o.Kind, o.Name, o.Sid, o.Gen)
			}
			if f.Lookup("dry-run").Value.String() == "true" || len(orphans) == 0 {
				fmt.Fprintf(stdout, "%d orphan(s)\n", len(orphans))
				return cli.ExitOK
			}
			if err := b.Reap(e.cfg.RunDir, orphans); err != nil {
				return fail(stderr, "gc", err)
			}
			fmt.Fprintf(stdout, "removed %d orphan(s)\n", len(orphans))
			revoked := map[string]bool{}
			for _, o := range orphans {
				if k := fmt.Sprintf("%s/%d", o.Sid, o.Gen); !revoked[k] && o.Gen >= 1 {
					revoked[k] = true
					e.revokeRelay(o.Sid, o.Gen)
				}
			}
			failed := false
			seen := map[string]bool{}
			for _, o := range orphans {
				if seen[o.Sid] {
					continue
				}
				seen[o.Sid] = true
				w, _ := e.prelude("gc")
				rec, err := loadRecord(e.cfg.StateDir, o.Sid, w.service, e.cfg.TrustedUID)
				if err == nil && rec.Backend == "k8s" {
					continue // a pod session has no host worktree: nothing here is root's to clean
				}
				if err == nil {
					err = e.stripSession(rec)
				}
				if err != nil {
					fmt.Fprintf(stderr, "ccsbx-root gc: session %s: workdir not cleaned of setid bits: %v\n", o.Sid, err)
					failed = true
				}
			}
			if failed {
				return cli.ExitFail
			}
			return cli.ExitOK
		},
	}
}

// notWired is a verb whose arguments are checked and whose act is not built yet: it refuses with
// exit 2 so a caller never mistakes it for success.
func (e *env) notWired(verb, what string) *cli.Command {
	return &cli.Command{
		Name: verb, Summary: what + " (not wired yet: always refuses)",
		Recipes: fmt.Sprintf("  sudo -n /usr/local/sbin/ccsbx-root %s <sid>\n  Takes only a sid, checks the caller and the record like every verb, then refuses: the act itself waits for the signer.", verb),
		Run: func(_ *cli.Flags, args []string, _, stderr io.Writer) int {
			sid, ok := oneSid(verb, args, stderr)
			if !ok {
				return cli.ExitUsage
			}
			w, err := e.prelude(verb)
			if err == nil {
				_, err = e.session(w, sid)
			}
			if err != nil {
				return fail(stderr, verb, err)
			}
			return fail(stderr, verb, refuse("%s is not wired yet (it needs the signer, U0b)", verb))
		},
	}
}

// dockerBackend is the production Backend: backend.Runner with the validated docker binary.
type dockerBackend struct {
	docker  string
	mounter Mounter
}

func (d *dockerBackend) runner(sessions string) *backend.Runner {
	return &backend.Runner{Docker: d.docker, SessionsDir: sessions, Unstage: d.unstage}
}

// unstage unmounts the two staging mountpoints of a generation (a run that died leaves them).
func (d *dockerBackend) unstage(s backend.Spec) {
	if d.mounter == nil {
		return
	}
	for _, n := range []string{"wt", "gd"} {
		t := filepath.Join(s.GenDir(), n)
		d.mounter.Unmount(t)
		os.Remove(t)
	}
}

func (d *dockerBackend) Run(s backend.Spec, files map[string][]byte, stage Stager, in io.Reader, out, errw io.Writer) (int, error) {
	r := d.runner("")
	r.Stdin, r.Stdout, r.Stderr, r.Files, r.Stage = in, out, errw, files, stage
	return r.Run(s)
}
func (d *dockerBackend) Stop(s backend.Spec) error { return d.runner("").Stop(s) }
func (d *dockerBackend) Running(s backend.Spec) (bool, error) {
	return d.runner("").Running(s)
}
func (d *dockerBackend) Orphans(runDir, sessions string) ([]backend.Orphan, error) {
	return d.runner(sessions).Orphans(runDir)
}
func (d *dockerBackend) Reap(runDir string, o []backend.Orphan) error {
	return d.runner("").Reap(runDir, o)
}

// logf is the audit line for each file whose setid bits were cleared.
func (e *env) logf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, "ccsbx-root: "+format+"\n", a...)
}

// seccomp returns the verified seccomp profile the fence runs with. Without one that is root-owned
// and carries the rule that refuses setuid/setgid chmod, the fence does not start.
func (e *env) seccomp() (string, error) {
	p := e.cfg.SeccompFile
	if err := trustedFile(p, e.cfg.TrustedUID, true); err != nil {
		return "", refuse("seccomp profile %s: %v (build it with: ccsbx seccomp-profile --base <docker default.json>)", p, err)
	}
	b, err := os.ReadFile(p)
	if err != nil {
		return "", refuse("seccomp profile: %v", err)
	}
	if err := seccomp.Verify(b); err != nil {
		return "", refuse("%v", err)
	}
	return p, nil
}

// stripSession clears setid bits in a session's workdir after its fence is gone (stop, gc). The
// workdir is re-validated like any other: a path that no longer passes is reported, not walked.
func (e *env) stripSession(rec *start.Record) error {
	home, err := e.d.HomeOf(rec.Uid)
	if err != nil {
		return err
	}
	wd, _, err := e.cfg.workdir(rec.Spec.Workdir.Path, rec.Uid, home)
	if err != nil {
		return err
	}
	pw, err := pin(e.d.OpenDir, wd, rec.Uid)
	if err != nil {
		return err
	}
	defer pw.close()
	_, err = e.d.Strip(pw.f, rec.Uid, e.logf)
	return err
}

// hostNS refuses unless this process is in the host's mount namespace: the staging mounts, the
// unmounts of them and the walk of the workdir all assume the host's view of the filesystem.
func (e *env) hostNS() error {
	if same, err := e.d.SameMountNS(); err != nil {
		return refuse("cannot compare mount namespaces: %v", err)
	} else if !same {
		return refuse("this process is not in the host's mount namespace (a sandboxed unit?): mounts and the workdir would not be the host's. Run ccsbx-root from the host's namespace")
	}
	return nil
}

// lockFile opens (creating it if needed) <RunDir>/locks/<sid>.lock: root's file in root's directory,
// never a link, never anyone else's. The daemon only ever opens it read-only, which is enough for it
// to take the lock.
//
// Only root and the service group may see them: a read-only descriptor is enough to hold a flock, so
// a lock file any local user could open is a lock any local user could hold forever (blocking every
// run, commit and resume of that session), and a listable directory would give away every sid. The
// directory is root:<service group> 0710 (the group can open a name it knows, not list), the files
// root:<service group> 0640.
func (e *env) lockFile(sid string, gid int) (*os.File, error) {
	if gid <= 0 {
		return nil, refuse("lock files need the service user's gid")
	}
	dir := filepath.Join(e.cfg.RunDir, "locks")
	if err := os.MkdirAll(dir, 0o710); err != nil {
		return nil, err
	}
	if err := os.Chmod(dir, 0o710); err != nil {
		return nil, err
	}
	if err := e.chown(dir, 0, gid); err != nil {
		return nil, err
	}
	if err := trustedFile(dir, e.cfg.TrustedUID, false); err != nil {
		return nil, refuse("lock directory: %v", err)
	}
	f, err := os.OpenFile(filepath.Join(dir, sid+".lock"), os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW|syscall.O_CLOEXEC, 0o640)
	if err != nil {
		return nil, refuse("lock file: %v", err)
	}
	st, err := f.Stat()
	sys, ok := st.Sys().(*syscall.Stat_t)
	if err != nil || !st.Mode().IsRegular() || !ok || int(sys.Uid) != e.cfg.TrustedUID {
		f.Close()
		return nil, refuse("lock file for %s is not a regular file owned by root", sid)
	}
	if err := f.Chmod(0o640); err != nil {
		f.Close()
		return nil, err
	}
	if err := e.chown(f.Name(), 0, gid); err != nil {
		f.Close()
		return nil, err
	}
	return f, nil
}

// chown is the injectable chown (os.Lchown in production): it never follows a link.
func (e *env) chown(path string, uid, gid int) error {
	if e.d.Chown != nil {
		return e.d.Chown(path, uid, gid)
	}
	return os.Lchown(path, uid, gid)
}
