// Command ccsbx-root is the one NOPASSWD sudo target (U0): the fixed verbs run, stop, renew, gc,
// attach and exec-token, each checked against root-owned policy (internal/rootcmd). It exists so
// that no caller needs the docker group. main only rebuilds the process environment, loads the
// root-owned config and hands over; every decision is in internal/rootcmd, where the tests are.
package main

import (
	"fmt"
	"os"
	"strings"
	"syscall"

	"github.com/landovsky/cc-sandbox/internal/rootcmd"
)

var version = "dev"

func main() { os.Exit(realMain(os.Args[1:])) }

func realMain(args []string) int {
	// keep only what the program is allowed to read from the caller's environment, then drop the
	// rest: PATH, HOME, TMUX, LD_*, DOCKER_* and the like never reach docker or tmux
	keep := map[string]string{}
	for _, kv := range os.Environ() {
		k, v, _ := strings.Cut(kv, "=")
		if k == "SUDO_UID" || k == "TERM" || strings.HasPrefix(k, "CCSBX_ROOT_") {
			keep[k] = v
		}
	}
	os.Clearenv()
	os.Setenv("PATH", "/usr/bin:/bin")
	os.Setenv("HOME", "/nonexistent")
	os.Setenv("DOCKER_CONFIG", "/nonexistent")
	syscall.Umask(0o022)
	_ = syscall.Setrlimit(syscall.RLIMIT_CORE, &syscall.Rlimit{})
	getenv := func(k string) string { return keep[k] }

	cfg, err := rootcmd.LoadConfig(rootcmd.DefaultConfigPath, 0)
	if err != nil {
		// --help and version run nothing, so they need no config; everything else refuses
		if !runsNothing(args) {
			fmt.Fprintln(os.Stderr, "ccsbx-root: refused:", err)
			return 2
		}
		cfg = &rootcmd.Config{Verbs: rootcmd.Verbs}
	}
	app := rootcmd.App(cfg, rootcmd.Real(getenv), getenv)
	app.Version = version
	return app.Main(args, os.Stdout, os.Stderr)
}

// runsNothing: the arguments ask for help or the version, which never reach a verb.
func runsNothing(args []string) bool {
	if len(args) == 0 {
		return true // usage, exit 64
	}
	switch args[0] {
	case "-h", "--help", "help", "version", "--version":
		return true
	}
	for _, a := range args[1:] {
		if a == "--" {
			return false
		}
		if a == "-h" || a == "--help" || a == "-help" {
			return true
		}
	}
	return false
}
