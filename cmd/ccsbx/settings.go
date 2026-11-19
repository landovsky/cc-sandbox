package main

import (
	"fmt"
	"io"
	"os"
	"path/filepath"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/profile"
	"github.com/landovsky/cc-sandbox/internal/settings"
)

func settingsCmd() *cli.Command {
	return &cli.Command{
		Name:    "settings",
		Summary: "compile a profile into managed-settings.json + managed-mcp.json (U17)",
		Recipes: `  ccsbx settings --profile dev-default --base-url http://fwd:8080 --out /tmp/managed
  ccsbx settings --profile dev-default --base-url http://fwd:8080 --out -   # settings to stdout`,
		Setup: func(f *cli.Flags) {
			f.String("profiles", "profiles", "directory of <name>.json profiles")
			f.String("profile", "dev-default", "profile to compile")
			f.String("base-url", "", "the forwarder's model route (required)")
			f.String("fetch-url", "", "the forwarder's fetch route, becomes HTTPS_PROXY (optional)")
			f.String("ccf-cmd", "", "command of the managed ccf MCP server (default /opt/ccf/bin/ccf-mcp)")
			f.String("out", "-", "directory to write both files into, or - to print managed-settings.json")
		},
		Run: func(f *cli.Flags, _ []string, stdout, stderr io.Writer) int {
			get := func(n string) string { return f.Lookup(n).Value.String() }
			p, err := profile.Load(get("profiles"), get("profile"))
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx settings:", err)
				return cli.ExitFail
			}
			r, err := settings.Compile(p, settings.Params{BaseURL: get("base-url"), FetchURL: get("fetch-url"), CCFCmd: get("ccf-cmd")})
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx settings:", err)
				return cli.ExitRefused
			}
			if out := get("out"); out != "-" {
				if err := os.MkdirAll(out, 0o755); err != nil {
					fmt.Fprintln(stderr, "ccsbx settings:", err)
					return cli.ExitFail
				}
				for name, b := range map[string][]byte{"managed-settings.json": r.Settings, "managed-mcp.json": r.MCP} {
					if err := os.WriteFile(filepath.Join(out, name), b, 0o644); err != nil {
						fmt.Fprintln(stderr, "ccsbx settings:", err)
						return cli.ExitFail
					}
				}
				return cli.ExitOK
			}
			stdout.Write(r.Settings)
			return cli.ExitOK
		},
	}
}
