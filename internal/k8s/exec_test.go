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
	kc := filepath.Join(t.TempDir(), "launcher.kubeconfig")
	os.WriteFile(kc, []byte("apiVersion: v1\n"), 0o600)
	x, err := NewKubectl(bin, kc)
	if err != nil {
		t.Fatal(err)
	}
	code, err := x.Exec(context.Background(), "cc-sandbox", "ccsbx-s-s0abcdefgh-g1", "side", []string{"sh", "-c", "cat >/run/x"},
		IO{Stdin: strings.NewReader("TOKEN\n"), Stdout: &out})
	if err != nil || code != 7 || out.String() != "TOKEN\n" {
		t.Fatalf("%d %v %q", code, err, out.String())
	}
	b, _ := os.ReadFile(log)
	first := strings.SplitN(string(b), "\n", 2)[0]
	if first != "--kubeconfig "+kc+" -n cc-sandbox exec -i -c side ccsbx-s-s0abcdefgh-g1 -- sh -c cat >/run/x" {
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

// M2: with no usable kubeconfig kubectl falls back to plain HTTP on localhost:8080, where any local
// process may listen, and the secrets that travel on stdin would go to it. Nothing is run, and stdin
// is not read, unless both paths are absolute and the kubeconfig is a regular file.
func TestKubectlRefusesUnusablePathsBeforeRunningAnything(t *testing.T) {
	bin, log := stubKubectl(t, `cat`)
	kc := filepath.Join(t.TempDir(), "kc")
	os.WriteFile(kc, []byte("x"), 0o600)
	dir := t.TempDir()
	for name, k := range map[string]Kubectl{
		"an empty kubeconfig":        {Bin: bin},
		"a relative kubeconfig":      {Bin: bin, Kubeconfig: "kubeconfig"},
		"a missing kubeconfig":       {Bin: bin, Kubeconfig: filepath.Join(dir, "nope")},
		"a kubeconfig that is a dir": {Bin: bin, Kubeconfig: dir},
		"a relative binary":          {Bin: "kubectl", Kubeconfig: kc},
		"an empty binary":            {Kubeconfig: kc},
		"an unclean binary path":     {Bin: filepath.Dir(bin) + "/../" + filepath.Base(filepath.Dir(bin)) + "/kubectl", Kubeconfig: kc},
	} {
		in := &readCounter{r: strings.NewReader("TOKEN")}
		code, err := k.Exec(context.Background(), "cc-sandbox", "ccsbx-s-s0abcdefgh-g1", "side", []string{"cat"}, IO{Stdin: in})
		if err == nil || code != -1 || in.n != 0 {
			t.Errorf("%s: %d %v (stdin read %d bytes)", name, code, err, in.n)
		}
		if _, err := NewKubectl(k.Bin, k.Kubeconfig); err == nil {
			t.Errorf("%s: NewKubectl accepted it", name)
		}
	}
	// a relative kubeconfig that happens to exist in the working directory is still refused
	wd := t.TempDir()
	os.WriteFile(filepath.Join(wd, "kubeconfig"), []byte("x"), 0o600)
	old, _ := os.Getwd()
	os.Chdir(wd)
	defer os.Chdir(old)
	if _, err := NewKubectl(bin, "kubeconfig"); err == nil || !strings.Contains(err.Error(), "absolute") {
		t.Errorf("a relative kubeconfig that exists was accepted: %v", err)
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("kubectl was run for a refused configuration")
	}
}

type readCounter struct {
	r *strings.Reader
	n int
}

func (c *readCounter) Read(p []byte) (int, error) { n, err := c.r.Read(p); c.n += n; return n, err }

// F5 / S1: the attach command is derived from the sid and generation, never from a caller's pod
// name, and is refused while kubectl or the kubeconfig is not an absolute path.
func TestAttachArgv(t *testing.T) {
	b := NewBackend(nil, nil, DefaultConfig())
	b.Kubectl, b.Kubeconfig = "/usr/local/bin/kubectl", "/var/lib/ccsbx-keys/launcher.kubeconfig"
	got, err := b.AttachArgv("s0abcdefgh", 3)
	want := "/usr/local/bin/kubectl --kubeconfig /var/lib/ccsbx-keys/launcher.kubeconfig -n cc-sandbox exec -it -c fence ccsbx-s-s0abcdefgh-g3 -- tmux attach-session -t =s0abcdefgh:"
	if err != nil || strings.Join(got, " ") != want {
		t.Errorf("%v %v", got, err)
	}
	for _, c := range []struct {
		sid string
		gen int
	}{{"-n", 1}, {"../x", 1}, {"S0ABCDEFGH", 1}, {"s0abcdefgh", 0}, {"s0abcdefgh", 10000}, {"", 1}} {
		if a, err := b.AttachArgv(c.sid, c.gen); err == nil {
			t.Errorf("(%q, %d) gave %v", c.sid, c.gen, a)
		}
	}
	for name, mod := range map[string]func(*Backend){
		"no kubectl":         func(b *Backend) { b.Kubectl = "" },
		"a relative kubectl": func(b *Backend) { b.Kubectl = "kubectl" },
		"no kubeconfig":      func(b *Backend) { b.Kubeconfig = "" },
		"a relative one":     func(b *Backend) { b.Kubeconfig = "kc" },
	} {
		b2 := *b
		mod(&b2)
		if a, err := b2.AttachArgv("s0abcdefgh", 1); err == nil {
			t.Errorf("%s gave %v", name, a)
		}
	}
}
