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
	"strings"
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

// ErrNoServer is what the daemon says when the tmux server is not there. hostd never starts the
// server (it lives in ccsbx-tmux.service, in the host mount namespace, so that panes can run
// ccsbx-root); it only connects to it.
const ErrNoServer = "tmux server not running: start ccsbx-tmux.service"

// Up reads the result of a `tmux list-sessions`: true if a server answered. A server that is up
// with no session says "no sessions" (rc 1); no server says "no server running" or "error
// connecting to <socket>". Anything else is not taken as proof of a server.
func Up(out []byte, err error) bool {
	if err == nil {
		return true
	}
	return strings.Contains(string(out), "no sessions")
}

// NoServer reports whether a failed tmux call failed because there is no server to talk to.
func NoServer(out []byte) bool {
	s := string(out)
	return strings.Contains(s, "no server running") || strings.Contains(s, "error connecting to")
}

// IsPath reports whether sock is a socket path (shared between uids) rather than a -L name.
func IsPath(sock string) bool { return filepath.IsAbs(sock) }
