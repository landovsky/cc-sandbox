//go:build linux

// attach_pty_linux.go runs a pod attach: kubectl (as the launch user, started by ccsbx-root itself) on a
// pty, and the owner's terminal served by THIS process after it has dropped to ccsbx-term. Everything the
// pod's tmux writes is the agent's, so it is read here, from the pty master, by a process with no
// credentials, passed through termfilter and only then written to the owner's terminal. The owner's
// keystrokes go the other way unchanged. Plain syscalls only (no cgo, no modules): /dev/ptmx, the
// termios and window-size ioctls.
package rootcmd

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"syscall"
	"unsafe"

	"github.com/landovsky/cc-sandbox/internal/termfilter"
)

// terminalReset puts the owner's terminal back to what a full-screen program may have changed: leave the
// alternate screen, show the cursor, stop bracketed paste, reset attributes.
var terminalReset = []byte("\x1b[?1049l\x1b[?25h\x1b[?2004l\x1b[0m")

// ptyRun is one session on a pty.
type ptyRun struct {
	Cmd   *exec.Cmd    // stdio is set here; SysProcAttr.Credential may be set by the caller
	In    io.Reader    // the owner's keystrokes
	Out   io.Writer    // the owner's terminal
	Tty   *os.File     // the owner's terminal, for raw mode and window size; nil when In and Out are not one
	Drop  func() error // run once the child has started: becomes the credential-less user
	Reset []byte       // written to Out at the end
}

func ioctl(fd uintptr, req uintptr, arg unsafe.Pointer) error {
	if _, _, e := syscall.Syscall(syscall.SYS_IOCTL, fd, req, uintptr(arg)); e != 0 {
		return e
	}
	return nil
}

// openPty opens a master and its slave. Neither becomes this process's controlling terminal.
func openPty() (master, slave *os.File, err error) {
	m, err := os.OpenFile("/dev/ptmx", os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		return nil, nil, err
	}
	var unlock int32
	if err := ioctl(m.Fd(), syscall.TIOCSPTLCK, unsafe.Pointer(&unlock)); err != nil {
		m.Close()
		return nil, nil, err
	}
	var n uint32
	if err := ioctl(m.Fd(), syscall.TIOCGPTN, unsafe.Pointer(&n)); err != nil {
		m.Close()
		return nil, nil, err
	}
	s, err := os.OpenFile("/dev/pts/"+itoa(int(n)), os.O_RDWR|syscall.O_NOCTTY, 0)
	if err != nil {
		m.Close()
		return nil, nil, err
	}
	return m, s, nil
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [12]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}

func isTerminal(f *os.File) bool {
	var t syscall.Termios
	return ioctl(f.Fd(), syscall.TCGETS, unsafe.Pointer(&t)) == nil
}

// makeRaw puts the owner's terminal in raw mode and returns the function that restores it.
func makeRaw(f *os.File) (restore func(), err error) {
	var old syscall.Termios
	if err := ioctl(f.Fd(), syscall.TCGETS, unsafe.Pointer(&old)); err != nil {
		return nil, err
	}
	raw := old
	raw.Iflag &^= syscall.IGNBRK | syscall.BRKINT | syscall.PARMRK | syscall.ISTRIP | syscall.INLCR | syscall.IGNCR | syscall.ICRNL | syscall.IXON
	raw.Oflag &^= syscall.OPOST
	raw.Lflag &^= syscall.ECHO | syscall.ECHONL | syscall.ICANON | syscall.ISIG | syscall.IEXTEN
	raw.Cflag &^= syscall.CSIZE | syscall.PARENB
	raw.Cflag |= syscall.CS8
	raw.Cc[syscall.VMIN], raw.Cc[syscall.VTIME] = 1, 0
	if err := ioctl(f.Fd(), syscall.TCSETS, unsafe.Pointer(&raw)); err != nil {
		return nil, err
	}
	return func() { ioctl(f.Fd(), syscall.TCSETS, unsafe.Pointer(&old)) }, nil
}

type winsize struct{ Row, Col, X, Y uint16 }

func copyWinsize(from, to *os.File) {
	var ws winsize
	if ioctl(from.Fd(), syscall.TIOCGWINSZ, unsafe.Pointer(&ws)) == nil {
		ioctl(to.Fd(), syscall.TIOCSWINSZ, unsafe.Pointer(&ws))
	}
}

// runOnPty starts r.Cmd on a new pty, drops to the credential-less user, and serves the owner's terminal
// until the child ends. It returns the child's exit status.
func runOnPty(r ptyRun) (int, error) {
	m, s, err := openPty()
	if err != nil {
		return 0, err
	}
	defer m.Close()
	if r.Tty != nil && isTerminal(r.Tty) {
		restore, err := makeRaw(r.Tty)
		if err != nil {
			s.Close()
			return 0, err
		}
		defer restore()
		copyWinsize(r.Tty, m)
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, syscall.SIGWINCH)
		defer signal.Stop(ch)
		go func() {
			for range ch {
				copyWinsize(r.Tty, m)
			}
		}()
	}
	r.Cmd.Stdin, r.Cmd.Stdout, r.Cmd.Stderr = s, s, s
	if r.Cmd.SysProcAttr == nil {
		r.Cmd.SysProcAttr = &syscall.SysProcAttr{}
	}
	r.Cmd.SysProcAttr.Setsid, r.Cmd.SysProcAttr.Setctty, r.Cmd.SysProcAttr.Ctty = true, true, 0
	if err := r.Cmd.Start(); err != nil {
		s.Close()
		return 0, err
	}
	s.Close() // so the master sees the end when the child goes
	if r.Drop != nil {
		if err := r.Drop(); err != nil {
			r.Cmd.Process.Kill() // still root here: it can
			r.Cmd.Wait()
			return 0, err
		}
	}
	go io.Copy(m, r.In) // the owner's keystrokes, unchanged
	var f termfilter.Filter
	buf := make([]byte, 32<<10)
	for {
		n, err := m.Read(buf)
		if n > 0 {
			if _, werr := r.Out.Write(f.Write(buf[:n])); werr != nil {
				break
			}
		}
		if err != nil {
			break // EIO once the child has gone: the end
		}
	}
	code := 0
	if err := r.Cmd.Wait(); err != nil {
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			code = ee.ExitCode()
		} else {
			return 0, err
		}
	}
	if len(r.Reset) > 0 {
		r.Out.Write(r.Reset)
	}
	return code, nil
}

// realAttachSession is the production AttachSession.
func realAttachSession(s AttachSpec) (int, error) {
	cmd := exec.Command(s.Self, s.Args...)
	cmd.Env = s.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: &syscall.Credential{Uid: uint32(s.LaunchUID), Gid: uint32(s.LaunchGID), Groups: []uint32{}}}
	return runOnPty(ptyRun{Cmd: cmd, In: os.Stdin, Out: os.Stdout, Tty: os.Stdin,
		Drop: func() error { return dropPrivs(s.TermUID, s.TermGID) }, Reset: terminalReset})
}
