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
	// SessionsDir is where session records live when that is not <stateDir>/sessions (gc only).
	SessionsDir string
	// Stage, when set, runs after the directories exist and before anything is mounted: it makes
	// the host paths docker will mount (Spec.MountWorkdir, MountGitDir) and returns what undoes it.
	Stage func(s Spec) (undo func(), err error)
	// Unstage undoes a staging left behind by a run that died; Stop and Reap call it.
	Unstage func(s Spec)
	Stdin   io.Reader
	Stdout  io.Writer
	Stderr  io.Writer
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
	if err := s.writePID(); err != nil {
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
	if r.Stage != nil {
		undo, err := r.Stage(s)
		if err != nil {
			return -1, fmt.Errorf("stage mounts: %w", err)
		}
		defer undo()
	}

	// Fail closed: if the home can't be swept, the generation doesn't start.
	var swept []string
	if s.NotEvid {
		out, err := exec.Command(r.docker(), s.SweepArgs()...).CombinedOutput()
		if err != nil {
			return -1, fmt.Errorf("sweep home: %v: %s", err, out)
		}
		swept = moved(string(out))
	} else {
		var err error
		if swept, err = s.sweepHomeGo(); err != nil {
			return -1, fmt.Errorf("sweep home: %w", err)
		}
	}
	if len(swept) > 0 && r.Stderr != nil {
		fmt.Fprintf(r.Stderr, "ccsbx run: moved %d steering file(s) out of the home to %s: %s\n", len(swept), s.QuarantineDir(), strings.Join(swept, " "))
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

// Stop removes one generation's container and network by the exact names derived from its sid and
// gen, and nothing else. A missing container or network is not an error: stop is idempotent.
func (r *Runner) Stop(s Spec) error {
	if r.Unstage != nil {
		defer r.Unstage(s)
	}
	var errs []string
	for _, a := range [][]string{{"rm", "-f", s.Container()}, {"network", "rm", s.Network()}} {
		if out, err := exec.Command(r.docker(), a...).CombinedOutput(); err != nil && !strings.Contains(string(out), "No such") {
			errs = append(errs, fmt.Sprintf("docker %s: %v: %s", a[0], err, strings.TrimSpace(string(out))))
		}
	}
	if len(errs) > 0 {
		return fmt.Errorf("%s", strings.Join(errs, "; "))
	}
	return nil
}
