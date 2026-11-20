// helper.go is the client of ccsbx-root for `ccsbx run`, `stop` and `gc` (U1c). It exists because
// after Gate M nobody on the host holds the docker group: in helper mode the same commands run
// `/usr/bin/sudo -n /usr/local/sbin/ccsbx-root <verb> <sid>` instead of driving docker, and a
// failure of sudo itself (a password needed, not permitted, the helper missing) is reported as what
// it is. There is deliberately no fallback from helper to direct mode: a refusal by the helper is
// an answer, not a reason to try another way in.
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/start"
)

// dockerSocket is where direct mode reaches docker; tests replace it.
var dockerSocket = "/var/run/docker.sock"

// modeFlagHelp is the shared --root flag text.
const modeFlagHelp = "privilege mode: auto (helper if /usr/local/sbin/ccsbx-root is installed and root-owned, else direct), direct (the caller drives docker; dev; results are not evidence) or helper (through sudo -n ccsbx-root; no docker for the caller)"

// resolveMode applies the flag/env value.
func resolveMode(f *cli.Flags) (string, error) {
	return start.ResolveRoot(f.Lookup("root").Value.String())
}

// directDockerReachable reports whether the caller can connect to docker's socket. Direct mode
// needs it; after the docker group is gone it is false, and the error says what to do.
func directDockerReachable() error {
	if h := os.Getenv("DOCKER_HOST"); h != "" && !strings.HasPrefix(h, "unix://") {
		return nil // a remote or context daemon: not ours to probe
	}
	sock := dockerSocket
	if h := strings.TrimPrefix(os.Getenv("DOCKER_HOST"), "unix://"); h != "" && os.Getenv("DOCKER_HOST") != h {
		sock = h
	}
	c, err := net.DialTimeout("unix", sock, 2*time.Second)
	if err != nil {
		return fmt.Errorf("direct mode needs docker, and this user cannot reach it (%v). After Gate M the docker group is gone: use the helper (--root helper / CCSBX_ROOT=helper; it is chosen automatically when /usr/local/sbin/ccsbx-root is installed)", err)
	}
	c.Close()
	return nil
}

// sudoPreflight asks sudo, before anything runs, whether this user may run the helper without a
// password (`sudo -n -l <helper>` exits 0 exactly then). Everything sudo says to refuse is said here,
// in output that is sudo's alone: the helper's and the fence's output is never read for sudo
// errors, so a fence that prints "sudo: a password is required" cannot turn its own exit code into
// a refusal. It returns the explanation, or "" when sudo permits.
func sudoPreflight() string {
	c := exec.Command(start.SudoBin, "-n", "-l", start.RootBin)
	c.Env = []string{"PATH=/usr/bin:/bin", "LANG=C"}
	var out cappedBuffer
	c.Stdout, c.Stderr = &out, &out
	err := c.Run()
	if err == nil {
		return ""
	}
	if _, ok := err.(*exec.ExitError); !ok {
		return fmt.Sprintf("cannot run %s: %v", start.SudoBin, err)
	}
	return sudoProblem(out.String())
}

// cappedBuffer keeps the first 4 KiB written to it.
type cappedBuffer struct{ b bytes.Buffer }

func (c *cappedBuffer) Write(p []byte) (int, error) {
	if room := 4096 - c.b.Len(); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		c.b.Write(p[:room])
	}
	return len(p), nil
}
func (c *cappedBuffer) String() string { return c.b.String() }

// sudoProblem explains sudo's own refusal from its own output (the formats of sudo 1.9: lines
// "sudo: ..." and "Sorry, user X is not allowed to execute ..."). It always returns something for
// a failed `sudo -n -l`: an unknown failure is still a refusal.
func sudoProblem(out string) string {
	switch {
	case strings.Contains(out, "a password is required"), strings.Contains(out, "a terminal is required"):
		return "sudo asks for a password: this user has no passwordless rule for ccsbx-root (the installer's /etc/sudoers.d/90-ccsbx-root is missing or does not name this user)"
	case strings.Contains(out, "command not found"):
		return "sudo could not find " + start.RootBin + " (is ccsbx-root installed?)"
	case strings.Contains(out, "is not allowed to execute"), strings.Contains(out, "is not in the sudoers"), strings.Contains(out, "may not run sudo"):
		return "sudo does not permit this user to run ccsbx-root (see /etc/sudoers.d/90-ccsbx-root)"
	}
	if l := firstSudoLine(out); l != "" {
		return "sudo refused: " + l
	}
	return "sudo refused to list the helper (is ccsbx-root installed, and is this user in /etc/sudoers.d/90-ccsbx-root?)"
}

// firstSudoLine is the first non-empty line, which is where sudo says why.
func firstSudoLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if l = strings.TrimSpace(l); l != "" {
			return l
		}
	}
	return ""
}

// runHelper runs `sudo -n ccsbx-root <verb> <args...>` with this process's stdin/stdout/stderr and
// returns the exit code: the helper's own (0 ok, 1 failed, 2 refused, 64 usage), or 128+signal when
// it was killed. Whether sudo permits is asked first (sudoPreflight): a refusal there is exit 2 and
// is never followed by a fallback to direct mode.
func runHelper(cmdName, verb string, args []string, stdout, stderr io.Writer) int {
	if !start.HelperInstalled() {
		fmt.Fprintf(stderr, "ccsbx %s: --root helper, but %s is not installed and root-owned (units/install-linux.sh)\n", cmdName, start.RootBin)
		return cli.ExitRefused
	}
	if why := sudoPreflight(); why != "" {
		fmt.Fprintf(stderr, "ccsbx %s: %s. Not falling back to direct mode.\n", cmdName, why)
		return cli.ExitRefused
	}
	argv := append([]string{"-n", start.RootBin, verb}, args...)
	c := exec.Command(start.SudoBin, argv...)
	c.Env = []string{"PATH=/usr/bin:/bin", "TERM=" + os.Getenv("TERM"), "LANG=C.UTF-8"}
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, stdout, stderr
	err := c.Run()
	if err == nil {
		return cli.ExitOK
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		fmt.Fprintf(stderr, "ccsbx %s: cannot run %s: %v\n", cmdName, start.SudoBin, err)
		return cli.ExitRefused
	}
	if ws, ok := ee.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return ee.ExitCode()
}

// refuseExplicit is exit 64 when the caller named, on the command line, a flag that only direct
// mode uses. In helper mode root decides the image, the generation, the state and docker; a flag
// that is silently ignored would let the caller think it had chosen them.
func refuseExplicit(cmdName string, f *cli.Flags, stderr io.Writer, names ...string) bool {
	var bad []string
	f.Visit(func(fl *flag.Flag) {
		for _, n := range names {
			if fl.Name == n {
				bad = append(bad, "--"+n)
			}
		}
	})
	if len(bad) == 0 {
		return false
	}
	fmt.Fprintf(stderr, "ccsbx %s: %s is not accepted in helper mode: ccsbx-root takes it from the daemon's record and its own config (use --root direct for a dev run)\n", cmdName, strings.Join(bad, ", "))
	return true
}
