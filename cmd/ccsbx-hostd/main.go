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
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/daemonapi"
	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/start"
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
  ccsbx-hostd run --listen 127.0.0.1:7481 # ...and on loopback, for curl:
    curl -s 'http://127.0.0.1:7481/v1/events?after=0'          # replay after a cursor
    curl -s 'http://127.0.0.1:7481/v1/events?after=41&epoch=E&wait=20'   # long-poll
    curl -N 'http://127.0.0.1:7481/v1/events/stream?after=41'  # SSE
    curl -s -d '{"seq":41,"epoch":"E","tree_sha":"<sha>"}' http://127.0.0.1:7481/v1/cursors/production
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
				f.String("listen", "", "also serve the API on this loopback address, e.g. 127.0.0.1:7481 (non-loopback is refused until M2a)")
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
	h := hostd.New(hostd.Config{Journal: j,
		StateDir: get("state"), FenceUID: num("fence-uid"), MaxBytes: int64(num("max-bytes")),
		Handler: m.handle, Rate: float64(num("rate")), Burst: num("burst"), MaxTotal: num("max-total"), Logf: lg.Printf,
	})
	if get("once") == "true" {
		st := h.Scan()
		fmt.Fprintf(stdout, "accepted=%d refused=%d shed=%d reasons=%v\n", st.Accepted, st.Refused, st.Shed, st.Reasons)
		return cli.ExitOK
	}
	var servers []*http.Server
	srv := &daemonapi.Server{J: j, MaxWait: 60 * time.Second, Info: map[string]any{"machine": get("machine"), "not_evidence": true}}
	sock := get("socket")
	if sock == "" {
		sock = filepath.Join(get("state"), "hostd.sock")
	}
	serve := func(l net.Listener, err error, what string) bool {
		if err != nil {
			fmt.Fprintf(stderr, "ccsbx-hostd: %s: %v\n", what, err)
			return false
		}
		hs := &http.Server{Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
		servers = append(servers, hs)
		go hs.Serve(l)
		lg.Printf("api on %s", what)
		return true
	}
	if sock != "-" {
		l, err := daemonapi.ListenUnix(sock)
		if !serve(l, err, "unix:"+sock) {
			return cli.ExitFail
		}
	}
	if a := get("listen"); a != "" {
		l, err := daemonapi.ListenLoopback(a)
		if !serve(l, err, a) {
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

func hostname() string { h, _ := os.Hostname(); return h }

func main() { os.Exit(app().Main(os.Args[1:], os.Stdout, os.Stderr)) }
