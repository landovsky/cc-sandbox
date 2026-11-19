// Command ccsbx is the cc-sandbox CLI: start fenced sessions, run the fence backend, and run
// the rule self-test. Zero third-party modules; every flag also reads CCSBX_<FLAG>.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/landovsky/cc-sandbox/internal/cli"
)

var version = "dev"

// exitNotBuilt is returned by commands whose piece has not landed yet, so a caller can tell
// "not built" from "failed" (1) and "refused" (2).
const exitNotBuilt = 70

func notBuilt(piece string) func(*cli.Flags, []string, io.Writer, io.Writer) int {
	return func(_ *cli.Flags, _ []string, _, stderr io.Writer) int {
		fmt.Fprintf(stderr, "ccsbx: not built yet (piece %s)\n", piece)
		return exitNotBuilt
	}
}

func app() *cli.App {
	return &cli.App{
		Name:    "ccsbx",
		Summary: "fenced Claude Code sessions (cc-sandbox)",
		Version: version,
		Cmds: []*cli.Command{
			runCmd(),
			startCmd(),
			deliverCmd(),
			lsCmd(),
			eventsCmd(),
			cursorCmd(),
			pendingCmd(),
			cockpitCmd(),
			selftestCmd(),
			settingsCmd(),
			{
				Name:    "steering-check",
				Summary: "flag changes to steering files in a diff (H1)",
				Setup:   func(f *cli.Flags) { f.String("base", "main", "base ref to diff against") },
				Run:     notBuilt("H1"),
			},
		},
	}
}

func main() { os.Exit(app().Main(os.Args[1:], os.Stdout, os.Stderr)) }
