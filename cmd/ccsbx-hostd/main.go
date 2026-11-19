// Command ccsbx-hostd is the session daemon's outbox watcher (U3a). It reads fence requests
// from every live outbox under the state directory, stamps them from the path and the session
// record, and answers them into in/replies/. Zero third-party modules.
package main

import (
	"fmt"
	"io"
	"log"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/hostd"
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
  ccsbx-hostd run --interval 250ms        # the daemon loop
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
	lg := log.New(stderr, "ccsbx-hostd: ", log.LstdFlags)
	h := hostd.New(hostd.Config{
		StateDir: get("state"), FenceUID: num("fence-uid"), MaxBytes: int64(num("max-bytes")),
		Rate: float64(num("rate")), Burst: num("burst"), MaxTotal: num("max-total"), Logf: lg.Printf,
	})
	if get("once") == "true" {
		st := h.Scan()
		fmt.Fprintf(stdout, "accepted=%d refused=%d shed=%d reasons=%v\n", st.Accepted, st.Refused, st.Shed, st.Reasons)
		return cli.ExitOK
	}
	stop := make(chan struct{})
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sig; close(stop) }()
	lg.Printf("watching %s every %s (direct mode: results are not evidence)", get("state"), iv)
	h.Run(iv, stop)
	return cli.ExitOK
}

func main() { os.Exit(app().Main(os.Args[1:], os.Stdout, os.Stderr)) }
