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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/landovsky/cc-sandbox/internal/fwd"
	"github.com/landovsky/cc-sandbox/internal/journal"
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
	// Marker is the no-secret file that records the one load in the side-state volume, which outlives a
	// container restart (the process does not). Default: <control dir>/loaded.
	Marker string
	// ControlTimeout bounds one control connection (default 5s).
	ControlTimeout time.Duration
	// Journal, when set, gets the forwarder's lifecycle facts (fwd.loaded, fwd.expired, fwd.denied
	// summaries), which the cockpit's idle and hear state is computed from. Sid and Gen are then required:
	// identity comes from here, never from the token's say-so or the fence.
	Journal Appender
	Sid     string
	Gen     int
	// MaxFwdLogs caps fwd.denied summaries per lifetime (default 50); DenySummaryEvery is how often a
	// changed denial count is written (default 30s).
	MaxFwdLogs       int
	DenySummaryEvery time.Duration
	Logf             func(format string, a ...any)
	Now              func() time.Time
	Resolve          func(host string) ([]net.IP, error) // tests
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
	spent   atomic.Bool // the marker file exists: an earlier process of this container took the load
	handler atomic.Pointer[http.Handler]
	expiry  atomic.Int64 // unix seconds; 0 until loaded
	loadMu  sync.Mutex
	loadCh  chan struct{} // closed once the secrets are in
	failed  chan error    // the forwarder died
	fwdp    atomic.Pointer[fwd.Forwarder]
	failLog appendFailures
	logs    int // fwd.denied summaries written (Serve's goroutine only)
	lastDen int64
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
	if cfg.Marker == "" {
		cfg.Marker = filepath.Join(filepath.Dir(cfg.Control), "loaded")
	}
	if cfg.Journal != nil && (cfg.Sid == "" || cfg.Gen < 1) {
		return nil, errors.New("side: a journal needs --sid and --gen (identity comes from the config)")
	}
	if cfg.MaxFwdLogs == 0 {
		cfg.MaxFwdLogs = 50
	}
	if cfg.DenySummaryEvery == 0 {
		cfg.DenySummaryEvery = 30 * time.Second
	}
	if cfg.ControlTimeout == 0 {
		cfg.ControlTimeout = 5 * time.Second
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
	return &Server{cfg: cfg, loadCh: make(chan struct{}), failed: make(chan error, 1)}, nil
}

// notLoaded answers while there are no secrets. Same body for every path: the fence learns only that
// the forwarder is not ready.
var notLoaded http.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
	http.Error(w, "forwarder not ready", http.StatusServiceUnavailable)
})

// Serve opens the control socket and the forwarder port and runs until ctx ends or the token expires.
func (s *Server) Serve(ctx context.Context) error {
	// Kubernetes makes an emptyDir root 0777 and root-owned, so the socket sits one level down, in a
	// directory made here as 0700 (it survives a container restart: EEXIST is fine). The check stays.
	dir := filepath.Dir(s.cfg.Control)
	if err := os.Mkdir(dir, 0o700); err == nil {
		if err := os.Chmod(dir, 0o700); err != nil { // not subject to the umask
			return err
		}
	} else if !errors.Is(err, os.ErrExist) {
		return fmt.Errorf("side: control directory: %w", err)
	}
	if err := checkControlDir(dir); err != nil {
		return err
	}
	if _, err := os.Lstat(s.cfg.Marker); err == nil {
		s.spent.Store(true)
		s.cfg.Logf("side: a token was loaded by an earlier run of this sidecar; refusing every load (the launcher must replace the pod)")
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
	}), ReadHeaderTimeout: 10 * time.Second, IdleTimeout: 2 * time.Minute, MaxHeaderBytes: 64 << 10}
	go func() {
		if err := srv.Serve(fl); err != nil && !errors.Is(err, http.ErrServerClosed) {
			s.cfg.Logf("side: forwarder stopped: %v", err)
			s.fatal(err)
		}
	}()
	go s.control(cl)
	s.cfg.Logf("side: forwarder on %s, waiting for secrets on %s", s.cfg.Listen, s.cfg.Control)

	var tick <-chan time.Time
	if s.cfg.Journal != nil {
		t := time.NewTicker(s.cfg.DenySummaryEvery)
		defer t.Stop()
		tick = t.C
	}
	var expired <-chan time.Time
	loadCh := s.loadCh // s.loadCh is closed by load; this copy is set to nil after it fires
	for {
		select {
		case <-tick:
			s.denySummary(false)
		case <-ctx.Done():
			s.denySummary(true)
			srv.Close()
			return nil
		case err := <-s.failed:
			s.denySummary(true)
			return fmt.Errorf("side: forwarder: %w", err)
		case <-loadCh:
			loadCh = nil // a nil channel blocks: this case fires once
			expired = time.After(time.Until(time.Unix(s.expiry.Load(), 0)))
		case <-expired:
			s.cfg.Logf("side: the token has expired; ending with the generation")
			s.denySummary(true)
			s.note("fwd.expired", map[string]any{"exp": s.expiry.Load()})
			srv.Close()
			return nil
		}
	}
}

// note journals one lifecycle fact; a failing journal is logged and ignored (it observes, it does not gate).
func (s *Server) note(typ string, data map[string]any) {
	if s.cfg.Journal == nil {
		return
	}
	b, _ := json.Marshal(data)
	if _, err := s.cfg.Journal.Append(journal.Event{Sid: s.cfg.Sid, Gen: s.cfg.Gen, Src: "host", Type: typ, Data: b}); err != nil {
		s.failLog.log(s.cfg.Logf, "side", err)
	}
}

// denySummary writes the cumulative refusal counts by fixed reason when they changed, at most MaxFwdLogs
// times per lifetime; the periodic ones stop one short so the final one at the end still fits.
func (s *Server) denySummary(final bool) {
	f := s.fwdp.Load()
	if f == nil || s.cfg.Journal == nil {
		return
	}
	limit := s.cfg.MaxFwdLogs
	if !final {
		limit--
	}
	_, denied := f.Stats()
	if denied == s.lastDen || s.logs >= limit {
		return
	}
	s.lastDen = denied
	s.logs++
	s.note("fwd.denied", map[string]any{"total": denied, "classes": f.DenyClasses()})
}

func (s *Server) fatal(err error) {
	select {
	case s.failed <- err:
	default:
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
	c.SetDeadline(time.Now().Add(s.cfg.ControlTimeout))
	uid, err := peerUID(c)
	if err != nil || int(uid) != s.cfg.AllowUID {
		s.cfg.Logf("side: control: refused a peer (uid %d)", uid) // the uid is not a secret
		send(reply{Error: "refused"})
		return
	}
	var sec secrets
	dec := json.NewDecoder(io.LimitReader(c, MaxSecrets))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&sec); err != nil || dec.More() {
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
	if s.spent.Load() {
		return errors.New("already loaded by an earlier run of this sidecar")
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
	// The marker is created exclusively and holds no secret. It is what makes "one load" last across a
	// restart of this container (a native sidecar restarts whatever its exit code).
	mf, err := os.OpenFile(s.cfg.Marker, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("already loaded by an earlier run of this sidecar")
	}
	fmt.Fprintf(mf, "{\"cnf\":%q,\"exp\":%d}\n", c.Cnf, c.Exp)
	mf.Close()
	var h http.Handler = f
	s.fwdp.Store(f)
	s.handler.Store(&h)
	s.expiry.Store(c.Exp)
	s.loaded.Store(true)
	sum := sha256.Sum256([]byte(c.Cnf))
	s.note("fwd.loaded", map[string]any{"cnf": hex.EncodeToString(sum[:8]), "exp": c.Exp}) // a hash, never the key or token
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
		return fmt.Errorf("side load: %w: %w", ErrNoSocket, err)
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(10 * time.Second))
	// A server that refuses the peer answers and closes at once, so the write can fail with a broken pipe
	// while the answer is already waiting: the answer is read either way.
	_, werr := c.Write(b)
	if uc, ok := c.(*net.UnixConn); ok && werr == nil {
		uc.CloseWrite() // the server reads exactly one JSON object and then wants to see the end
	}
	var r reply
	if err := json.NewDecoder(c).Decode(&r); err != nil {
		if werr != nil {
			return werr
		}
		return fmt.Errorf("side load: no answer: %w", err)
	}
	if !r.OK {
		if strings.HasPrefix(r.Error, "already loaded") {
			return fmt.Errorf("side load: refused: %s: %w", r.Error, ErrAlreadyLoaded)
		}
		return fmt.Errorf("side load: refused: %s", r.Error)
	}
	return nil
}

// Exit codes of `ccsbx side load`, so the launcher can tell what happened without reading text: 1 is a
// refusal (a bad payload, an expired token), 3 is that nothing was listening (the sidecar is not up yet:
// safe to try again), 4 is that this sidecar already holds a load (the launcher's earlier try got through,
// or somebody else's did). kubectl's own failures also exit 1, so 3 and 4 only ever come from here.
const (
	ExitNoSocket      = 3
	ExitAlreadyLoaded = 4
)

// ErrNoSocket and ErrAlreadyLoaded are what Load returns for those two cases.
var (
	ErrNoSocket      = errors.New("no control socket")
	ErrAlreadyLoaded = errors.New("already loaded")
	// ErrRefused is what LoadInto returns when the sidecar's command exited 1: a refusal of the payload.
	ErrRefused = errors.New("refused")
)

// appendFailures rate-limits the log line for a failing journal: once the store is broken (journal.ErrBroken)
// every append fails, and a line per decision would flood the sidecar's log. The first failure is logged,
// then every 1000th with the count.
type appendFailures struct{ n atomic.Int64 }

func (a *appendFailures) log(logf func(string, ...any), who string, err error) {
	n := a.n.Add(1)
	if n == 1 || n%1000 == 0 {
		logf("%s: journal append failed (%v); %d failures so far", who, err, n)
	}
}
