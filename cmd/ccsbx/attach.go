package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"syscall"

	"github.com/landovsky/cc-sandbox/internal/cli"
)

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
			f.String("tmux-socket", "ccsbx", "tmux -L socket the panes live on")
			f.Bool("read-only", false, "attach read-only (no keys reach the session)")
		},
		Run: func(f *cli.Flags, args []string, _, stderr io.Writer) int {
			if len(args) != 1 || !attachSid.MatchString(args[0]) {
				fmt.Fprintln(stderr, "usage: ccsbx attach <sid>   (a session id like s0k1dry66qg; ccsbx ls lists them)")
				return cli.ExitUsage
			}
			bin, err := exec.LookPath("tmux")
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx attach: tmux isn't installed here")
				return cli.ExitFail
			}
			argv := []string{"tmux", "-L", f.Lookup("tmux-socket").Value.String(), "attach", "-t", "=" + args[0] + ":"}
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
