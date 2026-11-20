// exec.go runs a command in a pod container through kubectl, because exec needs the SPDY/WebSocket
// upgrade that the plain REST client does not do. kubectl is run by absolute path with a rebuilt
// environment (no inherited KUBECONFIG, HOME or PATH), and the only inputs are a namespace, an exact
// pod name, a container and an argv, each validated before it gets here.
package k8s

import (
	"context"
	"errors"
	"os/exec"
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

// Exec implements Execer.
func (k Kubectl) Exec(ctx context.Context, ns, pod, container string, cmd []string, io IO) (int, error) {
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
