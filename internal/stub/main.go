package stub

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/token"
)

// Usage is the `ccsbx stub` help text.
const Usage = `ccsbx stub — the scripted model stub, local mode (C18)

Usage:
  ccsbx stub --listen 127.0.0.1:PORT [--key-file FILE] [--aud NAME] [--state FILE]
  ccsbx stub --print-catalog

A fake Anthropic Messages API for e2e runs. Put it behind the forwarder as the
"model" door:  ccsbx fwd --door model=http://127.0.0.1:PORT ...
It serves only session tokens whose profile starts "e2e-" and whose billing is
"stub"; everything else is refused 403. --listen must be a loopback address.

Scripts: a user message containing E2E:<name> selects a row of the fixed catalog
(--print-catalog). Names select; they never carry arguments.

Flags: (each also CCSBX_STUB_<NAME>)
  --listen ADDR      loopback host:port                      (required)
  --key-file FILE    dev key whose public half verifies tokens (default $CCSBX_KEY_FILE)
  --aud NAME         audience the token must list            (default ccsbx-model)
  --state FILE       persist max_gen/revocations here        (default: memory only)
  --print-catalog    print the scripts as JSON and exit (starts nothing)
  --help             this text (starts nothing)

Recipes:
  say hi through a fence:   say "E2E:outcome" in the session; the stub calls ccf outcome
  see what a prompt can do: ccsbx stub --print-catalog
`

func envOr(name, def string) string {
	if v := os.Getenv("CCSBX_STUB_" + name); v != "" {
		return v
	}
	return def
}

// Main is `ccsbx stub`. It returns the exit code (2 = usage/config error).
func Main(args []string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			fmt.Fprint(stdout, Usage)
			return 0
		}
	}
	fs := flag.NewFlagSet("stub", flag.ContinueOnError)
	fs.SetOutput(stderr)
	listen := fs.String("listen", envOr("LISTEN", ""), "")
	keyFile := fs.String("key-file", envOr("KEY_FILE", os.Getenv("CCSBX_KEY_FILE")), "")
	aud := fs.String("aud", envOr("AUD", "ccsbx-model"), "")
	state := fs.String("state", envOr("STATE", ""), "")
	printCat := fs.Bool("print-catalog", false, "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *printCat {
		enc := json.NewEncoder(stdout)
		enc.SetIndent("", "  ")
		enc.Encode(Catalog)
		return 0
	}
	host, _, err := net.SplitHostPort(*listen)
	if err != nil || !isLoopback(host) {
		fmt.Fprintln(stderr, "stub: --listen must be a loopback host:port (local mode only), got", *listen)
		return 2
	}
	k, err := token.LoadKeyFile(*keyFile)
	if err != nil {
		fmt.Fprintln(stderr, "stub:", err)
		return 2
	}
	v := token.NewVerifier(token.Keyring{k.KID: k.Public}, *aud)
	if *state != "" {
		if err := v.PersistTo(*state); err != nil {
			fmt.Fprintln(stderr, "stub:", err)
			return 2
		}
	}
	logf := func(f string, a ...any) { fmt.Fprintf(stderr, f+"\n", a...) }
	srv := &http.Server{Addr: *listen, Handler: Gate(v, Handler(), logf), ReadHeaderTimeout: 10 * time.Second}
	logf("stub: serving e2e scripts on %s", *listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(stderr, "stub:", err)
		return 1
	}
	return 0
}

func isLoopback(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}
