// side_test.go checks the sidecar's first slice from the outside: real sockets, a real forwarder, a
// door that verifies what it receives. Each context names the leak or swap it exists to stop.
package side

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/token"
)

type rig struct {
	t       *testing.T
	srv     *Server
	port    string
	ctl     string
	cancel  context.CancelFunc
	done    chan error
	door    *httptest.Server
	seen    chan *http.Request
	key     *token.Key
	gk      *token.GenKey
	seed    []byte
	ring    token.Keyring
	verify  *token.Verifier
	nowSkew time.Time
}

func newRig(t *testing.T, mut func(*Config)) *rig {
	t.Helper()
	r := &rig{t: t, seen: make(chan *http.Request, 8)}
	k, err := token.LoadOrCreateKeyFile(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	r.key = k
	r.ring = k.Ring([]string{"tomas"}, []string{"hp-ubuntu"})
	r.verify = token.NewVerifier(r.ring, "ccsbx-model")
	r.door = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		r.seen <- q
		w.Write([]byte("door-ok"))
	}))
	t.Cleanup(r.door.Close)
	u, _ := url.Parse(r.door.URL)
	dir, err := os.MkdirTemp("", "side")
	if err != nil {
		t.Fatal(err)
	}
	os.Chmod(dir, 0o700)
	t.Cleanup(func() { os.RemoveAll(dir) })
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	r.port = l.Addr().String()
	l.Close()
	r.ctl = filepath.Join(dir, "ctl.sock")
	cfg := Config{Listen: r.port, Control: r.ctl, Doors: map[string]*url.URL{"model": u}}
	if mut != nil {
		mut(&cfg)
	}
	r.srv, err = New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	r.cancel, r.done = cancel, make(chan error, 1)
	go func() { r.done <- r.srv.Serve(ctx) }()
	t.Cleanup(cancel)
	for i := 0; i < 200; i++ { // wait until both listeners are up
		if c, err := net.Dial("unix", r.ctl); err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return r
}

// secrets mints a token bound to a fresh generation key (cnf), expiring in ttl.
func (r *rig) secrets(ttl time.Duration) (string, *bytes.Buffer) {
	return r.secretsAt(time.Now(), ttl)
}

func (r *rig) secretsAt(now time.Time, ttl time.Duration) (string, *bytes.Buffer) {
	r.t.Helper()
	seed := make([]byte, ed25519.SeedSize)
	for i := range seed {
		seed[i] = byte(i + 7)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	r.gk = &token.GenKey{Private: priv, Public: priv.Public().(ed25519.PublicKey)}
	tok, err := token.Mint(r.key, token.Claims{Sub: "tomas/hp-ubuntu/sSIDE1/g1", Label: "l", Billing: "api", Profile: "dev-default",
		Aud: []string{"ccsbx-model"}, Cnf: token.CnfOf(r.gk.Public), Exp: now.Add(ttl).Unix()}, now)
	if err != nil {
		r.t.Fatal(err)
	}
	b, _ := json.Marshal(secrets{Token: tok, Key: base64.StdEncoding.EncodeToString(seed)})
	return tok, bytes.NewBuffer(b)
}

func (r *rig) fence(path string) (int, string) {
	r.t.Helper()
	resp, err := http.Post("http://"+r.port+path, "application/json", strings.NewReader("{}"))
	if err != nil {
		r.t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// context "the launcher hands over the token by exec stdin": after the load the fence's calls reach the
// door with the token and a proof the door accepts, and before it they get a bare 503.
func TestLoadThenForward(t *testing.T) {
	r := newRig(t, nil)
	if code, body := r.fence("/model/v1/messages"); code != 503 || strings.Contains(body, "token") {
		t.Fatalf("before the load: %d %q, want a bare 503", code, body)
	}
	tok, in := r.secrets(time.Hour)
	if err := Load(r.ctl, in); err != nil {
		t.Fatalf("load: %v", err)
	}
	code, body := r.fence("/model/v1/messages")
	if code != 200 || body != "door-ok" {
		t.Fatalf("after the load: %d %q", code, body)
	}
	q := <-r.seen
	got := strings.TrimPrefix(q.Header.Get("Authorization"), "Bearer ")
	if got != tok || q.Header.Get(token.ProofHeader) == "" {
		t.Fatalf("the door did not get the token and a proof")
	}
	if _, err := r.verify.Verify(got, q.Header.Get(token.ProofHeader), q.Method, q.URL.RequestURI(), []byte("{}"), time.Now()); err != nil {
		t.Fatalf("the door would refuse what the sidecar sent: %v", err)
	}
}

// context "a second load would swap the session's identity under a running fence".
func TestOnlyOneLoadPerProcess(t *testing.T) {
	r := newRig(t, nil)
	_, in := r.secrets(time.Hour)
	if err := Load(r.ctl, in); err != nil {
		t.Fatal(err)
	}
	_, in2 := r.secrets(time.Hour)
	if err := Load(r.ctl, in2); err == nil || !strings.Contains(err.Error(), "already loaded") {
		t.Fatalf("second load: %v, want refused", err)
	}
}

// context "a launcher bug pairs the wrong key with a token": better to stop at the load than to fail
// every door call with a 401 nobody can explain. And an expired token is never served.
func TestLoadRefusesWhatCannotWork(t *testing.T) {
	for name, mut := range map[string]func(*secrets){
		"a key that is not the token's cnf": func(s *secrets) { s.Key = base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32)) },
		"a short key":                       func(s *secrets) { s.Key = "AAAA" },
		"not a token":                       func(s *secrets) { s.Token = "nope" },
	} {
		r := newRig(t, nil)
		_, in := r.secrets(time.Hour)
		var s secrets
		json.Unmarshal(in.Bytes(), &s)
		mut(&s)
		b, _ := json.Marshal(s)
		if err := Load(r.ctl, bytes.NewReader(b)); err == nil {
			t.Errorf("%s: accepted", name)
		}
		if r.srv.Loaded() {
			t.Errorf("%s: left the server loaded", name)
		}
	}
	r := newRig(t, nil)
	_, in := r.secretsAt(time.Now().Add(-2*time.Hour), time.Hour) // issued two hours ago, ran out an hour ago
	if err := Load(r.ctl, in); err == nil || !strings.Contains(err.Error(), "expired") {
		t.Fatalf("expired token: %v", err)
	}
}

// context "the fence shares the pod's network, and the control socket sits in a volume only the sidecar
// mounts": a peer with another uid is refused even if it could reach the socket.
func TestControlSocketRefusesAnotherUID(t *testing.T) {
	r := newRig(t, func(c *Config) { c.AllowUID = os.Getuid() + 1 })
	_, in := r.secrets(time.Hour)
	if err := Load(r.ctl, in); err == nil || !strings.Contains(err.Error(), "refused") {
		t.Fatalf("load from a uid that is not allowed: %v", err)
	}
	if r.srv.Loaded() {
		t.Fatal("the secrets were taken from the wrong uid")
	}
}

// context "the sidecar dies with the generation".
func TestEndsWhenTheTokenExpires(t *testing.T) {
	r := newRig(t, nil)
	_, in := r.secrets(1500 * time.Millisecond)
	if err := Load(r.ctl, in); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-r.done:
		if err != nil {
			t.Fatalf("ended with %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("still serving after the token expired")
	}
}

// context "only the forwarder port may be open to the fence": a wildcard or non-loopback address, a
// port 0, or a control socket outside an owned 0700 directory is refused at start.
func TestRefusesAnAddressThatWidensWhatTheFenceSees(t *testing.T) {
	u, _ := url.Parse("http://127.0.0.1:1")
	doors := map[string]*url.URL{"model": u}
	for _, listen := range []string{":8080", "0.0.0.0:8080", "10.0.0.5:8080", "127.0.0.1:0", "localhost:8080", "127.0.0.1"} {
		if _, err := New(Config{Listen: listen, Control: "/run/side/ctl.sock", Doors: doors}); err == nil {
			t.Errorf("--listen %q accepted", listen)
		}
	}
	if _, err := New(Config{Listen: "127.0.0.1:8080", Control: "ctl.sock", Doors: doors}); err == nil {
		t.Error("a relative control path accepted")
	}
	if _, err := New(Config{Listen: "127.0.0.1:8080", Control: "/run/side/ctl.sock", Doors: doors}); err != nil {
		t.Errorf("allow twin: %v", err)
	}
	dir := t.TempDir()
	os.Chmod(dir, 0o755)
	s, _ := New(Config{Listen: "127.0.0.1:8080", Control: filepath.Join(dir, "c.sock"), Doors: doors})
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // a missing guard would serve forever
	defer cancel()
	if err := s.Serve(ctx); err == nil {
		t.Error("a control directory open to others was accepted")
	}
}

// context "no token in the fence" (T:no-token-in-fence, this half): what the fence can read back from
// the forwarder never carries the token or the key, on success or when the door fails.
func TestTheFenceNeverSeesTheSecrets(t *testing.T) {
	r := newRig(t, nil)
	tok, in := r.secrets(time.Hour)
	var s secrets
	json.Unmarshal(in.Bytes(), &s)
	if err := Load(r.ctl, in); err != nil {
		t.Fatal(err)
	}
	r.door.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		// a careless door that echoes the credential into a response header (a body echo is not scanned)
		w.Header().Set("X-Echo", q.Header.Get("Authorization"))
		http.Error(w, "door failed", 500)
	})
	resp, err := http.Post("http://"+r.port+"/model/v1/messages", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	all := string(b)
	for k, v := range resp.Header {
		all += k + ": " + strings.Join(v, ",") + "\n"
	}
	for _, secret := range []string{tok, s.Key} {
		if strings.Contains(all, secret) {
			t.Fatalf("the fence got a secret back through the forwarder:\n%.300s", all)
		}
	}
}

func TestHelpAndUsage(t *testing.T) {
	var o, e bytes.Buffer
	if rc := Main([]string{"--help"}, nil, &o, &e); rc != 0 || !strings.Contains(o.String(), "Flags") {
		t.Fatalf("--help: rc=%d", rc)
	}
	if rc := Main([]string{"bogus"}, nil, &o, &e); rc != 64 {
		t.Fatalf("unknown subcommand: rc=%d", rc)
	}
	if rc := Main([]string{"load"}, strings.NewReader("{}"), &o, &e); rc != 2 {
		t.Fatalf("load without --control: rc=%d", rc)
	}
}

// context "the volume mount shows up owned by someone else": a control directory the sidecar's uid does
// not own could let another uid swap the socket, so the sidecar refuses to start there.
func TestControlDirectoryOfAnotherOwnerIsRefused(t *testing.T) {
	d := t.TempDir()
	if err := os.Chmod(d, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := checkControlDirAs(d, os.Getuid()); err != nil {
		t.Fatalf("own 0700 directory refused: %v", err)
	}
	if err := checkControlDirAs(d, os.Getuid()+1); err == nil || !strings.Contains(err.Error(), "not owned") {
		t.Fatalf("another uid's directory: %v, want refused", err)
	}
}
