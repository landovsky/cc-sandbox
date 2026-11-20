//go:build linux

// Tests for the pty session of a pod attach, with a real pty and no credentials (the test runs as an
// ordinary user): what the child writes is filtered before it reaches the owner's terminal, what the owner
// types reaches the child, the exit status comes back, the drop runs after the child started, and a drop
// that fails kills the child.
package rootcmd

import (
	"bytes"
	"errors"
	"github.com/landovsky/cc-sandbox/internal/termfilter"
	"os/exec"
	"strings"
	"testing"
	"time"
)

type outBuf struct{ bytes.Buffer }

func TestThePtySessionFiltersWhatTheChildWritesAndPassesWhatTheOwnerTypes(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", `printf '\033]52;c;QUJDRA==\007hello \033[1mbold\033[m \033[6n\033]0;lure\007done\n'; read x; echo "got=$x"; exit 3`)
	var out outBuf
	dropped := false
	code, err := runOnPty(ptyRun{Cmd: cmd, In: strings.NewReader("typed\n"), Out: &out,
		Drop: func() error { dropped = true; return nil }, Reset: []byte("<reset>")})
	if err != nil || code != 3 || !dropped {
		t.Fatalf("code %d err %v dropped %v", code, err, dropped)
	}
	s := out.String()
	for _, want := range []string{"hello ", "\x1b[1mbold\x1b[m", "done", "got=typed", "<reset>"} {
		if !strings.Contains(s, want) {
			t.Errorf("%q is missing from %q", want, s)
		}
	}
	for _, bad := range []string{"QUJDRA", "lure", "\x1b]", "\x1b[6n"} {
		if strings.Contains(s, bad) {
			t.Errorf("%q reached the owner's terminal: %q", bad, s)
		}
	}
}

// A drop that cannot happen must not leave a child running with a terminal nobody filters.
func TestAFailedDropKillsTheChildAndSendsNothing(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", `printf 'secret-output'; sleep 30`)
	var out outBuf
	begin := time.Now()
	_, err := runOnPty(ptyRun{Cmd: cmd, In: strings.NewReader(""), Out: &out, Drop: func() error { return errors.New("setuid failed") }})
	if err == nil || time.Since(begin) > 10*time.Second || out.Len() != 0 {
		t.Errorf("err %v after %s, output %q", err, time.Since(begin), out.String())
	}
}

// The child runs in its own session with the pty as its terminal (kubectl needs one), not this process's.
func TestTheChildGetsATerminal(t *testing.T) {
	cmd := exec.Command("/bin/sh", "-c", `[ -t 0 ] && [ -t 1 ] && echo tty-ok`)
	var out outBuf
	if code, err := runOnPty(ptyRun{Cmd: cmd, In: strings.NewReader(""), Out: &out}); err != nil || code != 0 || !strings.Contains(out.String(), "tty-ok") {
		t.Errorf("%d %v %q", code, err, out.String())
	}
}

// context "the agent may have left the owner's terminal in a mode": every private mode the filter passes is
// undone when the session ends (focus reporting, which is not passed, as well).
func TestTheTerminalResetUndoesEveryModeTheFilterPasses(t *testing.T) {
	for _, m := range termfilter.Modes {
		if !strings.Contains(string(terminalReset), "\x1b["+m+"l") && !strings.Contains(string(terminalReset), "\x1b["+m+"h") {
			t.Errorf("mode %s is passed by the filter but not reset", m)
		}
	}
	if !strings.Contains(string(terminalReset), "\x1b[?1004l") || !strings.HasSuffix(string(terminalReset), "\x1b[0m") {
		t.Errorf("%q", terminalReset)
	}
}
