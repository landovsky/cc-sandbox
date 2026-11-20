// exec.go runs a command in a pod container through kubectl, because exec needs the SPDY/WebSocket
// upgrade that the plain REST client does not do. kubectl is run by absolute path with a rebuilt
// environment (no inherited KUBECONFIG, HOME or PATH), and the only inputs are a namespace, an exact
// pod name, a container and an argv, each validated before it gets here.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Kubectl is the production Execer.
type Kubectl struct {
	Bin        string // absolute path of kubectl
	Kubeconfig string // the launcher kubeconfig (under ccsbx-keys)
}

func (k Kubectl) argv(ns, pod, container string, cmd []string, io IO) []string {
	a := []string{"--kubeconfig", k.Kubeconfig, "-n", ns, "exec"}
	if io.Stdin != nil {
		a = append(a, "-i")
	}
	if io.TTY {
		a = append(a, "-t")
	}
	a = append(a, "-c", container, pod, "--")
	return append(a, cmd...)
}

// NewKubectl checks the two paths once; Exec checks them again on every call, so a Kubectl made as a
// struct literal is held to the same rule.
func NewKubectl(bin, kubeconfig string) (Kubectl, error) {
	k := Kubectl{Bin: bin, Kubeconfig: kubeconfig}
	return k, k.check()
}

// check refuses what would make kubectl fall back to its defaults. With no kubeconfig kubectl talks
// plain HTTP to localhost:8080, where any local process may listen, and the secrets this Execer
// carries on stdin (the session token, the generation key) would go to it; a relative binary is
// looked up in the CALLER's PATH, not the rebuilt one.
func (k Kubectl) check() error {
	if !filepath.IsAbs(k.Bin) || filepath.Clean(k.Bin) != k.Bin {
		return fmt.Errorf("kubectl binary %q must be a clean absolute path", k.Bin)
	}
	if !filepath.IsAbs(k.Kubeconfig) || filepath.Clean(k.Kubeconfig) != k.Kubeconfig {
		return fmt.Errorf("kubeconfig %q must be a clean absolute path", k.Kubeconfig)
	}
	if st, err := os.Stat(k.Kubeconfig); err != nil || !st.Mode().IsRegular() {
		return fmt.Errorf("kubeconfig %s is not a readable regular file", k.Kubeconfig)
	}
	return nil
}

// Exec implements Execer. Nothing is run, and stdin is never read, unless the paths check out.
func (k Kubectl) Exec(ctx context.Context, ns, pod, container string, cmd []string, io IO) (int, error) {
	if err := k.check(); err != nil {
		return -1, err
	}
	c := exec.CommandContext(ctx, k.Bin, k.argv(ns, pod, container, cmd, io)...)
	c.Env = []string{"PATH=/usr/bin:/bin", "HOME=/nonexistent", "LANG=C.UTF-8"}
	c.Stdin, c.Stdout, c.Stderr = io.Stdin, io.Stdout, io.Stderr
	err := c.Run()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return 0, nil
	case errors.As(err, &ee):
		return ee.ExitCode(), nil
	}
	return -1, err
}
