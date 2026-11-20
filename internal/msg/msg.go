// Package msg is local message delivery (U3b): a fenced session's msg.send / msg.reply is
// scope-checked, stamped by the daemon and typed into the target's pane.
//
//	msg.send {to:{node|bus_id}, kind, text}
//	  1. scope: the sender may reach its manager (resolved BY NODE to the live session at send
//	     time, so a rolled manager still receives), its Talk: peers, and whoever messaged it in
//	     this generation within reply_window_s (reply-to-sender). Anything else: refused: scope.
//	     Only sessions of the sender's own realm are reachable.
//	  2. rate: at most PairMax messages per PairWindow per (sender, target); more: rate_capped.
//	  3. idle: a message is typed ONLY into a pane that is at its input prompt (the journal's
//	     last hook event for the session is Stop). Otherwise it waits in a bounded queue and is
//	     delivered by Pump when the turn ends. Typing into a pane that shows a menu (a
//	     permission prompt, a question, a plan approval) would answer the menu with the stamp's
//	     keystrokes. The native inbox (C0b, needs root) is the way around this; until then idle
//	     gating is the rule. "Unknown" is never idle.
//	  4. stamp: "[MSG from <bus-id> · <label> · fenced · #<boundary>] <text> ‹end #<boundary>›".
//	     The stamp is built by the daemon from the session record; the boundary is random per
//	     message, so text cannot carry a valid second stamp; and the text itself is one line
//	     of printable characters with [ ] · ‹ › and a trailing backslash removed.
//	  5. deliver: tmux paste-buffer -p (bracketed paste when the app asked for it), then Enter.
//	  6. receipt: kept in memory and appended to <state>/msg/receipts.jsonl (ids and statuses,
//	     never the text).
//
// Owner deliveries (`ccsbx deliver`, the daemon's unix socket) are stamped
// "trust: unfenced (owner device)" and skip the scope rule, but not the idle rule. They are
// never stamped "human".
//
// No cc-tree, no hub, no network: a same-machine message needs only this package and tmux
// (T:msg-no-cockpit).
package msg

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/landovsky/cc-sandbox/internal/start"
	"github.com/landovsky/cc-sandbox/internal/ulid"
)

// MaxText bounds the text of one message, in runes.
const MaxText = 4000

// Scope is the sender's msg_scope from its LaunchSpec (plan §4.1).
type Scope struct {
	Manager struct {
		Node string `json:"node"`
	} `json:"manager"`
	Peers        []string `json:"peers"`
	ReplyWindowS int      `json:"reply_window_s"`
}

// Session is what the router knows of one session.
type Session struct {
	Sid, Label, BusID, Realm string
	Gen                      int
	Scope                    Scope
	Live                     bool
	Started                  time.Time
}

// Directory lists sessions. Production: the daemon's records plus tmux liveness.
type Directory interface{ Sessions() ([]Session, error) }

// Deliverer types a line into a session's pane.
type Deliverer interface{ Deliver(sid, line string) error }

// Idler says whether a session's pane is at its input prompt, and since when. nil: never.
type Idler interface {
	IdleSince(sid string, gen int) (time.Time, bool)
}

// Origin says where the final result of a queued message goes (the sender's inbox).
type Origin struct {
	Sid string `json:"sid"`
	Gen int    `json:"gen"`
	Rid string `json:"rid"`
	ID  string `json:"id"` // the daemon's ULID for the request
}

// Router applies the scope rule and delivers.
type Router struct {
	Dir      Directory
	Out      Deliverer
	Hub      Hub    // optional: for an in-scope node that has no live ccsbx session (an unfenced manager)
	Uplink   Uplink // optional: a node of ANOTHER machine, through the session bus hub (M2b)
	Idle     Idler
	StateDir string
	Now      func() time.Time

	PairMax        int           // messages per PairWindow per (sender, target); default 6
	PairWindow     time.Duration // default 1 minute
	PendingMax     int           // queued messages per target node; default 20
	PendingTTL     time.Duration // a queued message older than this is dropped as expired; default 1h
	MaxReplyWindow time.Duration // receipts older than this are forgotten; default 7 days

	mu        sync.Mutex
	loaded    bool
	rcpts     []Receipt
	pending   []pendingItem
	delivered map[string]time.Time
	pairs     map[string][]time.Time
	// Errs counts state writes that failed (receipts, the pending file): visible, never silent.
	Errs int
}

// Result is the receipt returned to the sender (as the reply's result).
type Result struct {
	Status  string `json:"status"`         // ok | queued | refused | error
	Code    string `json:"code,omitempty"` // scope | no_live_session | bad_request | undeliverable | target_busy | rate_capped | queue_full | expired
	Receipt string `json:"receipt,omitempty"`
	To      string `json:"to,omitempty"` // the sid it was delivered to
}

type sendBody struct {
	Type string `json:"type"`
	To   struct {
		Node  string `json:"node"`
		BusID string `json:"bus_id"`
	} `json:"to"`
	Kind string `json:"kind"`
	Text string `json:"text"`
}

func (r *Router) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func (r *Router) cfg() {
	if r.PairMax == 0 {
		r.PairMax = 6
	}
	if r.PairWindow == 0 {
		r.PairWindow = time.Minute
	}
	if r.PendingMax == 0 {
		r.PendingMax = 20
	}
	if r.PendingTTL == 0 {
		r.PendingTTL = time.Hour
	}
	if r.MaxReplyWindow == 0 {
		r.MaxReplyWindow = 7 * 24 * time.Hour
	}
	if r.delivered == nil {
		r.delivered = map[string]time.Time{}
		r.pairs = map[string][]time.Time{}
	}
}

// Send handles one msg.send or msg.reply from the fenced session `from`.
func (r *Router) Send(from Session, body []byte) Result { return r.SendWith(from, body, Origin{}) }

// SendWith is Send with the place the final result of a queued message goes.
func (r *Router) SendWith(from Session, body []byte, o Origin) Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg()
	r.load()
	var b sendBody
	if json.Unmarshal(body, &b) != nil || strings.TrimSpace(b.Text) == "" || (b.To.Node == "" && b.To.BusID == "") {
		return r.receipt(from.Sid, 0, "", Result{Status: "refused", Code: "bad_request"})
	}
	all, err := r.Dir.Sessions()
	if err != nil {
		return r.receipt(from.Sid, 0, "", Result{Status: "error", Code: "undeliverable"})
	}
	// Re-read the sender from the directory: scope comes from the record, not from the request.
	var sender *Session
	for i := range all {
		if all[i].Sid == from.Sid {
			sender = &all[i]
		}
	}
	if sender == nil {
		return r.receipt(from.Sid, 0, "", Result{Status: "refused", Code: "scope"})
	}
	target := resolve(all, sender.Realm, b.To.Node, b.To.BusID)
	if target == nil {
		// Not live. Whether it would have been in scope decides what we may say about it.
		if !nodeInScope(*sender, b.To.Node) {
			return r.receipt(sender.Sid, sender.Gen, "", Result{Status: "refused", Code: "scope"})
		}
		// An in-scope node with no ccsbx session is an unfenced session on this machine (a
		// manager is always local): the hub path (ccmsg) reaches it, if there is one.
		// Only when NO ccsbx record anywhere (any realm, live or between generations) holds
		// that label: otherwise the label is a fenced node that is briefly not live (a replace)
		// or lives in another realm, and the bus must not become a way around the realm rule.
		if r.Hub != nil && b.To.Node != "" && !labelKnown(all, b.To.Node) {
			if !r.pairOK(sender.Sid, "hub:"+b.To.Node) {
				return r.receipt(sender.Sid, sender.Gen, "", Result{Status: "refused", Code: "rate_capped"})
			}
			if err := r.Hub.Send(b.To.Node, stampFenced(*sender, b.Text)); err != nil {
				return r.receipt(sender.Sid, sender.Gen, "", Result{Status: "error", Code: "undeliverable"})
			}
			return r.receipt(sender.Sid, sender.Gen, "", Result{Status: "ok", To: "hub:" + b.To.Node})
		}
		// Not a session of this machine at all: across machines, through the hub (M2b). The sender is
		// signed for there, and an unreachable target is `dropped`, never a silent success.
		if r.Uplink != nil && b.To.Node != "" && !labelKnown(all, b.To.Node) {
			if !r.pairOK(sender.Sid, "hub:"+b.To.Node) {
				return r.receipt(sender.Sid, sender.Gen, "", Result{Status: "refused", Code: "rate_capped"})
			}
			delivered, err := r.Uplink.SendRemote(*sender, b.To.Node, b.Kind, b.Text)
			switch {
			case err != nil:
				return r.receipt(sender.Sid, sender.Gen, "hub:"+b.To.Node, Result{Status: "error", Code: "undeliverable"})
			case !delivered:
				return r.receipt(sender.Sid, sender.Gen, "hub:"+b.To.Node, Result{Status: "error", Code: "dropped"})
			}
			return r.receipt(sender.Sid, sender.Gen, "hub:"+b.To.Node, Result{Status: "ok", To: "hub:" + b.To.Node})
		}
		return r.receipt(sender.Sid, sender.Gen, "", Result{Status: "error", Code: "no_live_session"})
	}
	if target.Sid == sender.Sid || !r.inScope(*sender, *target) {
		return r.receipt(sender.Sid, sender.Gen, target.Sid, Result{Status: "refused", Code: "scope"})
	}
	if !r.pairOK(sender.Sid, target.Sid) {
		return r.receipt(sender.Sid, sender.Gen, target.Sid, Result{Status: "refused", Code: "rate_capped"})
	}
	if !r.ready(target.Sid, target.Gen) {
		if r.queued(target.Label) >= r.PendingMax {
			return r.receipt(sender.Sid, sender.Gen, target.Sid, Result{Status: "refused", Code: "queue_full"})
		}
		r.pending = append(r.pending, pendingItem{ID: ulid.New(), Queued: r.now().UTC(), FromSid: sender.Sid, ToNode: target.Label,
			Realm: sender.Realm, Text: b.Text, Origin: o})
		r.savePending()
		return r.receipt(sender.Sid, sender.Gen, target.Sid, Result{Status: "queued", Code: "target_busy"})
	}
	return r.deliverFenced(*sender, *target, b.Text)
}

func (r *Router) deliverFenced(sender, target Session, text string) Result {
	line := stampFenced(sender, text)
	r.delivered[target.Sid] = r.now() // busy from here on, before the paste can race a second sender
	if err := r.Out.Deliver(target.Sid, line); err != nil {
		delete(r.delivered, target.Sid)
		return r.receipt(sender.Sid, sender.Gen, target.Sid, Result{Status: "error", Code: "undeliverable"})
	}
	return r.receipt(sender.Sid, sender.Gen, target.Sid, Result{Status: "ok", To: target.Sid})
}

// Uplink sends a message from a fenced session of THIS machine to a node on ANOTHER machine, through
// the session bus hub: the daemon signs it as the sender (B3) and the hub routes it by tree address.
// delivered is false when the hub found no live target or the target did not take it.
type Uplink interface {
	SendRemote(sender Session, node, kind, text string) (delivered bool, err error)
}

// RemoteSender is a sender on another machine as the RECEIVING daemon learned it: Node is the
// sender's tree node (from the hub's presence entry for its sid, not from the message), Sid the
// sender's session id, Fenced whether its stamp verified.
type RemoteSender struct {
	Node, Sid, Machine string
	Fenced             bool
}

// DeliverRemote is a message that arrived from another machine through the hub (M2b). The
// signature (when fenced) was verified by the caller; THIS is the scope re-check, made by the
// receiver against the TARGET's own record: the sender's node must be the target's manager or one of
// its peers, or the target messaged that node lately (reply-to-sender). Then the same pair limit,
// idle gating and queue as a local message, and a stamp that says where it came from and how far it
// can be trusted: "fenced" for a verified stamp, "unfenced (hub)" otherwise. It never uses the owner
// path (DeliverOwner): a remote sender is not the owner's device.
func (r *Router) DeliverRemote(rs RemoteSender, targetSid, text string) Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg()
	r.load()
	from := "remote:" + rs.Node
	all, err := r.Dir.Sessions()
	if err != nil {
		return Result{Status: "error", Code: "undeliverable"}
	}
	var target *Session
	for i := range all {
		if all[i].Sid == targetSid && all[i].Live {
			target = &all[i]
		}
	}
	if target == nil || strings.TrimSpace(text) == "" {
		return r.receipt(from, 0, "", Result{Status: "error", Code: "no_live_session"})
	}
	if rs.Node == "" || !r.remoteInScope(rs, *target) {
		return r.receipt(from, 0, target.Sid, Result{Status: "refused", Code: "scope"})
	}
	if !r.pairOK(from, target.Sid) {
		return r.receipt(from, 0, target.Sid, Result{Status: "refused", Code: "rate_capped"})
	}
	if !r.ready(target.Sid, target.Gen) {
		if r.queued(target.Label) >= r.PendingMax {
			return r.receipt(from, 0, target.Sid, Result{Status: "refused", Code: "queue_full"})
		}
		r.pending = append(r.pending, pendingItem{ID: ulid.New(), Queued: r.now().UTC(), Remote: &rs, ToNode: target.Label, Realm: target.Realm, Text: text})
		r.savePending()
		return r.receipt(from, 0, target.Sid, Result{Status: "queued", Code: "target_busy"})
	}
	return r.deliverRemote(rs, *target, text)
}

// remoteInScope: the sender's node is in the target's own scope, or the target messaged it lately.
func (r *Router) remoteInScope(rs RemoteSender, target Session) bool {
	if nodeInScope(target, rs.Node) {
		return true
	}
	win := time.Duration(target.Scope.ReplyWindowS) * time.Second
	if win <= 0 {
		return false
	}
	if win > r.MaxReplyWindow {
		win = r.MaxReplyWindow
	}
	for _, rc := range r.rcpts {
		if rc.Status == "ok" && rc.From == target.Sid && rc.FromGen == target.Gen && rc.To == "hub:"+rs.Node &&
			r.now().Sub(rc.TS) <= win && !rc.TS.Before(target.Started) {
			return true
		}
	}
	return false
}

func (r *Router) deliverRemote(rs RemoteSender, target Session, text string) Result {
	trust := "unfenced (hub)"
	if rs.Fenced {
		trust = "fenced"
	}
	b := boundary()
	line := fmt.Sprintf("[MSG from %s · %s · %s · #%s] %s ‹end #%s›", ident(rs.Machine+"/"+rs.Sid, 80), ident(rs.Node, 120), trust, b, flat(text, MaxText), b)
	r.delivered[target.Sid] = r.now()
	if err := r.Out.Deliver(target.Sid, line); err != nil {
		delete(r.delivered, target.Sid)
		return r.receipt("remote:"+rs.Node, 0, target.Sid, Result{Status: "error", Code: "undeliverable"})
	}
	return r.receipt("remote:"+rs.Node, 0, target.Sid, Result{Status: "ok", To: target.Sid})
}

// Hub delivers to a node that is not a ccsbx session (an unfenced manager on this machine),
// outside the daemon: today through ccmsg. The line is already stamped.
type Hub interface{ Send(node, line string) error }

// ToManager sends text from a fenced session to its manager node, as the daemon (not the
// fence) decides: C23's questions and hear. The node comes from the sender's record.
func (r *Router) ToManager(from Session, kind, text string, o Origin) Result {
	all, err := r.Dir.Sessions()
	if err != nil {
		return Result{Status: "error", Code: "undeliverable"}
	}
	for _, s := range all {
		if s.Sid == from.Sid {
			node := s.Scope.Manager.Node
			if node == "" {
				return Result{Status: "refused", Code: "scope"}
			}
			b, _ := json.Marshal(map[string]any{"type": "msg.send", "to": map[string]string{"node": node}, "kind": kind, "text": text})
			return r.SendWith(from, b, o)
		}
	}
	return Result{Status: "refused", Code: "scope"}
}

// DeliverRelayed is the ONLY entry for a message that arrives from outside this machine's fences
// (a hub relay, the downlink reader of B10/B11/M2b). It is deliberately not DeliverOwner:
// DeliverOwner skips the scope rule and stamps "owner device", which is right for the owner's own
// CLI and wrong for anything a remote project sent. Here the sender is a daemon-minted sid on this
// machine (the relay's own session record), the scope rule is applied to it, and the stamp says
// fenced, never owner.
func (r *Router) DeliverRelayed(fromSid, target, text string, o Origin) Result {
	all, err := r.Dir.Sessions()
	if err != nil {
		return Result{Status: "error", Code: "undeliverable"}
	}
	var sender *Session
	for i := range all {
		if all[i].Sid == fromSid {
			sender = &all[i]
		}
	}
	if sender == nil {
		return Result{Status: "refused", Code: "scope"}
	}
	b, _ := json.Marshal(map[string]any{"type": "msg.send", "to": map[string]string{"node": target}, "kind": "message", "text": text})
	return r.SendWith(*sender, b, o) // inScope, rate limit, idle gating, fenced stamp
}

// DeliverOwner is a delivery from the owner's device (the daemon's unix socket). The target is
// a sid, node or bus id; the stamp says unfenced, never human. It skips the scope rule on
// purpose and so must only ever be reached from the 0600 owner socket: a relayed message
// (hub downlink, another machine) goes through DeliverRelayed, never through here. A target that is not at its
// prompt is refused (target_busy), or, with queue, held until it is.
func (r *Router) DeliverOwner(who, to, text string, queue bool) Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg()
	r.load()
	all, err := r.Dir.Sessions()
	if err != nil {
		return Result{Status: "error", Code: "undeliverable"}
	}
	var target *Session
	for i := range all {
		if all[i].Live && all[i].Sid == to {
			target = &all[i]
		}
	}
	if target == nil {
		target = resolve(all, "", to, to)
	}
	if target == nil || strings.TrimSpace(text) == "" {
		return r.receipt("owner", 0, "", Result{Status: "error", Code: "no_live_session"})
	}
	if !r.ready(target.Sid, target.Gen) {
		if !queue {
			return r.receipt("owner", 0, target.Sid, Result{Status: "refused", Code: "target_busy"})
		}
		if r.queued(target.Label) >= r.PendingMax {
			return r.receipt("owner", 0, target.Sid, Result{Status: "refused", Code: "queue_full"})
		}
		r.pending = append(r.pending, pendingItem{ID: ulid.New(), Queued: r.now().UTC(), Owner: true, FromLabel: ident(who, 64),
			ToNode: target.Label, Realm: target.Realm, Text: text})
		r.savePending()
		return r.receipt("owner", 0, target.Sid, Result{Status: "queued", Code: "target_busy"})
	}
	return r.deliverOwner(ident(who, 64), *target, text)
}

func (r *Router) deliverOwner(who string, target Session, text string) Result {
	b := boundary()
	line := fmt.Sprintf("[MSG from %s · trust: unfenced (owner device) · #%s] %s ‹end #%s›", who, b, flat(text, MaxText), b)
	r.delivered[target.Sid] = r.now()
	if err := r.Out.Deliver(target.Sid, line); err != nil {
		delete(r.delivered, target.Sid)
		return r.receipt("owner", 0, target.Sid, Result{Status: "error", Code: "undeliverable"})
	}
	return r.receipt("owner", 0, target.Sid, Result{Status: "ok", To: target.Sid})
}

// WithIdle runs act only if the session is at its prompt, and marks the pane busy first, under
// the router's lock; the action itself runs outside it. The idle check and the busy mark are one
// step, so a queued message can't be pumped into the pane between them (C9b's replace would kill a
// turn that message started), and other sessions' messages are not held up while it runs.
// It reports whether act ran.
func (r *Router) WithIdle(sid string, gen int, act func() error) (bool, error) {
	r.mu.Lock()
	r.cfg()
	r.load()
	if !r.ready(sid, gen) {
		r.mu.Unlock()
		return false, nil
	}
	prev, had := r.delivered[sid]
	r.delivered[sid] = r.now() // busy: Pump and senders see this pane as not at its prompt
	r.mu.Unlock()
	// act (a stop and a launch: seconds) runs WITHOUT the router's lock, so every other
	// session's messages keep flowing; this pane stays busy until its next Stop.
	err := act()
	if err != nil { // nothing changed: the pane is as idle as it was
		r.mu.Lock()
		if had {
			r.delivered[sid] = prev
		} else {
			delete(r.delivered, sid)
		}
		r.mu.Unlock()
	}
	return true, err
}

// IdleNow reports whether the session is at its input prompt right now.
func (r *Router) IdleNow(sid string, gen int) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg()
	return r.ready(sid, gen)
}

// ready: the generation's pane is at its prompt (a Stop of THIS generation came after the last
// thing typed into it). The check and the paste happen under the router's lock, one message at
// a time, and the pane is marked busy before the paste; the window that remains (the human
// typing in the pane between the last Stop and the paste) is tmux's, not ours to close.
func (r *Router) ready(sid string, gen int) bool {
	if r.Idle == nil {
		return false
	}
	since, ok := r.Idle.IdleSince(sid, gen)
	if !ok {
		return false
	}
	return since.After(r.delivered[sid])
}

func (r *Router) pairOK(from, to string) bool {
	k, now := from+">"+to, r.now()
	var keep []time.Time
	for _, t := range r.pairs[k] {
		if now.Sub(t) < r.PairWindow {
			keep = append(keep, t)
		}
	}
	if len(keep) >= r.PairMax {
		r.pairs[k] = keep
		return false
	}
	r.pairs[k] = append(keep, now)
	return true
}

// resolve finds the LIVE session for a node (or bus id) in the sender's realm. When a node has
// several live sessions (a roll in progress) the newest wins. realm "" means any (the owner).
func resolve(all []Session, realm, node, bus string) *Session {
	var best *Session
	for i := range all {
		s := &all[i]
		if !s.Live || (realm != "" && s.Realm != realm) {
			continue
		}
		if (node != "" && s.Label == node) || (bus != "" && s.BusID == bus) {
			if best == nil || s.Started.After(best.Started) {
				best = s
			}
		}
	}
	return best
}

// labelKnown: some ccsbx session record, in any realm and live or not, has this label.
func labelKnown(all []Session, node string) bool {
	for _, s := range all {
		if s.Label == node {
			return true
		}
	}
	return false
}

// Resolve returns the live session for a label (the daemon's GET /v1/resolve).
func Resolve(all []Session, label string) *Session { return resolve(all, "", label, "") }

func nodeInScope(from Session, node string) bool {
	if node == "" {
		return false
	}
	if node == from.Scope.Manager.Node {
		return true
	}
	for _, p := range from.Scope.Peers {
		if p == node {
			return true
		}
	}
	return false
}

func (r *Router) inScope(from, to Session) bool {
	if nodeInScope(from, to.Label) {
		return true
	}
	// reply-to-sender: the target messaged us in this generation, recently enough.
	win := time.Duration(from.Scope.ReplyWindowS) * time.Second
	if win <= 0 {
		return false
	}
	if win > r.MaxReplyWindow {
		win = r.MaxReplyWindow
	}
	for _, rc := range r.rcpts {
		if rc.Status == "ok" && rc.To == from.Sid && rc.From == to.Sid && rc.FromGen == to.Gen &&
			r.now().Sub(rc.TS) <= win && !rc.TS.Before(from.Started) {
			return true
		}
	}
	return false
}

// ---- pending (queued until the target is idle) ----

type pendingItem struct {
	ID        string    `json:"id"`
	Queued    time.Time `json:"queued"`
	Owner     bool      `json:"owner,omitempty"`
	FromSid   string    `json:"from_sid,omitempty"`
	FromLabel string    `json:"from_label,omitempty"`
	ToNode    string    `json:"to_node"`
	Realm     string    `json:"realm"`
	Text      string    `json:"text"`
	Origin    Origin    `json:"origin"`
	// Remote is set for a message that arrived from another machine through the hub (M2b).
	Remote *RemoteSender `json:"remote,omitempty"`
}

func (r *Router) queued(node string) int {
	n := 0
	for _, p := range r.pending {
		if p.ToNode == node {
			n++
		}
	}
	return n
}

func (r *Router) pendingPath() string { return filepath.Join(r.StateDir, "msg", "pending.json") }

func (r *Router) savePending() {
	if os.MkdirAll(filepath.Dir(r.pendingPath()), 0o700) != nil {
		r.Errs++
		return
	}
	b, _ := json.Marshal(r.pending)
	tmp := r.pendingPath() + ".tmp"
	if os.WriteFile(tmp, b, 0o600) != nil || os.Rename(tmp, r.pendingPath()) != nil {
		r.Errs++
	}
}

// Pump delivers queued messages whose target is idle now (or expires old ones). done is called
// for every queued fenced message that reached a final state, so the sender's inbox gets it.
func (r *Router) Pump(done func(Origin, Result)) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.cfg()
	r.load()
	if len(r.pending) == 0 {
		return
	}
	all, err := r.Dir.Sessions()
	if err != nil {
		return
	}
	var keep []pendingItem
	changed := false
	for _, p := range r.pending {
		fin := func(res Result) {
			changed = true
			if !p.Owner && done != nil && p.Origin.Rid != "" {
				done(p.Origin, res)
			}
		}
		if r.now().Sub(p.Queued) > r.PendingTTL {
			fin(Result{Status: "refused", Code: "expired"})
			continue
		}
		target := resolve(all, p.Realm, p.ToNode, "")
		if target == nil || !r.ready(target.Sid, target.Gen) {
			keep = append(keep, p)
			continue
		}
		if p.Owner {
			r.deliverOwner(p.FromLabel, *target, p.Text)
			fin(Result{Status: "ok"})
			continue
		}
		if p.Remote != nil {
			if !r.remoteInScope(*p.Remote, *target) {
				r.receipt("remote:"+p.Remote.Node, 0, target.Sid, Result{Status: "refused", Code: "scope"})
				changed = true
				continue
			}
			r.deliverRemote(*p.Remote, *target, p.Text)
			changed = true
			continue
		}
		var sender *Session
		for i := range all {
			if all[i].Sid == p.FromSid {
				sender = &all[i]
			}
		}
		if sender == nil || !r.inScope(*sender, *target) {
			fin(r.receipt(p.FromSid, 0, target.Sid, Result{Status: "refused", Code: "scope"}))
			continue
		}
		fin(r.deliverFenced(*sender, *target, p.Text))
	}
	if changed {
		r.pending = keep
		r.savePending()
	}
}

// ---- receipts ----

// Receipt is one line of receipts.jsonl.
type Receipt struct {
	ID      string    `json:"id"`
	TS      time.Time `json:"ts"`
	From    string    `json:"from"`
	FromGen int       `json:"from_gen"`
	To      string    `json:"to,omitempty"`
	Status  string    `json:"status"`
	Code    string    `json:"code,omitempty"`
}

func (r *Router) path() string { return filepath.Join(r.StateDir, "msg", "receipts.jsonl") }

// load reads the receipts and the pending file once. Only the last MaxReplyWindow of receipts
// is kept in memory (reply-to-sender never looks further back), and the file is compacted to
// that when it has grown past 256 KiB, so a send never scans an unbounded log.
func (r *Router) load() {
	if r.loaded {
		return
	}
	r.loaded = true
	if b, err := os.ReadFile(r.pendingPath()); err == nil {
		_ = json.Unmarshal(b, &r.pending)
	}
	f, err := os.Open(r.path())
	if err != nil {
		return
	}
	defer f.Close()
	cut := r.now().Add(-r.MaxReplyWindow)
	total := 0
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		total++
		var rc Receipt
		if json.Unmarshal(sc.Bytes(), &rc) == nil && rc.TS.After(cut) {
			r.rcpts = append(r.rcpts, rc)
		}
	}
	if st, err := f.Stat(); err == nil && st.Size() > 256<<10 && len(r.rcpts) < total {
		var sb strings.Builder
		for _, rc := range r.rcpts {
			b, _ := json.Marshal(rc)
			sb.Write(b)
			sb.WriteByte('\n')
		}
		tmp := r.path() + ".tmp"
		if os.WriteFile(tmp, []byte(sb.String()), 0o600) != nil || os.Rename(tmp, r.path()) != nil {
			r.Errs++
		}
	}
}

// receipt records the outcome and stamps the id into the result. The log never holds the text.
func (r *Router) receipt(from string, fromGen int, to string, res Result) Result {
	rc := Receipt{ID: ulid.New(), TS: r.now().UTC(), From: from, FromGen: fromGen, To: to, Status: res.Status, Code: res.Code}
	res.Receipt = rc.ID
	r.rcpts = append(r.rcpts, rc)
	if os.MkdirAll(filepath.Dir(r.path()), 0o700) != nil {
		r.Errs++
		return res
	}
	f, err := os.OpenFile(r.path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		r.Errs++
		return res
	}
	b, _ := json.Marshal(rc)
	if _, err := f.Write(append(b, '\n')); err != nil {
		r.Errs++
	}
	f.Close()
	return res
}

// ---- stamps and text ----

func boundary() string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func stampFenced(sender Session, text string) string {
	b := boundary()
	return fmt.Sprintf("[MSG from %s · %s · fenced · #%s] %s ‹end #%s›", ident(sender.BusID, 80), ident(sender.Label, 120), b, flat(text, MaxText), b)
}

// ident limits a field the daemon puts into a stamp to characters that can't end it.
func ident(s string, max int) string {
	var sb strings.Builder
	for _, c := range s {
		if sb.Len() >= max {
			break
		}
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == ':', c == '.', c == '_', c == '-', c == '/', c == '#':
			sb.WriteRune(c)
		default:
			sb.WriteByte('_')
		}
	}
	if sb.Len() == 0 {
		return "unknown"
	}
	return sb.String()
}

// flat makes s one line of printable text of at most max runes: control characters (ESC,
// newline, ...) become spaces, the characters a stamp is made of ([ ] · ‹ ›) are replaced, and
// a trailing backslash (Claude Code's "continue on the next line") is dropped, so the text can
// neither start a new line, forge a stamp, nor merge with the next message.
func flat(s string, max int) string {
	repl := map[rune]rune{'[': '(', ']': ')', '·': '.', '‹': '<', '›': '>'}
	var sb strings.Builder
	n := 0
	lastSpace := false
	for _, c := range s {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) || unicode.IsSpace(c) {
			c = ' '
		}
		if x, ok := repl[c]; ok {
			c = x
		}
		if c == ' ' {
			if lastSpace {
				continue
			}
			lastSpace = true
		} else {
			lastSpace = false
		}
		if n++; n > max {
			sb.WriteString("…")
			break
		}
		sb.WriteRune(c)
	}
	return strings.TrimRight(strings.TrimSpace(sb.String()), "\\ ")
}

// DaemonDir is the production Directory: the daemon's session records plus a liveness probe.
type DaemonDir struct {
	D     *start.Daemon
	Alive func(sid string) bool
}

func (d DaemonDir) Sessions() ([]Session, error) {
	recs, err := d.D.Records()
	if err != nil {
		return nil, err
	}
	var out []Session
	for _, rec := range recs {
		realm := rec.Spec.Realm
		if realm == "" {
			realm = "production"
		}
		s := Session{Sid: rec.Sid, Label: rec.Spec.Label, Gen: rec.Gen, BusID: "ccsbx:" + rec.Sid, Realm: realm, Started: rec.Started}
		_ = json.Unmarshal(rec.Spec.MsgScope, &s.Scope)
		s.Live = rec.Ended == nil && d.Alive(rec.Sid)
		out = append(out, s)
	}
	return out, nil
}
