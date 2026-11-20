// Tests of the daemon API. Why: this is the one door cc-tree and the owner CLI use, so each case
// names a property of it (replay is exact, stale epochs are told so, token holders never get owner
// operations, replace never touches a mid-turn pane).

package daemonapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/verbq"
)

type rig struct {
	j   *journal.Store
	srv *httptest.Server
}

func newRig(t *testing.T) *rig {
	j, err := journal.Open(t.TempDir(), "hp-ubuntu", 0)
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer((&Server{J: j, MaxWait: 5 * time.Second}).Handler())
	t.Cleanup(func() { s.Close(); j.Close() })
	return &rig{j, s}
}

func (r *rig) add(typ string) {
	r.j.Append(journal.Event{Sid: "s1", Gen: 1, Realm: "production", Src: "host", Type: typ})
}

func (r *rig) get(t *testing.T, path string) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(r.srv.URL + path)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

func evTypes(m map[string]any) string {
	var o []string
	for _, e := range m["events"].([]any) {
		o = append(o, e.(map[string]any)["type"].(string))
	}
	return strings.Join(o, ",")
}

// context: the cockpit consumer was stopped at its cursor; a session ran to done meanwhile.
// curl .../v1/events?after=<last> must return exactly the missed events (M3's acceptance).
func TestReplayOverHTTPReturnsExactlyTheMissedEvents(t *testing.T) {
	r := newRig(t)
	r.add("session.started")
	r.add("hook.Stop")
	_, first := r.get(t, "/v1/events")
	last := int64(first["head"].(float64))
	for _, ty := range []string{"hook.UserPromptSubmit", "hook.Stop", "session.ended"} {
		r.add(ty)
	}
	code, m := r.get(t, fmt.Sprintf("/v1/events?after=%d&epoch=%s", last, r.j.Epoch()))
	if code != 200 || evTypes(m) != "hook.UserPromptSubmit,hook.Stop,session.ended" {
		t.Fatalf("%d %v", code, m)
	}
}

// context: a consumer from before a journal wipe must get a clear signal, not silence.
func TestStaleEpochGets409WithTheCurrentEpoch(t *testing.T) {
	r := newRig(t)
	r.add("x")
	code, m := r.get(t, "/v1/events?after=1&epoch=eOLD")
	if code != 409 || m["error"] != "epoch_changed" || m["epoch"] != r.j.Epoch() {
		t.Fatalf("%d %v", code, m)
	}
}

func TestMalformedQueriesAre400(t *testing.T) {
	r := newRig(t)
	for _, q := range []string{"after=-1", "after=x", "limit=-5", "wait=abc"} {
		if code, _ := r.get(t, "/v1/events?"+q); code != 400 {
			t.Errorf("%s -> %d", q, code)
		}
	}
	if code, _ := r.get(t, "/v1/events?after=99"); code != 400 {
		t.Errorf("cursor past head -> %d", code)
	}
}

// context: a cockpit that is up waits on the daemon instead of polling.
func TestLongPollWakesOnAppend(t *testing.T) {
	r := newRig(t)
	go func() { time.Sleep(150 * time.Millisecond); r.add("hook.Stop") }()
	start := time.Now()
	code, m := r.get(t, "/v1/events?after=0&wait=3")
	if code != 200 || evTypes(m) != "hook.Stop" || time.Since(start) > 2*time.Second {
		t.Fatalf("%d %v after %v", code, m, time.Since(start))
	}
}

func TestSSEStreamsReplayThenLive(t *testing.T) {
	r := newRig(t)
	r.add("session.started")
	resp, err := http.Get(r.srv.URL + "/v1/events/stream?after=0")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatal(ct)
	}
	sc := bufio.NewScanner(resp.Body)
	read := func() string {
		for sc.Scan() {
			if strings.HasPrefix(sc.Text(), "id: ") {
				return strings.TrimPrefix(sc.Text(), "id: ")
			}
		}
		return ""
	}
	if got := read(); got != "1" {
		t.Fatalf("replayed id %q", got)
	}
	r.add("hook.Stop")
	if got := read(); got != "2" {
		t.Fatalf("live id %q", got)
	}
}

func TestSSEWithStaleEpochIsRefusedBeforeStreaming(t *testing.T) {
	r := newRig(t)
	resp, err := http.Get(r.srv.URL + "/v1/events/stream?after=0&epoch=eOLD")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 409 {
		t.Fatal(resp.StatusCode)
	}
}

func post(t *testing.T, r *rig, name string, body string) (int, map[string]any) {
	resp, err := http.Post(r.srv.URL+"/v1/cursors/"+name, "application/json", bytes.NewBufferString(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// context: the e2e realm's cockpit advances its own cursor; production's is untouched, and a
// cursor without its tree commit is refused.
func TestCursorsPerRealmNeedTheirTreeCommit(t *testing.T) {
	r := newRig(t)
	r.add("a")
	r.add("b")
	ep := r.j.Epoch()
	if code, _ := post(t, r, "production", fmt.Sprintf(`{"seq":1,"epoch":%q}`, ep)); code != 400 {
		t.Fatalf("no sha: %d", code)
	}
	if code, _ := post(t, r, "e2e", fmt.Sprintf(`{"seq":2,"epoch":%q,"tree_sha":"deadbeef01"}`, ep)); code != 200 {
		t.Fatalf("e2e: %d", code)
	}
	if code, _ := r.get(t, "/v1/cursors/production"); code != 404 {
		t.Fatalf("production cursor moved by the e2e realm: %d", code)
	}
	code, m := r.get(t, "/v1/cursors/e2e")
	if code != 200 || m["tree_sha"] != "deadbeef01" || m["seq"].(float64) != 2 {
		t.Fatalf("%d %v", code, m)
	}
}

func TestLoopbackOnlyUntilTheTailnetListenerExists(t *testing.T) {
	if _, err := ListenLoopback("0.0.0.0:0"); err == nil {
		t.Fatal("bound a non-loopback address")
	}
	l, err := ListenLoopback("127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	l.Close()
}

// ---- M4: cockpit lease and verb queue over HTTP --------------------------------------------

type qrig struct {
	*rig
	q *verbq.Queue
}

func newQRig(t *testing.T) *qrig {
	j, _ := journal.Open(t.TempDir(), "hp-ubuntu", 0)
	q, err := verbq.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	s := httptest.NewServer((&Server{J: j, Q: q}).Handler())
	t.Cleanup(func() { s.Close(); j.Close(); q.Close() })
	return &qrig{&rig{j, s}, q}
}

func (r *qrig) do(t *testing.T, method, path, token, body string) (int, map[string]any) {
	req, _ := http.NewRequest(method, r.srv.URL+path, bytes.NewBufferString(body))
	if token != "" {
		req.Header.Set("X-Lease", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// context: "no cockpit → done returns queued; a second lease in the same realm → 409; the e2e
// realm leaves production alone" — M4's acceptance, over the wire.
func TestCockpitLeaseAndQueueOverHTTP(t *testing.T) {
	r := newQRig(t)
	r.q.Enqueue("production", verbq.Item{ID: "i1", Sid: "s1", Gen: 1, Rid: "a", Type: "ccf.done"})
	if code, m := r.do(t, "GET", "/v1/cockpit/status?realm=production", "", ""); code != 200 || m["pending"].(float64) != 1 || m["lease"] != nil {
		t.Fatalf("status without a cockpit: %d %v", code, m)
	}
	code, m := r.do(t, "POST", "/v1/cockpit/lease?realm=production", "", `{"holder":"hp"}`)
	if code != 200 || m["token"] == "" || m["ttl_s"].(float64) != 90 {
		t.Fatalf("%d %v", code, m)
	}
	tok := m["token"].(string)
	code, m = r.do(t, "POST", "/v1/cockpit/lease?realm=production", "", `{"holder":"mac"}`)
	if code != 409 || m["holder"] != "hp" || m["token"] != nil {
		t.Fatalf("second lease: %d %v (the holder's token must not leak)", code, m)
	}
	if code, _ := r.do(t, "POST", "/v1/cockpit/lease?realm=e2e", "", `{"holder":"mac"}`); code != 200 {
		t.Fatalf("e2e realm: %d", code)
	}
	if code, _ := r.do(t, "GET", "/v1/cockpit/requests?realm=production", "wrong", ""); code != 409 {
		t.Fatalf("wrong token: %d", code)
	}
	code, m = r.do(t, "GET", "/v1/cockpit/requests?realm=production", tok, "")
	items := m["items"].([]any)
	if code != 200 || len(items) != 1 {
		t.Fatalf("%d %v", code, m)
	}
	if code, m = r.do(t, "POST", "/v1/cockpit/requests/i1/result?realm=production", tok, `{"status":"transient","code":"RepoBusy"}`); code != 200 || m["applied"] != false {
		t.Fatalf("%d %v", code, m)
	}
	if code, m = r.do(t, "GET", "/v1/cockpit/status?realm=production", "", ""); m["warnings"].([]any)[0].(map[string]any)["last_error"] != "RepoBusy" {
		t.Fatalf("%d %v", code, m)
	}
	if code, m = r.do(t, "POST", "/v1/cockpit/requests/i1/result?realm=production", tok, `{"status":"ok"}`); code != 200 || m["applied"] != true {
		t.Fatalf("%d %v", code, m)
	}
	if code, _ = r.do(t, "POST", "/v1/cockpit/lease?realm=production&release=1", "", fmt.Sprintf(`{"token":%q}`, tok)); code != 200 {
		t.Fatalf("release %d", code)
	}
	if code, _ := r.do(t, "POST", "/v1/cockpit/lease?realm=production", "", `{"holder":"mac"}`); code != 200 {
		t.Fatalf("after release: %d", code)
	}
}

func TestCockpitEndpointsNeedARealm(t *testing.T) {
	r := newQRig(t)
	if code, _ := r.do(t, "GET", "/v1/cockpit/status", "", ""); code != 400 {
		t.Fatal(code)
	}
	if code, _ := r.do(t, "GET", "/v1/cockpit/status?realm=..%2Fx", "", ""); code != 400 {
		t.Fatal(code)
	}
}

// ---- caller classes: the unix socket is the owner, the TCP port is a token holder ------------

type fakeDirT struct{ s []msg.Session }

func (f fakeDirT) Sessions() ([]msg.Session, error) { return f.s, nil }

type nopOut struct{ lines []string }

func (n *nopOut) Deliver(sid, line string) error {
	n.lines = append(n.lines, sid+": "+line)
	return nil
}

type idleT struct{ idle bool }

func (i idleT) IdleSince(string, int) (time.Time, bool) { return time.Now().Add(-time.Hour), i.idle }

func newAuthRig(t *testing.T, idle bool) (local, tcp *httptest.Server, out *nopOut) {
	j, _ := journal.Open(t.TempDir(), "hp-ubuntu", 0)
	q, _ := verbq.Open(t.TempDir())
	dir := fakeDirT{[]msg.Session{{Sid: "sM", Label: "cc-sandbox:mgr", Realm: "production", Live: true, Gen: 1}}}
	out = &nopOut{}
	rt := &msg.Router{Dir: dir, Out: out, Idle: idleT{idle}, StateDir: t.TempDir()}
	s := &Server{J: j, Q: q, Msg: rt, Dir: dir, Token: "0123456789abcdef-token"}
	local, tcp = httptest.NewServer(s.Handler()), httptest.NewServer(s.TCPHandler())
	t.Cleanup(func() { local.Close(); tcp.Close(); j.Close(); q.Close() })
	return
}

func call(t *testing.T, method, url, token, body string) (int, map[string]any) {
	req, _ := http.NewRequest(method, url, bytes.NewBufferString(body))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	json.NewDecoder(resp.Body).Decode(&m)
	return resp.StatusCode, m
}

// context: any local process can reach 127.0.0.1. Without the token the TCP port gives a
// stranger neither the journal (every session's fence text) nor the cockpit lease.
func TestTCPListenerNeedsTheTokenAndNeverServesOwnerOperations(t *testing.T) {
	local, tcp, out := newAuthRig(t, true)
	if code, _ := call(t, "GET", tcp.URL+"/v1/events", "", ""); code != 401 {
		t.Fatalf("no token: %d", code)
	}
	if code, _ := call(t, "GET", tcp.URL+"/v1/events", "wrong-token-wrong-token", ""); code != 401 {
		t.Fatalf("wrong token: %d", code)
	}
	if code, _ := call(t, "GET", tcp.URL+"/v1/events", "0123456789abcdef-token", ""); code != 200 {
		t.Fatalf("right token: %d", code)
	}
	if code, _ := call(t, "POST", tcp.URL+"/v1/cockpit/lease?realm=production", "0123456789abcdef-token", `{"holder":"hp"}`); code != 200 {
		t.Fatalf("lease with token: %d", code)
	}
	if code, m := call(t, "POST", tcp.URL+"/v1/cockpit/lease?realm=production", "0123456789abcdef-token", `{"holder":"evil","steal":true}`); code != 403 {
		t.Fatalf("steal over TCP: %d %v", code, m)
	}
	if code, _ := call(t, "POST", tcp.URL+"/v1/sessions/sM/deliver", "0123456789abcdef-token", `{"from":"x","text":"hi"}`); code != 403 || len(out.lines) != 0 {
		t.Fatalf("deliver over TCP: %d %v", code, out.lines)
	}
	if code, _ := call(t, "POST", local.URL+"/v1/cockpit/lease?realm=production", "", `{"holder":"owner","steal":true}`); code != 200 {
		t.Fatalf("steal over the socket: %d", code)
	}
}

func TestTCPWithNoTokenConfiguredRefusesEverything(t *testing.T) {
	j, _ := journal.Open(t.TempDir(), "m", 0)
	defer j.Close()
	s := httptest.NewServer((&Server{J: j}).TCPHandler())
	defer s.Close()
	if code, _ := call(t, "GET", s.URL+"/v1/info", "", ""); code != 401 {
		t.Fatalf("%d", code)
	}
	if code, _ := call(t, "GET", s.URL+"/v1/info", "Bearer", ""); code != 401 {
		t.Fatalf("empty token matched an empty configured token: %d", code)
	}
}

// context: the owner's deliver goes through the daemon, which refuses to type into a busy pane
// (409), and queues on request (202).
func TestOwnerDeliverIsGatedByTheIdleRule(t *testing.T) {
	local, _, out := newAuthRig(t, false)
	if code, m := call(t, "POST", local.URL+"/v1/sessions/sM/deliver", "", `{"from":"cc-tree","text":"hi"}`); code != 409 || m["code"] != "target_busy" || len(out.lines) != 0 {
		t.Fatalf("busy: %d %v %v", code, m, out.lines)
	}
	if code, _ := call(t, "POST", local.URL+"/v1/sessions/sM/deliver", "", `{"from":"cc-tree","text":"hi","queue":true}`); code != 202 || len(out.lines) != 0 {
		t.Fatalf("queue: %d %v", code, out.lines)
	}
	if code, m := call(t, "GET", local.URL+"/v1/resolve?label=cc-sandbox:mgr", "", ""); code != 200 || m["sid"] != "sM" || m["idle"] != false {
		t.Fatalf("resolve: %d %v", code, m)
	}
	if code, _ := call(t, "GET", local.URL+"/v1/resolve?label=nope:x", "", ""); code != 404 {
		t.Fatalf("resolve unknown: %d", code)
	}
}

func TestOwnerDeliverToAnIdlePaneLands(t *testing.T) {
	local, _, out := newAuthRig(t, true)
	code, m := call(t, "POST", local.URL+"/v1/sessions/sM/deliver", "", `{"from":"cc-tree","text":"hi"}`)
	if code != 200 || len(out.lines) != 1 || !strings.Contains(out.lines[0], "trust: unfenced (owner device)") {
		t.Fatalf("%d %v %v", code, m, out.lines)
	}
}

// ---- review of PR #4: C9b (session.prompt / session.replace) had no tests ----------------------

func newSteerRig(t *testing.T, idle bool, rep func(sid, brief string) (int, error)) (*httptest.Server, *httptest.Server, *nopOut) {
	j, _ := journal.Open(t.TempDir(), "hp-ubuntu", 0)
	q, _ := verbq.Open(t.TempDir())
	dir := fakeDirT{[]msg.Session{{Sid: "sM", Label: "cc-sandbox:mgr", Realm: "production", Live: true, Gen: 1}}}
	out := &nopOut{}
	rt := &msg.Router{Dir: dir, Out: out, Idle: idleT{idle}, StateDir: t.TempDir()}
	s := &Server{J: j, Q: q, Msg: rt, Dir: dir, Token: "0123456789abcdef-token", Replace: rep}
	local, tcp := httptest.NewServer(s.Handler()), httptest.NewServer(s.TCPHandler())
	t.Cleanup(func() { local.Close(); tcp.Close(); j.Close(); q.Close() })
	return local, tcp, out
}

// context: replace kills a pane. A pane that is mid-turn must come out untouched (409, and the
// replace function never ran), or a turn's work is lost.
func TestReplaceMidTurnIs409AndTouchesNothing(t *testing.T) {
	ran := 0
	local, _, _ := newSteerRig(t, false, func(string, string) (int, error) { ran++; return 2, nil })
	if code, m := call(t, "POST", local.URL+"/v1/sessions/sM/replace", "", `{"brief":"new"}`); code != 409 || ran != 0 {
		t.Fatalf("%d %v ran=%d", code, m, ran)
	}
}

func TestReplaceAtTheBoundaryStartsTheNextGeneration(t *testing.T) {
	var gotSid, gotBrief string
	local, _, _ := newSteerRig(t, true, func(sid, b string) (int, error) { gotSid, gotBrief = sid, b; return 2, nil })
	code, m := call(t, "POST", local.URL+"/v1/sessions/sM/replace", "", `{"brief":"new brief"}`)
	if code != 200 || m["gen"] != float64(2) || gotSid != "sM" || gotBrief != "new brief" {
		t.Fatalf("%d %v", code, m)
	}
	// a second replace straight after is refused: the pane was marked busy by the first
	if code, _ := call(t, "POST", local.URL+"/v1/sessions/sM/replace", "", `{"brief":"again"}`); code != 409 {
		t.Fatalf("second replace: %d", code)
	}
}

func TestReplaceFailureIs500AndNotAnAcknowledgement(t *testing.T) {
	local, _, _ := newSteerRig(t, true, func(string, string) (int, error) { return 0, os.ErrPermission })
	if code, _ := call(t, "POST", local.URL+"/v1/sessions/sM/replace", "", `{"brief":"x"}`); code != 500 {
		t.Fatalf("%d", code)
	}
}

// context: steering text is typed only at the prompt, stamped as the owner's device, and the
// loopback TCP port (token holders) can do neither prompt nor replace.
func TestPromptOnlyAtThePromptAndNeverOverTheTokenPort(t *testing.T) {
	local, tcp, out := newSteerRig(t, false, func(string, string) (int, error) { return 2, nil })
	if code, _ := call(t, "POST", local.URL+"/v1/sessions/sM/prompt", "", `{"kind":"nudge","text":"checkpoint now"}`); code != 409 || len(out.lines) != 0 {
		t.Fatalf("busy pane: %d %v", code, out.lines)
	}
	for _, p := range []string{"prompt", "replace"} {
		if code, _ := call(t, "POST", tcp.URL+"/v1/sessions/sM/"+p, "0123456789abcdef-token", `{"text":"x","brief":"x"}`); code != 403 {
			t.Fatalf("%s over the token port: %d", p, code)
		}
	}
	local2, _, out2 := newSteerRig(t, true, nil)
	if code, _ := call(t, "POST", local2.URL+"/v1/sessions/sM/prompt", "", `{"kind":"nudge","text":"checkpoint now"}`); code != 200 || len(out2.lines) != 1 {
		t.Fatalf("idle pane: %d %v", code, out2.lines)
	}
}
