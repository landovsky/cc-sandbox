package main

import (
	"bytes"
	"os"
	"path/filepath"
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

// H1-N3: steering-check run in a fenced session's worktree must not trust that worktree's own
// .git (the agent can write it): it refuses and asks for the host-owned --gitdir.
func TestSteeringCheckRefusesFencedWorktreeFallback(t *testing.T) {
	state, wt := t.TempDir(), t.TempDir()
	os.MkdirAll(filepath.Join(state, "sessions"), 0o700)
	os.Mkdir(filepath.Join(wt, ".git"), 0o755)
	rec := `{"sid":"s123456abc","spec":{"workdir":{"path":"` + wt + `"}}}`
	os.WriteFile(filepath.Join(state, "sessions", "s123456abc.json"), []byte(rec), 0o600)
	var out, errb bytes.Buffer
	if code := app().Main([]string{"steering-check", "--state", state, "--worktree", wt}, &out, &errb); code != 2 || !strings.Contains(errb.String(), "s123456abc") {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	// a developer's own checkout (no record) is not refused by this rule
	other := t.TempDir()
	os.Mkdir(filepath.Join(other, ".git"), 0o755)
	errb.Reset()
	app().Main([]string{"steering-check", "--state", state, "--worktree", other}, &out, &errb)
	if strings.Contains(errb.String(), "worktree of session") {
		t.Fatalf("an unrelated checkout was refused: %s", errb.String())
	}
}
