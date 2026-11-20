// Tests of the tmux pane launcher. Why: the pane of a fenced agent is the owner's way in, so its
// server must be hardened, and when it lives on a shared socket path that path must be reachable by
// the group and by no one else.

package start

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The pane's output is controlled by the fenced agent, so the tmux server must refuse clipboard
// writes and passthrough escapes. They are set in the same invocation as new-session because a
// pane that exits at once takes the server with it.
func TestTmuxHardeningIsInTheSameInvocation(t *testing.T) {
	var got []string
	tl := &TmuxLauncher{Exe: "/bin/ccsbx", Socket: "ccsbx", Command: func(name string, args ...string) *exec.Cmd {
		got = append([]string{name}, args...)
		return exec.Command("true")
	}}
	rec := &Record{Sid: "sabc123", Gen: 1}
	rec.Spec.Workdir.Path = "/var/tmp"
	if err := tl.Launch(rec); err != nil {
		t.Fatal(err)
	}
	line := strings.Join(got, " ")
	for _, want := range []string{"tmux -L ccsbx new-session -d -s sabc123", "; set-option -g set-clipboard off", "; set-option -g allow-passthrough off", "run --sid sabc123 --gen 1"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

// context: hostd runs as a service uid and the owner attaches from another uid through a shared
// socket path (--tmux-socket /run/ccsbx/tmux.sock). tmux creates its socket 0600, so the launcher
// must make it 0660 itself, and the server must be addressed with -S, not -L.
func TestAbsoluteTmuxSocketIsUsedWithSAndMadeGroupReachable(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir, _ := os.MkdirTemp("/tmp", "tmx")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "t.sock")
	exe := filepath.Join(dir, "pane.sh")
	os.WriteFile(exe, []byte("#!/bin/sh\nsleep 30\n"), 0o755)
	tl := &TmuxLauncher{Exe: exe, Socket: sock}
	defer exec.Command("tmux", "-S", sock, "kill-server").Run()
	rec := &Record{Sid: "sabc123", Gen: 1}
	rec.Spec.Workdir.Path = dir
	if err := tl.Launch(rec); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(sock)
	if err != nil || st.Mode().Perm() != 0o660 {
		t.Fatalf("socket %v err %v", st, err)
	}
	if !tl.Alive("sabc123") {
		t.Fatal("the session is not on the -S server")
	}
}

// context: if the socket can't be made 0660 (it is not where it should be), the session must not be
// left running on a socket with the wrong mode: Launch fails.
func TestLaunchFailsClosedWhenTheSharedSocketCannotBeChmodded(t *testing.T) {
	tl := &TmuxLauncher{Exe: "/bin/ccsbx", Socket: "/nonexistent-dir/t.sock", Command: func(string, ...string) *exec.Cmd { return exec.Command("true") }}
	rec := &Record{Sid: "sabc123", Gen: 1}
	rec.Spec.Workdir.Path = "/var/tmp"
	if err := tl.Launch(rec); err == nil || !strings.Contains(err.Error(), "0660") {
		t.Fatalf("%v", err)
	}
}
