// Package tmuxsock turns the --tmux-socket setting into tmux's own flag. It exists because three
// programs (hostd's delivery, the pane launcher, `ccsbx attach`) must address the same tmux server,
// and once hostd runs as a service uid that server is reached through a shared socket PATH, not a
// per-user -L name: a bare name means `tmux -L name`, an absolute path means `tmux -S path`.
package tmuxsock

import (
	"bytes"
	"fmt"
	"os/exec"
	"path/filepath"
	"time"
)

// Timeout bounds every tmux call the daemon makes (a wedged tmux server must not stall it).
var Timeout = 5 * time.Second

// Run runs c, killing it after Timeout, and returns its combined output.
func Run(c *exec.Cmd) ([]byte, error) { return RunFor(c, Timeout) }

// RunFor is Run with an explicit timeout.
func RunFor(c *exec.Cmd, d time.Duration) ([]byte, error) {
	var buf bytes.Buffer
	c.Stdout, c.Stderr = &buf, &buf
	if err := c.Start(); err != nil {
		return nil, err
	}
	done := make(chan error, 1)
	go func() { done <- c.Wait() }()
	select {
	case err := <-done:
		return buf.Bytes(), err
	case <-time.After(d):
		_ = c.Process.Kill()
		<-done
		return buf.Bytes(), fmt.Errorf("timed out after %s", d)
	}
}

// Args is the tmux flag pair that selects the server: ["-S", path] for an absolute path,
// ["-L", name] otherwise.
func Args(sock string) []string {
	if filepath.IsAbs(sock) {
		return []string{"-S", sock}
	}
	return []string{"-L", sock}
}

// IsPath reports whether sock is a socket path (shared between uids) rather than a -L name.
func IsPath(sock string) bool { return filepath.IsAbs(sock) }
