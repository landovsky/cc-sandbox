// start.go is `ccsbx start`: it asks the (direct-mode) daemon to mint a session and records it.
// It exists so the sid, generation and refusals are decided in one place, not by each caller.

package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/profile"
	"github.com/landovsky/cc-sandbox/internal/remotestart"
	"github.com/landovsky/cc-sandbox/internal/start"
	"github.com/landovsky/cc-sandbox/profiles"
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
	f.String("profiles", "profiles", "directory of <name>.json profiles; '-' means the profiles compiled into this binary (what hostd uses and passes to the panes it starts)")
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
	profilesDir, loader := abs(get("profiles")), (func(string) (*profile.Profile, error))(nil)
	if get("profiles") == "-" {
		profilesDir, loader = "-", profiles.Load // the embedded copy, not a directory anyone can edit
	}
	d := &start.Daemon{
		ProfileLoader: loader,
		StateDir:      abs(get("state")), ProfilesDir: profilesDir, Machine: get("machine"), Uid: os.Getuid(),
		Backend: "docker", Root: mode, Runtime: runtimeFor(mode), Exe: exe, PinsFile: abs(get("pins")), WorktreeRoot: start.AbsRoots(get("worktree-root")),
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
		rep, err = guardedStart(d, spec)
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

// runtimeFor is how the stopped proof asks whether a container runs: docker in direct mode, the
// read-only `ccsbx-root status` in helper mode (the caller has no docker there).
func runtimeFor(mode string) start.Runtime {
	if mode == "helper" {
		return start.HelperRuntime{}
	}
	return start.DockerRuntime{Docker: "docker"}
}

// guardedStart is the direct-mode start: when hostd published a peer list, this start reserves the label and asks
// every peer first, exactly as a start through hostd does (one live generation per label across machines, whichever
// way the start arrives). A peers file that exists but cannot be read refuses the start: never "no check".
func guardedStart(d *start.Daemon, spec *start.LaunchSpec) (*start.StartReply, error) {
	pub, err := remotestart.LoadPublished(d.StateDir)
	if err != nil {
		return nil, &start.Refusal{Code: remotestart.Internal, Msg: "the cross-machine peer list cannot be read: refusing to start without the label check"}
	}
	if pub == nil || len(pub.Peers) == 0 || d.Recorded(d.Uid, "", spec.RequestID) != nil {
		return d.Start(spec) // no peers configured, or a retry of a start already recorded (answered by Start itself)
	}
	svc, err := remotestart.New(remotestart.Config{
		Machine: pub.Machine, Peers: pub.Peers, PeerSet: pub.PeerSet, StateDir: filepath.Join(d.StateDir, "remote"),
		AskTimeout: time.Duration(pub.AskTimeoutMs) * time.Millisecond,
		Live: func(label string) ([]remotestart.View, error) {
			recs, err := d.LiveByLabel(label)
			var out []remotestart.View
			for _, r := range recs {
				out = append(out, remotestart.View{Machine: pub.Machine, Sid: r.Sid, Gen: r.Gen, State: "running"})
			}
			return out, err
		},
	})
	if err != nil {
		return nil, err
	}
	rep, ref := svc.Guard(context.Background(), spec.Label, "local:cli", func() (*start.StartReply, error) { return d.Start(spec) })
	if ref != nil {
		return nil, ref
	}
	return rep, nil
}
