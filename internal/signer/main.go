// main.go is `ccsbx-signer`: flags, --init, socket setup (systemd or its own) and serving. It exists
// so the signer binary is a few lines and all of its decisions are in this package, where the tests
// can reach them. Every flag also reads CCSBX_SIGNER_<NAME>, and --help lists them.
package signer

import (
	"encoding/base64"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/token"
)

// Usage is the --help text.
const Usage = `ccsbx-signer — the machine-key signer (U0b): sign(token_claims) and nothing else

Usage:
  ccsbx-signer --keys DIR --systemd-socket          serve on the socket systemd passes in (fd 3)
  ccsbx-signer --keys DIR --socket PATH             serve on a unix socket this process creates (tests, direct mode)
  ccsbx-signer --keys DIR --init                    create the machine key if absent; print its PUBLIC key
  ccsbx-signer --help

One request per connection: a JSON object of claims in, {"token":...} or {"error":...} out. The
claims are checked before the key is used: sub owner/machine, profile and billing must be inside this
key's scope; every aud must be on --audiences; the lifetime must not exceed --max-ttl. The caller
never sets iat. The reply holds the token only, never the key. Every signing and refusal is logged to
stderr with the claims and without any secret. Peers outside --allow-group are refused (SO_PEERCRED).

Flags: (each also CCSBX_SIGNER_<NAME>, e.g. CCSBX_SIGNER_KEYS)
  --keys DIR          key directory: machine.key (0600, owned by the running uid) and signer.scope  (required)
  --systemd-socket    take the listening socket from systemd: fd 3, LISTEN_FDS=1 and LISTEN_PID checked
  --socket PATH       listen on this unix socket instead (mode 0660; refuses an existing path)
  --init              create machine.key if absent (0600), refuse one with a wrong owner or mode, print
                      "kid <kid>" and "public <base64>" and exit
  --scope FILE        what the key may sign: ONE keyring line for this key with owners=, machines=,
                      profiles= and billings= (all required; 'ccsbx token pubkey' writes it).
                      Without a valid scope the signer will not serve.     (default KEYS/signer.scope)
  --audiences A,B     the only audiences it signs        (default ccsbx-model,ccsbx-egress,ccsbx-stub)
  --max-ttl D         longest token lifetime, at most 12h                   (default 12h)
  --allow-group G     group name or numeric gid whose members may ask       (default ccsbx)
  --max-request N     largest request in bytes                              (default 8192)
  --timeout D         per-connection read/write deadline                    (default 5s)

Recipes:
  first install:   runuser -u ccsbx-keys -- ccsbx-signer --keys /var/lib/ccsbx-keys --init
                   then write the scope:  ccsbx token pubkey --key-file /var/lib/ccsbx-keys/machine.key \
                     --owners tomas --machines hp-ubuntu --profiles 'dev-*' --billings api,subscription \
                     > /var/lib/ccsbx-keys/signer.scope        (run as ccsbx-keys)
  ask for a token: printf '%s' '{"sub":"tomas/hp-ubuntu/sX/g1","billing":"api","profile":"dev-default",
                     "aud":["ccsbx-model"],"cnf":"<b64 pubkey>"}' | nc -U /run/ccsbx-signer/sign.sock
Exit codes: 0 ok, 1 runtime failure, 2 refused to start (bad key, scope, socket or flags).
`

func envOr(name, def string) string {
	if v := os.Getenv("CCSBX_SIGNER_" + name); v != "" {
		return v
	}
	return def
}

// Main is `ccsbx-signer`.
func Main(args []string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			io.WriteString(stdout, Usage)
			return 0
		}
	}
	fs := flag.NewFlagSet("ccsbx-signer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	keys := fs.String("keys", envOr("KEYS", ""), "")
	sysd := fs.Bool("systemd-socket", envOr("SYSTEMD_SOCKET", "") == "1", "")
	sock := fs.String("socket", envOr("SOCKET", ""), "")
	init_ := fs.Bool("init", envOr("INIT", "") == "1", "")
	scope := fs.String("scope", envOr("SCOPE", ""), "")
	auds := fs.String("audiences", envOr("AUDIENCES", "ccsbx-model,ccsbx-egress,ccsbx-stub"), "")
	ttl := fs.Duration("max-ttl", durEnv("MAX_TTL", token.MaxLifetime), "")
	group := fs.String("allow-group", envOr("ALLOW_GROUP", "ccsbx"), "")
	maxReq := fs.Int("max-request", intEnv("MAX_REQUEST", 8192), "")
	timeout := fs.Duration("timeout", durEnv("TIMEOUT", 5*time.Second), "")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if *keys == "" {
		fmt.Fprintln(stderr, "ccsbx-signer: --keys is required (see --help)")
		return 2
	}
	if *init_ {
		k, created, err := Init(*keys)
		if err != nil {
			fmt.Fprintln(stderr, "ccsbx-signer:", err)
			return 2
		}
		fmt.Fprintf(stdout, "kid %s\npublic %s\n", k.KID, base64.StdEncoding.EncodeToString(k.Public))
		if created {
			fmt.Fprintln(stderr, "ccsbx-signer: created", KeyName)
		}
		return 0
	}
	if *sysd == (*sock != "") {
		fmt.Fprintln(stderr, "ccsbx-signer: give exactly one of --systemd-socket and --socket")
		return 2
	}
	k, sc, err := load(*keys, *scope)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	gid, err := groupID(*group)
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx-signer:", err)
		return 2
	}
	logf := func(f string, a ...any) { fmt.Fprintf(stderr, f+"\n", a...) }
	srv, err := New(Config{Key: k, Scope: sc, Audiences: splitCSV(*auds), MaxTTL: *ttl, AllowGID: gid,
		MaxRequest: *maxReq, ReadTimeout: *timeout, Logf: logf})
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 2
	}
	var l net.Listener
	if *sysd {
		l, err = SystemdListener(os.Getenv, os.Getpid(), 3)
	} else {
		l, err = listenUnix(*sock)
	}
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx-signer:", err)
		return 2
	}
	logf(`{"signer":"serving","kid":%q,"allow_gid":%d}`, k.KID, gid)
	if err := srv.Serve(l); err != nil {
		fmt.Fprintln(stderr, "ccsbx-signer:", err)
		return 1
	}
	return 0
}

// SystemdListener takes the socket systemd passes: LISTEN_PID must be this process and LISTEN_FDS
// exactly 1, the socket being the fd at first (3 under systemd). Anything else is refused, so a
// stray environment can't make the signer serve on an fd it didn't get from its parent.
func SystemdListener(env func(string) string, pid, first int) (net.Listener, error) {
	if p, err := strconv.Atoi(env("LISTEN_PID")); err != nil || p != pid {
		return nil, fmt.Errorf("LISTEN_PID %q is not this process (%d): not started by systemd socket activation", env("LISTEN_PID"), pid)
	}
	if n, err := strconv.Atoi(env("LISTEN_FDS")); err != nil || n != 1 {
		return nil, fmt.Errorf("LISTEN_FDS is %q, want exactly 1 socket", env("LISTEN_FDS"))
	}
	f := os.NewFile(uintptr(first), "systemd-socket")
	defer f.Close()
	l, err := net.FileListener(f)
	if err != nil {
		return nil, fmt.Errorf("fd %d is not a listening socket: %w", first, err)
	}
	if _, ok := l.(*net.UnixListener); !ok {
		l.Close()
		return nil, fmt.Errorf("fd %d is not a unix socket", first)
	}
	return l, nil
}

// listenUnix creates the socket itself (not under systemd). It refuses a path that exists.
func listenUnix(path string) (net.Listener, error) {
	if _, err := os.Lstat(path); err == nil {
		return nil, fmt.Errorf("%s already exists; remove a stale socket yourself", path)
	}
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o660); err != nil {
		l.Close()
		return nil, err
	}
	return l, nil
}

// groupID resolves a group name or a number to a gid.
func groupID(g string) (uint32, error) {
	if n, err := strconv.ParseUint(g, 10, 32); err == nil {
		return uint32(n), nil
	}
	gr, err := user.LookupGroup(g)
	if err != nil {
		return 0, fmt.Errorf("--allow-group %q: %w", g, err)
	}
	n, err := strconv.ParseUint(gr.Gid, 10, 32)
	return uint32(n), err
}

func splitCSV(s string) []string {
	var out []string
	for _, x := range strings.Split(s, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func durEnv(name string, def time.Duration) time.Duration {
	if v := os.Getenv("CCSBX_SIGNER_" + name); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			return d
		}
	}
	return def
}

func intEnv(name string, def int) int {
	if n, err := strconv.Atoi(os.Getenv("CCSBX_SIGNER_" + name)); err == nil {
		return n
	}
	return def
}
