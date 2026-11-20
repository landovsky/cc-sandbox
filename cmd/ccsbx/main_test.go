package main

import (
	"bytes"
	"strings"
	"testing"
)

// Plan P1's acceptance: `ccsbx run --help` exits 0 and runs nothing — and the same holds for
// every subcommand, so a fresh agent can learn the tool without side effects.
func TestEverySubcommandHelpRunsNothing(t *testing.T) {
	a := app()
	for _, c := range a.Cmds {
		var out, errb bytes.Buffer
		if code := a.Main([]string{c.Name, "--help"}, &out, &errb); code != 0 {
			t.Errorf("%s --help: exit %d", c.Name, code)
		}
		if strings.Contains(errb.String(), "not built yet") {
			t.Errorf("%s --help reached Run", c.Name)
		}
		if !strings.Contains(out.String(), "Flags:") {
			t.Errorf("%s --help has no flag list", c.Name)
		}
	}
}

// A command whose piece has not landed must not look like it worked.
func TestUnbuiltCommandIsNotSuccess(t *testing.T) {
	var out, errb bytes.Buffer
	if code := app().Main([]string{"steering-check"}, &out, &errb); code != exitNotBuilt {
		t.Fatalf("exit %d, want %d", code, exitNotBuilt)
	}
}

// context: the ccsbx tmux server belongs to the service uid (socket 0600). Without ccsbx-root the
// owner cannot attach, and `ccsbx attach` must say exactly which command is the way in and exit 2,
// not exec tmux against a socket it cannot reach.
func TestAttachToAnUnreachableServiceSocketNamesTheCommandAndExits2(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no ccsbx-root, no tmux
	var out, errb bytes.Buffer
	code := app().Main([]string{"attach", "--tmux-socket", "/nonexistent/ccsbx/tmux.sock", "s0abcd1"}, &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "ccsbx-root attach s0abcd1") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
}
