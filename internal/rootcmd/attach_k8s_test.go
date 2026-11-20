// Tests for `ccsbx-root attach` on a session that runs as a pod (F5). kubectl is never run: Exec is
// a recorder, the API read is a fake, and "root" is a fake euid, so what is tested is the policy:
// what kubectl is allowed to read, who it runs as, and that the pod's UID is checked first.
// Every refusal is a block twin, seen red with the check removed.
package rootcmd

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/landovsky/cc-sandbox/internal/k8s"
)

const (
	keysUID = 5151
	keysGID = 5152
	podUID  = "7c1f0a52-aaaa-bbbb-cccc-0123456789ab"
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
	keys := filepath.Join(f.dir, "keys")
	mk(t, keys, 0o750)
	put(t, filepath.Join(f.dir, "bin", "kubectl"), "#!/bin/sh\n", 0o755)
	put(t, filepath.Join(etc, "ca.pem"), "x", 0o644)
	put(t, filepath.Join(keys, "launcher.kubeconfig"), "apiVersion: v1\n", 0o600)
	put(t, filepath.Join(keys, "token"), "tok\n", 0o600)
	put(t, filepath.Join(etc, "k8s-conn.json"), fmt.Sprintf(`{"server":"https://10.0.0.1:6443","ca_file":%q,"token_file":%q}`,
		filepath.Join(etc, "ca.pem"), filepath.Join(keys, "token")), 0o644)
	f.writeConfig(fmt.Sprintf("kubectl = %q\nkubeconfig = %q\nk8s_conn = %q\nkeys_user = \"ccsbx-keys\"\nk8s_namespace = \"cc-sandbox\"\n",
		filepath.Join(f.dir, "bin", "kubectl"), filepath.Join(keys, "launcher.kubeconfig"), filepath.Join(etc, "k8s-conn.json")))
	owner := me + 7 // the session's owner is neither root, the service user nor the keys user
	f.env["SUDO_UID"] = fmt.Sprint(owner)
	f.rec.Uid, f.rec.Backend, f.rec.PodUID, f.rec.Spec.Machine = owner, "k8s", podUID, "future-two"
	f.writeRecord()
	calls := &k8sCalls{uid: podUID}
	lookup := f.d.LookupUser
	f.d.LookupUser = func(n string) (int, int, error) {
		if n == "ccsbx-keys" {
			return keysUID, keysGID, nil
		}
		return lookup(n)
	}
	f.d.LivePodUID = func(c *k8s.ConnConfig, ns, s string, gen int) (string, error) {
		calls.live = append(calls.live, fmt.Sprintf("%s/%s/%d", ns, s, gen))
		calls.order = append(calls.order, "live")
		return calls.uid, calls.err
	}
	dp, ex := f.d.DropPrivs, f.d.Exec
	f.d.DropPrivs = func(u, g int) error { calls.order = append(calls.order, "drop"); return dp(u, g) }
	f.d.Exec = func(p string, a, e []string) error { calls.order = append(calls.order, "exec"); return ex(p, a, e) }
	// the keys user owns what kubectl reads; in the test that is the test's own uid, which root-config trusts
	_ = me
	return f, calls
}

// refusedK8s: exit 2 for the named reason and kubectl never exec'd.
func (f *fx) refusedK8s(why, msg string) {
	f.t.Helper()
	code, out := f.call("attach", sid)
	if code != 2 || !strings.Contains(out, msg) {
		f.t.Errorf("%s: exit %d, %s", why, code, out)
	}
	if len(f.execs) != 0 {
		f.t.Errorf("%s: kubectl was exec'd anyway: %v", why, f.execs)
	}
}

func TestAttachToAPodRunsKubectlAsTheKeysUserAfterTheUIDCheck(t *testing.T) {
	f, calls := newK8sFx(t)
	f.env["TERM"] = "screen-256color"
	f.env["KUBECONFIG"] = "/home/owner/evil"
	if c, out := f.call("attach", sid); c != 0 {
		t.Fatalf("%d %s", c, out)
	}
	if strings.Join(calls.order, ",") != "drop,live,exec" {
		t.Errorf("order %v: drop to the keys user, read the live UID, only then exec", calls.order)
	}
	if len(f.dropped) != 1 || f.dropped[0] != [2]int{keysUID, keysGID} {
		t.Errorf("dropped %v", f.dropped)
	}
	if len(calls.live) != 1 || calls.live[0] != "cc-sandbox/"+sid+"/2" {
		t.Errorf("live %v", calls.live)
	}
	e := f.execs[0]
	kc := filepath.Join(f.dir, "keys", "launcher.kubeconfig")
	want := filepath.Join(f.dir, "bin", "kubectl") + " --kubeconfig " + kc + " --server https://10.0.0.1:6443 --certificate-authority " + filepath.Join(f.dir, "etc", "ca.pem") +
		" --insecure-skip-tls-verify=false -n cc-sandbox exec -it -c fence ccsbx-s-" + sid + "-g2 -- tmux attach-session -t =" + sid + ":"
	if e.path != filepath.Join(f.dir, "bin", "kubectl") || strings.Join(e.argv, " ") != want {
		t.Errorf("exec %q %v", e.path, e.argv)
	}
	if strings.Join(e.env, " ") != "PATH=/usr/bin:/bin HOME=/nonexistent LANG=C.UTF-8 TERM=screen-256color" {
		t.Errorf("env %v (the caller's KUBECONFIG must not reach kubectl)", e.env)
	}
}

func TestAttachToAPodIsRefusedWithoutTheRightUID(t *testing.T) {
	f, calls := newK8sFx(t)
	f.rec.PodUID = ""
	f.writeRecord()
	f.refusedK8s("no UID recorded", "no pod UID recorded")
	if len(calls.live) != 0 || len(f.dropped) != 0 {
		t.Error("the API was asked, or privileges dropped, with no UID to compare to")
	}

	f, calls = newK8sFx(t)
	calls.uid = "someone-elses-pod"
	f.refusedK8s("another pod under the name", "pod_conflict")
	f, calls = newK8sFx(t)
	calls.uid = ""
	f.refusedK8s("a pod with no UID", "pod_conflict")
	f, calls = newK8sFx(t)
	calls.err = fmt.Errorf("connection refused")
	if c, out := f.call("attach", sid); c != 1 || len(f.execs) != 0 || !strings.Contains(out, "cannot read the pod") {
		t.Errorf("API down: %d %s", c, out)
	}
	f, calls = newK8sFx(t)
	f.rec.PodUID = "has spaces; rm"
	f.writeRecord()
	f.refusedK8s("a malformed UID in the record", "pod_uid")
}

func TestAttachToAPodRefusesWhatTheOwnerCouldSwap(t *testing.T) {
	cases := map[string]func(f *fx){
		"a kubeconfig others can write": func(f *fx) { os.Chmod(filepath.Join(f.dir, "keys", "launcher.kubeconfig"), 0o666) },
		"a kubeconfig that is a link": func(f *fx) {
			p := filepath.Join(f.dir, "keys", "launcher.kubeconfig")
			os.Rename(p, p+".real")
			os.Symlink(p+".real", p)
		},
		"a kubeconfig directory others can write": func(f *fx) { os.Chmod(filepath.Join(f.dir, "keys"), 0o777) },
		"a token others can write":                func(f *fx) { os.Chmod(filepath.Join(f.dir, "keys", "token"), 0o662) },
		"a connection file others can write":      func(f *fx) { os.Chmod(filepath.Join(f.dir, "etc", "k8s-conn.json"), 0o666) },
		"a CA file others can write":              func(f *fx) { os.Chmod(filepath.Join(f.dir, "etc", "ca.pem"), 0o666) },
		"a kubectl others can write":              func(f *fx) { os.Chmod(filepath.Join(f.dir, "bin", "kubectl"), 0o777) },
		"a plain-http server": func(f *fx) {
			put(f.t, filepath.Join(f.dir, "etc", "k8s-conn.json"), fmt.Sprintf(`{"server":"http://localhost:8080","ca_file":%q,"token_file":%q}`,
				filepath.Join(f.dir, "etc", "ca.pem"), filepath.Join(f.dir, "keys", "token")), 0o644)
		},
		"a missing kubeconfig": func(f *fx) { os.Remove(filepath.Join(f.dir, "keys", "launcher.kubeconfig")) },
	}
	for why, mod := range cases {
		f, calls := newK8sFx(t)
		mod(f)
		code, out := f.call("attach", sid)
		if code != 2 || len(f.execs) != 0 || len(f.dropped) != 0 || len(calls.live) != 0 {
			t.Errorf("%s: exit %d exec=%d drop=%d live=%d: %s", why, code, len(f.execs), len(f.dropped), len(calls.live), out)
		}
	}
}

func TestAttachToAPodNeedsAKeysUserApartFromTheCallerAndTheService(t *testing.T) {
	f, _ := newK8sFx(t)
	l := f.d.LookupUser
	f.d.LookupUser = func(n string) (int, int, error) {
		if n == "ccsbx-keys" {
			return f.rec.Uid, 1, nil // the owner holds the keys: the owner could rewrite what kubectl reads
		}
		return l(n)
	}
	f.refusedK8s("the caller is the keys user", "neither the caller")
	f, _ = newK8sFx(t)
	l = f.d.LookupUser
	f.d.LookupUser = func(n string) (int, int, error) {
		if n == "ccsbx-keys" {
			return svcUID, 1, nil
		}
		return l(n)
	}
	f.refusedK8s("the service user is the keys user", "neither the caller")
	f, _ = newK8sFx(t)
	f.d.LookupUser = func(n string) (int, int, error) {
		if n == "ccsbx-keys" {
			return 0, 0, fmt.Errorf("no such user")
		}
		return svcUID, svcGID, nil
	}
	f.refusedK8s("no keys user", "keys user")
	f, calls := newK8sFx(t)
	f.dropErr = fmt.Errorf("setuid failed")
	if c, _ := f.call("attach", sid); c == 0 || len(f.execs) != 0 || len(calls.live) != 0 {
		t.Error("kubectl ran, or the API was read as root, after the drop failed")
	}
}

// Another user's pod session is as invisible as another user's docker session, and the docker verbs
// do not act on a pod's session at all.
func TestPodSessionsAreOnlyAttachable(t *testing.T) {
	f, _ := newK8sFx(t)
	f.env["SUDO_UID"] = fmt.Sprint(os.Getuid() + 99)
	f.refusedK8s("another user's pod", "no such session")
	for _, v := range []string{"run", "stop", "status"} {
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
		"a relative kubectl":                 "kubectl = \"kubectl\"\n",
		"a relative kubeconfig":              "kubeconfig = \"k\"\n",
		"a relative connection file":         "k8s_conn = \"c.json\"\n",
		"the service user as the keys user":  "keys_user = \"ccsbx\"\n",
		"a keys user with a slash":           "keys_user = \"a/b\"\n",
		"a namespace that is not a DNS name": "k8s_namespace = \"Cc_Sandbox\"\n",
	} {
		put(t, p, base+extra, 0o644)
		if _, err := LoadConfig(p, os.Getuid()); err == nil {
			t.Errorf("%s was accepted", why)
		}
	}
	if d := Defaults(); d.KeysUser != "ccsbx-keys" || d.K8sNamespace != "cc-sandbox" || d.Kubectl != "/usr/local/bin/kubectl" {
		t.Errorf("defaults moved: %+v", d)
	}
}

func TestAttachToAPodTakesOnlyAPlausibleTERM(t *testing.T) {
	f, _ := newK8sFx(t)
	f.env["TERM"] = "x;touch /x"
	f.call("attach", sid)
	if len(f.execs) != 1 || !strings.Contains(strings.Join(f.execs[0].env, " "), "TERM=xterm-256color") {
		t.Errorf("a hostile TERM reached kubectl: %v", f.execs)
	}
}
