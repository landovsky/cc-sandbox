// Tests of the shared sender (hostd and a pod's sidecar). Why each exists: what leaves the machine is a stamp the
// receiver verifies byte for byte, so the sender must produce exactly what internal/stamp verifies; and WHO a
// message is sent to is a security decision (a squatter's entry for a manager's node must get nothing).
package hubsend

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hubc"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/stamp"
)

func entry(id, node, machine string, fenced bool) map[string]any {
	e := map[string]any{"id": id, "uid": "u-" + id, "node": node, "machine": machine}
	if fenced {
		e["fenced"] = true
	}
	return e
}

// context: the sender's own record says where a node lives; entries elsewhere are squatters, and a node the record
// does not place is only sent to when it is fenced (signature-identified).
func TestResolveChoosesTheOneEntryTheRecordPlacesTheNodeOn(t *testing.T) {
	mgrMac := entry("m1", "cc-sandbox:mgr", "mac", false)
	squat := entry("s1", "cc-sandbox:mgr", "x4", false)
	local := entry("l1", "cc-sandbox:mgr", "hp", false)
	fenced := entry("f1", "cc-sandbox:mgr", "mac", true)
	for name, c := range map[string]struct {
		entries []map[string]any
		want    string
		id      string
		err     error
	}{
		"the entry on the bound machine":             {[]map[string]any{squat, mgrMac}, "mac", "m1", nil},
		"only a squatter on another machine":         {[]map[string]any{squat}, "mac", "", msg.ErrWrongMachine},
		"nothing at all":                             {nil, "mac", "", msg.ErrUnknownNode},
		"two on the bound machine (not guessed)":     {[]map[string]any{mgrMac, fenced}, "mac", "", msg.ErrAmbiguousNode},
		"on this machine: the local bus's":           {[]map[string]any{local}, "hp", "", msg.ErrLocalNode},
		"an unfenced node the record does not place": {[]map[string]any{mgrMac}, "", "", msg.ErrWrongMachine},
		// a fenced session's node is only the label its starter gave it: a decoy would receive the report
		"a fenced node the record does not place": {[]map[string]any{fenced}, "", "", msg.ErrWrongMachine},
		"the node asked in the #-form":            {[]map[string]any{mgrMac}, "mac", "m1", nil},
	} {
		node := "cc-sandbox:mgr"
		if strings.Contains(name, "#-form") {
			node = "cc-sandbox:#mgr"
		}
		got, err := Resolve(c.entries, node, c.want, "hp")
		if c.id != "" {
			c.id = "u-" + c.id // the address is the entry's full uid
		}
		if !errors.Is(err, c.err) || got.ID != c.id {
			t.Errorf("%s: %v %v", name, got, err)
		}
	}
}

type hubSeen struct {
	auth string
	body map[string]any
}

func fakeHub(t *testing.T, status int, delivered int) (*httptest.Server, *hubSeen) {
	s := &hubSeen{}
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.auth = r.Header.Get("Authorization")
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &s.body)
		w.WriteHeader(status)
		json.NewEncoder(w).Encode(map[string]any{"delivered": delivered, "matched": delivered})
	}))
	t.Cleanup(ts.Close)
	return ts, s
}

// context: the receiver verifies the stamp over (from sid, to id, kind, body) with the relay's public key, so what
// the sender posts must verify exactly so, carry the relay token, and say the sender is fenced.
func TestSendPostsAStampTheReceiverVerifiesWithTheRelaysToken(t *testing.T) {
	ts, seen := fakeHub(t, 200, 1)
	pub, priv, _ := stamp.NewKey()
	now := time.Now()
	ok, err := Send(t.Context(), ts.URL, "sbp1.relay-token", priv, From{Sid: "s0worker", Label: "cc-sandbox:w", Machine: "hp"}, Target{ID: "m1", Sid: "u"}, "report", "done", now)
	if err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if seen.auth != "Bearer sbp1.relay-token" {
		t.Fatalf("auth %q", seen.auth)
	}
	from, _ := seen.body["from"].(map[string]any)
	if seen.body["to"] != "m1" || seen.body["text"] != "done" || seen.body["kind"] != "report" || from["sid"] != "s0worker" || from["fenced"] != "true" || from["machine"] != "hp" || from["id"] != "ccsbx:s0worker" {
		t.Fatalf("what was posted: %v", seen.body)
	}
	sig, _ := seen.body["sig"].(string)
	if _, err := stamp.Verify(pub, sig, stamp.Fields{From: "s0worker", To: "m1", Kind: "report", Body: "done"}, now, stamp.NewCache()); err != nil {
		t.Fatalf("the receiver cannot verify what the sender signed: %v", err)
	}
	if _, err := stamp.Verify(pub, sig, stamp.Fields{From: "s0worker", To: "m1", Kind: "report", Body: "edited"}, now, stamp.NewCache()); err == nil {
		t.Fatal("an edited body verified")
	}
}

// context: a kind that is not a plain word cannot be smuggled into the signed input.
func TestAnOddKindBecomesAPlainMessage(t *testing.T) {
	for in, want := range map[string]string{"report": "report", "": "message", "Report!": "message", strings.Repeat("a", 40): "message", "a b": "message"} {
		if got := Kind(in); got != want {
			t.Errorf("%q -> %q, want %q", in, got, want)
		}
	}
}

// context: how a caller reacts depends on the status: 401 means the credential is stale (hostd re-mints, a pod cannot:
// it says relay_auth), 403 is a refusal of this message (never retried); delivered 0 is "nobody got it".
func TestTheHubsStatusReachesTheCallerAsItWas(t *testing.T) {
	_, priv, _ := stamp.NewKey()
	for status, check := range map[int]func(error) bool{401: hubc.IsUnauthorized, 403: hubc.IsForbidden} {
		ts, _ := fakeHub(t, status, 0)
		ok, err := Send(t.Context(), ts.URL, "tok", priv, From{Sid: "s0w"}, Target{ID: "m1"}, "report", "x", time.Now())
		if ok || err == nil || !check(err) {
			t.Errorf("status %d: %v %v", status, ok, err)
		}
	}
	ts, _ := fakeHub(t, 200, 0)
	if ok, err := Send(t.Context(), ts.URL, "tok", priv, From{Sid: "s0w"}, Target{ID: "m1"}, "report", "x", time.Now()); ok || err != nil {
		t.Errorf("delivered 0: %v %v", ok, err)
	}
}

// context (R8-MUST-2): an entry's short id is only 24 bits of a hash of its uid, so another machine's owner can grind a
// uid whose id collides with a fenced manager's. The sender must not address the pair: it addresses the full uid, and
// refuses when ANY other entry shares the chosen one's uid, id or key.
func TestAnEntryThatSharesTheChosenOnesAddressIsNeverSentToBlock(t *testing.T) {
	mgr := map[string]any{"id": "a1b2c3", "uid": "u-real", "key": "ccsbx:s0real", "node": "cc-sandbox:mgr", "machine": "mac", "fenced": true}
	for name, other := range map[string]map[string]any{
		"the same uid":                  {"id": "ffffff", "uid": "u-real", "key": "x4/%2", "machine": "x4"},
		"a key equal to the chosen uid": {"id": "eeeeee", "uid": "u-else", "key": "u-real", "machine": "x4"},
		"an id equal to the chosen uid": {"id": "u-real", "uid": "u-x", "key": "x4/%3", "machine": "x4"},
	} {
		if _, err := Resolve([]map[string]any{mgr, other}, "cc-sandbox:mgr", "mac", "hp"); !errors.Is(err, msg.ErrAmbiguousNode) {
			t.Errorf("%s: %v", name, err)
		}
	}
	// a ground SHORT id alone blocks nothing: the address is the full uid and the hub never fans a uid out
	twin := map[string]any{"id": "a1b2c3", "uid": "u-ground", "key": "x4/%1", "node": "cc-sandbox:other", "machine": "x4"}
	if got, err := Resolve([]map[string]any{mgr, twin}, "cc-sandbox:mgr", "mac", "hp"); err != nil || got.ID != "u-real" {
		t.Errorf("a shared short id blocked the send: %v %v", got, err)
	}
	unrelated := map[string]any{"id": "123456", "uid": "u-other", "key": "x4/%9", "node": "cc-sandbox:other", "machine": "x4"}
	got, err := Resolve([]map[string]any{mgr, unrelated}, "cc-sandbox:mgr", "mac", "hp")
	if err != nil || got.ID != "u-real" {
		t.Fatalf("an unrelated entry blocked the send, or the address is not the uid: %v %v", got, err)
	}
}
