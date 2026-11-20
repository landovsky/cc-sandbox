// Tests for relay-mint and the relay's revoke (docs/relay-mint-spec.md). The hub is a fake HTTP server on
// loopback that records what it is asked, "root" is a fake euid and the clock is fake. What is checked is
// the policy: only the service user, only the current running generation of this machine, what the relay
// may reach comes from the record and from nowhere else, the admin secret is read only after the record
// checks and never appears in any output, and the revoke happens when a generation ends.
package rootcmd

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/backend"
	"github.com/landovsky/cc-sandbox/internal/k8s"
	"github.com/landovsky/cc-sandbox/internal/start"
)

const (
	hubSecretText = "ADMIN-SECRET-VALUE-123"
	relayPub      = "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA" // 43 characters
)

type hubReq struct {
	method, path, rawPath, auth, body string
}

type relayFx struct {
	*fx
	hub    *httptest.Server
	mu     sync.Mutex
	reqs   []hubReq
	status int    // the answer to POST (default 201)
	token  string // sbp1.… in the 201
	audit  *strings.Builder
	clock  time.Time
	hang   chan struct{}
	redir  bool
	lis    func(port int) []TCPListener // who listens on the hub's port; default: the hub's user
}

const hubUID = 8181

func newRelayFx(t *testing.T) *relayFx {
	t.Helper()
	r := &relayFx{fx: newFx(t), status: 201, token: "sbp1.TOKENVALUE", audit: &strings.Builder{}, clock: time.Unix(1_800_000_000, 0)}
	r.hub = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		b, _ := io.ReadAll(q.Body)
		r.mu.Lock()
		r.reqs = append(r.reqs, hubReq{q.Method, q.URL.Path, q.URL.EscapedPath(), q.Header.Get("Authorization"), string(b)})
		r.mu.Unlock()
		if r.hang != nil {
			<-r.hang
		}
		if r.redir {
			http.Redirect(w, q, "/elsewhere", http.StatusFound)
			return
		}
		if q.Method == "POST" {
			w.WriteHeader(r.status)
			if r.status == 201 {
				fmt.Fprintf(w, `{"token":%q}`, r.token)
			} else {
				fmt.Fprint(w, `{"error":"BODY-OF-THE-REFUSAL"}`)
			}
			return
		}
		w.WriteHeader(204)
	}))
	t.Cleanup(r.hub.Close)
	put(t, filepath.Join(r.dir, "etc", "hub.secret"), hubSecretText+"\n", 0o600)
	r.reconfig("")
	r.env["SUDO_UID"] = fmt.Sprint(svcUID) // hostd
	r.running(2)
	return r
}

func (r *relayFx) reconfig(extra string) {
	r.writeConfig(fmt.Sprintf("hub_url = %q\nhub_secret = %q\nhub_user = \"hubuser\"\n%s", r.hub.URL, filepath.Join(r.dir, "etc", "hub.secret"), extra))
	r.d.Audit = r.audit
	r.d.Now = func() time.Time { return r.clock }
	l := r.d.LookupUser
	r.d.LookupUser = func(n string) (int, int, error) {
		if n == "hubuser" {
			return hubUID, hubUID, nil
		}
		return l(n)
	}
	r.d.HubListeners = func(port int) ([]TCPListener, error) {
		if r.lis != nil {
			return r.lis(port), nil
		}
		return []TCPListener{{IP: "0100007F", Port: port, UID: hubUID}}, nil
	}
}

func (r *relayFx) running(gen int) {
	d := filepath.Join(r.run, "s", sid, fmt.Sprintf("g%d", gen))
	mk(r.t, d, 0o755)
	put(r.t, filepath.Join(d, "PID"), "1234\n", 0o600)
}

func (r *relayFx) hubCalls() []hubReq {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]hubReq(nil), r.reqs...)
}

func (r *relayFx) mint(args ...string) (int, string) {
	return r.call(append([]string{"relay-mint"}, args...)...)
}

func (r *relayFx) refusedNoHub(why string, args ...string) {
	r.t.Helper()
	code, out := r.mint(args...)
	if code != 2 || len(r.hubCalls()) != 0 || strings.Contains(out, hubSecretText) {
		r.t.Errorf("%s: exit %d, hub calls %d: %s", why, code, len(r.hubCalls()), out)
	}
}

func TestRelayMintRegistersTheKeyWithTheManagerAsTheOnlyReach(t *testing.T) {
	r := newRelayFx(t)
	r.rec.Spec.MsgScope = json.RawMessage(`{"manager":{"node":"cc-sandbox:#6b3s","machine":"hp-ubuntu"},"peers":["cc-sandbox:#aaaa"],"peer_machines":{"cc-sandbox:#aaaa":"mac"}}`)
	r.writeRecord()
	code, out := r.mint("--", sid, "2", relayPub)
	if code != 0 || strings.TrimSpace(out) != `{"token":"sbp1.TOKENVALUE"}` {
		t.Fatalf("%d %q", code, out)
	}
	reqs := r.hubCalls()
	if len(reqs) != 1 || reqs[0].method != "POST" || reqs[0].path != "/v1/principals" || reqs[0].auth != "Bearer "+hubSecretText {
		t.Fatalf("%+v", reqs)
	}
	var body map[string]any
	json.Unmarshal([]byte(reqs[0].body), &body)
	reach, _ := body["may_reach"].([]any)
	if body["role"] != "relay" || body["sid"] != sid || body["gen"] != "2" || body["pub"] != relayPub || len(reach) != 2 || reach[0] != "hp-ubuntu/cc-sandbox:#6b3s" || reach[1] != "mac/cc-sandbox:#aaaa" || body["manager"] != "hp-ubuntu/cc-sandbox:#6b3s" || len(body) != 6 {
		t.Errorf("body %s (the relay reaches the manager, then the placed peers, each as <machine>/<node> from the record, and nobody else)", reqs[0].body)
	}
	if strings.Contains(r.audit.String(), "TOKENVALUE") || strings.Contains(r.audit.String(), hubSecretText) || !strings.Contains(r.audit.String(), "relay-mint") || !strings.Contains(r.audit.String(), "sid="+sid) {
		t.Errorf("audit: %q", r.audit.String())
	}
}

func TestMayReachComesFromTheRecordAndIsEmptyWithoutAManagerMachine(t *testing.T) {
	for why, c := range map[string]struct{ scope, want string }{
		"no manager machine":                          {`{"manager":{"node":"cc-sandbox:#6b3s"}}`, "[]"},
		"no manager at all":                           {`{}`, "[]"},
		"a machine with a slash":                      {`{"manager":{"node":"n","machine":"a/b"}}`, "[]"},
		"a node with a space":                         {`{"manager":{"node":"a b","machine":"mac"}}`, "[]"},
		"a node over 128 chars":                       {`{"manager":{"node":"` + strings.Repeat("n", 129) + `","machine":"mac"}}`, "[]"},
		"another machine's node":                      {`{"manager":{"node":"cc-sandbox:#6b3s","machine":"mac"}}`, `["mac/cc-sandbox:#6b3s"]`},
		"manager and peers, in that order":            {`{"manager":{"node":"m","machine":"hp"},"peers":["p1","p2"],"peer_machines":{"p1":"mac","p2":"future-two"}}`, `["hp/m","mac/p1","future-two/p2"]`},
		"a peer with no machine is left out":          {`{"manager":{"node":"m","machine":"hp"},"peers":["p1","p2"],"peer_machines":{"p2":"mac"}}`, `["hp/m","mac/p2"]`},
		"a peer with a malformed machine":             {`{"manager":{"node":"m","machine":"hp"},"peers":["p1"],"peer_machines":{"p1":"a/b"}}`, `["hp/m"]`},
		"a peer with a malformed node":                {`{"manager":{"node":"m","machine":"hp"},"peers":["p 1"],"peer_machines":{"p 1":"mac"}}`, `["hp/m"]`},
		"no manager machine, a placed peer":           {`{"manager":{"node":"m"},"peers":["p1"],"peer_machines":{"p1":"mac"}}`, `["mac/p1"]`},
		"the same node twice":                         {`{"manager":{"node":"m","machine":"hp"},"peers":["m","m"],"peer_machines":{"m":"hp"}}`, `["hp/m"]`},
		"a machine map for a node that is not a peer": {`{"manager":{"node":"m","machine":"hp"},"peers":[],"peer_machines":{"x":"mac"}}`, `["hp/m"]`},
	} {
		r := newRelayFx(t)
		r.rec.Spec.MsgScope = json.RawMessage(c.scope)
		r.writeRecord()
		if code, out := r.mint("--", sid, "2", relayPub); code != 0 {
			t.Fatalf("%s: %d %s", why, code, out)
		}
		var body struct {
			MayReach json.RawMessage `json:"may_reach"`
			Manager  *string         `json:"manager"`
		}
		json.Unmarshal([]byte(r.hubCalls()[0].body), &body)
		if string(body.MayReach) != c.want {
			t.Errorf("%s: may_reach %s, want %s", why, body.MayReach, c.want)
		}
		// the manager is its own field (never inferred from the order of may_reach): <machine>/<node>, or ""
		// when the record does not place it, even if a peer is placed
		wantMgr := ""
		var sc struct {
			Manager struct{ Node, Machine string }
		}
		json.Unmarshal([]byte(c.scope), &sc)
		if sc.Manager.Machine != "" && sc.Manager.Node != "" && strings.HasPrefix(c.want, `["`+sc.Manager.Machine+"/"+sc.Manager.Node+`"`) {
			wantMgr = sc.Manager.Machine + "/" + sc.Manager.Node
		}
		if body.Manager == nil || *body.Manager != wantMgr {
			t.Errorf("%s: manager %v, want %q", why, body.Manager, wantMgr)
		}
	}
}

func TestRelayMintRefusalsMakeNoHubCall(t *testing.T) {
	r := newRelayFx(t)
	r.refusedNoHub("no arguments")
	r.refusedNoHub("too few", "--", sid, "2")
	r.refusedNoHub("an extra argument", "--", sid, "2", relayPub, "x")
	r.refusedNoHub("a bad sid", "--", "../x", "2", relayPub)
	r.refusedNoHub("an upper-case sid", "--", "S0ABCDEFGH", "2", relayPub)
	for _, g := range []string{"0", "-1", "x", "02", "99999", "2 "} {
		r.refusedNoHub("generation "+g, "--", sid, g, relayPub)
	}
	for _, p := range []string{"short", relayPub + "A", strings.Repeat("!", 43), strings.Repeat("A", 43) + "="} {
		r.refusedNoHub("pub "+p, "--", sid, "2", p)
	}
	r.refusedNoHub("an unknown session", "--", "s0unknownxx", "2", relayPub)
	for _, g := range []string{"1", "3"} { // an older generation (whose directory may even be running) and a future one
		r.running(1)
		r.running(3)
		if code, out := r.mint("--", sid, g, relayPub); code != 2 || !strings.Contains(out, "stale_generation") || len(r.hubCalls()) != 0 {
			t.Errorf("generation %s: %d %s", g, code, out)
		}
	}
	// a caller-supplied may-reach, in any form, is not a thing: the flag does not exist and no hub is called
	for _, a := range [][]string{{"--may-reach", "mac/x", "--", sid, "2", relayPub}, {"--may-reach=mac/x", "--", sid, "2", relayPub}} {
		if code, _ := r.mint(a...); code == 0 || len(r.hubCalls()) != 0 {
			t.Errorf("%v: exit %d, hub calls %d", a, code, len(r.hubCalls()))
		}
	}
}

func TestRelayMintIsForTheServiceUserOnly(t *testing.T) {
	r := newRelayFx(t)
	r.env["SUDO_UID"] = fmt.Sprint(os.Getuid()) // the session's owner
	r.refusedNoHub("the owner", "--", sid, "2", relayPub)
	delete(r.env, "SUDO_UID") // root by login
	r.refusedNoHub("root by login", "--", sid, "2", relayPub)
	r = newRelayFx(t)
	r.writeConfig("verbs = [\"run\"]\n")
	r.env["SUDO_UID"] = fmt.Sprint(svcUID)
	r.refusedNoHub("the verb switched off", "--", sid, "2", relayPub)
}

func TestRelayMintNeedsARunningGenerationOfThisMachine(t *testing.T) {
	r := newRelayFx(t)
	os.Remove(filepath.Join(r.run, "s", sid, "g2", "PID"))
	r.refusedNoHub("no PID: a pane that never started", "--", sid, "2", relayPub)
	r = newRelayFx(t)
	put(t, filepath.Join(r.run, "s", sid, "g2", "CLOSED"), "closed\n", 0o600)
	r.refusedNoHub("a closed generation", "--", sid, "2", relayPub)
	r = newRelayFx(t)
	now := time.Now()
	r.rec.Ended = &now
	r.writeRecord()
	r.refusedNoHub("an ended record", "--", sid, "2", relayPub)
	r = newRelayFx(t)
	r.rec.Spec.Machine = "mac"
	r.writeRecord()
	r.refusedNoHub("another machine's session", "--", sid, "2", relayPub)
	r = newRelayFx(t)
	r.rec.Backend, r.rec.Spec.Machine, r.rec.PodUID = "k8s", "future-two", podUID
	r.writeRecord()
	r.refusedNoHub("a pod session (the hub is on hp)", "--", sid, "2", relayPub)
	r = newRelayFx(t)
	os.Chmod(filepath.Join(r.state, "sessions"), 0o777)
	r.refusedNoHub("a sessions directory others can write", "--", sid, "2", relayPub)
}

func TestRelayMintRateLimitAndAfter401(t *testing.T) {
	r := newRelayFx(t)
	if c, out := r.mint("--", sid, "2", relayPub); c != 0 {
		t.Fatal(out)
	}
	if c, out := r.mint("--", sid, "2", relayPub); c != 2 || !strings.Contains(out, "rate") || len(r.hubCalls()) != 1 {
		t.Errorf("a plain repeat inside the window: %d %s", c, out)
	}
	if c, out := r.mint("--after-401", "--", sid, "2", relayPub); c != 0 || len(r.hubCalls()) != 2 {
		t.Errorf("--after-401 allows ONE: %d %s", c, out)
	}
	if c, _ := r.mint("--after-401", "--", sid, "2", relayPub); c != 2 || len(r.hubCalls()) != 2 {
		t.Errorf("a second --after-401 inside the window: %d", c)
	}
	r.clock = r.clock.Add(11 * time.Second)
	if c, out := r.mint("--", sid, "2", relayPub); c != 0 || len(r.hubCalls()) != 3 {
		t.Errorf("after the window: %d %s", c, out)
	}
	// another generation has its own window
	r2 := newRelayFx(t)
	r2.mint("--", sid, "2", relayPub)
	r2.rec.Gen = 3
	r2.writeRecord()
	r2.running(3)
	if c, out := r2.mint("--", sid, "3", relayPub); c != 0 {
		t.Errorf("a new generation shares the old one's window: %d %s", c, out)
	}
}

func TestTheAdminSecretIsReadOnlyFromATrustedFileAndNeverShown(t *testing.T) {
	for why, mod := range map[string]func(r *relayFx){
		"missing":            func(r *relayFx) { os.Remove(filepath.Join(r.dir, "etc", "hub.secret")) },
		"readable by group":  func(r *relayFx) { os.Chmod(filepath.Join(r.dir, "etc", "hub.secret"), 0o640) },
		"readable by others": func(r *relayFx) { os.Chmod(filepath.Join(r.dir, "etc", "hub.secret"), 0o644) },
		"writable by group":  func(r *relayFx) { os.Chmod(filepath.Join(r.dir, "etc", "hub.secret"), 0o620) },
		"a symlink": func(r *relayFx) {
			p := filepath.Join(r.dir, "etc", "hub.secret")
			os.Rename(p, p+".real")
			os.Symlink(p+".real", p)
		},
		"a directory": func(r *relayFx) {
			p := filepath.Join(r.dir, "etc", "hub.secret")
			os.Remove(p)
			mk(r.t, p, 0o700)
		},
		"empty": func(r *relayFx) { put(r.t, filepath.Join(r.dir, "etc", "hub.secret"), "\n", 0o600) },
	} {
		r := newRelayFx(t)
		mod(r)
		code, out := r.mint("--", sid, "2", relayPub)
		if code != 3 || len(r.hubCalls()) != 0 || strings.Contains(out, hubSecretText) || strings.Contains(r.audit.String(), hubSecretText) {
			t.Errorf("%s: exit %d hub calls %d: %s", why, code, len(r.hubCalls()), out)
		}
	}
	// the secret is not read when the record checks fail: a refused request makes the missing secret moot
	r := newRelayFx(t)
	os.Remove(filepath.Join(r.dir, "etc", "hub.secret"))
	if code, _ := r.mint("--", sid, "1", relayPub); code != 2 {
		t.Errorf("a stale generation must be refused (2) before the secret is looked at, got %d", code)
	}
}

func TestTheHubsAnswersAreExitThreeWithTheStatusOnly(t *testing.T) {
	for _, st := range []int{401, 403, 422, 500} {
		r := newRelayFx(t)
		r.status = st
		code, out := r.mint("--", sid, "2", relayPub)
		if code != 3 || !strings.Contains(out, fmt.Sprint(st)) || strings.Contains(out, "BODY-OF-THE-REFUSAL") || strings.Contains(out, hubSecretText) {
			t.Errorf("hub %d: exit %d: %s", st, code, out)
		}
	}
	r := newRelayFx(t)
	r.token = ""
	if code, _ := r.mint("--", sid, "2", relayPub); code != 3 {
		t.Errorf("a 201 with no token: %d", code)
	}
	r = newRelayFx(t)
	r.redir = true
	if code, _ := r.mint("--", sid, "2", relayPub); code != 3 || len(r.hubCalls()) != 1 {
		t.Errorf("a redirect must not be followed (the secret would go with it): exit %d, requests %d", code, len(r.hubCalls()))
	}
	r = newRelayFx(t)
	r.hub.Close()
	if code, out := r.mint("--", sid, "2", relayPub); code != 3 || strings.Contains(out, hubSecretText) {
		t.Errorf("a hub that is down: %d %s", code, out)
	}
	r = newRelayFx(t)
	r.hang = make(chan struct{})
	r.d.Hub = &http.Client{Timeout: 200 * time.Millisecond}
	begin := time.Now()
	code, _ := r.mint("--", sid, "2", relayPub)
	close(r.hang)
	if code != 3 || time.Since(begin) > 5*time.Second {
		t.Errorf("a hub that hangs: exit %d after %s", code, time.Since(begin))
	}
}

func TestTheHubMustBeOnLoopback(t *testing.T) {
	r := newRelayFx(t)
	p := filepath.Join(r.dir, "etc", "c.toml")
	base := "allowed_roots = [\"" + r.roots + "\"]\nmachine = \"m\"\n"
	for why, u := range map[string]string{
		"a remote host": "http://10.0.0.5:4011", "a name": "http://hub.example:4011", "https": "https://127.0.0.1:4011",
		"userinfo": "http://u:p@127.0.0.1:4011", "a path": "http://127.0.0.1:4011/x", "none": "",
	} {
		put(t, p, base+"hub_url = \""+u+"\"\n", 0o644)
		if _, err := LoadConfig(p, os.Getuid()); err == nil {
			t.Errorf("%s was accepted as the hub", why)
		}
	}
	for _, u := range []string{"http://127.0.0.1:4011", "http://localhost:4011", "http://[::1]:4011"} {
		put(t, p, base+"hub_url = \""+u+"\"\n", 0o644)
		if _, err := LoadConfig(p, os.Getuid()); err != nil {
			t.Errorf("%s: %v", u, err)
		}
	}
}

// ---- revoke ----

func (r *relayFx) deletes() []hubReq {
	var out []hubReq
	for _, q := range r.hubCalls() {
		if q.method == "DELETE" {
			out = append(out, q)
		}
	}
	return out
}

func TestTheRelayIsRevokedWhenAGenerationEnds(t *testing.T) {
	want := "/v1/principals/relay:" + sid + "%2F2"
	// run's cleanup
	r := newRelayFx(t)
	r.env["SUDO_UID"] = fmt.Sprint(os.Getuid())
	if code, out := r.call("run", sid); code != 7 {
		t.Fatalf("run: %d %s", code, out)
	}
	if d := r.deletes(); len(d) != 1 || d[0].rawPath != want || d[0].auth != "Bearer "+hubSecretText {
		t.Errorf("run: %+v", d)
	}
	// stop
	r = newRelayFx(t)
	if code, out := r.call("stop", sid); code != 0 {
		t.Fatalf("stop: %d %s", code, out)
	}
	if d := r.deletes(); len(d) != 1 || d[0].rawPath != want {
		t.Errorf("stop: %+v", d)
	}
	// gc: one revoke per reaped generation
	r = newRelayFx(t)
	r.fb.orphans = []backend.Orphan{{Kind: "container", Name: "ccsbx-" + sid + "-g2", Sid: sid, Gen: 2}, {Kind: "network", Name: "ccsbx-net-" + sid + "-g2", Sid: sid, Gen: 2}}
	if code, out := r.call("gc"); code != 0 {
		t.Fatalf("gc: %d %s", code, out)
	}
	if d := r.deletes(); len(d) != 1 || d[0].rawPath != want {
		t.Errorf("gc: %+v", d)
	}
	// a dry run revokes nothing
	r = newRelayFx(t)
	r.fb.orphans = []backend.Orphan{{Kind: "container", Name: "ccsbx-" + sid + "-g2", Sid: sid, Gen: 2}}
	r.call("gc", "--dry-run")
	if len(r.deletes()) != 0 {
		t.Error("gc --dry-run revoked a relay")
	}
}

func TestARevokeThatCannotBeMadeNeverFailsTheVerb(t *testing.T) {
	r := newRelayFx(t)
	r.hub.Close()
	if code, out := r.call("stop", sid); code != 0 {
		t.Errorf("stop with the hub down: %d %s", code, out)
	}
	if !strings.Contains(r.audit.String(), "relay-revoke") || !strings.Contains(r.audit.String(), "unreachable") {
		t.Errorf("audit: %q", r.audit.String())
	}
	r = newRelayFx(t)
	os.Remove(filepath.Join(r.dir, "etc", "hub.secret"))
	if code, out := r.call("stop", sid); code != 0 || len(r.hubCalls()) != 0 {
		t.Errorf("stop with no secret: %d %s (hub calls %d)", code, out, len(r.hubCalls()))
	}
	// a record that is not the one asked about never reaches the hub
	r = newRelayFx(t)
	r.env["SUDO_UID"] = fmt.Sprint(os.Getuid() + 7)
	r.call("stop", sid)
	if len(r.hubCalls()) != 0 {
		t.Error("a refused stop revoked a relay")
	}
}

var _ = start.ValidSid

// ---- the listener check (R2-S3) ----

// The hub is plain HTTP on loopback: with it down, any local user could bind its port and be handed the
// admin bearer. The secret is sent only if every LISTEN socket on that port belongs to the hub's user.
func TestTheAdminSecretGoesOnlyToTheHubsUser(t *testing.T) {
	cases := map[string]func(port int) []TCPListener{
		"another uid listens (a squatter)":  func(p int) []TCPListener { return []TCPListener{{Port: p, UID: os.Getuid() + 7}} },
		"hostd's uid listens":               func(p int) []TCPListener { return []TCPListener{{Port: p, UID: svcUID}} },
		"root listens (not the hub's user)": func(p int) []TCPListener { return []TCPListener{{Port: p, UID: 0}} },
		"the hub and a squatter both listen": func(p int) []TCPListener {
			return []TCPListener{{Port: p, UID: hubUID}, {IP: "00000000", Port: p, UID: svcUID}}
		},
		"nothing listens (the hub is down)": func(p int) []TCPListener { return nil },
	}
	for why, lis := range cases {
		r := newRelayFx(t)
		r.lis = lis
		code, out := r.mint("--", sid, "2", relayPub)
		if code != 3 || len(r.hubCalls()) != 0 || strings.Contains(out, hubSecretText) || !strings.Contains(r.audit.String(), "refused") && !strings.Contains(out, "refusing") {
			t.Errorf("%s: exit %d hub calls %d: %s", why, code, len(r.hubCalls()), out)
		}
	}
	// the revoke at the end of a generation is held to the same rule: no request, and the verb still succeeds
	r := newRelayFx(t)
	r.lis = cases["another uid listens (a squatter)"]
	if code, out := r.call("stop", sid); code != 0 || len(r.hubCalls()) != 0 || !strings.Contains(r.audit.String(), "relay-revoke") {
		t.Errorf("stop with a squatter: %d %s (hub calls %d)", code, out, len(r.hubCalls()))
	}
	// no hub_user in the config: nothing is sent either
	r = newRelayFx(t)
	r.writeConfig(fmt.Sprintf("hub_url = %q\nhub_secret = %q\n", r.hub.URL, filepath.Join(r.dir, "etc", "hub.secret")))
	r.env["SUDO_UID"] = fmt.Sprint(svcUID)
	if code, _ := r.mint("--", sid, "2", relayPub); code != 3 || len(r.hubCalls()) != 0 {
		t.Errorf("no hub_user: exit %d hub calls %d", code, len(r.hubCalls()))
	}
	// the hub's own user, alone on the port: allowed (the allow twin of every case above)
	r = newRelayFx(t)
	if code, out := r.mint("--", sid, "2", relayPub); code != 0 {
		t.Errorf("the hub's user: %d %s", code, out)
	}
}

func TestProcNetParsing(t *testing.T) {
	tcp := `  sl  local_address rem_address   st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 0100007F:0FAB 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1000        0 11111 1 0000000000000000 100 0 0 10 0
   1: 0100007F:0FAB 0100007F:9C40 01 00000000:00000000 00:00000000 00000000  1000        0 22222 1 0000000000000000 100 0 0 10 0
   2: 00000000:0FAB 00000000:0000 0A 00000000:00000000 00:00000000 00000000  1234        0 33333 1 0000000000000000 100 0 0 10 0
   3: 0100007F:1F90 00000000:0000 0A 00000000:00000000 00:00000000 00000000  7777        0 44444 1 0000000000000000 100 0 0 10 0
   4: garbage
`
	tcp6 := `  sl  local_address                         remote_address                        st tx_queue rx_queue tr tm->when retrnsmt   uid  timeout inode
   0: 00000000000000000000000000000000:0FAB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  2222        0 55555 1 0000000000000000 100 0 0 10 0
   1: 0000000000000000FFFF00000100007F:0FAB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  3333        0 66666 1 0000000000000000 100 0 0 10 0
   2: 00000000000000000000000001000000:0FAB 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  4444        0 77777 1 0000000000000000 100 0 0 10 0
   3: 00000000000000000000000001000000:1F90 00000000000000000000000000000000:0000 0A 00000000:00000000 00:00000000 00000000  5555        0 88888 1 0000000000000000 100 0 0 10 0
`
	// IPv6 wildcard, a v4-mapped loopback and ::1 are all listeners on the port, whoever holds them
	if got := parseProcNet(strings.NewReader(tcp6), 0x0FAB); len(got) != 3 || got[0].UID != 2222 || got[1].UID != 3333 || got[2].UID != 4444 {
		t.Errorf("tcp6: %+v", got)
	}
	// a header-only file (no sockets at all) and an empty one hold no listener
	hdr := tcp[:strings.Index(tcp, "\n")+1]
	if len(parseProcNet(strings.NewReader(hdr), 0x0FAB)) != 0 {
		t.Error("a header-only file has listeners")
	}
	got := parseProcNet(strings.NewReader(tcp), 0x0FAB) // 4011
	if len(got) != 2 || got[0].UID != 1000 || got[1].UID != 1234 || got[1].IP != "00000000" {
		t.Errorf("%+v (only LISTEN sockets on the port: an established connection and another port are not listeners)", got)
	}
	if len(parseProcNet(strings.NewReader(""), 80)) != 0 || len(parseProcNet(strings.NewReader(tcp), 1)) != 0 {
		t.Error("nothing was expected")
	}
}

// the hub's list holds at most 32 entries
func TestMayReachIsCutAtTheHubsLimit(t *testing.T) {
	peers, machines := []string{}, map[string]string{}
	for i := 0; i < 40; i++ {
		n := fmt.Sprintf("p%02d", i)
		peers = append(peers, n)
		machines[n] = "mac"
	}
	b, _ := json.Marshal(map[string]any{"manager": map[string]string{"node": "m", "machine": "hp"}, "peers": peers, "peer_machines": machines})
	d, err := k8s.ParseScope(b)
	if err != nil {
		t.Fatal(err)
	}
	got := mayReach(d)
	if len(got) != 32 || got[0] != "hp/m" || got[31] != "mac/p30" {
		t.Errorf("%d entries, first %q last %q", len(got), got[0], got[len(got)-1])
	}
}

// The docker relay-mint reads the record's scope as strictly as pod-create does: a scope the sidecar's flag
// would be refused for (duplicate or wrong-case keys, an unknown key, null, trailing data) gets no relay.
func TestRelayMintReadsTheScopeAsStrictlyAsPodCreate(t *testing.T) {
	for why, scope := range map[string]string{
		"null":                       `null`,
		"a duplicate manager":        `{"manager":{"node":"a","machine":"hp"},"manager":{"node":"b","machine":"mac"}}`,
		"a wrong-case key":           `{"Manager":{"node":"a","machine":"hp"}}`,
		"an unknown key":             `{"manager":{"node":"a","machine":"hp"},"extra":1}`,
		"a duplicate key in manager": `{"manager":{"node":"a","node":"b","machine":"hp"}}`,
		"a duplicate peer machine":   `{"manager":{"node":"a","machine":"hp"},"peers":["p"],"peer_machines":{"p":"mac","p":"hp"}}`,
	} {
		r := newRelayFx(t)
		r.rec.Spec.MsgScope = json.RawMessage(scope)
		r.writeRecord()
		code, out := r.mint("--", sid, "2", relayPub)
		if code != 2 || len(r.hubCalls()) != 0 || !strings.Contains(out, "not well formed") {
			t.Errorf("%s: exit %d hub calls %d: %s", why, code, len(r.hubCalls()), out)
		}
	}
}
