// Tests of the remote-start routes (M2c): who may call them, what they answer, and that every refusal has the
// contract's one shape ({"refused":<code>,"message":...}) with the right status, so cc-tree's client can rely on it.
package daemonapi

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/profile"
	"github.com/landovsky/cc-sandbox/internal/remotestart"
	"github.com/landovsky/cc-sandbox/internal/start"
	"github.com/landovsky/cc-sandbox/internal/verbq"
	"github.com/landovsky/cc-sandbox/internal/whois"
)

func remoteRig(t *testing.T, starter func() (*start.StartReply, error), live []remotestart.View) (http.Handler, *Server) {
	t.Helper()
	j, _ := journal.Open(t.TempDir(), "hp-ubuntu", 0)
	q, _ := verbq.Open(t.TempDir())
	t.Cleanup(func() { j.Close(); q.Close() })
	prof := &profile.Profile{Name: "dev-default", Machines: []string{"hp-ubuntu"}, Billing: []string{"api"}}
	svc, err := remotestart.New(remotestart.Config{
		Machine: "hp-ubuntu", OwnerUser: "tomas", PeerSet: "S", StateDir: t.TempDir(),
		Profiles:      func(string) (*profile.Profile, error) { return prof, nil },
		LookupUID:     func(string) (int, error) { return 1000, nil },
		Starter:       func(int, []byte, string) (*start.StartReply, error) { return starter() },
		Live:          func(string) ([]remotestart.View, error) { return live, nil },
		RemoteEnabled: true,
		PerUserMinute: 3,
	})
	if err != nil {
		t.Fatal(err)
	}
	s := &Server{J: j, Q: q, Info: map[string]any{"machine": "hp-ubuntu"}, Remote: svc}
	who := &fakeWho{ids: map[string]whois.Identity{
		"100.64.0.1": id("tomas@example.com", "mac-tomas"),         // the owner
		"100.64.0.4": id("tagged-devices", "future-two", hostdTag), // a peer daemon
	}}
	return s.TailnetHandler(who, cc), s
}

const goodSpec = `{"v":1,"request_id":"r1","owner":"tomas","machine":"hp-ubuntu","label":"cc-sandbox:w1","harness":"claude","workdir":{"path":"/x"},"profile":"dev-default","billing":"api"}`

func rpost(h http.Handler, from, path, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest("POST", path, strings.NewReader(body))
	req.RemoteAddr, req.Host = net.JoinHostPort(from, "50000"), "hp-ubuntu.example.ts.net:7480"
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

func refused(t *testing.T, rec *httptest.ResponseRecorder, status int, code string) map[string]any {
	t.Helper()
	var m map[string]any
	json.Unmarshal(rec.Body.Bytes(), &m)
	if rec.Code != status || m["refused"] != code {
		t.Fatalf("%d %s, want %d %s", rec.Code, rec.Body.String(), status, code)
	}
	if _, ok := m["message"].(string); !ok {
		t.Fatalf("a refusal without a message: %s", rec.Body.String())
	}
	return m
}

// context (C1): only the owner starts remotely; a peer daemon cannot, and the answer has the one refusal shape.
func TestOnlyTheOwnerStartsASessionRemotely(t *testing.T) {
	h, _ := remoteRig(t, func() (*start.StartReply, error) { return &start.StartReply{V: 1, Sid: "s0new", Gen: 1}, nil }, nil)
	rec := rpost(h, "100.64.0.1", "/v1/sessions", goodSpec)
	var rep start.StartReply
	json.Unmarshal(rec.Body.Bytes(), &rep)
	if rec.Code != 200 || rep.Sid != "s0new" {
		t.Fatalf("the owner: %d %s", rec.Code, rec.Body.String())
	}
	refused(t, rpost(h, "100.64.0.4", "/v1/sessions", goodSpec), 403, "forbidden") // a peer daemon
	refused(t, rpost(h, "100.64.9.9", "/v1/sessions", goodSpec), 403, "forbidden") // nobody Tailscale knows
}

// context: the local unix socket starts sessions through /v1/start as the caller's own uid; POST /v1/sessions there
// would be a start with no caller identity, so it is refused.
func TestPostSessionsIsNotAnotherWayToStartLocally(t *testing.T) {
	_, s := remoteRig(t, func() (*start.StartReply, error) { return &start.StartReply{}, nil }, nil)
	req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(goodSpec))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	refused(t, rec, 403, "forbidden")
}

// context (C8): the statuses of the contract table, each with its refusal shape and, where it says when to come
// back, a Retry-After header.
func TestRefusalsHaveTheContractsStatusAndShape(t *testing.T) {
	cases := map[string]struct {
		err    error
		status int
		code   string
		retry  bool
	}{
		"already running": {&start.Refusal{Code: start.AlreadyRunning, Msg: "x", Machine: "hp-ubuntu", Sid: "s0a"}, 409, "already_running", false},
		"machine full":    {&start.Refusal{Code: start.MachineFull, Msg: "x"}, 503, "machine_full", false},
		"not built":       {&start.Refusal{Code: start.NotBuilt, Msg: "x"}, 422, "not_built", false},
		"profile":         {&start.Refusal{Code: start.ProfileNotApproved, Msg: "x"}, 403, "profile_not_approved", false},
		"in progress":     {&start.Refusal{Code: start.StartInProgress, Msg: "x", RetryAfter: 3}, 409, "start_in_progress", true},
		"rate capped":     {&start.Refusal{Code: start.RateCapped, Msg: "x", RetryAfter: 7}, 429, "rate_capped", true},
		"a plain failure": {errors.New("docker exploded"), 500, "internal", false},
	}
	for name, c := range cases {
		h, _ := remoteRig(t, func() (*start.StartReply, error) { return nil, c.err }, nil)
		rec := rpost(h, "100.64.0.1", "/v1/sessions", goodSpec)
		m := refused(t, rec, c.status, c.code)
		if c.retry && rec.Header().Get("Retry-After") == "" {
			t.Errorf("%s: no Retry-After", name)
		}
		if c.code == "already_running" && (m["sid"] != "s0a" || m["machine"] != "hp-ubuntu") {
			t.Errorf("%s: the client cannot reattach: %v", name, m)
		}
		if strings.Contains(rec.Body.String(), "docker exploded") {
			t.Errorf("%s: an internal error text reached the caller", name)
		}
	}
}

// context (C8): the refusals that happen before the caller is known (a form POST, a browser's Origin, a rebound
// Host) have the same shape, so the client has one thing to parse.
func TestTheTailnetsOwnRefusalsHaveTheSameShape(t *testing.T) {
	h, _ := remoteRig(t, func() (*start.StartReply, error) { return &start.StartReply{}, nil }, nil)
	req := httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(goodSpec))
	req.RemoteAddr, req.Host = "100.64.0.1:50000", "hp-ubuntu.example.ts.net:7480"
	req.Header.Set("Content-Type", "text/plain")
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	refused(t, rec, 415, "unsupported_media_type")
	req = httptest.NewRequest("POST", "/v1/sessions", strings.NewReader(goodSpec))
	req.RemoteAddr, req.Host = "100.64.0.1:50000", "hp-ubuntu.example.ts.net:7480"
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://evil.example")
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	refused(t, rec, 403, "forbidden")
}

// context (C3): another daemon asks whether a label is live or being started here; it answers for peers and the
// owner, never for a stranger, and never for a malformed label.
func TestAPeerCanAskWhetherALabelIsLiveHere(t *testing.T) {
	h, _ := remoteRig(t, func() (*start.StartReply, error) { return &start.StartReply{}, nil },
		[]remotestart.View{{Machine: "hp-ubuntu", Sid: "s0live", Gen: 1, State: "running"}})
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/resolve?v=2&label=cc-sandbox:w1", nil)
	req.RemoteAddr, req.Host = "100.64.0.4:50000", "hp-ubuntu.example.ts.net:7480"
	h.ServeHTTP(rec, req)
	var a remotestart.Answer
	json.Unmarshal(rec.Body.Bytes(), &a)
	if rec.Code != 200 || len(a.Live) != 1 || a.Live[0].Sid != "s0live" || a.PeerSet != "S" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	for _, c := range []struct {
		from, path string
		status     int
	}{{"100.64.0.4", "/v1/resolve?v=2&label=bad%20label", 400}, {"100.64.9.9", "/v1/resolve?v=2&label=cc-sandbox:w1", 403}} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest("GET", c.path, nil)
		req.RemoteAddr, req.Host = c.from+":50000", "hp-ubuntu.example.ts.net:7480"
		h.ServeHTTP(rec, req)
		refused(t, rec, c.status, map[int]string{400: "bad_spec", 403: "forbidden"}[c.status])
	}
}

// context (R2-S1): one node must not be able to fill the listener's connection cap and starve every other one.
func TestOneSourceCannotUseUpTheConnectionCap(t *testing.T) {
	base, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l := &limitListener{Listener: base, sem: make(chan struct{}, 10), perSrc: 2, bySrc: map[string]int{}}
	defer l.Close()
	acc := make(chan net.Conn, 10)
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			acc <- c
		}
	}()
	var cs []net.Conn
	for i := 0; i < 5; i++ {
		c, err := net.Dial("tcp", base.Addr().String())
		if err != nil {
			t.Fatal(err)
		}
		defer c.Close()
		cs = append(cs, c)
	}
	time.Sleep(300 * time.Millisecond)
	if len(acc) != 2 {
		t.Fatalf("%d connections accepted from one source, want 2", len(acc))
	}
	dropped := 0
	for _, c := range cs {
		c.SetReadDeadline(time.Now().Add(200 * time.Millisecond))
		if _, err := c.Read(make([]byte, 1)); err != nil && !strings.Contains(err.Error(), "timeout") {
			dropped++
		}
	}
	if dropped != 3 {
		t.Fatalf("%d of the 5 were closed by the listener, want 3", dropped)
	}
	// closing one frees a slot for that source
	(<-acc).Close()
	time.Sleep(100 * time.Millisecond)
	c, _ := net.Dial("tcp", base.Addr().String())
	defer c.Close()
	select {
	case <-acc:
	case <-time.After(time.Second):
		t.Fatal("a closed connection did not free its slot")
	}
	_ = context.Background
}
