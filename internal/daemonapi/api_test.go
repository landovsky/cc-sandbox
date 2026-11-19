package daemonapi

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/journal"
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
	r.add("session.start")
	r.add("hook.Stop")
	_, first := r.get(t, "/v1/events")
	last := int64(first["head"].(float64))
	for _, ty := range []string{"hook.UserPromptSubmit", "hook.Stop", "session.end"} {
		r.add(ty)
	}
	code, m := r.get(t, fmt.Sprintf("/v1/events?after=%d&epoch=%s", last, r.j.Epoch()))
	if code != 200 || evTypes(m) != "hook.UserPromptSubmit,hook.Stop,session.end" {
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
	r.add("session.start")
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
