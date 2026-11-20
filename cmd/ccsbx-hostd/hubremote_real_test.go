// M2b against the REAL session-bus hub (dotfiles branch n1-g-hub: B10/B11 plus the downlink payload and
// node changes), two logical machines on one host: "hp" (a fenced worker) and "mac" (a fenced manager),
// each a full hubRemote with its own spoke credential. Why: the fake hub proves hostd's logic; this proves
// the real hub carries a stamp end to end, resolves a node to an entry, drops what it must, and that the
// refusal of a forged message happens at the hub and, independently, at the receiver. It only runs from
// tests/hub-spoke-test.sh (which starts the hub and sets the variables). T:xmachine-msg proper (a real
// Mac) is tests/xmachine-mac-test.sh and waits for the Mac.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hubc"
	"github.com/landovsky/cc-sandbox/internal/hubspoke"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/stamp"
)

type realMachine struct {
	name string
	hr   *hubRemote
	out  *xOut
	j    *xJ
	sp   *hubspoke.Spoke
	tok  string
	mu   sync.Mutex
	warn []string // what the hub said about a track that did not fully hold
}

func (m *realMachine) warnings() []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]string(nil), m.warn...)
}

func adminPost(t *testing.T, url, admin, path string, body any) map[string]any {
	t.Helper()
	b, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", url+path, bytes.NewReader(b))
	req.Header.Set("Authorization", "Bearer "+admin)
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var m map[string]any
	rb, _ := io.ReadAll(resp.Body)
	json.Unmarshal(rb, &m)
	if resp.StatusCode/100 != 2 {
		t.Fatalf("%s: %d %s", path, resp.StatusCode, rb)
	}
	return m
}

// startMachine starts a logical machine's hostd. ttl (optional, seconds) is the lease of its hub entries: a short one
// makes it renew (and so re-bind) every ttl/3 seconds.
func startMachine(t *testing.T, ctx context.Context, url, admin, name string, s msg.Session, ttl ...int) *realMachine {
	t.Helper()
	tok := adminPost(t, url, admin, "/v1/principals", map[string]any{"role": "spoke", "machine": name, "name": "ccsbx", "trees": []string{"cc-sandbox"}})["token"].(string)
	out, j := &xOut{}, &xJ{}
	rt := &msg.Router{Dir: &xDir{[]msg.Session{s}}, Out: out, Idle: xIdle{}, StateDir: t.TempDir()}
	rm := &realMachine{name: name}
	client := &hubc.Client{URL: url, Token: func() (string, error) { return tok, nil }, Warn: func(w string) { rm.mu.Lock(); rm.warn = append(rm.warn, w); rm.mu.Unlock() }}
	// (hostd's own principal: spoke:<machine>/ccsbx, fenced entries and its own downlink only)
	// the stand-in for `ccsbx-root relay-mint`: the hub's admin API, which hostd itself never holds
	mint := func(_ context.Context, sid string, gen int, pub string, _ bool) (string, error) {
		// may_reach is the session's MANAGER only, as root derives it (Tomas's open question #3cy2: peers stay
		// reachable only when they are fenced, over the signed path)
		var reach []string
		if s.Scope.Manager.Node != "" && s.Scope.Manager.Machine != "" {
			reach = append(reach, s.Scope.Manager.Machine+"/"+s.Scope.Manager.Node)
		}
		m := adminPost(t, url, admin, "/v1/principals", map[string]any{"role": "relay", "sid": sid, "gen": fmt.Sprint(gen), "pub": pub, "may_reach": reach})
		return m["token"].(string), nil
	}
	hr := newHubRemote(client, url, name, rt, j, t.Logf, mint, t.TempDir())
	rt.Uplink = hr
	sp := hubspoke.New(hubspoke.Config{
		Client: client, Machine: name, Name: "ccsbx", TTL: leaseTTL(ttl), PollWait: time.Second,
		// the production entry builder, so what the real hub sees is what hostd really publishes
		Entries: func() []map[string]any {
			return fencedEntries(name, []msg.Session{s}, func(string, int) (time.Time, bool) { return time.Now(), true })
		},
		OnDeliver: func(c context.Context, f hubc.Frame, d hubc.Deliver) hubspoke.Result { return hr.onDeliver(c, f, d) },
		Logf:      t.Logf,
	})
	go sp.Run(ctx)
	rm.hr, rm.out, rm.j, rm.sp, rm.tok = hr, out, j, sp, tok
	return rm
}

func leaseTTL(l []int) int {
	if len(l) > 0 {
		return l[0]
	}
	return 60
}

func waitLines(t *testing.T, m *realMachine, n int) {
	t.Helper()
	for i := 0; i < 150; i++ {
		m.out.mu.Lock()
		got := len(m.out.lines)
		m.out.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("%s: %d lines typed, wanted %d", m.name, len(m.out.lines), n)
}

func TestRealHubTwoMachinesExchangeSignedMessagesAndRefuseForgeries(t *testing.T) {
	url, admin := os.Getenv("CCSBX_REAL_HUB_URL"), os.Getenv("CCSBX_REAL_HUB_ADMIN")
	if url == "" || admin == "" || os.Getenv("CCSBX_REAL_HUB_M2B") == "" {
		t.Skip("needs the n1-g-hub test hub: run tests/hub-spoke-test.sh")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := msg.Session{Sid: "s0worker01", Label: "cc-sandbox:worker", Realm: "production", Gen: 1, Live: true, Started: time.Now().Add(-time.Hour)}
	worker.Scope.Manager.Node = "cc-sandbox:mgr"
	worker.Scope.ReplyWindowS = 3600
	mgr := msg.Session{Sid: "s0mgr00001", Label: "cc-sandbox:mgr", Realm: "production", Gen: 1, Live: true, Started: time.Now().Add(-time.Hour)}
	mgr.Scope.Peers = []string{"cc-sandbox:worker"}
	mgr.Scope.ReplyWindowS = 3600
	hp := startMachine(t, ctx, url, admin, "hp", worker)
	mac := startMachine(t, ctx, url, admin, "mac", mgr)
	// both sessions are on the hub with their nodes
	for i := 0; i < 100; i++ {
		es, _ := hp.hr.hub.Sessions(ctx)
		if hubc.FindNode(es, "cc-sandbox:mgr") != nil && hubc.FindNode(es, "cc-sandbox:worker") != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// 1. T:xmachine-msg (on one host): the worker's message reaches the manager's pane on the other machine,
	// signed, verified there, scope-checked there, typed once with a fenced stamp that names the sender.
	ok, err := sendVia(hp, worker, "cc-sandbox:mgr", "report", "step 1 done")
	if !ok || err != nil {
		t.Fatalf("send: %v %v", ok, err)
	}
	waitLines(t, mac, 1)
	l := mac.out.lines[0]
	if !strings.Contains(l, "· fenced ·") || !strings.Contains(l, "hp/s0worker01") || !strings.Contains(l, "cc-sandbox:worker") || !strings.Contains(l, "step 1 done") {
		t.Fatalf("what the manager's pane got: %q", l)
	}

	// 2. ... and the manager's reply goes back the same way (no cockpit anywhere).
	hp.out.mu.Lock()
	hp.out.lines = nil
	hp.out.mu.Unlock()
	if ok, err := sendVia(mac, mgr, "cc-sandbox:worker", "message", "thanks"); !ok || err != nil {
		t.Fatalf("reply: %v %v", ok, err)
	}
	waitLines(t, hp, 1)

	// 3. T:xmachine-forged-from, at the hub: a relay that signs for one sid but sends as another, or tampers
	// with the body, is refused by the hub (403) and nothing is typed.
	pub, priv, _ := stamp.NewKey()
	rtok := adminPost(t, url, admin, "/v1/principals", map[string]any{"role": "relay", "sid": "s0forger01", "gen": "1", "pub": pub})["token"].(string)
	es, _ := hp.hr.hub.Sessions(ctx)
	mid := hubc.FindNode(es, "cc-sandbox:mgr")["id"].(string)
	c := &hubc.Client{URL: url, Token: func() (string, error) { return rtok, nil }}
	good := stamp.Sign(priv, stamp.Fields{From: "s0forger01", To: mid, Kind: "report", Body: "hello"}, time.Now())
	before := len(mac.out.lines)
	for name, req := range map[string]hubc.SendReq{
		"a forged from.sid":  {To: mid, Text: "hello", Kind: "report", Sig: good, From: map[string]string{"sid": "s0worker01", "fenced": "true"}},
		"a body edited":      {To: mid, Text: "hello, edited", Kind: "report", Sig: good, From: map[string]string{"sid": "s0forger01", "fenced": "true"}},
		"an unsigned fenced": {To: mid, Text: "hello", Kind: "report", From: map[string]string{"sid": "s0forger01", "fenced": "true"}},
		"a changed kind":     {To: mid, Text: "hello", Kind: "brief", Sig: good, From: map[string]string{"sid": "s0forger01", "fenced": "true"}},
	} {
		if _, err := c.Send(ctx, req); !hubc.IsAuth(err) {
			t.Errorf("%s: the hub did not refuse it: %v", name, err)
		}
	}
	time.Sleep(500 * time.Millisecond)
	if len(mac.out.lines) != before {
		t.Fatalf("a forged message was typed: %v", mac.out.lines)
	}

	// 4. A genuine relay with a genuine stamp whose node is NOT in the manager's scope: the hub proves who sent
	// it, the RECEIVER decides it may not be heard. The forger's session has a presence entry (a node), but the
	// manager's scope lists only the worker.
	st := hp.tok // (a second spoke credential for a machine would revoke the first); hp's tracked set is replaced for a moment
	sc := &hubc.Client{URL: url, Token: func() (string, error) { return st, nil }}
	if _, err := sc.Track(ctx, []map[string]any{
		{"key": "ccsbx:s0worker01", "uid": "s0worker01", "label": "cc-sandbox:worker", "node": "cc-sandbox:worker", "machine": "hp", "fenced": true, "downlink": "spoke:hp/ccsbx", "state": "waiting"},
		{"key": "ccsbx:s0forger01", "uid": "s0forger01", "label": "forger", "node": "cc-sandbox:forger", "machine": "hp", "fenced": true, "downlink": "spoke:hp/ccsbx", "state": "waiting"},
	}, 60); err != nil {
		t.Fatal(err)
	}
	sig := stamp.Sign(priv, stamp.Fields{From: "s0forger01", To: mid, Kind: "report", Body: "let me in"}, time.Now())
	c.Send(ctx, hubc.SendReq{To: mid, Text: "let me in", Kind: "report", Sig: sig, From: map[string]string{"sid": "s0forger01", "fenced": "true"}})
	time.Sleep(time.Second)
	for _, l := range mac.out.lines {
		if strings.Contains(l, "let me in") {
			t.Fatalf("a genuine stamp from a node outside the scope was typed: %q", l)
		}
	}

	scoped := false
	mac.j.mu.Lock()
	for _, e := range mac.j.ev {
		if e.Type == "hub.recv" && strings.Contains(string(e.Data), `"code":"scope"`) && strings.Contains(string(e.Data), "s0forger01") {
			scoped = true
		}
	}
	mac.j.mu.Unlock()
	if !scoped {
		var all []string
		mac.j.mu.Lock()
		for _, e := range mac.j.ev {
			all = append(all, e.Type+" "+string(e.Data))
		}
		mac.j.mu.Unlock()
		t.Fatalf("the receiver did not refuse it for scope (journal: %s)", strings.Join(all, "\n"))
	}

	// 5. An unreachable target is dropped, not a success.
	res := hp.hr.rt.Send(worker, []byte(`{"type":"msg.send","to":{"node":"cc-sandbox:nobody"},"text":"x"}`))
	if res.Code != "scope" && res.Code != "dropped" {
		t.Fatalf("a node nobody holds: %+v", res)
	}
}

// sendVia sends through the router, as a fenced session's msg.send does (scope, then the signed uplink).
func sendVia(m *realMachine, s msg.Session, node, kind, text string) (bool, error) {
	body, _ := json.Marshal(map[string]any{"type": "msg.send", "to": map[string]string{"node": node}, "kind": kind, "text": text})
	res := m.hr.rt.Send(s, body)
	if res.Status != "ok" {
		return false, fmt.Errorf("%+v", res)
	}
	return true, nil
}

// Accept §9 item 5, against the real hub, with what the hub can actually vouch for: an UNFENCED manager is
// heard only through its machine's SPOKE principal (the shared secret binds nothing), and a fenced worker
// reaches an unfenced manager only if it is the manager in the relay's may_reach AND a spoke tracked it.
func TestRealHubFencedAndUnfencedManagersReachEachOther(t *testing.T) {
	url, admin := os.Getenv("CCSBX_REAL_HUB_URL"), os.Getenv("CCSBX_REAL_HUB_ADMIN")
	if url == "" || admin == "" || os.Getenv("CCSBX_REAL_HUB_M2B") == "" {
		t.Skip("needs the n1-g-hub2 test hub: run tests/hub-spoke-test.sh")
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	worker := msg.Session{Sid: "s0worker02", Label: "cc-sandbox:worker2", Realm: "production", Gen: 1, Live: true, Started: time.Now().Add(-time.Hour)}
	worker.Scope.Manager.Node = "cc-sandbox:umgr"
	worker.Scope.Manager.Machine = "mac3"
	worker.Scope.ReplyWindowS = 3600
	hp := startMachine(t, ctx, url, admin, "hp2", worker)

	// the Mac's owner sessions track through the Mac's SPOKE principal (a node within its trees)
	macTok := adminPost(t, url, admin, "/v1/principals", map[string]any{"role": "spoke", "machine": "mac3", "trees": []string{"cc-sandbox"}})["token"].(string)
	mac := &hubc.Client{URL: url, Token: func() (string, error) { return macTok, nil }}
	owner := &hubc.Client{URL: url, Token: func() (string, error) { return admin, nil }} // the SHARED secret
	entry := func(label, node, uid, down string) map[string]any {
		_ = down
		return map[string]any{"key": "mac3/" + label, "label": label, "node": node, "uid": uid, "machine": "mac3", "pane": "%1", "state": "waiting"}
	}
	if _, err := mac.Track(ctx, []map[string]any{
		entry("umgr", "cc-sandbox:umgr", "eeeeeeee-0000-4000-8000-0000000000a1", "spoke:mac3/umgr"),
		entry("uother", "cc-sandbox:uother", "eeeeeeee-0000-4000-8000-0000000000a2", "spoke:mac3/uother"),
	}, 120); err != nil {
		t.Fatal(err)
	}
	var umgrID, uotherID, workerID string
	// (the owner's unfenced entries route to the machine's own spoke:<m> downlink: the hub refuses a named one)
	for i := 0; i < 100 && (umgrID == "" || uotherID == "" || workerID == ""); i++ {
		es, _ := owner.Sessions(ctx)
		if e := hubc.FindNode(es, "cc-sandbox:umgr"); e != nil {
			umgrID, _ = e["id"].(string)
		}
		if e := hubc.FindNode(es, "cc-sandbox:uother"); e != nil {
			uotherID, _ = e["id"].(string)
		}
		if e := hubc.FindSid(es, "s0worker02"); e != nil {
			workerID, _ = e["id"].(string)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if umgrID == "" || uotherID == "" || workerID == "" {
		t.Fatalf("not on the hub with their nodes: umgr=%q uother=%q worker=%q (the spoke's trees must keep an unfenced node)", umgrID, uotherID, workerID)
	}
	typed := func(sub string) bool {
		hp.out.mu.Lock()
		defer hp.out.mu.Unlock()
		for _, l := range hp.out.lines {
			if strings.Contains(l, sub) {
				return true
			}
		}
		return false
	}

	// 1. the unfenced manager, posting with ITS MACHINE'S spoke credential, is bound by the hub, confirmed by
	// hostd against the presence list, typed as unfenced (hub), scope checked on its node (the worker's manager).
	res, err := mac.Send(ctx, hubc.SendReq{To: workerID, Text: "please report", From: map[string]string{"id": umgrID, "machine": "mac3"}})
	if err != nil || res.Delivered != 1 {
		t.Fatalf("unfenced manager -> fenced worker: %+v %v", res, err)
	}
	waitLines(t, hp, 1)
	if l := hp.out.lines[0]; !strings.Contains(l, "unfenced (hub)") || !strings.Contains(l, "cc-sandbox:umgr") || !strings.Contains(l, "please report") {
		t.Fatalf("what the worker's pane got: %q", l)
	}

	refused := func(why, text string, c *hubc.Client, from map[string]string) {
		t.Helper()
		c.Send(ctx, hubc.SendReq{To: workerID, Text: text, From: from})
		time.Sleep(400 * time.Millisecond)
		if typed(text) {
			t.Fatalf("%s: was typed", why)
		}
	}
	// 2. the shared secret is a forgery vector: any owner process can claim the manager's id
	refused("a legacy poster claiming the spoke-tracked manager, no machine", "forged-legacy-1", owner, map[string]string{"id": umgrID})
	refused("a legacy poster claiming it WITH the machine", "forged-legacy-2", owner, map[string]string{"id": umgrID, "machine": "mac3"})
	// ... even for an entry the shared secret itself tracked, which the hub binds ("legacy") but hostd refuses
	if _, err := owner.Track(ctx, []map[string]any{{"key": "ws/legacy", "label": "legacyunf", "node": "cc-sandbox:legacyonly", "uid": "eeeeeeee-0000-4000-8000-0000000000a3", "machine": "mac3", "pane": "%1", "state": "waiting", "downlink": "spoke:mac3/legacyunf"}}, 120); err != nil {
		t.Fatal(err)
	}
	es, _ := owner.Sessions(ctx)
	var legacyID string
	for _, e := range es {
		if e["label"] == "legacyunf" {
			legacyID, _ = e["id"].(string)
		}
	}
	refused("an entry tracked by the shared secret, posted with it", "forged-legacy-3", owner, map[string]string{"id": legacyID, "machine": "mac3"})
	// 3. from.machine left out, with the right credential
	refused("from.machine omitted", "no-machine", mac, map[string]string{"id": umgrID})
	// 4. an unsigned claim of a FENCED identity, end to end
	refused("an unsigned claim of the fenced worker's id", "claims-fenced", mac, map[string]string{"id": workerID, "machine": "mac3"})
	// 5. a bound sender outside the worker's scope (uother is not its manager)
	refused("a bound sender outside the scope", "out-of-scope", mac, map[string]string{"id": uotherID, "machine": "mac3"})

	// 5b. the two principals of a machine (hub-enforced): the owner's cannot read hostd's downlink, nor track a
	// fenced entry; hostd's cannot track an unfenced one. (Why: a process of the owner's uid holds the owner's token.)
	if _, _, err := mac.Poll(ctx, "spoke:mac3/ccsbx", 0, time.Second); !hubc.IsForbidden(err) {
		t.Errorf("the owner's spoke polling hostd's downlink: want 403, got %v", err)
	}
	if _, err := mac.Track(ctx, []map[string]any{{"key": "ccsbx:forged", "label": "forged", "machine": "mac3", "fenced": true, "uid": "eeeeeeee-0000-4000-8000-0000000000a9", "downlink": "spoke:mac3/ccsbx"}}, 120); !hubc.IsForbidden(err) {
		t.Errorf("the owner's spoke tracking a fenced entry: want 403, got %v", err)
	}
	if _, err := (&hubc.Client{URL: url, Token: func() (string, error) { return hp.tok, nil }}).Track(ctx, []map[string]any{{"key": "hp2/%1", "label": "unf", "machine": "hp2", "pane": "%1"}}, 120); !hubc.IsForbidden(err) {
		t.Errorf("hostd's principal tracking an unfenced entry: want 403, got %v", err)
	}

	// 5c. (round 4) the owner token of a THIRD machine cannot squat a node a fenced session holds, nor one
	// another principal's live entry holds: the hub refuses (409), so nobody is heard as the fenced manager
	// and the real channel is not cut.
	x4Tok := adminPost(t, url, admin, "/v1/principals", map[string]any{"role": "spoke", "machine": "x4", "trees": []string{"cc-sandbox"}})["token"].(string)
	x4 := &hubc.Client{URL: url, Token: func() (string, error) { return x4Tok, nil }}
	squat := func(node string) error {
		_, err := x4.Track(ctx, []map[string]any{{"key": "x4/%1", "label": "squat", "node": node, "uid": "eeeeeeee-0000-4000-8000-0000000000c1", "machine": "x4", "pane": "%1", "state": "waiting"}}, 120)
		return err
	}
	if err := squat("cc-sandbox:worker2"); !hubc.IsConflict(err) {
		t.Errorf("an owner token squatting a FENCED session's node: want 409, got %v", err)
	}
	if err := squat("cc-sandbox:umgr"); !hubc.IsConflict(err) {
		t.Errorf("an owner token squatting another principal's live node: want 409, got %v", err)
	}
	if es, _ := owner.Sessions(ctx); hubc.FindSid(es, "eeeeeeee-0000-4000-8000-0000000000c1") != nil {
		t.Error("a refused squat was tracked")
	}

	// 6. the fenced worker reaches the unfenced manager (its node is in the relay's may_reach: the manager
	// only), typed there with the fenced stamp; the test plays the manager's spoke by polling its downlink.
	got := make(chan hubc.Frame, 4)
	go func() {
		for ctx.Err() == nil {
			fs, _, err := mac.Poll(ctx, "spoke:mac3", 0, time.Second)
			if err != nil {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			for _, f := range fs {
				var d hubc.Deliver
				json.Unmarshal(f.Payload, &d)
				mac.Ack(ctx, hubc.Ack{Topic: "spoke:mac3", Seq: f.Seq, AckTopic: d.AckTopic, AckRef: d.AckRef, OK: true, Confirmed: true})
				got <- f
			}
		}
	}()
	if ok, err := sendVia(hp, worker, "cc-sandbox:umgr", "report", "done"); !ok || err != nil {
		t.Fatalf("fenced worker -> unfenced manager in may_reach: %v %v", ok, err)
	}
	select {
	case f := <-got:
		var d hubc.Deliver
		json.Unmarshal(f.Payload, &d)
		if d.Trust != "fenced" || !strings.Contains(d.Text, "· fenced ·") || !strings.Contains(d.Text, "done") {
			t.Fatalf("what the unfenced manager's pane gets: %+v", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the unfenced manager got nothing")
	}
	// ... but NOT an unfenced peer (not in may_reach), straight with the relay credential: 403 at the hub
	kpriv, ktok, err := hp.hr.relayFor(ctx, worker, "")
	if err != nil {
		t.Fatal(err)
	}
	rc := &hubc.Client{URL: url, Token: func() (string, error) { return ktok, nil }}
	sig := stamp.Sign(kpriv, stamp.Fields{From: worker.Sid, To: uotherID, Kind: "report", Body: "x"}, time.Now())
	if _, err := rc.Send(ctx, hubc.SendReq{To: uotherID, Text: "x", Kind: "report", Sig: sig, From: map[string]string{"sid": worker.Sid, "fenced": "true"}}); !hubc.IsForbidden(err) {
		t.Fatalf("a relay reached an unfenced node outside its may_reach: %v", err)
	}

	// 7. the shared secret (every owner session on every machine holds it) is conflict-checked too: it cannot
	// re-track the manager's node elsewhere to make it ambiguous and cut the worker's channel (the hub refuses,
	// 409; hostd's own handling of a node held twice is covered by the unit tests).
	if _, err := owner.Track(ctx, []map[string]any{{"key": "ws/evil", "label": "evil", "node": "cc-sandbox:umgr", "uid": "eeeeeeee-0000-4000-8000-0000000000a4", "machine": "mac9", "pane": "%1", "state": "waiting"}}, 120); !hubc.IsConflict(err) {
		t.Fatalf("the shared secret re-tracking a held node: want 409, got %v", err)
	}

	// 9. (rounds 5 and 6) squatting an ABSENT manager's node. The worker's manager umgr5 is an unfenced session
	// on mac3 that is not tracked right now (a restart, a closed laptop). An owner token of a third machine, x4,
	// publishes its node (nobody has said whose it is yet, so the hub accepts it). Then the worker starts: its
	// record binds umgr5 to mac3 (the owner's start spec), but a bind NEVER moves a claim whose machine has a live
	// entry for the node, so while the squatter renews, the claim stays and hostd says so. Nothing leaks meanwhile:
	// hostd hears and sends by the record's machine. When the squatter stops renewing, the next bind takes the claim.
	w3 := msg.Session{Sid: "s0worker03", Label: "cc-sandbox:worker3", Realm: "production", Gen: 1, Live: true, Started: time.Now().Add(-time.Hour)}
	w3.Scope.Manager.Node, w3.Scope.Manager.Machine, w3.Scope.ReplyWindowS = "cc-sandbox:umgr5", "mac3", 3600
	squatEntry := map[string]any{"key": "x4/%1", "label": "squat", "node": "cc-sandbox:umgr5", "uid": "eeeeeeee-0000-4000-8000-0000000000b1", "machine": "x4", "pane": "%1", "state": "waiting"}
	x4Client := &hubc.Client{URL: url, Token: func() (string, error) { return x4Tok, nil }}
	if _, err := x4Client.Track(ctx, []map[string]any{squatEntry}, 4); err != nil {
		t.Fatalf("a squat with no worker bound to the node yet is accepted (nobody has said whose it is): %v", err)
	}
	hp3 := startMachine(t, ctx, url, admin, "hp3", w3, 3) // renews (and so re-binds) every second
	var w3ID, squatID string
	for i := 0; i < 100 && (w3ID == "" || squatID == ""); i++ {
		es, _ := owner.Sessions(ctx)
		if e := hubc.FindSid(es, w3.Sid); e != nil {
			w3ID, _ = e["id"].(string)
		}
		if e := hubc.FindSid(es, "eeeeeeee-0000-4000-8000-0000000000b1"); e != nil {
			squatID, _ = e["id"].(string)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if w3ID == "" || squatID == "" {
		t.Fatalf("worker3 %q or the squatter %q is not on the hub", w3ID, squatID)
	}
	// B1: while the squatter is live the bind did not take the claim, and hostd was told
	if ws := hp3.warnings(); len(ws) == 0 || !strings.Contains(ws[0], "claimed by x4") {
		t.Errorf("hostd was not told that the manager's node is claimed by another machine: %v", ws)
	}
	// the squatter, bound correctly by its own spoke and the only holder, speaks as the manager: not heard
	x4Client.Send(ctx, hubc.SendReq{To: w3ID, Text: "squatter speaking as your manager", From: map[string]string{"id": squatID, "machine": "x4"}})
	time.Sleep(600 * time.Millisecond)
	hp3.out.mu.Lock()
	for _, l := range hp3.out.lines {
		if strings.Contains(l, "squatter speaking") {
			t.Fatalf("the squatter was heard as the absent manager: %q", l)
		}
	}
	hp3.out.mu.Unlock()
	macEntries := func() []map[string]any {
		return []map[string]any{
			entry("umgr", "cc-sandbox:umgr", "eeeeeeee-0000-4000-8000-0000000000a1", ""),
			entry("uother", "cc-sandbox:uother", "eeeeeeee-0000-4000-8000-0000000000a2", ""),
			entry("umgr5", "cc-sandbox:umgr5", "eeeeeeee-0000-4000-8000-0000000000b2", ""),
		}
	}
	// the squatter lapses (its 4 s lease); hostd's next renewal binds umgr5 to mac3; the real manager gets in
	var trackErr error
	for i := 0; i < 80; i++ {
		if _, trackErr = mac.Track(ctx, macEntries(), 120); trackErr == nil {
			break
		}
		if !hubc.IsConflict(trackErr) {
			t.Fatal(trackErr)
		}
		time.Sleep(250 * time.Millisecond)
	}
	if trackErr != nil {
		t.Fatalf("the real manager never got its node back after the squatter lapsed: %v", trackErr)
	}
	if _, err := x4Client.Track(ctx, []map[string]any{squatEntry}, 120); !hubc.IsConflict(err) {
		t.Errorf("the squatter's re-track once the node is bound to mac3: want 409, got %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	// the worker's report goes to the REAL manager (mac3), never to the squatter
	if ok, err := sendVia(hp3, w3, "cc-sandbox:umgr5", "report", "secret report"); !ok || err != nil {
		t.Fatalf("worker3 -> its real manager: %v %v", ok, err)
	}
	select {
	case f := <-got:
		var d hubc.Deliver
		json.Unmarshal(f.Payload, &d)
		if !strings.Contains(d.Text, "secret report") {
			t.Fatalf("what mac3 got: %+v", d)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the real manager got nothing")
	}
	if fs, _, err := x4Client.Poll(ctx, "spoke:x4", 0, time.Second); err == nil && len(fs) != 0 {
		t.Fatalf("the squatter's downlink got the worker's report: %v", fs)
	}

	// 10. (round 6, B0-B3) a start spec that names the WRONG machine for a live manager (a typo, or a hostile
	// spec): hostd's bind does not move the claim, the real manager is not locked out, and hostd says so.
	w4 := msg.Session{Sid: "s0worker04", Label: "cc-sandbox:worker4", Realm: "production", Gen: 1, Live: true, Started: time.Now().Add(-time.Hour)}
	w4.Scope.Manager.Node, w4.Scope.Manager.Machine, w4.Scope.ReplyWindowS = "cc-sandbox:umgr5", "x6", 3600
	hp4 := startMachine(t, ctx, url, admin, "hp4", w4, 3)
	time.Sleep(1500 * time.Millisecond) // two renewals
	if ws := hp4.warnings(); len(ws) == 0 || !strings.Contains(ws[0], "claimed by mac3") {
		t.Errorf("the daemon was not told its session's manager node is claimed by mac3: %v", ws)
	}
	for i := 0; i < 3; i++ {
		if _, err := mac.Track(ctx, macEntries(), 120); err != nil {
			t.Fatalf("the real manager was locked out by a wrong machine in someone's spec (renewal %d): %v", i, err)
		}
		time.Sleep(700 * time.Millisecond)
	}
}
