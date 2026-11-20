// Package hubsend is how a fenced session's daemon (or its pod's sidecar) sends a message to a session on another
// machine through the hub: pick the ONE hub entry the sender's own record places the target on, sign the message
// with the generation's Ed25519 key (the stamp, domain ccsbx-stamp-v1) and post it with the generation's relay
// credential. It exists so the docker host's hostd and the pod's sidecar send the same bytes by the same rules, and
// so neither holds a second copy of the stamp or the target-resolution rules that could drift from the other.
//
// It holds no key and no credential between calls and mints nothing: the caller passes the private key and the
// relay token for this one send, and decides what a 401 means (hostd re-mints through ccsbx-root; a pod's
// sidecar cannot, and answers relay_auth to the fence). A 403 is a refusal of THIS message and is never retried.
package hubsend

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"regexp"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hubc"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/stamp"
)

var kindRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// Kind is the stamp's kind for a message: the caller's if it is a plain word, else "message".
func Kind(k string) string {
	if kindRe.MatchString(k) {
		return k
	}
	return "message"
}

// Target is the hub entry a message goes to.
type Target struct {
	ID  string // the hub's id for the entry (what `to` is signed over)
	Sid string // the session id, when the entry has one (for the sender's reply window)
}

// Resolve picks the entry for a tree node out of the hub's presence list, by the rules a sender is held to:
//
//   - A node belongs to the machine the SENDER'S record places it on (wantMachine = Scope.MachineOf(node), from the
//     owner's start spec, never from the fence): an entry on another machine is a squatter's and is neither sent to
//     nor allowed to make the real one ambiguous (msg.ErrWrongMachine when only such entries exist).
//   - Two entries on the node (on the right machine) are not guessed between (msg.ErrAmbiguousNode).
//   - No entry at all: msg.ErrUnknownNode. An entry on THIS machine (myMachine): msg.ErrLocalNode, which the router
//     hands to the local bus.
//   - An UNFENCED node the record does not place on a machine is not sent to (msg.ErrWrongMachine); a fenced one
//     is (its signature identifies it).
func Resolve(entries []map[string]any, node, wantMachine, myMachine string) (Target, error) {
	var held []map[string]any
	for _, x := range entries {
		if hubc.CountNode([]map[string]any{x}, node) > 0 {
			held = append(held, x)
		}
	}
	if wantMachine != "" {
		var on []map[string]any
		for _, x := range held {
			if m, _ := x["machine"].(string); m == wantMachine {
				on = append(on, x)
			}
		}
		if len(on) == 0 && len(held) > 0 {
			return Target{}, msg.ErrWrongMachine
		}
		held = on
	}
	if len(held) > 1 {
		return Target{}, msg.ErrAmbiguousNode
	}
	if len(held) == 0 {
		return Target{}, msg.ErrUnknownNode
	}
	e := held[0]
	m, _ := e["machine"].(string)
	if m == myMachine {
		return Target{}, msg.ErrLocalNode
	}
	if wantMachine == "" && e["fenced"] != true {
		return Target{}, msg.ErrWrongMachine
	}
	id, _ := e["id"].(string)
	if id == "" {
		return Target{}, fmt.Errorf("the hub's entry for that node has no id")
	}
	sid, _ := e["uid"].(string)
	return Target{ID: id, Sid: sid}, nil
}

// From says who is sending, as the hub and the receiver learn it. Sid and Machine are the daemon's own facts.
type From struct {
	Sid, Label, Machine string
}

// Send signs (kind, text) for the target with priv and posts it with token. It returns whether the hub delivered it
// to at least one session. The error carries the hub's status (hubc.IsUnauthorized: the credential expired or was
// revoked; hubc.IsForbidden: the hub refused this message). priv is NOT zeroed here: it is the caller's copy.
func Send(ctx context.Context, hubURL, token string, priv ed25519.PrivateKey, from From, to Target, kind, text string, now time.Time) (bool, error) {
	kind = Kind(kind)
	sig := stamp.Sign(priv, stamp.Fields{From: from.Sid, To: to.ID, Kind: kind, Body: text}, now)
	c := &hubc.Client{URL: hubURL, Token: func() (string, error) { return token, nil }}
	res, err := c.Send(ctx, hubc.SendReq{To: to.ID, Text: text, Kind: kind, Sig: sig,
		From: map[string]string{"sid": from.Sid, "fenced": "true", "label": from.Label, "machine": from.Machine, "id": "ccsbx:" + from.Sid}})
	if err != nil {
		return false, err
	}
	return res.Delivered >= 1, nil
}
