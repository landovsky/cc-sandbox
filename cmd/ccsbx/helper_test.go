// Tests for the helper-mode client (U1c): `ccsbx run`, `stop` and `gc` reach ccsbx-root through
// sudo, with the right arguments, and every way sudo or the helper can say no is reported as that,
// with no fallback to direct mode. A stub sudo and a stub ccsbx-root stand in for the real ones; the
// paths are injected through package variables that exist for tests only (production never takes
// them from the environment or a flag, or a caller could choose what runs as root).
package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/landovsky/cc-sandbox/internal/start"
)

const stubSid = "s0abcdefgh"

type stubs struct {
	dir, log string
}

// newStubs installs a stub sudo and a stub ccsbx-root. The stub sudo logs its arguments, then
// either behaves like sudo failing (SUDO_MODE file: pw, notallowed, notfound, other) or runs the
// stub helper; the stub helper logs its arguments and exits as the HELPER_EXIT file says, saying
// what the real one says on a refusal.
func newStubs(t *testing.T) *stubs {
	t.Helper()
	d, _ := filepath.EvalSymlinks(t.TempDir())
	s := &stubs{dir: d, log: filepath.Join(d, "log")}
	write := func(name, body string) string {
		p := filepath.Join(d, name)
		os.WriteFile(p, []byte("#!/bin/sh\n"+body), 0o755)
		return p
	}
	root := write("ccsbx-root", `echo "root $*" >> `+s.log+`
code=$(cat `+d+`/helper_exit 2>/dev/null || echo 0)
[ "$code" = 2 ] && echo "ccsbx-root $1: refused: the image is not pinned" >&2
exit "$code"
`)
	sudo := write("sudo", `echo "sudo $*" >> `+s.log+`
case "$(cat `+d+`/sudo_mode 2>/dev/null)" in
  pw) echo "sudo: a password is required" >&2; exit 1 ;;
  notallowed) echo "sudo: tomas is not allowed to execute '/usr/local/sbin/ccsbx-root' as root on hp" >&2; exit 1 ;;
  notfound) echo "sudo: /usr/local/sbin/ccsbx-root: command not found" >&2; exit 1 ;;
  other) echo "sudo: unable to resolve host" >&2; exit 1 ;;
esac
shift   # -n
exec "$@"
`)
	oldS, oldR, oldD := start.SudoBin, start.RootBin, dockerSocket
	start.SudoBin, start.RootBin, dockerSocket = sudo, root, filepath.Join(d, "no-docker.sock")
	restore := start.SetTestRootUID(os.Getuid())
	t.Cleanup(func() { start.SudoBin, start.RootBin, dockerSocket = oldS, oldR, oldD; restore() })
	return s
}

func (s *stubs) set(name, v string) { os.WriteFile(filepath.Join(s.dir, name), []byte(v), 0o644) }
func (s *stubs) calls() []string {
	b, _ := os.ReadFile(s.log)
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func run(args ...string) (int, string) {
	var out, errb bytes.Buffer
	code := app().Main(args, &out, &errb)
	return code, out.String() + errb.String()
}

func TestRunStopGcGoThroughSudoToTheHelper(t *testing.T) {
	s := newStubs(t)
	for _, c := range []struct {
		args []string
		want string
	}{
		{[]string{"run", "--root", "helper", "--sid", stubSid}, "sudo -n " + start.RootBin + " run " + stubSid},
		{[]string{"stop", "--root", "helper", stubSid}, "sudo -n " + start.RootBin + " stop " + stubSid},
		{[]string{"gc", "--root", "helper"}, "sudo -n " + start.RootBin + " gc"},
		{[]string{"gc", "--root", "helper", "--dry-run"}, "sudo -n " + start.RootBin + " gc --dry-run"},
	} {
		os.Remove(s.log)
		if code, out := run(c.args...); code != 0 {
			t.Errorf("%v: exit %d %s", c.args, code, out)
		}
		if got := s.calls(); got[0] != c.want {
			t.Errorf("%v: sudo was called as %q, want %q", c.args, got[0], c.want)
		}
	}
}

// With no --root the helper is chosen because it is installed (the mode is not a flag the caller
// has to remember), and CCSBX_ROOT=direct overrides that.
func TestTheModeIsChosenWithoutAFlag(t *testing.T) {
	s := newStubs(t)
	if code, out := run("stop", stubSid); code != 0 || !strings.HasPrefix(s.calls()[0], "sudo -n") {
		t.Fatalf("auto did not use the helper: %d %s %v", code, out, s.calls())
	}
	t.Setenv("CCSBX_ROOT", "direct")
	os.Remove(s.log)
	code, out := run("stop", stubSid)
	if code != 2 || !strings.Contains(out, "cannot reach it") && !strings.Contains(out, "cannot reach docker") && !strings.Contains(out, "needs docker") {
		t.Errorf("direct mode with no docker: exit %d %s", code, out)
	}
	if _, err := os.Stat(s.log); err == nil {
		t.Error("the helper was called although direct mode was asked for")
	}
}

// Every way the helper or sudo can say no is passed through, as exit 2 for what sudo says and the
// helper's own code for what the helper says. None of them runs docker (the socket does not exist
// here, so a fallback would fail differently and the stub log would show no second attempt).
func TestEveryRefusalIsPassedThroughAndNeverFallsBack(t *testing.T) {
	cases := []struct {
		name, sudoMode, helperExit string
		wantCode                   int
		wantText                   string
	}{
		{"sudo wants a password", "pw", "", 2, "password"},
		{"sudo does not permit the user", "notallowed", "", 2, "does not permit"},
		{"sudo cannot find the helper", "notfound", "", 2, "ccsbx-root installed"},
		{"another sudo error", "other", "", 2, "sudo refused"},
		{"the helper refuses", "", "2", 2, "refused: the image is not pinned"},
		{"the helper fails", "", "1", 1, ""},
		{"the helper's usage error", "", "64", 64, ""},
	}
	for _, c := range cases {
		s := newStubs(t)
		s.set("sudo_mode", c.sudoMode)
		if c.helperExit != "" {
			s.set("helper_exit", c.helperExit)
		}
		code, out := run("run", "--root", "helper", "--sid", stubSid)
		if code != c.wantCode || !strings.Contains(out, c.wantText) {
			t.Errorf("%s: exit %d, want %d, text %q: %s", c.name, code, c.wantCode, c.wantText, out)
		}
		if c.sudoMode != "" && !strings.Contains(out, "Not falling back to direct mode") {
			t.Errorf("%s: it does not say there is no fallback: %s", c.name, out)
		}
		n := 0
		for _, l := range s.calls() {
			if strings.HasPrefix(l, "sudo ") {
				n++
			}
		}
		if n != 1 {
			t.Errorf("%s: %d sudo calls, want 1 (a retry or fallback ran something)", c.name, n)
		}
	}
}

func TestHelperModeNeedsAnInstalledHelper(t *testing.T) {
	newStubs(t)
	os.Chmod(start.RootBin, 0o777)
	if code, out := run("run", "--root", "helper", "--sid", stubSid); code != 2 || !strings.Contains(out, "writable") {
		t.Errorf("a writable helper was used: %d %s", code, out)
	}
	os.Remove(start.RootBin)
	if code, out := run("gc", "--root", "helper"); code != 2 || !strings.Contains(out, "does not exist") {
		t.Errorf("a missing helper: %d %s", code, out)
	}
}

func TestHelperRunTakesNoCommandAndStopTakesOnlyASid(t *testing.T) {
	s := newStubs(t)
	if code, out := run("run", "--root", "helper", "--sid", stubSid, "--", "sh", "-c", "id"); code != 64 {
		t.Errorf("a command was accepted in helper mode: %d %s", code, out)
	}
	for _, a := range [][]string{{"stop"}, {"stop", "../x"}, {"stop", stubSid, "extra"}, {"stop", "S0ABCDEFGH"}} {
		if code, _ := run(append([]string{a[0], "--root", "helper"}, a[1:]...)...); code != 64 {
			t.Errorf("%v: exit %d, want 64", a, code)
		}
	}
	if _, err := os.Stat(s.log); err == nil {
		t.Error("the helper was called for a malformed request")
	}
}

// Direct mode says plainly that it cannot work, instead of failing inside docker.
func TestDirectModeRefusesWhenDockerIsUnreachable(t *testing.T) {
	newStubs(t)
	for _, args := range [][]string{{"run", "--root", "direct", "--sid", stubSid}, {"gc", "--root", "direct"}, {"stop", "--root", "direct", stubSid}} {
		code, out := run(args...)
		if code != 2 || !strings.Contains(out, "CCSBX_ROOT=helper") {
			t.Errorf("%v: exit %d, %s", args, code, out)
		}
	}
}

func TestSudoProblem(t *testing.T) {
	for in, want := range map[string]string{
		"sudo: a password is required":                "password",
		"sudo: a terminal is required to read":        "password",
		"sudo: x is not in the sudoers file":          "does not permit",
		"sudo: sorry, user x may not run sudo on hp":  "does not permit",
		"sudo: ccsbx-root: command not found":         "installed",
		"sudo: unable to resolve host hp":             "sudo refused",
		"ccsbx-root run: refused: not pinned":         "",
		"":                                            "",
		"some docker error without sudo in it at all": "",
	} {
		got := sudoProblem(in)
		if (want == "") != (got == "") || !strings.Contains(got, want) {
			t.Errorf("%q -> %q, want to contain %q", in, got, want)
		}
	}
}
