// signer_test.go checks the signer's refusals and its one allowed path. Each block twin is meant to
// go red if its check is removed: unscoped claims, a lifetime over the cap, a peer outside the group,
// an oversized request, a key with the wrong mode, and a socket that systemd did not hand over. They
// guard against the signer ever signing more than its scope, or serving someone it shouldn't.
package signer

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/token"
)

type rig struct {
	dir  string
	key  *token.Key
	srv  *Server
	sock string
	gen  *token.GenKey
	mu   sync.Mutex
	logs strings.Builder
}

// newRig makes a key, a scope (owner tomas, machine hp-ubuntu, profiles dev-*, billings api|stub)
// and a server on a unix socket in a temp dir. mod may change the config before the server starts.
const testPairs = "dev-default:api|stub,e2e-default:stub"

func newRig(t *testing.T, mod func(*Config)) *rig {
	t.Helper()
	r := &rig{dir: keyDir(t)}
	k, _, err := Init(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	line, err := k.KeyringLineWithPairs([]string{"tomas"}, []string{"hp-ubuntu"}, testPairs)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(r.dir, ScopeName), []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	k2, sc, err := load(r.dir, "")
	if err != nil {
		t.Fatal(err)
	}
	r.key = k2
	r.gen, _ = token.NewGenKey()
	cfg := Config{Key: k2, Scope: sc, Audiences: []string{"ccsbx-model", "ccsbx-stub"}, MaxTTL: time.Hour,
		AllowUID: uint32(os.Getuid()), MaxRequest: 1024, ReadTimeout: 2 * time.Second,
		Logf: func(f string, a ...any) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.logs.WriteString(strings.TrimSpace(formatf(f, a...)) + "\n")
		}}
	if mod != nil {
		mod(&cfg)
	}
	r.srv, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	// the socket path must be short enough for sun_path
	sd, err := os.MkdirTemp("", "sg")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(sd) })
	r.sock = filepath.Join(sd, "s")
	l, err := net.Listen("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go r.srv.Serve(l)
	return r
}

func formatf(f string, a ...any) string { return strings.TrimSpace(sprintf(f, a...)) }

func (r *rig) log() string { r.mu.Lock(); defer r.mu.Unlock(); return r.logs.String() }

func (r *rig) claims(mod func(*token.Claims)) []byte {
	c := token.Claims{Sub: "tomas/hp-ubuntu/sSIGN/g1", Label: "t", Billing: "api", Profile: "dev-default",
		Aud: []string{"ccsbx-model"}, Cnf: token.CnfOf(r.gen.Public)}
	if mod != nil {
		mod(&c)
	}
	b, _ := json.Marshal(c)
	return b
}

func (r *rig) ask(t *testing.T, body []byte) reply {
	t.Helper()
	c, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(3 * time.Second))
	c.Write(body)
	c.(*net.UnixConn).CloseWrite()
	var out reply
	json.NewDecoder(c).Decode(&out)
	return out
}

// context "hostd asks for a session token (the allowed path)": the claims are inside the key's
// scope, so the signer returns a token a door accepts, and the token alone comes back.
func TestSignsInScopeClaims(t *testing.T) {
	r := newRig(t, nil)
	out := r.ask(t, r.claims(nil))
	if out.Token == "" || out.Error != "" {
		t.Fatalf("%+v", out)
	}
	ring := r.key.Ring([]string{"tomas"}, []string{"hp-ubuntu"})
	v := token.NewVerifier(ring, "ccsbx-model")
	now := time.Now()
	p, _ := token.SignProof(r.gen, "POST", "/x", "ccsbx-model", out.Token, nil, now)
	if c, err := v.Verify(out.Token, p, "POST", "/x", nil, now); err != nil || c.Profile != "dev-default" {
		t.Fatalf("a door refused the signer's token: %v", err)
	}
	if strings.Contains(r.log(), out.Token) || strings.Contains(r.log(), base64.StdEncoding.EncodeToString(r.key.Private.Seed())) {
		t.Fatal("the log holds a token or the key")
	}
	if !strings.Contains(r.log(), `"signer":"signed"`) || !strings.Contains(r.log(), "tomas/hp-ubuntu/sSIGN/g1") {
		t.Fatalf("the signing is not logged with its claims: %s", r.log())
	}
}

// context "claims outside the key's scope (T:mac-key-above-mac-max, invariant 27)": billing, profile,
// owner, machine and audience beyond the scope are refused and nothing is signed.
func TestRefusesUnscopedClaims(t *testing.T) {
	r := newRig(t, nil)
	for name, mod := range map[string]func(*token.Claims){
		"billing above the tier":   func(c *token.Claims) { c.Billing = "subscription" },
		"profile outside":          func(c *token.Claims) { c.Profile = "prod-admin" },
		"profile not in a pair":    func(c *token.Claims) { c.Profile = "dev-x" },
		"dev-default+subscription": func(c *token.Claims) { c.Billing = "subscription" },
		"e2e-default+api":          func(c *token.Claims) { c.Profile, c.Billing = "e2e-default", "api" },
		"other machine":            func(c *token.Claims) { c.Sub = "tomas/mac/sX/g1" },
		"other owner":              func(c *token.Claims) { c.Sub = "eve/hp-ubuntu/sX/g1" },
		"audience off the list":    func(c *token.Claims) { c.Aud = []string{"ccsbx-model", "ccsbx-admin"} },
		"caller sets iat":          func(c *token.Claims) { c.Iat = 1 },
	} {
		if out := r.ask(t, r.claims(mod)); out.Token != "" || out.Error == "" {
			t.Fatalf("%s: signed: %+v", name, out)
		}
	}
	// Unknown fields (a kid, a jti, anything) are refused even when the rest is valid: the body is a
	// real claim set plus one extra field, so only the unknown-field check can refuse it.
	var withKid map[string]any
	json.Unmarshal(r.claims(nil), &withKid)
	withKid["kid"] = "x"
	b, _ := json.Marshal(withKid)
	if out := r.ask(t, b); out.Token != "" || out.Error != "bad request" {
		t.Fatalf("an unknown field was accepted: %+v", out)
	}
	if out := r.ask(t, r.claims(func(c *token.Claims) { c.Billing = "stub" })); out.Token == "" {
		t.Fatalf("allow twin: dev-default+stub is a listed pair: %+v", out)
	}
}

// context "a long-lived token is a long-lived leak": exp beyond --max-ttl is refused, within it signed.
func TestRefusesOverLongExpiry(t *testing.T) {
	r := newRig(t, nil)
	if out := r.ask(t, r.claims(func(c *token.Claims) { c.Exp = time.Now().Add(30 * time.Minute).Unix() })); out.Token == "" {
		t.Fatalf("allow twin: %+v", out)
	}
	if out := r.ask(t, r.claims(func(c *token.Claims) { c.Exp = time.Now().Add(2 * time.Hour).Unix() })); out.Token != "" {
		t.Fatal("signed past the 1h cap")
	}
	if _, err := New(Config{Key: r.key, Audiences: []string{"a"}, MaxTTL: 13 * time.Hour}); err == nil {
		t.Fatal("a cap above the token maximum was accepted")
	}
}

// context "only hostd's uid may ask": the kernel supplies the peer's uid (SO_PEERCRED), so this runs
// over a real socket as this user. Another uid is refused; so is the right uid with the wrong primary
// gid when a gid is required. Group membership in /etc/group is not consulted at all.
func TestRefusesPeerThatIsNotTheAllowedUser(t *testing.T) {
	r := newRig(t, nil)
	if out := r.ask(t, r.claims(nil)); out.Token == "" {
		t.Fatalf("allow twin: %+v", out)
	}
	r2 := newRig(t, func(c *Config) { c.AllowUID = uint32(os.Getuid()) + 1 })
	if out := r2.ask(t, r2.claims(nil)); out.Token != "" {
		t.Fatal("a peer with another uid was served")
	}
	if !strings.Contains(r2.log(), "peer is not the allowed caller") {
		t.Fatalf("refusal not logged: %s", r2.log())
	}
	wrong := uint32(os.Getgid()) + 1
	r3 := newRig(t, func(c *Config) { c.AllowGID = &wrong })
	if out := r3.ask(t, r3.claims(nil)); out.Token != "" {
		t.Fatal("a peer with the wrong gid was served")
	}
}

// context "the hostd client writes its request and waits for the reply": it does not close its write
// side. The signer must answer at once, not after its deadline.
func TestClientThatDoesNotHalfClose(t *testing.T) {
	r := newRig(t, nil)
	c, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(1500 * time.Millisecond)) // far below the rig's 2s server deadline
	c.Write(r.claims(nil))
	var out reply
	if err := json.NewDecoder(c).Decode(&out); err != nil || out.Token == "" {
		t.Fatalf("no reply without a half-close: %v %+v", err, out)
	}
}

// context "a client that stalls": half a request and silence ends in a 'timeout' reply, not a hang
// and not a misleading 'too large'.
func TestStalledClientTimesOut(t *testing.T) {
	r := newRig(t, func(c *Config) { c.ReadTimeout = 300 * time.Millisecond })
	c, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write([]byte(`{"sub":`))
	var out reply
	json.NewDecoder(c).Decode(&out)
	if out.Error != "timeout" || out.Token != "" {
		t.Fatalf("%+v", out)
	}
}

// context "a log line is JSON whatever the caller sends": control bytes and a huge sub don't break
// the line or fill the log.
func TestLogLinesAreJSON(t *testing.T) {
	r := newRig(t, nil)
	r.ask(t, r.claims(func(c *token.Claims) { c.Sub = "tomas/hp-ubuntu/s\x00\a\v" + strings.Repeat("x", 2000) + "/g1" }))
	n := 0
	for _, l := range strings.Split(strings.TrimSpace(r.log()), "\n") {
		var m map[string]any
		if err := json.Unmarshal([]byte(l), &m); err != nil {
			t.Fatalf("not JSON: %q", l)
		}
		if len(l) > 1500 {
			t.Fatalf("log line of %d bytes", len(l))
		}
		n++
	}
	if n == 0 {
		t.Fatal("nothing logged")
	}
}

// context "an oversized request is never parsed": it is refused, the signer stays up and still signs.
func TestRefusesOversizedRequest(t *testing.T) {
	r := newRig(t, nil)
	big := r.claims(func(c *token.Claims) { c.Label = strings.Repeat("x", 4096) })
	if out := r.ask(t, big); out.Token != "" || !strings.Contains(out.Error, "too large") {
		t.Fatalf("signed an oversized request: %+v", out)
	}
	if out := r.ask(t, r.claims(nil)); out.Token == "" {
		t.Fatal("the signer did not survive an oversized request")
	}
}

// writeScope puts a valid scope for k in dir (as --write-scope would).
func writeScope(t *testing.T, dir string) {
	t.Helper()
	if _, err := WriteScope(dir, "", []string{"tomas"}, []string{"hp-ubuntu"}, testPairs); err != nil {
		t.Fatal(err)
	}
}

// context "a normal install": --init makes a 0600 key in a 0700 directory, --write-scope a scope, and
// the signer loads both. A second --init changes nothing.
func TestKeyFileAccepted(t *testing.T) {
	dir := keyDir(t)
	k, created, err := Init(dir)
	if err != nil || !created {
		t.Fatal(err, created)
	}
	if fi, _ := os.Stat(filepath.Join(dir, KeyName)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", fi.Mode().Perm())
	}
	k2, created2, err := Init(dir)
	if err != nil || created2 || k2.KID != k.KID {
		t.Fatalf("second init: %v %v", err, created2)
	}
	writeScope(t, dir)
	if _, sc, err := load(dir, ""); err != nil || sc.Pairs["dev-default"] == nil {
		t.Fatalf("load: %v", err)
	}
}

// context "the key file is wrong": a mode other than exactly 0600 (0644 anyone could read; 0400 is
// not what the installer made), a symlink to a good key elsewhere, a key owned by someone else and a
// key directory that is not 0700 are all refused by --init and by the signer at start. Each is a
// case only these checks catch.
func TestKeyFileIsChecked(t *testing.T) {
	dir := keyDir(t)
	Init(dir)
	writeScope(t, dir)
	key := filepath.Join(dir, KeyName)
	for _, mode := range []os.FileMode{0o644, 0o400, 0o660} {
		os.Chmod(key, mode)
		if _, _, err := Init(dir); err == nil {
			t.Fatalf("--init accepted a key with mode %04o", mode)
		}
		if _, _, err := load(dir, ""); err == nil {
			t.Fatalf("the signer loaded a key with mode %04o", mode)
		}
	}
	os.Chmod(key, 0o600)

	// a symlink to another, perfectly good 0600 key file
	other := filepath.Join(t.TempDir(), "elsewhere.key")
	b, _ := os.ReadFile(key)
	os.WriteFile(other, b, 0o600)
	os.Remove(key)
	os.Symlink(other, key)
	if _, _, err := load(dir, ""); err == nil {
		t.Fatal("the signer followed a symlink for the key")
	}
	os.Remove(key)
	os.WriteFile(key, b, 0o600)
	if _, _, err := load(dir, ""); err != nil {
		t.Fatalf("allow twin: %v", err)
	}

	// owner: play another uid (no root needed)
	runUID = func() int { return os.Getuid() + 1 }
	_, err := readKey(key) // the key alone, so the directory check can't be what catches it
	runUID = os.Getuid
	if err == nil {
		t.Fatal("the signer read a key owned by another uid")
	}

	// the directory
	os.Chmod(dir, 0o755)
	_, _, err = load(dir, "")
	os.Chmod(dir, 0o700)
	if err == nil {
		t.Fatal("the signer used a 0755 key directory")
	}
}

// context "the scope is the signing policy": a scope file that group or others can write, one owned by
// another uid, or one that is a symlink is refused, since whoever can edit it decides what is signed.
func TestScopeFileIsChecked(t *testing.T) {
	dir := keyDir(t)
	Init(dir)
	writeScope(t, dir)
	sc := filepath.Join(dir, ScopeName)
	os.Chmod(sc, 0o666)
	if _, _, err := load(dir, ""); err == nil {
		t.Fatal("a world-writable scope was accepted")
	}
	os.Chmod(sc, 0o600)
	runUID = func() int { return os.Getuid() + 1 }
	_, err := readScope(sc)
	runUID = os.Getuid
	if err == nil && os.Getuid() != 0 {
		t.Fatal("a scope owned by another uid was accepted")
	}
	b, _ := os.ReadFile(sc)
	os.Remove(sc)
	other := filepath.Join(t.TempDir(), "scope")
	os.WriteFile(other, b, 0o600)
	os.Symlink(other, sc)
	if _, _, err := load(dir, ""); err == nil {
		t.Fatal("a symlinked scope was accepted")
	}
	// --scope in a directory anyone can write into
	loose := t.TempDir()
	os.Chmod(loose, 0o777)
	os.WriteFile(filepath.Join(loose, "s"), b, 0o600)
	if _, _, err := load(dir, filepath.Join(loose, "s")); err == nil {
		t.Fatal("a scope in a world-writable directory was accepted")
	}
}

// context "no scope, no signing": a missing scope file, a scope without pairs (the older flat form),
// or a scope for another key stops the signer from starting.
func TestWontServeWithoutAScope(t *testing.T) {
	dir := keyDir(t)
	k, _, _ := Init(dir)
	if _, _, err := load(dir, ""); err == nil {
		t.Fatal("loaded with no scope file")
	}
	flat := k.KeyringLineScoped([]string{"tomas"}, []string{"hp-ubuntu"}, []string{"dev-*"}, []string{"api"})
	os.WriteFile(filepath.Join(dir, ScopeName), []byte(flat+"\n"), 0o600)
	if _, _, err := load(dir, ""); err == nil {
		t.Fatal("loaded a scope with no pairs")
	}
	other := keyDir(t)
	k3, _, _ := Init(other)
	l3, _ := k3.KeyringLineWithPairs([]string{"tomas"}, []string{"hp-ubuntu"}, testPairs)
	os.WriteFile(filepath.Join(dir, ScopeName), []byte(l3+"\n"), 0o600)
	if _, _, err := load(dir, ""); err == nil {
		t.Fatal("loaded another key's scope")
	}
}

// context "systemd hands the socket over as fd 3": a fake LISTEN_FDS/LISTEN_PID with a real
// listening socket is taken.
func TestSystemdSocketAccepted(t *testing.T) {
	fd, cleanup := listeningFD(t)
	defer cleanup()
	pid := os.Getpid()
	l, err := SystemdListener(envOf(map[string]string{"LISTEN_PID": itoa(pid), "LISTEN_FDS": "1"}), pid, dupFD(t, fd))
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}

// context "a stray environment must not make the signer serve": the wrong pid, a count other than 1
// and an fd that isn't a socket are refused.
func TestSystemdSocketRefusals(t *testing.T) {
	fd, cleanup := listeningFD(t)
	defer cleanup()
	pid := os.Getpid()
	for name, m := range map[string]map[string]string{
		"wrong pid":   {"LISTEN_PID": itoa(pid + 1), "LISTEN_FDS": "1"},
		"two sockets": {"LISTEN_PID": itoa(pid), "LISTEN_FDS": "2"},
		"none":        {"LISTEN_PID": itoa(pid), "LISTEN_FDS": "0"},
		"unset":       {},
	} {
		if l, err := SystemdListener(envOf(m), pid, dupFD(t, fd)); err == nil {
			l.Close()
			t.Fatalf("%s: accepted", name)
		}
	}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	if l, err := SystemdListener(envOf(map[string]string{"LISTEN_PID": itoa(pid), "LISTEN_FDS": "1"}), pid, int(devnull.Fd())); err == nil {
		l.Close()
		t.Fatal("an fd that is not a socket was accepted")
	}
}

func envOf(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func listeningFD(t *testing.T) (int, func()) {
	t.Helper()
	sd, _ := os.MkdirTemp("", "sg")
	l0, err := net.Listen("unix", filepath.Join(sd, "s"))
	if err != nil {
		t.Fatal(err)
	}
	f, _ := l0.(*net.UnixListener).File()
	return int(f.Fd()), func() { f.Close(); l0.Close(); os.RemoveAll(sd) }
}

// context "--write-scope: the key is read by the signer only": it writes the scope (0600, with the
// pairs), prints the line, never prints the seed, and refuses missing owners or a bad pair.
func TestWriteScopeCommand(t *testing.T) {
	dir := keyDir(t)
	var out, errb bytes.Buffer
	Main([]string{"--keys", dir, "--init"}, &out, &errb)
	out.Reset()
	args := []string{"--keys", dir, "--write-scope", "--owners", "tomas", "--machines", "hp-ubuntu", "--pairs", testPairs}
	if Main(args, &out, &errb) != 0 || !strings.Contains(out.String(), "pairs="+testPairs) {
		t.Fatalf("%s %s", out.String(), errb.String())
	}
	seed, _ := os.ReadFile(filepath.Join(dir, KeyName))
	if strings.Contains(out.String(), strings.TrimSpace(string(seed))) {
		t.Fatal("the seed was printed")
	}
	if fi, _ := os.Stat(filepath.Join(dir, ScopeName)); fi.Mode().Perm() != 0o600 {
		t.Fatalf("scope mode %v", fi.Mode().Perm())
	}
	if _, _, err := load(dir, ""); err != nil {
		t.Fatal(err)
	}
	for name, a := range map[string][]string{
		"no owners":       {"--keys", dir, "--write-scope", "--machines", "hp-ubuntu", "--pairs", testPairs},
		"bad billing":     {"--keys", dir, "--write-scope", "--owners", "tomas", "--machines", "hp-ubuntu", "--pairs", "dev-default:free"},
		"glob in profile": {"--keys", dir, "--write-scope", "--owners", "tomas", "--machines", "hp-ubuntu", "--pairs", "dev-*:api"},
		"no pairs":        {"--keys", dir, "--write-scope", "--owners", "tomas", "--machines", "hp-ubuntu"},
	} {
		if Main(a, &out, &errb) != 2 {
			t.Fatalf("%s: accepted", name)
		}
	}
}

// context "--help and the environment": every flag is listed with its env twin, --help starts
// nothing, CCSBX_SIGNER_* reaches the same code path as a flag, and a value that doesn't parse stops
// the signer instead of quietly using the default (a typo'd MAX_TTL must not mean 12h).
func TestHelpAndEnv(t *testing.T) {
	var out, errb bytes.Buffer
	if Main([]string{"--help"}, &out, &errb) != 0 {
		t.Fatal("help")
	}
	for _, fl := range []string{"--keys", "--systemd-socket", "--socket", "--init", "--write-scope", "--owners", "--machines",
		"--pairs", "--scope", "--audiences", "--max-ttl", "--allow-user", "--allow-group", "--keys-peer-user", "--keys-peer-socket", "--keys-peer-machines", "--max-request", "--timeout", "CCSBX_SIGNER_"} {
		if !strings.Contains(out.String(), fl) {
			t.Errorf("--help lacks %s", fl)
		}
	}
	dir := keyDir(t)
	t.Setenv("CCSBX_SIGNER_KEYS", dir)
	t.Setenv("CCSBX_SIGNER_INIT", "1")
	out.Reset()
	if Main(nil, &out, &errb) != 0 || !strings.Contains(out.String(), "kid ") || strings.Contains(out.String(), "seed") {
		t.Fatalf("init via env: %s %s", out.String(), errb.String())
	}
	t.Setenv("CCSBX_SIGNER_INIT", "")
	if Main([]string{"--systemd-socket"}, &out, &errb) != 2 {
		t.Fatal("served with no scope file")
	}
	for k, v := range map[string]string{"MAX_TTL": "1hr", "MAX_REQUEST": "lots", "TIMEOUT": "soon", "SYSTEMD_SOCKET": "maybe", "INIT": "yes please"} {
		t.Setenv("CCSBX_SIGNER_"+k, v)
		errb.Reset()
		if Main([]string{"--socket", filepath.Join(dir, "x")}, &out, &errb) != 2 || !strings.Contains(errb.String(), "CCSBX_SIGNER_"+k) {
			t.Fatalf("%s=%q was not refused: %s", k, v, errb.String())
		}
		t.Setenv("CCSBX_SIGNER_"+k, "")
	}
	writeScope(t, dir)
	if _, err := New(Config{Key: &token.Key{}, Audiences: []string{"a"}, MaxTTL: time.Hour, MaxRequest: -1, ReadTimeout: time.Second}); err == nil {
		t.Fatal("a negative --max-request was replaced by a default")
	}
}

func sprintf(f string, a ...any) string { return fmt.Sprintf(f, a...) }
func itoa(n int) string                 { return strconv.Itoa(n) }

// dupFD returns a new fd for the same open file, as systemd would pass at fd 3 (SystemdListener
// closes the fd it is given).
func dupFD(t *testing.T, fd int) int {
	t.Helper()
	n, err := syscall.Dup(fd)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// keyDir is a not-yet-existing key directory; Init makes it 0700 (t.TempDir() itself is 0775 under a
// 002 umask, which the signer rightly refuses).
func keyDir(t *testing.T) string { return filepath.Join(t.TempDir(), "keys") }

// context "one request is one JSON value": anything after it, in the same write or just after, is a
// refused request rather than silently ignored (a second object must not ride along unseen).
func TestRefusesDataAfterTheRequest(t *testing.T) {
	r := newRig(t, nil)
	good := r.claims(nil)
	if out := r.ask(t, append(append([]byte{}, good...), '\n', ' ')); out.Token == "" {
		t.Fatalf("allow twin: trailing white space: %+v", out)
	}
	if out := r.ask(t, append(append([]byte{}, good...), []byte(`{"sub":"x"}`)...)); out.Token != "" || out.Error != "bad request" {
		t.Fatalf("a second object in the same write was accepted: %+v", out)
	}
	c, err := net.Dial("unix", r.sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(2 * time.Second))
	c.Write(good)
	time.Sleep(3 * time.Millisecond)
	c.Write([]byte(`garbage`))
	var out reply
	json.NewDecoder(c).Decode(&out)
	if out.Token != "" {
		t.Fatalf("data sent just after the request was accepted: %+v", out)
	}
}

type failListener struct{ calls int }

func (f *failListener) Accept() (net.Conn, error) {
	f.calls++
	return nil, errors.New("too many open files")
}
func (f *failListener) Close() error   { return nil }
func (f *failListener) Addr() net.Addr { return &net.UnixAddr{Name: "x", Net: "unix"} }

// context "accept keeps failing (a permanent error)": Serve gives up and returns the error, so the
// signer exits non-zero and systemd restarts it, instead of logging ten lines a second for ever.
func TestPermanentAcceptErrorEndsServe(t *testing.T) {
	r := newRig(t, func(c *Config) { c.AcceptPause = time.Millisecond })
	fl := &failListener{}
	done := make(chan error, 1)
	go func() { done <- r.srv.Serve(fl) }()
	select {
	case err := <-done:
		if err == nil || fl.calls < maxAcceptFailures {
			t.Fatalf("err=%v after %d calls", err, fl.calls)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Serve kept retrying a permanent accept error")
	}
}

// context "a scope file is read whole or not at all": over 64 KiB is refused, since a scope cut short
// could lose its last lines; one just under the cap still loads.
func TestOversizedScopeIsRefused(t *testing.T) {
	dir := keyDir(t)
	Init(dir)
	line, _ := WriteScope(dir, "", []string{"tomas"}, []string{"hp-ubuntu"}, testPairs)
	sc := filepath.Join(dir, ScopeName)
	pad := func(n int) []byte {
		return []byte(line + "\n" + strings.Repeat("# padding\n", n/10)) // the policy comes first: a cut-short read would still parse
	}
	os.WriteFile(sc, pad(60<<10), 0o600)
	if _, _, err := load(dir, ""); err != nil {
		t.Fatalf("allow twin: %v", err)
	}
	os.WriteFile(sc, pad(70<<10), 0o600)
	if _, _, err := load(dir, ""); err == nil {
		t.Fatal("a scope over 64 KiB was accepted")
	}
}

// context "owners and machines are names": a space, '=', a glob or a stray separator can't be written
// into the scope, the same rule as profile names.
func TestScopeNamesAreChecked(t *testing.T) {
	dir := keyDir(t)
	Init(dir)
	if _, err := WriteScope(dir, "", []string{"tomas"}, []string{"hp-ubuntu"}, testPairs); err != nil {
		t.Fatalf("allow twin: %v", err)
	}
	for _, bad := range []string{"to mas", "tomas=x", "to*", "a/b", "tom\tas", ""} {
		if _, err := WriteScope(dir, "", []string{bad}, []string{"hp-ubuntu"}, testPairs); err == nil {
			t.Fatalf("owner %q accepted", bad)
		}
		if _, err := WriteScope(dir, "", []string{"tomas"}, []string{bad}, testPairs); err == nil {
			t.Fatalf("machine %q accepted", bad)
		}
	}
}

// context "a stray CCSBX_SIGNER_SCOPE": --write-scope must not quietly write somewhere else and leave
// the old scope in force. A destination outside KEYS has to be named on the command line.
func TestWriteScopeDestinationMustBeExplicit(t *testing.T) {
	dir := keyDir(t)
	var out, errb bytes.Buffer
	Main([]string{"--keys", dir, "--init"}, &out, &errb)
	elsewhere := filepath.Join(t.TempDir(), "scope")
	args := []string{"--keys", dir, "--write-scope", "--owners", "tomas", "--machines", "hp-ubuntu", "--pairs", testPairs}
	t.Setenv("CCSBX_SIGNER_SCOPE", elsewhere)
	if Main(args, &out, &errb) != 2 {
		t.Fatal("wrote a scope outside KEYS because of an environment variable")
	}
	if _, err := os.Stat(elsewhere); err == nil {
		t.Fatal("the stray destination was written")
	}
	t.Setenv("CCSBX_SIGNER_SCOPE", "")
	if Main(append(args, "--scope", elsewhere), &out, &errb) != 0 {
		t.Fatalf("an explicit --scope was refused: %s", errb.String())
	}
	if _, err := os.Stat(elsewhere); err != nil {
		t.Fatal("the explicit destination was not written")
	}
	// inside KEYS, even from the environment, is fine
	t.Setenv("CCSBX_SIGNER_SCOPE", filepath.Join(dir, ScopeName))
	if Main(args, &out, &errb) != 0 {
		t.Fatalf("a destination inside KEYS was refused: %s", errb.String())
	}
}

// ---- the pod launcher as a second caller (F2w) ----

// peers makes a signer in which the test process plays one of two uids: asHostd=true means the
// process is the hostd uid and the keys peer is somebody else, false the other way round. future-two
// is in the key's scope and belongs to the keys peer.
func peers(t *testing.T, asHostd bool) *rig {
	t.Helper()
	me, other := uint32(os.Getuid()), uint32(os.Getuid())+1
	return newRig(t, func(c *Config) {
		c.Scope.Machines = append(c.Scope.Machines, "future-two")
		c.KeysMachines = []string{"future-two"}
		sign := other + 100 // the signer's own uid: neither caller (the test process plays one of the two)
		c.OwnUID = &sign
		if asHostd {
			c.AllowUID, c.KeysUID = me, &other
		} else {
			c.AllowUID, c.KeysUID = other, &me
		}
	})
}

func (r *rig) onMachine(m string) []byte {
	return r.claims(func(c *token.Claims) { c.Sub = "tomas/" + m + "/sSIGN/g1" })
}

// context "hostd must not end up holding a pod session's token and key": the machine the pod launcher
// owns is closed to hostd, while hostd keeps signing for its own machine.
func TestHostdCannotSignForThePodMachine(t *testing.T) {
	r := peers(t, true)
	if out := r.ask(t, r.onMachine("hp-ubuntu")); out.Token == "" {
		t.Fatalf("hostd lost its own machine: %+v", out)
	}
	out := r.ask(t, r.onMachine("future-two"))
	if out.Token != "" || !strings.Contains(out.Error, "pod launcher") {
		t.Fatalf("hostd got a pod-session token: %+v", out)
	}
}

// context "the launcher asks for exactly its pod sessions": only its own machines, only with a cnf,
// and still inside the key's scope (owner, profile and billing pair).
func TestThePodLauncherSignsOnlyPodSessionsBoundToAKey(t *testing.T) {
	r := peers(t, false)
	if out := r.ask(t, r.onMachine("future-two")); out.Token == "" {
		t.Fatalf("allow twin: %+v", out)
	}
	if out := r.ask(t, r.onMachine("hp-ubuntu")); out.Token != "" {
		t.Error("the pod launcher signed for hostd's machine")
	}
	if out := r.ask(t, r.claims(func(c *token.Claims) { c.Sub = "tomas/future-two/sSIGN/g1"; c.Cnf = "" })); out.Token != "" || !strings.Contains(out.Error, "generation key") {
		t.Errorf("the pod launcher's no-cnf request: %+v (refused by its own rule, not by chance)", out)
	}
	for why, mod := range map[string]func(*token.Claims){
		"another owner": func(c *token.Claims) { c.Sub = "mallory/future-two/sSIGN/g1" },
		"a pair off the scope": func(c *token.Claims) {
			c.Sub = "tomas/future-two/sSIGN/g1"
			c.Profile, c.Billing = "e2e-default", "api"
		},
	} {
		if out := r.ask(t, r.claims(mod)); out.Token != "" {
			t.Errorf("%s was signed for the pod launcher", why)
		}
	}
}

// context "a third uid": neither caller, refused as before.
func TestAThirdUIDIsRefusedEvenWithTheKeysPeerConfigured(t *testing.T) {
	r := newRig(t, func(c *Config) {
		a, b := uint32(os.Getuid())+1, uint32(os.Getuid())+2
		own := uint32(os.Getuid()) + 100
		c.OwnUID = &own
		c.Scope.Machines = append(c.Scope.Machines, "future-two")
		c.AllowUID, c.KeysUID, c.KeysMachines = a, &b, []string{"future-two"}
	})
	if out := r.ask(t, r.onMachine("future-two")); out.Token != "" {
		t.Error("a third uid was served")
	}
}

func TestKeysPeerConfigurationIsChecked(t *testing.T) {
	me := uint32(os.Getuid())
	k, _, err := Init(keyDir(t))
	if err != nil {
		t.Fatal(err)
	}
	own := me + 100
	base := Config{OwnUID: &own, Key: k, Audiences: []string{"a"}, MaxTTL: time.Hour, MaxRequest: 10, ReadTimeout: time.Second, AllowUID: me}
	other := me + 1
	for why, mod := range map[string]func(*Config){
		"a keys peer with no machines":   func(c *Config) { c.KeysUID = &other },
		"machines with no keys peer":     func(c *Config) { c.KeysMachines = []string{"future-two"} },
		"the keys peer is the hostd uid": func(c *Config) { c.KeysUID, c.KeysMachines = &me, []string{"future-two"} },
	} {
		c := base
		mod(&c)
		if _, err := New(c); err == nil {
			t.Errorf("%s was accepted", why)
		}
	}
}

// The client builds what the sidecar's load reads, and the key never crosses the socket.
func TestMintFwdSecretsBindsTheTokenToAKeyTheClientHolds(t *testing.T) {
	r := peers(t, false)
	var sent token.Claims
	sign := func(ctx context.Context, c token.Claims) (string, error) {
		sent = c
		return Client{Socket: r.sock}.Sign(ctx, c)
	}
	b, err := MintFwdSecrets(context.Background(), sign, token.Claims{Sub: "tomas/future-two/sSIGN/g1", Billing: "api", Profile: "dev-default", Aud: []string{"ccsbx-model"}})
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]string
	if json.Unmarshal(b, &s) != nil || len(s) != 2 || s["token"] == "" {
		t.Fatalf("%s", b)
	}
	seed, err := base64.StdEncoding.DecodeString(s["key"])
	if err != nil || len(seed) != 32 {
		t.Fatalf("key %q: %v", s["key"], err)
	}
	if sent.Cnf == "" || strings.Contains(r.log(), s["key"]) || strings.Contains(r.log(), s["token"]) {
		t.Errorf("cnf %q, or a secret reached the signer's log", sent.Cnf)
	}
	if _, err := (Client{Socket: r.sock + "x"}).Sign(context.Background(), sent); err == nil {
		t.Error("a missing socket signed")
	}
}

// The pod's one payload carries three secrets, and the relay is registered for the very key the token
// is bound to (cnf equality holds by construction).
func TestMintPodSecretsRegistersTheRelayForTheTokensOwnKey(t *testing.T) {
	r := peers(t, false)
	var signedCnf, relayPub string
	sign := func(ctx context.Context, c token.Claims) (string, error) {
		signedCnf = c.Cnf
		return Client{Socket: r.sock}.Sign(ctx, c)
	}
	b, err := MintPodSecrets(context.Background(), sign, token.Claims{Sub: "tomas/future-two/sSIGN/g1", Billing: "api", Profile: "dev-default", Aud: []string{"ccsbx-model"}},
		func(pub string) (string, error) { relayPub = pub; return "sbp1.RELAY", nil })
	if err != nil {
		t.Fatal(err)
	}
	var s map[string]string
	if json.Unmarshal(b, &s) != nil || len(s) != 3 || s["token"] == "" || s["relay_token"] != "sbp1.RELAY" || s["key"] == "" {
		t.Fatalf("%s", b)
	}
	if relayPub == "" || relayPub != signedCnf {
		t.Errorf("relay registered for %q, token bound to %q", relayPub, signedCnf)
	}
	if _, err := MintPodSecrets(context.Background(), sign, token.Claims{Sub: "tomas/future-two/sSIGN/g1", Billing: "api", Profile: "dev-default", Aud: []string{"ccsbx-model"}},
		func(string) (string, error) { return "", errors.New("hub refused") }); err == nil {
		t.Error("a payload was made without a relay token")
	}
}

// ---- the keys peer's own socket (R2-S2) ----

// twoSockets serves a signer on two unix sockets, the way ccsbx-signer does with a keys peer: the main
// socket takes hostd only, the other takes the keys peer only. The test process plays hostd or the keys peer.
func twoSockets(t *testing.T, asHostd bool) (r *rig, main, keys string) {
	t.Helper()
	r = peers(t, asHostd)
	sd, _ := os.MkdirTemp("", "sg")
	t.Cleanup(func() { os.RemoveAll(sd) })
	main, keys = filepath.Join(sd, "m"), filepath.Join(sd, "k")
	l1, err := net.Listen("unix", main)
	if err != nil {
		t.Fatal(err)
	}
	l2, err := net.Listen("unix", keys)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l1.Close(); l2.Close() })
	go r.srv.ServeHostd(l1)
	go r.srv.ServeKeysPeer(l2)
	return r, main, keys
}

func askOn(t *testing.T, r *rig, sock string, body []byte) reply {
	t.Helper()
	r.sock = sock
	return r.ask(t, body)
}

// context "the launch user reaches the signer on a socket of its own, and each socket takes its own class":
// hostd's socket (group ccsbx) is not reachable by ccsbx-launch, so the keys peer has another; and a
// mode that is later widened on one of them still lets in only the class that socket is for.
func TestEachSocketTakesItsOwnClassOnly(t *testing.T) {
	r, main, keys := twoSockets(t, true) // this process is hostd
	if out := askOn(t, r, main, r.onMachine("hp-ubuntu")); out.Token == "" {
		t.Fatalf("hostd on its own socket: %+v", out)
	}
	if out := askOn(t, r, keys, r.onMachine("hp-ubuntu")); out.Token != "" || !strings.Contains(r.log(), "other caller's socket") {
		t.Errorf("hostd got a token on the keys peer's socket: %+v\n%s", out, r.log())
	}
	r, main, keys = twoSockets(t, false) // this process is the keys peer
	if out := askOn(t, r, keys, r.onMachine("future-two")); out.Token == "" {
		t.Fatalf("the keys peer on its own socket: %+v", out)
	}
	if out := askOn(t, r, main, r.onMachine("future-two")); out.Token != "" || !strings.Contains(r.log(), "other caller's socket") {
		t.Errorf("the keys peer got a token on hostd's socket: %+v", out)
	}
}

func TestTwoSystemdSocketsAreTakenByNameNotByOrder(t *testing.T) {
	fd1, c1 := listeningFD(t)
	fd2, c2 := listeningFD(t)
	defer c1()
	defer c2()
	pid := os.Getpid()
	// every call takes (and closes) its own two consecutive descriptors, as a fresh process would be given them
	pair := func() int {
		first := dupFD(t, fd1)
		if dupFD(t, fd2) != first+1 {
			t.Skip("could not get two consecutive descriptors")
		}
		return first
	}
	names := []string{SocketNameHostd, SocketNameKeys}
	env := func(fds, fdnames string) func(string) string {
		return envOf(map[string]string{"LISTEN_PID": itoa(pid), "LISTEN_FDS": fds, "LISTEN_FDNAMES": fdnames})
	}
	ls, err := SystemdListeners(env("2", "main:launch"), pid, pair(), names)
	if err != nil || len(ls) != 2 {
		t.Fatalf("in order: %v %v", ls, err)
	}
	a1, a2 := ls[0].Addr().String(), ls[1].Addr().String()
	ls[0].Close()
	ls[1].Close()
	// the units started the other way round: the names still decide, so hostd's socket stays hostd's
	ls, err = SystemdListeners(env("2", "launch:main"), pid, pair(), names)
	if err != nil || ls[0].Addr().String() != a2 || ls[1].Addr().String() != a1 {
		t.Fatalf("swapped: %v %v", ls, err)
	}
	ls[0].Close()
	ls[1].Close()
	// anything that is not exactly the two names is refused
	for why, e := range map[string]func(string) string{
		"one socket where two are wanted": env("1", "main"),
		"three sockets":                   env("3", "main:launch:x"),
		"no names":                        env("2", ""),
		"systemd's default name":          env("2", "unknown:unknown"),
		"a name twice":                    env("2", "main:main"),
		"another name":                    env("2", "main:other"),
		"a count that is not a number":    env("many", "main:launch"),
	} {
		if _, err := SystemdListeners(e, pid, pair(), names); err == nil {
			t.Errorf("%s was accepted", why)
		}
	}
}

func TestTheKeysPeerNeedsASocketOfItsOwn(t *testing.T) {
	dir := keyDir(t)
	var out, errb bytes.Buffer
	Main([]string{"--keys", dir, "--init"}, &out, &errb)
	writeScope(t, dir)
	sd, _ := os.MkdirTemp("", "sg")
	defer os.RemoveAll(sd)
	for why, args := range map[string][]string{
		"a keys peer and no socket for it":     {"--keys", dir, "--allow-user", "2", "--socket", filepath.Join(sd, "a"), "--keys-peer-user", "1", "--keys-peer-machines", "future-two"},
		"a keys-peer socket with no keys peer": {"--keys", dir, "--allow-user", "2", "--socket", filepath.Join(sd, "b"), "--keys-peer-socket", filepath.Join(sd, "c")},
	} {
		var eb bytes.Buffer
		done := make(chan int, 1)
		go func() { done <- Main(args, &bytes.Buffer{}, &eb) }() // a signer that wrongly starts would serve for ever
		select {
		case c := <-done:
			if c != 2 {
				t.Errorf("%s: exit %d: %s", why, c, eb.String())
			}
		case <-time.After(3 * time.Second):
			t.Errorf("%s: the signer started serving", why)
		}
	}
}

// ---- revoke: the signer signs the doors' revoke{sid,gen} (F2x) ----

// a door's own handler accepts what the signer signed: the real verifier over the signer's public key
func doorFor(t *testing.T, r *rig) (*token.Verifier, http.Handler) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "ring")
	os.WriteFile(p, []byte(r.key.KeyringLineScoped([]string{"tomas"}, []string{"hp-ubuntu", "future-two"}, []string{"*"}, []string{"api", "stub"})+"\n"), 0o600)
	ring, err := token.LoadPublicKeyring(p)
	if err != nil {
		t.Fatal(err)
	}
	v := token.NewVerifier(ring, "ccsbx-model")
	return v, v.RevokeHandler(nil)
}

func (r *rig) askRevoke(t *testing.T, owner, machine, sid string, gen int) reply {
	t.Helper()
	b, _ := json.Marshal(map[string]any{"revoke": token.RevokeBody{Owner: owner, Machine: machine, Sid: sid, Gen: gen}})
	return r.ask(t, b)
}

func TestTheSignerSignsARevokeTheDoorAccepts(t *testing.T) {
	r := peers(t, true) // this process is hostd: it revokes for hp-ubuntu
	out := r.askRevoke(t, "tomas", "hp-ubuntu", "s0signxx1", 3)
	if out.Revoke == "" || out.Body == "" || out.Error != "" {
		t.Fatalf("%+v", out)
	}
	v, h := doorFor(t, r)
	rq := httptest.NewRequest("POST", token.RevokePath, strings.NewReader(out.Body))
	rq.Header.Set(token.RevokeHeader, out.Revoke)
	rw := httptest.NewRecorder()
	h.ServeHTTP(rw, rq)
	if rw.Code != 204 && rw.Code != 503 { // 503: no state file to persist to in this test; the signature was accepted
		t.Fatalf("the door answered %d", rw.Code)
	}
	if v.MaxGen("tomas", "hp-ubuntu", "s0signxx1") != 3 {
		t.Error("the door did not record the revoke")
	}
	if !strings.Contains(r.log(), `"op":"revoke"`) || strings.Contains(r.log(), out.Revoke) {
		t.Errorf("log: %s", r.log())
	}
}

func TestARevokeIsHeldToTheSameClassesAndScopeAsAToken(t *testing.T) {
	r := peers(t, true) // hostd
	for why, c := range map[string]struct {
		owner, machine, sid string
		gen                 int
	}{
		"the pod machine (closed to hostd)": {"tomas", "future-two", "s0signxx1", 1},
		"another owner":                     {"mallory", "hp-ubuntu", "s0signxx1", 1},
		"another machine":                   {"tomas", "mac", "s0signxx1", 1},
		"generation 0":                      {"tomas", "hp-ubuntu", "s0signxx1", 0},
		"a bad sid":                         {"tomas", "hp-ubuntu", "../x", 1},
	} {
		if out := r.askRevoke(t, c.owner, c.machine, c.sid, c.gen); out.Revoke != "" || out.Error == "" {
			t.Errorf("%s was signed: %+v", why, out)
		}
	}
	k := peers(t, false) // the keys peer
	if out := k.askRevoke(t, "tomas", "future-two", "s0signxx1", 2); out.Revoke == "" {
		t.Errorf("the pod launcher's own machine: %+v", out)
	}
	if out := k.askRevoke(t, "tomas", "hp-ubuntu", "s0signxx1", 2); out.Revoke != "" {
		t.Errorf("the pod launcher revoked for hostd's machine: %+v", out)
	}
	// anything but exactly the revoke object is refused: extra keys, a second key beside it, a claims body
	for why, body := range map[string]string{
		"an extra field in the revoke": `{"revoke":{"owner":"tomas","machine":"hp-ubuntu","sid":"s0signxx1","gen":1,"x":1}}`,
		"a second top-level key":       `{"revoke":{"owner":"tomas","machine":"hp-ubuntu","sid":"s0signxx1","gen":1},"sub":"a/b/c/g1"}`,
		"revoke null":                  `{"revoke":null}`,
		"trailing data":                `{"revoke":{"owner":"tomas","machine":"hp-ubuntu","sid":"s0signxx1","gen":1}} {}`,
	} {
		if out := r.ask(t, []byte(body)); out.Revoke != "" {
			t.Errorf("%s was signed", why)
		}
	}
}

func TestTheClientAsksForARevoke(t *testing.T) {
	r := peers(t, true)
	h, b, err := Client{Socket: r.sock}.SignRevoke(context.Background(), token.RevokeBody{Owner: "tomas", Machine: "hp-ubuntu", Sid: "s0signxx1", Gen: 2})
	if err != nil || h == "" || !strings.Contains(string(b), `"gen":2`) {
		t.Fatalf("%q %s %v", h, b, err)
	}
	if _, _, err := (Client{Socket: r.sock}).SignRevoke(context.Background(), token.RevokeBody{Owner: "mallory", Machine: "hp-ubuntu", Sid: "s0signxx1", Gen: 2}); err == nil {
		t.Error("a revoke outside the scope was signed")
	}
}
