// helper.go is the client of ccsbx-root for `ccsbx run`, `stop` and `gc` (U1c). It exists because
// after Gate M nobody on the host holds the docker group: in helper mode the same commands run
// `/usr/bin/sudo -n /usr/local/sbin/ccsbx-root <verb> <sid>` instead of driving docker, and a
// failure of sudo itself (a password needed, not permitted, the helper missing) is reported as what
// it is. There is deliberately no fallback from helper to direct mode: a refusal by the helper is
// an answer, not a reason to try another way in.
package main

import (
	"bytes"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/start"
)

// dockerSocket is where direct mode reaches docker; tests replace it.
var dockerSocket = "/var/run/docker.sock"

// modeFlag is the shared --root flag text.
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

// sudoProblem recognises sudo's own refusals in its stderr so they are explained, not mistaken for
// the helper's exit codes (sudo exits 1 for them, like ccsbx-root's own failures).
func sudoProblem(stderr string) string {
	switch {
	case strings.Contains(stderr, "a password is required"), strings.Contains(stderr, "a terminal is required"):
		return "sudo asks for a password: this user has no passwordless rule for ccsbx-root (the installer's /etc/sudoers.d/90-ccsbx-root is missing or does not name this user)"
	case strings.Contains(stderr, "is not allowed to execute"), strings.Contains(stderr, "is not in the sudoers"), strings.Contains(stderr, "may not run sudo"), strings.Contains(stderr, "not allowed"):
		return "sudo does not permit this user to run ccsbx-root (see /etc/sudoers.d/90-ccsbx-root)"
	case strings.Contains(stderr, "command not found"), strings.Contains(stderr, "No such file or directory"):
		return "sudo could not run " + start.RootBin + " (is ccsbx-root installed?)"
	case strings.HasPrefix(strings.TrimSpace(stderr), "sudo:") || strings.Contains(stderr, "\nsudo:"):
		return "sudo refused: " + strings.TrimSpace(firstSudoLine(stderr))
	}
	return ""
}

func firstSudoLine(s string) string {
	for _, l := range strings.Split(s, "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "sudo:") {
			return l
		}
	}
	return s
}

// runHelper runs `sudo -n ccsbx-root <verb> <args...>` with this process's stdin/stdout, passes the
// helper's stderr through, and returns the exit code. A sudo failure is turned into exit 2 and a
// clear message; any other code is the helper's own (0 ok, 1 failed, 2 refused, 64 usage).
func runHelper(cmdName, verb string, args []string, stdout, stderr io.Writer) int {
	if p := start.HelperInstalled(); !p {
		fmt.Fprintf(stderr, "ccsbx %s: --root helper, but %s is not installed and root-owned (units/install-linux.sh)\n", cmdName, start.RootBin)
		return cli.ExitRefused
	}
	argv := append([]string{"-n", start.RootBin, verb}, args...)
	c := exec.Command(start.SudoBin, argv...)
	c.Env = []string{"PATH=/usr/bin:/bin", "TERM=" + os.Getenv("TERM"), "LANG=C.UTF-8"}
	var errb bytes.Buffer
	c.Stdin, c.Stdout, c.Stderr = os.Stdin, stdout, io.MultiWriter(stderr, &errb)
	err := c.Run()
	if err == nil {
		return cli.ExitOK
	}
	ee, ok := err.(*exec.ExitError)
	if !ok {
		fmt.Fprintf(stderr, "ccsbx %s: cannot run %s: %v\n", cmdName, start.SudoBin, err)
		return cli.ExitRefused
	}
	// the helper's own refusals and failures always begin "ccsbx-root <verb>:"; anything else on
	// stderr that sudo said is sudo's
	if !strings.Contains(errb.String(), "ccsbx-root ") {
		if why := sudoProblem(errb.String()); why != "" {
			fmt.Fprintf(stderr, "ccsbx %s: %s. Not falling back to direct mode.\n", cmdName, why)
			return cli.ExitRefused
		}
	}
	return ee.ExitCode()
}
