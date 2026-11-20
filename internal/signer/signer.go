// signer.go is the signing service: one request per connection on a unix socket, claims checked
// against the key's scope, the signed token returned and nothing else. It exists so hostd can have
// tokens minted without ever holding the machine key (U0b): the only things that cross the socket
// are claims in and a token out, and every refusal happens before the key is used.
package signer

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/token"
)

// Wire format. Request: one JSON object of claims (sub, label, billing, profile, aud, routes,
// repos, branch, placed_by, cnf, exp). The signer reads exactly one JSON value (at most
// --max-request bytes), so a client need not close its write side and may simply wait for the reply.
// Reply: one JSON object, {"token": "..."} or {"error": "..."}; then the connection is closed. iat
// is the signer's own clock, never the caller's.
type reply struct {
	Token string `json:"token,omitempty"`
	Error string `json:"error,omitempty"`
}

// Config is everything a Server needs; Main fills it from flags and the environment.
type Config struct {
	Key       *token.Key
	Scope     token.KeyScope
	Audiences []string      // the only audiences a token may name
	MaxTTL    time.Duration // longest exp-iat the signer will sign (at most token.MaxLifetime)
	AllowUID  uint32        // the one caller: hostd's uid (exact, from SO_PEERCRED)
	AllowGID  *uint32       // when set, the caller's primary gid must be this too
	// KeysUID, when set, is a second allowed caller: the pod launcher, which runs as ccsbx-keys and
	// asks for the token of a pod session it will load into the pod's sidecar itself (so hostd never
	// holds that token or its generation key). It may sign ONLY for the machines in KeysMachines, and
	// only a token bound to a generation key (cnf). Those machines are closed to AllowUID: hostd gets
	// no token it could hold together with a key for a pod session.
	KeysUID      *uint32
	KeysMachines []string
	MaxRequest   int // bytes
	ReadTimeout  time.Duration
	MaxConns     int           // concurrent connections (default 64)
	AcceptPause  time.Duration // wait after a failed Accept (default 100ms; tests shorten it)
	Now          func() time.Time
	Logf         func(format string, a ...any)
}

// maxAcceptFailures consecutive Accept errors end Serve (about two seconds at the default pause).
const maxAcceptFailures = 20

func (s *Server) acceptPause() time.Duration {
	if s.cfg.AcceptPause > 0 {
		return s.cfg.AcceptPause
	}
	return 100 * time.Millisecond
}

// Server answers signing requests.
type Server struct {
	cfg Config
	sem chan struct{}
}

// New checks the configuration. A signer with no audience list, a lifetime cap above the token
// maximum, or a non-positive size or timeout would sign or wait more than the plan allows, so it is
// refused at start rather than quietly replaced by a default.
func New(cfg Config) (*Server, error) {
	if cfg.Key == nil {
		return nil, errors.New("signer: no key")
	}
	if len(cfg.Audiences) == 0 {
		return nil, errors.New("signer: --audiences is required")
	}
	if cfg.MaxTTL <= 0 || cfg.MaxTTL > token.MaxLifetime {
		return nil, fmt.Errorf("signer: --max-ttl must be in (0, %s]", token.MaxLifetime)
	}
	if cfg.MaxRequest <= 0 {
		return nil, errors.New("signer: --max-request must be positive")
	}
	if cfg.ReadTimeout <= 0 {
		return nil, errors.New("signer: --timeout must be positive")
	}
	if cfg.KeysUID != nil && (len(cfg.KeysMachines) == 0 || *cfg.KeysUID == cfg.AllowUID) {
		return nil, errors.New("signer: --keys-peer-user needs --keys-peer-machines and a uid other than --allow-user")
	}
	if cfg.KeysUID == nil && len(cfg.KeysMachines) > 0 {
		return nil, errors.New("signer: --keys-peer-machines without --keys-peer-user")
	}
	if cfg.MaxConns <= 0 {
		cfg.MaxConns = 64
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Server{cfg: cfg, sem: make(chan struct{}, cfg.MaxConns)}, nil
}

// logJSON writes one log line built with json.Marshal (valid JSON whatever the caller sent), with
// every string field cut to 200 bytes. It never takes a token or key.
func (s *Server) logJSON(kv ...any) {
	m := map[string]any{}
	for i := 0; i+1 < len(kv); i += 2 {
		v := kv[i+1]
		if str, ok := v.(string); ok && len(str) > 200 {
			v = str[:200] + "…"
		}
		m[kv[i].(string)] = v
	}
	b, _ := json.Marshal(m)
	s.cfg.Logf("%s", b)
}

// Serve accepts connections until the listener closes. Each is handled on its own goroutine, at most
// MaxConns at a time. A failing Accept (for example no free descriptors) is retried after a pause
// instead of ending the signer.
func (s *Server) Serve(l net.Listener) error {
	failures := 0
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			// A transient error (no free descriptors) clears; one that doesn't is permanent, and a
			// signer that can't accept must exit so systemd restarts it, not log ten lines a second.
			failures++
			s.logJSON("signer", "accept-error", "err", err.Error(), "consecutive", failures)
			if failures >= maxAcceptFailures {
				return fmt.Errorf("accept failed %d times in a row: %w", failures, err)
			}
			time.Sleep(s.acceptPause())
			continue
		}
		failures = 0
		s.sem <- struct{}{}
		go func() {
			defer func() { <-s.sem }()
			s.handle(c)
		}()
	}
}

// countingReader reports how many bytes were read, to tell "too large" from "malformed".
type countingReader struct {
	r io.Reader
	n int
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	c.n += n
	return n, err
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	// Deadlines use the real clock (cfg.Now is for claims only), and reading and writing have their own.
	c.SetReadDeadline(time.Now().Add(s.cfg.ReadTimeout))
	uid, gid, err := peerCred(c)
	if err != nil {
		s.logJSON("signer", "refused", "why", "peer credentials unavailable", "err", err.Error())
		s.send(c, reply{Error: "refused"})
		return
	}
	peer := PeerHostd
	if s.cfg.KeysUID != nil && uid == *s.cfg.KeysUID {
		peer = PeerKeys
	} else if uid != s.cfg.AllowUID || (s.cfg.AllowGID != nil && gid != *s.cfg.AllowGID) {
		s.logJSON("signer", "refused", "why", "peer is not the allowed caller", "peer_uid", uid, "peer_gid", gid)
		s.send(c, reply{Error: "refused"}) // no detail to a peer that shouldn't be here
		return
	}
	// Read exactly one JSON value, at most MaxRequest bytes (one more is read to notice an excess).
	cr := &countingReader{r: io.LimitReader(c, int64(s.cfg.MaxRequest)+1)}
	var raw json.RawMessage
	dec := json.NewDecoder(cr)
	err = dec.Decode(&raw)
	if cr.n > s.cfg.MaxRequest {
		s.logJSON("signer", "refused", "why", "request too large", "peer_uid", uid)
		s.send(c, reply{Error: "request too large"})
		return
	}
	if err != nil {
		why, msg := "malformed request", "bad request"
		var ne net.Error
		if errors.As(err, &ne) && ne.Timeout() {
			why, msg = "timeout waiting for the request", "timeout"
		}
		s.logJSON("signer", "refused", "why", why, "peer_uid", uid)
		s.send(c, reply{Error: msg})
		return
	}
	if s.trailingData(c, dec) {
		s.logJSON("signer", "refused", "why", "data after the request", "peer_uid", uid)
		s.send(c, reply{Error: "bad request"})
		return
	}
	tok, cl, err := s.SignFor(peer, raw)
	if err != nil {
		s.logJSON("signer", "refused", "why", err.Error(), "peer_uid", uid, "sub", cl.Sub, "profile", cl.Profile, "billing", cl.Billing)
		s.send(c, reply{Error: err.Error()})
		return
	}
	// The log carries the claims and never the token or the key.
	s.logJSON("signer", "signed", "peer_uid", uid, "sub", cl.Sub, "label", cl.Label, "profile", cl.Profile,
		"billing", cl.Billing, "aud", strings.Join(cl.Aud, ","), "exp", cl.Exp)
	s.send(c, reply{Token: tok})
}

// trailingData reports whether anything but white space follows the first JSON value: what the
// decoder already buffered, or what the peer has sent within the next few milliseconds. A client that
// sends one value and waits (or half-closes) sees no delay beyond that short look.
func (s *Server) trailingData(c net.Conn, dec *json.Decoder) bool {
	rest, _ := io.ReadAll(dec.Buffered())
	if len(strings.TrimSpace(string(rest))) > 0 {
		return true
	}
	c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
	buf := make([]byte, 64)
	n, _ := c.Read(buf)
	return len(strings.TrimSpace(string(buf[:n]))) > 0
}

func (s *Server) send(c net.Conn, r reply) {
	c.SetWriteDeadline(time.Now().Add(s.cfg.ReadTimeout))
	json.NewEncoder(c).Encode(r)
}

// Peer is which allowed caller is asking.
type Peer int

const (
	PeerHostd Peer = iota // AllowUID: hostd
	PeerKeys              // KeysUID: the pod launcher (ccsbx-keys)
)

// checkPeer applies the per-caller rule on top of the key's scope: the keys peer signs for its own
// machines only and only with a generation key bound (cnf); hostd signs nothing for those machines.
func (s *Server) checkPeer(p Peer, c *token.Claims) error {
	if s.cfg.KeysUID == nil {
		return nil
	}
	_, machine, _, _, err := c.SubParts()
	if err != nil {
		return err
	}
	for_keys := contains(s.cfg.KeysMachines, machine)
	switch {
	case p == PeerKeys && !for_keys:
		return fmt.Errorf("the pod launcher may sign only for %s", strings.Join(s.cfg.KeysMachines, ","))
	case p == PeerKeys && c.Cnf == "":
		return errors.New("the pod launcher signs only tokens bound to a generation key (cnf)")
	case p == PeerHostd && for_keys:
		return fmt.Errorf("machine %s is placed through the pod launcher, not hostd", machine)
	}
	return nil
}

// Sign signs as hostd (the in-process entry); SignFor names the caller.
func (s *Server) Sign(body []byte) (string, token.Claims, error) { return s.SignFor(PeerHostd, body) }

// SignFor validates the claims in body and signs them. It is the whole policy: unknown fields, a sub
// or key scope that doesn't permit it, an audience off the list, or a lifetime over the cap are
// all refused before the key is touched. The returned claims are what was asked for (for the log).
func (s *Server) SignFor(peer Peer, body []byte) (string, token.Claims, error) {
	var c token.Claims
	dec := json.NewDecoder(strings.NewReader(string(body)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&c); err != nil {
		return "", c, errors.New("bad request")
	}
	if dec.More() {
		return "", c, errors.New("bad request")
	}
	if c.Iat != 0 {
		return "", c, errors.New("iat is the signer's, not the caller's")
	}
	now := s.cfg.Now()
	if err := s.checkPeer(peer, &c); err != nil {
		return "", c, err
	}
	if err := s.cfg.Scope.Permits(&c); err != nil {
		return "", c, err
	}
	for _, a := range c.Aud {
		if !contains(s.cfg.Audiences, a) {
			return "", c, fmt.Errorf("audience %q is not one this signer signs", a)
		}
	}
	if c.Exp == 0 {
		c.Exp = now.Add(s.cfg.MaxTTL).Unix()
	}
	if c.Exp-now.Unix() > int64(s.cfg.MaxTTL/time.Second) {
		return "", c, fmt.Errorf("lifetime over the cap of %s", s.cfg.MaxTTL)
	}
	tok, err := token.Mint(s.cfg.Key, c, now)
	if err != nil {
		return "", c, err
	}
	return tok, c, nil
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
