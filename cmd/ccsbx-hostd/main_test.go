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
	"github.com/landovsky/cc-sandbox/internal/start"
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

// context: the fence-event budget is a tunable, so it is a flag with an env var.
func TestEventBudgetHasFlags(t *testing.T) {
	_, h := runHostd("run", "--help")
	for _, w := range []string{"-event-rate", "-event-burst", "CCSBX_EVENT_RATE", "CCSBX_EVENT_BURST"} {
		if !strings.Contains(h, w) {
			t.Errorf("--help does not list %s", w)
		}
	}
}

// context: the installer's unit passes the run dir and the start-policy limits; each is a flag with an
// env var and a --help line.
func TestRunDirAndStartPolicyFlags(t *testing.T) {
	_, h := runHostd("run", "--help")
	for _, w := range []string{"-run-dir", "CCSBX_RUN_DIR", "-pins", "-worktree-root", "-max-live", "-max-per-hour", "-cc-version"} {
		if !strings.Contains(h, w) {
			t.Errorf("--help does not list %s", w)
		}
	}
}

// context: helper mode without --run-dir would look for outboxes in the state dir and find none, so
// every request would go unanswered. It is refused at start, and --once (a dry run) must not skip it.
func TestHelperModeWithoutARunDirIsRefusedEvenWithOnce(t *testing.T) {
	root := filepath.Join(t.TempDir(), "ccsbx-root")
	os.WriteFile(root, []byte("#!/bin/sh\n"), 0o755)
	oldBin := start.RootBin
	start.RootBin = root
	defer func() { start.RootBin = oldBin }()
	defer start.SetTestRootUID(os.Getuid())()
	state := t.TempDir()
	code, out := runHostd("run", "--once", "--root", "helper", "--state", state)
	if code == 0 || !strings.Contains(out, "--run-dir") {
		t.Fatalf("%d %s", code, out)
	}
	if code, out := runHostd("run", "--once", "--root", "helper", "--state", state, "--run-dir", t.TempDir()); code != 0 {
		t.Fatalf("with a run dir: %d %s", code, out)
	}
}

// context: hostd uses the profiles compiled into the binary (the same copy ccsbx-root has); there is
// no --profiles flag to point it at a different set.
func TestHostdHasNoProfilesFlag(t *testing.T) {
	if code, _ := runHostd("run", "--once", "--state", t.TempDir(), "--profiles", "x"); code == 0 {
		t.Fatal("--profiles was accepted")
	}
}

// context: a direct-mode pane started by hostd (a replaced session) must not be told to read profiles
// from a directory relative to hostd's working directory: the profiles are the ones in the binary.
func TestPaneEnvUsesTheEmbeddedProfiles(t *testing.T) {
	env := strings.Join(paneEnv("/var/lib/ccsbx", "hp", "ccsbx-fence:dev", "direct"), " ")
	if !strings.Contains(env, "--profiles -") || strings.Contains(env, "/profiles") {
		t.Fatalf("%s", env)
	}
}

// context: the hub credential file is written by the installer (root) and read by hostd through a
// group, so group-read is fine; group-write, any access by others, or a token of the wrong kind is
// refused (a world-readable spoke token would let any local user speak for the machine).
func TestHubTokenFileModesAndShape(t *testing.T) {
	p := filepath.Join(t.TempDir(), "hub.token")
	good := "sbp1.abcdefghijklmnop\n"
	write := func(mode os.FileMode, body string) {
		os.Remove(p)
		os.WriteFile(p, []byte(body), mode)
		os.Chmod(p, mode)
	}
	for _, m := range []os.FileMode{0o600, 0o640, 0o400} {
		write(m, good)
		if tok, err := readHubToken(p); err != nil || tok != "sbp1.abcdefghijklmnop" {
			t.Errorf("mode %o refused: %v", m, err)
		}
	}
	for _, m := range []os.FileMode{0o660, 0o644, 0o604, 0o666, 0o610} {
		write(m, good)
		if _, err := readHubToken(p); err == nil {
			t.Errorf("mode %o accepted", m)
		}
	}
	write(0o600, "not-a-token-at-all-0123456789\n")
	if _, err := readHubToken(p); err == nil {
		t.Error("a token of the wrong kind was accepted")
	}
}
