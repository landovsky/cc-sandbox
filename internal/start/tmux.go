package start

import (
	"fmt"
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
}

// base is the fixed start of every tmux call: this server's socket and an empty config file, so
// nothing from the user's ~/.tmux.conf (bindings, plugins, run-shell lines) runs in the ccsbx server.
func (t *TmuxLauncher) base() []string { return []string{"-L", t.Socket, "-f", "/dev/null"} }

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
	args := []string{"new-session", "-d", "-s", r.Sid, "-c", r.Spec.Workdir.Path, t.Exe, "run", "--sid", r.Sid, "--gen", strconv.Itoa(r.Gen)}
	args = append(args, t.Env...)
	args = append(args, ";", "set-option", "-g", "set-clipboard", "off", ";", "set-option", "-g", "allow-passthrough", "off",
		// A pane is a window onto a fence, not a tmux session to drive: no status line, no prefix
		// key (so a keystroke never reaches tmux's own command mode), no escape delay.
		";", "set-option", "-g", "status", "off", ";", "set-option", "-g", "prefix", "None", ";", "set-option", "-g", "escape-time", "0")
	if out, err := t.cmd(args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session: %v: %s", err, out)
	}
	return nil
}

// Alive reports whether the session's tmux session exists.
func (t *TmuxLauncher) Alive(sid string) bool {
	return t.cmd("has-session", "-t", "="+sid).Run() == nil
}
