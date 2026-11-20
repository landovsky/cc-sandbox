package main

import (
	"fmt"
	"io"

	"github.com/landovsky/cc-sandbox/internal/backend"
	"github.com/landovsky/cc-sandbox/internal/cli"
)

func gcCmd() *cli.Command {
	return &cli.Command{
		Name:    "gc",
		Summary: "remove containers and networks of generations whose 'ccsbx run' died (SIGKILL, crash)",
		Recipes: `  ccsbx gc --dry-run          # list what would go
  ccsbx gc                    # remove it and mark those generations CLOSED
  Only resources labelled ccsbx.sid whose sid has a session record under --state are considered, and
  only when the run that owns the generation is gone. A live run, and anything unknown, is left alone.`,
		Setup: func(f *cli.Flags) {
			f.String("state", defaultState(), "daemon state directory; only its sessions are considered")
			f.String("root", "auto", modeFlagHelp)
			f.String("docker", "docker", "docker binary")
			f.Bool("dry-run", false, "list orphans, remove nothing")
		},
		Run: func(f *cli.Flags, _ []string, stdout, stderr io.Writer) int {
			get := func(n string) string { return f.Lookup(n).Value.String() }
			mode, err := resolveMode(f)
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx gc:", err)
				return cli.ExitRefused
			}
			if mode == "helper" {
				var a []string
				if get("dry-run") == "true" {
					a = []string{"--dry-run"}
				}
				return runHelper("gc", "gc", a, stdout, stderr)
			}
			if err := directDockerReachable(); err != nil {
				fmt.Fprintln(stderr, "ccsbx gc:", err)
				return cli.ExitRefused
			}
			r := &backend.Runner{Docker: get("docker")}
			orphans, err := r.Orphans(get("state"))
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx gc:", err)
				return cli.ExitFail
			}
			for _, o := range orphans {
				fmt.Fprintf(stdout, "orphan %s %s (sid %s g%d)\n", o.Kind, o.Name, o.Sid, o.Gen)
			}
			if get("dry-run") == "true" || len(orphans) == 0 {
				fmt.Fprintf(stdout, "%d orphan(s)\n", len(orphans))
				return cli.ExitOK
			}
			if err := r.Reap(get("state"), orphans); err != nil {
				fmt.Fprintln(stderr, "ccsbx gc:", err)
				return cli.ExitFail
			}
			fmt.Fprintf(stdout, "removed %d orphan(s)\n", len(orphans))
			return cli.ExitOK
		},
	}
}
