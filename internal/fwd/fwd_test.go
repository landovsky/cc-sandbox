// Tests for the forwarder. They guard against the fence injecting or seeing a credential, reaching a
// non-door or a decision path, a redirect leaking a location, and the proof not binding the body (P2).

package fwd

import (
	"bytes"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/token"
)

// rig: a real door (U5's Middleware as the verifier), a forwarder in front of
// it, and a "fence" client that knows only the fwd URL.
type rig struct {
	door *httptest.Server
	fwd  *httptest.Server
	f    *Forwarder
	tok  string
	gen  *token.GenKey
	seen chan *http.Request
	logs *bytes.Buffer
	hits atomic.Int64
}

func newRig(t *testing.T) *rig {
	t.Helper()
	key, err := token.LoadOrCreateKeyFile(filepath.Join(t.TempDir(), "dev.key"))
	if err != nil {
		t.Fatal(err)
	}
	gen, _ := token.NewGenKey()
	tok, err := token.Mint(key, token.Claims{Sub: "tomas/hp-ubuntu/sFWD/g1", Billing: "stub", Profile: "dev-default",
		Aud: []string{"ccsbx-model", "ccsbx-egress"}, Cnf: token.CnfOf(gen.Public)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	r := &rig{tok: tok, gen: gen, seen: make(chan *http.Request, 64), logs: &bytes.Buffer{}}
	ring := key.Ring([]string{"tomas"}, []string{"hp-ubuntu"})
	handler := func(w http.ResponseWriter, q *http.Request) {
		r.hits.Add(1)
		r.seen <- q
		io.Copy(io.Discard, q.Body)
		w.Header().Set("Location", "http://169.254.169.254/")
		w.WriteHeader(200)
		w.Write([]byte("door ok"))
	}
	// Two doors, each with its own verifier and audience: a proof for one is useless at the other.
	r.door = httptest.NewServer(token.NewVerifier(ring, "ccsbx-model").Middleware(http.HandlerFunc(handler), nil))
	t.Cleanup(r.door.Close)
	egress := httptest.NewServer(token.NewVerifier(ring, "ccsbx-egress").Middleware(http.HandlerFunc(handler), nil))
	t.Cleanup(egress.Close)
	du, _ := url.Parse(r.door.URL)
	eu, _ := url.Parse(egress.URL)
	f, err := New(Config{Token: tok, Key: gen, Doors: map[string]*url.URL{"model": du, "egress": eu},
		Logf: func(format string, a ...any) { r.logs.WriteString(strings.TrimSpace(format) + "\n") }})
	if err != nil {
		t.Fatal(err)
	}
	r.f = f
	r.fwd = httptest.NewServer(f)
	t.Cleanup(r.fwd.Close)
	return r
}

func (r *rig) fence(t *testing.T, method, path string, hdr map[string]string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(method, r.fwd.URL+path, strings.NewReader("{}"))
	for k, v := range hdr {
		req.Header.Set(k, v)
	}
	resp, err := (&http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

// context "the fence has no credentials; the forwarder adds them": a plain call
// through fwd is accepted by a door that demands token + proof.
func TestForwardsWithTokenAndProof(t *testing.T) {
	r := newRig(t)
	resp := r.fence(t, "POST", "/model/v1/messages?beta=1", map[string]string{"X-Api-Key": "fence-made-up"})
	b, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || string(b) != "door ok" {
		t.Fatalf("%d %s", resp.StatusCode, b)
	}
	q := <-r.seen
	if q.Header.Get("X-Api-Key") != "" {
		t.Fatal("the fence's own key reached the door")
	}
	if q.URL.RequestURI() != "/v1/messages?beta=1" {
		t.Fatalf("door saw %s", q.URL.RequestURI())
	}
}

// context "the fence tries to speak as someone else": headers it sets to claim
// a token, proof or Tailscale identity are dropped, never relayed.
func TestFenceCannotInjectCredentials(t *testing.T) {
	r := newRig(t)
	resp := r.fence(t, "POST", "/model/v1/messages", map[string]string{
		"Authorization": "Bearer stolen", "X-Ccsbx-Proof": "x", "Tailscale-User-Login": "someone@example.invalid",
		"X-Forwarded-For": "1.2.3.4", "Cookie": "a=b"})
	if resp.StatusCode != 200 {
		t.Fatalf("%d (the door accepted fwd's own credentials only if this is 200)", resp.StatusCode)
	}
	q := <-r.seen
	for _, h := range []string{"Tailscale-User-Login", "X-Forwarded-For", "Cookie"} {
		if q.Header.Get(h) != "" {
			t.Fatalf("%s relayed", h)
		}
	}
}

// T:approve-via-fwd — a decision endpoint is refused even with a valid token
// behind it, and a rules file that names one is rejected at start.
func TestApproveViaFwd(t *testing.T) {
	r := newRig(t)
	for _, p := range []string{"/egress/v1/approvals/ar-1/decide", "/egress/v1/grants/ar-1/approve",
		"/egress/github/owner/decide", "/model/v1/admin/keys", "/egress/v1/x/approve", "/egress/v1/x/decide", "/egress/v1/x/decision", "/egress/v1/x/revoke"} {
		for _, m := range []string{"POST", "PUT", "GET"} {
			if c := r.fence(t, m, p, nil).StatusCode; c != 403 {
				t.Fatalf("%s %s -> %d", m, p, c)
			}
		}
	}
	if n := r.hits.Load(); n != 0 {
		t.Fatalf("door was reached %d times", n)
	}
	du, _ := url.Parse(r.door.URL)
	_, err := New(Config{Token: r.tok, Key: r.gen, Doors: map[string]*url.URL{"egress": du},
		Rules: []Rule{{Door: "egress", Methods: []string{"POST"}, Path: "/v1/approvals/*/decide"}}})
	if err == nil {
		t.Fatal("a rules file routing a decision endpoint was accepted")
	}
	// A careless broad rule loads, but the deny list still wins over it.
	f, err := New(Config{Token: r.tok, Key: r.gen, Doors: map[string]*url.URL{"egress": du},
		Rules: []Rule{{Door: "egress", Methods: []string{"POST"}, Path: "/**"}}})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(f)
	defer s.Close()
	for _, seg := range []string{"approve", "approvals", "approval", "decide", "decision", "decisions", "grants", "owner", "admin", "revoke", "keys", "sign"} {
		resp, _ := http.Post(s.URL+"/egress/v1/x/"+seg, "application/json", strings.NewReader("{}"))
		if resp.StatusCode != 403 {
			t.Fatalf("broad rule routed a /%s endpoint: %d", seg, resp.StatusCode)
		}
	}
}

// default deny, and shapes of path that could be read two ways.
func TestAllowlistIsDefaultDeny(t *testing.T) {
	r := newRig(t)
	cases := []struct {
		m, p string
		want int
	}{
		{"POST", "/model/v1/messages", 200},
		{"GET", "/model/v1/messages", 403},
		{"POST", "/model/v1/complete", 403},
		{"POST", "/nodoor/v1/messages", 403},
		{"POST", "/model/v1%2Fmessages", 403},
		{"POST", "/model//v1/messages", 403},
		{"POST", "/model/v1/messages;x", 403},
		{"POST", "/model/v1/%2e%2e/messages", 403},
		{"POST", "/egress/github/repos/o/r/pulls", 200},
		{"PUT", "/egress/github/repos/o/r/contents/x", 403},
		{"POST", "/egress/github/repos/o/r/merges", 403},
		{"POST", "/egress/github/repos/o/r/pulls/1/merge", 403},
	}
	for _, c := range cases {
		if got := r.fence(t, c.m, c.p, nil).StatusCode; got != c.want {
			t.Errorf("%s %s = %d, want %d", c.m, c.p, got, c.want)
		}
	}
}

// T:fwd-cannot-reach-non-door — absolute-form requests and CONNECT don't move
// fwd to another address; the dialer only knows the configured doors.
func TestCannotReachNonDoor(t *testing.T) {
	r := newRig(t)
	var other atomic.Int64
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { other.Add(1) }))
	defer evil.Close()
	evilAddr := strings.TrimPrefix(evil.URL, "http://")
	raw := func(req string) string {
		c, err := net.Dial("tcp", strings.TrimPrefix(r.fwd.URL, "http://"))
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		c.Write([]byte(req))
		c.SetReadDeadline(time.Now().Add(2 * time.Second))
		b, _ := io.ReadAll(c)
		return string(b)
	}
	if out := raw("POST " + evil.URL + "/v1/messages HTTP/1.1\r\nHost: " + evilAddr + "\r\nConnection: close\r\n\r\n"); !strings.Contains(out, "403") {
		t.Fatalf("absolute-form: %s", out)
	}
	if out := raw("CONNECT " + evilAddr + " HTTP/1.1\r\nHost: " + evilAddr + "\r\nConnection: close\r\n\r\n"); strings.Contains(out, " 200") {
		t.Fatalf("CONNECT: %s", out)
	}
	if other.Load() != 0 {
		t.Fatal("reached a non-door")
	}
	if len(r.f.cfg.pinned[evilAddr]) > 0 {
		t.Fatal("a non-door is in the dial set")
	}
}

// context "a door answers with a redirect to the metadata address": fwd hands
// the response back without Location and never follows it with the token.
func TestRedirectLocationNotPassed(t *testing.T) {
	r := newRig(t)
	if r.fence(t, "POST", "/model/v1/messages", nil).Header.Get("Location") != "" {
		t.Fatal("Location leaked to the fence")
	}
}

// T:no-token-in-fence (forwarder half): nothing the fence can see (bodies,
// headers, error pages, health, the log) contains the token. The in-fence half
// (env, home, /proc/*/environ, /run) belongs to U1b's selftest.
func TestNoTokenInAnythingTheFenceSees(t *testing.T) {
	r := newRig(t)
	var seen strings.Builder
	for _, c := range []struct{ m, p string }{{"POST", "/model/v1/messages"}, {"GET", "/model/x"},
		{"GET", "/_fwd/health"}, {"POST", "/egress/v1/approvals/1/decide"}, {"GET", "/"}} {
		resp := r.fence(t, c.m, c.p, nil)
		b, _ := io.ReadAll(resp.Body)
		seen.Write(b)
		for k, vs := range resp.Header {
			seen.WriteString(k + strings.Join(vs, ","))
		}
	}
	seen.WriteString(r.logs.String())
	sig := r.tok[strings.LastIndexByte(r.tok, '.')+1:]
	if strings.Contains(seen.String(), sig) || strings.Contains(seen.String(), "Bearer") {
		t.Fatal("token material visible on the fence side")
	}
	if scrub("dial "+r.tok, r.tok) != "dial [token]" {
		t.Fatal("scrub")
	}
}

// context "the generation ends": after the token's exp, fwd stops forwarding.
func TestStopsAtTokenExpiry(t *testing.T) {
	r := newRig(t)
	r.f.cfg.Now = func() time.Time { return time.Now().Add(13 * time.Hour) }
	if c := r.fence(t, "POST", "/model/v1/messages", nil).StatusCode; c != 503 {
		t.Fatalf("%d", c)
	}
}

func TestMainHelpStartsNothing(t *testing.T) {
	var out, errb bytes.Buffer
	if Main([]string{"--help"}, strings.NewReader(""), &out, &errb) != 0 || !strings.Contains(out.String(), "--door") {
		t.Fatal("help")
	}
	out.Reset()
	if Main([]string{"--print-routes"}, strings.NewReader(""), &out, &errb) != 0 || !strings.Contains(out.String(), "/v1/messages") {
		t.Fatal("print-routes")
	}
	if Main([]string{"--listen", "127.0.0.1:0"}, strings.NewReader(""), &out, &errb) != 2 {
		t.Fatal("missing doors must be a usage error")
	}
}

// context "DNS for a door name changes after start (P2-S1)": the token and a fresh proof must
// not follow it. Door names are resolved once and the addresses pinned; the dialer never asks DNS
// again, and it refuses any host:port that is not a configured door.
func TestDoorAddressesArePinned(t *testing.T) {
	r := newRig(t)
	du, _ := url.Parse(r.door.URL)
	_, port, _ := net.SplitHostPort(du.Host)
	var lookups atomic.Int64
	f, err := New(Config{Token: r.tok, Key: r.gen,
		Doors: map[string]*url.URL{"model": {Scheme: "http", Host: "door.invalid:" + port}},
		Resolve: func(host string) ([]net.IP, error) {
			if lookups.Add(1) > 1 {
				return []net.IP{net.ParseIP("203.0.113.9")}, nil // a changed DNS answer
			}
			return []net.IP{net.ParseIP("127.0.0.1")}, nil
		}})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(f)
	defer s.Close()
	for i := 0; i < 3; i++ {
		resp, err := http.Post(s.URL+"/model/v1/messages", "application/json", strings.NewReader("{}"))
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("call %d: %v %v", i, resp, err)
		}
	}
	if lookups.Load() != 1 {
		t.Fatalf("the door name was resolved %d times; once at start only", lookups.Load())
	}
	if _, err := New(Config{Token: r.tok, Key: r.gen, Doors: map[string]*url.URL{"model": {Scheme: "http", Host: "gone.invalid:80"}},
		Resolve: func(string) ([]net.IP, error) { return nil, errors.New("nxdomain") }}); err == nil {
		t.Fatal("a door that does not resolve at start was accepted")
	}
}

// NIT P2-N1: the door receives the bytes CleanPath checked, not a re-escaped variant.
func TestEscapedPathForwardedAsChecked(t *testing.T) {
	r := newRig(t)
	resp := r.fence(t, "GET", "/egress/github/repos/o/r/contents/a%20b", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("%d", resp.StatusCode)
	}
	if got := (<-r.seen).URL.EscapedPath(); got != "/github/repos/o/r/contents/a%20b" {
		t.Fatalf("door saw %s", got)
	}
}

// NIT P2-N2: a method the door's own rules might name exactly must reach it in canonical form only.
func TestMethodMustBeUpperCase(t *testing.T) {
	r := newRig(t)
	c, _ := net.Dial("tcp", strings.TrimPrefix(r.fwd.URL, "http://"))
	defer c.Close()
	c.Write([]byte("post /model/v1/messages HTTP/1.1\r\nHost: x\r\nContent-Length: 0\r\nConnection: close\r\n\r\n"))
	b, _ := io.ReadAll(c)
	if !strings.Contains(string(b), " 403 ") && !strings.Contains(string(b), " 400 ") {
		t.Fatalf("lower-case method relayed: %s", b)
	}
	if r.hits.Load() != 0 {
		t.Fatal("the door was reached")
	}
}

// NIT P2-N3: behind a broad rules file, ?action=approve on a write is still a decision.
func TestDecisionWordInQueryOfAWrite(t *testing.T) {
	r := newRig(t)
	du, _ := url.Parse(r.door.URL)
	f, err := New(Config{Token: r.tok, Key: r.gen, Doors: map[string]*url.URL{"model": du},
		Rules: []Rule{{Door: "model", Methods: []string{"GET", "POST"}, Path: "/v1/**"}}})
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer(f)
	defer s.Close()
	resp, _ := http.Post(s.URL+"/model/v1/x?action=approve", "application/json", strings.NewReader("{}"))
	if resp.StatusCode != 403 {
		t.Fatalf("write with a decision query: %d", resp.StatusCode)
	}
	resp, _ = http.Post(s.URL+"/model/v1/x?action=list", "application/json", strings.NewReader("{}"))
	if resp.StatusCode != 200 {
		t.Fatalf("allow twin: %d", resp.StatusCode)
	}
}

// context "the proof is bound to the body (U5-S1)": a large body arrives intact and verified.
func TestBodyIsHashedIntoTheProof(t *testing.T) {
	r := newRig(t)
	big := strings.Repeat("x", 3<<20)
	req, _ := http.NewRequest("POST", r.fwd.URL+"/model/v1/messages", strings.NewReader(big))
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
}
