// run.go runs a Spec through docker and returns the container's exit code. It exists so the
// backend has one place that shells out, which the tests can replace with a fake docker.

package backend

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
)

// Runner runs a generation: prepare dirs, create the internal network, run the container in the
// foreground, and always clean up the container and network it created ("/exit leaves no
// container"). Docker is called as the current user (direct mode).
type Runner struct {
	Docker string // docker binary, default "docker"
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
	// Files written into the generation before the container starts (relative to GenDir).
	Files map[string][]byte
}

func (r *Runner) docker() string {
	if r.Docker != "" {
		return r.Docker
	}
	return "docker"
}

// Run returns the container's exit code (or -1 with an error if it never ran).
func (r *Runner) Run(s Spec) (int, error) {
	if err := s.Prepare(); err != nil {
		return -1, err
	}
	for name, b := range r.Files {
		p := filepath.Join(s.GenDir(), name)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			return -1, err
		}
		if err := os.WriteFile(p, b, 0o644); err != nil {
			return -1, err
		}
	}
	defer s.Close()

	// Fail closed: if the home can't be swept, the generation doesn't start.
	if out, err := exec.Command(r.docker(), s.SweepArgs()...).CombinedOutput(); err != nil {
		return -1, fmt.Errorf("sweep home: %v: %s", err, out)
	} else if m := moved(string(out)); len(m) > 0 && r.Stderr != nil {
		fmt.Fprintf(r.Stderr, "ccsbx run: moved %d steering file(s) out of the home to %s: %s\n", len(m), s.QuarantineDir(), strings.Join(m, " "))
	}

	if out, err := exec.Command(r.docker(), s.NetworkArgs()...).CombinedOutput(); err != nil {
		return -1, fmt.Errorf("docker network create: %v: %s", err, out)
	}
	// Cleanup is by the exact names derived from sid and gen, nothing else is ever touched.
	cleanup := func() {
		_ = exec.Command(r.docker(), "rm", "-f", s.Container()).Run()
		_ = exec.Command(r.docker(), "network", "rm", s.Network()).Run()
	}
	defer cleanup()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
	defer stop()
	c := exec.CommandContext(ctx, r.docker(), s.RunArgs()...)
	c.Stdin, c.Stdout, c.Stderr = r.Stdin, r.Stdout, r.Stderr
	err := c.Run()
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), nil
	}
	if err != nil {
		return -1, err
	}
	return 0, nil
}
