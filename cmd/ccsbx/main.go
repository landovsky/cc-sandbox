// Command ccsbx is the cc-sandbox CLI: start fenced sessions, run the fence backend, and run
// the rule self-test. Zero third-party modules; every flag also reads CCSBX_<FLAG>.
package main

import (
	"io"
	"os"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/fwd"
	"github.com/landovsky/cc-sandbox/internal/stub"
	"github.com/landovsky/cc-sandbox/internal/token"
)

var version = "dev"

func app() *cli.App {
	return &cli.App{
		Name:    "ccsbx",
		Summary: "fenced Claude Code sessions (cc-sandbox)",
		Version: version,
		Cmds: []*cli.Command{
			runCmd(),
			gcCmd(),
			pinCheckCmd(),
			pinPromoteCmd(),
			startCmd(),
			deliverCmd(),
			lsCmd(),
			eventsCmd(),
			cursorCmd(),
			pendingCmd(),
			cockpitCmd(),
			attachCmd(),
			promptCmd(),
			replaceCmd(),
			selftestCmd(),
			settingsCmd(),
			{
				Name: "fwd", Raw: true,
				Summary: "the per-session forwarder: holds the token, signs door calls (P2)",
				Run:     func(_ *cli.Flags, a []string, o, e io.Writer) int { return fwd.Main(a, os.Stdin, o, e) },
			},
			{
				Name: "stub", Raw: true,
				Summary: "scripted model stub, local mode, e2e-* profiles only (C18)",
				Run:     func(_ *cli.Flags, a []string, o, e io.Writer) int { return stub.Main(a, o, e) },
			},
			{
				Name: "token", Raw: true,
				Summary: "mint and verify session tokens, dev key file (U5)",
				Run:     func(_ *cli.Flags, a []string, o, e io.Writer) int { return token.Main(a, o, e) },
			},
			steeringCheckCmd(), seccompCmd(),
		},
	}
}

func main() { os.Exit(app().Main(os.Args[1:], os.Stdout, os.Stderr)) }
