// hubremote.go is hostd's side of messages across machines (M2b): what it does with a delivery that
// arrives from the session bus hub, and how it sends one. It exists apart from handler.go because it
// is the one place a message from OUTSIDE this machine meets a fenced pane: every step that decides
// whether it may is here, in order, and each is tested.
//
// Receiving: only a FENCED sender can be identified, so only a fenced sender is heard. Its stamp (B3) is
// verified HERE, against the public key the hub supplied (trusting that key is trusting the hub's routing
// and key bookkeeping: the end-to-end chain, the hp-signed token that carries the key, comes with U5/M9),
// judged at the time the hub received it; the stamp's `to` must name THIS target; the sender's node comes
// from the hub's presence entry for its sid, which must be unique, fenced and from the machine it claims;
// and scope is re-checked against the target's own record (msg.Router.DeliverRemote). The text typed is
// rendered by hostd from the VERIFIED body, and the hub's own rendering must agree with it. An unfenced
// sender's `from` is a claim nobody authenticates (any owner process holding the hub secret can write any
// id), so an unfenced message is refused: it must not pass the scope check on a claimed identity.
//
// Sending: a fenced session's message to a node of another machine is signed by hostd with a
// per-generation key whose public half ccsbx-root registers on the hub as a relay principal
// (`ccsbx-root relay-mint`; hostd never holds the hub's admin secret). The key lives in memory only and is
// wiped when its generation is no longer live.
package main

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/landovsky/cc-sandbox/internal/hubc"
	"github.com/landovsky/cc-sandbox/internal/hubspoke"
	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/stamp"
	"github.com/landovsky/cc-sandbox/internal/start"
	"github.com/landovsky/cc-sandbox/internal/tmuxsock"
)

// minter registers a relay principal for one generation's public key and returns its token.
type minter func(ctx context.Context, sid string, gen int, pubB64 string, after401 bool) (token string, err error)

// rootMinter runs `sudo -n ccsbx-root relay-mint -- <sid> <gen> <pub>` (the helper validates the sid and
// generation against the daemon's record, holds the hub admin secret, and prints {"token": "..."}).
func rootMinter() minter {
	return func(ctx context.Context, sid string, gen int, pub string, after401 bool) (string, error) {
		if !start.HelperInstalled() {
			return "", fmt.Errorf("ccsbx-root is not installed: no relay credential can be minted")
		}
		args := []string{"-n", start.RootBin, "relay-mint"}
		if after401 {
			args = append(args, "--after-401")
		}
		args = append(args, "--", sid, strconv.Itoa(gen), pub)
		c := exec.CommandContext(ctx, start.SudoBin, args...)
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
	// handledPath: keys of deliveries already typed or queued, kept on disk for handledTTL, so a restart
	// between typing and a successful ack does not type the same message again.
	handledPath string

	lastNodeCheck time.Time
	mintMu        sync.Mutex // serializes relay mints
	mu            sync.Mutex
	keys          map[string]*relayKey // "<sid>/<gen>" -> its key and relay token
	handled       map[string]time.Time
}

const (
	handledTTL = 20 * time.Minute // the stamp's skew (2 min) plus how long the hub holds an unacked frame
	// maxQueueAge: a frame older than this at the receiver (by the hub's receive time) is refused even if its
	// stamp was fine when received: the nonce memory does not outlive it.
	maxQueueAge = 15 * time.Minute
)

func newHubRemote(c *hubc.Client, url, machine string, rt *msg.Router, j appender, logf func(string, ...any), mint minter, stateDir string) *hubRemote {
	h := &hubRemote{hub: c, url: url, machine: machine, rt: rt, j: j, logf: logf, mint: mint, cache: stamp.NewCache(), now: time.Now,
		keys: map[string]*relayKey{}, handled: map[string]time.Time{}}
	if stateDir != "" {
		h.handledPath = filepath.Join(stateDir, "hub", "handled.json")
		if b, err := os.ReadFile(h.handledPath); err == nil {
			_ = json.Unmarshal(b, &h.handled)
		}
	}
	rt.Dropped = func(s msg.Session, node, why string) {
		h.event("msg.dropped", s.Sid, s.Gen, s.Realm, s.Label, map[string]any{"to_node": cap200(node), "reason": cap200(why)})
	}
	return h
}

func cap200(s string) string {
	if len(s) > 200 {
		return s[:200] + "…"
	}
	return s
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

func (h *hubRemote) markHandled(key string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	now := h.now()
	h.handled[key] = now
	for k, t := range h.handled {
		if now.Sub(t) > handledTTL {
			delete(h.handled, k)
		}
	}
	if h.handledPath == "" {
		return
	}
	b, _ := json.Marshal(h.handled)
	if err := os.MkdirAll(filepath.Dir(h.handledPath), 0o700); err == nil {
		tmp := h.handledPath + ".tmp"
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, h.handledPath)
		}
		if err != nil {
			h.logf("hub: the handled-messages file could not be written (%v): a restart may type a message again", err)
		}
	}
}

func (h *hubRemote) wasHandled(key string) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	t, ok := h.handled[key]
	return ok && h.now().Sub(t) <= handledTTL
}

// onDeliver handles one deliver frame from the hub.
func (h *hubRemote) onDeliver(ctx context.Context, f hubc.Frame, d hubc.Deliver) hubspoke.Result {
	hk := "m:" + hex.EncodeToString(sumOf(d.Sig+"\x00"+d.UID+"\x00"+d.Body)) // the stamp is unique per message
	if d.Sig != "" && h.wasHandled(hk) {
		return hubspoke.Result{OK: true} // already typed or queued before a restart: acknowledge, do not type again
	}
	rs, text, verdict := h.vet(ctx, d)
	if verdict != "" {
		h.logf("hub: frame %d for %s refused: %s", f.Seq, cap200(d.UID), cap200(verdict))
		h.event("msg.refused", cap200(d.UID), 0, "", "", map[string]any{"seq": f.Seq, "reason": cap200(verdict), "from_sid": cap200(d.From["sid"])})
		return hubspoke.Result{}
	}
	res := h.rt.DeliverRemote(rs, d.UID, text)
	h.event("hub.recv", d.UID, 0, "", "", map[string]any{"seq": f.Seq, "from_sid": rs.Sid, "from_node": rs.Node, "fenced": rs.Fenced, "status": res.Status, "code": res.Code, "bytes": len(text)})
	if res.Status == "ok" || res.Status == "queued" {
		h.markHandled(hk)
	}
	return hubspoke.Result{OK: res.Status == "ok" || res.Status == "queued", Confirmed: res.Status == "ok"}
}

func sumOf(s string) []byte { h := sha256.Sum256([]byte(s)); return h[:] }

// hubFlat is the hub's own flattening of a relay's text (SessionBus.HubRouter.relay_line): every run of
// control, format or whitespace characters becomes one space, the stamp quotes become a middle dot, and
// the ends are trimmed. The hub's line and ours must agree on it, character for character.
func hubFlat(s string) string {
	var sb strings.Builder
	space := false
	for _, c := range s {
		if unicode.Is(unicode.C, c) || unicode.IsSpace(c) {
			if !space {
				sb.WriteByte(' ')
			}
			space = true
			continue
		}
		space = false
		if c == '‹' || c == '›' {
			c = '·'
		}
		sb.WriteRune(c)
	}
	return strings.TrimSpace(sb.String())
}

var hubLineRe = regexp.MustCompile(`^\[MSG from [^ ]+ · fenced · #([0-9a-f]{12})\] (.*) ‹end #([0-9a-f]{12})›$`)

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
	te := uniqueEntry(entries, d.UID)
	if te == nil || te["machine"] != h.machine || !addressNames(te, d.To) {
		return rs, "", "the address in the stamp does not name this target"
	}
	if d.Trust != "fenced" {
		return h.vetUnfenced(d, entries)
	}
	sid := d.From["sid"]
	if d.Sig == "" || d.Pub == "" || sid == "" {
		return rs, "", "claims fenced without a stamp"
	}
	// Judged at the time the HUB received it (a frame can wait in the mailbox), bounded by how long we
	// remember nonces; the cache ages by our own clock.
	recv := h.now()
	if d.RecvTS > 0 {
		recv = time.Unix(d.RecvTS, 0)
		if h.now().Sub(recv) > maxQueueAge {
			return rs, "", "the message is older than the receiver remembers nonces for"
		}
	}
	if _, err := stamp.VerifyAt(d.Pub, d.Sig, stamp.Fields{From: sid, To: d.To, Kind: d.Kind, Body: d.Body}, recv, h.now(), h.cache); err != nil {
		return rs, "", "stamp refused: " + err.Error()
	}
	// What is typed is rendered HERE from the verified body; the hub's own rendering must agree with it, so a
	// body the hub altered or a text the hub wrote itself is refused, not typed.
	m := hubLineRe.FindStringSubmatch(d.Text)
	if m == nil || m[1] != m[3] || m[2] != hubFlat(d.Body) {
		return rs, "", "the hub's text does not match the verified body"
	}
	se := uniqueEntry(entries, sid)
	if se == nil || se["fenced"] != true {
		return rs, "", "the sender is not exactly one fenced session on the hub"
	}
	machine, _ := se["machine"].(string)
	if machine == h.machine || (d.From["machine"] != "" && d.From["machine"] != machine) {
		return rs, "", "the sender's hub entry is not from the machine it claims"
	}
	node, _ := se["node"].(string)
	if node == "" {
		return rs, "", "the hub has no node for the sender's session"
	}
	return msg.RemoteSender{Node: node, Sid: sid, Machine: machine, Fenced: true}, d.Body, ""
}

// vetUnfenced hears an unfenced sender only when a machine's SPOKE principal vouches for it: the deliver
// must have come in on `spoke:<machine>`, and the hub must have bound from.id to an entry that SAME
// principal tracked, on that same machine, which from.machine names (it may not be left out). The shared
// owner secret ("legacy") is held by every owner session on every machine, so "bound by legacy" proves
// nothing and is refused outright. hostd cross-checks the hub's from_entry against its own read of the
// hub's presence list (the entry exists, is not fenced, is tracked by that principal): the binding is
// verified here, not just believed. The line is typed as "unfenced (hub)" and the normal scope check runs
// on the bound node. Residual (the documented local-ccmsg trust): any owner-uid process on that ONE machine
// holds that machine's spoke credential and can name another session tracked under it; it can never claim
// a fenced identity (that needs a relay signature) or a session of another machine. Absent or negative
// binding means refuse.
func (h *hubRemote) vetUnfenced(d hubc.Deliver, entries []map[string]any) (rs msg.RemoteSender, text, verdict string) {
	if !d.Bound || d.FromEntry == nil {
		return rs, "", "an unfenced sender the hub did not bind to the credential it posted with is not heard"
	}
	machine, _ := d.FromEntry["machine"].(string)
	id, _ := d.FromEntry["id"].(string)
	if machine == "" || d.Via != "spoke:"+machine {
		return rs, "", "an unfenced sender is heard only through its own machine's spoke principal (not the shared secret, not another machine's)"
	}
	if d.From["machine"] == "" || d.From["machine"] != machine || d.From["fenced"] == "true" || d.Sig != "" {
		return rs, "", "the unfenced sender does not name its machine, or claims a fenced identity without a relay signature"
	}
	var live map[string]any
	for _, e := range entries {
		if e["id"] == id && id != "" {
			live = e
		}
	}
	if live == nil || live["fenced"] == true || live["tracked_by"] != d.Via || live["machine"] != machine {
		return rs, "", "the hub's own presence list does not bind that sender to the posting principal"
	}
	node, _ := live["node"].(string)
	uid, _ := live["uid"].(string)
	if node == "" {
		return rs, "", "the sender's hub entry has no node"
	}
	// The node is the scope identity the target trusts, so only ONE holder of it is heard: if any other hub
	// entry holds it (a fenced manager's own, or another machine's owner token squatting on it: the hub
	// refuses that now, but this is checked here, not believed), or a session record of this machine does,
	// which of them is the sender is not for us to guess. Impersonation, and the real channel cut, is what a
	// duplicate would otherwise be.
	for _, x := range entries {
		if x["id"] == id || hubc.CountNode([]map[string]any{x}, node) == 0 {
			continue
		}
		if xm, _ := x["machine"].(string); x["fenced"] == true || xm == machine {
			return rs, "", "the sender's node is also held by a fenced session or by another session of its machine: not heard"
		}
		// held also by an unfenced entry of ANOTHER machine: not ours to weigh here; the target's record binds
		// the node to one machine (remoteInScope), so a squatter elsewhere is never heard and cannot cut this one
	}
	if h.rt != nil && h.rt.Dir != nil {
		if ss, err := h.rt.Dir.Sessions(); err == nil {
			for _, s := range ss {
				if strings.Replace(s.Label, ":#", ":", 1) == strings.Replace(node, ":#", ":", 1) {
					return rs, "", "the sender's node is the label of a session record of this machine: not heard"
				}
			}
		}
	}
	body := d.Body
	if body == "" {
		body = d.Text
	}
	return msg.RemoteSender{Node: node, Sid: uid, Machine: machine}, body, ""
}

// uniqueEntry is the hub entry whose uid is sid, only if there is exactly one: two entries for one sid
// (another machine's spoke publishing it too) leave no way to say which one is the sender.
func uniqueEntry(entries []map[string]any, sid string) map[string]any {
	var found map[string]any
	for _, e := range entries {
		if u, _ := e["uid"].(string); u == sid {
			if found != nil {
				return nil
			}
			found = e
		}
	}
	return found
}

// addressNames: to is one of the names the hub knows the entry by (its id, key, label or node). A label
// can name several sessions on one machine; a stamp is good once, so the others see it as a replay.
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

// SendRemote implements msg.Uplink. It takes no lock of the router's: it may take seconds.
func (h *hubRemote) SendRemote(sender msg.Session, node, kind, text string) (bool, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	h.pruneKeys()
	entries, err := h.hub.Sessions(ctx)
	if err != nil {
		return false, "", err
	}
	// A node belongs to the machine the sender's record binds it to (scope.manager.machine, from the owner's
	// start spec): an entry for it on another machine is a squatter's (any machine's owner token can publish an
	// unclaimed node) and gets nothing, however it is tracked; and it cannot make the real one ambiguous.
	want := sender.Scope.MachineOf(node)
	var held []map[string]any
	for _, x := range entries {
		if hubc.CountNode([]map[string]any{x}, node) > 0 {
			held = append(held, x)
		}
	}
	if want != "" {
		var on []map[string]any
		for _, x := range held {
			if m, _ := x["machine"].(string); m == want {
				on = append(on, x)
			}
		}
		if len(on) == 0 && len(held) > 0 {
			return false, "", msg.ErrWrongMachine
		}
		held = on
	}
	if len(held) > 1 {
		return false, "", msg.ErrAmbiguousNode // two sessions hold the node: which one is the manager is not for us to guess
	}
	if len(held) == 0 {
		return false, "", msg.ErrUnknownNode
	}
	e := held[0]
	m, _ := e["machine"].(string)
	if m == h.machine {
		return false, "", msg.ErrLocalNode
	}
	if want == "" && e["fenced"] != true {
		return false, "", msg.ErrWrongMachine // an unfenced node the scope does not place: not guessed
	}
	to, _ := e["id"].(string)
	toSid, _ := e["uid"].(string)
	if to == "" {
		return false, "", fmt.Errorf("the hub's entry for that node has no id")
	}
	if !kindRe.MatchString(kind) {
		kind = "message"
	}
	stale := ""
	for attempt := 0; attempt < 2; attempt++ {
		priv, token, err := h.relayFor(ctx, sender, stale)
		if err != nil {
			return false, toSid, err
		}
		sig := stamp.Sign(priv, stamp.Fields{From: sender.Sid, To: to, Kind: kind, Body: text}, h.now())
		for i := range priv { // this send's own copy: nothing shared is left behind
			priv[i] = 0
		}
		c := &hubc.Client{URL: h.url, Token: func() (string, error) { return token, nil }}
		res, err := c.Send(ctx, hubc.SendReq{To: to, Text: text, Kind: kind, Sig: sig,
			From: map[string]string{"sid": sender.Sid, "fenced": "true", "label": sender.Label, "machine": h.machine, "id": "ccsbx:" + sender.Sid}})
		switch {
		case err == nil:
			if res.Delivered < 1 {
				return false, toSid, nil
			}
			return true, toSid, nil
		case hubc.IsUnauthorized(err) && attempt == 0:
			stale = token // the relay credential expired or was revoked: ONE send mints a new one
			continue
		case hubc.IsForbidden(err):
			// a refusal of THIS message (scope, stamp, sender): a new credential would not change it
			h.logf("hub: message from %s to %s refused by the hub: %v", sender.Sid, node, err)
			return false, toSid, nil
		default:
			return false, toSid, err
		}
	}
	return false, toSid, fmt.Errorf("the hub refused the relay credential twice")
}

// relayFor returns a COPY of the generation's private key and its relay token, minting them once. stale is
// the token a send just had refused with 401: only the first send that sees it mints a new pair, the
// others (which hold the same stale token) find the new one already stored. The stored key is wiped by
// pruneKeys when its generation ends; a send never uses the stored slice itself, so that cannot zero a key
// that is mid-signature.
func (h *hubRemote) relayFor(ctx context.Context, s msg.Session, stale string) (ed25519.PrivateKey, string, error) {
	k := s.Sid + "/" + strconv.Itoa(s.Gen)
	get := func() (ed25519.PrivateKey, string, bool) {
		h.mu.Lock()
		defer h.mu.Unlock()
		if rk := h.keys[k]; rk != nil && rk.token != stale {
			return append(ed25519.PrivateKey(nil), rk.priv...), rk.token, true
		}
		return nil, "", false
	}
	if priv, tok, ok := get(); ok {
		return priv, tok, nil
	}
	if h.mint == nil {
		return nil, "", fmt.Errorf("no way to mint a relay credential (ccsbx-root relay-mint)")
	}
	h.mintMu.Lock() // one mint at a time: a burst of sends after an expiry asks root once
	defer h.mintMu.Unlock()
	if priv, tok, ok := get(); ok { // another send minted while we waited
		return priv, tok, nil
	}
	pub, priv, err := stamp.NewKey()
	if err != nil {
		return nil, "", err
	}
	tok, err := h.mint(ctx, s.Sid, s.Gen, pub, stale != "")
	if err != nil {
		return nil, "", err
	}
	mine := append(ed25519.PrivateKey(nil), priv...) // copied BEFORE the stored one is visible to pruneKeys
	h.mu.Lock()
	if old := h.keys[k]; old != nil {
		wipe(old)
	}
	h.keys[k] = &relayKey{priv: priv, token: tok}
	h.mu.Unlock()
	return mine, tok, nil
}

func wipe(k *relayKey) {
	for i := range k.priv {
		k.priv[i] = 0
	}
	k.token = ""
}

// warnNodeless logs, at most once an hour, when the hub returns our own fenced entries without their
// node: the hub drops a node unless the machine's spoke principal was minted with `trees` that include
// the node's tree, and without nodes no cross-machine message can be scoped or addressed (M2b). The
// symptom is otherwise silent.
func (h *hubRemote) warnNodeless(ctx context.Context, own []map[string]any) {
	h.mu.Lock()
	if h.now().Sub(h.lastNodeCheck) < time.Hour && !h.lastNodeCheck.IsZero() {
		h.mu.Unlock()
		return
	}
	h.lastNodeCheck = h.now()
	h.mu.Unlock()
	if len(own) == 0 {
		h.mu.Lock()
		h.lastNodeCheck = time.Time{} // nothing to check yet: look again next time
		h.mu.Unlock()
		return
	}
	entries, err := h.hub.Sessions(ctx)
	if err != nil {
		return
	}
	for _, o := range own {
		sid, _ := o["uid"].(string)
		if e := uniqueEntry(entries, sid); e != nil {
			if n, _ := e["node"].(string); n == "" {
				h.logf("hub: WARNING: the hub keeps no node for %s: mint this machine's spoke credential with trees (for example [\"cc-sandbox\"]), or cross-machine messages cannot be scoped or addressed", sid)
				return
			}
		}
	}
}

// pruneKeys drops (and wipes) the keys of generations that are not live any more: a key is for the life
// of one generation, not of the daemon.
func (h *hubRemote) pruneKeys() {
	all, err := h.rt.Dir.Sessions()
	if err != nil {
		return
	}
	live := map[string]bool{}
	for _, s := range all {
		if s.Live {
			live[s.Sid+"/"+strconv.Itoa(s.Gen)] = true
		}
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for k, rk := range h.keys {
		if !live[k] {
			wipe(rk)
			delete(h.keys, k)
		}
	}
}

var _ = errors.New
