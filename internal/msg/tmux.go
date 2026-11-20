package msg

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// ExecTimeout bounds every tmux and ccmsg call. A wedged tmux server or a hung hub must not
// stop the daemon's scan loop (invariant 16), so nothing here waits forever.
var ExecTimeout = 5 * time.Second

// runCmd runs c, killing it after ExecTimeout, and returns its combined output.
func runCmd(c *exec.Cmd) ([]byte, error) {
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
	case <-time.After(ExecTimeout):
		_ = c.Process.Kill()
		<-done
		return buf.Bytes(), fmt.Errorf("timed out after %s", ExecTimeout)
	}
}

// Tmux delivers into the pane of session `sid` on `tmux -L Socket`. It is the only delivery on
// the Mac and the fallback on Linux until the native inbox (C0b, needs root). The router calls
// it only for a pane that is at its input prompt.
type Tmux struct {
	Socket  string
	Command func(name string, args ...string) *exec.Cmd // tests replace it
	// Stdin lets a test feed the buffer; production uses `load-buffer -` on stdin.
}

func (t *Tmux) run(args ...string) ([]byte, error) { return t.runIn("", args...) }

func (t *Tmux) runIn(stdin string, args ...string) ([]byte, error) {
	a := append([]string{"-L", t.Socket}, args...)
	var c *exec.Cmd
	if t.Command != nil {
		c = t.Command("tmux", a...)
	} else {
		c = exec.Command("tmux", a...)
	}
	if stdin != "" {
		c.Stdin = strings.NewReader(stdin)
	}
	return runCmd(c)
}

// Alive reports whether the tmux session exists.
func (t *Tmux) Alive(sid string) bool {
	_, err := t.run("has-session", "-t", "="+sid)
	return err == nil
}

// Deliver pastes line with a bracketed paste (-p: if the application asked for bracketed paste
// the text arrives inside ESC[200~ … ESC[201~, so it is data, not keystrokes), in a buffer of
// its own that is deleted again (-d), then presses Enter. The target is "=sid:", an exact
// session name, so a sid can't prefix-match another.
func (t *Tmux) Deliver(sid, line string) error {
	buf := "ccsbx-msg-" + sid
	if out, err := t.runIn(line, "load-buffer", "-b", buf, "-"); err != nil {
		return fmt.Errorf("load-buffer: %v: %s", err, out)
	}
	if out, err := t.run("paste-buffer", "-d", "-p", "-b", buf, "-t", "="+sid+":"); err != nil {
		return fmt.Errorf("paste-buffer: %v: %s", err, out)
	}
	if out, err := t.run("send-keys", "-t", "="+sid+":", "Enter"); err != nil {
		return fmt.Errorf("send-keys Enter: %v: %s", err, out)
	}
	return nil
}

// CcmsgHub reaches an unfenced session through the session bus's ccmsg (direct mode: the daemon
// runs as the same user). node is "tree:id"; ccmsg wants "tree:#id".
type CcmsgHub struct {
	Bin     string
	Command func(name string, args ...string) *exec.Cmd
}

func (h CcmsgHub) Send(node, line string) error {
	addr := node
	if t, id, ok := strings.Cut(node, ":"); ok && !strings.HasPrefix(id, "#") {
		addr = t + ":#" + id
	}
	args := []string{"--kind", "question", addr, line}
	var c *exec.Cmd
	if h.Command != nil {
		c = h.Command(h.Bin, args...)
	} else {
		c = exec.Command(h.Bin, args...)
	}
	if out, err := runCmd(c); err != nil {
		return fmt.Errorf("ccmsg: %v: %s", err, out)
	}
	return nil
}
