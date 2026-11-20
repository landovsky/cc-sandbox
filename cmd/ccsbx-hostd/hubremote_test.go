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
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
		{"id": "u1", "key": "mac/%2", "uid": "u-unfenced", "label": "plain", "node": "cc-sandbox:peer", "machine": "mac"},
		{"id": "n1", "key": "mac/%3", "uid": "s0nonode99", "label": "nonode", "machine": "mac", "fenced": true},
	}
	worker := msg.Session{Sid: targetSid, Label: "cc-sandbox:worker", BusID: "ccsbx:" + targetSid, Realm: "production", Gen: 1, Live: true, Started: time.Now().Add(-time.Hour)}
	worker.Scope.Manager.Node = "cc-sandbox:mgr"
	worker.Scope.Peers = []string{"cc-sandbox:peer"}
	worker.Scope.ReplyWindowS = 3600
	out, j := &xOut{}, &xJ{}
	rt := &msg.Router{Dir: &xDir{[]msg.Session{worker}}, Out: out, Idle: xIdle{}, StateDir: t.TempDir()}
	r := &rig{hub: hub, out: out, j: j}
	spoke := &hubc.Client{URL: ts.URL, Token: func() (string, error) { return "sbp1.spoke-token-0001", nil }}
	r.hr = newHubRemote(spoke, ts.URL, "hp", rt, j, func(f string, a ...any) { r.logs = append(r.logs, f) }, mint)
	rt.Uplink = r.hr
	return r
}

// frame builds a deliver payload for the target, signed by a fresh sender key.
func signed(t *testing.T, from, to, kind, body string) (hubc.Deliver, string) {
	pub, priv, _ := stamp.NewKey()
	sig := stamp.Sign(priv, stamp.Fields{From: from, To: to, Kind: kind, Body: body}, time.Now())
	return hubc.Deliver{UID: targetSid, To: to, Kind: kind, Sig: sig, Pub: pub, Trust: "fenced", Body: body, Text: "[MSG from x · fenced · #ab] " + body, From: map[string]string{"sid": from, "fenced": "true"}}, pub
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
	// the text typed is the VERIFIED body, never the hub's rewritten text
	d2, _ := signed(t, "s0mac12345", "t1", "report", "real body")
	d2.Text = "[MSG from boss · fenced · #zz] FORGED BY THE HUB"
	r.recv(d2)
	if strings.Contains(strings.Join(r.out.lines, "\n"), "FORGED BY THE HUB") {
		t.Fatalf("the hub's text was typed: %v", r.out.lines)
	}
}

// T:xmachine-forged-from: nothing of this may reach the pane.
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
	// a replay of the good one: once, then refused
	if !r.recv(good) {
		t.Fatalf("the genuine message was refused: %v", r.logs)
	}
	before := len(r.out.lines)
	if r.recv(good) || len(r.out.lines) != before {
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

// context: a message from an unfenced session (an ordinary ccmsg from a Mac manager) has no stamp. It is
// delivered as "unfenced (hub)", the sender known by the hub's entry for its bus id, under the same scope.
func TestUnfencedSenderIsDeliveredAsUnfencedUnderTheSameScope(t *testing.T) {
	r := newRig(t, nil)
	d := hubc.Deliver{UID: targetSid, To: "t1", Trust: "unfenced", Text: "hello", From: map[string]string{"id": "u1"}}
	if !r.recv(d) || !strings.Contains(r.out.lines[0], "unfenced (hub)") {
		t.Fatalf("%v %v", r.out.lines, r.logs)
	}
	d2 := hubc.Deliver{UID: targetSid, To: "t1", Trust: "unfenced", Text: "hi", From: map[string]string{"id": "nobody"}}
	if r.recv(d2) || len(r.out.lines) != 1 {
		t.Fatal("an unknown unfenced sender was accepted")
	}
}

// ---- sending ----

func fixedMint(token string, calls *int) minter {
	return func(context.Context, string, int, string) (string, error) { *calls++; return token, nil }
}

func worker() msg.Session {
	return msg.Session{Sid: targetSid, Label: "cc-sandbox:worker", Realm: "production", Gen: 1}
}

func TestSendRemoteSignsWithTheGenerationKeyAndReportsDelivery(t *testing.T) {
	calls := 0
	r := newRig(t, fixedMint("sbp1.relay-token-0001", &calls))
	r.hub.relayToken = "sbp1.relay-token-0001"
	ok, err := r.hr.SendRemote(worker(), "cc-sandbox:#mgr", "Report!", "done step 1")
	if !ok || err != nil || calls != 1 || len(r.hub.got) != 1 {
		t.Fatalf("%v %v calls=%d %v", ok, err, calls, r.hub.got)
	}
	q := r.hub.got[0]
	if q.To != "m1" || q.Kind != "message" || q.From["sid"] != targetSid || q.From["fenced"] != "true" {
		t.Fatalf("%+v", q)
	}
	// the stamp it sent verifies under the key the relay was minted with (the key hostd made)
	k := r.hr.keys[targetSid+"/1"]
	if _, err := stamp.Verify(pubOf(k), q.Sig, stamp.Fields{From: targetSid, To: "m1", Kind: "message", Body: "done step 1"}, time.Now(), stamp.NewCache()); err != nil {
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
	if ok, err := r.hr.SendRemote(worker(), "cc-sandbox:nobody", "report", "x"); ok || err != nil {
		t.Fatalf("an unknown node: %v %v", ok, err)
	}
	r.hub.delivered = 0
	if ok, err := r.hr.SendRemote(worker(), "cc-sandbox:mgr", "report", "x"); ok || err != nil {
		t.Fatalf("nobody took it: %v %v", ok, err)
	}
	if got := r.j.types(); strings.Count(got, "msg.dropped") != 2 {
		t.Fatalf("journal: %s", got)
	}
	for _, e := range r.j.ev {
		if strings.Contains(string(e.Data), "x\"") && strings.Contains(string(e.Data), "text") {
			t.Fatalf("the message text reached the journal: %s", e.Data)
		}
	}
}

func TestAnExpiredRelayCredentialIsReplacedOnce(t *testing.T) {
	calls := 0
	r := newRig(t, fixedMint("sbp1.relay-token-0001", &calls))
	r.hub.auth401 = 1
	ok, err := r.hr.SendRemote(worker(), "cc-sandbox:mgr", "report", "x")
	if !ok || err != nil || calls != 2 {
		t.Fatalf("%v %v minted %d times, want 2 (the first was refused)", ok, err, calls)
	}
	r.hub.auth401 = 5
	if ok, err := r.hr.SendRemote(worker(), "cc-sandbox:mgr", "report", "y"); ok || err == nil {
		t.Fatalf("a hub that keeps refusing must be an error, not a success: %v %v", ok, err)
	}
}

func TestNoMinterMeansAnErrorNotAnUnsignedSend(t *testing.T) {
	r := newRig(t, nil)
	if ok, err := r.hr.SendRemote(worker(), "cc-sandbox:mgr", "report", "x"); ok || err == nil || len(r.hub.got) != 0 {
		t.Fatalf("%v %v %v", ok, err, r.hub.got)
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
	tok, err := rootMinter()(context.Background(), "s0abc12345", 3, "PUBKEY_B64")
	if err != nil || tok != "sbp1.minted-by-root-0001" {
		t.Fatalf("%q %v", tok, err)
	}
	b, _ := os.ReadFile(argsFile)
	if strings.TrimSpace(string(b)) != "relay-mint -- s0abc12345 3 PUBKEY_B64" {
		t.Fatalf("args: %q", b)
	}
	os.WriteFile(root, []byte("#!/bin/sh\necho 'not json'\n"), 0o755)
	if _, err := rootMinter()(context.Background(), "s0abc12345", 3, "k"); err == nil {
		t.Fatal("a non-token answer was accepted")
	}
}

func pubOf(k *relayKey) string {
	return base64.RawURLEncoding.EncodeToString(k.priv.Public().(ed25519.PublicKey))
}
