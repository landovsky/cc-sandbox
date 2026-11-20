// hubremote.go is hostd's side of messages across machines (M2b): what it does with a delivery that
// arrives from the session bus hub, and how it sends one. It exists apart from handler.go because it
// is the one place a message from OUTSIDE this machine meets a fenced pane: every step that decides
// whether it may is here, in order, and each is tested.
//
// Receiving: a fenced sender's stamp (B3) is verified HERE, against the public key the hub supplied
// (trusting that key is trusting the hub's routing: the end-to-end chain, the hp-signed token that
// carries the key, comes with U5/M9), the stamp's `to` must name THIS target, the sender's node is
// taken from the hub's presence entry for its sid (published by that machine's own daemon, never from
// the message), and scope is re-checked against the target's own record (msg.Router.DeliverRemote).
// An unfenced sender is delivered as "unfenced (hub)" under the same scope rule.
//
// Sending: a fenced session's message to a node of another machine is signed by hostd with a
// per-generation key whose public half ccsbx-root registers on the hub as a relay principal
// (`ccsbx-root relay-mint`; hostd never holds the hub's admin secret). The key lives in memory only.
package main

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hubc"
	"github.com/landovsky/cc-sandbox/internal/hubspoke"
	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/stamp"
	"github.com/landovsky/cc-sandbox/internal/start"
	"github.com/landovsky/cc-sandbox/internal/tmuxsock"
)

// minter registers a relay principal for one generation's public key and returns its token.
type minter func(ctx context.Context, sid string, gen int, pubB64 string) (token string, err error)

// rootMinter runs `sudo -n ccsbx-root relay-mint -- <sid> <gen> <pub>` (the helper validates the sid and
// generation against the daemon's record, holds the hub admin secret, and prints {"token": "..."}).
func rootMinter() minter {
	return func(ctx context.Context, sid string, gen int, pub string) (string, error) {
		if !start.HelperInstalled() {
			return "", fmt.Errorf("ccsbx-root is not installed: no relay credential can be minted")
		}
		c := exec.CommandContext(ctx, start.SudoBin, "-n", start.RootBin, "relay-mint", "--", sid, strconv.Itoa(gen), pub)
		out, err := tmuxsock.RunFor(c, 15*time.Second)
		if err != nil {
			return "", fmt.Errorf("relay-mint: %v", err)
		}
		var r struct {
			Token string `json:"token"`
		}
		if json.Unmarshal(out, &r) != nil || !strings.HasPrefix(r.Token, "sbp1.") {
			return "", fmt.Errorf("relay-mint answered something that is not a token")
		}
		return r.Token, nil
	}
}

type relayKey struct {
	priv  ed25519.PrivateKey
	token string
}

// hubRemote is hostd's messaging through the hub.
type hubRemote struct {
	hub     *hubc.Client // the machine's SPOKE credential: presence reads, downlink, acks
	url     string
	machine string
	rt      *msg.Router
	j       appender
	logf    func(string, ...any)
	mint    minter
	cache   *stamp.Cache
	now     func() time.Time

	mu   sync.Mutex
	keys map[string]*relayKey // "<sid>/<gen>" -> its key and relay token
}

func newHubRemote(c *hubc.Client, url, machine string, rt *msg.Router, j appender, logf func(string, ...any), mint minter) *hubRemote {
	return &hubRemote{hub: c, url: url, machine: machine, rt: rt, j: j, logf: logf, mint: mint, cache: stamp.NewCache(), now: time.Now, keys: map[string]*relayKey{}}
}

func (h *hubRemote) event(typ, sid string, gen int, realm, node string, data map[string]any) {
	b, _ := json.Marshal(data)
	if realm == "" {
		realm = "production"
	}
	if _, err := h.j.Append(journal.Event{Src: "host", Type: typ, Sid: sid, Gen: gen, Node: node, Realm: realm, Data: b}); err != nil {
		h.logf("hub: %s could not be journaled: %v", typ, err)
	}
}

// ---- receiving ----

var kindRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// onDeliver handles one deliver frame from the hub.
func (h *hubRemote) onDeliver(ctx context.Context, f hubc.Frame, d hubc.Deliver) hubspoke.Result {
	rs, text, verdict := h.vet(ctx, d)
	if verdict != "" {
		h.logf("hub: frame %d for %s refused: %s", f.Seq, d.UID, verdict)
		h.event("msg.refused", d.UID, 0, "", "", map[string]any{"seq": f.Seq, "reason": verdict, "from_sid": d.From["sid"]})
		return hubspoke.Result{}
	}
	res := h.rt.DeliverRemote(rs, d.UID, text)
	h.event("hub.recv", d.UID, 0, "", "", map[string]any{"seq": f.Seq, "from_sid": rs.Sid, "from_node": rs.Node, "fenced": rs.Fenced, "status": res.Status, "code": res.Code, "bytes": len(text)})
	return hubspoke.Result{OK: res.Status == "ok" || res.Status == "queued", Confirmed: res.Status == "ok"}
}

// vet decides who the sender really is. verdict "" means: deliver text as rs.
func (h *hubRemote) vet(ctx context.Context, d hubc.Deliver) (rs msg.RemoteSender, text, verdict string) {
	if d.UID == "" {
		return rs, "", "no target"
	}
	entries, err := h.hub.Sessions(ctx)
	if err != nil {
		return rs, "", "the hub's presence list could not be read: " + err.Error()
	}
	// The hub must have sent this to a session that really is ours (its entry names the address used).
	te := hubc.FindSid(entries, d.UID)
	if te == nil || te["machine"] != h.machine || !addressNames(te, d.To) {
		return rs, "", "the address in the stamp does not name this target"
	}
	body := d.Body
	if body == "" {
		body = d.Text
	}
	if d.Trust == "fenced" {
		sid := d.From["sid"]
		if d.Sig == "" || d.Pub == "" || sid == "" {
			return rs, "", "claims fenced without a stamp"
		}
		if _, err := stamp.Verify(d.Pub, d.Sig, stamp.Fields{From: sid, To: d.To, Kind: d.Kind, Body: d.Body}, h.now(), h.cache); err != nil {
			return rs, "", "stamp refused: " + err.Error()
		}
		se := hubc.FindSid(entries, sid)
		node, _ := se["node"].(string)
		if se == nil || node == "" {
			return rs, "", "the hub has no node for the sender's session"
		}
		machine, _ := se["machine"].(string)
		return msg.RemoteSender{Node: node, Sid: sid, Machine: machine, Fenced: true}, d.Body, ""
	}
	// Unfenced: the sender is whoever the hub's presence says holds that bus id; its word about itself
	// is only a claim, so it is delivered as such ("unfenced (hub)") and held to the same scope rule.
	id := d.From["id"]
	var se map[string]any
	for _, e := range entries {
		if e["id"] == id && id != "" {
			se = e
		}
	}
	node, _ := se["node"].(string)
	if se == nil || node == "" {
		return rs, "", "the sender is not a session with a node on the hub"
	}
	machine, _ := se["machine"].(string)
	sid, _ := se["uid"].(string)
	return msg.RemoteSender{Node: node, Sid: sid, Machine: machine}, body, ""
}

// addressNames: to is one of the names the hub knows the entry by (its id, key, label or node).
func addressNames(e map[string]any, to string) bool {
	if to == "" {
		return false
	}
	for _, k := range []string{"id", "key", "label", "node"} {
		if v, _ := e[k].(string); v != "" && (v == to || (k == "node" && strings.Replace(to, ":#", ":", 1) == v)) {
			return true
		}
	}
	return false
}

// ---- sending ----

// SendRemote implements msg.Uplink.
func (h *hubRemote) SendRemote(sender msg.Session, node, kind, text string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	entries, err := h.hub.Sessions(ctx)
	if err != nil {
		return false, err
	}
	e := hubc.FindNode(entries, node)
	if e == nil {
		h.dropped(sender, node, "no session on the hub holds that node")
		return false, nil
	}
	to, _ := e["id"].(string)
	if !kindRe.MatchString(kind) {
		kind = "message"
	}
	for attempt := 0; attempt < 2; attempt++ {
		k, err := h.relayFor(ctx, sender, attempt > 0)
		if err != nil {
			return false, err
		}
		sig := stamp.Sign(k.priv, stamp.Fields{From: sender.Sid, To: to, Kind: kind, Body: text}, h.now())
		c := &hubc.Client{URL: h.url, Token: func() (string, error) { return k.token, nil }}
		res, err := c.Send(ctx, hubc.SendReq{To: to, Text: text, Kind: kind, Sig: sig,
			From: map[string]string{"sid": sender.Sid, "fenced": "true", "label": sender.Label, "machine": h.machine, "id": "ccsbx:" + sender.Sid}})
		if err != nil {
			if hubc.IsAuth(err) && attempt == 0 {
				continue // an expired or revoked relay: mint a new one once
			}
			return false, err
		}
		if res.Delivered < 1 {
			h.dropped(sender, node, fmt.Sprintf("the hub delivered to %d of %d matches", res.Delivered, res.Matched))
			return false, nil
		}
		return true, nil
	}
	return false, fmt.Errorf("the hub refused the relay credential twice")
}

func (h *hubRemote) dropped(sender msg.Session, node, why string) {
	h.logf("hub: message from %s to %s dropped: %s", sender.Sid, node, why)
	h.event("msg.dropped", sender.Sid, sender.Gen, sender.Realm, sender.Label, map[string]any{"to_node": node, "reason": why})
}

// relayFor returns the generation's key and relay token, minting them once (or again when fresh).
func (h *hubRemote) relayFor(ctx context.Context, s msg.Session, fresh bool) (*relayKey, error) {
	k := s.Sid + "/" + strconv.Itoa(s.Gen)
	h.mu.Lock()
	defer h.mu.Unlock()
	if rk := h.keys[k]; rk != nil && !fresh {
		return rk, nil
	}
	if h.mint == nil {
		return nil, fmt.Errorf("no way to mint a relay credential (ccsbx-root relay-mint)")
	}
	pub, priv, err := stamp.NewKey()
	if err != nil {
		return nil, err
	}
	tok, err := h.mint(ctx, s.Sid, s.Gen, pub)
	if err != nil {
		return nil, err
	}
	rk := &relayKey{priv: priv, token: tok}
	for old := range h.keys { // an older generation of this session is over: its key is not kept
		if strings.HasPrefix(old, s.Sid+"/") && old != k {
			delete(h.keys, old)
		}
	}
	h.keys[k] = rk
	return rk, nil
}
