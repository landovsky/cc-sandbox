// Tests for how `ccsbx attach` reaches the root door. The door refuses to run unless it is root,
// so in helper mode the owner's command goes through sudo, by absolute path and not through the
// caller's PATH, whatever tmux socket is named, with a rebuilt environment and only the sid.
package main

import (
	"strings"
	"testing"

	"github.com/landovsky/cc-sandbox/internal/start"
)

func TestAttachGoesThroughSudoToTheRootDoor(t *testing.T) {
	newStubs(t)
	old := attachExec
	defer func() { attachExec = old }()
	var got, env []string
	var path string
	attachExec = func(p string, argv, e []string) error { path, got, env = p, argv, e; return nil }
	t.Setenv("PATH", "/tmp/attacker-bin")
	t.Setenv("TERM", "xterm")
	// the default socket (a bare name) must not send helper mode to the owner's own tmux
	if code, out := run("attach", "--root", "helper", "s0abcd1"); code != 0 {
		t.Fatalf("exit %d: %s", code, out)
	}
	if path != start.SudoBin || strings.Join(got, " ") != "sudo -n "+start.RootBin+" attach -- s0abcd1" {
		t.Errorf("exec'd %q %v", path, got)
	}
	if strings.Join(env, " ") != "PATH=/usr/bin:/bin TERM=xterm LANG=C.UTF-8" {
		t.Errorf("env %v", env)
	}
	// auto picks the helper too, once it is installed
	got = nil
	if code, _ := run("attach", "s0abcd1"); code != 0 || got == nil {
		t.Error("auto did not use the helper")
	}
	// --read-only cannot be carried through the door: refused (64) before anything runs
	got = nil
	if code, _ := run("attach", "--root", "helper", "--read-only", "s0abcd1"); code != 64 || got != nil {
		t.Errorf("--read-only: exec %v", got)
	}
	for _, sid := range []string{"S0ABCD1", "s0", "../x"} {
		if code, _ := run("attach", "--root", "helper", sid); code != 64 {
			t.Errorf("sid %q: exit %d", sid, code)
		}
	}
}

func TestAttachReportsSudoRefusalsAndDoesNotFallBack(t *testing.T) {
	s := newStubs(t)
	s.set("sudo_mode", "pw")
	old := attachExec
	defer func() { attachExec = old }()
	called := false
	attachExec = func(string, []string, []string) error { called = true; return nil }
	code, out := run("attach", "--root", "helper", "s0abcd1")
	if code != 2 || !strings.Contains(out, "password") || !strings.Contains(out, "Not falling back") || called {
		t.Errorf("%d %s called=%v", code, out, called)
	}
}

// the recipe in `attach --help` is what people copy: flags go before the sid (a flag after it is exit 64)
func TestTheAttachRecipesParse(t *testing.T) {
	newStubs(t)
	old := attachExec
	defer func() { attachExec = old }()
	attachExec = func(string, []string, []string) error { return nil }
	_, help := run("attach", "--help")
	if strings.Contains(help, "s0k1dry66qg --read-only") {
		t.Errorf("the recipe puts the flag after the sid:\n%s", help)
	}
	// helper mode refuses --read-only, but only after the parser accepted the flag before the sid
	if _, out := run("attach", "--root", "helper", "--read-only", "s0abcd1"); !strings.Contains(out, "not available through ccsbx-root") {
		t.Errorf("flags before the sid do not parse: %s", out)
	}
}
