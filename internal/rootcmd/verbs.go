// The verbs of ccsbx-root: run, stop, gc, attach, and the two that are not wired yet. Each takes
// at most one session id and a flag or two; a flag nobody listed is a usage error (exit 64), a
// policy refusal is exit 2.
package rootcmd

import (
	"fmt"
	"io"
	"os"
	"regexp"

	"github.com/landovsky/cc-sandbox/internal/backend"
	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/pins"
	"github.com/landovsky/cc-sandbox/internal/settings"
	"github.com/landovsky/cc-sandbox/profiles"
)

func (e *env) runCmd() *cli.Command {
	return &cli.Command{
		Name:    "run",
		Summary: "run one generation of a session in the fence, in the foreground (helper mode: 10001-owned outbox)",
		Recipes: `  sudo -n /usr/local/sbin/ccsbx-root run s0k1dry66qg          # what the pane of 'ccsbx start' runs
  sudo -n /usr/local/sbin/ccsbx-root run --tty s0k1dry66qg    # with a terminal
  Takes only the sid. The image, workdir, gitdir, caps and managed settings come from the
  daemon's record, re-checked here: the image must be a pinned digest (pins.toml, or a candidate
  digest for an e2e-* profile), the workdir strictly inside an allowed root with no symlink in
  its path and owned by the session's owner. Refusals exit 2 and say why.`,
		Setup: func(f *cli.Flags) { f.Bool("tty", false, "give the container a terminal (the caller's stdin is one)") },
		Run: func(f *cli.Flags, args []string, stdout, stderr io.Writer) int {
			sid, ok := oneSid("run", args, stderr)
			if !ok {
				return cli.ExitUsage
			}
			code, err := e.run(sid, f.Lookup("tty").Value.String() == "true", stdout, stderr)
			if err != nil {
				return fail(stderr, "run", err)
			}
			return code
		},
	}
}

// harness arguments root will pass to claude: a short allowlist, because the record is less
// trusted than root and an argument can switch the harness's own safety off.
var harnessFlagValue = map[string]*regexp.Regexp{
	"--model":  regexp.MustCompile(`^[A-Za-z0-9._:-]{1,64}$`),
	"--resume": regexp.MustCompile(`^[A-Za-z0-9-]{1,64}$`),
}
var harnessFlagOnly = map[string]bool{"--continue": true}

func harnessArgs(in []string) ([]string, error) {
	var out []string
	for i := 0; i < len(in); i++ {
		a := in[i]
		switch {
		case harnessFlagOnly[a]:
			out = append(out, a)
		case harnessFlagValue[a] != nil && i+1 < len(in) && harnessFlagValue[a].MatchString(in[i+1]):
			out = append(out, a, in[i+1])
			i++
		default:
			return nil, refuse("harness argument %q is not allowed (allowed: --continue, --model NAME, --resume ID)", a)
		}
	}
	return out, nil
}

func (e *env) run(sid string, tty bool, stdout, stderr io.Writer) (int, error) {
	w, err := e.prelude("run")
	if err != nil {
		return 0, err
	}
	rec, err := e.session(w, sid)
	if err != nil {
		return 0, err
	}
	if rec.Ended != nil {
		return 0, refuse("session %s has ended", sid)
	}
	p, err := profiles.Load(rec.Spec.Profile)
	if err != nil {
		return 0, refuse("%v", err)
	}
	if m, b := p.Allows(rec.Spec.Machine, rec.Spec.Billing); !m || !b {
		return 0, refuse("profile %s is not approved for machine %s with billing %s", p.Name, rec.Spec.Machine, rec.Spec.Billing)
	}
	// the image: a pinned digest, or a candidate digest of ours for an e2e-* profile. Never a tag,
	// never the dev channel (that is direct mode only).
	if err := trustedPins(e.cfg.PinsFile, e.cfg.TrustedUID); err != nil {
		return 0, refuse("pins: %v", err)
	}
	pl, err := pins.Load(e.cfg.PinsFile)
	if err != nil {
		return 0, refuse("pins: %v", err)
	}
	image := rec.Reply.Pin.Image
	if _, err := pins.Check(pl, nil, image, p, false); err != nil {
		return 0, refuse("%v", err)
	}
	wd, root, err := e.cfg.workdir(rec.Spec.Workdir.Path, rec.Uid)
	if err != nil {
		return 0, err
	}
	gd := ""
	if rec.GitDir != "" {
		if gd, err = e.cfg.gitdir(rec.GitDir, rec.Uid); err != nil {
			return 0, err
		}
	}
	extra, err := harnessArgs(rec.Spec.HarnessArgs)
	if err != nil {
		return 0, err
	}
	res, err := settings.Compile(p, settings.Params{BaseURL: e.cfg.BaseURL, FetchURL: e.cfg.FetchURL})
	if err != nil {
		return 0, err
	}
	docker, err := e.cfg.binary(e.cfg.Docker)
	if err != nil {
		return 0, err
	}
	if err := e.ensureRunDir(); err != nil {
		return 0, err
	}
	spec := backend.Spec{
		Sid: rec.Sid, Label: rec.Label, Gen: rec.Gen, Image: image, StateDir: e.cfg.RunDir,
		Workdir: wd, GitDir: gd, Uid: rec.Uid, WorktreeRoot: root,
		MemoryMB: p.MemoryMB, Pids: p.Pids, Cmd: append([]string{"claude"}, extra...), TTY: tty,
		NotEvid: false, Chown: e.d.Chown,
	}
	files := rec.FenceFiles(e.cfg.Machine, res.Settings, res.MCP)
	return e.d.NewBackend(docker).Run(spec, files, e.d.Stdin, stdout, stderr)
}

// ensureRunDir makes root's own tree: a real directory, root-owned, nobody else's to write.
func (e *env) ensureRunDir() error {
	if err := os.MkdirAll(e.cfg.RunDir, 0o755); err != nil {
		return err
	}
	return trustedFile(e.cfg.RunDir, e.cfg.TrustedUID, false)
}

func trustedPins(path string, uid int) error {
	if _, err := os.Lstat(path); os.IsNotExist(err) {
		return nil // no pins: nothing production can run, candidates only
	}
	return trustedFile(path, uid, true)
}

func (e *env) stopCmd() *cli.Command {
	return &cli.Command{
		Name:    "stop",
		Summary: "remove the container and network of one session's current generation",
		Recipes: `  sudo -n /usr/local/sbin/ccsbx-root stop s0k1dry66qg
  Takes only the sid. Removes exactly ccsbx-<sid>-g<gen> and its network, nothing else.`,
		Run: func(_ *cli.Flags, args []string, stdout, stderr io.Writer) int {
			sid, ok := oneSid("stop", args, stderr)
			if !ok {
				return cli.ExitUsage
			}
			w, err := e.prelude("stop")
			if err != nil {
				return fail(stderr, "stop", err)
			}
			rec, err := e.session(w, sid)
			if err != nil {
				return fail(stderr, "stop", err)
			}
			docker, err := e.cfg.binary(e.cfg.Docker)
			if err != nil {
				return fail(stderr, "stop", err)
			}
			if err := e.d.NewBackend(docker).Stop(backend.Spec{Sid: rec.Sid, Gen: rec.Gen}); err != nil {
				return fail(stderr, "stop", err)
			}
			fmt.Fprintf(stdout, "stopped %s g%d\n", rec.Sid, rec.Gen)
			return cli.ExitOK
		},
	}
}

func (e *env) gcCmd() *cli.Command {
	return &cli.Command{
		Name:    "gc",
		Summary: "remove containers and networks of generations whose run died",
		Recipes: `  sudo -n /usr/local/sbin/ccsbx-root gc --dry-run
  sudo -n /usr/local/sbin/ccsbx-root gc
  Only resources labelled ccsbx.sid whose sid has a daemon record, and only when the run that owns
  the generation is gone. Takes no sid.`,
		Setup: func(f *cli.Flags) { f.Bool("dry-run", false, "list orphans, remove nothing") },
		Run: func(f *cli.Flags, args []string, stdout, stderr io.Writer) int {
			if len(args) != 0 {
				fmt.Fprintln(stderr, "usage: ccsbx-root gc [--dry-run]   (no arguments)")
				return cli.ExitUsage
			}
			if _, err := e.prelude("gc"); err != nil {
				return fail(stderr, "gc", err)
			}
			docker, err := e.cfg.binary(e.cfg.Docker)
			if err != nil {
				return fail(stderr, "gc", err)
			}
			b := e.d.NewBackend(docker)
			orphans, err := b.Orphans(e.cfg.RunDir, e.cfg.StateDir+"/sessions")
			if err != nil {
				return fail(stderr, "gc", err)
			}
			for _, o := range orphans {
				fmt.Fprintf(stdout, "orphan %s %s (sid %s g%d)\n", o.Kind, o.Name, o.Sid, o.Gen)
			}
			if f.Lookup("dry-run").Value.String() == "true" || len(orphans) == 0 {
				fmt.Fprintf(stdout, "%d orphan(s)\n", len(orphans))
				return cli.ExitOK
			}
			if err := b.Reap(e.cfg.RunDir, orphans); err != nil {
				return fail(stderr, "gc", err)
			}
			fmt.Fprintf(stdout, "removed %d orphan(s)\n", len(orphans))
			return cli.ExitOK
		},
	}
}

// notWired is a verb whose arguments are checked and whose act is not built yet: it refuses with
// exit 2 so a caller never mistakes it for success.
func (e *env) notWired(verb, what string) *cli.Command {
	return &cli.Command{
		Name: verb, Summary: what + " (not wired yet: always refuses)",
		Recipes: fmt.Sprintf("  sudo -n /usr/local/sbin/ccsbx-root %s <sid>\n  Takes only a sid, checks the caller and the record like every verb, then refuses: the act itself waits for the signer.", verb),
		Run: func(_ *cli.Flags, args []string, _, stderr io.Writer) int {
			sid, ok := oneSid(verb, args, stderr)
			if !ok {
				return cli.ExitUsage
			}
			w, err := e.prelude(verb)
			if err == nil {
				_, err = e.session(w, sid)
			}
			if err != nil {
				return fail(stderr, verb, err)
			}
			return fail(stderr, verb, refuse("%s is not wired yet (it needs the signer, U0b)", verb))
		},
	}
}

// dockerBackend is the production Backend: backend.Runner with the validated docker binary.
type dockerBackend struct{ docker string }

func (d *dockerBackend) runner(sessions string) *backend.Runner {
	return &backend.Runner{Docker: d.docker, SessionsDir: sessions}
}

func (d *dockerBackend) Run(s backend.Spec, files map[string][]byte, in io.Reader, out, errw io.Writer) (int, error) {
	r := d.runner("")
	r.Stdin, r.Stdout, r.Stderr, r.Files = in, out, errw, files
	return r.Run(s)
}
func (d *dockerBackend) Stop(s backend.Spec) error { return d.runner("").Stop(s) }
func (d *dockerBackend) Orphans(runDir, sessions string) ([]backend.Orphan, error) {
	return d.runner(sessions).Orphans(runDir)
}
func (d *dockerBackend) Reap(runDir string, o []backend.Orphan) error {
	return d.runner("").Reap(runDir, o)
}
