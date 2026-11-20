// Tests of the tailnet listener's caller classes (M2a, T:daemon-authz). Why: the whole point is that a
// request's authority comes from what Tailscale says about the peer's address and from nothing the
// request carries, so each case is a way to claim more than you are: another login, an unlisted device,
// a tagged node, a forged identity header, an unanswered WhoIs, a wildcard listener.
package daemonapi

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/verbq"
	"github.com/landovsky/cc-sandbox/internal/whois"
)

// fakeWho answers by remote IP.
type fakeWho struct {
	ids   map[string]whois.Identity
	calls int
}

func (f *fakeWho) WhoIs(_ context.Context, addr string) (whois.Identity, error) {
	f.calls++
	host, _, _ := net.SplitHostPort(addr)
	if id, ok := f.ids[host]; ok {
		if id.Addrs == nil { // the node's own address is the one asked about, unless a case says otherwise
			id.Addrs = []netip.Addr{netip.MustParseAddr(host)}
		}
		return id, nil
	}
	return whois.Identity{}, errors.New("whois: no such peer")
}

func id(login, short string, tags ...string) whois.Identity {
	return whois.Identity{Login: login, Device: short + ".example.ts.net", Short: short, Tags: tags}
}

// idAt is an identity that tailscaled answered for an address that is NOT the node's own (a subnet route).
func idAt(i whois.Identity, own string) whois.Identity {
	i.Addrs = []netip.Addr{netip.MustParseAddr(own)}
	return i
}

// hostdTag is the tag only hostd's devices carry; tag:ccsbx is the operator proxies' and is never a peer.
const hostdTag = "tag:ccsbx-hostd"

var cc = ClassConfig{
	OwnerLogin:   "tomas@example.com",
	OwnerDevices: []string{"mac-tomas.example.ts.net", "old-hp.example.ts.net"},
	PeerDevices:  []string{"future-two.example.ts.net"},
	PeerTags:     []string{hostdTag},
	Hosts:        []string{"hp-ubuntu.example.ts.net", "100.64.0.99"},
}

func tailnetRig(t *testing.T) (http.Handler, *fakeWho) {
	t.Helper()
	j, _ := journal.Open(t.TempDir(), "hp-ubuntu", 0)
	q, _ := verbq.Open(t.TempDir())
	t.Cleanup(func() { j.Close(); q.Close() })
	who := &fakeWho{ids: map[string]whois.Identity{
		"100.64.0.1":        id("tomas@example.com", "mac-tomas"),                     // the owner on a listed device
		"100.64.0.2":        id("tomas@example.com", "old-laptop"),                    // the owner's login on an UNLISTED device
		"100.64.0.3":        id("mallory@example.com", "mac-tomas"),                   // someone else's login on a listed device name
		"100.64.0.4":        id("tagged-devices", "future-two", hostdTag),             // a listed daemon
		"100.64.0.5":        id("tagged-devices", "other-hostd", hostdTag),            // carries the daemon tag
		"100.64.0.6":        id("tagged-devices", "rogue", "tag:other"),               // some other tag
		"100.64.0.7":        id("tomas@example.com", "old-hp", hostdTag),              // the owner's login but TAGGED: not a person
		"100.64.0.8":        id("tagged-devices", "stranger-pod", "tag:ccsbx"),        // a cluster operator proxy: what a fenced pod reaches
		"100.64.0.9":        id("eve@other.com", "future-two"),                        // a listed peer device's NAME, not tagged, not the owner's login
		"100.64.0.10":       idAt(id("tomas@example.com", "mac-tomas"), "100.64.0.1"), // an address behind the owner's node (a subnet route)
		"100.64.0.11":       func() whois.Identity { i := id("tomas@example.com", "mac-tomas"); i.Router = true; return i }(),
		"fd7a:115c:a1e0::1": id("tomas@example.com", "mac-tomas"), // the owner, over IPv6
	}}
	dir := fakeDirT{[]msg.Session{{Sid: "sM", Label: "cc-sandbox:mgr", Realm: "production", Live: true, Gen: 1}}}
	rt := &msg.Router{Dir: dir, Out: &nopOut{}, Idle: idleT{true}, StateDir: t.TempDir()}
	s := &Server{J: j, Q: q, Msg: rt, Dir: dir, Info: map[string]any{"machine": "hp-ubuntu"}}
	return s.TailnetHandler(who, cc), who
}

func tcall(h http.Handler, from, method, path string, hdr ...string) (int, string) {
	req := httptest.NewRequest(method, path, strings.NewReader(`{}`))
	req.RemoteAddr = net.JoinHostPort(from, "50000")
	req.Host = "hp-ubuntu.example.ts.net:7480"
	if method != "GET" {
		req.Header.Set("Content-Type", "application/json")
	}
	for i := 0; i+1 < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec.Code, rec.Body.String()
}

// T:daemon-authz, allow: the owner on a listed device and a listed daemon each get what their class gets.
func TestOwnerAndPeerGetTheirClassAllow(t *testing.T) {
	h, _ := tailnetRig(t)
	if code, body := tcall(h, "100.64.0.1", "GET", "/v1/info"); code != 200 || !strings.Contains(body, `"caller_class":"owner"`) {
		t.Fatalf("owner: %d %s", code, body)
	}
	if code, body := tcall(h, "100.64.0.1", "GET", "/v1/sessions"); code != 200 {
		t.Fatalf("owner sessions: %d %s", code, body)
	}
	if code, body := tcall(h, "100.64.0.4", "GET", "/v1/info"); code != 200 || !strings.Contains(body, `"caller_class":"peer"`) {
		t.Fatalf("peer: %d %s", code, body)
	}
	if code, _ := tcall(h, "100.64.0.5", "GET", "/v1/info"); code != 200 {
		t.Fatalf("a node carrying the listed tag is a peer: %d", code)
	}
	if code, body := tcall(h, "fd7a:115c:a1e0::1", "GET", "/v1/info"); code != 200 || !strings.Contains(body, `"caller_class":"owner"`) {
		t.Fatalf("an IPv6 tailnet caller: %d %s", code, body)
	}
}

// context (S1): the whole point of a peer is that another machine's daemon can ask it for its sessions and
// resolve a label; resolve used to answer 403 to a peer, hidden by a test rig with no messaging behind it.
func TestAPeerCanListAndResolve(t *testing.T) {
	h, _ := tailnetRig(t)
	if code, body := tcall(h, "100.64.0.4", "GET", "/v1/sessions"); code != 200 || !strings.Contains(body, "sM") {
		t.Fatalf("peer sessions: %d %s", code, body)
	}
	if code, body := tcall(h, "100.64.0.4", "GET", "/v1/resolve?label=cc-sandbox:mgr"); code != 200 || !strings.Contains(body, "sM") {
		t.Fatalf("peer resolve: %d %s", code, body)
	}
}

// T:daemon-authz, block: every way to claim more than you are.
func TestEveryOtherCallerIsRefusedBlock(t *testing.T) {
	h, _ := tailnetRig(t)
	for name, from := range map[string]string{
		"the owner's login on an unlisted device":                                "100.64.0.2",
		"another login on a listed device's name":                                "100.64.0.3",
		"a tagged node with another tag":                                         "100.64.0.6",
		"the owner's login on a TAGGED node":                                     "100.64.0.7",
		"an address Tailscale does not know":                                     "100.64.9.9",
		"a LAN address":                                                          "192.168.1.5",
		"the cluster operator's proxy tag:ccsbx (M3: a fenced pod can reach it)": "100.64.0.8",
		"a listed peer device's name, untagged, another login (S4)":              "100.64.0.9",
		"an address inside the owner's subnet route: not the node's own (S5)":    "100.64.0.10",
		"a node that advertises subnet routes (S5)":                              "100.64.0.11",
	} {
		if name == "the owner's login on a TAGGED node" {
			// tagged with a daemon tag: it is a peer, never the owner
			if code, body := tcall(h, from, "GET", "/v1/info"); code != 200 || !strings.Contains(body, `"caller_class":"peer"`) {
				t.Errorf("%s: %d %s", name, code, body)
			}
			if code, _ := tcall(h, from, "POST", "/v1/cockpit/lease?realm=production"); code != 403 {
				t.Errorf("%s: owner operation: %d", name, code)
			}
			continue
		}
		if code, _ := tcall(h, from, "GET", "/v1/info"); code != 403 {
			t.Errorf("%s: %d", name, code)
		}
	}
}

// context: a peer daemon is not the owner: it may not deliver, take the cockpit lease, read the journal or
// prompt/replace a session. Even if a route forgot to check, the peer handler is not the owner handler.
func TestAPeerCannotDoOwnerThingsBlock(t *testing.T) {
	h, _ := tailnetRig(t)
	for _, c := range [][2]string{
		{"POST", "/v1/sessions/s0x/deliver"}, {"POST", "/v1/sessions/s0x/prompt"}, {"POST", "/v1/sessions/s0x/replace"},
		{"POST", "/v1/cockpit/lease?realm=production"}, {"GET", "/v1/events?after=0"}, {"GET", "/v1/cockpit/status?realm=production"},
		{"POST", "/v1/cursors/production"}, {"GET", "/v1/events/stream"}, {"POST", "/v1/start"},
	} {
		if code, _ := tcall(h, "100.64.0.4", c[0], c[1]); code != 403 {
			t.Errorf("%s %s as a peer: %d", c[0], c[1], code)
		}
	}
}

// context (M1): hp's own processes (the fence's uid, a sidecar, any service) can connect to hp's own
// tailnet address; the kernel makes the machine's own address the source, and Tailscale then answers "hp".
// That must never be the owner, whatever is listed.
func TestALocalProcessOnTheMachinesOwnTailnetAddressIsRefused(t *testing.T) {
	h, _ := tailnetRig(t)
	req := httptest.NewRequest("GET", "/v1/info", nil)
	req.RemoteAddr = "100.64.0.1:40000"
	req.Host = "hp-ubuntu.example.ts.net"
	req = req.WithContext(context.WithValue(req.Context(), http.LocalAddrContextKey, &net.TCPAddr{IP: net.ParseIP("100.64.0.1"), Port: 7480}))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 403 {
		t.Fatalf("a connection from the machine's own address was served: %d", rec.Code)
	}
}

// context (M2): a web page in the owner's browser reaches the listener through the owner's device, so the
// network identity says "owner". Rebinding, an Origin header, a cross-site fetch and a form-style POST are
// what such a page can do; none may pass.
func TestABrowserPageCannotDriveTheOwnersOperations(t *testing.T) {
	h, _ := tailnetRig(t)
	rebind := httptest.NewRequest("GET", "/v1/info", nil)
	rebind.RemoteAddr, rebind.Host = "100.64.0.1:40000", "evil.example.com:7480"
	if rec := httptest.NewRecorder(); func() int { h.ServeHTTP(rec, rebind); return rec.Code }() != 403 {
		t.Error("a rebound Host name was served")
	}
	for name, hdr := range map[string][]string{
		"an Origin header":    {"Origin", "http://evil.example.com"},
		"a cross-site fetch":  {"Sec-Fetch-Site", "cross-site"},
		"a same-site fetch":   {"Sec-Fetch-Site", "same-site"},
		"a same-origin fetch": {"Sec-Fetch-Site", "same-origin"},
	} {
		if code, _ := tcall(h, "100.64.0.1", "GET", "/v1/info", hdr...); code != 403 {
			t.Errorf("%s: %d", name, code)
		}
	}
	for _, ct := range []string{"text/plain", "application/x-www-form-urlencoded", "multipart/form-data; boundary=x", ""} {
		req := httptest.NewRequest("POST", "/v1/cockpit/lease?realm=production", strings.NewReader(`{"steal":true}`))
		req.RemoteAddr, req.Host = "100.64.0.1:40000", "hp-ubuntu.example.ts.net:7480"
		if ct != "" {
			req.Header.Set("Content-Type", ct)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != 415 {
			t.Errorf("a POST with Content-Type %q: %d", ct, rec.Code)
		}
	}
	// the legitimate forms still work: the listen IP as Host, a JSON POST, a CLI's "none" fetch site
	if code, _ := tcall(h, "100.64.0.1", "GET", "/v1/info", "Sec-Fetch-Site", "none"); code != 200 {
		t.Errorf("sec-fetch-site none: %d", code)
	}
}

// context (S5): nothing outside the tailnet's address space is even asked about (no forking tailscale for a LAN
// scanner), and an answer for another address is not believed.
func TestOnlyTailnetAddressesAreEverLookedUp(t *testing.T) {
	h, who := tailnetRig(t)
	for _, from := range []string{"192.168.1.5", "10.0.0.7", "8.8.8.8", "127.0.0.1"} {
		tcall(h, from, "GET", "/v1/info")
	}
	if who.calls != 0 {
		t.Fatalf("WhoIs was asked %d times for non-tailnet addresses", who.calls)
	}
}

// context (S2): the owner's permission is an allow-list, so a route added to the API is closed on the
// tailnet until it is classified. This walks the real mux.
func TestEveryRouteIsClassifiedForTheTailnet(t *testing.T) {
	s := &Server{}
	seen := map[string]bool{}
	for _, r := range s.routes() {
		seen[r.Pattern] = true
		if _, ok := tailnetRoutes[r.Pattern]; !ok {
			t.Errorf("route %s has no entry in tailnetRoutes: classify it (owner/peer methods, or empty = closed on the tailnet)", r.Pattern)
		}
	}
	for p := range tailnetRoutes {
		if !seen[p] {
			t.Errorf("tailnetRoutes has %s, which the API does not serve", p)
		}
	}
	// and what an unclassified or wrong-method request gets
	req := httptest.NewRequest("DELETE", "/v1/info", nil)
	if allowed(ClassOwner, req) {
		t.Error("an unlisted method was allowed")
	}
	if allowed(ClassOwner, httptest.NewRequest("GET", "/v1/new-thing", nil)) {
		t.Error("an unlisted route was allowed")
	}
	if allowed(ClassOwner, httptest.NewRequest("GET", "/v1/sessions/../start", nil)) || allowed(ClassOwner, httptest.NewRequest("POST", "/v1//start", nil)) {
		t.Error("a path with dot segments or doubled slashes was allowed")
	}
}

// context (S6/S7): access is checked when a stream or long poll starts, so how long it may run bounds how
// long a revoked caller keeps listening.
func TestAStreamOnTheTailnetEndsAtTheCap(t *testing.T) {
	old := maxTailnetRequest
	maxTailnetRequest = 300 * time.Millisecond
	defer func() { maxTailnetRequest = old }()
	h, _ := tailnetRig(t)
	t0 := time.Now()
	tcall(h, "100.64.0.1", "GET", "/v1/events?after=999999&wait=30")
	if d := time.Since(t0); d > 5*time.Second {
		t.Fatalf("a 30 s long poll ran %v despite the cap", d)
	}
}

// context (S7): any node the ACL admits could otherwise open thousands of idle connections.
func TestTheListenerCapsOpenConnections(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &limitListener{Listener: base, sem: make(chan struct{}, 2)}
	defer l.Close()
	var cs []net.Conn
	for i := 0; i < 3; i++ {
		c, _ := net.Dial("tcp", base.Addr().String())
		defer c.Close()
		cs = append(cs, c)
	}
	acc := make(chan net.Conn, 3)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			acc <- c
		}
	}()
	a1, a2 := <-acc, <-acc
	select {
	case <-acc:
		t.Fatal("a third connection was accepted over the cap of 2")
	case <-time.After(200 * time.Millisecond):
	}
	a1.Close()
	select {
	case c := <-acc:
		c.Close()
	case <-time.After(2 * time.Second):
		t.Fatal("closing a connection did not free its slot")
	}
	a2.Close()
}

// context (M3, S4): the configuration itself must not widen the classes.
func TestConfigurationThatWouldWidenTheClassesIsRefused(t *testing.T) {
	for name, c := range map[string]ClassConfig{
		"tag:ccsbx as a peer tag (the operator proxies, reachable by a fenced pod)": {PeerTags: []string{"tag:ccsbx"}},
		"the same tag in another case":   {PeerTags: []string{"TAG:CCSBX"}},
		"a short owner device name":      {OwnerLogin: "a@b.c", OwnerDevices: []string{"mac-tomas"}},
		"a short peer device name":       {OwnerLogin: "a@b.c", OwnerDevices: []string{"m.x.ts.net"}, PeerDevices: []string{"future-two"}},
		"an owner login with no devices": {OwnerLogin: "a@b.c"},
		"owner devices with no login":    {OwnerDevices: []string{"m.x.ts.net"}},
		"nothing at all":                 {},
		"a peer tag that is no tag":      {PeerTags: []string{"hostd"}},
	} {
		if err := c.Validate(); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if err := cc.Validate(); err != nil {
		t.Errorf("the test configuration itself: %v", err)
	}
}

// context (S9): 100.64.0.0/10 is also what an ISP's CGNAT or another VPN hands out, so an address in it is
// not necessarily tailscale's: it must be one of this node's own tailnet addresses. The prefix rule is a pure
// function, tested without binding anything.
func TestListenAddressRule(t *testing.T) {
	self := []netip.Addr{netip.MustParseAddr("100.64.0.1"), netip.MustParseAddr("fd7a:115c:a1e0::1")}
	for addr, ok := range map[string]bool{
		"100.64.0.1:7480": true, "[fd7a:115c:a1e0::1]:7480": true,
		"100.64.0.2:7480": false, "100.64.0.1:0": false, "0.0.0.0:7480": false, "[::]:7480": false, "127.0.0.1:7480": false,
		"10.0.0.1:7480": false, "192.168.1.5:7480": false, "100.128.0.1:7480": false, "100.63.255.255:7480": false, "nonsense": false,
	} {
		if _, err := CheckListenAddr(addr, self); (err == nil) != ok {
			t.Errorf("%s: err=%v, want ok=%v", addr, err, ok)
		}
	}
	if !tailnetAddr(netip.MustParseAddr("100.127.255.254")) || tailnetAddr(netip.MustParseAddr("100.128.0.0")) || tailnetAddr(netip.MustParseAddr("127.0.0.1")) {
		t.Error("the prefix rule")
	}
}

// context (manager decision, SEC-20): the owner's authority on a listed device is ambient (no secret), so over
// the tailnet even the owner gets only info, resolve and the session list (and, M2c, start). Everything that
// types into a pane, steals the cockpit or reads the journal stays on the local socket: refused over the
// tailnet, and still served locally.
func TestOwnerOperationsStayLocalOnlyEvenForTheOwnerBlock(t *testing.T) {
	h, _ := tailnetRig(t)
	local := func(s *Server, method, path string) int {
		req := httptest.NewRequest(method, path, strings.NewReader(`{"text":"x","from":"cc-tree:x"}`))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	j, _ := journal.Open(t.TempDir(), "hp-ubuntu", 0)
	q, _ := verbq.Open(t.TempDir())
	defer j.Close()
	defer q.Close()
	dir := fakeDirT{[]msg.Session{{Sid: "sM", Label: "cc-sandbox:mgr", Realm: "production", Live: true, Gen: 1}}}
	srv := &Server{J: j, Q: q, Msg: &msg.Router{Dir: dir, Out: &nopOut{}, Idle: idleT{true}, StateDir: t.TempDir()}, Dir: dir}
	for _, c := range [][3]string{
		{"POST", "/v1/sessions/sM/deliver", "deliver"}, {"POST", "/v1/sessions/sM/prompt", "prompt"}, {"POST", "/v1/sessions/sM/replace", "replace"},
		{"POST", "/v1/cockpit/lease?realm=production", "steal the cockpit lease"}, {"GET", "/v1/cockpit/status?realm=production", "cockpit status"},
		{"GET", "/v1/events?after=0", "the journal"}, {"GET", "/v1/events/stream", "the journal stream"}, {"GET", "/v1/cursors", "cursors"},
		{"POST", "/v1/cursors/production", "a cursor"}, {"POST", "/v1/start", "start"},
	} {
		if code, _ := tcall(h, "100.64.0.1", c[0], c[1]); code != 403 {
			t.Errorf("the OWNER over the tailnet, %s (%s %s): %d, want 403", c[2], c[0], c[1], code)
		}
	}
	for _, c := range [][2]string{{"GET", "/v1/events?after=0"}, {"GET", "/v1/cursors"}, {"POST", "/v1/sessions/sM/deliver"}} {
		if code := local(srv, c[0], c[1]); code == 403 || code == 404 && c[0] == "GET" {
			t.Errorf("%s %s is no longer served on the local socket: %d", c[0], c[1], code)
		}
	}
}

// context: session start needs the caller's uid, which a tailnet caller does not have; it stays on the
// local socket even for the owner (M2c adds the remote form with its own identity and rate limits).
func TestStartIsNotServedOnTheTailnetEvenToTheOwner(t *testing.T) {
	h, _ := tailnetRig(t)
	if code, _ := tcall(h, "100.64.0.1", "POST", "/v1/start"); code != 403 {
		t.Fatalf("%d", code)
	}
}

// context: identity headers are what an operator proxy in front of a door sets; here they are the caller's
// own words. Claiming to be the owner in one changes nothing.
// (What this proves is that classification ignores them; the strip in TailnetHandler is defence in depth that
// no test can see, because nothing downstream reads those headers.)
func TestForgedIdentityHeadersAreIgnoredBlock(t *testing.T) {
	h, _ := tailnetRig(t)
	if code, _ := tcall(h, "100.64.0.6", "GET", "/v1/info", "Tailscale-User-Login", "tomas@example.com", "Tailscale-User-Name", "Tomas", "X-Forwarded-For", "100.64.0.1"); code != 403 {
		t.Fatalf("a forged Tailscale-* header or X-Forwarded-For was believed: %d", code)
	}
	if code, _ := tcall(h, "100.64.0.4", "POST", "/v1/sessions/s0x/deliver", "Tailscale-User-Login", "tomas@example.com"); code != 403 {
		t.Fatalf("a peer with a forged owner header: %d", code)
	}
}

// context: WhoIs unanswered (tailscaled down, the CLI missing, a timeout) is "unknown", never "owner".
func TestAFailedWhoisIsForbidden(t *testing.T) {
	h, who := tailnetRig(t)
	who.ids = nil
	if code, _ := tcall(h, "100.64.0.1", "GET", "/v1/info"); code != 403 {
		t.Fatalf("%d", code)
	}
}

// context: nothing may listen on a wildcard or LAN address: "nothing live before the ACL".
func TestTailnetListenerBindsOnlyATailnetAddress(t *testing.T) {
	for _, bad := range []string{"0.0.0.0:7480", "[::]:7480", ":7480", "192.168.1.5:7480", "127.0.0.1:7480", "10.0.0.1:7480", "100.64.0.1", "100.64.0.1:0", "nonsense", "100.128.0.1:7480"} {
		if l, err := ListenTailnet(bad, nil, 0); err == nil {
			l.Close()
			t.Errorf("%q was accepted", bad)
		}
	}
}

func TestClassifyMatchesShortAndFullNamesCaseInsensitively(t *testing.T) {
	c := ClassConfig{OwnerLogin: "Tomas@Example.com", OwnerDevices: []string{"HP-Ubuntu.example.ts.net."}}
	if got := c.Classify(whois.Identity{Login: "tomas@example.com", Device: "hp-ubuntu.example.ts.net", Short: "hp-ubuntu"}); got != ClassOwner {
		t.Fatalf("%v", got)
	}
	if got := c.Classify(whois.Identity{Login: "tomas@example.com", Device: "hp-ubuntu.other-tailnet.ts.net", Short: "hp-ubuntu"}); got != ClassNone {
		t.Fatalf("a node of another tailnet with the same short name: %v", got)
	}
	if got := (ClassConfig{}).Classify(whois.Identity{Login: "tomas@example.com", Device: "x.ts.net", Short: "x"}); got != ClassNone {
		t.Fatalf("an empty config must classify nobody: %v", got)
	}
}
