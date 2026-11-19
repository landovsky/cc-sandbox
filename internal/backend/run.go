package backend

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
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
