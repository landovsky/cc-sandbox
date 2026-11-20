// Command ccsbx-hostd is the session daemon's outbox watcher (U3a). It reads fence requests
// from every live outbox under the state directory, stamps them from the path and the session
// record, and answers them into in/replies/. Zero third-party modules.
package main

import (
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/daemonapi"
	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/start"
	"github.com/landovsky/cc-sandbox/internal/verbq"
)

var version = "dev"

func app() *cli.App {
	return &cli.App{
		Name:    "ccsbx-hostd",
		Summary: "watch fence outboxes and answer their requests (U3a)",
		Version: version,
		Cmds: []*cli.Command{{
			Name:    "run",
			Summary: "scan every live outbox until stopped (or once with --once)",
			Recipes: `  ccsbx-hostd run --once                  # one pass, print what was accepted/refused/shed
  ccsbx-hostd run --interval 250ms        # the daemon loop + the API on <state>/hostd.sock
  curl --unix-socket <state>/hostd.sock http://d/v1/events?after=0              # replay after a cursor
  curl --unix-socket <state>/hostd.sock 'http://d/v1/events?after=41&epoch=E&wait=20'   # long-poll
  curl --unix-socket <state>/hostd.sock -N 'http://d/v1/events/stream?after=41'  # SSE
  curl --unix-socket <state>/hostd.sock -d '{"seq":41,"epoch":"E","tree_sha":"<sha>"}' http://d/v1/cursors/production
  ccsbx-hostd run --listen 127.0.0.1:7481 --listen-token-file ~/.config/ccsbx/api-token
    curl -H "Authorization: Bearer $(cat ~/.config/ccsbx/api-token)" http://127.0.0.1:7481/v1/info
  The local unix socket is mode 0600 (owner class: also deliver and lease steal), or 0660 and
  chgrp'ed with --socket-group GROUP (fails closed if the group is unknown or the chgrp refused).
  --signer PATH: the signer's unix socket; hostd signs tokens only through it and never reads a key.
  Every flag also reads CCSBX_<FLAG> (--local-socket: CCSBX_LOCAL_SOCKET). Unknown flags are fatal. The loopback port is
  token-protected and read/cockpit only.
  Direct mode (dev): the outbox is writable by the host, so T:outbox-host-write-refused is red
  until gate M, and every result is "not evidence". Requests are never echoed: refusals carry
  a rule name (too_big, not_regular, hardlink, wrong_owner, unsafe_path, bad_json, unknown_type,
  rate_capped), never request bytes.`,
			Setup: func(f *cli.Flags) {
				home, _ := os.UserHomeDir()
				f.String("state", filepath.Join(home, ".local", "state", "ccsbx"), "daemon state directory (the same one ccsbx start uses)")
				f.Int("fence-uid", 10001, "owner uid every outbox file must have (the fence's uid on the host)")
				f.Int("max-bytes", 64<<10, "largest request file; bigger (even sparse) is refused unread")
				f.Int("rate", 10, "requests per second per outbox")
				f.Int("burst", 20, "token bucket size per outbox")
				f.Int("event-rate", 50, "fence hook events journaled per second per outbox; beyond the burst they are dropped and counted (events_shed)")
				f.Int("event-burst", 500, "event bucket size per outbox")
				f.Int("max-total", 20000, "requests per generation in its lifetime; beyond that they are shed")
				f.String("run-dir", "", "where generations' outboxes are read: <run-dir>/s/<sid>/g<n>/{out,in,CLOSED} (default: the state dir). Helper mode: ccsbx-root's run dir, /var/lib/ccsbx-run, where out/ is 10001:ccsbx 2770 and hostd reads it through the group; every openat2/fstat check (owner uid = --fence-uid) still applies")
				f.String("pins", "pins.toml", "production pins file for session starts (digests that may run under any profile)")
				f.String("worktree-root", home, "session starts: comma-separated absolute directories (the syntax and meaning of ccsbx-root.toml allowed_roots, e.g. /home/u/git,/home/u/wt): a workdir must be strictly inside one of them after symlinks are resolved; a bad entry stops every start; hidden dirs of the home are refused when a root is the home")
				f.String("cc-version", "", "Claude Code version recorded in a started session's pin")
				f.Int("max-live", 8, "session starts: machine_full above this many live sessions")
				f.Int("max-per-hour", 20, "session starts: rate_capped above this many starts per caller per hour")
				f.String("tmux-socket", "ccsbx", "tmux server the session panes live on: a bare name is tmux -L NAME, an absolute path is tmux -S PATH (a socket shared between uids; hostd makes it 0660)")
				f.String("machine", hostname(), "this machine's name, stamped on journal records")
				f.String("journal", "", "journal directory (default <state>/journal)")
				f.Int("retention-days", 14, "journal retention; values below 14 are refused")
				f.String("local-socket", "", "unix socket for local clients (ccsbx, cc-tree): the daemon API incl. owner operations; mode 0600, or 0660 with --socket-group (default <state>/hostd.sock; '-' disables)")
				f.String("socket", "", "old name of --local-socket (kept; --local-socket wins)")
				f.String("socket-group", "", "chgrp the local socket to this group and make it 0660 (the owner's user must be in it); an unknown group or a refused chgrp stops the daemon")
				f.String("signer", "", "the signer's unix socket: hostd connects there for every token signing and never reads a key file; checked at start (a missing socket is an error, not a hang)")
				f.String("listen", "", "also serve the journal/cockpit API on this loopback address, e.g. 127.0.0.1:7481; needs --listen-token-file; no deliver, no steal there")
				f.String("listen-token-file", "", "file (mode 0600) holding the bearer token the --listen address requires")
				f.Int("lease-ttl", 90, "cockpit lease TTL in seconds (one holder per realm)")
				f.Int("queue-max", 10000, "pending cockpit requests per realm before refused: queue_full")
				f.String("ccmsg-bin", "", "ccmsg (session-bus) to reach an unfenced manager on this machine, e.g. ~/.dotfiles/session-bus/bin/ccmsg; empty: only ccsbx sessions are reachable")
				f.Bool("hear", false, "C23 hear: tell a worker's manager by message when the worker needs a human decision (permission prompt, question dialog)")
				f.String("ccsbx-bin", "", "the ccsbx binary a replaced session's pane runs (default: ccsbx next to this binary)")
				f.String("profiles", "profiles", "directory of <name>.json profiles, passed to the pane of a replaced session")
				f.String("image", "ccsbx-fence:dev", "fence image, passed to the pane of a replaced session")
				f.String("interval", "250ms", "time between scans")
				f.Bool("once", false, "scan once and print the stats as a line, then exit")
			},
			Run: run,
		}},
	}
}

func run(f *cli.Flags, _ []string, stdout, stderr io.Writer) int {
	get := func(n string) string { return f.Lookup(n).Value.String() }
	num := func(n string) int { var v int; fmt.Sscanf(get(n), "%d", &v); return v }
	iv, err := time.ParseDuration(get("interval"))
	if err != nil || iv <= 0 {
		fmt.Fprintf(stderr, "ccsbx-hostd: --interval %q is not a positive duration\n", get("interval"))
		return cli.ExitUsage
	}
	if sg := get("signer"); sg != "" {
		if err := checkSigner(sg); err != nil {
			fmt.Fprintf(stderr, "ccsbx-hostd: --signer: %v\n", err)
			return cli.ExitFail
		}
	}
	d := &start.Daemon{StateDir: get("state")}
	tm := &msg.Tmux{Socket: get("tmux-socket")}
	rt := &msg.Router{Dir: msg.DaemonDir{D: d, Alive: tm.Alive}, Out: tm, StateDir: get("state")}
	m := &mux{}
	m.on("msg.", msgHandler(rt))
	q, err := verbq.Open(filepath.Join(get("state"), "queue"))
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx-hostd:", err)
		return cli.ExitFail
	}
	defer q.Close()
	q.TTL = time.Duration(num("lease-ttl")) * time.Second
	q.MaxPending = num("queue-max")
	q.Reply = replyWriter(runDirOr(get("run-dir"), get("state")), d)
	lg := log.New(stderr, "ccsbx-hostd: ", log.LstdFlags)
	jdir := get("journal")
	if jdir == "" {
		jdir = filepath.Join(get("state"), "journal")
	}
	j, err := journal.Open(jdir, get("machine"), num("retention-days"))
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx-hostd:", err)
		return cli.ExitFail
	}
	defer j.Close()
	m.on("ccf.", verbHandler(q, d, j, rt))
	rt.Idle = j
	if b := get("ccmsg-bin"); b != "" {
		rt.Hub = msg.CcmsgHub{Bin: b} // an unfenced manager on this machine, through the session bus
	} else {
		// Not an error: this is the state at Gate M (hostd's uid has no access to the owner's bus).
		lg.Printf("--ccmsg-bin is not set: a fenced worker's message to an unfenced manager is answered no_live_session (the hub is reached only through the spoke, not built yet); messages between ccsbx sessions are unaffected")
	}
	var onEvent func(journal.Record)
	if get("hear") == "true" {
		onEvent = hearFunc(rt)
	} // a message is typed only into a pane whose last hook event is Stop
	runDir := get("run-dir")
	if runDir == "" {
		runDir = get("state")
	}
	h := hostd.New(hostd.Config{Journal: j, OnEvent: onEvent, OnTick: func() { rt.Pump(pumpReply(runDir)) },
		StateDir: get("state"), RunDir: runDir, FenceUID: num("fence-uid"), MaxBytes: int64(num("max-bytes")),
		Handler: m.handle, Rate: float64(num("rate")), Burst: num("burst"), EventRate: float64(num("event-rate")), EventBurst: num("event-burst"), MaxTotal: num("max-total"), Logf: lg.Printf,
	})
	if get("once") == "true" {
		st := h.Scan()
		fmt.Fprintf(stdout, "accepted=%d refused=%d shed=%d reasons=%v\n", st.Accepted, st.Refused, st.Shed, st.Reasons)
		return cli.ExitOK
	}
	exe := get("ccsbx-bin")
	if exe == "" {
		if self, err := os.Executable(); err == nil {
			exe = filepath.Join(filepath.Dir(self), "ccsbx")
		}
	}
	abs := func(p string) string {
		a, err := filepath.Abs(p)
		if err != nil {
			return p
		}
		return a
	}
	sd := &start.Daemon{StateDir: abs(get("state")), RunDir: runDir, Machine: get("machine"), Root: "direct", Exe: exe, Backend: "docker",
		Launcher: &start.TmuxLauncher{Exe: exe, Socket: get("tmux-socket"), NoSpawn: true, Env: []string{"--state", abs(get("state")),
			"--profiles", abs(get("profiles")), "--machine", get("machine"), "--image", get("image"), "--root", "direct"}}}
	var servers []*http.Server
	srv := &daemonapi.Server{J: j, Q: q, Msg: rt, Dir: rt.Dir, MaxWait: 60 * time.Second, Info: map[string]any{"machine": get("machine"), "not_evidence": true}}
	srv.Replace = func(sid, brief string) (int, error) {
		rec, err := sd.Replace(sid, brief)
		if err != nil {
			return 0, err
		}
		return rec.Gen, nil
	}
	// Session start for helper mode: the owner's `ccsbx start` asks here; hostd writes the record in
	// its state dir and launches the pane. The caller's uid is the socket peer's, not the spec's.
	srv.Start = func(uid int, specJSON []byte) (*start.StartReply, error) {
		spec, err := start.ParseSpec(specJSON)
		if err != nil {
			return nil, err
		}
		d := *sd // a copy per call: Uid differs, everything else is hostd's configuration
		d.Uid, d.RunDir = uid, runDir
		d.ProfilesDir, d.PinsFile, d.WorktreeRoot = abs(get("profiles")), abs(get("pins")), start.AbsRoots(get("worktree-root"))
		d.Image, d.CCVersion, d.Socket = get("image"), get("cc-version"), get("tmux-socket")
		d.MaxLive, d.MaxPerHour = num("max-live"), num("max-per-hour")
		return d.Start(spec)
	}
	sock := get("local-socket")
	if sock == "" {
		sock = get("socket")
	}
	if sock == "" {
		sock = filepath.Join(get("state"), "hostd.sock")
	}
	serve := func(l net.Listener, err error, what string, h http.Handler) bool {
		if err != nil {
			fmt.Fprintf(stderr, "ccsbx-hostd: %s: %v\n", what, err)
			return false
		}
		hs := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, ConnContext: daemonapi.PeerContext}
		servers = append(servers, hs)
		go hs.Serve(l)
		lg.Printf("api on %s", what)
		return true
	}
	if sock != "-" {
		l, err := daemonapi.ListenUnixGroup(sock, get("socket-group"))
		if !serve(l, err, "unix:"+sock, srv.Handler()) {
			return cli.ExitFail
		}
	}
	if a := get("listen"); a != "" {
		tok, err := readToken(get("listen-token-file"))
		if err != nil {
			fmt.Fprintf(stderr, "ccsbx-hostd: --listen needs --listen-token-file: %v\n", err)
			return cli.ExitUsage
		}
		srv.Token = tok
		l, err := daemonapi.ListenLoopback(a)
		if !serve(l, err, a, srv.TCPHandler()) {
			return cli.ExitFail
		}
	}
	go func() {
		for range time.Tick(time.Hour) {
			if n, err := j.Prune(); err != nil {
				lg.Printf("journal prune: %v", err)
			} else if n > 0 {
				lg.Printf("journal prune: %d old segments removed", n)
			}
		}
	}()
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; close(stop) }()
	lg.Printf("watching %s every %s (direct mode: results are not evidence)", get("state"), iv)
	h.Run(iv, stop)
	for _, hs := range servers {
		hs.Close()
	}
	return cli.ExitOK
}

// checkSigner dials the signer's socket with a short timeout, so a signer that is not up is a
// clear error at start rather than a hang or a later failure at the first signing.
func checkSigner(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("signer socket %s: %v", path, err)
	}
	if st.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is not a unix socket", path)
	}
	c, err := net.DialTimeout("unix", path, 2*time.Second)
	if err != nil {
		return fmt.Errorf("cannot connect to the signer at %s: %v", path, err)
	}
	return c.Close()
}

// readToken reads the bearer token for the TCP listener. The file must be private (0600 or
// stricter) and the token at least 16 characters, so a shared or empty one is refused.
func readToken(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("no file given")
	}
	st, err := os.Stat(path)
	if err != nil {
		return "", err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("%s is readable by others (chmod 600)", path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	t := strings.TrimSpace(string(b))
	if len(t) < 16 {
		return "", fmt.Errorf("%s: the token must be at least 16 characters", path)
	}
	return t, nil
}

// runDirOr is the run dir, or the state dir when none is given.
func runDirOr(run, state string) string {
	if run != "" {
		return run
	}
	return state
}

func hostname() string { h, _ := os.Hostname(); return h }

func main() { os.Exit(app().Main(os.Args[1:], os.Stdout, os.Stderr)) }
