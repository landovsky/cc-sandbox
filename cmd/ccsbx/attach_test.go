// Tests for how `ccsbx attach` reaches the root door. The door refuses to run unless it is root,
// so the owner's command must go through sudo, by absolute path and not through the caller's PATH.
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAttachGoesThroughSudoToTheRootDoor(t *testing.T) {
	door := filepath.Join(t.TempDir(), "ccsbx-root")
	os.WriteFile(door, []byte("#!/bin/sh\n"), 0o755)
	oldRoot, oldSudo, oldExec := attachRoot, attachSudo, attachExec
	defer func() { attachRoot, attachSudo, attachExec = oldRoot, oldSudo, oldExec }()
	attachRoot, attachSudo = door, "/usr/bin/sudo"
	var got []string
	var path string
	attachExec = func(p string, argv []string, _ []string) error { path, got = p, argv; return nil }
	t.Setenv("PATH", "/tmp/attacker-bin")
	var out, errb bytes.Buffer
	if code := app().Main([]string{"attach", "--tmux-socket", "/nonexistent/ccsbx/tmux.sock", "s0abcd1"}, &out, &errb); code != 0 {
		t.Fatalf("exit %d: %s", code, errb.String())
	}
	if path != "/usr/bin/sudo" || strings.Join(got, " ") != "sudo -n "+door+" attach s0abcd1" {
		t.Errorf("exec'd %q %v", path, got)
	}
	// --read-only cannot be carried through the door: refused (64) before anything runs
	got = nil
	if code := app().Main([]string{"attach", "--read-only", "--tmux-socket", "/nonexistent/ccsbx/tmux.sock", "s0abcd1"}, &out, &errb); code != 64 || got != nil {
		t.Errorf("--read-only: exit %d, exec %v", code, got)
	}
}
