// Package side is `ccsbx side` (plan F3): the sidecar of a fenced pod, the part that sits OUTSIDE the
// fence's uid. It exists so the session token and the generation key live in a process the fence can't
// read, and so the fence's only way out is one local forwarder port.
//
// This file is the first slice (F3a): the control socket that receives the secrets, and the forwarder
// that uses them.
//   - The secrets never touch the pod spec, an argument or the environment. The launcher runs
//     `ccsbx side load` inside the sidecar container (pods/exec) with the secrets on stdin; that client
//     hands them to the running sidecar over a unix socket in a volume only the sidecar mounts.
//   - The control socket takes ONE load per process and only from the sidecar's own uid (SO_PEERCRED):
//     a second load, or one from the fence's uid, is refused, so a token can't be swapped.
//   - The forwarder serves one loopback host:port (the pod's forwarder port). Until the secrets are
//     loaded it answers 503; after the token expires the process ends (dies with the generation).
//   - Before serving, the key is checked against the token's cnf claim, so a launcher bug that pairs
//     the wrong key with a token stops here, not at the first door call.
//
// Not in this slice (F3b/F3c): the outbox relay with U3a's file checks, the session journal, the bus
// relay with the B6c credential, and clone/commit/push with H1.
package side

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	"github.com/landovsky/cc-sandbox/internal/fwd"
	"github.com/landovsky/cc-sandbox/internal/token"
)

// MaxSecrets caps what the control socket reads: a token, a seed and a few hundred bytes of JSON.
const MaxSecrets = 64 << 10

// Config is everything the sidecar needs before it has secrets.
type Config struct {
	Listen   string              // loopback host:port of the forwarder (the pod's one declared port)
	Control  string              // unix socket path, inside a directory only the sidecar can reach
	Doors    map[string]*url.URL // doors this session may reach
	Rules    []fwd.Rule          // nil: the built-in allowlist for those doors
	CAFile   string
	MaxBody  int64
	AllowUID int // who may load secrets; default: the sidecar's own uid
	Logf     func(format string, a ...any)
	Now      func() time.Time
	Resolve  func(host string) ([]net.IP, error) // tests
}

// secrets is what `side load` sends (the same shape `ccsbx fwd` reads on stdin).
type secrets struct {
	Token string `json:"token"`
	Key   string `json:"key"` // base64 of the 32-byte generation seed
}

type reply struct {
	OK    bool   `json:"ok,omitempty"`
	Error string `json:"error,omitempty"`
}

// Server is the sidecar's listeners and state.
type Server struct {
	cfg     Config
	loaded  atomic.Bool
	handler atomic.Pointer[http.Handler]
	expiry  atomic.Int64 // unix seconds; 0 until loaded
	loadMu  sync.Mutex
	loadCh  chan struct{} // closed once the secrets are in
}

// New checks the configuration. It refuses a forwarder address that anything but the pod could reach.
func New(cfg Config) (*Server, error) {
	host, port, err := net.SplitHostPort(cfg.Listen)
	ip := net.ParseIP(host)
	if err != nil || port == "" || port == "0" || ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("side: --listen must be a loopback IP:port (the pod's forwarder port), got %q", cfg.Listen)
	}
	if cfg.Control == "" || !filepath.IsAbs(cfg.Control) {
		return nil, errors.New("side: --control must be an absolute socket path in a sidecar-only volume")
	}
	if len(cfg.Doors) == 0 {
		return nil, errors.New("side: at least one --door is required")
	}
	if cfg.AllowUID == 0 {
		cfg.AllowUID = os.Getuid()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Server{cfg: cfg, loadCh: make(chan struct{})}, nil
}

// notLoaded answers while there are no secrets. Same body for every path: the fence learns only that
// the forwarder is not ready.
var notLoaded http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "forwarder not ready", http.StatusServiceUnavailable)
})

// Serve opens the control socket and the forwarder port and runs until ctx ends or the token expires.
func (s *Server) Serve(ctx context.Context) error {
	if err := checkControlDir(filepath.Dir(s.cfg.Control)); err != nil {
		return err
	}
	os.Remove(s.cfg.Control) // a stale socket from a previous run of this container
	cl, err := net.Listen("unix", s.cfg.Control)
	if err != nil {
		return fmt.Errorf("side: control socket: %w", err)
	}
	defer cl.Close()
	if err := os.Chmod(s.cfg.Control, 0o600); err != nil {
		return err
	}
	fl, err := net.Listen("tcp", s.cfg.Listen)
	if err != nil {
		return fmt.Errorf("side: forwarder port: %w", err)
	}
	defer fl.Close()
	s.handler.Store(&notLoaded)
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		(*s.handler.Load()).ServeHTTP(w, r)
	}), ReadHeaderTimeout: 10 * time.Second}
	go srv.Serve(fl)
	go s.control(cl)
	s.cfg.Logf("side: forwarder on %s, waiting for secrets on %s", s.cfg.Listen, s.cfg.Control)

	var expired <-chan time.Time
	loadCh := s.loadCh // s.loadCh is closed by load; this copy is set to nil after it fires
	for {
		select {
		case <-ctx.Done():
			srv.Close()
			return nil
		case <-loadCh:
			loadCh = nil // a nil channel blocks: this case fires once
			expired = time.After(time.Until(time.Unix(s.expiry.Load(), 0)))
		case <-expired:
			s.cfg.Logf("side: the token has expired; ending with the generation")
			srv.Close()
			return nil
		}
	}
}

// Loaded reports whether the secrets are in (tests and health).
func (s *Server) Loaded() bool { return s.loaded.Load() }

// checkControlDir refuses a directory the fence (or anyone else) could write a socket into or swap.
func checkControlDir(dir string) error { return checkControlDirAs(dir, os.Getuid()) }

// checkControlDirAs is checkControlDir for a given uid (a test passes another one).
func checkControlDirAs(dir string, uid int) error {
	fi, err := os.Lstat(dir)
	if err != nil {
		return fmt.Errorf("side: control directory: %w", err)
	}
	if !fi.IsDir() || fi.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("side: control directory %s is not a plain directory", dir)
	}
	if fi.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("side: control directory %s has mode %o; it must be 0700", dir, fi.Mode().Perm())
	}
	if !ownedBy(fi, uid) {
		return fmt.Errorf("side: control directory %s is not owned by uid %d", dir, uid)
	}
	return nil
}

func (s *Server) control(l net.Listener) {
	for {
		c, err := l.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	send := func(r reply) { json.NewEncoder(c).Encode(r) }
	c.SetDeadline(time.Now().Add(5 * time.Second))
	uid, err := peerUID(c)
	if err != nil || int(uid) != s.cfg.AllowUID {
		s.cfg.Logf("side: control: refused a peer (uid %d)", uid) // the uid is not a secret
		send(reply{Error: "refused"})
		return
	}
	var sec secrets
	dec := json.NewDecoder(io.LimitReader(c, MaxSecrets))
	if err := dec.Decode(&sec); err != nil {
		send(reply{Error: "bad request"})
		return
	}
	if err := s.load(sec); err != nil {
		s.cfg.Logf("side: control: load refused: %v", err)
		send(reply{Error: err.Error()})
		return
	}
	send(reply{OK: true})
}

// load installs the secrets once.
func (s *Server) load(sec secrets) error {
	s.loadMu.Lock()
	defer s.loadMu.Unlock()
	if s.loaded.Load() {
		return errors.New("already loaded")
	}
	seed, err := base64.StdEncoding.DecodeString(sec.Key)
	if err != nil || len(seed) != ed25519.SeedSize {
		return errors.New("key must be a base64 32-byte seed")
	}
	priv := ed25519.NewKeyFromSeed(seed)
	gk := &token.GenKey{Private: priv, Public: priv.Public().(ed25519.PublicKey)}
	c, err := token.UnverifiedClaims(sec.Token)
	if err != nil {
		return errors.New("token is not a session token")
	}
	if c.Cnf != token.CnfOf(gk.Public) {
		return errors.New("the key does not match the token's cnf")
	}
	if c.Exp <= s.cfg.Now().Unix() {
		return errors.New("the token has expired")
	}
	f, err := fwd.New(fwd.Config{Token: sec.Token, Key: gk, Doors: s.cfg.Doors, Rules: s.cfg.Rules,
		CAFile: s.cfg.CAFile, MaxBody: s.cfg.MaxBody, Logf: s.cfg.Logf, Now: s.cfg.Now, Resolve: s.cfg.Resolve})
	if err != nil {
		return err
	}
	var h http.Handler = f
	s.handler.Store(&h)
	s.expiry.Store(c.Exp)
	s.loaded.Store(true)
	close(s.loadCh)
	return nil
}

// Load is the `side load` client: it sends the secrets read from in to the control socket.
func Load(control string, in io.Reader) error {
	b, err := io.ReadAll(io.LimitReader(in, MaxSecrets+1))
	if err != nil {
		return err
	}
	if len(b) > MaxSecrets {
		return errors.New("side load: secrets are too large")
	}
	c, err := net.DialTimeout("unix", control, 5*time.Second)
	if err != nil {
		return fmt.Errorf("side load: %w", err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	if _, err := c.Write(b); err != nil {
		return err
	}
	var r reply
	if err := json.NewDecoder(c).Decode(&r); err != nil {
		return fmt.Errorf("side load: no answer: %w", err)
	}
	if !r.OK {
		return fmt.Errorf("side load: refused: %s", r.Error)
	}
	return nil
}
