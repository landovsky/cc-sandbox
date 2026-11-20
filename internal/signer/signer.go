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
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/token"
)

// Wire format. Request: one JSON object of claims (sub, label, billing, profile, aud, routes,
// repos, branch, placed_by, cnf, exp), then the client half-closes or just waits. Reply: one JSON
// object, {"token": "..."} or {"error": "..."}; then the connection is closed. iat is the signer's
// own clock, never the caller's.
type reply struct {
	Token string `json:"token,omitempty"`
	Error string `json:"error,omitempty"`
}

// Config is everything a Server needs; Main fills it from flags and the environment.
type Config struct {
	Key         *token.Key
	Scope       token.KeyScope
	Audiences   []string      // the only audiences a token may name
	MaxTTL      time.Duration // longest exp-iat the signer will sign (at most token.MaxLifetime)
	AllowGID    uint32        // peers must be this group, or a member of it
	MaxRequest  int           // bytes
	ReadTimeout time.Duration
	Now         func() time.Time
	Logf        func(format string, a ...any)
	// Member reports whether uid belongs to group gid. nil means look it up in the system's groups.
	Member func(uid, gid uint32) bool
}

// Server answers signing requests.
type Server struct{ cfg Config }

// New checks the configuration. A signer with no audience list, or a lifetime cap above the token
// maximum, would sign more than the plan allows, so it is refused at start.
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
		cfg.MaxRequest = 8192
	}
	if cfg.ReadTimeout <= 0 {
		cfg.ReadTimeout = 5 * time.Second
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Member == nil {
		cfg.Member = systemMember
	}
	return &Server{cfg: cfg}, nil
}

// systemMember looks uid up in the system's user and group files.
func systemMember(uid, gid uint32) bool {
	u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10))
	if err != nil {
		return false
	}
	gs, err := u.GroupIds()
	if err != nil {
		return false
	}
	want := strconv.FormatUint(uint64(gid), 10)
	for _, g := range gs {
		if g == want {
			return true
		}
	}
	return false
}

// Serve accepts connections until the listener closes. Each is handled on its own goroutine.
func (s *Server) Serve(l net.Listener) error {
	for {
		c, err := l.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	c.SetDeadline(s.cfg.Now().Add(s.cfg.ReadTimeout))
	uid, gid, err := peerCred(c)
	if err != nil || !(gid == s.cfg.AllowGID || s.cfg.Member(uid, s.cfg.AllowGID)) {
		s.cfg.Logf(`{"signer":"refused","why":"peer not in the allowed group","peer_uid":%d,"peer_gid":%d}`, uid, gid)
		write(c, reply{Error: "refused"}) // no detail to a peer that shouldn't be here
		return
	}
	// One byte over the cap is enough to know it is too big, without reading the rest.
	body, err := io.ReadAll(io.LimitReader(c, int64(s.cfg.MaxRequest)+1))
	if err != nil || len(body) > s.cfg.MaxRequest {
		s.cfg.Logf(`{"signer":"refused","why":"request too large or unreadable","peer_uid":%d}`, uid)
		write(c, reply{Error: "request too large"})
		return
	}
	tok, cl, err := s.Sign(body)
	if err != nil {
		s.cfg.Logf(`{"signer":"refused","why":%q,"peer_uid":%d,"sub":%q,"profile":%q,"billing":%q}`,
			err.Error(), uid, cl.Sub, cl.Profile, cl.Billing)
		write(c, reply{Error: err.Error()})
		return
	}
	// The log carries the claims and never the token or the key.
	s.cfg.Logf(`{"signer":"signed","peer_uid":%d,"sub":%q,"label":%q,"profile":%q,"billing":%q,"aud":%q,"exp":%d}`,
		uid, cl.Sub, cl.Label, cl.Profile, cl.Billing, strings.Join(cl.Aud, ","), cl.Exp)
	write(c, reply{Token: tok})
}

func write(c net.Conn, r reply) { json.NewEncoder(c).Encode(r) }

// Sign validates the claims in body and signs them. It is the whole policy: unknown fields, a sub
// or key scope that doesn't permit it, an audience off the list, or a lifetime over the cap are
// all refused before the key is touched. The returned claims are what was asked for (for the log).
func (s *Server) Sign(body []byte) (string, token.Claims, error) {
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
