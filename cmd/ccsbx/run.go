package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/landovsky/cc-sandbox/internal/backend"
	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/profile"
	"github.com/landovsky/cc-sandbox/internal/settings"
)

func runCmd() *cli.Command {
	return &cli.Command{
		Name:    "run",
		Summary: "run one fence generation on the docker backend, in the foreground (U1b)",
		Recipes: `  ccsbx run --sid s01jd7k2m9                 # claude in the fence; the pane of 'ccsbx start' runs this
  ccsbx run --sid S -- sh -c 'id; ls /run/ccsbx'   # any command instead of claude (tests, debugging)
  The sid must exist: 'ccsbx start' mints it and writes <state>/sessions/<sid>.json, which run
  reads (image, workdir, profile caps). Direct mode: runs as you via docker; results are not
  evidence. On exit the container and its network are removed.`,
		Setup: func(f *cli.Flags) {
			stateFlags(f)
			f.String("sid", "", "session id minted by 'ccsbx start' (required)")
			f.Int("gen", 1, "generation number (the record's gen)")
			f.String("image", "ccsbx-fence:dev", "fence image reference")
			f.String("base-url", "http://fwd:8080", "the forwarder's model route written into managed settings")
			f.String("fetch-url", "", "the forwarder's fetch route (HTTPS_PROXY), optional")
			f.String("docker", "docker", "docker binary")
		},
		Run: runRun,
	}
}

func runRun(f *cli.Flags, args []string, stdout, stderr io.Writer) int {
	get := func(n string) string { return f.Lookup(n).Value.String() }
	if get("sid") == "" {
		fmt.Fprintln(stderr, "ccsbx run: --sid is required (see ccsbx run --help)")
		return cli.ExitUsage
	}
	d, err := daemonFrom(f)
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx run:", err)
		return cli.ExitFail
	}
	rec, err := d.Load(get("sid"))
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx run:", err)
		return cli.ExitFail
	}
	var gen int
	fmt.Sscanf(get("gen"), "%d", &gen)
	if gen != rec.Gen {
		fmt.Fprintf(stderr, "ccsbx run: gen %d is not the record's gen %d\n", gen, rec.Gen)
		return cli.ExitRefused
	}
	p, err := profile.Load(d.ProfilesDir, rec.Spec.Profile)
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx run:", err)
		return cli.ExitFail
	}
	res, err := settings.Compile(p, settings.Params{BaseURL: get("base-url"), FetchURL: get("fetch-url")})
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx run:", err)
		return cli.ExitFail
	}
	sess, _ := json.MarshalIndent(map[string]any{
		"id": rec.Reply.ID, "sid": rec.Sid, "gen": rec.Gen, "label": rec.Label, "machine": d.Machine,
		"owner": rec.Spec.Owner, "billing": rec.Spec.Billing, "profile": rec.Spec.Profile, "bus_id": "",
	}, "", "  ")
	files := map[string][]byte{
		"managed/managed-settings.json": res.Settings, "managed/managed-mcp.json": res.MCP,
		"in/session.json": sess, "in/scope.json": append(append([]byte(nil), rec.Spec.MsgScope...), '\n'),
	}
	if rec.Spec.Brief != nil {
		files["in/brief.md"] = []byte(rec.Spec.Brief.Text + "\n")
	}
	cmd := args
	if len(cmd) == 0 {
		cmd = append([]string{"claude"}, rec.Spec.HarnessArgs...)
	}
	tty := isTerminal(os.Stdin.Fd())
	spec := backend.Spec{
		Sid: rec.Sid, Label: rec.Label, Gen: rec.Gen, Image: get("image"), StateDir: d.StateDir,
		Workdir: rec.Spec.Workdir.Path, GitDir: rec.GitDir, WorktreeRoot: d.WorktreeRoot,
		MemoryMB: p.MemoryMB, Pids: p.Pids, Cmd: cmd, TTY: tty, NotEvid: d.Root != "helper",
	}
	if spec.NotEvid {
		fmt.Fprintln(stderr, "ccsbx run: direct mode, NOT EVIDENCE (invariant 14 waived)")
	}
	r := &backend.Runner{Docker: get("docker"), Stdin: os.Stdin, Stdout: stdout, Stderr: stderr, Files: files}
	code, err := r.Run(spec)
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx run:", err)
		return cli.ExitFail
	}
	return code
}
