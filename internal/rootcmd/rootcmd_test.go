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
	"testing"

	"github.com/landovsky/cc-sandbox/internal/backend"
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
}

func (f *fakeBackend) Run(s backend.Spec, files map[string][]byte, _ io.Reader, _, _ io.Writer) (int, error) {
	f.ran, f.files = append(f.ran, s), files
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
	f := &fx{t: t, dir: vtmp(t), env: map[string]string{"SUDO_UID": fmt.Sprint(me), "TERM": "xterm-256color"}, fb: &fakeBackend{code: 7}}
	f.roots, f.state, f.run = filepath.Join(f.dir, "roots"), filepath.Join(f.dir, "state"), filepath.Join(f.dir, "run")
	f.wd = filepath.Join(f.roots, "git", "wt")
	mk(t, f.wd, 0o755)
	mk(t, filepath.Join(f.state, "sessions"), 0o700)
	mk(t, filepath.Join(f.dir, "bin"), 0o755)
	mk(t, filepath.Join(f.dir, "etc"), 0o755)
	put(t, filepath.Join(f.dir, "bin", "docker"), "#!/bin/sh\n", 0o755)
	put(t, filepath.Join(f.dir, "bin", "tmux"), "#!/bin/sh\n", 0o755)
	put(t, filepath.Join(f.dir, "etc", "pins.toml"), "[[pin]]\nref = \""+goodImage+"\"\ncc = \"2.1.0\"\n", 0o644)
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
	body := fmt.Sprintf("allowed_roots = [%q]\nstate_dir = %q\nrun_dir = %q\ndocker = %q\ntmux = %q\ntmux_socket = \"/run/ccsbx/tmux.sock\"\npins_file = %q\nmachine = \"hp-ubuntu\"\n%s",
		f.roots, f.state, f.run, filepath.Join(f.dir, "bin", "docker"), filepath.Join(f.dir, "bin", "tmux"), filepath.Join(f.dir, "etc", "pins.toml"), extra)
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
		NewBackend: func(docker string) Backend { f.fb.docker = docker; return f.fb },
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
	if strings.Join(s.Cmd, " ") != "claude --model opus" {
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
	if strings.Join(e.argv, " ") != "tmux -S /run/ccsbx/tmux.sock attach -t ="+sid+":" {
		t.Errorf("argv %v", e.argv)
	}
	// the environment is rebuilt: no PATH, HOME or TMUX of the caller
	want := "PATH=/usr/bin:/bin HOME=/nonexistent LANG=C.UTF-8 TERM=screen-256color"
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
	for _, ok := range [][]string{nil, {"--continue"}, {"--model", "opus"}, {"--resume", "abc-123", "--model", "claude-opus-4"}} {
		if _, err := harnessArgs(ok); err != nil {
			t.Errorf("%v refused: %v", ok, err)
		}
	}
	for _, bad := range [][]string{{"--model"}, {"--model", "a b"}, {"--settings", "/x"}, {"--mcp-config", "x"}, {"--add-dir", "/"}, {"--dangerously-skip-permissions"}, {"-p"}, {"x"}, {"--resume", "../x"}} {
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
	if _, _, err := f.cfg.workdir(f.wd, me+1); err == nil || !strings.Contains(err.Error(), "not owned") {
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
