// signer_test.go checks the signer's refusals and its one allowed path. Each block twin is meant to
// go red if its check is removed: unscoped claims, a lifetime over the cap, a peer outside the group,
// an oversized request, a key with the wrong mode, and a socket that systemd did not hand over. They
// guard against the signer ever signing more than its scope, or serving someone it shouldn't.
package signer

import (
	"fmt"
	"strconv"
	"syscall"
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
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
func newRig(t *testing.T, mod func(*Config)) *rig {
	t.Helper()
	r := &rig{dir: t.TempDir()}
	k, _, err := Init(r.dir)
	if err != nil {
		t.Fatal(err)
	}
	line := k.KeyringLineScoped([]string{"tomas"}, []string{"hp-ubuntu"}, []string{"dev-*"}, []string{"api", "stub"})
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
		AllowGID: uint32(os.Getgid()), MaxRequest: 1024, ReadTimeout: 2 * time.Second,
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
		"billing above the tier": func(c *token.Claims) { c.Billing = "subscription" },
		"profile outside":        func(c *token.Claims) { c.Profile = "prod-admin" },
		"other machine":          func(c *token.Claims) { c.Sub = "tomas/mac/sX/g1" },
		"other owner":            func(c *token.Claims) { c.Sub = "eve/hp-ubuntu/sX/g1" },
		"audience off the list":  func(c *token.Claims) { c.Aud = []string{"ccsbx-model", "ccsbx-admin"} },
		"caller sets iat":        func(c *token.Claims) { c.Iat = 1 },
	} {
		if out := r.ask(t, r.claims(mod)); out.Token != "" || out.Error == "" {
			t.Fatalf("%s: signed: %+v", name, out)
		}
	}
	if out := r.ask(t, []byte(`{"sub":"tomas/hp-ubuntu/sX/g1","billing":"api","profile":"dev-x","aud":["ccsbx-model"],"admin":true}`)); out.Token != "" {
		t.Fatal("an unknown field was accepted")
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

// context "only the ccsbx group may ask": a peer outside the allowed group gets no token. The kernel
// supplies the peer's uid and gid (SO_PEERCRED), so this runs over a real socket as this user.
func TestRefusesPeerOutsideTheGroup(t *testing.T) {
	r := newRig(t, nil) // allowed gid is this process's own gid
	if out := r.ask(t, r.claims(nil)); out.Token == "" {
		t.Fatalf("allow twin: %+v", out)
	}
	r2 := newRig(t, func(c *Config) { c.AllowGID = 59999 }) // a group nobody here is in
	if out := r2.ask(t, r2.claims(nil)); out.Token != "" {
		t.Fatal("a peer outside the group was served")
	}
	if !strings.Contains(r2.log(), "peer not in the allowed group") {
		t.Fatalf("refusal not logged: %s", r2.log())
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

// context "the key file is wrong": mode 0644 (anyone could have read it) is refused at load and by
// --init; a key made by --init is 0600 and a second --init changes nothing.
func TestKeyModeIsChecked(t *testing.T) {
	dir := t.TempDir()
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
	os.Chmod(filepath.Join(dir, KeyName), 0o644)
	if _, _, err := Init(dir); err == nil {
		t.Fatal("--init accepted a 0644 key")
	}
	os.WriteFile(filepath.Join(dir, ScopeName), []byte(k.KeyringLine([]string{"tomas"}, []string{"hp-ubuntu"})+"\n"), 0o600)
	if _, _, err := load(dir, ""); err == nil {
		t.Fatal("the signer loaded a 0644 key")
	}
	os.Chmod(filepath.Join(dir, KeyName), 0o600)
	if _, _, err := load(dir, ""); err != nil {
		t.Fatalf("allow twin: %v", err)
	}
}

// context "no scope, no signing": a missing scope file, a scope without tier limits, or a scope for
// another key stops the signer from starting.
func TestWontServeWithoutAScope(t *testing.T) {
	dir := t.TempDir()
	k, _, _ := Init(dir)
	if _, _, err := load(dir, ""); err == nil {
		t.Fatal("loaded with no scope file")
	}
	os.WriteFile(filepath.Join(dir, ScopeName), []byte(k.KID+" "+base64.StdEncoding.EncodeToString(k.Public)+" owners=tomas machines=hp-ubuntu\n"), 0o600)
	if _, _, err := load(dir, ""); err == nil {
		t.Fatal("loaded a scope with no profiles/billings")
	}
	other := t.TempDir()
	k3, _, _ := Init(other)
	os.WriteFile(filepath.Join(dir, ScopeName), []byte(k3.KeyringLine([]string{"tomas"}, []string{"hp-ubuntu"})+"\n"), 0o600)
	if _, _, err := load(dir, ""); err == nil {
		t.Fatal("loaded another key's scope")
	}
}

// context "systemd hands the socket over as fd 3": a fake LISTEN_FDS/LISTEN_PID with a real
// listening socket works; the wrong pid, a count other than 1 and an fd that isn't a socket don't.
func TestSystemdSocketActivation(t *testing.T) {
	sd, _ := os.MkdirTemp("", "sg")
	defer os.RemoveAll(sd)
	l0, err := net.Listen("unix", filepath.Join(sd, "s"))
	if err != nil {
		t.Fatal(err)
	}
	defer l0.Close()
	f, _ := l0.(*net.UnixListener).File()
	defer f.Close()
	fd := int(f.Fd())
	env := func(m map[string]string) func(string) string { return func(k string) string { return m[k] } }
	pid := os.Getpid()
	good := map[string]string{"LISTEN_PID": itoa(pid), "LISTEN_FDS": "1"}
	l, err := SystemdListener(env(good), pid, dupFD(t, fd))
	if err != nil {
		t.Fatalf("allow twin: %v", err)
	}
	l.Close()
	for name, m := range map[string]map[string]string{
		"wrong pid":   {"LISTEN_PID": itoa(pid + 1), "LISTEN_FDS": "1"},
		"two sockets": {"LISTEN_PID": itoa(pid), "LISTEN_FDS": "2"},
		"none":        {"LISTEN_PID": itoa(pid), "LISTEN_FDS": "0"},
		"unset":       {},
	} {
		if l, err := SystemdListener(env(m), pid, dupFD(t, fd)); err == nil {
			l.Close()
			t.Fatalf("%s: accepted", name)
		}
	}
	devnull, _ := os.Open(os.DevNull)
	defer devnull.Close()
	if l, err := SystemdListener(env(good), pid, int(devnull.Fd())); err == nil {
		l.Close()
		t.Fatal("an fd that is not a socket was accepted")
	}
}

// context "--help and the environment": every flag is listed with its env twin, --help starts
// nothing, and CCSBX_SIGNER_* reaches the same code path as a flag.
func TestHelpAndEnv(t *testing.T) {
	var out, errb bytes.Buffer
	if Main([]string{"--help"}, &out, &errb) != 0 {
		t.Fatal("help")
	}
	for _, fl := range []string{"--keys", "--systemd-socket", "--socket", "--init", "--scope", "--audiences", "--max-ttl",
		"--allow-group", "--max-request", "--timeout", "CCSBX_SIGNER_"} {
		if !strings.Contains(out.String(), fl) {
			t.Errorf("--help lacks %s", fl)
		}
	}
	dir := t.TempDir()
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
