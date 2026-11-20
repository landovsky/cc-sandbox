// Tests for ccsbx-root. The program is the one NOPASSWD sudo target, so every refusal below is a
// block twin: it was written to go red when the check it names is removed (invariant 23). The
// whole suite runs as an ordinary user with no docker and no tmux: the backend is a fake that
// records what it was asked to do, tmux is a stub that records its argv, and "root" is a fake
// Euid, so what is tested is the policy and not the machine.
package rootcmd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/landovsky/cc-sandbox/internal/backend"
	"github.com/landovsky/cc-sandbox/internal/seccomp"
	"github.com/landovsky/cc-sandbox/internal/start"
)

const (
	goodImage = "ghcr.io/landovsky/ccsbx-fence@sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	candImage = "ghcr.io/landovsky/ccsbx-fence@sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	evilImage = "docker.io/evil/img@sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
	sid       = "s0abcdefgh"
	svcUID    = 4242
	svcGID    = 4243
)

type fakeBackend struct {
	ran     []backend.Spec
	files   map[string][]byte
	stopped []backend.Spec
	orphans []backend.Orphan
	reaped  []backend.Orphan
	code    int
	docker  string
	// beforeStage runs after the checks and before the staging: the moment the owner could swap a path.
	beforeStage func()
	staged      bool
}

// fakeMounter records every bind: which inode it was handed, where, and how.
type bindCall struct {
	ino      uint64
	target   string
	readonly bool
}

// stripCall records a setid strip: which directory, for which owner, and how many mounts had been
// made when it ran (it must be before the first).
type stripCall struct {
	ino   uint64
	owner int
	binds int
}

type fakeMounter struct {
	binds   []bindCall
	unbound []string
	err     error
	// after runs once a bind is recorded: the test's way to make the staged mountpoint look like
	// the bound directory (a real bind shows the source's mode and owner there).
	after func(target string)
}

func (m *fakeMounter) Bind(src *os.File, target string, ro bool) error {
	if m.err != nil {
		return m.err
	}
	st, err := src.Stat()
	if err != nil {
		return err
	}
	m.binds = append(m.binds, bindCall{st.Sys().(*syscall.Stat_t).Ino, target, ro})
	if m.after != nil {
		m.after(target)
	}
	return nil
}
func (m *fakeMounter) Unmount(t string) error { m.unbound = append(m.unbound, t); return nil }

func (f *fakeBackend) Run(s backend.Spec, files map[string][]byte, stage Stager, _ io.Reader, _, _ io.Writer) (int, error) {
	f.ran, f.files = append(f.ran, s), files
	os.MkdirAll(s.GenDir(), 0o755) // what backend.Prepare does
	if f.beforeStage != nil {
		f.beforeStage()
	}
	if stage != nil {
		undo, err := stage(s)
		if err != nil {
			return -1, err
		}
		f.staged = true
		defer undo()
	}
	return f.code, nil
}
func (f *fakeBackend) Stop(s backend.Spec) error { f.stopped = append(f.stopped, s); return nil }
func (f *fakeBackend) Orphans(string, string) ([]backend.Orphan, error) {
	return f.orphans, nil
}
func (f *fakeBackend) Reap(_ string, o []backend.Orphan) error { f.reaped = o; return nil }

type execCall struct {
	path      string
	argv, env []string
}

type fx struct {
	t                 *testing.T
	dir, roots, state string
	wd, run           string
	cfg               *Config
	d                 *Deps
	fb                *fakeBackend
	rec               *start.Record
	env               map[string]string
	mounter           *fakeMounter
	sameNS            bool
	nsErr             error
	strips            []stripCall
	stripErr          error
	execs             []execCall
	dropped           [][2]int
	dropErr           error
}

// vtmp is a directory under /var/tmp: /tmp is a fixed mount point of the fence, so a worktree
// there is (rightly) refused.
func vtmp(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/var/tmp", "ccsbx-root-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	d, _ = filepath.EvalSymlinks(d)
	return d
}

func mk(t *testing.T, p string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(p, mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

func put(t *testing.T, p, body string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(p, []byte(body), mode); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(p, mode); err != nil {
		t.Fatal(err)
	}
}

// newFx builds a world in which `ccsbx-root run <sid>` is allowed; each test then breaks one thing.
func newFx(t *testing.T) *fx {
	t.Helper()
	me := os.Getuid()
	f := &fx{t: t, dir: vtmp(t), env: map[string]string{"SUDO_UID": fmt.Sprint(me), "TERM": "xterm-256color"}, fb: &fakeBackend{code: 7}, mounter: &fakeMounter{}}
	f.roots, f.state, f.run = filepath.Join(f.dir, "roots"), filepath.Join(f.dir, "state"), filepath.Join(f.dir, "run")
	f.wd = filepath.Join(f.roots, "git", "wt")
	mk(t, f.wd, 0o755)
	mk(t, filepath.Join(f.state, "sessions"), 0o700)
	mk(t, filepath.Join(f.dir, "bin"), 0o755)
	mk(t, filepath.Join(f.dir, "etc"), 0o755)
	put(t, filepath.Join(f.dir, "etc", "seccomp.json"), goodProfile(t), 0o644)
	put(t, filepath.Join(f.dir, "bin", "docker"), "#!/bin/sh\n", 0o755)
	put(t, filepath.Join(f.dir, "bin", "tmux"), "#!/bin/sh\n", 0o755)
	put(t, filepath.Join(f.dir, "etc", "pins.toml"), "[[pin]]\nref = \""+goodImage+"\"\ncc = \"2.1.0\"\n", 0o644)
	f.sameNS = true
	f.writeConfig("")
	f.rec = &start.Record{Sid: sid, Gen: 2, Label: "cc-sandbox:x", Uid: me, Spec: start.LaunchSpec{
		Machine: "hp-ubuntu", Profile: "dev-default", Billing: "stub", Owner: "tomas", Workdir: start.Workdir{Path: f.wd},
		MsgScope: json.RawMessage(`{}`), HarnessArgs: []string{"--model", "opus"}},
		Reply: start.StartReply{Sid: sid, Gen: 2, ID: "tomas/hp-ubuntu/" + sid + "/g2", Pin: start.Pin{Image: goodImage, Channel: "production"}}}
	f.writeRecord()
	return f
}

func (f *fx) writeConfig(extra string) {
	f.t.Helper()
	body := fmt.Sprintf("allowed_roots = [%q]\nstate_dir = %q\nrun_dir = %q\ndocker = %q\ntmux = %q\ntmux_socket = \"/run/ccsbx/tmux.sock\"\npins_file = %q\nseccomp_profile = %q\nmachine = \"hp-ubuntu\"\n%s%s",
		f.roots, f.state, f.run, filepath.Join(f.dir, "bin", "docker"), filepath.Join(f.dir, "bin", "tmux"), filepath.Join(f.dir, "etc", "pins.toml"), filepath.Join(f.dir, "etc", "seccomp.json"), minUID(extra), extra)
	p := filepath.Join(f.dir, "etc", "ccsbx-root.toml")
	put(f.t, p, body, 0o644)
	cfg, err := LoadConfig(p, os.Getuid())
	if err != nil {
		f.t.Fatal(err)
	}
	f.cfg = cfg
	f.d = &Deps{
		Euid: 0, Getenv: func(k string) string { return f.env[k] },
		LookupUser: func(n string) (int, int, error) {
			if n != "ccsbx" {
				return 0, 0, fmt.Errorf("no such user")
			}
			return svcUID, svcGID, nil
		},
		OpenDir: OpenDir, Mounter: f.mounter, SameMountNS: func() (bool, error) { return f.sameNS, f.nsErr },
		Strip: func(root *os.File, owner int, _ func(string, ...any)) (int, error) {
			st, _ := root.Stat()
			f.strips = append(f.strips, stripCall{st.Sys().(*syscall.Stat_t).Ino, owner, len(f.mounter.binds)})
			return 0, f.stripErr
		},
		HomeOf:     func(uid int) (string, error) { return filepath.Join(f.dir, "home"), nil },
		NewBackend: func(docker string, m Mounter) Backend { f.fb.docker = docker; return f.fb },
		DropPrivs: func(uid, gid int) error {
			if f.dropErr != nil {
				return f.dropErr
			}
			f.dropped = append(f.dropped, [2]int{uid, gid})
			return nil
		},
		Exec: func(path string, argv, env []string) error {
			f.execs = append(f.execs, execCall{path, argv, env})
			return nil
		},
		Chown: func(string, int, int) error { return nil },
	}
}

func (f *fx) writeRecord() {
	f.t.Helper()
	b, err := json.Marshal(f.rec)
	if err != nil {
		f.t.Fatal(err)
	}
	put(f.t, filepath.Join(f.state, "sessions", f.rec.Sid+".json"), string(b), 0o600)
}

// call runs ccsbx-root with args and returns the exit code and the output.
func (f *fx) call(args ...string) (int, string) {
	f.t.Helper()
	var out, errb bytes.Buffer
	code := App(f.cfg, f.d, func(k string) string { return f.env[k] }).Main(args, &out, &errb)
	return code, out.String() + errb.String()
}

func (f *fx) acted() bool {
	return len(f.fb.ran)+len(f.fb.stopped)+len(f.execs)+len(f.dropped) != 0
}

func (f *fx) wantRefused(why string, args ...string) {
	f.t.Helper()
	f.wantRefusedMsg(why, "", args...)
}

// wantRefusedMsg also requires the reason to be the named one, so a case cannot pass because some
// other check happened to refuse it first.
func (f *fx) wantRefusedMsg(why, msg string, args ...string) {
	f.t.Helper()
	code, out := f.call(args...)
	if !strings.Contains(out, msg) {
		f.t.Errorf("%s: refused for another reason: %s", why, out)
	}
	if code != 2 {
		f.t.Errorf("%s: exit %d, want 2 (refused): %s", why, code, out)
	}
	if f.acted() {
		f.t.Errorf("%s: it acted anyway: ran=%d stopped=%d exec=%d drop=%d", why, len(f.fb.ran), len(f.fb.stopped), len(f.execs), len(f.dropped))
	}
}

// ---- configuration ----

func TestConfigAllow(t *testing.T) {
	f := newFx(t)
	if f.cfg.Machine != "hp-ubuntu" || len(f.cfg.AllowedRoots) != 1 || !f.cfg.Enabled("attach") || f.cfg.StateDir != f.state {
		t.Fatalf("%+v", f.cfg)
	}
	if d := Defaults(); d.StateDir != "/var/lib/ccsbx" || d.Docker != "/usr/bin/docker" || d.TmuxSocket != "/run/ccsbx/tmux.sock" {
		t.Errorf("defaults moved: %+v", d)
	}
}

// The installer writes exactly `allowed_roots = [...]` and `verbs = [...]`; that file must load.
func TestConfigInstallerFile(t *testing.T) {
	f := newFx(t)
	p := filepath.Join(f.dir, "etc", "installer.toml")
	put(t, p, "# written by install-linux.sh (U0). Root-owned: tomas can't widen it. Edit with sudoedit.\nallowed_roots = [\""+f.roots+"\"]\nverbs = [\"run\", \"stop\", \"renew\", \"gc\", \"attach\", \"exec-token\"]\n", 0o644)
	if _, err := LoadConfig(p, os.Getuid()); err != nil { // machine defaults to the hostname
		t.Fatal(err)
	}
}

func TestConfigBlocks(t *testing.T) {
	f := newFx(t)
	p := filepath.Join(f.dir, "etc", "c.toml")
	base := "allowed_roots = [\"" + f.roots + "\"]\nmachine = \"m\"\n"
	cases := map[string]string{
		"an unknown key (a typo must not silently widen policy)": base + "allowed_root = [\"/\"]\n",
		"no allowed roots":              "machine = \"m\"\nallowed_roots = []\n",
		"the root directory as a root":  "allowed_roots = [\"/\"]\nmachine = \"m\"\n",
		"a relative root":               "allowed_roots = [\"git\"]\nmachine = \"m\"\n",
		"a root with a comma":           "allowed_roots = [\"" + f.roots + ",x\"]\nmachine = \"m\"\n",
		"an unknown verb":               base + "verbs = [\"run\", \"shell\"]\n",
		"a relative docker":             base + "docker = \"docker\"\n",
		"state and run dir overlapping": base + "state_dir = \"/var/lib/x\"\nrun_dir = \"/var/lib/x/run\"\n",
		"a table (only top-level keys)": base + "[[x]]\na = 1\n",
		"a non-string path":             base + "docker = 3\n",
		"a service user with a slash":   base + "service_user = \"a/b\"\n",
	}
	for why, body := range cases {
		put(t, p, body, 0o644)
		if _, err := LoadConfig(p, os.Getuid()); err == nil {
			t.Errorf("accepted: %s", why)
		}
	}
	// the file must be root's (here: the trusted uid) and closed to group and others
	put(t, p, base, 0o666)
	if _, err := LoadConfig(p, os.Getuid()); err == nil {
		t.Error("a world-writable config was believed")
	}
	put(t, p, base, 0o664)
	if _, err := LoadConfig(p, os.Getuid()); err == nil {
		t.Error("a group-writable config was believed")
	}
	put(t, p, base, 0o644)
	if _, err := LoadConfig(p, os.Getuid()+1); err == nil {
		t.Error("a config owned by someone else was believed")
	}
	mk(t, filepath.Join(f.dir, "etc"), 0o777)
	if _, err := LoadConfig(p, os.Getuid()); err == nil {
		t.Error("a config in a world-writable directory was believed")
	}
	mk(t, filepath.Join(f.dir, "etc"), 0o755)
	link := filepath.Join(f.dir, "etc", "l.toml")
	os.Symlink(p, link)
	if _, err := LoadConfig(link, os.Getuid()); err != nil {
		t.Errorf("a symlink to a trusted config was refused: %v", err)
	}
}

// ---- the prelude: who may call at all ----

func TestPrelude(t *testing.T) {
	f := newFx(t)
	f.d.Euid = 1000
	f.wantRefused("not root", "run", sid)
	f.d.Euid = 0
	f.env["SUDO_UID"] = "1000abc"
	f.wantRefused("a SUDO_UID that is not a number", "run", sid)
	f.env["SUDO_UID"] = fmt.Sprint(os.Getuid())
	f.d.LookupUser = func(string) (int, int, error) { return 0, 0, fmt.Errorf("nope") }
	f.wantRefused("no service user", "run", sid)
	f.writeConfig("verbs = [\"gc\"]\n")
	f.wantRefused("a verb the config switched off", "run", sid)
	if c, out := f.call("gc", "--dry-run"); c != 0 {
		t.Errorf("an enabled verb was refused: %d %s", c, out)
	}
}

// ---- run ----

func TestRunAllow(t *testing.T) {
	f := newFx(t)
	code, out := f.call("run", sid)
	if code != 7 || len(f.fb.ran) != 1 {
		t.Fatalf("exit %d (the fence's), ran %d: %s", code, len(f.fb.ran), out)
	}
	s := f.fb.ran[0]
	if s.Sid != sid || s.Gen != 2 || s.Image != goodImage || s.NotEvid || s.Uid != os.Getuid() || s.TTY {
		t.Errorf("%+v", s)
	}
	if s.Workdir != f.wd || s.WorktreeRoot != f.cfg.AllowedRoots[0] || s.StateDir != f.run || s.MemoryMB != 4096 || s.Pids != 512 {
		t.Errorf("%+v", s)
	}
	if strings.Join(s.Cmd, " ") != "claude --model=opus" {
		t.Errorf("cmd %v", s.Cmd)
	}
	if f.fb.docker != filepath.Join(f.dir, "bin", "docker") {
		t.Errorf("docker %q", f.fb.docker)
	}
	for _, name := range []string{"managed/managed-settings.json", "managed/managed-mcp.json", "in/session.json", "in/scope.json"} {
		if len(f.fb.files[name]) == 0 {
			t.Errorf("the fence gets no %s", name)
		}
	}
	if c, _ := f.call("run", "--tty", sid); c != 7 || !f.fb.ran[1].TTY {
		t.Error("--tty did not reach the spec")
	}
	// the owner is not the only caller: hostd runs as the service user
	f.env["SUDO_UID"] = fmt.Sprint(svcUID)
	if c, out := f.call("run", sid); c != 7 {
		t.Errorf("the service user was refused: %d %s", c, out)
	}
}

func TestRunBlocks(t *testing.T) {
	cases := []struct {
		why string
		mod func(f *fx)
		msg string // the reason it must be refused for ("" = any refusal)
	}{
		{"an unpinned image tag", func(f *fx) { f.rec.Reply.Pin.Image = "ccsbx-fence:dev" }, "unpinned"},
		{"the placeholder 'unpinned'", func(f *fx) { f.rec.Reply.Pin.Image = "unpinned" }, ""},
		{"a digest from another repository", func(f *fx) { f.rec.Reply.Pin.Image = evilImage }, "not from our repos"},
		{"a candidate digest for a non-e2e profile", func(f *fx) { f.rec.Reply.Pin.Image = candImage }, "candidate"},
		{"a malformed digest", func(f *fx) { f.rec.Reply.Pin.Image = "ghcr.io/landovsky/ccsbx-fence@sha256:zz" }, ""},
		{"an image with an option in it", func(f *fx) { f.rec.Reply.Pin.Image = "--privileged" }, ""},
		{"a workdir outside the allowed roots", func(f *fx) { f.rec.Spec.Workdir.Path = "/etc" }, "not inside an allowed root"},
		{"the allowed root itself", func(f *fx) { f.rec.Spec.Workdir.Path = f.cfg.AllowedRoots[0] }, ""},
		{"a workdir with .. in it", func(f *fx) { f.rec.Spec.Workdir.Path = f.wd + "/../../../etc" }, ""},
		{"a relative workdir", func(f *fx) { f.rec.Spec.Workdir.Path = "git/wt" }, ""},
		{"a comma in the workdir (a mount option)", func(f *fx) { f.rec.Spec.Workdir.Path = f.wd + ",readonly=false" }, ""},
		{"a workdir that is a symlink out of the root", func(f *fx) {
			os.Symlink("/etc", filepath.Join(f.roots, "git", "esc"))
			f.rec.Spec.Workdir.Path = filepath.Join(f.roots, "git", "esc")
		}, "symlink"},
		{"a symlink in the middle of the workdir path", func(f *fx) {
			os.Symlink(f.wd, filepath.Join(f.roots, "git", "alias"))
			f.rec.Spec.Workdir.Path = filepath.Join(f.roots, "git", "alias")
		}, "symlink"},
		{"a workdir owned by someone else", func(f *fx) { f.rec.Uid = os.Getuid() + 1; f.env["SUDO_UID"] = fmt.Sprint(f.rec.Uid) }, "not owned by the session"},
		{"a workdir that is a file", func(f *fx) {
			put(f.t, filepath.Join(f.roots, "git", "file"), "x", 0o644)
			f.rec.Spec.Workdir.Path = filepath.Join(f.roots, "git", "file")
		}, ""},
		{"a gitdir outside the allowed roots", func(f *fx) { f.rec.GitDir = "/etc" }, "not inside an allowed root"},
		{"a gitdir that is a symlink", func(f *fx) {
			mk(f.t, filepath.Join(f.dir, "elsewhere"), 0o755)
			os.Symlink(filepath.Join(f.dir, "elsewhere"), filepath.Join(f.roots, "git", "gd"))
			f.rec.GitDir = filepath.Join(f.roots, "git", "gd")
		}, "symlink"},
		{"a harness argument that switches the harness's safety off", func(f *fx) {
			f.rec.Spec.HarnessArgs = []string{"--dangerously-skip-permissions"}
		}, "not allowed"},
		{"a harness value with a space", func(f *fx) { f.rec.Spec.HarnessArgs = []string{"--model", "opus --x"} }, ""},
		{"an ended session", func(f *fx) { t := f.rec.Started; f.rec.Ended = &t }, "has ended"},
		{"a record for another machine", func(f *fx) { f.rec.Spec.Machine = "other-host" }, "is for machine"},
		{"a profile that is not in this build", func(f *fx) { f.rec.Spec.Profile = "made-up" }, "not approved in this build"},
		{"a profile name that is a path", func(f *fx) { f.rec.Spec.Profile = "../../etc/passwd" }, ""},
		{"a profile not approved for the billing mode", func(f *fx) { f.rec.Spec.Billing = "subscription" }, "is not approved for machine"},
		{"another user's session", func(f *fx) { f.env["SUDO_UID"] = fmt.Sprint(os.Getuid() + 7) }, "may not act"},
		{"a gen that is not a generation", func(f *fx) { f.rec.Gen = 0 }, "out of range"},
		{"a record that names another sid", func(f *fx) { f.rec.Sid = "s0zzzzzzzz" }, "names session"},
	}
	for _, c := range cases {
		f := newFx(t)
		c.mod(f)
		if c.why == "a record that names another sid" {
			// written under the sid we ask for, but its content says another
			b, _ := json.Marshal(f.rec)
			put(t, filepath.Join(f.state, "sessions", sid+".json"), string(b), 0o600)
		} else {
			f.writeRecord()
		}
		f.wantRefusedMsg(c.why, c.msg, "run", sid)
	}
}

// A candidate digest of our own repository is allowed for an e2e-* profile and for nothing else.
func TestRunCandidateOnlyForE2E(t *testing.T) {
	f := newFx(t)
	f.rec.Reply.Pin.Image, f.rec.Spec.Profile = candImage, "e2e-default"
	f.writeRecord()
	if c, out := f.call("run", sid); c != 7 {
		t.Errorf("a candidate digest for an e2e profile was refused: %d %s", c, out)
	}
}

func TestRunRefusesUntrustedInputs(t *testing.T) {
	f := newFx(t)
	rp := filepath.Join(f.state, "sessions", sid+".json")
	// the record: group/world writable, a symlink, owned by an unknown uid
	os.Chmod(rp, 0o666)
	f.wantRefusedMsg("a world-writable record", "writable by group or others", "run", sid)
	os.Chmod(rp, 0o600)
	f.cfg.TrustedUID = os.Getuid() + 1 // the record's owner is now "neither root nor the service user"
	f.wantRefusedMsg("a record owned by an unknown uid", "neither root nor the service user", "run", sid)
	f.cfg.TrustedUID = os.Getuid()
	os.Rename(rp, rp+".real")
	os.Symlink(rp+".real", rp)
	f.wantRefused("a record that is a symlink", "run", sid)
	os.Remove(rp)
	os.Rename(rp+".real", rp)
	// the sessions directory writable by others
	os.Chmod(filepath.Join(f.state, "sessions"), 0o777)
	f.wantRefused("a world-writable sessions directory", "run", sid)
	os.Chmod(filepath.Join(f.state, "sessions"), 0o700)
	// the pins file
	os.Chmod(f.cfg.PinsFile, 0o666)
	f.wantRefused("a world-writable pins file", "run", sid)
	os.Chmod(f.cfg.PinsFile, 0o644)
	// the binary
	os.Chmod(f.cfg.Docker, 0o777)
	f.wantRefused("a docker binary others can write", "run", sid)
	os.Chmod(f.cfg.Docker, 0o755)
	os.Remove(f.cfg.Docker)
	f.wantRefused("a missing docker binary", "run", sid)
	put(t, f.cfg.Docker, "#!/bin/sh\n", 0o755)
	// a huge record is refused unread
	os.WriteFile(rp, bytes.Repeat([]byte("x"), maxRecord+10), 0o600)
	f.wantRefusedMsg("a record over the size cap", "under", "run", sid)
	f.writeRecord()
	if c, out := f.call("run", sid); c != 7 {
		t.Errorf("with everything restored the run was refused: %d %s", c, out)
	}
}

func TestRunWithoutPinsFileRefusesProductionImages(t *testing.T) {
	f := newFx(t)
	os.Remove(f.cfg.PinsFile)
	f.wantRefused("a production digest and no pins.toml", "run", sid)
}

// ---- arguments: every verb takes a sid and nothing else ----

func TestArgumentShapes(t *testing.T) {
	f := newFx(t)
	bad := [][]string{
		{"run"}, {"run", sid, "extra"}, {"run", "--image", "x", sid}, {"run", "--workdir=/", sid}, {"run", sid, "--", "sh"},
		{"run", "-x"}, {"run", "../s0abcdefgh"}, {"run", "S0ABCDEFGH"}, {"run", sid + "\n"}, {"run", "s0abc;id"}, {"run", ""},
		{"stop"}, {"stop", sid, sid}, {"stop", "--force", sid}, {"attach"}, {"attach", sid, "-t", "x"}, {"attach", "--read-only", sid},
		{"attach", "--target=s1", sid}, {"gc", sid}, {"gc", "--all"}, {"renew"}, {"exec-token"}, {"exec-token", "--sid", sid},
		{"bash"}, {"shell", "-c", "id"}, {"RUN", sid}, {"--help-all"},
	}
	for _, a := range bad {
		code, out := f.call(a...)
		if code != 64 {
			t.Errorf("%q: exit %d, want 64 (usage): %s", a, code, out)
		}
	}
	if f.acted() {
		t.Error("a malformed call acted")
	}
}

func TestHelpRunsNothing(t *testing.T) {
	f := newFx(t)
	for _, v := range Verbs {
		code, out := f.call(v, "--help")
		if code != 0 || !strings.Contains(out, "ccsbx-root "+v) {
			t.Errorf("%s --help: %d %q", v, code, out)
		}
	}
	if code, out := f.call("--help"); code != 0 || !strings.Contains(out, "CCSBX_ROOT_") {
		t.Errorf("%d %q", code, out)
	}
	if f.acted() {
		t.Error("--help acted")
	}
}

// Every flag reads CCSBX_ROOT_<FLAG>, and only that prefix: the CCSBX_ namespace of the rest of
// the tools must not reach a root program.
func TestFlagsReadTheirOwnPrefix(t *testing.T) {
	f := newFx(t)
	f.env["CCSBX_ROOT_TTY"] = "1"
	f.call("run", sid)
	if len(f.fb.ran) != 1 || !f.fb.ran[0].TTY {
		t.Error("CCSBX_ROOT_TTY was not read")
	}
	f.env = map[string]string{"SUDO_UID": f.env["SUDO_UID"], "CCSBX_TTY": "1"}
	f.call("run", sid)
	if f.fb.ran[1].TTY {
		t.Error("CCSBX_TTY (another tool's prefix) was read")
	}
	_, out := f.call("run", "--help")
	if !strings.Contains(out, "CCSBX_ROOT_TTY") {
		t.Errorf("--help does not list the variable: %s", out)
	}
}

// ---- stop and gc ----

func TestStop(t *testing.T) {
	f := newFx(t)
	if c, out := f.call("stop", sid); c != 0 || len(f.fb.stopped) != 1 || f.fb.stopped[0].Container() != "ccsbx-"+sid+"-g2" {
		t.Fatalf("%d %s %+v", c, out, f.fb.stopped)
	}
	f2 := newFx(t)
	f2.env["SUDO_UID"] = fmt.Sprint(os.Getuid() + 7)
	f2.wantRefused("stopping another user's session", "stop", sid)
	f3 := newFx(t)
	f3.wantRefused("an unknown session", "stop", "s0unknownx")
}

func TestGc(t *testing.T) {
	f := newFx(t)
	f.fb.orphans = []backend.Orphan{{Kind: "container", Name: "ccsbx-" + sid + "-g1", Sid: sid, Gen: 1}}
	c, out := f.call("gc", "--dry-run")
	if c != 0 || !strings.Contains(out, "1 orphan") || f.fb.reaped != nil {
		t.Fatalf("%d %s", c, out)
	}
	if c, out := f.call("gc"); c != 0 || len(f.fb.reaped) != 1 {
		t.Fatalf("%d %s", c, out)
	}
	f.d.Euid = 1000
	f.fb.reaped = nil
	if c, _ := f.call("gc"); c != 2 || f.fb.reaped != nil {
		t.Error("gc ran without root")
	}
}

// ---- attach ----

func TestAttachAllow(t *testing.T) {
	f := newFx(t)
	f.env["TERM"] = "screen-256color"
	if c, out := f.call("attach", sid); c != 0 {
		t.Fatalf("%d %s", c, out)
	}
	if len(f.dropped) != 1 || f.dropped[0] != [2]int{svcUID, svcGID} {
		t.Fatalf("dropped %v", f.dropped)
	}
	e := f.execs[0]
	if e.path != filepath.Join(f.dir, "bin", "tmux") {
		t.Errorf("exec'd %q", e.path)
	}
	// the environment is rebuilt: no PATH, HOME or TMUX of the caller
	want := "PATH=/usr/bin:/bin HOME=/nonexistent LANG=C.UTF-8 TERM=screen-256color"
	hard := "tmux -S /run/ccsbx/tmux.sock -f /dev/null set-option -g prefix None ; set-option -g prefix2 None ; set-option -g mouse off ; unbind-key -a ; unbind-key -a -T root ; attach -t =" + sid + ":"
	if strings.Join(e.argv, " ") != hard {
		t.Errorf("attach does not harden the server itself: %v", e.argv)
	}
	if strings.Join(e.env, " ") != want {
		t.Errorf("env %v", e.env)
	}
}

func TestAttachBlocks(t *testing.T) {
	// the caller's TERM is the only thing taken from the caller, and only if it is a terminal name
	f := newFx(t)
	f.env["TERM"] = "x;touch"
	f.call("attach", sid)
	if !strings.Contains(strings.Join(f.execs[0].env, " "), "TERM=xterm-256color") {
		t.Errorf("a hostile TERM reached tmux: %v", f.execs[0].env)
	}
	f = newFx(t)
	f.env["SUDO_UID"] = fmt.Sprint(os.Getuid() + 7)
	f.wantRefused("attaching to another user's session", "attach", sid)
	f = newFx(t)
	f.wantRefused("a session with no record", "attach", "s0unknownx")
	f = newFx(t)
	f.dropErr = fmt.Errorf("setuid failed")
	if c, _ := f.call("attach", sid); c == 0 || len(f.execs) != 0 {
		t.Error("tmux was exec'd after the privilege drop failed (it would run as root)")
	}
	f = newFx(t)
	os.Chmod(f.cfg.Tmux, 0o777)
	f.wantRefused("a tmux binary others can write", "attach", sid)
	f = newFx(t)
	f.writeConfig("verbs = [\"run\"]\n")
	f.wantRefused("attach switched off", "attach", sid)
	// the service user attaching to any session is how hostd's own tools get in
	f = newFx(t)
	f.env["SUDO_UID"] = fmt.Sprint(svcUID)
	if c, out := f.call("attach", sid); c != 0 {
		t.Errorf("the service user was refused: %d %s", c, out)
	}
	// the drop happens before the exec, never after
	f = newFx(t)
	order := []string{}
	f.d.DropPrivs = func(int, int) error { order = append(order, "drop"); return nil }
	f.d.Exec = func(string, []string, []string) error { order = append(order, "exec"); return nil }
	f.call("attach", sid)
	if strings.Join(order, ",") != "drop,exec" {
		t.Errorf("order %v", order)
	}
}

func TestDropPrivsRefusesRoot(t *testing.T) {
	if err := dropPrivs(0, 0); err == nil {
		t.Error("attach as uid 0 was possible")
	}
}

// ---- verbs that are not wired ----

func TestNotWiredVerbsRefuse(t *testing.T) {
	for _, v := range []string{"renew", "exec-token"} {
		f := newFx(t)
		c, out := f.call(v, sid)
		if c != 2 || !strings.Contains(out, "not wired") {
			t.Errorf("%s: %d %s", v, c, out)
		}
		f.env["SUDO_UID"] = fmt.Sprint(os.Getuid() + 7)
		if _, out := f.call(v, sid); strings.Contains(out, "not wired") {
			t.Errorf("%s: another user's session got past the checks: %s", v, out)
		}
	}
}

func TestHarnessArgs(t *testing.T) {
	for _, ok := range [][]string{nil, {"--continue"}, {"--model", "opus"}, {"--resume", "abc-123", "--model", "claude-opus-4"}, {"--model=opus"}} {
		if _, err := harnessArgs(ok); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	for _, bad := range [][]string{{"--model"}, {"--model", "a b"}, {"--settings", "/x"}, {"--mcp-config", "x"}, {"--add-dir", "/"}, {"--dangerously-skip-permissions"}, {"-p"}, {"x"}, {"--resume", "../x"},
		// S1: a value that is itself a flag would be parsed by claude as one
		{"--resume", "--dangerously-skip-permissions"}, {"--resume", "-p"}, {"--model", "-p"}, {"--resume=--debug"}, {"--model=-p"},
		{"--continue=x"}, {"--model", "opus", "--settings", "/x"}, {"--resume", "abc", "--add-dir", "/"}, {"--MODEL", "x"}} {
		if _, err := harnessArgs(bad); err == nil {
			t.Errorf("%v accepted", bad)
		}
	}
}

// A gitdir (and a workdir) must belong to the session's owner. A directory owned by someone else
// cannot be made here without root, so the check is run against the owner it is not.
func TestGitdirAndWorkdirOwnerChecks(t *testing.T) {
	f := newFx(t)
	gd := filepath.Join(f.roots, "git", "gd")
	mk(t, gd, 0o755)
	me := os.Getuid()
	if _, err := f.cfg.gitdir(gd, me); err != nil {
		t.Fatalf("the owner's gitdir was refused: %v", err)
	}
	if _, err := f.cfg.gitdir(gd, me+1); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Errorf("a gitdir owned by someone else was accepted: %v", err)
	}
	if _, _, err := f.cfg.workdir(f.wd, me+1, ""); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Errorf("a workdir owned by someone else was accepted: %v", err)
	}
}

// The manual is --help: every verb names how it is called and what it refuses, and the flags name
// their environment variables. (The text is also what the PR shows the reviewer.)
func TestHelpText(t *testing.T) {
	f := newFx(t)
	for _, v := range append([]string{""}, Verbs...) {
		args := []string{"--help"}
		if v != "" {
			args = []string{v, "--help"}
		}
		code, out := f.call(args...)
		if code != 0 {
			t.Fatalf("%v: %d", args, code)
		}
		t.Logf("$ ccsbx-root %s\n%s", strings.Join(args, " "), out)
		if v != "" && !strings.Contains(out, "sudo -n /usr/local/sbin/ccsbx-root "+v) {
			t.Errorf("%s --help has no recipe", v)
		}
	}
}

// ---- M1: what is checked is what is mounted ----

// The directories root validated are opened by root and bound by descriptor onto root-owned
// staging mountpoints; docker is told to mount those, never the owner's paths.
func TestRunStagesPinnedDirectories(t *testing.T) {
	f := newFx(t)
	gd := filepath.Join(f.roots, "git", "repo", ".git", "worktrees", "wt")
	mk(t, gd, 0o755)
	f.rec.GitDir = gd
	f.writeRecord()
	wst, _ := os.Stat(f.wd)
	gst, _ := os.Stat(gd)
	if c, out := f.call("run", sid); c != 7 {
		t.Fatalf("%d %s", c, out)
	}
	s := f.fb.ran[0]
	if s.MountWorkdir != filepath.Join(s.GenDir(), "wt") || s.MountGitDir != filepath.Join(s.GenDir(), "gd") {
		t.Fatalf("docker would mount %q and %q", s.MountWorkdir, s.MountGitDir)
	}
	if len(f.mounter.binds) != 2 {
		t.Fatalf("binds %+v", f.mounter.binds)
	}
	w, g := f.mounter.binds[0], f.mounter.binds[1]
	if w.ino != wst.Sys().(*syscall.Stat_t).Ino || w.target != s.MountWorkdir || w.readonly {
		t.Errorf("workdir bind %+v", w)
	}
	if g.ino != gst.Sys().(*syscall.Stat_t).Ino || g.target != s.MountGitDir || !g.readonly {
		t.Errorf("gitdir bind %+v (must be read-only)", g)
	}
	if len(f.mounter.unbound) != 2 {
		t.Errorf("the staging was not undone: %v", f.mounter.unbound)
	}
}

// The race: the owner replaces the workdir (and the gitdir) with a symlink to somewhere else after
// every check and before the mount. What is mounted must still be the directory that was checked.
func TestSwappedWorkdirIsNotWhatGetsMounted(t *testing.T) {
	f := newFx(t)
	gd := filepath.Join(f.roots, "git", "repo", ".git", "worktrees", "wt")
	mk(t, gd, 0o755)
	f.rec.GitDir = gd
	f.writeRecord()
	wst, _ := os.Stat(f.wd)
	gst, _ := os.Stat(gd)
	target := filepath.Join(f.dir, "victim") // a directory the owner must never get mounted
	mk(t, target, 0o755)
	f.fb.beforeStage = func() {
		for _, p := range []string{f.wd, gd} {
			if err := os.Rename(p, p+".moved"); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(target, p); err != nil {
				t.Fatal(err)
			}
		}
	}
	if c, out := f.call("run", sid); c != 7 {
		t.Fatalf("%d %s", c, out)
	}
	vst, _ := os.Stat(target)
	for _, b := range f.mounter.binds {
		if b.ino == vst.Sys().(*syscall.Stat_t).Ino {
			t.Fatalf("the swapped-in directory was mounted: %+v", b)
		}
	}
	if f.mounter.binds[0].ino != wst.Sys().(*syscall.Stat_t).Ino || f.mounter.binds[1].ino != gst.Sys().(*syscall.Stat_t).Ino {
		t.Errorf("the mounts are not the inodes that were checked: %+v", f.mounter.binds)
	}
}

// A symlink that is already there when root opens the directory (not just a later swap) fails the
// open at that component, and so does a directory that is not the owner's by the descriptor's
// own stat.
func TestPinRefusesWhatTheDescriptorSays(t *testing.T) {
	f := newFx(t)
	me := os.Getuid()
	if _, err := pin(OpenDir, f.wd, me); err != nil {
		t.Fatalf("the owner's directory was refused: %v", err)
	}
	if _, err := pin(OpenDir, f.wd, me+1); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Errorf("a directory of another owner was pinned: %v", err)
	}
	os.Symlink(f.wd, filepath.Join(f.roots, "git", "ln"))
	if _, err := pin(OpenDir, filepath.Join(f.roots, "git", "ln"), me); err == nil {
		t.Error("a symlink was followed by the pinned open")
	}
	os.Symlink(filepath.Join(f.roots, "git"), filepath.Join(f.roots, "mid"))
	if _, err := pin(OpenDir, filepath.Join(f.roots, "mid", "wt"), me); err == nil {
		t.Error("a symlink in the middle of the path was followed by the pinned open")
	}
	put(t, filepath.Join(f.roots, "git", "afile"), "x", 0o644)
	if _, err := pin(OpenDir, filepath.Join(f.roots, "git", "afile"), me); err == nil {
		t.Error("a file was pinned as a directory")
	}
	if _, err := pin(OpenDir, "relative/path", me); err == nil {
		t.Error("a relative path was opened")
	}
}

func TestRunStopsWhenTheMountFails(t *testing.T) {
	f := newFx(t)
	f.mounter.err = fmt.Errorf("mount: operation not permitted")
	if c, _ := f.call("run", sid); c == 7 || f.fb.staged {
		t.Error("the fence was run although the mounts failed")
	}
}

func TestStopUnmountsTheStaging(t *testing.T) {
	f := newFx(t)
	// the real dockerBackend unmounts; the fake records that Stop was asked with a GenDir to find them under
	f.call("stop", sid)
	if len(f.fb.stopped) != 1 || f.fb.stopped[0].GenDir() != filepath.Join(f.run, "s", sid, "g2") {
		t.Errorf("stop was not given the generation directory: %+v", f.fb.stopped)
	}
}

func TestDockerBackendUnstageUnmountsBothMountpoints(t *testing.T) {
	m := &fakeMounter{}
	d := &dockerBackend{docker: "/x", mounter: m}
	d.unstage(backend.Spec{Sid: sid, Gen: 3, StateDir: "/run-dir"})
	if strings.Join(m.unbound, ",") != "/run-dir/s/"+sid+"/g3/wt,/run-dir/s/"+sid+"/g3/gd" {
		t.Errorf("%v", m.unbound)
	}
}

// ---- S3: hidden directories of the owner's home, whatever $HOME is ----

func TestHiddenHomeDirectoryIsNeverAWorkdir(t *testing.T) {
	f := newFx(t)
	home := filepath.Join(f.dir, "home")
	hidden := filepath.Join(home, ".ssh", "proj")
	mk(t, hidden, 0o755)
	mk(t, filepath.Join(home, "proj"), 0o755)
	f.writeConfig("")
	f.cfg.AllowedRoots = []string{home} // the operator allowed the whole home
	me := os.Getuid()
	if _, _, err := f.cfg.workdir(filepath.Join(home, "proj"), me, home); err != nil {
		t.Fatalf("an ordinary directory of the home was refused: %v", err)
	}
	if _, _, err := f.cfg.workdir(hidden, me, home); err == nil || !strings.Contains(err.Error(), "hidden directory") {
		t.Errorf("a hidden directory of the home was accepted: %v", err)
	}
	// and with the home as a parent of the root
	f.cfg.AllowedRoots = []string{f.dir}
	if _, _, err := f.cfg.workdir(hidden, me, home); err == nil {
		t.Error("a hidden directory below a root above the home was accepted")
	}
}

// ---- N2: a record must belong to a login user ----

func TestRecordUidMustBeALoginUser(t *testing.T) {
	for _, uid := range []int{0, svcUID} {
		f := newFx(t)
		f.rec.Uid = uid
		f.writeRecord()
		f.env["SUDO_UID"] = fmt.Sprint(uid)
		f.wantRefusedMsg(fmt.Sprintf("a record of uid %d", uid), "not a login user", "run", sid)
	}
	f := newFx(t)
	f.writeConfig("min_uid = 100000\n")
	f.wantRefusedMsg("a uid below min_uid", "not a login user", "run", sid)
}

// minUID is the default min_uid line of the test config unless the test sets its own.
func minUID(extra string) string {
	if strings.Contains(extra, "min_uid") {
		return ""
	}
	return "min_uid = 1\n"
}

// The gitdir is checked again on the STAGED mount, which is what the fence will see: a gitdir
// the fence (or anyone) could write is refused even if it looked fine when the record was made.
func TestStagedGitdirIsCheckedForOwnershipAndMode(t *testing.T) {
	f := newFx(t)
	gd := filepath.Join(f.roots, "git", "repo", ".git", "worktrees", "wt")
	mk(t, gd, 0o755)
	f.rec.GitDir = gd
	f.writeRecord()
	f.mounter.after = func(target string) {
		if strings.HasSuffix(target, "/gd") {
			os.Chmod(target, 0o777) // what the bound gitdir would look like
		}
	}
	if c, out := f.call("run", sid); c != 2 || !strings.Contains(out, "world-writable") || f.fb.staged {
		t.Errorf("%d %s", c, out)
	}
}

func goodProfile(t *testing.T) string {
	t.Helper()
	b, err := seccomp.Overlay([]byte(`{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[{"names":["read"],"action":"SCMP_ACT_ALLOW"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---- S2: the fence's seccomp profile and the host's setid strip ----

func TestRunUsesTheVerifiedSeccompProfile(t *testing.T) {
	f := newFx(t)
	if c, out := f.call("run", sid); c != 7 || f.fb.ran[0].Seccomp != f.cfg.SeccompFile {
		t.Fatalf("%d %s %q", c, out, f.fb.ran[0].Seccomp)
	}
}

func TestRunRefusesWithoutAGoodSeccompProfile(t *testing.T) {
	cases := map[string]func(f *fx){
		"no profile installed":             func(f *fx) { os.Remove(f.cfg.SeccompFile) },
		"a world-writable profile":         func(f *fx) { os.Chmod(f.cfg.SeccompFile, 0o666) },
		"a profile that allows by default": func(f *fx) { put(f.t, f.cfg.SeccompFile, `{"defaultAction":"SCMP_ACT_ALLOW"}`, 0o644) },
		"docker's default alone (no setid rule)": func(f *fx) {
			put(f.t, f.cfg.SeccompFile, `{"defaultAction":"SCMP_ACT_ERRNO","syscalls":[{"names":["chmod"],"action":"SCMP_ACT_ALLOW"}]}`, 0o644)
		},
	}
	for why, mod := range cases {
		f := newFx(t)
		mod(f)
		f.wantRefused(why, "run", sid)
	}
}

// Before the new generation's mounts, the worktree is cleaned of what the last one left.
func TestRunStripsSetidBitsBeforeStaging(t *testing.T) {
	f := newFx(t)
	wst, _ := os.Stat(f.wd)
	if c, out := f.call("run", sid); c != 7 {
		t.Fatalf("%d %s", c, out)
	}
	if len(f.strips) != 1 || f.strips[0].ino != wst.Sys().(*syscall.Stat_t).Ino || f.strips[0].owner != os.Getuid() || f.strips[0].binds != 0 {
		t.Errorf("%+v (must be the workdir, for its owner, before any mount)", f.strips)
	}
	f2 := newFx(t)
	f2.stripErr = fmt.Errorf("boom")
	if c, _ := f2.call("run", sid); c == 7 || f2.fb.staged {
		t.Error("the fence started although the strip failed")
	}
}

func TestStopAndGcStripSetidBits(t *testing.T) {
	f := newFx(t)
	if c, out := f.call("stop", sid); c != 0 || len(f.strips) != 1 {
		t.Fatalf("stop: %d %s %+v", c, out, f.strips)
	}
	f.stripErr = fmt.Errorf("boom")
	if c, _ := f.call("stop", sid); c != 1 {
		t.Errorf("a failed strip at stop was reported as success: %d", c)
	}
	g := newFx(t)
	g.fb.orphans = []backend.Orphan{{Kind: "container", Name: "ccsbx-" + sid + "-g1", Sid: sid, Gen: 1}, {Kind: "network", Name: "ccsbx-net-" + sid + "-g1", Sid: sid, Gen: 1}}
	if c, out := g.call("gc"); c != 0 || len(g.strips) != 1 {
		t.Fatalf("gc: %d %s %+v (one strip per session)", c, out, g.strips)
	}
	g.stripErr = fmt.Errorf("boom")
	if c, _ := g.call("gc"); c != 1 {
		t.Error("a failed strip at gc was reported as success")
	}
}

// R1: in another mount namespace (hostd's sandboxed unit) the staging and docker would see
// different trees, so run refuses rather than start a fence on an empty workdir.
func TestRunRefusesOutsideTheHostMountNamespace(t *testing.T) {
	f := newFx(t)
	f.sameNS = false
	f.wantRefusedMsg("another mount namespace", "mount namespace", "run", sid)
	g := newFx(t)
	g.nsErr = fmt.Errorf("permission denied")
	g.wantRefusedMsg("an unreadable namespace", "cannot compare mount namespaces", "run", sid)
}

func TestSameNS(t *testing.T) {
	if ok, err := sameNS("/proc/self/ns/mnt", "/proc/self/ns/mnt"); err != nil || !ok {
		t.Errorf("a namespace differs from itself: %v %v", ok, err)
	}
	if ok, err := sameNS("/proc/self/ns/mnt", "/proc/self/ns/net"); err != nil || ok {
		t.Errorf("a mount and a net namespace compared equal: %v %v", ok, err)
	}
	if _, err := sameNS("/proc/self/ns/mnt", "/nonexistent"); err == nil {
		t.Error("an unreadable namespace was accepted")
	}
}
