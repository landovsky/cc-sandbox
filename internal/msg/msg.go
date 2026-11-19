// Package msg is local message delivery (U3b): a fenced session's msg.send / msg.reply is
// scope-checked, stamped by the daemon and typed into the target's pane.
//
//	msg.send {to:{node|bus_id}, kind, text}
//	  1. scope: the sender may reach its manager (resolved BY NODE to the live session at send
//	     time, so a rolled manager still receives), its Talk: peers, and whoever messaged it in
//	     this generation within reply_window_s (reply-to-sender). Anything else: refused: scope.
//	  2. stamp: "[MSG from <bus-id> · <label> · fenced] <text>" — built by the daemon from the
//	     session record. The text is one line of printable characters, so it can neither forge a
//	     second stamp nor type control sequences into the target.
//	  3. deliver: native inbox where C0b passes (not built: C0b needs root), else tmux send-keys
//	     (the only way on the Mac).
//	  4. receipt: appended to <state>/msg/receipts.jsonl (ids and statuses, never the text) and
//	     returned to the sender.
//
// Owner-device deliveries (ccsbx deliver, the local socket) are stamped
// "trust: unfenced (owner device)" and skip the scope rule: the owner may write to anyone. They
// are never stamped "human".
//
// No cc-tree, no hub, no network: a same-machine message needs only this package and tmux
// (T:msg-no-cockpit).
package msg

import (
	"bufio"
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
	Sid, Label, BusID string
	Gen               int
	Scope             Scope
	Live              bool
	Started           time.Time
}

// Directory lists sessions. Production: the daemon's records plus tmux liveness.
type Directory interface{ Sessions() ([]Session, error) }

// Deliverer types a line into a session's pane.
type Deliverer interface{ Deliver(sid, line string) error }

// Router applies the scope rule and delivers.
type Router struct {
	Dir      Directory
	Out      Deliverer
	StateDir string
	Now      func() time.Time
	mu       sync.Mutex
}

// Result is the receipt returned to the sender (as the reply's result).
type Result struct {
	Status  string `json:"status"`         // ok | refused | error
	Code    string `json:"code,omitempty"` // scope | no_live_session | bad_request | undeliverable
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

// Send handles one msg.send or msg.reply from the fenced session `from`.
func (r *Router) Send(from Session, body []byte) Result {
	var b sendBody
	if json.Unmarshal(body, &b) != nil || strings.TrimSpace(b.Text) == "" || (b.To.Node == "" && b.To.BusID == "") {
		return r.receipt(from.Sid, "", Result{Status: "refused", Code: "bad_request"})
	}
	all, err := r.Dir.Sessions()
	if err != nil {
		return r.receipt(from.Sid, "", Result{Status: "error", Code: "undeliverable"})
	}
	// Re-read the sender from the directory: scope comes from the record, not from the request.
	var sender *Session
	for i := range all {
		if all[i].Sid == from.Sid {
			sender = &all[i]
		}
	}
	if sender == nil {
		return r.receipt(from.Sid, "", Result{Status: "refused", Code: "scope"})
	}
	target := resolve(all, b.To.Node, b.To.BusID)
	if target == nil {
		// Not live. Whether it would have been in scope decides what we may say about it.
		if !r.nodeInScope(*sender, b.To.Node) {
			return r.receipt(sender.Sid, "", Result{Status: "refused", Code: "scope"})
		}
		return r.receipt(sender.Sid, "", Result{Status: "error", Code: "no_live_session"})
	}
	if target.Sid == sender.Sid || !r.inScope(*sender, *target) {
		return r.receipt(sender.Sid, target.Sid, Result{Status: "refused", Code: "scope"})
	}
	line := fmt.Sprintf("[MSG from %s · %s · fenced] %s", clean(sender.BusID, 80), clean(sender.Label, 120), clean(b.Text, MaxText))
	if err := r.Out.Deliver(target.Sid, line); err != nil {
		return r.receipt(sender.Sid, target.Sid, Result{Status: "error", Code: "undeliverable"})
	}
	return r.receipt(sender.Sid, target.Sid, Result{Status: "ok", To: target.Sid})
}

// DeliverOwner is a delivery from the owner's device (the CLI or the local socket). The
// target is a sid, node or bus id; the stamp says unfenced, never human.
func (r *Router) DeliverOwner(who, to, text string) Result {
	all, err := r.Dir.Sessions()
	if err != nil {
		return Result{Status: "error", Code: "undeliverable"}
	}
	target := resolve(all, to, to)
	if target == nil {
		for i := range all {
			if all[i].Sid == to && all[i].Live {
				target = &all[i]
			}
		}
	}
	if target == nil || strings.TrimSpace(text) == "" {
		return r.receipt("owner", "", Result{Status: "error", Code: "no_live_session"})
	}
	line := fmt.Sprintf("[MSG from %s · trust: unfenced (owner device)] %s", clean(who, 80), clean(text, MaxText))
	if err := r.Out.Deliver(target.Sid, line); err != nil {
		return r.receipt("owner", target.Sid, Result{Status: "error", Code: "undeliverable"})
	}
	return r.receipt("owner", target.Sid, Result{Status: "ok", To: target.Sid})
}

// resolve finds the LIVE session for a node (or bus id). When a node has several live sessions
// (a roll in progress) the newest wins.
func resolve(all []Session, node, bus string) *Session {
	var best *Session
	for i := range all {
		s := &all[i]
		if !s.Live {
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

func (r *Router) nodeInScope(from Session, node string) bool {
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
	if r.nodeInScope(from, to.Label) {
		return true
	}
	// reply-to-sender: the target messaged us in this generation, recently enough.
	win := time.Duration(from.Scope.ReplyWindowS) * time.Second
	if win <= 0 {
		return false
	}
	for _, rc := range r.receipts() {
		if rc.Status == "ok" && rc.To == from.Sid && rc.From == to.Sid && rc.FromGen == to.Gen &&
			r.now().Sub(rc.TS) <= win && !rc.TS.Before(from.Started) {
			return true
		}
	}
	return false
}

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

func (r *Router) receipts() []Receipt {
	f, err := os.Open(r.path())
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []Receipt
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var rc Receipt
		if json.Unmarshal(sc.Bytes(), &rc) == nil {
			out = append(out, rc)
		}
	}
	return out
}

// receipt records the outcome and stamps the id into the result. The log never holds the text.
func (r *Router) receipt(from, to string, res Result) Result {
	r.mu.Lock()
	defer r.mu.Unlock()
	rc := Receipt{ID: ulid.New(), TS: r.now().UTC(), From: from, To: to, Status: res.Status, Code: res.Code}
	if all, err := r.Dir.Sessions(); err == nil {
		for _, s := range all {
			if s.Sid == from {
				rc.FromGen = s.Gen
			}
		}
	}
	res.Receipt = rc.ID
	if os.MkdirAll(filepath.Dir(r.path()), 0o700) == nil {
		if f, err := os.OpenFile(r.path(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600); err == nil {
			b, _ := json.Marshal(rc)
			f.Write(append(b, '\n'))
			f.Close()
		}
	}
	return res
}

// clean makes s one line of printable text of at most max runes: control characters (ESC,
// newline, ...) become spaces, so the text can't start a new line or type an escape sequence.
func clean(s string, max int) string {
	var sb strings.Builder
	n := 0
	lastSpace := false
	for _, c := range s {
		if unicode.IsControl(c) || unicode.Is(unicode.Cf, c) || c == ' ' || c == ' ' {
			c = ' '
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
	return strings.TrimSpace(sb.String())
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
		s := Session{Sid: rec.Sid, Label: rec.Spec.Label, Gen: rec.Gen, BusID: "ccsbx:" + rec.Sid, Started: rec.Started}
		_ = json.Unmarshal(rec.Spec.MsgScope, &s.Scope)
		s.Live = rec.Ended == nil && d.Alive(rec.Sid)
		out = append(out, s)
	}
	return out, nil
}
