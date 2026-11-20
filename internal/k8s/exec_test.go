// Tests for exec and attach: the kubectl argv (checked with a stub kubectl that records what it
// gets), the exit code passing through, the rebuilt environment, and the checks Backend.Exec makes
// before anything is run. No cluster and no real kubectl are involved.
package k8s

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stubKubectl(t *testing.T, body string) (bin, log string) {
	t.Helper()
	d := t.TempDir()
	log = filepath.Join(d, "log")
	bin = filepath.Join(d, "kubectl")
	os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s\\n' \"$*\" >> "+log+"\nenv >> "+log+"\n"+body+"\n"), 0o755)
	return
}

func TestKubectlExecArgvEnvAndExitCode(t *testing.T) {
	bin, log := stubKubectl(t, `cat; exit 7`)
	t.Setenv("KUBECONFIG", "/home/tomas/.kube/config")
	t.Setenv("HTTPS_PROXY", "http://evil")
	var out bytes.Buffer
	x := Kubectl{Bin: bin, Kubeconfig: "/var/lib/ccsbx-keys/launcher.kubeconfig"}
	code, err := x.Exec(context.Background(), "cc-sandbox", "ccsbx-s-s0abcdefgh-g1", "side", []string{"sh", "-c", "cat >/run/x"},
		IO{Stdin: strings.NewReader("TOKEN\n"), Stdout: &out})
	if err != nil || code != 7 || out.String() != "TOKEN\n" {
		t.Fatalf("%d %v %q", code, err, out.String())
	}
	b, _ := os.ReadFile(log)
	first := strings.SplitN(string(b), "\n", 2)[0]
	if first != "--kubeconfig /var/lib/ccsbx-keys/launcher.kubeconfig -n cc-sandbox exec -i -c side ccsbx-s-s0abcdefgh-g1 -- sh -c cat >/run/x" {
		t.Errorf("kubectl was called as %q", first)
	}
	for _, leaked := range []string{"KUBECONFIG=", "HTTPS_PROXY", "evil", "HOME=/home"} {
		if strings.Contains(string(b), leaked) {
			t.Errorf("the caller's environment reached kubectl (%s)", leaked)
		}
	}
	// a tty exec asks for -t, a failure to start kubectl is an error and not an exit code
	if a := (Kubectl{Kubeconfig: "k"}).argv("n", "p", "fence", []string{"true"}, IO{TTY: true}); strings.Join(a, " ") != "--kubeconfig k -n n exec -t -c fence p -- true" {
		t.Errorf("%v", a)
	}
	if code, err := (Kubectl{Bin: "/nonexistent/kubectl"}).Exec(context.Background(), "n", "p", "fence", []string{"x"}, IO{}); err == nil || code != -1 {
		t.Errorf("%d %v", code, err)
	}
}

type fakeExecer struct{ calls int }

func (f *fakeExecer) Exec(context.Context, string, string, string, []string, IO) (int, error) {
	f.calls++
	return 0, nil
}

func TestBackendExecChecksBeforeRunning(t *testing.T) {
	fx := &fakeExecer{}
	b := NewBackend(nil, fx, DefaultConfig())
	ref := Ref{Namespace: "cc-sandbox", Name: PodName("s0abcdefgh", 1), Sid: "s0abcdefgh", Gen: 1}
	ctx := context.Background()
	if code, err := b.Exec(ctx, ref, "side", []string{"true"}, IO{}); err != nil || code != 0 || fx.calls != 1 {
		t.Fatalf("%d %v", code, err)
	}
	bad := map[string]func() (int, error){
		"another container": func() (int, error) { return b.Exec(ctx, ref, "istio-proxy", []string{"x"}, IO{}) },
		"no command":        func() (int, error) { return b.Exec(ctx, ref, "fence", nil, IO{}) },
		"a NUL":             func() (int, error) { return b.Exec(ctx, ref, "fence", []string{"a\x00b"}, IO{}) },
		"another namespace": func() (int, error) {
			r := ref
			r.Namespace = "kube-system"
			return b.Exec(ctx, r, "fence", []string{"x"}, IO{})
		},
		"a pod name that is not derived": func() (int, error) {
			r := ref
			r.Name = "ccsbx-s-other-g1"
			return b.Exec(ctx, r, "fence", []string{"x"}, IO{})
		},
		"a bad sid": func() (int, error) { r := ref; r.Sid = "../x"; return b.Exec(ctx, r, "fence", []string{"x"}, IO{}) },
	}
	for name, f := range bad {
		if _, err := f(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if fx.calls != 1 {
		t.Errorf("kubectl was run %d times for refused calls", fx.calls-1)
	}
	if _, err := NewBackend(nil, nil, DefaultConfig()).Exec(ctx, ref, "side", []string{"x"}, IO{}); err == nil {
		t.Error("no execer, no error")
	}
}

// F5: the attach command is kubectl exec -it into the fence's tmux by exact pod name.
func TestAttachArgv(t *testing.T) {
	b := NewBackend(nil, nil, DefaultConfig())
	b.Kubectl, b.Kubeconfig = "/usr/local/bin/kubectl", "/var/lib/ccsbx-keys/launcher.kubeconfig"
	ref := Ref{Namespace: "cc-sandbox", Name: PodName("s0abcdefgh", 3), Sid: "s0abcdefgh", Gen: 3}
	got := strings.Join(b.AttachArgv(ref, "s0abcdefgh"), " ")
	want := "/usr/local/bin/kubectl --kubeconfig /var/lib/ccsbx-keys/launcher.kubeconfig -n cc-sandbox exec -it -c fence ccsbx-s-s0abcdefgh-g3 -- tmux attach-session -t =s0abcdefgh:"
	if got != want {
		t.Errorf("%s", got)
	}
}
