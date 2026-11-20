// pin.go wires `ccsbx pin-check` and `ccsbx pin-promote` to internal/pins. It exists so the harness
// and image pins (invariant 26) can be checked and promoted from the CLI, never by hand edit.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/pins"
	"github.com/landovsky/cc-sandbox/internal/profile"
)

func pinCheckCmd() *cli.Command {
	return &cli.Command{
		Name:    "pin-check",
		Summary: "say which channel an image may run on under a profile, or refuse it (H2)",
		Recipes: `  ccsbx pin-check --image ghcr.io/landovsky/ccsbx-fence@sha256:<64 hex> --profile dev-default
  ccsbx pin-check --image ccsbx-fence:dev --root direct     # a local dev tag: channel dev
  Exit 0 and the channel on stdout, or exit 2 with the reason.`,
		Setup: func(f *cli.Flags) {
			f.String("image", "", "image reference to check (required)")
			f.String("profile", "dev-default", "profile the session would run under")
			f.String("profiles", "profiles", "directory of <name>.json profiles")
			f.String("pins", "pins.toml", "production pins file")
			f.String("root", "direct", "direct allows local dev tags; anything else requires a digest")
		},
		Run: func(f *cli.Flags, _ []string, stdout, stderr io.Writer) int {
			get := func(n string) string { return f.Lookup(n).Value.String() }
			if get("image") == "" {
				fmt.Fprintln(stderr, "ccsbx pin-check: --image is required")
				return cli.ExitUsage
			}
			p, err := profile.Load(get("profiles"), get("profile"))
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx pin-check:", err)
				return cli.ExitFail
			}
			pl, err := pins.Load(get("pins"))
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx pin-check:", err)
				return cli.ExitFail
			}
			ch, err := pins.Check(pl, nil, get("image"), p, get("root") != "helper")
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx pin-check: refused:", err)
				return cli.ExitRefused
			}
			fmt.Fprintln(stdout, ch)
			return cli.ExitOK
		},
	}
}

func pinPromoteCmd() *cli.Command {
	return &cli.Command{
		Name:    "pin-promote",
		Summary: "pin a candidate digest as production, only after a green full suite on every required host (H2)",
		Recipes: `  ccsbx pin-promote --ref ghcr.io/landovsky/ccsbx-fence@sha256:<64 hex> --cc 2.1.329 --result result.json
  result.json: {"suite":"full","ref":"<the same ref>","hosts":{"hp-ubuntu":"green","future-two":"green"}}
  Add the Mac to --hosts only when it is in scope (plan Q12). A red or missing host refuses.`,
		Setup: func(f *cli.Flags) {
			f.String("ref", "", "digest reference to promote (required)")
			f.String("cc", "", "Claude Code version inside the image (required)")
			f.String("result", "", "suite result JSON file (required)")
			f.String("hosts", "hp-ubuntu,future-two", "hosts that must be green, comma-separated")
			f.String("pins", "pins.toml", "production pins file to append to")
		},
		Run: func(f *cli.Flags, _ []string, stdout, stderr io.Writer) int {
			get := func(n string) string { return f.Lookup(n).Value.String() }
			if get("ref") == "" || get("cc") == "" || get("result") == "" {
				fmt.Fprintln(stderr, "ccsbx pin-promote: --ref, --cc and --result are required")
				return cli.ExitUsage
			}
			b, err := os.ReadFile(get("result"))
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx pin-promote:", err)
				return cli.ExitFail
			}
			var res pins.Result
			if err := json.Unmarshal(b, &res); err != nil {
				fmt.Fprintln(stderr, "ccsbx pin-promote: result:", err)
				return cli.ExitFail
			}
			if err := pins.Promote(get("pins"), get("ref"), get("cc"), res, strings.Split(get("hosts"), ",")); err != nil {
				fmt.Fprintln(stderr, "ccsbx pin-promote:", err)
				return cli.ExitRefused
			}
			fmt.Fprintf(stdout, "pinned %s in %s\n", get("ref"), get("pins"))
			return cli.ExitOK
		},
	}
}
