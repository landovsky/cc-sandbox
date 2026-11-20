// `ccsbx attach`: the owner's way into a fenced session's pane (C0a). It exists because the pane
// lives on the ccsbx tmux server, not the user's, so a plain tmux attach would not find it; the
// command wraps the nested attach and keeps the outer tmux's keys out of the way.

package main

import (
	"fmt"
	"github.com/landovsky/cc-sandbox/internal/tmuxsock"
	"io"
	"net"
	"os"
	"os/exec"
	"regexp"
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/cli"
)

// socketReachable: this user can connect to the unix socket.
func socketReachable(path string) bool {
	c, err := net.DialTimeout("unix", path, time.Second)
	if err != nil {
		return false
	}
	c.Close()
	return true
}

var attachSid = regexp.MustCompile(`^s[0-9a-z]{4,32}$`)

func attachCmd() *cli.Command {
	return &cli.Command{
		Name:    "attach",
		Summary: "attach this terminal to a session's pane (tmux -L ccsbx), in place",
		Recipes: `  ccsbx attach s0k1dry66qg            what 'ssh -t <machine> ccsbx attach <sid>' runs for a remote attach
  ccsbx attach s0k1dry66qg --read-only    watch without typing
  It execs tmux, so it behaves like the attach window cc-tree opens: detach with the outer
  tmux's own controls, close the window to let go; the session keeps running.`,
		Setup: func(f *cli.Flags) {
			f.String("tmux-socket", "ccsbx", "the tmux server the panes live on: a bare name is tmux -L NAME, an absolute path is tmux -S PATH (the service uid's socket: reached through ccsbx-root attach, exit 2 if that is not installed)")
			f.Bool("read-only", false, "attach read-only (no keys reach the session)")
		},
		Run: func(f *cli.Flags, args []string, _, stderr io.Writer) int {
			if len(args) != 1 || !attachSid.MatchString(args[0]) {
				fmt.Fprintln(stderr, "usage: ccsbx attach <sid>   (a session id like s0k1dry66qg; ccsbx ls lists them)")
				return cli.ExitUsage
			}
			sock := f.Lookup("tmux-socket").Value.String()
			// A shared socket path is the service uid's tmux (0600, owned by that uid): the owner's
			// uid can't, and must not be able to, talk to it directly. The way in is
			// `ccsbx-root attach <sid>`, a narrow entry that checks the sid against the daemon's
			// record and attaches that one session as the service uid. Only when the socket is
			// reachable (a dev run as the same uid) is it attached directly.
			if tmuxsock.IsPath(sock) && !socketReachable(sock) {
				if root, err := exec.LookPath("ccsbx-root"); err == nil {
					if f.Lookup("read-only").Value.String() == "true" {
						fmt.Fprintln(stderr, "ccsbx attach: --read-only is not available through ccsbx-root attach")
						return cli.ExitUsage
					}
					if err := syscall.Exec(root, []string{"ccsbx-root", "attach", args[0]}, os.Environ()); err != nil {
						fmt.Fprintln(stderr, "ccsbx attach:", err)
						return cli.ExitFail
					}
					return cli.ExitOK
				}
				fmt.Fprintf(stderr, "ccsbx attach: the tmux socket %s is not reachable by this user (it belongs to the service uid, mode 0600).\n"+
					"  The way in is:  ccsbx-root attach %s\n  but ccsbx-root is not installed here (Gate M).\n", sock, args[0])
				return 2
			}
			bin, err := exec.LookPath("tmux")
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx attach: tmux isn't installed here")
				return cli.ExitFail
			}
			argv := append(append([]string{"tmux"}, tmuxsock.Args(sock)...), "attach", "-t", "="+args[0]+":")
			if f.Lookup("read-only").Value.String() == "true" {
				argv = append(argv, "-r")
			}
			if err := syscall.Exec(bin, argv, os.Environ()); err != nil {
				fmt.Fprintln(stderr, "ccsbx attach:", err)
				return cli.ExitFail
			}
			return cli.ExitOK
		},
	}
}
