// tmux.go is the launcher that puts a session in its own tmux window. It exists so a started session
// is visible and attachable, with the tmux server hardened in the same invocation (U2).

package start

import (
	"fmt"
	"github.com/landovsky/cc-sandbox/internal/tmuxsock"
	"os"
	"os/exec"
	"strconv"
)

// TmuxLauncher runs the pane in `tmux -L ccsbx` as the calling user (direct mode). On Linux with
// root enabled the server will run under uid ccsbx instead (gate M).
type TmuxLauncher struct {
	Exe     string   // the ccsbx binary
	Socket  string   // tmux -L name, "ccsbx"
	Env     []string // extra args after `run`, e.g. --state, --profiles, so run finds the same state
	Command func(name string, args ...string) *exec.Cmd
	// NoSpawn: connect to a tmux server that is already running and never start one. hostd sets it:
	// the server is ccsbx-tmux.service's, in the host mount namespace, and a server spawned by a
	// `new-session` from inside hostd's sandbox could not run ccsbx-root properly.
	NoSpawn bool
	// Helper: the pane runs `sudo -n ccsbx-root run --tty <sid>` (no docker for the caller) instead
	// of `ccsbx run`. Env is not used then: ccsbx-root takes everything from the daemon's record.
	Helper bool
}

// paneCmd is what the pane runs.
func (t *TmuxLauncher) paneCmd(r *Record) []string {
	if t.Helper {
		return []string{SudoBin, "-n", RootBin, "run", "--tty", r.Sid}
	}
	return append([]string{t.Exe, "run", "--sid", r.Sid, "--gen", strconv.Itoa(r.Gen)}, t.Env...)
}

// base is the fixed start of every tmux call: this server's socket and an empty config file, so
// nothing from the user's ~/.tmux.conf (bindings, plugins, run-shell lines) runs in the ccsbx server.
// An absolute socket is a shared path (-S), a bare name is a per-user server (-L): see tmuxsock.
func (t *TmuxLauncher) base() []string { return append(tmuxsock.Args(t.Socket), "-f", "/dev/null") }

func (t *TmuxLauncher) cmd(args ...string) *exec.Cmd {
	if t.Command != nil {
		return t.Command("tmux", append(t.base(), args...)...)
	}
	return exec.Command("tmux", append(t.base(), args...)...)
}

// Launch creates a detached session named after the sid and hardens the server in the same tmux
// invocation: no clipboard writes and no passthrough escape sequences from the pane (a fenced
// agent controls the pane's output). One invocation, because a pane that exits at once takes
// the server with it, and a second call would find no server.
func (t *TmuxLauncher) Launch(r *Record) error {
	if t.NoSpawn {
		// `new-session` starts a server when there is none, so prove one is up first.
		out, err := tmuxsock.Run(t.cmd("list-sessions"))
		if !tmuxsock.Up(out, err) {
			return fmt.Errorf("%s (socket %s)", tmuxsock.ErrNoServer, t.Socket)
		}
	}
	args := append([]string{"new-session", "-d", "-s", r.Sid, "-c", r.Spec.Workdir.Path}, t.paneCmd(r)...)
	args = append(args, ";", "set-option", "-g", "set-clipboard", "off", ";", "set-option", "-g", "allow-passthrough", "off",
		// A pane is a window onto a fence, not a tmux session to drive: no status line, no prefix
		// key (so a keystroke never reaches tmux's own command mode), no escape delay.
		";", "set-option", "-g", "status", "off", ";", "set-option", "-g", "prefix", "None", ";", "set-option", "-g", "escape-time", "0")
	if out, err := tmuxsock.Run(t.cmd(args...)); err != nil {
		return fmt.Errorf("tmux new-session: %v: %s", err, out)
	}
	// The socket stays as tmux made it: 0600, owned by this uid. It is NOT opened to a group: a
	// group-writable tmux socket would let any process of a group member type into fenced panes
	// and run commands as this uid, undoing the privilege split. The owner reaches a pane
	// through `ccsbx-root attach <sid>` instead.
	if tmuxsock.IsPath(t.Socket) {
		if st, err := os.Stat(t.Socket); err != nil || st.Mode().Perm()&0o077 != 0 {
			_ = t.Stop(r.Sid)
			return fmt.Errorf("tmux socket %s is missing or readable by others (session ended): %v", t.Socket, err)
		}
	}
	return nil
}

// Stop ends the session's pane (and so its fence run: `ccsbx run` cleans its container up).
func (t *TmuxLauncher) Stop(sid string) error {
	_, err := tmuxsock.Run(t.cmd("kill-session", "-t", "="+sid))
	return err
}

// Alive reports whether the session's tmux session exists.
func (t *TmuxLauncher) Alive(sid string) bool {
	_, err := tmuxsock.Run(t.cmd("has-session", "-t", "="+sid))
	return err == nil
}
