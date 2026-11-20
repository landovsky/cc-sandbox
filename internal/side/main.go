// main.go is `ccsbx side`: flags and the two subcommands (serve, load). It exists so the sidecar's
// whole surface (the forwarder port, the control socket, what it refuses) is one --help away.
package side

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/landovsky/cc-sandbox/internal/fwd"
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
        stdin as {"token":"<session JWT>","key":"<base64 32-byte seed>"}. They never appear in the pod
        spec, an argument or the environment. One load per process, only from the sidecar's own uid;
        the key must match the token's cnf, and the token must not be expired.

Flags: (each also CCSBX_SIDE_<NAME>)
  --listen ADDR     forwarder host:port, a loopback IP only        (default 127.0.0.1:8080)
  --control PATH    control socket path, in a sidecar-only volume   (required)
  --door NAME=URL   a door this session may reach; repeatable; CCSBX_SIDE_DOORS is a space-separated list
  --routes FILE     JSON allowlist replacing the built-in one (see ccsbx fwd --help)
  --ca-file FILE    extra root CA for https doors
  --max-body N      request body cap in bytes (default 33554432)
  --allow-uid N     the uid that may load secrets (default: our own)

Recipes:
  from the launcher:  ccsbx token mint … --fwd-secrets | kubectl exec -i -c side POD -- ccsbx side load --control /run/side/ctl.sock
  what the fence sees: the forwarder port only; try  curl -s localhost:8080/model/health  (503 until loaded)
Not built yet (F3b/F3c): outbox relay, session journal, bus relay, git commit/push.
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
	maxBody := fs.Int64("max-body", 0, "")
	allowUID := fs.Int("allow-uid", 0, "")
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
	var rules []fwd.Rule
	if *routesFile != "" {
		b, err := os.ReadFile(*routesFile)
		if err == nil {
			err = json.Unmarshal(b, &rules)
		}
		if err != nil {
			fmt.Fprintln(stderr, "side:", err)
			return 2
		}
	}
	s, err := New(Config{Listen: *listen, Control: *control, Doors: doors, Rules: rules, CAFile: *caFile,
		MaxBody: *maxBody, AllowUID: *allowUID, Logf: func(f string, a ...any) { fmt.Fprintf(stderr, f+"\n", a...) }})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, os.Interrupt)
	defer stop()
	if err := s.Serve(ctx); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	return 0
}
