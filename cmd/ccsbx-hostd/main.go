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
  The unix socket is mode 0600 (owner class: also deliver and lease steal). The loopback port is
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
				f.Int("max-total", 20000, "requests per generation in its lifetime; beyond that they are shed")
				f.String("tmux-socket", "ccsbx", "tmux -L socket the session panes live on (message delivery by send-keys)")
				f.String("machine", hostname(), "this machine's name, stamped on journal records")
				f.String("journal", "", "journal directory (default <state>/journal)")
				f.Int("retention-days", 14, "journal retention; values below 14 are refused")
				f.String("socket", "", "unix socket for the daemon API, mode 0600 (default <state>/hostd.sock; '-' disables)")
				f.String("listen", "", "also serve the journal/cockpit API on this loopback address, e.g. 127.0.0.1:7481; needs --listen-token-file; no deliver, no steal there")
				f.String("listen-token-file", "", "file (mode 0600) holding the bearer token the --listen address requires")
				f.Int("lease-ttl", 90, "cockpit lease TTL in seconds (one holder per realm)")
				f.Int("queue-max", 10000, "pending cockpit requests per realm before refused: queue_full")
				f.String("ccmsg-bin", "", "ccmsg (session-bus) to reach an unfenced manager on this machine, e.g. ~/.dotfiles/session-bus/bin/ccmsg; empty: only ccsbx sessions are reachable")
				f.Bool("hear", false, "C23 hear: tell a worker's manager by message when the worker needs a human decision (permission prompt, question dialog)")
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
	q.Reply = replyWriter(get("state"), d)
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
	}
	var onEvent func(journal.Record)
	if get("hear") == "true" {
		onEvent = hearFunc(rt)
	} // a message is typed only into a pane whose last hook event is Stop
	h := hostd.New(hostd.Config{Journal: j, OnEvent: onEvent, OnTick: func() { rt.Pump(pumpReply(get("state"))) },
		StateDir: get("state"), FenceUID: num("fence-uid"), MaxBytes: int64(num("max-bytes")),
		Handler: m.handle, Rate: float64(num("rate")), Burst: num("burst"), MaxTotal: num("max-total"), Logf: lg.Printf,
	})
	if get("once") == "true" {
		st := h.Scan()
		fmt.Fprintf(stdout, "accepted=%d refused=%d shed=%d reasons=%v\n", st.Accepted, st.Refused, st.Shed, st.Reasons)
		return cli.ExitOK
	}
	var servers []*http.Server
	srv := &daemonapi.Server{J: j, Q: q, Msg: rt, Dir: rt.Dir, MaxWait: 60 * time.Second, Info: map[string]any{"machine": get("machine"), "not_evidence": true}}
	sock := get("socket")
	if sock == "" {
		sock = filepath.Join(get("state"), "hostd.sock")
	}
	serve := func(l net.Listener, err error, what string, h http.Handler) bool {
		if err != nil {
			fmt.Fprintf(stderr, "ccsbx-hostd: %s: %v\n", what, err)
			return false
		}
		hs := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
		servers = append(servers, hs)
		go hs.Serve(l)
		lg.Printf("api on %s", what)
		return true
	}
	if sock != "-" {
		l, err := daemonapi.ListenUnix(sock)
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

func hostname() string { h, _ := os.Hostname(); return h }

func main() { os.Exit(app().Main(os.Args[1:], os.Stdout, os.Stderr)) }
