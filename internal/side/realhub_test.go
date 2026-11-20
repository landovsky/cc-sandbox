// The sidecar's bus against the REAL session-bus hub (spare loopback port; tests/hub-spoke-test.sh starts it and sets the
// variables, and this test skips without them). Why: bus_test.go proves the sender's rules against a fake hub; this proves
// the real hub accepts the relay token the sidecar holds, lists to it ONLY the manager it may reach (read_scoped), carries
// the stamp end to end so the receiving machine can verify it with the key the hub vouches for, and refuses to let that
// relay reach anything else.

package side

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/hubc"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/stamp"
	"github.com/landovsky/cc-sandbox/internal/token"
)

func rhPost(t *testing.T, url, admin, path string, body any) map[string]any {
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
	raw, _ := io.ReadAll(resp.Body)
	var out map[string]any
	json.Unmarshal(raw, &out)
	if resp.StatusCode >= 300 {
		t.Fatalf("POST %s: %d %s", path, resp.StatusCode, raw)
	}
	return out
}

func TestRealHubTheSidecarBusReachesItsManagerAndNothingElse(t *testing.T) {
	url, admin := os.Getenv("CCSBX_REAL_HUB_URL"), os.Getenv("CCSBX_REAL_HUB_ADMIN")
	if url == "" || admin == "" || os.Getenv("CCSBX_REAL_HUB_M2B") == "" {
		t.Skip("needs the n1-g-hub2 test hub: run tests/hub-spoke-test.sh")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const sid, mgrNode, otherNode = "s0sidecar01", "cc-sandbox:sidecar-mgr", "cc-sandbox:sidecar-other" // unique: the hub is shared with the hostd real-hub tests, and a node is claimed by one machine

	// the manager's machine ("mac4") tracks two unfenced sessions through its spoke principal
	macTok := rhPost(t, url, admin, "/v1/principals", map[string]any{"role": "spoke", "machine": "mac4", "trees": []string{"cc-sandbox"}})["token"].(string)
	mac := &hubc.Client{URL: url, Token: func() (string, error) { return macTok, nil }}
	owner := &hubc.Client{URL: url, Token: func() (string, error) { return admin, nil }}
	entry := func(label, node, uid string) map[string]any {
		return map[string]any{"key": "mac4/" + label, "label": label, "node": node, "uid": uid, "machine": "mac4", "pane": "%1", "state": "waiting"}
	}
	if _, err := mac.Track(ctx, []map[string]any{entry("umgr", mgrNode, "dddddddd-0000-4000-8000-0000000000b1"), entry("uother", otherNode, "dddddddd-0000-4000-8000-0000000000b2")}, 120); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 100; i++ {
		es, _ := owner.Sessions(ctx)
		if hubc.FindNode(es, mgrNode) != nil && hubc.FindNode(es, otherNode) != nil {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// the sidecar's generation key, registered as a relay principal whose one unfenced target is the manager (what pod-load does)
	pub, priv, err := stamp.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	rtok := rhPost(t, url, admin, "/v1/principals", map[string]any{"role": "relay", "sid": sid, "gen": "1", "pub": pub, "may_reach": []string{}, "manager": "mac4/" + mgrNode})["token"].(string) // as ccsbx-root pod-load mints: the unfenced manager is `manager`, may_reach is fenced targets only
	relay := &hubc.Client{URL: url, Token: func() (string, error) { return rtok, nil }}

	// read_scoped: the relay sees the manager and not the other session on the same machine
	es, err := relay.Sessions(ctx)
	if err != nil {
		t.Fatalf("a relay token cannot read presence (the hub lacks read_scoped): %v", err)
	}
	if hubc.FindNode(es, mgrNode) == nil || hubc.FindNode(es, otherNode) != nil {
		t.Fatalf("the relay's view of the hub: %v (want the manager only)", es)
	}

	sc := msg.Scope{}
	sc.Manager.Node, sc.Manager.Machine = mgrNode, "mac4"
	sc.Peers, sc.PeerMachines = []string{otherNode}, map[string]string{otherNode: "mac4"} // in the record's scope, but not the relay's manager
	bus, err := NewBus(BusConfig{HubURL: url, Machine: "hp9", Sid: sid, Label: "cc-sandbox:w", Scope: sc, Timeout: 20 * time.Second,
		Cred: func() (RelayCred, bool) {
			return RelayCred{Token: rtok, Key: &token.GenKey{Private: priv, Public: ed25519.PublicKey(priv.Public().(ed25519.PublicKey))}}, true
		}})
	if err != nil {
		t.Fatal(err)
	}

	// the manager's machine receives frames on its own spoke topic and acks them, so the hub's /deliver can answer
	type got struct{ d hubc.Deliver }
	frames := make(chan got, 4)
	go func() {
		var after int64
		for ctx.Err() == nil {
			fs, next, err := mac.Poll(ctx, "spoke:mac4", after, 3*time.Second)
			if err != nil {
				time.Sleep(200 * time.Millisecond)
				continue
			}
			for _, f := range fs {
				var d hubc.Deliver
				if json.Unmarshal(f.Payload, &d) == nil {
					frames <- got{d}
					mac.Ack(ctx, hubc.Ack{Topic: "spoke:mac4", Seq: f.Seq, AckTopic: d.AckTopic, AckRef: d.AckRef, OK: true, Confirmed: true})
				}
			}
			after = next
		}
	}()

	rep := make(chan hostd.Reply, 1)
	go func() {
		rep <- bus.Forward(ctx, hostd.Request{Type: "msg.send", Body: json.RawMessage(`{"to":{"node":"` + mgrNode + `"},"kind":"report","text":"done from the sidecar"}`)})
	}()
	var d hubc.Deliver
	select {
	case g := <-frames:
		d = g.d
	case <-time.After(30 * time.Second):
		t.Fatal("the manager's machine received nothing")
	}
	if r := <-rep; r.Status != "ok" {
		t.Fatalf("the sidecar's reply: %+v", r)
	}
	if d.Pub != pub {
		t.Errorf("the hub vouches for key %q, the sidecar signed with %q", d.Pub, pub)
	}
	if d.From["sid"] != sid || d.From["fenced"] != "true" {
		t.Errorf("who it came from: %v", d.From)
	}
	if _, err := stamp.Verify(d.Pub, d.Sig, stamp.Fields{From: sid, To: d.To, Kind: "report", Body: "done from the sidecar"}, time.Now(), stamp.NewCache()); err != nil {
		t.Errorf("the receiving machine cannot verify the stamp the sidecar signed: %v", err)
	}

	// nothing else: the record's scope names a peer, the relay's manager is not that peer, so it never resolves
	r2 := bus.Forward(ctx, hostd.Request{Type: "msg.send", Body: json.RawMessage(`{"to":{"node":"` + otherNode + `"},"text":"to the other"}`)})
	if r2.Status == "ok" {
		t.Errorf("a node outside the relay's may_reach was reached: %+v", r2)
	}
	select {
	case g := <-frames:
		t.Errorf("the other session received %q", g.d.Text)
	case <-time.After(1500 * time.Millisecond):
	}
}
