// Tests of the pod's bus sender. Why: it is the one place a fenced pod's words leave the cluster, so each case
// is a rule about what the sidecar may do with the one relay credential it holds.

package side

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/stamp"
	"github.com/landovsky/cc-sandbox/internal/token"
)

type fakeHubSrv struct {
	mu           sync.Mutex
	entries      []map[string]any
	sessStatus   int
	deliverCode  int
	delivered    int
	deliverCalls int
	auth         string
	posted       map[string]any
	*httptest.Server
}

func newFakeHub(t *testing.T) *fakeHubSrv {
	h := &fakeHubSrv{sessStatus: 200, deliverCode: 200, delivered: 1}
	h.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.auth = r.Header.Get("Authorization")
		switch r.URL.Path {
		case "/sessions":
			w.WriteHeader(h.sessStatus)
			json.NewEncoder(w).Encode(h.entries)
		case "/deliver":
			h.deliverCalls++
			b, _ := io.ReadAll(r.Body)
			h.posted = nil
			json.Unmarshal(b, &h.posted)
			w.WriteHeader(h.deliverCode)
			json.NewEncoder(w).Encode(map[string]any{"delivered": h.delivered, "matched": h.delivered})
		}
	}))
	t.Cleanup(h.Close)
	return h
}

func hubEntry(id, node, machine string, fenced bool) map[string]any {
	e := map[string]any{"id": id, "uid": "u-" + id, "node": node, "machine": machine}
	if fenced {
		e["fenced"] = true
	}
	return e
}

type busRig struct {
	t   *testing.T
	hub *fakeHubSrv
	j   *lockedJournal
	pub string
	bus *Bus
}

func newBusRig(t *testing.T, mut func(*BusConfig)) *busRig {
	pub, priv, err := stamp.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	r := &busRig{t: t, hub: newFakeHub(t), j: &lockedJournal{}, pub: pub}
	r.hub.entries = []map[string]any{hubEntry("m1", "cc-sandbox:mgr", "mac", false)}
	sc := msg.Scope{}
	sc.Manager.Node, sc.Manager.Machine = "cc-sandbox:mgr", "mac"
	cfg := BusConfig{HubURL: r.hub.URL, Machine: "hp", Sid: "s0worker", Label: "cc-sandbox:w", Scope: sc, Journal: r.j,
		Cred: func() (RelayCred, bool) {
			return RelayCred{Token: "sbp1.relay-token", Key: &token.GenKey{Private: priv, Public: priv.Public().(ed25519.PublicKey)}}, true
		}}
	if mut != nil {
		mut(&cfg)
	}
	r.bus, err = NewBus(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func send(b *Bus, body string) hostd.Reply {
	return b.Forward(context.Background(), hostd.Request{Type: "msg.send", Body: json.RawMessage(body)})
}

func code(r hostd.Reply) string {
	m, _ := r.Result.(map[string]string)
	return m["code"]
}

// context "the manager is on another machine and the pod holds only a relay token": the message goes out
// signed by the generation key (the receiver verifies it with that key's public half), with the relay token as
// bearer, and the sender can send again (the key it signed with was a copy).
func TestAPodMessageToTheManagerIsStampedAndPostedWithTheRelayToken(t *testing.T) {
	r := newBusRig(t, nil)
	for i := 0; i < 2; i++ {
		if rep := send(r.bus, `{"to":{"node":"cc-sandbox:#mgr"},"kind":"report","text":"SECRET-TEXT done"}`); rep.Status != "ok" {
			t.Fatalf("send %d: %+v", i, rep)
		}
	}
	if r.hub.auth != "Bearer sbp1.relay-token" {
		t.Fatalf("auth %q", r.hub.auth)
	}
	sig, _ := r.hub.posted["sig"].(string)
	if _, err := stamp.Verify(r.pub, sig, stamp.Fields{From: "s0worker", To: "m1", Kind: "report", Body: "SECRET-TEXT done"}, time.Now(), stamp.NewCache()); err != nil {
		t.Fatalf("the receiver cannot verify the stamp: %v", err)
	}
}

// context "the fence names a node its record does not give it": refused before the hub is asked anything.
func TestAnOutOfScopeNodeNeverReachesTheHub(t *testing.T) {
	r := newBusRig(t, nil)
	rep := send(r.bus, `{"to":{"node":"cc-sandbox:other"},"text":"hi"}`)
	if rep.Status != "refused" || code(rep) != "scope" || r.hub.deliverCalls != 0 {
		t.Fatalf("%+v calls %d", rep, r.hub.deliverCalls)
	}
	for _, body := range []string{`{"to":{"bus_id":"x"},"text":"hi"}`, `{"to":{"node":"cc-sandbox:mgr"},"text":"  "}`, `not json`} {
		if rep := send(r.bus, body); rep.Status != "refused" || code(rep) != "bad_request" {
			t.Errorf("%s: %+v", body, rep)
		}
	}
	if rep := r.bus.Forward(context.Background(), hostd.Request{Type: "task.update", Body: json.RawMessage(`{}`)}); code(rep) != "unsupported_in_pod" {
		t.Errorf("another type: %+v", rep)
	}
}

// context "the load carried no relay token": the fence gets a clear error and nothing is sent.
func TestWithoutARelayCredentialNothingIsSent(t *testing.T) {
	r := newBusRig(t, func(c *BusConfig) { c.Cred = func() (RelayCred, bool) { return RelayCred{}, false } })
	if rep := send(r.bus, `{"to":{"node":"cc-sandbox:mgr"},"text":"hi"}`); rep.Status != "error" || code(rep) != "relay_unavailable" || r.hub.deliverCalls != 0 {
		t.Fatalf("%+v", rep)
	}
}

// context "the hub no longer accepts this generation's relay credential (expired or revoked)": a pod cannot mint a
// new one, so it must not retry (a loop would hammer the hub); the fence is told, and the cockpit can see why.
func TestA401IsFinalInAPodAndIsJournalled(t *testing.T) {
	r := newBusRig(t, nil)
	r.hub.deliverCode = 401
	rep := send(r.bus, `{"to":{"node":"cc-sandbox:mgr"},"text":"hi"}`)
	if code(rep) != "relay_auth" || r.hub.deliverCalls != 1 {
		t.Fatalf("%+v after %d deliver calls (want exactly 1: no retry)", rep, r.hub.deliverCalls)
	}
	if got := r.j.of("bus.relay_auth"); len(got) != 1 {
		t.Fatalf("journal %+v", r.j.evs)
	}
}

// context "the hub refuses THIS message (scope, stamp, sender)": a new credential would not change that, so it is
// final and reads as dropped, as in hostd.
func TestA403FromTheHubIsFinalAndDropped(t *testing.T) {
	r := newBusRig(t, nil)
	r.hub.deliverCode = 403
	if rep := send(r.bus, `{"to":{"node":"cc-sandbox:mgr"},"text":"hi"}`); code(rep) != "dropped" || r.hub.deliverCalls != 1 {
		t.Fatalf("%+v calls %d", rep, r.hub.deliverCalls)
	}
}

// context "the hub does not let a relay read presence": that is a configuration gap, not a refused message, so it
// must not be reported as 'dropped'.
func TestAHubThatWontListSessionsIsNotReportedAsADroppedMessage(t *testing.T) {
	r := newBusRig(t, nil)
	r.hub.sessStatus = 403
	rep := send(r.bus, `{"to":{"node":"cc-sandbox:mgr"},"text":"hi"}`)
	if code(rep) != "presence_forbidden" || r.hub.deliverCalls != 0 || len(r.j.of("bus.unavailable")) != 1 {
		t.Fatalf("%+v %+v", rep, r.j.evs)
	}
}

// context "someone on another machine squats the manager's node": the record binds the node to one machine, so
// the squatter gets nothing.
func TestASquatterOnAnotherMachineGetsNothing(t *testing.T) {
	r := newBusRig(t, nil)
	// a FENCED squatter is the dangerous one: a signature identifies it, so only the record's machine keeps it out
	r.hub.entries = []map[string]any{hubEntry("x9", "cc-sandbox:mgr", "evil", true)}
	if rep := send(r.bus, `{"to":{"node":"cc-sandbox:mgr"},"text":"hi"}`); code(rep) != "dropped" || r.hub.deliverCalls != 0 {
		t.Fatalf("%+v", rep)
	}
}

// context "a fence provokes failures in a loop": the journal gets a bounded number of facts, never the text, and
// never anything the fence typed.
func TestFailureFactsAreBoundedAndCarryNoMessageText(t *testing.T) {
	r := newBusRig(t, func(c *BusConfig) { c.MaxFacts = 2 })
	r.hub.deliverCode = 403
	for i := 0; i < 6; i++ {
		send(r.bus, `{"to":{"node":"cc-sandbox:mgr"},"text":"SECRET-TEXT"}`)
	}
	if n := len(r.j.of("bus.dropped")); n != 2 {
		t.Fatalf("%d facts, want the bound of 2", n)
	}
	for _, e := range r.j.evs {
		if strings.Contains(string(e.Data), "SECRET") || strings.Contains(string(e.Data), "relay-token") {
			t.Errorf("fact %s", e.Data)
		}
	}
}

// context "the sender is the relay's Forward": a msg.send file in the outbox becomes a signed hub message and the
// fence reads an ok reply, end to end.
func TestTheOutboxRelayHandsAMsgSendToTheBus(t *testing.T) {
	r := newBusRig(t, nil)
	rr := newRelayRig(t)
	rr.put("m.json", `{"type":"msg.send","to":{"node":"cc-sandbox:mgr"},"text":"hello"}`)
	rr.relay(func(c *RelayConfig) { c.Forward = r.bus.Forward }).Scan(context.Background())
	if rep := rr.replyOf("m"); rep.Status != "ok" || r.hub.deliverCalls != 1 {
		t.Fatalf("%+v calls %d", rep, r.hub.deliverCalls)
	}
}

// context "a compromised worker loops msg.send files": the outbox relay's own bucket is about 5 a second, which
// would all land in the manager's pane, so the bus keeps hostd's window (6 a minute per node), counts both
// spellings of the node as one, asks the hub nothing once capped, and lets the window reopen.
func TestAFloodToOneNodeIsCappedLikeHostdAndCostsTheHubNothing(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	r := newBusRig(t, func(c *BusConfig) { c.Now = func() time.Time { return now } })
	spell := []string{"cc-sandbox:mgr", "cc-sandbox:#mgr"}
	for i := 0; i < 6; i++ {
		if rep := send(r.bus, `{"to":{"node":"`+spell[i%2]+`"},"text":"hi"}`); rep.Status != "ok" {
			t.Fatalf("send %d: %+v", i, rep)
		}
	}
	rep := send(r.bus, `{"to":{"node":"cc-sandbox:mgr"},"text":"hi"}`)
	if rep.Status != "refused" || code(rep) != "rate_capped" || r.hub.deliverCalls != 6 {
		t.Fatalf("7th: %+v after %d deliveries", rep, r.hub.deliverCalls)
	}
	now = now.Add(61 * time.Second)
	if rep := send(r.bus, `{"to":{"node":"cc-sandbox:mgr"},"text":"hi"}`); rep.Status != "ok" {
		t.Fatalf("after the window: %+v", rep)
	}
}

func peerScope(machine string) func(*BusConfig) {
	return func(c *BusConfig) {
		c.Scope.Peers = []string{"cc-sandbox:peer"}
		c.Scope.PeerMachines = map[string]string{"cc-sandbox:peer": machine}
	}
}

// context "a peer of the session is as reachable as the manager, on the machine the record places it on": and a
// peer entry that only exists on another machine gets nothing.
func TestAPeerIsReachableOnItsOwnMachineOnly(t *testing.T) {
	r := newBusRig(t, peerScope("mac2"))
	r.hub.entries = append(r.hub.entries, hubEntry("p1", "cc-sandbox:peer", "mac2", false))
	if rep := send(r.bus, `{"to":{"node":"cc-sandbox:peer"},"text":"hi"}`); rep.Status != "ok" || r.hub.posted["to"] != "p1" {
		t.Fatalf("%+v posted %v", rep, r.hub.posted)
	}
	r2 := newBusRig(t, peerScope("mac2"))
	r2.hub.entries = []map[string]any{hubEntry("p9", "cc-sandbox:peer", "elsewhere", false)}
	if rep := send(r2.bus, `{"to":{"node":"cc-sandbox:peer"},"text":"hi"}`); code(rep) != "dropped" || r2.hub.deliverCalls != 0 {
		t.Fatalf("%+v", rep)
	}
}

// context "the record places the manager on THIS machine": that is the local bus's business, and a pod has none,
// so the fence is told there is no live session, never a false 'ok'.
func TestANodeOnThisMachineIsNotReportedAsSent(t *testing.T) {
	r := newBusRig(t, func(c *BusConfig) { c.Scope.Manager.Machine = "hp" })
	r.hub.entries = []map[string]any{hubEntry("l1", "cc-sandbox:mgr", "hp", false)}
	rep := send(r.bus, `{"to":{"node":"cc-sandbox:mgr"},"text":"hi"}`)
	if rep.Status != "error" || code(rep) != "no_live_session" || r.hub.deliverCalls != 0 {
		t.Fatalf("%+v", rep)
	}
}

// context "two sessions hold the manager's node on the right machine (a roll in progress or a squat)": a message
// must not guess between them, and the fence is told it was dropped, not that the hub is unavailable.
func TestTwoSessionsOnTheNodeMeansNoGuessing(t *testing.T) {
	r := newBusRig(t, nil)
	r.hub.entries = append(r.hub.entries, hubEntry("m2", "cc-sandbox:mgr", "mac", false))
	rep := send(r.bus, `{"to":{"node":"cc-sandbox:mgr"},"text":"hi"}`)
	if code(rep) != "dropped" || r.hub.deliverCalls != 0 || len(r.j.of("bus.dropped")) != 1 {
		t.Fatalf("%+v %+v", rep, r.j.evs)
	}
}

// context "a fence addresses by bus id as well as by node": bus ids are local-bus addresses and a pod has no
// local bus, so a request carrying one is malformed even when it also names a valid node.
func TestABusIDIsRefusedEvenNextToAValidNode(t *testing.T) {
	r := newBusRig(t, nil)
	rep := send(r.bus, `{"to":{"node":"cc-sandbox:mgr","bus_id":"x"},"text":"hi"}`)
	if rep.Status != "refused" || code(rep) != "bad_request" || r.hub.deliverCalls != 0 {
		t.Fatalf("%+v", rep)
	}
}
