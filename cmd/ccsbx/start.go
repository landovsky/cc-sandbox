// start.go is `ccsbx start`: it asks the (direct-mode) daemon to mint a session and records it.
// It exists so the sid, generation and refusals are decided in one place, not by each caller.

package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/start"
)

// stateFlags are shared by `start` and `run`: both must see the same state directory.
// defaultState is the daemon state directory unless --state or CCSBX_STATE says otherwise.
func defaultState() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".local", "state", "ccsbx")
}

func stateFlags(f *cli.Flags) {
	home, _ := os.UserHomeDir()
	f.String("state", defaultState(), "daemon state directory (session records, lock)")
	f.String("profiles", "profiles", "directory of <name>.json profiles")
	f.String("machine", hostname(), "this machine's name; a spec for another machine is refused")
	f.String("worktree-root", home, "comma-separated directories (like ccsbx-root.toml allowed_roots): every worktree must be strictly inside one (symlinks resolved); hidden dirs of the home are refused when a root is the home")
	f.String("pins", "pins.toml", "production pins file (digests that may run under any profile)")
	f.String("root", "auto", modeFlagHelp)
}

func hostname() string { h, _ := os.Hostname(); return h }

func daemonFrom(f *cli.Flags) (*start.Daemon, error) {
	get := func(n string) string { return f.Lookup(n).Value.String() }
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			return p
		}
		return a
	}
	mode, err := start.ResolveRoot(get("root"))
	if err != nil {
		return nil, err
	}
	exe, err := os.Executable()
	if err != nil {
		return nil, err
	}
	d := &start.Daemon{
		StateDir: abs(get("state")), ProfilesDir: abs(get("profiles")), Machine: get("machine"), Uid: os.Getuid(),
		Backend: "docker", Root: mode, Exe: exe, PinsFile: abs(get("pins")), WorktreeRoot: start.AbsRoots(get("worktree-root")),
	}
	return d, nil
}

func startCmd() *cli.Command {
	return &cli.Command{
		Name:    "start",
		Summary: "start a fenced session from a LaunchSpec; the daemon mints sid and gen (U2)",
		Recipes: `  ccsbx start --spec tests/specs/hello.json --json     # StartReply on stdout, exit 0
  cat spec.json | ccsbx start --spec -
  Refusals exit 2 and print the code: profile_not_approved, rate_capped, billing_unavailable,
  machine_full, already_running, wrong_machine, bad_spec, not_built.
  Direct mode: the pane runs in 'tmux -L ccsbx' as you; replies carry not_evidence=true.
  Helper mode (--root helper, or auto once ccsbx-root is installed): the spec is sent to ccsbx-hostd
  (POST /v1/start on --socket), which writes the record in its own state directory and launches the
  pane (sudo -n ccsbx-root run). Nothing is written under your home. hostd not running is exit 2.
  --launcher, --run-cmd, --image, --profiles, --pins, --worktree-root and --max-* are dev/direct
  flags: hostd uses its own settings, and --launcher/--run-cmd are refused in helper mode.`,
		Setup: func(f *cli.Flags) {
			stateFlags(f)
			f.String("socket", defaultHostdSocket, "helper mode: ccsbx-hostd's local unix socket; the daemon writes the record and launches the pane (CCSBX_SOCKET)")
			f.String("spec", "-", "LaunchSpec JSON file, or - for stdin")
			f.Bool("json", false, "print the StartReply (or the refusal) as JSON")
			f.String("image", "ccsbx-fence:dev", "fence image recorded in the pin and run by the pane")
			f.String("cc-version", "", "Claude Code version recorded in the pin")
			f.String("tmux-socket", "ccsbx", "tmux -L socket name for panes")
			f.String("launcher", "tmux", "tmux: run the pane; none: write the record only (tests run 'ccsbx run --sid' themselves)")
			f.String("run-cmd", "", "command the pane runs in the fence instead of claude, space-separated (tests, debugging)")
			f.Int("max-live", 8, "machine_full above this many live sessions")
			f.Int("max-per-hour", 20, "rate_capped above this many starts per caller per hour")
		},
		Run: runStart,
	}
}

func runStart(f *cli.Flags, _ []string, stdout, stderr io.Writer) int {
	get := func(n string) string { return f.Lookup(n).Value.String() }
	asJSON := get("json") == "true"
	var in []byte
	var err error
	if p := get("spec"); p == "-" {
		in, err = io.ReadAll(os.Stdin)
	} else {
		in, err = os.ReadFile(p)
	}
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx start:", err)
		return cli.ExitFail
	}
	mode, err := resolveMode(f)
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx start:", err)
		return cli.ExitRefused
	}
	if mode == "helper" {
		// The record must be written by the daemon (uid ccsbx, /var/lib/ccsbx), which is what
		// ccsbx-root reads: never by this process under the caller's home.
		return startViaHostd(f, in, asJSON, stdout, stderr)
	}
	d, err := daemonFrom(f)
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx start:", err)
		return cli.ExitFail
	}
	d.Image, d.CCVersion, d.Socket = get("image"), get("cc-version"), get("tmux-socket")
	fmt.Sscanf(get("max-live"), "%d", &d.MaxLive)
	fmt.Sscanf(get("max-per-hour"), "%d", &d.MaxPerHour)
	env := []string{"--state", d.StateDir, "--profiles", d.ProfilesDir, "--pins", d.PinsFile, "--worktree-root", d.WorktreeRoot, "--machine", d.Machine, "--image", d.Image, "--root", d.Root}
	if c := strings.Fields(get("run-cmd")); len(c) > 0 {
		env = append(append(env, "--"), c...)
	}
	switch get("launcher") {
	case "tmux":
		d.Launcher = &start.TmuxLauncher{Exe: d.Exe, Socket: get("tmux-socket"), Env: env, Helper: d.Root == "helper"}
	case "none":
		d.Launcher = start.NoLauncher{}
	default:
		fmt.Fprintf(stderr, "ccsbx start: --launcher must be tmux or none, got %q\n", get("launcher"))
		return cli.ExitUsage
	}

	spec, err := start.ParseSpec(in)
	var rep *start.StartReply
	if err == nil {
		rep, err = d.Start(spec)
	}
	if err != nil {
		if r, ok := err.(*start.Refusal); ok {
			if asJSON {
				b, _ := json.Marshal(r)
				fmt.Fprintln(stdout, string(b))
			}
			fmt.Fprintf(stderr, "ccsbx start: refused: %s\n", r)
			return cli.ExitRefused
		}
		fmt.Fprintln(stderr, "ccsbx start:", err)
		return cli.ExitFail
	}
	if asJSON {
		b, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(stdout, string(b))
	} else {
		fmt.Fprintf(stdout, "started %s (%s) attach: %v\n", rep.Sid, rep.ID, rep.Attach.Local)
	}
	return cli.ExitOK
}
