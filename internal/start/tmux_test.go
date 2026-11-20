// Tests of the tmux pane launcher. Why: the pane of a fenced agent is the owner's way in, so its
// server must be hardened, and when it lives on a shared socket path that path must be reachable by
// the group and by no one else.

package start

import (
	"time"

	"github.com/landovsky/cc-sandbox/internal/tmuxsock"
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
	for _, want := range []string{"tmux -L ccsbx -f /dev/null new-session -d -s sabc123", "; set-option -g set-clipboard off", "; set-option -g allow-passthrough off", "run --sid sabc123 --gen 1",
		"tmux -L ccsbx -f /dev/null new-session", "; set-option -g status off", "; set-option -g prefix None", "; set-option -g escape-time 0"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}

// context: hostd runs as a service uid and its tmux server lives on a shared socket PATH
// (--tmux-socket /run/ccsbx/tmux.sock). The server must be addressed with -S, and the socket must
// stay 0600: a group-writable tmux socket would let any process of a group member type into fenced
// panes and run commands as the service uid. The owner gets in through `ccsbx-root attach`.
func TestAbsoluteTmuxSocketIsUsedWithSAndStaysPrivate(t *testing.T) {
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
	if err != nil || st.Mode().Perm()&0o077 != 0 {
		t.Fatalf("socket %v err %v: must not be open to group or others", st, err)
	}
	if !tl.Alive("sabc123") {
		t.Fatal("the session is not on the -S server")
	}
}

// context: a wedged tmux server must not stall the daemon: the launcher's calls are bounded too.
func TestLauncherTmuxCallsTimeOut(t *testing.T) {
	old := tmuxsock.Timeout
	tmuxsock.Timeout = 200 * time.Millisecond
	defer func() { tmuxsock.Timeout = old }()
	tl := &TmuxLauncher{Exe: "x", Socket: "x", Command: func(string, ...string) *exec.Cmd { return exec.Command("sleep", "30") }}
	t0 := time.Now()
	// "cannot tell" (a wedged server) counts as alive: the label check fails closed, and the call is bounded
	if !tl.Alive("s1") || time.Since(t0) > 3*time.Second {
		t.Fatalf("not alive on a timeout, or slow: %s", time.Since(t0))
	}
}

// context: the tmux server is ccsbx-tmux.service's, in the host mount namespace. A `new-session` from
// hostd would start a server of its own inside hostd's sandbox (where panes can't run ccsbx-root
// properly), so with NoSpawn a missing server is an error with a clear message, and nothing is started.
func TestNoSpawnLauncherNeverStartsAServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir, _ := os.MkdirTemp("/tmp", "nsp")
	defer os.RemoveAll(dir)
	sock, marker := filepath.Join(dir, "t.sock"), filepath.Join(dir, "ran")
	exe := filepath.Join(dir, "pane.sh")
	os.WriteFile(exe, []byte("#!/bin/sh\ntouch "+marker+"\nsleep 5\n"), 0o755)
	tl := &TmuxLauncher{Exe: exe, Socket: sock, NoSpawn: true}
	rec := &Record{Sid: "sabc123", Gen: 1}
	rec.Spec.Workdir.Path = dir
	err := tl.Launch(rec)
	if err == nil || !strings.Contains(err.Error(), "start ccsbx-tmux.service") {
		t.Fatalf("no server must be a clear error: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	if _, e := os.Stat(sock); e == nil {
		t.Fatal("a tmux server was started (its socket exists)")
	}
	if _, e := os.Stat(marker); e == nil {
		t.Fatal("the pane command ran")
	}
}

// context: the allow side: with a server already up (the unit's), NoSpawn launches into it.
func TestNoSpawnLauncherUsesARunningServer(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("no tmux")
	}
	dir, _ := os.MkdirTemp("/tmp", "nsp")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "t.sock")
	defer exec.Command("tmux", "-S", sock, "kill-server").Run()
	if out, err := exec.Command("tmux", "-S", sock, "-f", "/dev/null", "new-session", "-d", "-s", "unit", "sleep", "30").CombinedOutput(); err != nil {
		t.Fatalf("%v %s", err, out)
	}
	exe := filepath.Join(dir, "pane.sh")
	os.WriteFile(exe, []byte("#!/bin/sh\nsleep 30\n"), 0o755)
	tl := &TmuxLauncher{Exe: exe, Socket: sock, NoSpawn: true}
	rec := &Record{Sid: "sabc123", Gen: 1}
	rec.Spec.Workdir.Path = dir
	if err := tl.Launch(rec); err != nil {
		t.Fatal(err)
	}
	if !tl.Alive("sabc123") {
		t.Fatal("the session is not on the running server")
	}
}

// U1c: in helper mode the pane runs the helper, through sudo, by absolute path and with nothing
// but the sid: no ccsbx binary, no --state or --image flags that the helper would not read.
func TestHelperPaneRunsSudoRootRunTty(t *testing.T) {
	var got []string
	l := &TmuxLauncher{Exe: "/opt/ccsbx", Socket: "ccsbx", Helper: true, Env: []string{"--state", "/x"},
		Command: func(name string, args ...string) *exec.Cmd { got = args; return exec.Command("true") }}
	if err := l.Launch(&Record{Sid: "s0abcdefgh", Gen: 2, Spec: LaunchSpec{Workdir: Workdir{Path: "/w"}}}); err != nil {
		t.Fatal(err)
	}
	j := strings.Join(got, " ")
	want := "new-session -d -s s0abcdefgh -c / " + SudoBin + " -n " + RootBin + " run --tty -- s0abcdefgh"
	if !strings.Contains(j, want) {
		t.Errorf("pane command not as expected:\n got %s\nwant it to contain %s", j, want)
	}
	if strings.Contains(j, "/opt/ccsbx") || strings.Contains(j, "--state") {
		t.Errorf("direct-mode arguments leaked into the helper pane: %s", j)
	}
	// direct mode is unchanged
	l.Helper = false
	l.Launch(&Record{Sid: "s0abcdefgh", Gen: 2, Spec: LaunchSpec{Workdir: Workdir{Path: "/w"}}})
	if j := strings.Join(got, " "); !strings.Contains(j, "/opt/ccsbx run --sid s0abcdefgh --gen 2 --state /x") {
		t.Errorf("direct pane changed: %s", j)
	}
}
