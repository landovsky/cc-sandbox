package msg

import (
	"fmt"
	"os/exec"
)

// Tmux delivers by send-keys into the pane of session `sid` on `tmux -L Socket`. It is the
// only delivery on the Mac and the fallback on Linux until the native inbox (C0b, needs root).
type Tmux struct {
	Socket  string
	Command func(name string, args ...string) *exec.Cmd // tests replace it
}

func (t *Tmux) run(args ...string) ([]byte, error) {
	a := append([]string{"-L", t.Socket}, args...)
	if t.Command != nil {
		return t.Command("tmux", a...).CombinedOutput()
	}
	return exec.Command("tmux", a...).CombinedOutput()
}

// Alive reports whether the tmux session exists.
func (t *Tmux) Alive(sid string) bool {
	_, err := t.run("has-session", "-t", "="+sid)
	return err == nil
}

// Deliver types line literally (-l: no key-name interpretation) and presses Enter. The target
// is "=sid:" (session, exact name; the colon makes it a pane target) for an exact session name, so a sid can't prefix-match another.
func (t *Tmux) Deliver(sid, line string) error {
	if out, err := t.run("send-keys", "-t", "="+sid+":", "-l", "--", line); err != nil {
		return fmt.Errorf("send-keys: %v: %s", err, out)
	}
	if out, err := t.run("send-keys", "-t", "="+sid+":", "Enter"); err != nil {
		return fmt.Errorf("send-keys Enter: %v: %s", err, out)
	}
	return nil
}
