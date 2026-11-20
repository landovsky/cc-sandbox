// Tests for `ccsbx-root attach` on a session that runs as a pod (F5). kubectl is never run, no pty is
// opened and "root" is a fake euid: the session runner and Exec are recorders, the API read is a fake.
// What is tested is the policy: who the launch and terminal users may be, what kubectl is allowed to
// read, that only the launch user's own process execs kubectl and only after the pod's live UID matched.
// Every refusal is a block twin, seen red with the check removed.
package rootcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/landovsky/cc-sandbox/internal/backend"
	"github.com/landovsky/cc-sandbox/internal/k8s"
)

const (
	launchUID = 5151
	launchGID = 5152
	termUID   = 6161
	termGID   = 6162
	signerUID = 7171
	signerGID = 7172
	podUID    = "7c1f0a52-aaaa-bbbb-cccc-0123456789ab"
)

type k8sCalls struct {
	live  []string // "ns/sid/gen" of each LivePodUID call
	order []string
	uid   string
	err   error
}

// newK8sFx is newFx with a pod session: connection file, CA, kubeconfig, token, kubectl, all
// acceptable, and a fake API that says the pod has UID podUID.
func newK8sFx(t *testing.T) (*fx, *k8sCalls) {
	t.Helper()
	f := newFx(t)
	me := os.Getuid()
	etc := filepath.Join(f.dir, "etc")
	launch := filepath.Join(f.dir, "launch")
	mk(t, launch, 0o750)
	put(t, filepath.Join(f.dir, "bin", "kubectl"), "#!/bin/sh\n", 0o755)
	put(t, filepath.Join(f.dir, "bin", "ccsbx-root"), "#!/bin/sh\n", 0o755)
	put(t, filepath.Join(etc, "ca.pem"), "x", 0o644)
	put(t, filepath.Join(launch, "launcher.kubeconfig"), "apiVersion: v1\n", 0o600)
	put(t, filepath.Join(launch, "token"), "tok\n", 0o600)
	put(t, filepath.Join(etc, "k8s-conn.json"), fmt.Sprintf(`{"server":"https://10.0.0.1:6443","ca_file":%q,"token_file":%q}`,
		filepath.Join(etc, "ca.pem"), filepath.Join(launch, "token")), 0o644)
	f.writeConfig(fmt.Sprintf("kubectl = %q\nkubeconfig = %q\nk8s_conn = %q\nlaunch_user = \"ccsbx-launch\"\nterm_user = \"ccsbx-term\"\nsigner_user = \"ccsbx-keys\"\nk8s_namespace = \"cc-sandbox\"\n",
		filepath.Join(f.dir, "bin", "kubectl"), filepath.Join(launch, "launcher.kubeconfig"), filepath.Join(etc, "k8s-conn.json")))
	owner := me + 7 // the session's owner is neither root, the service user nor the launch user
	f.env["SUDO_UID"] = fmt.Sprint(owner)
	f.rec.Uid, f.rec.Backend, f.rec.PodUID, f.rec.Spec.Machine = owner, "k8s", podUID, "future-two"
	f.writeRecord()
	calls := &k8sCalls{uid: podUID}
	lookup := f.d.LookupUser
	f.d.LookupUser = func(n string) (int, int, error) {
		switch n {
		case "ccsbx-launch":
			return launchUID, launchGID, nil
		case "ccsbx-term":
			return termUID, termGID, nil
		case "ccsbx-keys":
			return signerUID, signerGID, nil
		}
		return lookup(n)
	}
	f.d.LivePodUID = func(c *k8s.ConnConfig, ns, s string, gen int) (string, error) {
		calls.live = append(calls.live, fmt.Sprintf("%s/%s/%d", ns, s, gen))
		calls.order = append(calls.order, "live")
		return calls.uid, calls.err
	}
	f.d.SelfPath = func() (string, error) { return filepath.Join(f.dir, "bin", "ccsbx-root"), nil }
	f.d.AttachSession = func(s AttachSpec) (int, error) {
		f.sessions = append(f.sessions, s)
		return f.sessionCode, f.sessionErr
	}
	ex := f.d.Exec
	f.d.Exec = func(p string, a, e []string) error { calls.order = append(calls.order, "exec"); return ex(p, a, e) }
	return f, calls
}

// asLaunch runs `attach-exec` the way the launch user's own process does: its euid is the launch uid.
func (f *fx) asLaunch(args ...string) (int, string) {
	f.t.Helper()
	f.d.Euid = launchUID
	defer func() { f.d.Euid = 0 }()
	return f.call(append([]string{"attach-exec", "--"}, args...)...)
}

// refusedK8s: `attach` is refused (exit 2) for the named reason and no session was started.
func (f *fx) refusedK8s(why, msg string) {
	f.t.Helper()
	code, out := f.call("attach", sid)
	if code != 2 || !strings.Contains(out, msg) {
		f.t.Errorf("%s: exit %d, %s", why, code, out)
	}
	if len(f.sessions) != 0 || len(f.execs) != 0 || len(f.dropped) != 0 {
		f.t.Errorf("%s: it acted anyway: sessions=%d execs=%d drops=%d", why, len(f.sessions), len(f.execs), len(f.dropped))
	}
}

func TestAttachToAPodStartsTheSessionForTheRightUsersAndTouchesNothingItself(t *testing.T) {
	f, calls := newK8sFx(t)
	f.env["TERM"] = "screen-256color"
	f.env["KUBECONFIG"] = "/home/owner/evil"
	f.sessionCode = 7
	if c, out := f.call("attach", sid); c != 7 {
		t.Fatalf("%d %s (the session's exit status is the verb's)", c, out)
	}
	if len(f.sessions) != 1 || len(f.execs) != 0 || len(f.dropped) != 0 || len(calls.live) != 0 {
		t.Fatalf("sessions=%d execs=%d drops=%d live=%d: root must only check and hand over", len(f.sessions), len(f.execs), len(f.dropped), len(calls.live))
	}
	s := f.sessions[0]
	if s.Self != filepath.Join(f.dir, "bin", "ccsbx-root") || strings.Join(s.Args, " ") != "attach-exec -- "+sid+" 2 "+podUID {
		t.Errorf("the child: %q %v", s.Self, s.Args)
	}
	if s.LaunchUID != launchUID || s.LaunchGID != launchGID || s.TermUID != termUID || s.TermGID != termGID {
		t.Errorf("users %+v", s)
	}
	if strings.Join(s.Env, " ") != "PATH=/usr/bin:/bin HOME=/nonexistent LANG=C.UTF-8 TERM=screen-256color" {
		t.Errorf("env %v (the caller's KUBECONFIG must not reach kubectl)", s.Env)
	}
}

func TestTheLaunchUsersProcessChecksTheLivePodThenExecsKubectl(t *testing.T) {
	f, calls := newK8sFx(t)
	if c, out := f.asLaunch(sid, "2", podUID); c != 0 {
		t.Fatalf("%d %s", c, out)
	}
	if strings.Join(calls.order, ",") != "live,exec" || len(calls.live) != 1 || calls.live[0] != "cc-sandbox/"+sid+"/2" {
		t.Errorf("order %v live %v: the pod is looked up by the derived name, then kubectl", calls.order, calls.live)
	}
	e := f.execs[0]
	kc := filepath.Join(f.dir, "launch", "launcher.kubeconfig")
	want := filepath.Join(f.dir, "bin", "kubectl") + " --kubeconfig " + kc + " --server https://10.0.0.1:6443 --certificate-authority " + filepath.Join(f.dir, "etc", "ca.pem") +
		" --insecure-skip-tls-verify=false -n cc-sandbox exec -it -c fence ccsbx-s-" + sid + "-g2 -- env TERM=xterm-256color tmux attach-session -t =" + sid + ":"
	if e.path != filepath.Join(f.dir, "bin", "kubectl") || strings.Join(e.argv, " ") != want {
		t.Errorf("exec %q\n got %v\nwant %s", e.path, e.argv, want)
	}
	if strings.Join(e.env, " ") != "PATH=/usr/bin:/bin HOME=/nonexistent LANG=C.UTF-8" {
		t.Errorf("env %v", e.env)
	}
}

// Only the launch user's own process may run it: root, the owner and anyone else get nothing, and a
// pod that is not the recorded one never gets the owner's terminal.
func TestAttachExecRefusesEveryoneButTheLaunchUserAndAnyPodButTheRecordedOne(t *testing.T) {
	for who, euid := range map[string]int{"root": 0, "the owner": os.Getuid() + 7, "the service user": svcUID, "the signer's user": signerUID, "the terminal user": termUID} {
		f, calls := newK8sFx(t)
		f.d.Euid = euid
		if c, out := f.call("attach-exec", "--", sid, "2", podUID); c != 2 || len(f.execs) != 0 || len(calls.live) != 0 {
			t.Errorf("%s: exit %d execs=%d live=%d: %s", who, c, len(f.execs), len(calls.live), out)
		}
	}
	for why, args := range map[string][]string{
		"no arguments": {}, "too many": {sid, "2", podUID, "x"}, "a bad sid": {"../x", "2", podUID}, "generation 0": {sid, "0", podUID},
		"generation 02": {sid, "02", podUID}, "a bad uid": {sid, "2", "a b;c"}, "an empty uid": {sid, "2", ""},
	} {
		f, calls := newK8sFx(t)
		if c, _ := f.asLaunch(args...); c != 2 || len(f.execs) != 0 || len(calls.live) != 0 {
			t.Errorf("%s: exit %d", why, c)
		}
	}
	f, calls := newK8sFx(t)
	calls.uid = "someone-elses-pod"
	if c, out := f.asLaunch(sid, "2", podUID); c != 2 || len(f.execs) != 0 || !strings.Contains(out, "pod_conflict") {
		t.Errorf("another pod under the name: %d %s", c, out)
	}
	f, calls = newK8sFx(t)
	calls.uid = ""
	if c, _ := f.asLaunch(sid, "2", podUID); c != 2 || len(f.execs) != 0 {
		t.Errorf("a pod with no UID: %d", c)
	}
	f, calls = newK8sFx(t)
	calls.err = fmt.Errorf("connection refused")
	if c, out := f.asLaunch(sid, "2", podUID); c != 1 || len(f.execs) != 0 || !strings.Contains(out, "cannot read the pod") {
		t.Errorf("API down: %d %s", c, out)
	}
	// the files are checked again by the launch user's own process (root's check is not taken on trust)
	f, calls = newK8sFx(t)
	os.Chmod(filepath.Join(f.dir, "launch", "token"), 0o644)
	if c, _ := f.asLaunch(sid, "2", podUID); c != 2 || len(f.execs) != 0 || len(calls.live) != 0 {
		t.Errorf("a token others can read: %d", c)
	}
}

func TestAttachToAPodIsRefusedWithoutARecordedUID(t *testing.T) {
	f, _ := newK8sFx(t)
	f.rec.PodUID = ""
	f.writeRecord()
	f.refusedK8s("no UID recorded", "no pod UID recorded")
	f, _ = newK8sFx(t)
	f.rec.PodUID = "has spaces; rm"
	f.writeRecord()
	f.refusedK8s("a malformed UID in the record", "pod_uid")
}

func TestAttachToAPodRefusesWhatTheOwnerCouldSwap(t *testing.T) {
	cases := map[string]func(f *fx){
		"a kubeconfig others can write": func(f *fx) { os.Chmod(filepath.Join(f.dir, "launch", "launcher.kubeconfig"), 0o666) },
		"a kubeconfig that is a link": func(f *fx) {
			p := filepath.Join(f.dir, "launch", "launcher.kubeconfig")
			os.Rename(p, p+".real")
			os.Symlink(p+".real", p)
		},
		"a kubeconfig directory others can write": func(f *fx) { os.Chmod(filepath.Join(f.dir, "launch"), 0o777) },
		"a token others can write":                func(f *fx) { os.Chmod(filepath.Join(f.dir, "launch", "token"), 0o662) },
		"a connection file others can write":      func(f *fx) { os.Chmod(filepath.Join(f.dir, "etc", "k8s-conn.json"), 0o666) },
		"a CA file others can write":              func(f *fx) { os.Chmod(filepath.Join(f.dir, "etc", "ca.pem"), 0o666) },
		"a kubectl others can write":              func(f *fx) { os.Chmod(filepath.Join(f.dir, "bin", "kubectl"), 0o777) },
		"a ccsbx-root binary others can write":    func(f *fx) { os.Chmod(filepath.Join(f.dir, "bin", "ccsbx-root"), 0o777) },
		"a plain-http server": func(f *fx) {
			put(f.t, filepath.Join(f.dir, "etc", "k8s-conn.json"), fmt.Sprintf(`{"server":"http://localhost:8080","ca_file":%q,"token_file":%q}`,
				filepath.Join(f.dir, "etc", "ca.pem"), filepath.Join(f.dir, "launch", "token")), 0o644)
		},
		"a missing kubeconfig": func(f *fx) { os.Remove(filepath.Join(f.dir, "launch", "launcher.kubeconfig")) },
	}
	for why, mod := range cases {
		f, _ := newK8sFx(t)
		mod(f)
		code, out := f.call("attach", sid)
		if code != 2 || len(f.sessions) != 0 {
			t.Errorf("%s: exit %d sessions=%d: %s", why, code, len(f.sessions), out)
		}
	}
}

// Each way the launch user could be someone who can rewrite what kubectl reads, or the terminal user
// someone with something to steal, has its own case in which only that check refuses.
func TestTheLaunchAndTerminalUsersAreNobodyElse(t *testing.T) {
	as := func(f *fx, name string, uid, gid int) {
		l := f.d.LookupUser
		f.d.LookupUser = func(n string) (int, int, error) {
			if n == name {
				return uid, gid, nil
			}
			return l(n)
		}
	}
	missing := func(f *fx, name string) {
		l := f.d.LookupUser
		f.d.LookupUser = func(n string) (int, int, error) {
			if n == name {
				return 0, 0, fmt.Errorf("no such user")
			}
			return l(n)
		}
	}
	type tc struct {
		why, msg string
		mod      func(f *fx)
	}
	for _, c := range []tc{
		{"the launch user is the caller (root by login)", "is the caller", func(f *fx) { delete(f.env, "SUDO_UID"); as(f, "ccsbx-launch", 0, launchGID) }},
		{"the launch user is the owner", "is the session's owner", func(f *fx) { delete(f.env, "SUDO_UID"); as(f, "ccsbx-launch", f.rec.Uid, launchGID) }},
		{"the launch user is the service user", "is the service user", func(f *fx) { as(f, "ccsbx-launch", svcUID, launchGID) }},
		{"the launch user is the signer's own user", "signer's own user", func(f *fx) { as(f, "ccsbx-launch", signerUID, launchGID) }},
		{"a launch user with group 0", "primary group", func(f *fx) { as(f, "ccsbx-launch", launchUID, 0) }},
		{"a launch user in the service group", "primary group", func(f *fx) { as(f, "ccsbx-launch", launchUID, svcGID) }},
		{"the signer user cannot be found", "signer user", func(f *fx) { missing(f, "ccsbx-keys") }},
		{"no launch user", "launch user", func(f *fx) { missing(f, "ccsbx-launch") }},
		{"the terminal user is the launch user", "terminal user", func(f *fx) { as(f, "ccsbx-term", launchUID, termGID) }},
		{"the terminal user is the signer's user", "terminal user", func(f *fx) { as(f, "ccsbx-term", signerUID, termGID) }},
		{"the terminal user is the service user", "terminal user", func(f *fx) { as(f, "ccsbx-term", svcUID, termGID) }},
		{"the terminal user is the owner (root attaches)", "terminal user", func(f *fx) { delete(f.env, "SUDO_UID"); as(f, "ccsbx-term", f.rec.Uid, termGID) }},
		{"the terminal user is the caller (root by login)", "terminal user", func(f *fx) { delete(f.env, "SUDO_UID"); as(f, "ccsbx-term", 0, termGID) }},
		{"a terminal user with group 0", "primary group", func(f *fx) { as(f, "ccsbx-term", termUID, 0) }},
		{"a terminal user in the service group", "primary group", func(f *fx) { as(f, "ccsbx-term", termUID, svcGID) }},
		{"no terminal user", "terminal user", func(f *fx) { missing(f, "ccsbx-term") }},
	} {
		f, _ := newK8sFx(t)
		c.mod(f)
		f.refusedK8s(c.why, c.msg)
	}
}

// Only the owner, or root by login, gets a terminal in a pod: the service user (hostd) may start and stop
// pod sessions but must not hold kubectl exec into every fence with the launcher credential.
func TestOnlyTheOwnerOrRootAttachesToAPod(t *testing.T) {
	f, _ := newK8sFx(t)
	f.env["SUDO_UID"] = fmt.Sprint(svcUID)
	f.refusedK8s("the service user", "no such session")
	f, _ = newK8sFx(t)
	delete(f.env, "SUDO_UID") // root by login
	if c, out := f.call("attach", sid); c != 0 || len(f.sessions) != 1 {
		t.Errorf("root by login was refused: %d %s", c, out)
	}
}

// The launcher credential is the launch user's alone: readable by group or others is as bad as writable.
func TestAttachToAPodRefusesACredentialOthersCanRead(t *testing.T) {
	for why, mod := range map[string]func(f *fx){
		"a kubeconfig readable by others":     func(f *fx) { os.Chmod(filepath.Join(f.dir, "launch", "launcher.kubeconfig"), 0o644) },
		"a kubeconfig readable by group":      func(f *fx) { os.Chmod(filepath.Join(f.dir, "launch", "launcher.kubeconfig"), 0o640) },
		"a token readable by others":          func(f *fx) { os.Chmod(filepath.Join(f.dir, "launch", "token"), 0o644) },
		"a token readable by group":           func(f *fx) { os.Chmod(filepath.Join(f.dir, "launch", "token"), 0o640) },
		"a launch directory others can enter": func(f *fx) { os.Chmod(filepath.Join(f.dir, "launch"), 0o755) },
		"a kubeconfig that is a directory": func(f *fx) {
			p := filepath.Join(f.dir, "launch", "launcher.kubeconfig")
			os.Remove(p)
			mk(f.t, p, 0o700)
		},
		"a token that is a directory": func(f *fx) {
			p := filepath.Join(f.dir, "launch", "token")
			os.Remove(p)
			mk(f.t, p, 0o700)
		},
	} {
		f, _ := newK8sFx(t)
		mod(f)
		code, out := f.call("attach", sid)
		if code != 2 || len(f.sessions) != 0 {
			t.Errorf("%s: exit %d sessions=%d: %s", why, code, len(f.sessions), out)
		}
	}
	f, _ := newK8sFx(t)
	os.Chmod(filepath.Join(f.dir, "launch", "token"), 0o644)
	if _, out := f.call("attach", sid); !strings.Contains(out, "launch user alone") {
		t.Errorf("%s", out)
	}
}

func TestAttachToAPodTakesOnlyAPlausibleTERM(t *testing.T) {
	f, _ := newK8sFx(t)
	f.env["TERM"] = "evil;cmd"
	f.call("attach", sid)
	if len(f.sessions) != 1 || !strings.Contains(strings.Join(f.sessions[0].Env, " "), "TERM=xterm-256color") {
		t.Errorf("a hostile TERM reached the session: %v", f.sessions)
	}
}

// Another user's pod session is as invisible as another user's docker session, and the docker verbs
// do not act on a pod's session at all.
func TestPodSessionsAreOnlyAttachable(t *testing.T) {
	f, _ := newK8sFx(t)
	f.env["SUDO_UID"] = fmt.Sprint(os.Getuid() + 99)
	f.refusedK8s("another user's pod", "no such session")
	for _, v := range []string{"run", "status"} {
		f, _ := newK8sFx(t)
		code, out := f.call(v, sid)
		if code != 2 || !strings.Contains(out, "runs as a pod") || len(f.fb.ran)+len(f.fb.stopped) != 0 {
			t.Errorf("%s on a pod session: %d %s", v, code, out)
		}
	}
	f, _ = newK8sFx(t)
	f.rec.Backend = "nomad"
	f.writeRecord()
	f.refusedK8s("an unknown backend in the record", "backend")
}

func TestK8sConfigKeysAreValidated(t *testing.T) {
	f := newFx(t)
	p := filepath.Join(f.dir, "etc", "c.toml")
	base := "allowed_roots = [\"" + f.roots + "\"]\nmachine = \"m\"\n"
	for why, extra := range map[string]string{
		"a relative kubectl":                  "kubectl = \"kubectl\"\n",
		"a relative kubeconfig":               "kubeconfig = \"k\"\n",
		"a relative connection file":          "k8s_conn = \"c.json\"\n",
		"the service user as the launch user": "launch_user = \"ccsbx\"\n",
		"a launch user with a slash":          "launch_user = \"a/b\"\n",
		"a namespace that is not a DNS name":  "k8s_namespace = \"Cc_Sandbox\"\n",
	} {
		put(t, p, base+extra, 0o644)
		if _, err := LoadConfig(p, os.Getuid()); err == nil {
			t.Errorf("%s was accepted", why)
		}
	}
	if d := Defaults(); d.LaunchUser != "ccsbx-launch" || d.K8sNamespace != "cc-sandbox" || d.Kubectl != "/usr/local/bin/kubectl" {
		t.Errorf("defaults moved: %+v", d)
	}
}

// gc cleans docker orphans and the setid bits in a host worktree; a pod session has neither, so an
// orphan that names one is left alone (and gc does not fail on it).
func TestGcLeavesAPodSessionAlone(t *testing.T) {
	f, _ := newK8sFx(t)
	f.fb.orphans = []backend.Orphan{{Kind: "container", Name: "ccsbx-" + sid + "-g2", Sid: sid, Gen: 2}}
	if c, out := f.call("gc"); c != 0 || len(f.strips) != 0 {
		t.Errorf("gc on a pod session: %d %s strips=%v", c, out, f.strips)
	}
}
