// main.go is `ccsbx side`: flags and the two subcommands (serve, load). It exists so the sidecar's
// whole surface (the forwarder port, the control socket, what it refuses) is one --help away.
package side

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/msg"
)

// Usage is the `ccsbx side --help` text.
const Usage = `ccsbx side — the sidecar of a fenced pod (plan F3a: secrets in, forwarder out)

Usage:
  ccsbx side serve --control /path/ctl.sock --door NAME=URL [--door ...] [flags]
  ccsbx side load  --control /path/ctl.sock  < secrets.json
  ccsbx side --help

serve   runs as the sidecar container's main process (uid 10002). It opens the forwarder port on
        loopback and a control socket (mode 0600, in a directory that must be 0700 and ours), and answers
        503 on the forwarder until the secrets arrive. It ends when the token expires or on SIGTERM.
load    is what the launcher runs INSIDE the sidecar container through pods/exec, with the secrets on
        stdin as {"token":"<session JWT>","key":"<base64 32-byte seed>","relay_token":"sbp1...."} (the last is
        optional). They never appear in the pod
        spec, an argument or the environment. One load per process, only from the sidecar's own uid;
        the key must match the token's cnf, and the token must not be expired.

Flags: (each also CCSBX_SIDE_<NAME>, the name upper-cased with - as _; CCSBX_SIDE_DOORS is a space-separated list)
  --listen ADDR     forwarder host:port, a loopback IP only        (default 127.0.0.1:8080)
  --control PATH    control socket path, in a sidecar-only volume   (required)
  --door NAME=URL   a door this session may reach; repeatable
  --routes FILE     JSON allowlist replacing the built-in one (see ccsbx fwd --help)
  --ca-file FILE    extra root CA for https doors
  --max-body N      request body cap in bytes (default 33554432)
  --allow-uid N     the uid that may load secrets (default: our own)

Identity (set by the launcher in the pod spec; never read from a file the fence can write; needed by the
journal and the relay):
  --sid S --gen N   the session and generation these records belong to
  --label L         the session's label, as the daemon knows it
  --full-id ID      owner/machine/sid/gN
  --machine M       this pod's machine as the session bus hub knows it
  --scope JSON      who this session may message: {"manager":{"node":"..","machine":".."},"peers":[".."],
                    "peer_machines":{"..":".."}}  (every node must be placed on a machine, or serve refuses)

Session journal (plan F3; the pod mounts a sidecar-only volume at /run/ccsbx/journal, whose root is
root-owned 0777, so the journal directory is one level below it and made here as 0700):
  --journal-dir D   records forwarder lifecycle (fwd.loaded/expired/denied) and every relay decision;
                    empty = no journal. Needs --sid and --gen. Use /run/ccsbx/journal/s.

Outbox relay (the fence's request files, read with the daemon's file checks):
  --out D           the fence's outbox (read-only mount); empty = no relay
  --in D            where replies go (in/replies is made here)       (required with --out)
  --state D         the relay's ledger: a directory made 0700 under the sidecar-only volume
                    (required with --out; use /run/ccsbx/side/state)
  --fence-uid N     the uid every request file must have              (default 10001)
  --relay-every DUR scan interval                                     (default 1s)

Where state may live: --control, --state, --journal-dir, --routes and --ca-file must not be under a mount the
fence can write (default: /work /run/ccsbx/out /home/agent; --fence-writable PATH adds more, repeatable,
CCSBX_SIDE_FENCE_WRITABLE is a space-separated list). The directories must be ours and 0700; the files
regular, not group/other-writable, owned by us or root. serve refuses to start otherwise.

Git (plan F3c; git.commit and git.push from the fence's outbox, the repo cloned into the worktree once):
  --git-repo OWNER/NAME   enables it (needs --out, --sid, --gen, --git-url). The fence never names the repo,
                          the branch or a remote: a git.commit carries a message, a git.push nothing.
  --git-base REF          a branch name or a full commit id to start from            (default main)
  --git-branch B          the session branch, the only ref ever pushed; never main/master/HEAD (required)
  --git-url URL           the forwarder's git base, http on loopback, e.g. http://127.0.0.1:8080/egress/git
                          (the door's paths are /egress/git/<owner>/<repo>.git/{info/refs,git-upload-pack,git-receive-pack})
  --git-dir D             the sidecar's gitdir, made 0700               (default /run/ccsbx/side/git)
  --work D                the worktree emptyDir the fence writes        (default /work)
  --snapshot-dir D        staging of the clone and the copy a commit is made from, made 0700
                          (default /run/ccsbx/side/snapshot/s)
  --git-max-bytes N       cap on a snapshot, counting allocated blocks (default 1 GiB; set it a little under the
                          snapshot volume's size so a full tree cannot evict the pod)
  Protected: only main, master, HEAD and trunk are refused as the session branch here; develop, release/* and the
  like are the door's call (U16). A --git-base of exactly 40 or 64 hex digits is a commit id, never a branch.
  After the load the sidecar clones into the EMPTY worktree and writes in/worktree-ready (or
  in/worktree-failed); the fence must not write /work before the first of those exists.

Bus relay (msg.send to a session on another machine, through the hub, signed with the generation key):
  --hub-url URL     the hub as the pod reaches it (the operator egress Service to hp:4011); needs --out,
                    --machine and --scope. The relay token arrives in the one load as "relay_token".

Recipes:
  git on:             … --git-repo owner/name --git-base main --git-branch ccsbx/s1 --git-url http://127.0.0.1:8080/egress/git
  bus relay on:       ccsbx side serve --control … --door model=URL --sid sX --gen 1 --label L --machine hp --scope '{…}' \
                        --journal-dir /run/ccsbx/journal/s --out /run/ccsbx/out --in /run/ccsbx/in --state /run/ccsbx/side/state --hub-url http://hub:4011
  from the launcher:  ccsbx token mint … --fwd-secrets | kubectl exec -i -c side POD -- ccsbx side load --control /run/ccsbx/side/ctl/ctl.sock
  what the fence sees: the forwarder port only; try  curl -s localhost:8080/model/health  (503 until loaded)
Not built yet (F3c): git commit/push.
`

func envOr(name, def string) string {
	if v := os.Getenv("CCSBX_SIDE_" + name); v != "" {
		return v
	}
	return def
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

// Main runs `ccsbx side`; it returns the exit code.
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			io.WriteString(stdout, Usage)
			return 0
		}
	}
	if len(args) == 0 || (args[0] != "serve" && args[0] != "load") {
		fmt.Fprint(stderr, Usage)
		return 64
	}
	fs := flag.NewFlagSet("side "+args[0], flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", envOr("LISTEN", "127.0.0.1:8080"), "")
	control := fs.String("control", envOr("CONTROL", ""), "")
	routesFile := fs.String("routes", envOr("ROUTES", ""), "")
	caFile := fs.String("ca-file", envOr("CA_FILE", ""), "")
	maxBody := fs.Int64("max-body", envInt("MAX_BODY", 0), "")
	allowUID := fs.Int("allow-uid", int(envInt("ALLOW_UID", 0)), "")
	var o ServeOptions
	fs.StringVar(&o.Sid, "sid", envOr("SID", ""), "")
	fs.IntVar(&o.Gen, "gen", int(envInt("GEN", 0)), "")
	fs.StringVar(&o.Label, "label", envOr("LABEL", ""), "")
	fs.StringVar(&o.FullID, "full-id", envOr("FULL_ID", ""), "")
	fs.StringVar(&o.Machine, "machine", envOr("MACHINE", ""), "")
	fs.StringVar(&o.Scope, "scope", envOr("SCOPE", ""), "")
	fs.StringVar(&o.JournalDir, "journal-dir", envOr("JOURNAL_DIR", ""), "")
	fs.StringVar(&o.Out, "out", envOr("OUT", ""), "")
	fs.StringVar(&o.In, "in", envOr("IN", ""), "")
	fs.StringVar(&o.State, "state", envOr("STATE", ""), "")
	fs.IntVar(&o.FenceUID, "fence-uid", int(envInt("FENCE_UID", 10001)), "")
	fs.DurationVar(&o.RelayEvery, "relay-every", envDur("RELAY_EVERY", time.Second), "")
	fs.StringVar(&o.HubURL, "hub-url", envOr("HUB_URL", ""), "")
	fs.StringVar(&o.GitRepo, "git-repo", envOr("GIT_REPO", ""), "")
	fs.StringVar(&o.GitBase, "git-base", envOr("GIT_BASE", "main"), "")
	fs.StringVar(&o.GitBranch, "git-branch", envOr("GIT_BRANCH", ""), "")
	fs.StringVar(&o.GitURL, "git-url", envOr("GIT_URL", ""), "")
	fs.StringVar(&o.GitDir, "git-dir", envOr("GIT_DIR", "/run/ccsbx/side/git/g"), "")
	fs.StringVar(&o.Work, "work", envOr("WORK", "/work"), "")
	fs.Int64Var(&o.GitMaxBytes, "git-max-bytes", envInt("GIT_MAX_BYTES", 0), "")
	fs.StringVar(&o.SnapshotDir, "snapshot-dir", envOr("SNAPSHOT_DIR", "/run/ccsbx/side/snapshot/s"), "")
	var fenceW multi
	fs.Var(&fenceW, "fence-writable", "")
	var doorFlags multi
	fs.Var(&doorFlags, "door", "")
	if err := fs.Parse(args[1:]); err != nil {
		return 2
	}
	if *control == "" {
		fmt.Fprintln(stderr, "side: --control is required")
		return 2
	}
	if args[0] == "load" {
		if err := Load(*control, stdin); err != nil {
			fmt.Fprintln(stderr, err)
			switch {
			case errors.Is(err, ErrNoSocket):
				return ExitNoSocket
			case errors.Is(err, ErrAlreadyLoaded):
				return ExitAlreadyLoaded
			}
			return 1
		}
		return 0
	}
	if len(doorFlags) == 0 {
		doorFlags = strings.Fields(os.Getenv("CCSBX_SIDE_DOORS"))
	}
	doors := map[string]*url.URL{}
	for _, d := range doorFlags {
		name, raw, ok := strings.Cut(d, "=")
		u, err := url.Parse(raw)
		if !ok || name == "" || err != nil || u.Host == "" {
			fmt.Fprintf(stderr, "side: bad --door %q (want NAME=URL)\n", d)
			return 2
		}
		doors[name] = u
	}
	if len(fenceW) == 0 {
		fenceW = strings.Fields(os.Getenv("CCSBX_SIDE_FENCE_WRITABLE"))
	}
	o.FenceWritable = append(append([]string(nil), DefaultFenceWritable...), fenceW...)
	o.RoutesFile = *routesFile
	o.Config = Config{Listen: *listen, Control: *control, Doors: doors, CAFile: *caFile,
		MaxBody: *maxBody, AllowUID: *allowUID, Logf: func(f string, a ...any) { fmt.Fprintf(stderr, f+"\n", a...) }}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	return runServe(ctx, o, stderr)
}

// ServeOptions is everything `side serve` takes besides the secrets.
type ServeOptions struct {
	GitMaxBytes                                                    int64
	GitRepo, GitBase, GitBranch, GitURL, GitDir, Work, SnapshotDir string
	// wrapJournal is a test seam: it lets a test slow the relay\'s last journal write down to prove serve waits for it.
	wrapJournal                        func(Appender) Appender
	RoutesFile                         string
	FenceWritable                      []string // mounts the fence can write; nothing of ours may live there
	Config                             Config
	Sid, Label, FullID, Machine, Scope string
	Gen                                int
	JournalDir, Out, In, State, HubURL string
	FenceUID                           int
	RelayEvery                         time.Duration
}

// runServe wires the forwarder, the session journal, the outbox relay and the bus sender, runs them until ctx
// ends or the token expires, and then lets the relay write its final journal summary before returning.
func runServe(ctx context.Context, o ServeOptions, stderr io.Writer) int {
	logf := o.Config.Logf
	fail := func(format string, a ...any) int { fmt.Fprintf(stderr, "side: "+format+"\n", a...); return 2 }
	uid := os.Getuid()
	if o.GitRepo != "" {
		o.GitDir, o.SnapshotDir = pick(o.GitDir, "/run/ccsbx/side/git/g"), pick(o.SnapshotDir, "/run/ccsbx/side/snapshot/s")
		o.Work, o.GitBase = pick(o.Work, "/work"), pick(o.GitBase, "main")
	}
	if o.GitRepo != "" && (!filepath.IsAbs(o.Work) || filepath.Clean(o.Work) != o.Work || resolve(o.Work) != o.Work) {
		return fail("--work %q must be an absolute, clean path with no symlink in it", o.Work)
	}
	gitPaths := map[string]string{}
	if o.GitRepo != "" {
		gitPaths = map[string]string{"--git-dir": o.GitDir, "--snapshot-dir": o.SnapshotDir}
	}
	for name, p := range mergePaths(map[string]string{"--control": o.Config.Control, "--state": o.State, "--journal-dir": o.JournalDir,
		"--routes": o.RoutesFile, "--ca-file": o.Config.CAFile}, gitPaths) {
		if r, bad := underAny(p, o.FenceWritable); p != "" && bad {
			return fail("%s %s is under %s, which the fence can write", name, p, r)
		}
	}
	// Each path must already be what it says: absolute, clean (no "." or ".."), and with no symlink in it, so the
	// check above and the files opened below are about the same place and a link cannot be swapped in.
	for name, p := range mergePaths(map[string]string{"--control": o.Config.Control, "--state": o.State, "--journal-dir": o.JournalDir,
		"--routes": o.RoutesFile, "--ca-file": o.Config.CAFile}, gitPaths) {
		if p != "" && (!filepath.IsAbs(p) || filepath.Clean(p) != p || resolve(p) != p) {
			return fail("%s %q must be an absolute, clean path with no symlink in it", name, p)
		}
	}
	for name, p := range mergePaths(map[string]string{"--state": o.State, "--journal-dir": o.JournalDir}, gitPaths) {
		if p != "" {
			if err := ownDir(p, uid); err != nil {
				return fail("%s: %v", name, err)
			}
		}
	}
	for name, p := range map[string]string{"--routes": o.RoutesFile, "--ca-file": o.Config.CAFile} {
		if p != "" {
			if err := trustedFile(p, uid); err != nil {
				return fail("%s: %v", name, err)
			}
		}
	}
	if o.RoutesFile != "" {
		b, err := os.ReadFile(o.RoutesFile)
		if err == nil {
			err = json.Unmarshal(b, &o.Config.Rules)
		}
		if err != nil {
			return fail("--routes: %v", err)
		}
	}
	if o.Out != "" && o.RelayEvery <= 0 {
		return fail("--relay-every must be positive")
	}
	if o.HubURL != "" {
		if u, err := url.Parse(o.HubURL); err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
			return fail("--hub-url must be an http(s) URL with a host and no credentials")
		}
	}
	var scope msg.Scope
	if o.HubURL != "" || o.Scope != "" {
		var err error
		if scope, err = parseScope(o.Scope); err != nil {
			return fail("%v", err)
		}
	}
	if o.HubURL != "" && o.Machine != "" && scope.Manager.Machine == o.Machine {
		return fail("--scope places the manager on this pod's own machine (%s); the bus reaches other machines only", o.Machine)
	}
	if o.HubURL != "" && (o.Out == "" || o.Machine == "" || o.Sid == "") {
		return fail("--hub-url needs --out, --machine and --sid")
	}
	if o.Out != "" && (o.In == "" || o.State == "" || o.Sid == "" || o.Gen < 1) {
		return fail("--out needs --in, --state, --sid and --gen")
	}
	var js *journal.Store
	if o.JournalDir != "" {
		if o.Sid == "" || o.Gen < 1 {
			return fail("--journal-dir needs --sid and --gen")
		}
		var err error
		if js, err = journal.Open(o.JournalDir, o.Machine, 0); err != nil {
			return fail("journal: %v", err)
		}
		defer js.Close()
		o.Config.Journal = js
		o.Config.Sid, o.Config.Gen = o.Sid, o.Gen
	}
	s, err := New(o.Config)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	jr := journalOrNil(js)
	if jr != nil && o.wrapJournal != nil {
		jr = o.wrapJournal(jr)
	}
	var gitFwd func(context.Context, hostd.Request) hostd.Reply
	var gitops *GitOps
	if o.GitRepo != "" {
		if o.Out == "" || o.Sid == "" || o.Gen < 1 || o.GitURL == "" || o.GitBranch == "" {
			return fail("--git-repo needs --out, --sid, --gen, --git-url and --git-branch")
		}
		var err error
		gitops, err = NewGitOps(GitConfig{Repo: o.GitRepo, Base: o.GitBase, Branch: o.GitBranch, URL: o.GitURL, GitDir: o.GitDir,
			Work: o.Work, Snapshot: o.SnapshotDir, Sid: o.Sid, Gen: o.Gen, Journal: jr, Gate: s.GitGate, MaxSnapshotBytes: o.GitMaxBytes, Logf: logf})
		if err != nil {
			return fail("%v", err)
		}
		gitFwd = gitops.Forward
	}
	var relay *Relay
	if o.Out != "" {
		fwdFn := func(context.Context, hostd.Request) hostd.Reply { return busReply("refused", "unsupported_in_pod") }
		if o.HubURL != "" {
			bus, err := NewBus(BusConfig{HubURL: o.HubURL, Machine: o.Machine, Sid: o.Sid, Label: o.Label, Scope: scope,
				Cred: s.Relay, Journal: jr, Logf: logf})
			if err != nil {
				return fail("%v", err)
			}
			fwdFn = bus.Forward
		}
		if gitFwd != nil {
			fwdFn = Route(busFwd(fwdFn, o.HubURL != ""), gitFwd)
		}
		rc := RelayConfig{Out: o.Out, In: o.In, State: o.State, Sid: o.Sid, Gen: o.Gen, Label: o.Label, FullID: o.FullID,
			FenceUID: o.FenceUID, Forward: fwdFn, Journal: jr, Logf: logf}
		if relay, err = NewRelay(rc); err != nil {
			return fail("%v", err)
		}
	}
	rctx, stopRelay := context.WithCancel(ctx)
	relayDone := make(chan struct{})
	if relay != nil {
		go func() { defer close(relayDone); relay.Run(rctx, o.RelayEvery) }()
	} else {
		close(relayDone)
	}
	if gitops != nil {
		go cloneWhenLoaded(rctx, s, gitops, o.In, logf)
	}
	serr := s.Serve(ctx)
	stopRelay()
	<-relayDone // Run writes the final shed summary before it returns
	if serr != nil {
		fmt.Fprintln(stderr, serr)
		return 1
	}
	return 0
}

// journalOrNil keeps a nil *journal.Store from becoming a non-nil Appender.
func journalOrNil(js *journal.Store) Appender {
	if js == nil {
		return nil
	}
	return js
}

var machineIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// parseScope reads --scope and refuses one that leaves a node unplaced: the sender reaches a node only on the
// machine the record names, so a node with no machine would reach nobody and is a launcher bug worth failing on.
func parseScope(raw string) (msg.Scope, error) {
	var sc msg.Scope
	if raw == "" {
		return sc, errors.New("--scope is required with the bus relay")
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sc); err != nil || dec.More() {
		return sc, errors.New("--scope is not the scope JSON")
	}
	place := func(node, machine string) error {
		if node != "" && !machineIDRe.MatchString(machine) {
			return fmt.Errorf("--scope: node %q is not placed on a valid machine", node)
		}
		return nil
	}
	if err := place(sc.Manager.Node, sc.Manager.Machine); err != nil {
		return sc, err
	}
	for _, p := range sc.Peers {
		if err := place(p, sc.PeerMachines[p]); err != nil {
			return sc, err
		}
	}
	return sc, nil
}

func envInt(name string, def int64) int64 {
	if v := os.Getenv("CCSBX_SIDE_" + name); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func envDur(name string, def time.Duration) time.Duration {
	if v := os.Getenv("CCSBX_SIDE_" + name); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func pick(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

func mergePaths(a, b map[string]string) map[string]string {
	out := make(map[string]string, len(a)+len(b))
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

// busFwd is the bus's Forward, or a refusal when no hub is configured (then msg.* is unavailable in this pod).
func busFwd(f func(context.Context, hostd.Request) hostd.Reply, have bool) func(context.Context, hostd.Request) hostd.Reply {
	if !have {
		return nil
	}
	return f
}

// cloneWhenLoaded waits for the secrets, clones once, and tells the fence's side through in/ whether the worktree is
// ready: the fence must not write /work before one of these files exists.
func cloneWhenLoaded(ctx context.Context, s *Server, g *GitOps, in string, logf func(string, ...any)) {
	select {
	case <-ctx.Done():
		return
	case <-s.loadCh:
	}
	name, body := "worktree-ready", "ok\n"
	if err := g.Clone(ctx); err != nil {
		logf("side git: clone failed: %v", err)
		g.fact("git.refused", map[string]any{"op": "clone", "code": "clone_failed"})
		name, body = "worktree-failed", cleanText(err.Error(), 200)+"\n"
	}
	if err := writePublic(filepath.Join(in, name), []byte(body)); err != nil {
		logf("side git: %s: %v", name, err)
	}
}
