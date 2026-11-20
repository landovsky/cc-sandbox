// Tests of ccsbx-hostd's flag handling. Why: every tunable must be reachable as a flag/env var and
// listed in --help, and a bad value must stop the daemon at start, not later.

package main

import (
	"bytes"
	"net"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/daemonapi"
)

// Every subcommand's --help runs nothing (the next agent learns the tool without side effects).
func TestHelpRunsNothing(t *testing.T) {
	var out, errb bytes.Buffer
	if code := app().Main([]string{"run", "--help"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "-fence-uid") {
		t.Fatalf("exit %d out=%q err=%q", code, out.String(), errb.String())
	}
	if strings.Contains(errb.String(), "watching") {
		t.Fatal("--help started the loop")
	}
}

func TestOnceOnEmptyStateTouchesNothing(t *testing.T) {
	var out, errb bytes.Buffer
	dir := t.TempDir()
	if code := app().Main([]string{"run", "--once", "--state", dir}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "accepted=0") {
		t.Fatalf("exit %d %q %q", code, out.String(), errb.String())
	}
}

// ---- the installer's contract (R1): --local-socket, --socket-group, --signer -----------------

func runHostd(args ...string) (int, string) {
	var out, errb bytes.Buffer
	code := app().Main(args, &out, &errb)
	return code, out.String() + errb.String()
}

// context: the installer's unit passes flags this daemon must know, or it crash-loops at Gate M.
func TestInstallerFlagsAreListedInHelpAndEnv(t *testing.T) {
	_, h := runHostd("run", "--help")
	for _, w := range []string{"-local-socket", "-socket-group", "-signer", "-listen-token-file", "CCSBX_LOCAL_SOCKET", "CCSBX_SOCKET_GROUP", "CCSBX_SIGNER"} {
		if !strings.Contains(h, w) {
			t.Errorf("--help does not list %s", w)
		}
	}
	if code, out := runHostd("run", "--once", "--state", t.TempDir(), "--no-such-flag"); code == 0 {
		t.Fatalf("an unknown flag was accepted: %s", out)
	}
}

// context: a signer that is not up must be a clear error at start, not a hang or a daemon that
// finds out at the first token.
func TestMissingSignerSocketIsAClearErrorNotAHang(t *testing.T) {
	done := make(chan struct{})
	var code int
	var out string
	go func() {
		code, out = runHostd("run", "--once", "--state", t.TempDir(), "--signer", "/nonexistent/signer.sock")
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("hung")
	}
	if code == 0 || !strings.Contains(out, "--signer") || !strings.Contains(out, "/nonexistent/signer.sock") {
		t.Fatalf("%d %q", code, out)
	}
}

func TestSignerThatIsListeningIsAccepted(t *testing.T) {
	dir, err := os.MkdirTemp("/tmp", "sg")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	l, err := net.Listen("unix", filepath.Join(dir, "s.sock"))
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	if code, out := runHostd("run", "--once", "--state", t.TempDir(), "--signer", filepath.Join(dir, "s.sock")); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
}

func sockMode(t *testing.T, group string) (os.FileMode, uint32, error) {
	dir, _ := os.MkdirTemp("/tmp", "sk")
	defer os.RemoveAll(dir)
	p := filepath.Join(dir, "h.sock")
	l, err := daemonapi.ListenUnixGroup(p, group)
	if err != nil {
		return 0, 0, err
	}
	defer l.Close()
	st, _ := os.Stat(p)
	return st.Mode().Perm(), st.Sys().(*syscall.Stat_t).Gid, nil
}

// context: the local socket is the owner class. Without --socket-group it stays 0600; with a group
// it is 0660 and that group (so the owner's user can reach a service uid's daemon, and no one else).
func TestSocketModeIs0600AndWithAGroup0660(t *testing.T) {
	if m, _, err := sockMode(t, ""); err != nil || m != 0o600 {
		t.Fatalf("no group: %v %v", m, err)
	}
	g, err := user.LookupGroupId(strconv.Itoa(os.Getgid())) // a group this user may chgrp to, root or not
	if err != nil {
		t.Skip("no group name for our gid")
	}
	m, gid, err := sockMode(t, g.Name)
	if err != nil || m != 0o660 || int(gid) != os.Getgid() {
		t.Fatalf("group %s: mode %v gid %d err %v", g.Name, m, gid, err)
	}
}

// context: fail closed. An unknown group must not leave a 0600 socket the unit's clients can't use
// (and a mis-set group must be noticed at start): no listener at all.
func TestUnknownSocketGroupFailsClosed(t *testing.T) {
	if _, _, err := sockMode(t, "no-such-group-ccsbx"); err == nil {
		t.Fatal("an unknown group gave a socket")
	}
	if code, _ := runHostd("run", "--state", t.TempDir(), "--local-socket", "/tmp/ccsbx-unused.sock", "--socket-group", "no-such-group-ccsbx"); code == 0 {
		t.Fatal("the daemon started")
	}
}
