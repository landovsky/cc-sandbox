// main.go is `ccsbx stub`: flags and the loopback-only listener. It exists so the scripted model can
// run as a door behind the forwarder in local mode and nowhere else.

package stub

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/machines"
	"github.com/landovsky/cc-sandbox/internal/token"
)

// Usage is the `ccsbx stub` help text.
const Usage = `ccsbx stub — the scripted model stub, local mode (C18)

Usage:
  ccsbx stub --listen 127.0.0.1:PORT (--keyring FILE | --machines-dir DIR) [--aud NAME] [--state FILE]
  ccsbx stub --print-catalog

A fake Anthropic Messages API for e2e runs. Put it behind the forwarder as the
"stub" door:  ccsbx fwd --door stub=http://127.0.0.1:PORT ...
It serves only session tokens whose profile starts "e2e-" and whose billing is
"stub"; everything else is refused 403. --listen must be a loopback address.

Scripts: a user message containing E2E:<name> selects a row of the fixed catalog
(--print-catalog). Names select; they never carry arguments.

Flags: (each also CCSBX_STUB_<NAME>)
  --listen ADDR      loopback host:port                      (required)
  --keyring FILE     public keys that may sign tokens, with owner/machine scope; make it with
                     'ccsbx token pubkey'. A door never gets a signing key. (default $CCSBX_KEYRING)
  --machines-dir DIR the trusted-key list (config/machines or its mounted ConfigMap, plan M9) instead of a
                     keyring file: re-read every 15 s, so a removed key stops working with no restart, and
                     each key's profile→billing pairs are enforced. Give this or --keyring, not both
                     (CCSBX_STUB_MACHINES_DIR)
  --aud NAME         audience the token must list            (default ccsbx-stub)
  --state FILE       persist max_gen/revocations here; the stub refuses to start if it can't
                     (default $XDG_STATE_HOME/ccsbx/<aud>-<port>.json, else under ~/.local/state; one door per file)
  --owner-listen ADDR  loopback host:port serving POST /_ccsbx/revoke for hostd (signed by a machine
                     key in the keyring); never route it through the forwarder. Default: off
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
	keyring := fs.String("keyring", envOr("KEYRING", os.Getenv("CCSBX_KEYRING")), "")
	machinesDir := fs.String("machines-dir", envOr("MACHINES_DIR", ""), "")
	aud := fs.String("aud", envOr("AUD", DefaultAud), "")
	state := fs.String("state", envOr("STATE", ""), "")
	ownerListen := fs.String("owner-listen", envOr("OWNER_LISTEN", ""), "")
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
	if (*keyring == "") == (*machinesDir == "") {
		fmt.Fprintln(stderr, "stub: give exactly one of --keyring and --machines-dir")
		return 2
	}
	var ring token.Keyring
	var rl *machines.Reloader
	if *machinesDir != "" {
		if rl, err = machines.NewReloader(*machinesDir); err != nil {
			fmt.Fprintln(stderr, "stub:", err)
			return 2
		}
		rl.Logf = func(f string, a ...any) { fmt.Fprintf(stderr, f+"\n", a...) }
		ring = rl.Ring()
	} else if ring, err = token.LoadPublicKeyring(*keyring); err != nil {
		fmt.Fprintln(stderr, "stub:", err)
		return 2
	}
	v := token.NewVerifier(ring, *aud)
	if rl != nil {
		go rl.Run(context.Background(), v.SetRing)
	}
	if err := persist(v, *state, *aud, *listen); err != nil {
		fmt.Fprintln(stderr, "stub: cannot keep state, refusing to start:", err)
		return 2
	}
	logf := func(f string, a ...any) { fmt.Fprintf(stderr, f+"\n", a...) }
	if *ownerListen != "" {
		oh, _, err := net.SplitHostPort(*ownerListen)
		if err != nil || !isLoopback(oh) {
			fmt.Fprintln(stderr, "stub: --owner-listen must be a loopback host:port, got", *ownerListen)
			return 2
		}
		go func() {
			o := &http.Server{Addr: *ownerListen, Handler: v.RevokeHandler(nil), ReadHeaderTimeout: 10 * time.Second}
			if err := o.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				logf("stub: owner listener: %v", err)
			}
		}()
	}
	srv := &http.Server{Addr: *listen, Handler: Gate(v, Handler(), logf), ReadHeaderTimeout: 10 * time.Second}
	logf("stub: serving e2e scripts on %s", *listen)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		fmt.Fprintln(stderr, "stub:", err)
		return 1
	}
	return 0
}

// isLoopback accepts IP literals only: a name like "localhost" would be resolved later, by
// whatever /etc/hosts or NSS says, and "local mode only" must not depend on that.
func isLoopback(h string) bool {
	ip := net.ParseIP(strings.Trim(h, "[]"))
	return ip != nil && ip.IsLoopback()
}

// defaultState is where a door keeps max_gen and revocations when --state is not given. The listen
// port is part of the name: two stubs with one audience must not share a file.
func defaultState(aud, listen string) string {
	_, port, _ := net.SplitHostPort(listen)
	base := os.Getenv("XDG_STATE_HOME")
	if base == "" {
		h, _ := os.UserHomeDir()
		base = filepath.Join(h, ".local", "state")
	}
	return filepath.Join(base, "ccsbx", aud+"-"+port+".json")
}

// persist gives the verifier durable state. Review S5: a door that forgets revocations and max_gen
// on restart fails open against T:token-gen-bump and T:token-after-exit, so there is no
// memory-only mode: an empty path means the default one.
func persist(v *token.Verifier, state, aud, listen string) error {
	if state == "" {
		state = defaultState(aud, listen)
	}
	if err := os.MkdirAll(filepath.Dir(state), 0o700); err != nil {
		return err
	}
	return v.PersistTo(state)
}
