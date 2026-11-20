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
