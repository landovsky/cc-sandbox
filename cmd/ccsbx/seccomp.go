// `ccsbx seccomp-profile`: builds the fence's seccomp profile from Docker's own default profile by
// adding the rule that refuses setuid and setgid modes (internal/seccomp). It exists because
// Docker takes a whole profile, not an addition, and the default must be the one that matches the
// installed docker, which only the operator can supply. The result is installed root-owned for
// ccsbx-root, which verifies it before every run.
package main

import (
	"fmt"
	"io"
	"os"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/seccomp"
)

func seccompCmd() *cli.Command {
	return &cli.Command{
		Name:    "seccomp-profile",
		Summary: "make Docker's default seccomp profile refuse setuid/setgid modes (Gate M step)",
		Recipes: `  V=$(docker version --format '{{.Server.Version}}')
  curl -fsSL -o default.json https://raw.githubusercontent.com/moby/moby/v$V/profiles/seccomp/default.json
  ccsbx seccomp-profile --base default.json --out /tmp/seccomp.json
  sudo install -o root -g root -m 0644 /tmp/seccomp.json /etc/ccsbx/seccomp.json
  ccsbx seccomp-profile --verify /etc/ccsbx/seccomp.json     # what ccsbx-root checks before each run
  --base is moby's profiles/seccomp/default.json at the tag of the installed docker engine (URL above).
  The tool removes chmod, fchmod, fchmodat, fchmodat2, open, openat, creat, mknod, mknodat and openat2
  from every allow entry, adds the first nine back allowed only when (mode & 06000) == 0, and refuses
  openat2 (ENOSYS): a mode with a setuid or setgid bit then falls to the default action, which must be
  an EPERM denial. A base that does not deny by default is refused. --verify checks exactly that.`,
		Setup: func(f *cli.Flags) {
			f.String("base", "", "Docker's default seccomp profile (JSON) to extend")
			f.String("out", "-", "where to write the result; - is stdout")
			f.String("verify", "", "instead: check that this profile denies by default and has the setid rules")
		},
		Run: func(f *cli.Flags, _ []string, stdout, stderr io.Writer) int {
			get := func(n string) string { return f.Lookup(n).Value.String() }
			if v := get("verify"); v != "" {
				b, err := os.ReadFile(v)
				if err == nil {
					err = seccomp.Verify(b)
				}
				if err != nil {
					fmt.Fprintln(stderr, "ccsbx seccomp-profile:", err)
					return cli.ExitRefused
				}
				fmt.Fprintln(stdout, "ok")
				return cli.ExitOK
			}
			if get("base") == "" {
				fmt.Fprintln(stderr, "ccsbx seccomp-profile: --base is required (see --help)")
				return cli.ExitUsage
			}
			b, err := os.ReadFile(get("base"))
			if err == nil {
				b, err = seccomp.Overlay(b)
			}
			if err != nil {
				fmt.Fprintln(stderr, "ccsbx seccomp-profile:", err)
				return cli.ExitFail
			}
			if o := get("out"); o != "-" {
				if err := os.WriteFile(o, append(b, '\n'), 0o644); err != nil {
					fmt.Fprintln(stderr, "ccsbx seccomp-profile:", err)
					return cli.ExitFail
				}
				return cli.ExitOK
			}
			fmt.Fprintln(stdout, string(b))
			return cli.ExitOK
		},
	}
}
