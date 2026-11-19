package main

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/start"
	"github.com/landovsky/cc-sandbox/internal/verbq"
)

// mux sends each request type to its piece. A type nobody has built yet is answered
// error/not_built, never silently dropped. Later pieces register their prefix here.
type mux struct {
	byPrefix []prefixHandler
}

type prefixHandler struct {
	prefix string
	h      hostd.Handler
}

func (m *mux) on(prefix string, h hostd.Handler) {
	m.byPrefix = append(m.byPrefix, prefixHandler{prefix, h})
}

func (m *mux) handle(r hostd.Request) hostd.Reply {
	for _, p := range m.byPrefix {
		if strings.HasPrefix(r.Type, p.prefix) {
			return p.h(r)
		}
	}
	return hostd.Reply{Status: "error", Result: map[string]string{"code": "not_built"}}
}

// msgHandler is U3b: msg.send and msg.reply go through the scope rule and into the target pane.
func msgHandler(rt *msg.Router) hostd.Handler {
	return func(r hostd.Request) hostd.Reply {
		res := rt.Send(msg.Session{Sid: r.Sid}, r.Body) // scope is re-read from the record by sid
		return hostd.Reply{Status: res.Status, Result: res}
	}
}

// verbHandler is M4: ccf.* requests are queued for the realm's cockpit and answered at once.
func verbHandler(q *verbq.Queue, d *start.Daemon) hostd.Handler {
	return func(r hostd.Request) hostd.Reply {
		realm := "production"
		if rec, err := d.Load(r.Sid); err == nil && rec.Spec.Realm != "" {
			realm = rec.Spec.Realm
		}
		it, err := q.Enqueue(realm, verbq.Item{ID: r.ID, Sid: r.Sid, Gen: r.Gen, Rid: r.Rid, Node: r.Label, Type: r.Type, Body: r.Body})
		switch {
		case errors.Is(err, verbq.ErrQueueFull):
			return hostd.Reply{Status: "refused", Result: map[string]string{"code": "queue_full"}}
		case err != nil:
			return hostd.Reply{Status: "error", Result: map[string]string{"code": "io_error"}}
		}
		return hostd.Reply{Status: "queued", Result: map[string]any{"seq": it.Seq}, Seq: int(it.Seq)}
	}
}

// replyWriter puts a cockpit's final result into the session's inbox, if its generation is
// still the live one. false: the cockpit puts the result into the node's Outcome instead.
func replyWriter(stateDir string, d *start.Daemon) verbq.ReplyFunc {
	return func(it verbq.Item, res verbq.Result) bool {
		rec, err := d.Load(it.Sid)
		if err != nil || rec.Gen != it.Gen || rec.Ended != nil {
			return false
		}
		gdir := filepath.Join(stateDir, "s", it.Sid, fmt.Sprintf("g%d", it.Gen))
		var result any
		if len(res.Result) > 0 {
			result = res.Result
		} else if res.Code != "" {
			result = map[string]string{"code": res.Code}
		}
		return hostd.WriteReply(gdir, hostd.Reply{Rid: it.Rid, ID: it.ID, Status: res.Status, Result: result, Seq: int(it.Seq)}) == nil
	}
}
