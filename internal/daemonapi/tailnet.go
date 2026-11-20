// tailnet.go is the daemon API's listener on the tailnet (M2a, plan §4.4): the same API as the local
// socket, but the caller is whoever Tailscale says is on the other end of the connection. It exists so
// that a request's authority comes from the tailnet's own record of the peer (login, device, tags) and
// from nothing the request carries: no header, no body field, no X-Forwarded-For. A caller is
//
//	owner  the owner's login on a LISTED device (not a tagged node)
//	peer   a listed daemon: a listed device that is also tagged with a daemon tag or carries the owner's
//	       login, or a node carrying a daemon tag (never tag:ccsbx: see ClassConfig.Validate)
//	none   anything else, and every failure to find out: 403
//
// What each class may call is an explicit (route, method) table (tailnetRoutes): a route added to the API
// is closed on the tailnet until someone classifies it (a test walks the mux). An owner gets info, resolve, the
// session list and (M2c) the remote start; deliver, prompt, replace, steal and the journal stay on the local
// socket (see tailnetRoutes). A peer gets only what another daemon needs (info, resolve, the session list). Nothing is served
// unless hostd was told to listen (--tailnet-listen), and only on a tailnet address.
//
// Before WhoIs is even asked, a request must look like a daemon's, not a browser's or a local process's:
// it comes from a tailnet address that is not one of THIS machine's (a local process connecting to the
// machine's own tailnet address would otherwise be "the machine", i.e. the owner), it names this daemon in
// Host (DNS rebinding), it carries no Origin and no cross-site Sec-Fetch-Site (a web page in the owner's
// browser), and a POST says it is JSON (a form or text/plain POST cannot be crafted cross-origin).
package daemonapi

import (
	"context"
	"fmt"
	"mime"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/landovsky/cc-sandbox/internal/whois"
)

// Class is a caller's standing on the tailnet listener.
type Class int

const (
	ClassNone Class = iota
	ClassPeer
	ClassOwner
)

func (c Class) String() string { return [...]string{"none", "peer", "owner"}[c] }

// ClassConfig lists who counts as what. Devices are full MagicDNS names (host.tailnet.ts.net), matched
// case-insensitively: a short name would match a node of any tailnet that is shared in, or a renamed one.
type ClassConfig struct {
	OwnerLogin   string
	OwnerDevices []string
	PeerDevices  []string // also need a daemon tag or the owner's login (a listed name alone proves little)
	PeerTags     []string // daemon tags, e.g. tag:ccsbx-hostd: never tag:ccsbx (Validate)
	Hosts        []string // the Host header values this daemon answers to (listen IP, MagicDNS name), with or without the port
}

// forbiddenPeerTags are tags that must never make a caller a peer: tag:ccsbx is what the cluster's operator
// proxies (and so anything a fenced pod can reach through the egress Service) carry.
var forbiddenPeerTags = []string{"tag:ccsbx"}

// Validate refuses a configuration that would widen the classes by accident.
func (c ClassConfig) Validate() error {
	for _, d := range append(append([]string{}, c.OwnerDevices...), c.PeerDevices...) {
		if !strings.Contains(strings.TrimSuffix(d, "."), ".") {
			return fmt.Errorf("device %q: give the full MagicDNS name (host.<tailnet>.ts.net), not a short name", d)
		}
	}
	for _, t := range c.PeerTags {
		for _, f := range forbiddenPeerTags {
			if strings.EqualFold(t, f) {
				return fmt.Errorf("peer tag %q: it is carried by the cluster's operator proxies, which a fenced pod can reach; use a tag only hostd's devices carry (e.g. tag:ccsbx-hostd)", t)
			}
		}
		if !strings.HasPrefix(t, "tag:") {
			return fmt.Errorf("peer tag %q: want tag:<name>", t)
		}
	}
	if c.OwnerLogin != "" && len(c.OwnerDevices) == 0 {
		return fmt.Errorf("--owner-login needs --owner-devices: with no listed device nobody is the owner")
	}
	if c.OwnerLogin == "" && len(c.OwnerDevices) > 0 {
		return fmt.Errorf("--owner-devices needs --owner-login")
	}
	if c.OwnerLogin == "" && len(c.PeerDevices) == 0 && len(c.PeerTags) == 0 {
		return fmt.Errorf("--tailnet-listen needs --owner-login with --owner-devices, and/or --peer-tags/--peer-devices: with none, every caller would be refused")
	}
	return nil
}

func inList(l []string, v string) bool {
	for _, x := range l {
		if v != "" && strings.EqualFold(strings.TrimSuffix(x, "."), v) {
			return true
		}
	}
	return false
}

// Classify applies the lists to what Tailscale said.
func (c ClassConfig) Classify(id whois.Identity) Class {
	if id.Router {
		return ClassNone // what is behind a subnet router shares its address: never its class
	}
	if c.OwnerLogin != "" && len(id.Tags) == 0 && strings.EqualFold(id.Login, c.OwnerLogin) && inList(c.OwnerDevices, id.Device) {
		return ClassOwner
	}
	tagged := false
	for _, t := range id.Tags {
		for _, pt := range c.PeerTags {
			if strings.EqualFold(t, pt) {
				tagged = true
			}
		}
	}
	if tagged {
		return ClassPeer
	}
	if inList(c.PeerDevices, id.Device) && c.OwnerLogin != "" && len(id.Tags) == 0 && strings.EqualFold(id.Login, c.OwnerLogin) {
		return ClassPeer
	}
	return ClassNone
}

type classKey struct{}

func classOf(r *http.Request) Class { c, _ := r.Context().Value(classKey{}).(Class); return c }

// routePolicy: the methods each class may use on one mux pattern ("*" = any). Absent or empty = none.
type routePolicy struct{ Owner, Peer []string }

// tailnetRoutes classifies EVERY route of the API for the tailnet (patterns as registered in routes()).
// TestEveryRouteIsClassifiedForTheTailnet walks the mux against it: a new route fails the build until it
// has an entry here, and an entry with empty lists means "closed on the tailnet".
//
// DECISION (manager default, until Tomas answers ❓ cc-sandbox:#2dsc, SEC-20: owner authority on a listed device
// is ambient, with no secret): over the tailnet the owner gets only info, resolve, the session list and (M2c)
// session start. deliver, prompt, replace, the cockpit lease (steal) and the journal (events, streams, cursors,
// the cockpit's request queue) are LOCAL-ONLY: `{}` here, refused with 403 even to the owner, and still served on
// the unix socket. (attach is not an API route: it is tmux.) A cockpit on the other machine therefore cannot
// lease, stream events or type into this machine's panes until M2d gives those a per-device secret.
var tailnetRoutes = map[string]routePolicy{
	"/v1/info":              {Owner: []string{"GET"}, Peer: []string{"GET"}},
	"/v1/resolve":           {Owner: []string{"GET"}, Peer: []string{"GET"}},
	"/v1/sessions":          {Owner: []string{"GET", "POST"}, Peer: []string{"GET"}}, // POST: remote start (M2c), owner only
	"/v1/sessions/":         {},                                                      // deliver, prompt, replace: local only
	"/v1/start":             {},                                                      // needs the caller's uid: local socket only
	"/v1/events":            {},
	"/v1/events/stream":     {},
	"/v1/cursors/":          {},
	"/v1/cursors":           {},
	"/v1/cockpit/lease":     {}, // steal: local only
	"/v1/cockpit/requests":  {},
	"/v1/cockpit/requests/": {},
	"/v1/cockpit/status":    {},
	"/v1/cockpit/pending":   {},
}

// policyMux maps a request path to the pattern the real mux would pick (same matching rules).
var policyMux = func() *http.ServeMux {
	m := http.NewServeMux()
	for p := range tailnetRoutes {
		m.HandleFunc(p, func(http.ResponseWriter, *http.Request) {})
	}
	return m
}()

// allowed is what each class may call on the tailnet listener: the route's pattern, then its methods.
func allowed(c Class, r *http.Request) bool {
	p := r.URL.Path
	if strings.Contains(p, "//") || strings.Contains(p, "/./") || strings.Contains(p, "/../") || strings.HasSuffix(p, "/..") {
		return false
	}
	_, pat := policyMux.Handler(r)
	pol, ok := tailnetRoutes[pat]
	if !ok {
		return false
	}
	var ms []string
	switch c {
	case ClassOwner:
		ms = pol.Owner
	case ClassPeer:
		ms = pol.Peer
	}
	for _, m := range ms {
		if m == "*" || m == r.Method {
			return true
		}
	}
	return false
}

// maxStartRequest bounds a remote session start (POST /v1/sessions): the peers are asked and a container launched.
var maxStartRequest = 5 * time.Minute

type identKey struct{}

// identOf is who the tailnet said the caller is (set only on the tailnet listener).
func identOf(r *http.Request) whois.Identity {
	i, _ := r.Context().Value(identKey{}).(whois.Identity)
	return i
}

// refuse writes the contract's one refusal shape (docs/m2c-remote-start.md §1): {"refused":<code>,"message":...}.
// "error" repeats the code for the clients that read the older field.
func refuse(w http.ResponseWriter, status int, code, message string) {
	writeJSON(w, status, map[string]any{"refused": code, "error": code, "message": message})
}

// maxTailnetRequest bounds a streaming or long-poll request: access is checked once at the start, so how
// long a revoked caller can keep listening is bounded here (the client reconnects, and is classed again).
var maxTailnetRequest = 60 * time.Second

func hostOnly(h string) string {
	if hh, _, err := net.SplitHostPort(h); err == nil {
		return strings.Trim(hh, "[]")
	}
	return strings.Trim(h, "[]")
}

// localIP reports whether ip is an address of this machine: a connection from it is a local process, not
// a tailnet peer (the kernel gives it the machine's own tailnet address as its source).
func localIP(ip netip.Addr, conn net.Addr) bool {
	if ta, ok := conn.(*net.TCPAddr); ok {
		if la, ok := netip.AddrFromSlice(ta.IP); ok && la.Unmap() == ip {
			return true
		}
	}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if ipn, ok := a.(*net.IPNet); ok {
			if la, ok := netip.AddrFromSlice(ipn.IP); ok && la.Unmap() == ip {
				return true
			}
		}
	}
	return false
}

// screen is everything checked before WhoIs: it returns a refusal code (0 = go on).
func (cc ClassConfig) screen(r *http.Request) (ip netip.Addr, code int, why string) {
	ap, err := netip.ParseAddrPort(r.RemoteAddr)
	if err != nil {
		return ip, 403, "forbidden"
	}
	ip = ap.Addr().Unmap()
	if !tailnetAddr(ip) {
		return ip, 403, "forbidden"
	}
	if c, _ := r.Context().Value(http.LocalAddrContextKey).(net.Addr); localIP(ip, c) {
		return ip, 403, "forbidden" // a local process, not a tailnet peer
	}
	ok := false
	h := hostOnly(r.Host)
	for _, x := range cc.Hosts {
		if strings.EqualFold(strings.TrimSuffix(hostOnly(x), "."), strings.TrimSuffix(h, ".")) {
			ok = true
		}
	}
	if !ok {
		return ip, 403, "forbidden" // DNS rebinding: the name is not this daemon's
	}
	if r.Header.Get("Origin") != "" {
		return ip, 403, "forbidden" // a browser page, not a daemon or a CLI
	}
	if v := r.Header.Get("Sec-Fetch-Site"); v != "" && v != "none" {
		return ip, 403, "forbidden"
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			return ip, 415, "unsupported_media_type"
		}
	}
	return ip, 0, ""
}

// TailnetHandler serves the API to tailnet callers, classed through who. A failed WhoIs is class none.
func (s *Server) TailnetHandler(who whois.Resolver, cc ClassConfig) http.Handler {
	ownerH, peerH := s.mkHandler(true, false), s.mkHandler(false, false)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, code, why := cc.screen(r)
		if code != 0 {
			refuse(w, code, why, "refused before the caller was identified")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		class := ClassNone
		var ident whois.Identity
		if id, err := who.WhoIs(ctx, r.RemoteAddr); err == nil && id.Owns(ip) {
			class, ident = cc.Classify(id), id
		}
		cancel()
		if !allowed(class, r) {
			refuse(w, 403, "forbidden", "not allowed for this caller")
			return
		}
		// Identity headers are never read here; strip them so nothing downstream can.
		for k := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "tailscale-") || strings.EqualFold(k, "X-Forwarded-For") {
				r.Header.Del(k)
			}
		}
		// a stream or long poll ends at the cap; a session start (it launches a container: minutes) gets longer
		limit := maxTailnetRequest
		if r.Method == http.MethodPost && r.URL.Path == "/v1/sessions" {
			limit = maxStartRequest
		}
		rctx, rcancel := context.WithTimeout(r.Context(), limit)
		defer rcancel()
		r = r.WithContext(context.WithValue(context.WithValue(rctx, classKey{}, class), identKey{}, ident))
		if class == ClassOwner {
			ownerH.ServeHTTP(w, r)
			return
		}
		peerH.ServeHTTP(w, r) // a peer is not the owner: no owner operations even if a route forgot to ask
	})
}

var tailnetV4 = netip.MustParsePrefix("100.64.0.0/10")
var tailnetV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")

// tailnetAddr: is ip in the tailnet's address space (100.64.0.0/10 or fd7a:115c:a1e0::/48)? Pure, so the
// prefix rule is tested without binding anything.
func tailnetAddr(ip netip.Addr) bool {
	ip = ip.Unmap()
	return !ip.IsUnspecified() && (tailnetV4.Contains(ip) || tailnetV6.Contains(ip))
}

// CheckListenAddr is the bind rule: a tailnet-space address, with a port, that is one of THIS node's own
// tailnet addresses (self: tailscaled's report; 100.64.0.0/10 is also shared space an ISP or another VPN
// may hand out). self == nil skips the second check (tests).
func CheckListenAddr(addr string, self []netip.Addr) (netip.AddrPort, error) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return ap, fmt.Errorf("--tailnet-listen %q: want <tailnet ip>:<port>: %v", addr, err)
	}
	ip := ap.Addr().Unmap()
	if !tailnetAddr(ip) {
		return ap, fmt.Errorf("--tailnet-listen %s is not a tailnet address (100.64.0.0/10 or fd7a:115c:a1e0::/48): never a wildcard or a LAN address", ip)
	}
	if ap.Port() == 0 {
		return ap, fmt.Errorf("--tailnet-listen %q needs a port", addr)
	}
	if self != nil {
		found := false
		for _, a := range self {
			if a.Unmap() == ip {
				found = true
			}
		}
		if !found {
			return ap, fmt.Errorf("--tailnet-listen %s is not one of this node's own tailnet addresses (%v)", ip, self)
		}
	}
	return ap, nil
}

// ListenTailnet listens on one tailnet address (never a wildcard, never a LAN address): an exposed
// listener elsewhere would be a door nobody asked for. maxConns bounds the open connections (0 = 64): an
// idle connection costs a descriptor, and any node the ACL lets in could open thousands.
func ListenTailnet(addr string, self []netip.Addr, maxConns int, perSource ...int) (net.Listener, error) {
	if _, err := CheckListenAddr(addr, self); err != nil {
		return nil, err
	}
	l, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	if maxConns <= 0 {
		maxConns = 64
	}
	per := 16
	if len(perSource) > 0 && perSource[0] > 0 {
		per = perSource[0]
	}
	return &limitListener{Listener: l, sem: make(chan struct{}, maxConns), perSrc: per, bySrc: map[string]int{}}, nil
}

// limitListener caps concurrent accepted connections (a slot frees when the connection closes).
type limitListener struct {
	net.Listener
	sem    chan struct{}
	perSrc int // most open connections from one source address (0 = no limit)
	mu     sync.Mutex
	bySrc  map[string]int
}

// Accept returns connections up to the global cap (it waits for a slot) and drops, at once, any connection
// from a source that already holds perSrc: one node cannot fill the cap and starve the others.
func (l *limitListener) Accept() (net.Conn, error) {
	for {
		l.sem <- struct{}{}
		c, err := l.Listener.Accept()
		if err != nil {
			<-l.sem
			return nil, err
		}
		src := ""
		if ta, ok := c.RemoteAddr().(*net.TCPAddr); ok {
			src = ta.IP.String()
		}
		l.mu.Lock()
		if l.perSrc > 0 && l.bySrc != nil && l.bySrc[src] >= l.perSrc {
			l.mu.Unlock()
			c.Close()
			<-l.sem
			continue
		}
		if l.bySrc != nil {
			l.bySrc[src]++
		}
		l.mu.Unlock()
		return &limitConn{Conn: c, release: func() {
			l.mu.Lock()
			if l.bySrc != nil {
				if l.bySrc[src]--; l.bySrc[src] <= 0 {
					delete(l.bySrc, src)
				}
			}
			l.mu.Unlock()
			<-l.sem
		}}, nil
	}
}

type limitConn struct {
	net.Conn
	once    sync.Once
	release func()
}

func (c *limitConn) Close() error {
	err := c.Conn.Close()
	c.once.Do(c.release)
	return err
}
