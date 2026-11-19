package fwd

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/token"
)

// Usage is the `ccsbx fwd --help` text (recipes live here, not only in docs).
const Usage = `ccsbx fwd — the per-session forwarder (one per generation, outside the fence)

Usage:
  ccsbx fwd --listen ADDR --door NAME=URL [--door NAME=URL ...] [flags]  < secrets.json

Secrets arrive on STDIN as one JSON object, never as a flag or env var (those are
visible in /proc and ps):
  {"token":"<session JWT>","key":"<base64 32-byte seed of the generation key>"}

The fence calls http://ADDR/<door>/<path>. fwd adds the token and a proof of
possession, and relays to that door only. Anything not in the route allowlist is
refused 403; decision/approval/owner endpoints can never be routed. fwd exits when
the token expires or on SIGTERM. Closing stdin does not stop it.

Flags (each also settable as CCSBX_FWD_<NAME>, e.g. CCSBX_FWD_LISTEN):
  --listen ADDR     host:port or unix:/path to serve the fence on     (required)
  --door NAME=URL   a door this session may reach; repeatable        (required)
                    e.g. --door model=http://ccsp-model.cc-sandbox-doors:8080
  --routes FILE     JSON allowlist replacing the built-in one: [{"door","methods","path"}]
                    patterns: "*" = one segment, trailing "**" = the rest
  --ca-file FILE    extra root CA for https doors
  --max-body N      request body cap in bytes             (default 33554432)
  --print-routes    print the effective allowlist as JSON and exit (starts nothing)
  --help            this text (starts nothing)

Recipes:
  check what a fence could reach:   ccsbx fwd --print-routes
  local test with a stub door:      printf '{"token":"…","key":"…"}' | ccsbx fwd \
                                      --listen 127.0.0.1:7481 --door model=http://127.0.0.1:8080
`

type secrets struct {
	Token string `json:"token"`
	Key   string `json:"key"`
}

func envOr(name, def string) string {
	if v := os.Getenv("CCSBX_FWD_" + name); v != "" {
		return v
	}
	return def
}

type multi []string

func (m *multi) String() string     { return strings.Join(*m, ",") }
func (m *multi) Set(s string) error { *m = append(*m, s); return nil }

// Main is `ccsbx fwd`. It returns the exit code (2 = usage/config error).
func Main(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			fmt.Fprint(stdout, Usage)
			return 0
		}
	}
	fs := flag.NewFlagSet("fwd", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", envOr("LISTEN", ""), "")
	routesFile := fs.String("routes", envOr("ROUTES", ""), "")
	caFile := fs.String("ca-file", envOr("CA_FILE", ""), "")
	maxBody := fs.Int64("max-body", 0, "")
	printRoutes := fs.Bool("print-routes", false, "")
	var doorFlags multi
	fs.Var(&doorFlags, "door", "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if len(doorFlags) == 0 && os.Getenv("CCSBX_FWD_DOORS") != "" {
		doorFlags = strings.Fields(os.Getenv("CCSBX_FWD_DOORS"))
	}
	rules := DefaultRules
	if *routesFile != "" {
		b, err := os.ReadFile(*routesFile)
		if err == nil {
			rules = nil
			err = json.Unmarshal(b, &rules)
		}
		if err != nil {
			fmt.Fprintln(stderr, "fwd:", err)
			return 2
		}
	}
	if *printRoutes {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(rules)
		return 0
	}
	doors := map[string]*url.URL{}
	for _, d := range doorFlags {
		name, raw, ok := strings.Cut(d, "=")
		u, err := url.Parse(raw)
		if !ok || name == "" || err != nil || u.Host == "" {
			fmt.Fprintf(stderr, "fwd: bad --door %q (want NAME=URL)\n", d)
			return 2
		}
		doors[name] = u
	}
	if *listen == "" || len(doors) == 0 {
		fmt.Fprintln(stderr, "fwd: --listen and at least one --door are required (see --help)")
		return 2
	}
	var sec secrets
	if err := json.NewDecoder(io.LimitReader(stdin, 1<<20)).Decode(&sec); err != nil {
		fmt.Fprintln(stderr, "fwd: secrets on stdin:", err)
		return 2
	}
	seed, err := base64.StdEncoding.DecodeString(sec.Key)
	if err != nil || len(seed) != ed25519.SeedSize {
		fmt.Fprintln(stderr, "fwd: key must be a base64 32-byte seed")
		return 2
	}
	priv := ed25519.NewKeyFromSeed(seed)
	gk := &token.GenKey{Private: priv, Public: priv.Public().(ed25519.PublicKey)}
	f, err := New(Config{Token: sec.Token, Key: gk, Doors: doors, Rules: rules, CAFile: *caFile,
		MaxBody: *maxBody, Logf: func(format string, a ...any) { fmt.Fprintf(stderr, format+"\n", a...) }})
	if err != nil {
		fmt.Fprintln(stderr, "fwd:", err)
		return 2
	}
	network, addr := "tcp", *listen
	if strings.HasPrefix(addr, "unix:") {
		network, addr = "unix", strings.TrimPrefix(addr, "unix:")
	}
	ln, err := net.Listen(network, addr)
	if err != nil {
		fmt.Fprintln(stderr, "fwd:", err)
		return 1
	}
	srv := &http.Server{Handler: f, ReadHeaderTimeout: 10 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	go func() { // dies with the generation
		select {
		case <-ctx.Done():
		case <-time.After(time.Until(f.Expiry())):
		}
		srv.Close()
	}()
	fmt.Fprintf(stderr, "fwd: serving %s for %d door(s); ends %s\n", *listen, len(doors), f.Expiry().UTC().Format(time.RFC3339))
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(stderr, "fwd:", err)
		return 1
	}
	return 0
}
