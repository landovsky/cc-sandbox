// Command ccsbx is the cc-sandbox CLI: start fenced sessions, run the fence backend, and run
// the rule self-test. Zero third-party modules; every flag also reads CCSBX_<FLAG>.
package main

import (
	"os"

	"github.com/landovsky/cc-sandbox/internal/cli"
)

var version = "dev"

func app() *cli.App {
	return &cli.App{
		Name:    "ccsbx",
		Summary: "fenced Claude Code sessions (cc-sandbox)",
		Version: version,
		Cmds: []*cli.Command{
			runCmd(),
			pinCheckCmd(),
			pinPromoteCmd(),
			startCmd(),
			selftestCmd(),
			settingsCmd(),
			steeringCheckCmd(),
		},
	}
}

func main() { os.Exit(app().Main(os.Args[1:], os.Stdout, os.Stderr)) }
