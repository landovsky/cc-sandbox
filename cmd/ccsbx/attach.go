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
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/start"
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

// attachExec replaces the process; tests replace it.
var attachExec = syscall.Exec

func attachCmd() *cli.Command {
	return &cli.Command{
		Name:    "attach",
		Summary: "attach this terminal to a session's pane (tmux -L ccsbx), in place",
		Recipes: `  ccsbx attach s0k1dry66qg            what 'ssh -t <machine> ccsbx attach <sid>' runs for a remote attach
  ccsbx attach --read-only s0k1dry66qg    watch without typing (flags come before the sid)
  It execs tmux, so it behaves like the attach window cc-tree opens: detach with the outer
  tmux's own controls, close the window to let go; the session keeps running.`,
		Setup: func(f *cli.Flags) {
			f.String("tmux-socket", "ccsbx", "the tmux server the panes live on: a bare name is tmux -L NAME, an absolute path is tmux -S PATH (the service uid's socket: reached through ccsbx-root attach, exit 2 if that is not installed)")
			f.Bool("read-only", false, "attach read-only (no keys reach the session)")
			f.String("root", "auto", modeFlagHelp)
		},
		Run: func(f *cli.Flags, args []string, _, stderr io.Writer) int {
			if len(args) != 1 || !start.ValidSid(args[0]) {
				fmt.Fprintln(stderr, "usage: ccsbx attach <sid>   (a session id like s0k1dry66qg; ccsbx ls lists them)")
				return cli.ExitUsage
			}
			mode, err := resolveMode(f)
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx attach:", err)
				return cli.ExitRefused
			}
			sock := f.Lookup("tmux-socket").Value.String()
			if mode == "helper" {
				// The panes live in the service uid's tmux (socket 0600): the owner's uid can't, and
				// must not be able to, talk to it. The way in is `ccsbx-root attach <sid>`, which
				// checks the sid against the daemon's record and attaches that one session. It is
				// reached through sudo, by absolute path, whatever --tmux-socket says.
				if f.Lookup("read-only").Value.String() == "true" {
					fmt.Fprintln(stderr, "ccsbx attach: --read-only is not available through ccsbx-root attach")
					return cli.ExitUsage
				}
				if !start.HelperInstalled() {
					fmt.Fprintf(stderr, "ccsbx attach: --root helper, but %s is not installed and root-owned\n", start.RootBin)
					return cli.ExitRefused
				}
				if why := sudoPreflight(); why != "" {
					fmt.Fprintf(stderr, "ccsbx attach: %s. Not falling back to direct mode.\n", why)
					return cli.ExitRefused
				}
				env := []string{"PATH=/usr/bin:/bin", "TERM=" + os.Getenv("TERM"), "LANG=C.UTF-8"}
				if err := attachExec(start.SudoBin, []string{"sudo", "-n", start.RootBin, "attach", "--", args[0]}, env); err != nil {
					fmt.Fprintln(stderr, "ccsbx attach:", err)
					return cli.ExitFail
				}
				return cli.ExitOK
			}
			if tmuxsock.IsPath(sock) && !socketReachable(sock) {
				fmt.Fprintf(stderr, "ccsbx attach: the tmux socket %s is not reachable by this user (it belongs to the service uid, mode 0600).\n"+
					"  The way in is:  ccsbx attach --root helper %s   (sudo -n ccsbx-root attach), once ccsbx-root is installed (Gate M).\n", sock, args[0])
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
