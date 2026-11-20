// Command ccsbx-hostd is the session daemon (U3a and what grew on it): it reads fence requests from
// every live outbox under the run directory (found through its own session records, never by
// listing), stamps them from the path and the record, answers them into in/replies/, journals
// events, delivers messages, serves the daemon API on a local socket, and starts sessions for the
// owner who asks over that socket. Zero third-party modules.
package main

import (
	"context"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"os/signal"
	"os/user"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/daemonapi"
	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/revoke"
	"github.com/landovsky/cc-sandbox/internal/start"
	"github.com/landovsky/cc-sandbox/internal/verbq"
	"github.com/landovsky/cc-sandbox/internal/whois"
	"github.com/landovsky/cc-sandbox/profiles"
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
				f.String("root", "auto", "privilege mode: auto (helper if /usr/local/sbin/ccsbx-root is installed and root-owned, else direct), direct, or helper (panes run sudo -n ccsbx-root run; no docker for the daemon)")
				f.String("run-dir", "", "where generations' outboxes are read: <run-dir>/s/<sid>/g<n>/{out,in,CLOSED} (default: the state dir). Helper mode: ccsbx-root's run dir, /var/lib/ccsbx-run, where out/ is 10001:ccsbx 02750 (hostd reads it through the group and never writes it) and s/ and s/<sid> are not listable by hostd, so every path is derived from its session records; every openat2/fstat check (owner uid = --fence-uid) still applies")
				f.String("pins", "/etc/ccsbx/pins.toml", "production pins file for session starts (digests that may run under any profile); the same file ccsbx-root reads")
				f.Int("min-uid", 1000, "session starts: a caller with a uid below this, root, or hostd's own uid is refused (ccsbx-root refuses them anyway; a start must not hold a label and a slot first)")
				f.String("worktree-root", home, "session starts: comma-separated absolute directories (the syntax and meaning of ccsbx-root.toml allowed_roots, e.g. /home/u/git,/home/u/wt): a workdir must be strictly inside one of them after symlinks are resolved; a bad entry stops every start; hidden dirs of the home are refused when a root is the home")
				f.String("pod-machines", "", "session starts: comma-separated machine names whose sessions run as pods (e.g. future-two): a spec for one is started through `sudo -n ccsbx-root pod-create` then `pod-load` (helper mode only); hostd holds no kubeconfig and never sees the pod's token or key. Empty: no pod placement")
				f.String("signer-socket", "", "docker sessions: the signer's socket (hostd's own); with --revoke-door, the token of every generation that ends is revoked at the doors (the signer signs, hostd posts). Empty: no revoke (log only)")
				f.String("revoke-door", "", "docker sessions: comma-separated base URLs of the doors' owner listeners that take the signed revoke at the end of a generation")
				f.Int("pod-max-live", 4, "pod sessions: machine_full above this many live pod sessions; each Guaranteed pod holds up to 32 GiB of ephemeral storage (about 264 GiB at 8): set it to what future-two's allocatable ephemeral storage divided by that allows (a Gate M check). 0: only --max-live applies")
				f.Duration("pod-ttl", 8*time.Hour, "pod sessions: a pod session older than this is stopped by the sweep (the pod deleted by UID); keep it at the root config's k8s_token_hours")
				f.Duration("pod-sweep", time.Minute, "pod sessions: how often the sweep looks for expired pods and pods the owner already stopped (0 turns it off)")
				f.Duration("pod-load-wait", 0, "pod sessions: how long hostd waits for `ccsbx-root pod-load` (0: its worst case, 600 s ready deadline plus load and API time, plus 30 s)")
				f.String("cc-version", "", "Claude Code version recorded in a started session's pin")
				f.Int("max-live", 8, "session starts: machine_full above this many live sessions")
				f.String("known-machines", "", "comma-separated machines of this deployment (hub machine ids): a session start whose msg_scope puts its manager or a peer on any other machine is refused (bad_spec) before anything is published. Empty: only the shape is checked")
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
				f.String("tailnet-listen", "", "serve the daemon API to the tailnet on this ONE tailnet address, e.g. 100.x.y.z:7480 (never a wildcard or a LAN address). Callers are classed by Tailscale WhoIs, not by anything they send: owner = --owner-login on a device in --owner-devices; peer = a device in --peer-devices or a node with a tag in --peer-tags; everyone else, and every failed WhoIs, gets 403. Off by default: nothing is reachable from the tailnet until the ACL and this flag exist")
				f.String("owner-login", "", "the owner's Tailscale login name (e.g. you@example.com) for --tailnet-listen")
				f.String("owner-devices", "", "comma-separated FULL MagicDNS names (host.<tailnet>.ts.net; short names are refused) the owner's login counts as owner on. Do not list THIS machine: a process on it is never served as owner over the tailnet")
				f.String("peer-devices", "", "comma-separated FULL MagicDNS names of devices that count as peer daemons when they carry the owner's login (a name alone proves little; tag them and use --peer-tags instead)")
				f.String("peer-tags", "", "comma-separated Tailscale tags only hostd's devices carry whose nodes count as peer daemons. Empty by default: peers are listed devices (--peer-devices) until F4, which brings a tag:ccsbx-hostd egress proxy for the in-cluster hostd. tag:ccsbx is refused: it is the cluster operator proxies' tag, which a fenced pod can reach")
				f.String("whois", "auto", "how WhoIs is asked: auto (tailscaled's socket if it exists, else the tailscale CLI), localapi, or cli")
				f.String("whois-socket", "/var/run/tailscale/tailscaled.sock", "tailscaled's LocalAPI unix socket (--whois localapi|auto); must be a unix socket owned by root or this daemon's user")
				f.String("tailscale-bin", "", "ABSOLUTE path of the tailscale CLI used for --whois cli (and for this node's tailnet addresses when there is no LocalAPI socket); default: the macOS app's CLI on darwin, else `tailscale` found on PATH ONCE at startup. Refused when it, or its directory, is writable by group or other: whoever can write it decides every caller's identity")
				f.String("tailnet-host", "", "comma-separated MagicDNS names of THIS node (host.<tailnet>.ts.net) that requests may carry in Host, besides the --tailnet-listen address itself: any other Host is refused (DNS rebinding)")
				f.Int("tailnet-max-conns", 64, "most simultaneously open connections on the tailnet listener (idle ones count)")
				f.Int("lease-ttl", 90, "cockpit lease TTL in seconds (one holder per realm)")
				f.Int("queue-max", 10000, "pending cockpit requests per realm before refused: queue_full")
				f.String("hub-url", "", "the session-bus hub's HTTP address, e.g. http://127.0.0.1:4011: hostd tracks its fenced sessions there and receives deliveries on spoke:<machine>/ccsbx (B1). Empty: no hub (logged)")
				f.String("hub-token-file", "", "file (mode 0600/0640, owned by the daemon's user or group) holding this machine's hub credential for hostd, the spoke:<machine>/ccsbx principal (sbp1.…; the owner's spoke:<machine> token is refused for hostd's work); hostd never holds the hub's admin secret and never touches the owner's bus sockets. Re-read after a 401. Mint it once, as the hub admin, on the hub host: curl -X POST -H \"authorization: Bearer $ADMIN\" -d '{\"role\":\"spoke\",\"machine\":\"<this machine>\",\"name\":\"ccsbx\",\"trees\":[\"cc-sandbox\"]}' http://127.0.0.1:4011/v1/principals (without trees the hub drops every node and cross-machine messages cannot be scoped)")
				f.Int("hub-ttl", 60, "lease, in seconds, of the sessions tracked on the hub (the hub allows 1..300); renewed every third of it")
				f.String("ccmsg-bin", "", "ccmsg (session-bus) to reach an unfenced manager on this machine, e.g. ~/.dotfiles/session-bus/bin/ccmsg; empty: only ccsbx sessions are reachable")
				f.Bool("hear", false, "C23 hear: tell a worker's manager by message when the worker needs a human decision (permission prompt, question dialog)")
				f.String("ccsbx-bin", "", "the ccsbx binary a replaced session's pane runs (default: ccsbx next to this binary)")
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
	dur := func(n string) time.Duration { d, _ := time.ParseDuration(get(n)); return d }
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
	// The mode and its run-dir requirement are checked before anything else runs, --once included.
	mode, err := start.ResolveRoot(get("root"))
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx-hostd:", err)
		return cli.ExitRefused
	}
	if mode == "helper" && get("run-dir") == "" {
		fmt.Fprintln(stderr, "ccsbx-hostd: helper mode needs --run-dir (ccsbx-root's run dir, /var/lib/ccsbx-run): without it hostd would look for outboxes in the state dir and see none")
		return cli.ExitRefused
	}
	if ttl := num("hub-ttl"); ttl < 1 || ttl > 300 {
		fmt.Fprintf(stderr, "ccsbx-hostd: --hub-ttl %d is outside the hub's 1..300 seconds\n", ttl)
		return cli.ExitUsage
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
	for _, u := range strings.Split(get("revoke-door"), ",") {
		if u = strings.TrimSpace(u); u != "" {
			if err := revoke.CheckURL(u); err != nil {
				fmt.Fprintln(stderr, "ccsbx-hostd:", err)
				return cli.ExitFail
			}
		}
	}
	h := hostd.New(hostd.Config{Journal: j, OnEvent: onEvent, OnTick: func() { rt.Pump(pumpReply(runDir)) },
		StateDir: get("state"), RunDir: runDir, FenceUID: num("fence-uid"), MaxBytes: int64(num("max-bytes")),
		OnGenerationEnd: generationEndRevoker(get("signer-socket"), get("revoke-door"), get("state"), lg.Printf),
		Handler:         m.handle, Rate: float64(num("rate")), Burst: num("burst"), EventRate: float64(num("event-rate")), EventBurst: num("event-burst"), MaxTotal: num("max-total"), Logf: lg.Printf,
	})
	if get("once") == "true" {
		st := h.Scan()
		fmt.Fprintf(stdout, "accepted=%d refused=%d shed=%d reasons=%v\n", st.Accepted, st.Refused, st.Shed, st.Reasons)
		return cli.ExitOK
	}
	hubCancel := func() {}
	if u := get("hub-url"); u != "" {
		hubCancel = startHubSpoke(get, num, rt, j, lg, u)
	} else {
		lg.Printf("--hub-url is not set: hostd is not on the session bus (no cross-machine messages, no presence)")
	}
	defer hubCancel()
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
	sd := &start.Daemon{StateDir: abs(get("state")), RunDir: runDir, ProfileLoader: profiles.Load, Machine: get("machine"), Root: mode, Exe: exe, Backend: "docker", Runtime: runtimeFor(mode),
		Launcher: &start.TmuxLauncher{Exe: exe, Socket: get("tmux-socket"), NoSpawn: true, Helper: mode == "helper", Env: paneEnv(abs(get("state")), get("machine"), get("image"), mode)}}
	if pm := get("pod-machines"); pm != "" {
		if mode != "helper" {
			fmt.Fprintln(os.Stderr, "ccsbx-hostd: --pod-machines needs helper mode (--root helper): the pod verbs are ccsbx-root's")
			return cli.ExitFail
		}
		sd.Placements = map[string]string{}
		for _, m := range strings.Split(pm, ",") {
			if m = strings.TrimSpace(m); m != "" && m != get("machine") {
				sd.Placements[m] = "k8s"
			}
		}
		sd.PodRoot = start.SudoPodRoot{LoadWait: dur("pod-load-wait")}
		sd.PodVerbsRevoke = true
		sd.PodMaxLive = num("pod-max-live")
		// a start may be in flight as long as the create (90 s) and the load can take, plus a margin: after
		// that a resume is no longer refused and the sweep stops what is left
		lw := dur("pod-load-wait")
		if lw <= 0 {
			lw = start.DefaultLoadWait
		}
		sd.StartLimit = lw + 90*time.Second + 2*time.Minute
		sd.Logf = lg.Printf
		if every := dur("pod-sweep"); every > 0 {
			ttl := dur("pod-ttl")
			go func() {
				for range time.Tick(every) {
					sd.SweepPods(ttl)
				}
			}()
		}
	}
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
	srv.Start = startHandler(sd, startPolicy{
		Pins: get("pins"), WorktreeRoots: get("worktree-root"), Image: get("image"),
		CCVersion: get("cc-version"), TmuxSocket: get("tmux-socket"), RunDir: runDir,
		MaxLive: num("max-live"), MaxPerHour: num("max-per-hour"), MinUID: num("min-uid"), LookupID: user.LookupId,
		KnownMachines: splitList(get("known-machines")),
	})
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
		hs := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 32 << 10, ConnContext: daemonapi.PeerContext}
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
	if a := get("tailnet-listen"); a != "" {
		cc := daemonapi.ClassConfig{OwnerLogin: get("owner-login"), OwnerDevices: splitList(get("owner-devices")), PeerDevices: splitList(get("peer-devices")), PeerTags: splitList(get("peer-tags"))}
		if err := cc.Validate(); err != nil {
			fmt.Fprintf(stderr, "ccsbx-hostd: %v\n", err)
			return cli.ExitUsage
		}
		ap, err := netip.ParseAddrPort(a)
		if err != nil {
			fmt.Fprintf(stderr, "ccsbx-hostd: --tailnet-listen %q: want <tailnet ip>:<port>: %v\n", a, err)
			return cli.ExitUsage
		}
		cc.Hosts = append([]string{ap.Addr().String()}, splitList(get("tailnet-host"))...)
		for _, d := range cc.OwnerDevices {
			if h, _, _ := strings.Cut(strings.ToLower(hostname()), "."); h != "" && strings.HasPrefix(strings.ToLower(d), h+".") {
				lg.Printf("WARNING: --owner-devices lists this machine (%s): the owner's own processes here are never served as owner over the tailnet (the kernel makes this node the source), so the entry does nothing; and any process on a LISTED device, whichever user runs it, acts as the owner there (ambient authority, no secret)", d)
			}
		}
		r, self, rc := tailnetWho(get, stderr)
		if rc != 0 {
			return rc
		}
		l, err := daemonapi.ListenTailnet(a, self, num("tailnet-max-conns"))
		if !serve(l, err, "tailnet:"+a, srv.TailnetHandler(r, cc)) {
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
	lg.Printf("watching outboxes under %s (state %s) every %s (direct mode: results are not evidence)", runDir, get("state"), iv)
	h.Run(iv, stop)
	for _, hs := range servers {
		hs.Close()
	}
	return cli.ExitOK
}

// readHubToken reads the hub spoke credential: the installer (root) writes it, hostd reads it through
// the group. It is opened without following a symlink and without blocking on a FIFO, judged on the
// opened descriptor (a regular file, owned by root or by hostd's own uid, not writable by its group
// nor reachable by others: 0640 or 0600), read through that descriptor and at most 4 KiB, and the
// token must look like a principal token (sbp1.…).
func readHubToken(path string) (string, error) { return readHubTokenAs(path, 0, os.Getuid()) }

func readHubTokenAs(path string, okUIDs ...int) (string, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NOFOLLOW|syscall.O_NONBLOCK|syscall.O_CLOEXEC, 0)
	if err != nil {
		return "", &os.PathError{Op: "open", Path: path, Err: err}
	}
	f := os.NewFile(uintptr(fd), path)
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !st.Mode().IsRegular() {
		return "", fmt.Errorf("%s is not a regular file", path)
	}
	if sys, ok := st.Sys().(*syscall.Stat_t); !ok || !containsInt(okUIDs, int(sys.Uid)) {
		return "", fmt.Errorf("%s is not owned by root or by the daemon's user", path)
	}
	if st.Mode().Perm()&0o037 != 0 {
		return "", fmt.Errorf("%s is writable by its group or reachable by others (mode %o: want 0640 or 0600)", path, st.Mode().Perm())
	}
	b, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return "", err
	}
	if len(b) > 4096 {
		return "", fmt.Errorf("%s is larger than 4 KiB: not a token file", path)
	}
	t := strings.TrimSpace(string(b))
	if !strings.HasPrefix(t, "sbp1.") || len(t) < 16 {
		return "", fmt.Errorf("%s does not hold a hub principal token (sbp1.…)", path)
	}
	return t, nil
}

func containsInt(l []int, v int) bool {
	for _, x := range l {
		if x == v {
			return true
		}
	}
	return false
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

// paneEnv is what a direct-mode pane's `ccsbx run` is told besides the sid and generation. The profiles
// are the ones compiled into the binary ("-"), never a directory relative to hostd's working directory:
// hostd's cwd is not a place profiles come from.
func paneEnv(state, machine, image, mode string) []string {
	return []string{"--state", state, "--profiles", "-", "--machine", machine, "--image", image, "--root", mode}
}

// splitList splits a comma-separated flag into its non-empty, trimmed entries.
// tailnetWho builds the WhoIs resolver for the tailnet listener and reads this node's own tailnet
// addresses (the listen address must be one of them). Anything unusable is a startup error, not a
// listener that refuses everyone later: LocalAPI when its socket exists (a socket owned by root or this
// user), else the tailscale CLI at an absolute, checked path resolved ONCE here.
func tailnetWho(get func(string) string, stderr io.Writer) (whois.Resolver, []netip.Addr, int) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	w, sock := get("whois"), get("whois-socket")
	switch {
	case w == "localapi" || (w == "auto" && fileExists(sock)):
		if err := whois.CheckSocket(sock); err != nil {
			fmt.Fprintf(stderr, "ccsbx-hostd: --whois-socket: %v\n", err)
			return nil, nil, cli.ExitUsage
		}
		l := &whois.LocalAPI{Socket: sock}
		self, err := l.SelfIPs(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "ccsbx-hostd: this node's tailnet addresses from tailscaled: %v\n", err)
			return nil, nil, cli.ExitFail
		}
		return l, self, 0 // an in-memory lookup in tailscaled: no cache, so a re-tag is seen at once
	case w == "cli" || w == "auto":
		bin := get("tailscale-bin")
		if bin == "" {
			if runtime.GOOS == "darwin" {
				bin = "/Applications/Tailscale.app/Contents/MacOS/Tailscale"
			} else if p, err := exec.LookPath("tailscale"); err == nil {
				bin = p
			}
		}
		if err := whois.CheckBinary(bin); err != nil {
			fmt.Fprintf(stderr, "ccsbx-hostd: --tailscale-bin: %v\n", err)
			return nil, nil, cli.ExitUsage
		}
		c := whois.CLI{Bin: bin}
		self, err := c.SelfIPs(ctx)
		if err != nil {
			fmt.Fprintf(stderr, "ccsbx-hostd: this node's tailnet addresses from %s: %v\n", bin, err)
			return nil, nil, cli.ExitFail
		}
		return &whois.Cache{R: c}, self, 0
	}
	fmt.Fprintf(stderr, "ccsbx-hostd: --whois %q: want auto, localapi or cli\n", w)
	return nil, nil, cli.ExitUsage
}

func splitList(s string) []string {
	var out []string
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func fileExists(p string) bool { _, err := os.Stat(p); return err == nil }

// runDirOr is the run dir, or the state dir when none is given.
func runDirOr(run, state string) string {
	if run != "" {
		return run
	}
	return state
}

func hostname() string { h, _ := os.Hostname(); return h }

func main() { os.Exit(app().Main(os.Args[1:], os.Stdout, os.Stderr)) }

// runtimeFor is how the stopped proof asks whether a container runs: docker in direct mode, the
// read-only `ccsbx-root status` in helper mode (the caller has no docker there).
func runtimeFor(mode string) start.Runtime {
	if mode == "helper" {
		return start.HelperRuntime{}
	}
	return start.DockerRuntime{Docker: "docker"}
}
