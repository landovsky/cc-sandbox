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

func startMachine(t *testing.T, ctx context.Context, url, admin, name string, s msg.Session) *realMachine {
	t.Helper()
	tok := adminPost(t, url, admin, "/v1/principals", map[string]any{"role": "spoke", "machine": name, "trees": []string{"cc-sandbox"}})["token"].(string)
	out, j := &xOut{}, &xJ{}
	rt := &msg.Router{Dir: &xDir{[]msg.Session{s}}, Out: out, Idle: xIdle{}, StateDir: t.TempDir()}
	client := &hubc.Client{URL: url, Token: func() (string, error) { return tok, nil }}
	// the stand-in for `ccsbx-root relay-mint`: the hub's admin API, which hostd itself never holds
	mint := func(_ context.Context, sid string, gen int, pub string, _ bool) (string, error) {
		m := adminPost(t, url, admin, "/v1/principals", map[string]any{"role": "relay", "sid": sid, "gen": fmt.Sprint(gen), "pub": pub})
		return m["token"].(string), nil
	}
	hr := newHubRemote(client, url, name, rt, j, t.Logf, mint, t.TempDir())
	rt.Uplink = hr
	sp := hubspoke.New(hubspoke.Config{
		Client: client, Machine: name, Name: "ccsbx", TTL: 60, PollWait: time.Second,
		// the production entry builder, so what the real hub sees is what hostd really publishes
		Entries: func() []map[string]any {
			return fencedEntries(name, []msg.Session{s}, func(string, int) (time.Time, bool) { return time.Now(), true })
		},
		OnDeliver: func(c context.Context, f hubc.Frame, d hubc.Deliver) hubspoke.Result { return hr.onDeliver(c, f, d) },
		Logf:      t.Logf,
	})
	go sp.Run(ctx)
	return &realMachine{name: name, hr: hr, out: out, j: j, sp: sp, tok: tok}
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
