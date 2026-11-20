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
}

func (t *TmuxLauncher) cmd(args ...string) *exec.Cmd {
	if t.Command != nil {
		return t.Command("tmux", append(tmuxsock.Args(t.Socket), args...)...)
	}
	return exec.Command("tmux", append(tmuxsock.Args(t.Socket), args...)...)
}

// Launch creates a detached session named after the sid and hardens the server in the same tmux
// invocation: no clipboard writes and no passthrough escape sequences from the pane (a fenced
// agent controls the pane's output). One invocation, because a pane that exits at once takes
// the server with it, and a second call would find no server.
func (t *TmuxLauncher) Launch(r *Record) error {
	args := []string{"new-session", "-d", "-s", r.Sid, "-c", r.Spec.Workdir.Path, t.Exe, "run", "--sid", r.Sid, "--gen", strconv.Itoa(r.Gen)}
	args = append(args, t.Env...)
	args = append(args, ";", "set-option", "-g", "set-clipboard", "off", ";", "set-option", "-g", "allow-passthrough", "off")
	if out, err := t.cmd(args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux new-session: %v: %s", err, out)
	}
	// A shared socket path (the service uid's tmux, reached by the owner's uid through a group):
	// tmux creates it 0600, so make it 0660 ourselves. Fail closed: if that can't be done the
	// session is ended again rather than left on a socket nobody in the group can reach, or one
	// with the wrong mode.
	if tmuxsock.IsPath(t.Socket) {
		if err := os.Chmod(t.Socket, 0o660); err != nil {
			_ = t.Stop(r.Sid)
			return fmt.Errorf("tmux socket %s: cannot set mode 0660 (session ended): %v", t.Socket, err)
		}
	}
	return nil
}

// Stop ends the session's pane (and so its fence run: `ccsbx run` cleans its container up).
func (t *TmuxLauncher) Stop(sid string) error {
	return t.cmd("kill-session", "-t", "="+sid).Run()
}

// Alive reports whether the session's tmux session exists.
func (t *TmuxLauncher) Alive(sid string) bool {
	return t.cmd("has-session", "-t", "="+sid).Run() == nil
}
