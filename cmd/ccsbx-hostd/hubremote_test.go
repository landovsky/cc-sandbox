// Tests of hostd's messages across machines (M2b). Why: this is where a message from outside meets a
// fenced pane, so each way a sender could be forged or a message misdirected (a bad or replayed stamp,
// a stamp made for another target, a sender the hub has no node for, a node outside the target's scope,
// an address that does not name the target) is a case that must type nothing, and each way an outgoing
// message can fail (no such node, nobody took it, an expired relay credential) must end as dropped or
// an error, never as a success.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hubc"
	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/stamp"
	"github.com/landovsky/cc-sandbox/internal/start"
)

type xDir struct{ s []msg.Session }

func (d *xDir) Sessions() ([]msg.Session, error) { return d.s, nil }

type xOut struct {
	mu    sync.Mutex
	lines []string
}

func (o *xOut) Deliver(sid, line string) error {
	o.mu.Lock()
	o.lines = append(o.lines, line)
	o.mu.Unlock()
	return nil
}

type xIdle struct{}

func (xIdle) IdleSince(string, int) (time.Time, bool) { return time.Now().Add(-time.Hour), true }

type xJ struct {
	mu sync.Mutex
	ev []journal.Event
}

func (j *xJ) Append(e journal.Event) (journal.Record, error) {
	j.mu.Lock()
	j.ev = append(j.ev, e)
	j.mu.Unlock()
	return journal.Record{}, nil
}
func (j *xJ) types() string {
	j.mu.Lock()
	defer j.mu.Unlock()
	var o []string
	for _, e := range j.ev {
		o = append(o, e.Type)
	}
	return strings.Join(o, ",")
}

// fakeHub serves the two reads and the send a hostd needs.
type fakeHub struct {
	mu       sync.Mutex
	sessions []map[string]any
	// /deliver
	relayToken string // the only token /deliver accepts ("" = any)
	delivered  int    // what the hub reports
	matched    int
	got        []hubc.SendReq
	auth401    int // answer 401 this many times first
	status     int // when set, /deliver answers this status (e.g. 403)
}

func (h *fakeHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		switch r.URL.Path {
		case "/sessions":
			json.NewEncoder(w).Encode(h.sessions)
		case "/deliver":
			tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if h.auth401 > 0 || (h.relayToken != "" && tok != h.relayToken) {
				if h.auth401 > 0 {
					h.auth401--
				}
				w.WriteHeader(401)
				return
			}
			if h.status != 0 {
				w.WriteHeader(h.status)
				return
			}
			var q hubc.SendReq
			json.NewDecoder(r.Body).Decode(&q)
			h.got = append(h.got, q)
			json.NewEncoder(w).Encode(map[string]any{"trust": "fenced", "delivered": h.delivered, "matched": h.matched, "dispatched": h.matched})
		default:
			w.WriteHeader(404)
		}
	})
}

type rig struct {
	hr    *hubRemote
	hub   *fakeHub
	out   *xOut
	j     *xJ
	logs  []string
	token string
}

const targetSid = "s0worker01"

func newRig(t *testing.T, mint minter) *rig {
	t.Helper()
	hub := &fakeHub{delivered: 1, matched: 1}
	ts := httptest.NewServer(hub.handler())
	t.Cleanup(ts.Close)
	hub.sessions = []map[string]any{
		{"id": "t1", "key": "ccsbx:" + targetSid, "uid": targetSid, "label": "cc-sandbox:worker", "node": "cc-sandbox:worker", "machine": "hp", "fenced": true},
		{"id": "m1", "key": "mac/%1", "uid": "s0mac12345", "label": "mgr", "node": "cc-sandbox:mgr", "machine": "mac", "fenced": true},
		{"id": "um1", "key": "mac/%2", "uid": "u-peer", "label": "plain", "node": "cc-sandbox:peer", "machine": "mac", "tracked_by": "spoke:mac"},
		{"id": "n1", "key": "mac/%3", "uid": "s0nonode99", "label": "nonode", "machine": "mac", "fenced": true},
	}
	worker := msg.Session{Sid: targetSid, Label: "cc-sandbox:worker", BusID: "ccsbx:" + targetSid, Realm: "production", Gen: 1, Live: true, Started: time.Now().Add(-time.Hour)}
	worker.Scope.Manager.Node = "cc-sandbox:mgr"
	worker.Scope.Manager.Machine = "mac"
	worker.Scope.Peers = []string{"cc-sandbox:peer"}
	worker.Scope.PeerMachines = map[string]string{"cc-sandbox:peer": "mac"}
	worker.Scope.ReplyWindowS = 3600
	out, j := &xOut{}, &xJ{}
	rt := &msg.Router{Dir: &xDir{[]msg.Session{worker}}, Out: out, Idle: xIdle{}, StateDir: t.TempDir()}
	r := &rig{hub: hub, out: out, j: j}
	spoke := &hubc.Client{URL: ts.URL, Token: func() (string, error) { return "sbp1.spoke-token-0001", nil }}
	r.hr = newHubRemote(spoke, ts.URL, "hp", rt, j, func(f string, a ...any) { r.logs = append(r.logs, f) }, mint, t.TempDir())
	rt.Uplink = r.hr
	return r
}

// frame builds a deliver payload for the target, signed by a fresh sender key.
func signed(t *testing.T, from, to, kind, body string) (hubc.Deliver, string) {
	pub, priv, _ := stamp.NewKey()
	sig := stamp.Sign(priv, stamp.Fields{From: from, To: to, Kind: kind, Body: body}, time.Now())
	return hubc.Deliver{UID: targetSid, To: to, Kind: kind, Sig: sig, Pub: pub, Trust: "fenced", Body: body, Text: hubLine(body), From: map[string]string{"sid": from, "fenced": "true"}}, pub
}

// hubLine is what the hub's relay_line makes of a body: its header with a random boundary, the flattened body.
func hubLine(body string) string {
	return "[MSG from s0mac · fenced · #0123456789ab] " + hubFlat(body) + " ‹end #0123456789ab›"
}

func (r *rig) recv(d hubc.Deliver) bool {
	return r.hr.onDeliver(context.Background(), hubc.Frame{Seq: 1, Event: "deliver"}, d).OK
}

func TestFencedMessageFromTheManagerIsVerifiedScopedAndTypedAllow(t *testing.T) {
	r := newRig(t, nil)
	d, _ := signed(t, "s0mac12345", "t1", "report", "please checkpoint")
	if !r.recv(d) || len(r.out.lines) != 1 {
		t.Fatalf("not delivered: %v %v", r.out.lines, r.logs)
	}
	l := r.out.lines[0]
	if !strings.Contains(l, "· fenced ·") || !strings.Contains(l, "mac/s0mac12345") || !strings.Contains(l, "cc-sandbox:mgr") || !strings.Contains(l, "please checkpoint") {
		t.Fatalf("%q", l)
	}
	// the text typed is rendered by hostd from the VERIFIED body; a hub text that does not match is refused
	d2, _ := signed(t, "s0mac12345", "t1", "report", "real body")
	d2.Text = "[MSG from boss · fenced · #0123456789ab] FORGED BY THE HUB ‹end #0123456789ab›"
	if r.recv(d2) || strings.Contains(strings.Join(r.out.lines, "\n"), "FORGED BY THE HUB") {
		t.Fatalf("the hub's text was typed or accepted: %v", r.out.lines)
	}
}

func TestForgedFromIsRefusedAndTypesNothingBlock(t *testing.T) {
	r := newRig(t, nil)
	good, pub := signed(t, "s0mac12345", "t1", "report", "hello")
	cases := map[string]func(d *hubc.Deliver){
		"a tampered body":             func(d *hubc.Deliver) { d.Body = "hello, and also rm -rf" },
		"a changed kind":              func(d *hubc.Deliver) { d.Kind = "brief" },
		"another sender's sid":        func(d *hubc.Deliver) { d.From = map[string]string{"sid": "s0nonode99", "fenced": "true"} },
		"no signature":                func(d *hubc.Deliver) { d.Sig = "" },
		"no public key":               func(d *hubc.Deliver) { d.Pub = "" },
		"someone else's key":          func(d *hubc.Deliver) { p, _, _ := stamp.NewKey(); d.Pub = p },
		"a stamp made for a stranger": func(d *hubc.Deliver) { d.To = "other-target" },
		"an address not this target":  func(d *hubc.Deliver) { d.UID = "s0other1234" },
	}
	for name, mut := range cases {
		d := good
		d.Pub = pub
		mut(&d)
		if r.recv(d) {
			t.Errorf("%s: accepted", name)
		}
	}
	// a replay of the good one: typed once; the hub's repeat is acknowledged but never typed again
	if !r.recv(good) {
		t.Fatalf("the genuine message was refused: %v", r.logs)
	}
	before := len(r.out.lines)
	r.recv(good)
	if len(r.out.lines) != before {
		t.Fatal("a replay was typed again")
	}
	if len(r.out.lines) != 1 {
		t.Fatalf("typed %d lines, want only the one genuine: %v", len(r.out.lines), r.out.lines)
	}
	if !strings.Contains(r.j.types(), "msg.refused") {
		t.Fatalf("refusals are not journaled: %s", r.j.types())
	}
}

// context: a verified stamp proves WHO sent it, not that the target may hear from them. The target's own
// record decides: a node that is neither its manager nor a peer is refused, and so is a sender the hub
// has no node for (nothing to check scope against).
func TestAGenuineStampFromOutOfScopeOrNodelessSenderIsRefused(t *testing.T) {
	r := newRig(t, nil)
	r.hub.sessions = append(r.hub.sessions, map[string]any{"id": "x1", "uid": "s0stranger", "label": "stranger", "node": "other:stranger", "machine": "mac", "fenced": true})
	d, _ := signed(t, "s0stranger", "t1", "report", "hi")
	if r.recv(d) {
		t.Fatal("a stranger's genuine stamp was accepted")
	}
	d, _ = signed(t, "s0nonode99", "t1", "report", "hi")
	if r.recv(d) {
		t.Fatal("a sender with no node was accepted")
	}
	if len(r.out.lines) != 0 {
		t.Fatalf("typed: %v", r.out.lines)
	}
}

// ---- unfenced senders: only on what the hub authenticated ----

// unfenced is a payload as the hub builds it for an unfenced sender: via the principal it posted with,
// bound (or not) to an entry that principal itself tracked.
func unfenced(via string, bound bool, entry map[string]any, from map[string]string) hubc.Deliver {
	return hubc.Deliver{UID: targetSid, To: "t1", Trust: "unfenced", Text: "hello", Body: "hello", From: from, Via: via, Bound: bound, FromEntry: entry}
}

var macUnfencedMgr = map[string]any{"id": "um1", "node": "cc-sandbox:peer", "machine": "mac", "uid": "u-peer", "label": "plain", "tracked_by": "spoke:mac"}

// context: a Mac manager that is an ordinary session (ccmsg) reaches a fenced worker through the hub. The
// hub says it posted with its machine's SPOKE principal, which itself tracked that session on that machine,
// and hostd confirms it against its own read of the presence list: heard, typed visibly as unfenced, and
// held to the normal scope rule on the BOUND node.
func TestUnfencedSenderBoundToItsMachinesSpokeIsDeliveredAndLabelledAllow(t *testing.T) {
	r := newRig(t, nil)
	if !r.recv(unfenced("spoke:mac", true, macUnfencedMgr, map[string]string{"id": "um1", "machine": "mac"})) || len(r.out.lines) != 1 {
		t.Fatalf("%v %v", r.out.lines, r.logs)
	}
	if l := r.out.lines[0]; !strings.Contains(l, "unfenced (hub)") || !strings.Contains(l, "cc-sandbox:peer") {
		t.Fatalf("%q", l)
	}
	// a bound sender whose node is outside the target's scope is refused by the normal scope check
	r.hub.sessions = append(r.hub.sessions, map[string]any{"id": "ux", "uid": "u-x", "node": "other:stranger", "machine": "mac", "tracked_by": "spoke:mac"})
	stranger := map[string]any{"id": "ux", "node": "other:stranger", "machine": "mac", "uid": "u-x", "tracked_by": "spoke:mac"}
	if r.recv(unfenced("spoke:mac", true, stranger, map[string]string{"id": "ux", "machine": "mac"})) || len(r.out.lines) != 1 {
		t.Fatal("a bound sender outside the target's scope was heard")
	}
}

// context (round 4): the node is the scope identity a fenced worker trusts. An owner token of ANY machine can
// publish an unfenced entry carrying a fenced manager's node; bound correctly, it would be heard as that
// manager, and the duplicate would also cut the worker's own reports to the real one. So a sender whose node
// anything else holds (a fenced entry, another principal's entry, a session record of this machine) is not heard.
func TestUnfencedSenderWhoseNodeAnythingElseHoldsIsNotHeardBlock(t *testing.T) {
	squat := func(node string) (map[string]any, map[string]string) {
		e := map[string]any{"id": "sq1", "key": "x4/%1", "uid": "u-squat", "label": "squat", "node": node, "machine": "x4", "tracked_by": "spoke:x4"}
		return e, map[string]string{"id": "sq1", "machine": "x4"}
	}
	for name, node := range map[string]string{
		"a node a FENCED manager holds (the worker's manager)": "cc-sandbox:mgr",
		"the same node in the #-form":                          "cc-sandbox:#mgr",
		"a node a session record of this machine carries":      "cc-sandbox:worker",
	} {
		r := newRig(t, nil)
		e, from := squat(node)
		r.hub.sessions = append(r.hub.sessions, e)
		if r.recv(unfenced("spoke:x4", true, e, from)) || len(r.out.lines) != 0 {
			t.Errorf("%s: heard (%v)", name, r.out.lines)
		}
	}
}

// context (round 5): a node belongs to the machine the TARGET's record binds it to (scope.manager.machine, from
// the owner's start spec). A squatter on a third machine publishing the absent manager's node is bound
// correctly by its own spoke and is the only holder: it must still not be heard as the manager, must not be
// sent the worker's reports, and must not cut the real manager's channel once that comes back.
func TestASquatterOnAnotherMachineIsNeitherHeardNorSentToBlock(t *testing.T) {
	calls := 0
	r := newRig(t, fixedMint("sbp1.relay-token-0001", &calls))
	r.hub.relayToken = "sbp1.relay-token-0001"
	// the real unfenced manager (um1, node cc-sandbox:peer on mac) is away; x4's owner token squats its node
	r.hub.sessions = r.hub.sessions[:2]
	r.hub.sessions = append(r.hub.sessions, map[string]any{"id": "sq1", "key": "x4/%1", "uid": "u-squat", "label": "squat", "node": "cc-sandbox:peer", "machine": "x4", "tracked_by": "spoke:x4"})
	e, from := r.hub.sessions[2], map[string]string{"id": "sq1", "machine": "x4"}
	if r.recv(unfenced("spoke:x4", true, e, from)) || len(r.out.lines) != 0 {
		t.Fatalf("the squatter was heard as the manager: %v", r.out.lines)
	}
	if ok, _, err := r.hr.SendRemote(worker(), "cc-sandbox:peer", "report", "secret"); ok || !errors.Is(err, msg.ErrWrongMachine) || len(r.hub.got) != 0 {
		t.Fatalf("the worker's report reached the squatter: %v %v (%d sends)", ok, err, len(r.hub.got))
	}
	// the real manager comes back: both hold the node, yet the real one (on its bound machine) is heard and
	// sent to, and the squatter is still refused
	r.hub.sessions = append(r.hub.sessions, map[string]any{"id": "um1", "key": "mac/%2", "uid": "u-peer", "label": "plain", "node": "cc-sandbox:peer", "machine": "mac", "tracked_by": "spoke:mac"})
	if !r.recv(unfenced("spoke:mac", true, macUnfencedMgr, map[string]string{"id": "um1", "machine": "mac"})) {
		t.Fatalf("the real manager was cut off by a squatter's duplicate: %v", r.logs)
	}
	if r.recv(unfenced("spoke:x4", true, e, from)) {
		t.Fatal("the squatter was heard once the real manager was back")
	}
	if ok, toSid, err := r.hr.SendRemote(worker(), "cc-sandbox:peer", "report", "x"); !ok || err != nil || toSid != "u-peer" {
		t.Fatalf("the report did not reach the real manager: %v %q %v", ok, toSid, err)
	}
	// a scope that names no machine for an unfenced node reaches nobody unfenced (refused, not guessed)
	w := worker()
	w.Scope.PeerMachines = nil
	n := len(r.hub.got)
	if ok, _, err := r.hr.SendRemote(w, "cc-sandbox:peer", "report", "x"); ok || err == nil || len(r.hub.got) != n {
		t.Fatalf("an unplaced unfenced node was sent to: %v %v", ok, err)
	}
}

// context: the shared owner secret is held by every owner session on every machine, so "bound by legacy"
// proves nothing: refused outright. So is a binding the hub did not make, another machine's principal, a
// from.machine that is missing or wrong, and a sender the presence list does not back.
func TestUnfencedSenderTheHubDidNotBindToASpokeIsRefusedBlock(t *testing.T) {
	r := newRig(t, nil)
	r.hub.sessions = append(r.hub.sessions, map[string]any{"id": "nn1", "uid": "u-nn", "machine": "mac", "tracked_by": "spoke:mac"})
	ok := map[string]string{"id": "um1", "machine": "mac"}
	for name, d := range map[string]hubc.Deliver{
		"via the shared secret (legacy)":    unfenced("legacy", true, macUnfencedMgr, ok),
		"no via (an old hub)":               unfenced("", true, macUnfencedMgr, ok),
		"another machine's spoke principal": unfenced("spoke:elsewhere", true, macUnfencedMgr, ok),
		"not bound":                         unfenced("spoke:mac", false, nil, ok),
		"bound but no entry":                unfenced("spoke:mac", true, nil, ok),
		"from.machine left out":             unfenced("spoke:mac", true, macUnfencedMgr, map[string]string{"id": "um1"}),
		"from.machine is another machine":   unfenced("spoke:mac", true, macUnfencedMgr, map[string]string{"id": "um1", "machine": "elsewhere"}),
		"a presence entry with no node":     unfenced("spoke:mac", true, map[string]any{"id": "nn1", "machine": "mac", "uid": "u"}, map[string]string{"id": "nn1", "machine": "mac"}),
		"an old hub (no binding at all)":    {UID: targetSid, To: "t1", Trust: "unfenced", Text: "hi", From: ok},
		"no from":                           unfenced("spoke:mac", false, nil, nil),
		"an entry the presence list lacks":  unfenced("spoke:mac", true, map[string]any{"id": "gone", "node": "cc-sandbox:mgr", "machine": "mac", "uid": "u"}, map[string]string{"id": "gone", "machine": "mac"}),
		"an entry tracked by someone else": func() hubc.Deliver {
			e := map[string]any{"id": "u1", "node": "cc-sandbox:peer", "machine": "mac", "uid": "u-peer"}
			return unfenced("spoke:mac", true, e, map[string]string{"id": "u1", "machine": "mac"})
		}(),
	} {
		if r.recv(d) {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(r.out.lines) != 0 {
		t.Fatalf("typed: %v", r.out.lines)
	}
}

// context: a fenced identity speaks only signed. An unfenced message that claims one is refused even when
// the hub bound it.
func TestClaimingAFencedIdentityWithoutARelaySignatureIsRefusedBlock(t *testing.T) {
	r := newRig(t, nil)
	for name, d := range map[string]hubc.Deliver{
		"from.fenced = true": unfenced("spoke:mac", true, macUnfencedMgr, map[string]string{"id": "um1", "machine": "mac", "fenced": "true"}),
		"a stray signature": func() hubc.Deliver {
			d := unfenced("spoke:mac", true, macUnfencedMgr, map[string]string{"id": "um1", "machine": "mac"})
			d.Sig = "v1.x"
			return d
		}(),
		"the id of a fenced entry": unfenced("spoke:mac", true, map[string]any{"id": "m1", "node": "cc-sandbox:mgr", "machine": "mac", "uid": "s0mac12345", "tracked_by": "spoke:mac"}, map[string]string{"id": "m1", "machine": "mac"}),
	} {
		if r.recv(d) {
			t.Errorf("%s: accepted", name)
		}
	}
	if len(r.out.lines) != 0 {
		t.Fatalf("typed: %v", r.out.lines)
	}
}

// context: two live entries hold one node (a re-tracked or ghost entry): which one is the manager is not for
// hostd to guess, so a message to that node is dropped, never sent to the newest.
func TestANodeHeldByTwoEntriesIsNotSentToEither(t *testing.T) {
	calls := 0
	r := newRig(t, fixedMint("sbp1.relay-token-0001", &calls))
	r.hub.sessions = append(r.hub.sessions, map[string]any{"id": "m9", "uid": "s0mac99999", "label": "ghost", "node": "cc-sandbox:mgr", "machine": "mac", "fenced": true})
	if _, _, err := r.hr.SendRemote(worker(), "cc-sandbox:mgr", "report", "secret"); !errors.Is(err, msg.ErrAmbiguousNode) || calls != 0 || len(r.hub.got) != 0 {
		t.Fatalf("%v minted %d sent %d", err, calls, len(r.hub.got))
	}
}

// context: a verified stamp names a sid; the hub entry for that sid must be exactly one, fenced, and from
// the machine the sender says. Another machine's spoke can publish an entry for the same sid.
func TestSenderEntryMustBeUniqueFencedAndFromItsMachine(t *testing.T) {
	r := newRig(t, nil)
	d, _ := signed(t, "s0mac12345", "t1", "report", "hello")
	r.hub.sessions = append(r.hub.sessions, map[string]any{"id": "dup", "uid": "s0mac12345", "node": "cc-sandbox:worker", "machine": "mac2", "fenced": true})
	if r.recv(d) {
		t.Fatal("two entries for the sender's sid: accepted")
	}
	r.hub.sessions = r.hub.sessions[:len(r.hub.sessions)-1]
	d2, _ := signed(t, "s0mac12345", "t1", "report", "hello 2")
	d2.From["machine"] = "elsewhere"
	if r.recv(d2) {
		t.Fatal("a sender claiming another machine: accepted")
	}
	d3, _ := signed(t, "s0mac12345", "t1", "report", "hello 3")
	if !r.recv(d3) {
		t.Fatalf("the honest one was refused: %v", r.logs)
	}
}

// context: the typed line is checked against the hub's own rendering: a body the hub changed, or a
// header that is not the hub's, is refused.
func TestHubTextMustMatchTheVerifiedBody(t *testing.T) {
	r := newRig(t, nil)
	for name, mut := range map[string]func(*hubc.Deliver){
		"another body in the text": func(d *hubc.Deliver) { d.Text = hubLine("something else") },
		"mismatched boundaries": func(d *hubc.Deliver) {
			d.Text = strings.Replace(d.Text, "‹end #0123456789ab›", "‹end #ffffffffffff›", 1)
		},
		"no hub header": func(d *hubc.Deliver) { d.Text = "plain" },
	} {
		d, _ := signed(t, "s0mac12345", "t1", "report", "hello")
		mut(&d)
		if r.recv(d) {
			t.Errorf("%s: accepted", name)
		}
	}
	// controls and quote characters in the body are flattened the way the hub does, and still match
	d, _ := signed(t, "s0mac12345", "t1", "report", "line one\nline two\t‹x›\x1b[31m")
	if !r.recv(d) {
		t.Fatalf("a body with control characters was refused: %v", r.logs)
	}
	if strings.ContainsAny(r.out.lines[len(r.out.lines)-1], "\x1b\t") {
		t.Fatalf("control characters reached the pane: %q", r.out.lines)
	}
}

// context: a frame waits in the hub's mailbox while hostd is down. Its skew is judged at the hub's
// receive time (within the queue limit), and one older than the nonce memory is refused.
func TestQueuedFrameIsJudgedAtTheHubsReceiveTime(t *testing.T) {
	r := newRig(t, nil)
	d, _ := signed(t, "s0mac12345", "t1", "report", "hello")
	d.RecvTS = time.Now().Unix()
	r.hr.now = func() time.Time { return time.Now().Add(10 * time.Minute) } // we woke up 10 minutes later
	if !r.recv(d) {
		t.Fatalf("a queued genuine frame was refused: %v", r.logs)
	}
	d2, _ := signed(t, "s0mac12345", "t1", "report", "hello 2")
	d2.RecvTS = time.Now().Add(-20 * time.Minute).Unix()
	r.hr.now = time.Now
	if r.recv(d2) {
		t.Fatal("a frame older than the nonce memory was accepted")
	}
}

// context: hostd restarts after typing a message whose ack failed; the hub sends it again. The handled
// key is on disk, so it is acknowledged and not typed twice.
func TestAMessageTypedBeforeARestartIsNotTypedAgain(t *testing.T) {
	r := newRig(t, nil)
	d, _ := signed(t, "s0mac12345", "t1", "report", "hello")
	if !r.recv(d) || len(r.out.lines) != 1 {
		t.Fatal("first delivery")
	}
	rt2 := r.hr.rt
	again := newHubRemote(r.hr.hub, r.hr.url, "hp", rt2, r.j, func(string, ...any) {}, nil, filepath.Dir(filepath.Dir(r.hr.handledPath)))
	if !again.onDeliver(context.Background(), hubc.Frame{Seq: 9}, d).OK || len(r.out.lines) != 1 {
		t.Fatalf("typed again after a restart: %v", r.out.lines)
	}
}

// ---- sending ----

func fixedMint(token string, calls *int) minter {
	return func(context.Context, string, int, string, bool) (string, error) { *calls++; return token, nil }
}

func worker() msg.Session {
	s := msg.Session{Sid: targetSid, Label: "cc-sandbox:worker", Realm: "production", Gen: 1}
	s.Scope.Manager.Node, s.Scope.Manager.Machine = "cc-sandbox:mgr", "mac"
	s.Scope.Peers, s.Scope.PeerMachines = []string{"cc-sandbox:peer"}, map[string]string{"cc-sandbox:peer": "mac"}
	return s
}

func TestSendRemoteSignsWithTheGenerationKeyAndReportsDelivery(t *testing.T) {
	calls := 0
	r := newRig(t, fixedMint("sbp1.relay-token-0001", &calls))
	r.hub.relayToken = "sbp1.relay-token-0001"
	ok, toSid, err := r.hr.SendRemote(worker(), "cc-sandbox:#mgr", "Report!", "done step 1")
	if !ok || err != nil || toSid != "s0mac12345" || calls != 1 || len(r.hub.got) != 1 {
		t.Fatalf("%v %v calls=%d %v", ok, err, calls, r.hub.got)
	}
	q := r.hub.got[0]
	if q.To != "s0mac12345" || q.Kind != "message" || q.From["sid"] != targetSid || q.From["fenced"] != "true" {
		t.Fatalf("%+v", q)
	}
	// the stamp it sent verifies under the key the relay was minted with (the key hostd made)
	k := r.hr.keys[targetSid+"/1"]
	if _, err := stamp.Verify(pubOf(k), q.Sig, stamp.Fields{From: targetSid, To: "s0mac12345", Kind: "message", Body: "done step 1"}, time.Now(), stamp.NewCache()); err != nil {
		t.Fatalf("the stamp does not verify: %v", err)
	}
	// the key and relay are minted once per generation
	r.hr.SendRemote(worker(), "cc-sandbox:mgr", "report", "second")
	if calls != 1 {
		t.Fatalf("minted %d times", calls)
	}
}

func TestUnreachableTargetsAreDroppedAndJournaledNeverSuccess(t *testing.T) {
	calls := 0
	r := newRig(t, fixedMint("sbp1.relay-token-0001", &calls))
	// the Router (not the uplink) decides "dropped" and journals it through its hook
	w := worker()
	if _, _, err := r.hr.SendRemote(w, "cc-sandbox:nobody", "report", "x"); !errors.Is(err, msg.ErrUnknownNode) {
		t.Fatalf("an unknown node: %v", err)
	}
	r.hub.delivered = 0
	if ok, _, err := r.hr.SendRemote(w, "cc-sandbox:mgr", "report", "x"); ok || err != nil {
		t.Fatalf("nobody took it: %v %v", ok, err)
	}
	// through the router: both end as `dropped` and are journaled, without the text
	rt := r.hr.rt
	rt.Pump(nil)
	worker := w
	worker.Scope.Manager.Node = "cc-sandbox:mgr"
	worker.Scope.Manager.Machine = "mac"
	rt.Dir.(*xDir).s = []msg.Session{worker}
	for _, to := range []string{"cc-sandbox:mgr"} {
		res := rt.Send(worker, []byte(`{"type":"msg.send","to":{"node":"`+to+`"},"kind":"report","text":"secret text"}`))
		if res.Code != "dropped" {
			t.Fatalf("%+v", res)
		}
	}
	if got := r.j.types(); !strings.Contains(got, "msg.dropped") {
		t.Fatalf("journal: %s", got)
	}
	for _, e := range r.j.ev {
		if strings.Contains(string(e.Data), "secret text") {
			t.Fatalf("the message text reached the journal: %s", e.Data)
		}
	}
}

// context: a 401 means the relay credential is stale, so one fresh key and mint; a 403 is the hub refusing
// THIS message (scope, stamp, sender) and a new credential would not change it: no mint, a clear refusal.
func TestAnExpiredRelayCredentialIsReplacedOnceButARefusalIsNotRetried(t *testing.T) {
	calls := 0
	r := newRig(t, fixedMint("sbp1.relay-token-0001", &calls))
	r.hub.auth401 = 1
	ok, _, err := r.hr.SendRemote(worker(), "cc-sandbox:mgr", "report", "x")
	if !ok || err != nil || calls != 2 {
		t.Fatalf("%v %v minted %d times, want 2 (the first was refused)", ok, err, calls)
	}
	r.hub.auth401 = 5
	if ok, _, err := r.hr.SendRemote(worker(), "cc-sandbox:mgr", "report", "y"); ok || err == nil {
		t.Fatalf("a hub that keeps answering 401 must be an error, not a success: %v %v", ok, err)
	}
	r2 := newRig(t, fixedMint("sbp1.relay-token-0001", &calls))
	calls = 0
	r2.hub.status = 403
	if ok, _, err := r2.hr.SendRemote(worker(), "cc-sandbox:mgr", "report", "x"); ok || err != nil || calls != 1 {
		t.Fatalf("403: %v %v minted %d times, want one mint and no retry", ok, err, calls)
	}
}

// context: a node on this machine, or one the hub does not hold, is reported so the router can use ccmsg
// for it; an UNFENCED node of another machine is sent (the hub lets a relay reach it only when the relay was
// minted with that node in may_reach, and answers 403 otherwise).
func TestUplinkLeavesLocalNodesAloneAndSendsUnfencedRemoteOnes(t *testing.T) {
	calls := 0
	r := newRig(t, fixedMint("sbp1.relay-token-0001", &calls))
	if _, _, err := r.hr.SendRemote(worker(), "cc-sandbox:worker", "report", "x"); !errors.Is(err, msg.ErrLocalNode) {
		t.Fatalf("a node on this machine: %v", err)
	}
	if ok, toSid, err := r.hr.SendRemote(worker(), "cc-sandbox:peer", "report", "x"); !ok || err != nil || toSid != "u-peer" || len(r.hub.got) != 1 {
		t.Fatalf("an unfenced node of another machine: %v %q %v", ok, toSid, err)
	}
}

// context: the signed path must be taken for a node of another machine even when ccmsg is configured, and
// ccmsg is used only for what the hub says is not remote.
type ccmsgSpy struct{ sent []string }

func (c *ccmsgSpy) Send(node, line string) error { c.sent = append(c.sent, node); return nil }

func TestAFencedNodeOfAnotherMachineTakesTheSignedPathEvenWithCcmsgConfigured(t *testing.T) {
	calls := 0
	r := newRig(t, fixedMint("sbp1.relay-token-0001", &calls))
	spy := &ccmsgSpy{}
	r.hr.rt.Hub = spy
	w := worker()
	w.Scope.Manager.Node = "cc-sandbox:mgr"
	w.Scope.Manager.Machine = "mac"
	w.Scope.Peers = []string{"cc-sandbox:nobody"}
	r.hr.rt.Dir.(*xDir).s = []msg.Session{w}
	if res := r.hr.rt.Send(w, []byte(`{"type":"msg.send","to":{"node":"cc-sandbox:mgr"},"kind":"report","text":"hi"}`)); res.Status != "ok" || len(spy.sent) != 0 || len(r.hub.got) != 1 {
		t.Fatalf("%+v ccmsg=%v signed=%d", res, spy.sent, len(r.hub.got))
	}
	// a node the hub does not hold falls through to ccmsg
	if res := r.hr.rt.Send(w, []byte(`{"type":"msg.send","to":{"node":"cc-sandbox:nobody"},"text":"hi"}`)); res.Status != "ok" || len(spy.sent) != 1 {
		t.Fatalf("%+v ccmsg=%v", res, spy.sent)
	}
}

// context: the router's lock is NOT held across the hub round trip: a send that waits on the hub must not
// block a delivery arriving for another session (two machines sending to each other would deadlock and
// type both messages twice after "dropped").
func TestASlowSendDoesNotBlockADeliveryOrALocalSend(t *testing.T) {
	calls := 0
	r := newRig(t, fixedMint("sbp1.relay-token-0001", &calls))
	gate := make(chan struct{})
	slow := &slowUplink{release: gate, entered: make(chan struct{})}
	r.hr.rt.Uplink = slow
	w := worker()
	w.Live, w.Started = true, time.Now().Add(-time.Hour)
	w.Scope.Manager.Node = "cc-sandbox:mgr"
	w.Scope.Manager.Machine = "mac"
	r.hr.rt.Dir.(*xDir).s = []msg.Session{w}
	done := make(chan msg.Result, 1)
	go func() {
		done <- r.hr.rt.Send(w, []byte(`{"type":"msg.send","to":{"node":"cc-sandbox:mgr"},"text":"slow"}`))
	}()
	<-slow.entered
	got := make(chan msg.Result, 1)
	go func() {
		got <- r.hr.rt.DeliverRemote(msg.RemoteSender{Node: "cc-sandbox:mgr", Sid: "s0mac12345", Machine: "mac", Fenced: true}, targetSid, "while you wait")
	}()
	select {
	case res := <-got:
		if res.Status != "ok" {
			t.Fatalf("%+v", res)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("DeliverRemote blocked while a send was in flight (the router lock is held across the network)")
	}
	close(gate)
	<-done
}

type slowUplink struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (u *slowUplink) SendRemote(msg.Session, string, string, string) (bool, string, error) {
	u.once.Do(func() { close(u.entered) })
	<-u.release
	return true, "s0mac12345", nil
}

func TestNoMinterMeansAnErrorNotAnUnsignedSend(t *testing.T) {
	r := newRig(t, nil)
	if ok, _, err := r.hr.SendRemote(worker(), "cc-sandbox:mgr", "report", "x"); ok || err == nil || len(r.hub.got) != 0 {
		t.Fatalf("%v %v %v", ok, err, r.hub.got)
	}
}

// context: a key is for the life of ONE generation, not of the daemon: when the generation is no longer
// live its key is wiped and dropped.
func TestKeysOfEndedGenerationsAreWipedAndDropped(t *testing.T) {
	calls := 0
	r := newRig(t, fixedMint("sbp1.relay-token-0001", &calls))
	r.hr.SendRemote(worker(), "cc-sandbox:mgr", "report", "x")
	k := r.hr.keys[targetSid+"/1"]
	if k == nil {
		t.Fatal("no key")
	}
	priv := k.priv
	r.hr.rt.Dir.(*xDir).s[0].Live = false // the generation ended
	r.hr.pruneKeys()
	if len(r.hr.keys) != 0 {
		t.Fatalf("%d keys kept", len(r.hr.keys))
	}
	for _, b := range priv {
		if b != 0 {
			t.Fatal("the private key was not wiped")
		}
	}
}

// context: what production publishes on the hub for a fenced session must include its node (from the
// daemon's own record) and still no last-output line: otherwise M2b works only in test rigs.
func TestProductionEntriesCarryTheNodeAndNoOutputLine(t *testing.T) {
	all := []msg.Session{{Sid: "s0abc12345", Label: "cc-sandbox:3cvt", Live: true, Gen: 2}, {Sid: "s0dead0000", Label: "x:y", Live: false}}
	es := fencedEntries("hp", all, func(string, int) (time.Time, bool) { return time.Now(), true })
	if len(es) != 1 {
		t.Fatalf("%v", es)
	}
	e := es[0]
	if e["node"] != "cc-sandbox:3cvt" || e["fenced"] != true || e["uid"] != "s0abc12345" || e["downlink"] != "spoke:hp/ccsbx" || e["state"] != "waiting" {
		t.Fatalf("%v", e)
	}
	for _, banned := range []string{"gist", "pane", "last_output"} {
		if _, ok := e[banned]; ok {
			t.Fatalf("an entry published %q", banned)
		}
	}
}

// context: the relay credential comes from ccsbx-root, run through sudo with no password and no
// tty; hostd passes the sid, generation and the PUBLIC key after `--` and reads the token from stdout.
func TestRootMinterRunsTheHelperVerbWithTheKeyAfterDashDash(t *testing.T) {
	dir := t.TempDir()
	argsFile := filepath.Join(dir, "args")
	root := filepath.Join(dir, "ccsbx-root")
	os.WriteFile(root, []byte("#!/bin/sh\necho \"$@\" > "+argsFile+"\necho '{\"token\":\"sbp1.minted-by-root-0001\"}'\n"), 0o755)
	sudo := filepath.Join(dir, "sudo")
	os.WriteFile(sudo, []byte("#!/bin/sh\n[ \"$1\" = -n ] || exit 9\nshift\nexec \"$@\"\n"), 0o755)
	oldS, oldR := start.SudoBin, start.RootBin
	start.SudoBin, start.RootBin = sudo, root
	defer func() { start.SudoBin, start.RootBin = oldS, oldR }()
	defer start.SetTestRootUID(os.Getuid())()
	tok, err := rootMinter()(context.Background(), "s0abc12345", 3, "PUBKEY_B64", false)
	if err != nil || tok != "sbp1.minted-by-root-0001" {
		t.Fatalf("%q %v", tok, err)
	}
	b, _ := os.ReadFile(argsFile)
	if strings.TrimSpace(string(b)) != "relay-mint -- s0abc12345 3 PUBKEY_B64" {
		t.Fatalf("args: %q", b)
	}
	rootMinter()(context.Background(), "s0abc12345", 3, "PUBKEY_B64", true)
	b, _ = os.ReadFile(argsFile)
	if strings.TrimSpace(string(b)) != "relay-mint --after-401 -- s0abc12345 3 PUBKEY_B64" {
		t.Fatalf("the retry after a 401 passes --after-401: %q", b)
	}
	os.WriteFile(root, []byte("#!/bin/sh\necho 'not json'\n"), 0o755)
	if _, err := rootMinter()(context.Background(), "s0abc12345", 3, "k", false); err == nil {
		t.Fatal("a non-token answer was accepted")
	}
}

func pubOf(k *relayKey) string {
	return base64.RawURLEncoding.EncodeToString(k.priv.Public().(ed25519.PublicKey))
}

// context: eight sends at once and the relay credential expires (one 401): exactly ONE re-mint happens,
// no send signs with a key another send is wiping, and every send ends delivered. Run under -race.
func TestParallelSendsAfterAnExpiryMintOnceAndNeverShareAKeyMidSignature(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	mint := func(context.Context, string, int, string, bool) (string, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		time.Sleep(20 * time.Millisecond)
		return "sbp1.relay-token-" + strconv.Itoa(calls) + "00000", nil
	}
	r := newRig(t, mint)
	if _, _, err := r.hr.SendRemote(worker(), "cc-sandbox:mgr", "report", "warm"); err != nil { // the first mint
		t.Fatal(err)
	}
	r.hub.mu.Lock()
	r.hub.relayToken = "sbp1.relay-token-200000" // the hub now only accepts the SECOND token: the first one "expired"
	r.hub.mu.Unlock()
	var wg sync.WaitGroup
	oks := make(chan bool, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			ok, _, err := r.hr.SendRemote(worker(), "cc-sandbox:mgr", "report", "msg "+strconv.Itoa(i))
			oks <- ok && err == nil
		}(i)
	}
	wg.Add(1)
	go func() { defer wg.Done(); r.hr.pruneKeys() }() // a renewal prunes while the sends run
	wg.Wait()
	close(oks)
	for ok := range oks {
		if !ok {
			t.Error("a send failed")
		}
	}
	mu.Lock()
	defer mu.Unlock()
	if calls > 3 { // the first mint, the one re-mint after the 401, and at most one more if a prune hit
		t.Fatalf("minted %d times: the burst did not share one re-mint", calls)
	}
}

// context (R-H review S3): the worktree check runs while hostd holds the daemon lock. A helper that stalls and IGNORES the
// termination signal while holding stdout open must not keep hostd waiting beyond the bound.
func TestAStalledSignalIgnoringHelperCannotHoldTheCallerPastItsBound(t *testing.T) {
	script := filepath.Join(t.TempDir(), "stall.sh")
	os.WriteFile(script, []byte("#!/bin/sh\ntrap '' TERM\nsleep 30 &\nwait\n"), 0o755)
	t0 := time.Now()
	_, err := runBounded(500*time.Millisecond, script)
	if err == nil || time.Since(t0) > 4*time.Second {
		t.Fatalf("err=%v after %v", err, time.Since(t0))
	}
}
