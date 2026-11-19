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
	if code := app().Main([]string{"start"}, &out, &errb); code != exitNotBuilt {
		t.Fatalf("exit %d, want %d", code, exitNotBuilt)
	}
}
