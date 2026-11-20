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
	ID  string // the address `to` is posted and signed over: the entry's full uid (its short id only when it has no uid)
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
//   - A node the record places on NO machine is not sent to, fenced or not (msg.ErrWrongMachine): a fenced session's
//     node is only the label its starter gave it, so a decoy from another machine would otherwise receive the report.
//   - The address is the entry's full uid (its short id is 24 bits of a hash, so another machine's owner can grind a
//     colliding one): if ANY other entry's uid, key or id equals that address the node is not sent to
//     (msg.ErrAmbiguousNode). A shared short id alone does not block: the hub never fans a full uid out.
func Resolve(entries []map[string]any, node, wantMachine, myMachine string) (Target, error) {
	var held []int // indexes into entries
	for i, x := range entries {
		if hubc.CountNode([]map[string]any{x}, node) > 0 {
			held = append(held, i)
		}
	}
	if wantMachine != "" {
		var on []int
		for _, i := range held {
			if m, _ := entries[i]["machine"].(string); m == wantMachine {
				on = append(on, i)
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
	at := held[0]
	e := entries[at]
	if m, _ := e["machine"].(string); m == myMachine {
		return Target{}, msg.ErrLocalNode
	}
	if wantMachine == "" {
		return Target{}, msg.ErrWrongMachine
	}
	id, _ := e["id"].(string)
	sid, _ := e["uid"].(string)
	addr := sid
	if addr == "" {
		addr = id
	}
	if addr == "" {
		return Target{}, fmt.Errorf("the hub's entry for that node has no uid or id")
	}
	for i, o := range entries {
		if i == at {
			continue
		}
		// The hub resolves the address we post (the full uid) against every entry's uid, key and id; refuse when
		// another entry would also answer to it. A shared SHORT id alone is no collision for a full-uid address
		// (the hub never fans a uid out), so an entry grinding the manager's short id cannot block the send.
		for _, k := range []string{"uid", "id", "key"} {
			if v, _ := o[k].(string); v != "" && v == addr {
				return Target{}, msg.ErrAmbiguousNode
			}
		}
	}
	return Target{ID: addr, Sid: sid}, nil
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
