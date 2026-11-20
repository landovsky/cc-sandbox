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
	"path/filepath"
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
  ccsbx-signer --keys DIR --write-scope --owners O --machines M --pairs P     write DIR/signer.scope
  ccsbx-signer --help

Protocol: one request per connection. The client sends ONE JSON object of claims (sub, billing, profile,
aud, cnf; optional label, exp, routes, repos, branch, placed_by) and may then wait: the signer reads exactly
one JSON value, at most --max-request bytes, so the client need not close its write side. The reply is
{"token":...} or {"error":...}, then the connection closes. The caller never sets iat or kid; unknown
fields are refused. Claims are checked before the key is used: sub owner/machine and the profile→billing
pair must be inside the scope; every aud must be on --audiences; the lifetime must not exceed --max-ttl.
The reply holds the token only, never the key. Every signing and refusal is logged to stderr as JSON, with
the claims and without any secret. Only the process whose uid is --allow-user may ask (SO_PEERCRED).

Flags: (each also CCSBX_SIGNER_<NAME>, e.g. CCSBX_SIGNER_KEYS; a value that does not parse is an error, never a default)
  --keys DIR          key directory, owned by the running uid, mode 0700: machine.key (0600) and signer.scope (required)
  --systemd-socket    take the listening socket from systemd: fd 3, LISTEN_FDS=1 and LISTEN_PID checked
  --socket PATH       listen on this unix socket instead (mode 0660; refuses an existing path)
  --init              create machine.key if absent (0600), refuse a wrong owner, mode or symlink, print
                      "kid <kid>" and "public <base64>" and exit
  --write-scope       write the scope file for this key (atomic, 0600) and print its line; needs --owners,
                      --machines and --pairs (names: letters, digits . _ -). Only this binary ever opens
                      machine.key. A destination outside KEYS must be given as --scope on the command line.
  --owners A,B        owners the key may sign for                                  (with --write-scope)
  --machines X,Y      machines the key may sign for                                (with --write-scope)
  --pairs P           exact profile→billing pairs: dev-default:api|stub,e2e-default:stub   (with --write-scope)
                      a profile not listed, or a billing not listed for it, is never signed
  --scope FILE        scope file: ONE line for this key with owners=, machines=, profiles=, billings= and
                      pairs= (written by --write-scope); owned by root or the running uid, not group/other
                      writable. Without a valid scope the signer will not serve.     (default KEYS/signer.scope)
  --audiences A,B     the only audiences it signs        (default ccsbx-model,ccsbx-egress,ccsbx-stub)
  --max-ttl D         longest token lifetime, at most 12h                          (default 12h)
  --allow-user U      user name or numeric uid allowed to ask: the exact uid, not group membership (default ccsbx)
  --allow-group G     optional: the caller's primary gid must ALSO be this group (name or gid)  (default: not checked)
  --keys-peer-user U  a second allowed caller (name or uid): the pod launcher, ccsbx-launch (never the signer's own user, which can read the machine key; refused). It may sign ONLY for
                      --keys-peer-machines and only tokens with a cnf (a generation key it holds itself, so
                      hostd never sees a pod session's token and key). Those machines are then closed to
                      --allow-user. Off by default; needs --keys-peer-machines, and a uid other than --allow-user.
  --keys-peer-machines M,N   the machines only the pod launcher may sign for, e.g. future-two
  --max-request N     largest request in bytes                                     (default 8192)
  --timeout D         per-connection read and write deadline                       (default 5s)

Recipes:
  first install:   runuser -u ccsbx-keys -- ccsbx-signer --keys /var/lib/ccsbx-keys --init
                   runuser -u ccsbx-keys -- ccsbx-signer --keys /var/lib/ccsbx-keys --write-scope \
                     --owners tomas --machines hp-ubuntu --pairs 'dev-default:api|stub,e2e-default:stub'
  ask for a token: printf '%s' '{"sub":"tomas/hp-ubuntu/sX/g1","billing":"api","profile":"dev-default",
                     "aud":["ccsbx-model"],"cnf":"<base64url pubkey, no padding>"}' | nc -U /run/ccsbx-signer/sign.sock
Exit codes: 0 ok, 1 runtime failure, 2 refused to start (bad key, scope, socket, flag or environment value).
`

type envErr struct{ errs []string }

func (e *envErr) bad(name, v string, err error) {
	e.errs = append(e.errs, fmt.Sprintf("CCSBX_SIGNER_%s=%q: %v", name, v, err))
}

func envOr(name, def string) string {
	if v := os.Getenv("CCSBX_SIGNER_" + name); v != "" {
		return v
	}
	return def
}

func (e *envErr) boolean(name string) bool {
	v := os.Getenv("CCSBX_SIGNER_" + name)
	if v == "" {
		return false
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		e.bad(name, v, err)
	}
	return b
}

func (e *envErr) dur(name string, def time.Duration) time.Duration {
	v := os.Getenv("CCSBX_SIGNER_" + name)
	if v == "" {
		return def
	}
	d, err := time.ParseDuration(v)
	if err != nil {
		e.bad(name, v, err)
		return def
	}
	return d
}

func (e *envErr) integer(name string, def int) int {
	v := os.Getenv("CCSBX_SIGNER_" + name)
	if v == "" {
		return def
	}
	n, err := strconv.Atoi(v)
	if err != nil {
		e.bad(name, v, err)
		return def
	}
	return n
}

// Main is `ccsbx-signer`.
func Main(args []string, stdout, stderr io.Writer) int {
	for _, a := range args {
		if a == "-h" || a == "--help" || a == "-help" {
			io.WriteString(stdout, Usage)
			return 0
		}
	}
	var ee envErr
	fs := flag.NewFlagSet("ccsbx-signer", flag.ContinueOnError)
	fs.SetOutput(stderr)
	keys := fs.String("keys", envOr("KEYS", ""), "")
	sysd := fs.Bool("systemd-socket", ee.boolean("SYSTEMD_SOCKET"), "")
	sock := fs.String("socket", envOr("SOCKET", ""), "")
	init_ := fs.Bool("init", ee.boolean("INIT"), "")
	writeScope := fs.Bool("write-scope", ee.boolean("WRITE_SCOPE"), "")
	owners := fs.String("owners", envOr("OWNERS", ""), "")
	machines := fs.String("machines", envOr("MACHINES", ""), "")
	pairs := fs.String("pairs", envOr("PAIRS", ""), "")
	scope := fs.String("scope", envOr("SCOPE", ""), "")
	auds := fs.String("audiences", envOr("AUDIENCES", "ccsbx-model,ccsbx-egress,ccsbx-stub"), "")
	ttl := fs.Duration("max-ttl", ee.dur("MAX_TTL", token.MaxLifetime), "")
	user_ := fs.String("allow-user", envOr("ALLOW_USER", "ccsbx"), "")
	group := fs.String("allow-group", envOr("ALLOW_GROUP", ""), "")
	kuser := fs.String("keys-peer-user", envOr("KEYS_PEER_USER", ""), "")
	kmach := fs.String("keys-peer-machines", envOr("KEYS_PEER_MACHINES", ""), "")
	maxReq := fs.Int("max-request", ee.integer("MAX_REQUEST", 8192), "")
	timeout := fs.Duration("timeout", ee.dur("TIMEOUT", 5*time.Second), "")
	if len(ee.errs) > 0 {
		for _, m := range ee.errs {
			fmt.Fprintln(stderr, "ccsbx-signer:", m)
		}
		return 2
	}
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
	if *writeScope {
		// A destination outside KEYS must be named on the command line. A value that came from the
		// environment (a stray CCSBX_SIGNER_SCOPE) never counts, or the old scope could stay in force.
		explicit := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "scope" {
				explicit = true
			}
		})
		if *scope != "" && !explicit && filepath.Dir(filepath.Clean(*scope)) != filepath.Clean(*keys) {
			fmt.Fprintf(stderr, "ccsbx-signer: --write-scope would write %s, outside %s, and that came from the environment; pass --scope on the command line to mean it\n", *scope, *keys)
			return 2
		}
		line, err := WriteScope(*keys, *scope, splitCSV(*owners), splitCSV(*machines), *pairs)
		if err != nil {
			fmt.Fprintln(stderr, "ccsbx-signer:", err)
			return 2
		}
		fmt.Fprintln(stdout, line)
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
	uid, err := userID(*user_)
	if err != nil {
		fmt.Fprintln(stderr, "ccsbx-signer:", err)
		return 2
	}
	var gid *uint32
	if *group != "" {
		g, err := groupID(*group)
		if err != nil {
			fmt.Fprintln(stderr, "ccsbx-signer:", err)
			return 2
		}
		gid = &g
	}
	var keysUID *uint32
	if *kuser != "" {
		u, err := userID(*kuser)
		if err != nil {
			fmt.Fprintln(stderr, "ccsbx-signer:", err)
			return 2
		}
		keysUID = &u
	}
	logf := func(f string, a ...any) { fmt.Fprintf(stderr, f+"\n", a...) }
	srv, err := New(Config{Key: k, Scope: sc, Audiences: splitCSV(*auds), MaxTTL: *ttl, AllowUID: uid, AllowGID: gid, KeysUID: keysUID, KeysMachines: splitCSV(*kmach),
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
	srv.logJSON("signer", "serving", "kid", k.KID, "allow_uid", uid)
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

// userID resolves a user name or a number to a uid.
func userID(u string) (uint32, error) {
	if n, err := strconv.ParseUint(u, 10, 32); err == nil {
		return uint32(n), nil
	}
	us, err := user.Lookup(u)
	if err != nil {
		return 0, fmt.Errorf("--allow-user %q: %w", u, err)
	}
	n, err := strconv.ParseUint(us.Uid, 10, 32)
	return uint32(n), err
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
