// tailnet.go is the daemon API's listener on the tailnet (M2a, plan §4.4): the same API as the local
// socket, but the caller is whoever Tailscale says is on the other end of the connection. It exists so
// that a request's authority comes from the tailnet's own record of the peer (login, device, tags) and
// from nothing the request carries: no header, no body field, no X-Forwarded-For. A caller is
//
//	owner  the owner's login on a LISTED device (not a tagged node)
//	peer   a listed daemon (a listed device, or a node carrying a listed tag)
//	none   anything else, and every failure to find out: 403
//
// An owner gets the owner operations the local socket serves, except session start (that one needs the
// caller's uid and stays local until M2c adds the remote form); a peer gets only what another daemon
// needs (info, resolve, the session list; M2c and M2d add start and deliver). Nothing is served here
// unless hostd was told to listen (--tailnet-listen), and only on a tailnet address.
package daemonapi

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"strings"
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

// ClassConfig lists who counts as what. Devices match a node's short name or its full MagicDNS name,
// case-insensitively.
type ClassConfig struct {
	OwnerLogin   string
	OwnerDevices []string
	PeerDevices  []string
	PeerTags     []string
}

func inList(l []string, vs ...string) bool {
	for _, x := range l {
		for _, v := range vs {
			if v != "" && strings.EqualFold(strings.TrimSuffix(x, "."), v) {
				return true
			}
		}
	}
	return false
}

// Classify applies the lists to what Tailscale said.
func (c ClassConfig) Classify(id whois.Identity) Class {
	listedOwner := inList(c.OwnerDevices, id.Device, id.Short)
	if c.OwnerLogin != "" && len(id.Tags) == 0 && strings.EqualFold(id.Login, c.OwnerLogin) && listedOwner {
		return ClassOwner
	}
	if inList(c.PeerDevices, id.Device, id.Short) {
		return ClassPeer
	}
	for _, t := range id.Tags {
		if inList(c.PeerTags, t) {
			return ClassPeer
		}
	}
	return ClassNone
}

type classKey struct{}

func classOf(r *http.Request) Class { c, _ := r.Context().Value(classKey{}).(Class); return c }

// allowed is what each class may call on the tailnet listener.
func allowed(c Class, method, path string) bool {
	switch c {
	case ClassOwner:
		return path != "/v1/start" // session start needs the caller's uid: local socket only
	case ClassPeer:
		return method == http.MethodGet && (path == "/v1/info" || path == "/v1/resolve" || path == "/v1/sessions")
	}
	return false
}

// TailnetHandler serves the API to tailnet callers, classed through who. A failed WhoIs is class none.
func (s *Server) TailnetHandler(who whois.Resolver, cc ClassConfig) http.Handler {
	ownerH, peerH := s.mkHandler(true, false), s.mkHandler(false, false)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 6*time.Second)
		defer cancel()
		class := ClassNone
		if id, err := who.WhoIs(ctx, r.RemoteAddr); err == nil {
			class = cc.Classify(id)
		}
		if !allowed(class, r.Method, r.URL.Path) {
			writeJSON(w, 403, map[string]any{"error": "forbidden"})
			return
		}
		// Identity headers are never read here; strip them so nothing downstream can.
		for k := range r.Header {
			if strings.HasPrefix(strings.ToLower(k), "tailscale-") || strings.EqualFold(k, "X-Forwarded-For") {
				r.Header.Del(k)
			}
		}
		r = r.WithContext(context.WithValue(r.Context(), classKey{}, class))
		if class == ClassOwner {
			ownerH.ServeHTTP(w, r)
			return
		}
		peerH.ServeHTTP(w, r) // a peer is not the owner: no owner operations even if a route forgot to ask
	})
}

var tailnetV4 = netip.MustParsePrefix("100.64.0.0/10")
var tailnetV6 = netip.MustParsePrefix("fd7a:115c:a1e0::/48")

// ListenTailnet listens on one tailnet address (never a wildcard, never a LAN address): an exposed
// listener elsewhere would be a door nobody asked for.
func ListenTailnet(addr string) (net.Listener, error) {
	ap, err := netip.ParseAddrPort(addr)
	if err != nil {
		return nil, fmt.Errorf("--tailnet-listen %q: want <tailnet ip>:<port>: %v", addr, err)
	}
	ip := ap.Addr().Unmap()
	if ip.IsUnspecified() || !(tailnetV4.Contains(ip) || tailnetV6.Contains(ip)) {
		return nil, fmt.Errorf("--tailnet-listen %s is not a tailnet address (100.64.0.0/10 or fd7a:115c:a1e0::/48): never a wildcard or a LAN address", ip)
	}
	if ap.Port() == 0 {
		return nil, fmt.Errorf("--tailnet-listen %q needs a port", addr)
	}
	return net.Listen("tcp", addr)
}
