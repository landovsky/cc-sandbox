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
			{
				Name:    "run",
				Summary: "run one fence generation on the docker backend (U1b)",
				Recipes: "  ccsbx run --sid s01jd7k2m9 --gen 1 --image ccsbx-fence:dev --workdir /path/to/wt\n  CCSBX_ROOT=direct ccsbx run ...   # dev mode, as the calling user (not evidence)",
				Setup: func(f *cli.Flags) {
					f.String("sid", "", "session id minted by the daemon")
					f.Int("gen", 1, "generation number")
					f.String("image", "", "fence image reference (pinned)")
					f.String("workdir", "", "worktree to mount")
					f.String("root", "direct", "privilege mode: direct (dev, as the caller) or helper")
				},
				Run: notBuilt("U1b"),
			},
			{
				Name:    "start",
				Summary: "start a session from a LaunchSpec (U2)",
				Recipes: "  ccsbx start --spec tests/specs/hello.json --json\n  cat spec.json | ccsbx start --spec -",
				Setup: func(f *cli.Flags) {
					f.String("spec", "-", "LaunchSpec JSON file, or - for stdin")
					f.Bool("json", false, "print the StartReply as JSON")
				},
				Run: notBuilt("U2"),
			},
			selftestCmd(),
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
