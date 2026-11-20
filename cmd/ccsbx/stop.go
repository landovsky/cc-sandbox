// stop.go is `ccsbx stop <sid>`: end one session's current generation. It exists because in helper
// mode the owner cannot run docker, so stopping a fence is also a request to ccsbx-root (U1c).

package main

import (
	"fmt"
	"io"

	"github.com/landovsky/cc-sandbox/internal/backend"
	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/start"
)

func stopCmd() *cli.Command {
	return &cli.Command{
		Name:    "stop",
		Summary: "remove the container and network of one session's current generation",
		Recipes: `  ccsbx stop s0k1dry66qg
  Helper mode (the default once ccsbx-root is installed): sudo -n /usr/local/sbin/ccsbx-root stop <sid>.
  Direct mode: docker rm of exactly ccsbx-<sid>-g<gen> and its network, from the record under --state.
  Neither mode touches the tmux pane or the record. Helper mode also clears setid bits the fence left
  in the workdir (ccsbx-root does that); direct mode does not. --profiles, --pins and --worktree-root
  are accepted but unused here.`,
		Setup: func(f *cli.Flags) {
			stateFlags(f)
			f.String("docker", "docker", "docker binary (direct mode)")
		},
		Run: func(f *cli.Flags, args []string, stdout, stderr io.Writer) int {
			if len(args) != 1 || !start.ValidSid(args[0]) {
				fmt.Fprintln(stderr, "usage: ccsbx stop <sid>   (a session id like s0k1dry66qg)")
				return cli.ExitUsage
			}
			mode, err := resolveMode(f)
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx stop:", err)
				return cli.ExitRefused
			}
			if mode == "helper" {
				return runHelper("stop", "stop", []string{"--", args[0]}, stdout, stderr)
			}
			if err := directDockerReachable(); err != nil {
				fmt.Fprintln(stderr, "ccsbx stop:", err)
				return cli.ExitRefused
			}
			d, err := daemonFrom(f)
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx stop:", err)
				return cli.ExitFail
			}
			rec, err := d.Load(args[0])
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx stop:", err)
				return cli.ExitFail
			}
			if err := (&backend.Runner{Docker: f.Lookup("docker").Value.String()}).Stop(backend.Spec{Sid: rec.Sid, Gen: rec.Gen, StateDir: d.StateDir}); err != nil {
				fmt.Fprintln(stderr, "ccsbx stop:", err)
				return cli.ExitFail
			}
			fmt.Fprintf(stdout, "stopped %s g%d\n", rec.Sid, rec.Gen)
			return cli.ExitOK
		},
	}
}
