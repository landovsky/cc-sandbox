// The request handlers of ccsbx-hostd: what a fence's request becomes. It exists apart from main.go so
// that main only wires flags, and these (msg, verb queue + journal, question/hear, replies) can be
// tested without a process: every fence request is untrusted, the daemon's records are the authority.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/hubc"
	"github.com/landovsky/cc-sandbox/internal/hubspoke"
	"github.com/landovsky/cc-sandbox/internal/journal"
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
		// scope is re-read from the record by sid; a message to a busy pane waits, and the
		// final result is written into the sender's inbox by the pump.
		res := rt.SendWith(msg.Session{Sid: r.Sid}, r.Body, msg.Origin{Sid: r.Sid, Gen: r.Gen, Rid: r.Rid, ID: r.ID})
		return hostd.Reply{Status: res.Status, Result: res}
	}
}

type appender interface {
	Append(journal.Event) (journal.Record, error)
}

// verbHandler is M4: ccf.* requests are queued for the realm's cockpit and answered at once.
func verbHandler(q *verbq.Queue, d *start.Daemon, j appender, rt *msg.Router) hostd.Handler {
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
		// The cockpit's follower reads the journal: the request goes there too, under the id the
		// daemon gave it, so the result it reports names the same id.
		var env struct {
			Data json.RawMessage `json:"data"`
		}
		_ = json.Unmarshal(r.Body, &env)
		data := json.RawMessage(`{}`)
		if len(env.Data) > 0 && env.Data[0] == '{' {
			data = env.Data
		}
		if j != nil {
			if it.Seq == 0 {
				// Already applied and answered (a re-ingest after an epoch reset): don't journal
				// it again, and don't turn its final answer back into "queued".
				return hostd.Reply{Status: "queued", Result: map[string]any{"seq": 0}}
			}
			if _, err := j.Append(journal.Event{ID: r.ID, Sid: r.Sid, Gen: r.Gen, Node: r.Label, Realm: realm, Src: "fence", Type: r.Type, Data: data}); err != nil {
				// The session is told io_error and will retry under a new rid: the verb must not
				// stay queued too, or the cockpit applies it twice.
				if werr := q.Withdraw(realm, it.ID); werr != nil {
					log.Printf("ccsbx-hostd: verb %s could not be withdrawn after a journal failure (%v); the queue for realm %s is now refused until restart", it.ID, werr, realm)
				}
				return hostd.Reply{Status: "error", Result: map[string]string{"code": "io_error"}}
			}
		}
		if r.Type == "ccf.question" && rt != nil {
			notifyQuestion(rt, r, env.Data)
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
		if err := hostd.WriteReply(gdir, hostd.Reply{Rid: it.Rid, ID: it.ID, Status: res.Status, Result: result, Seq: int(it.Seq)}); err != nil {
			log.Printf("ccsbx-hostd: %s: the cockpit's result for %s could not be written to the session: %v", it.Sid, it.Rid, err)
			return false
		}
		return true
	}
}

// pumpReply writes the final result of a queued message into the sender's inbox.
func pumpReply(stateDir string) func(msg.Origin, msg.Result) {
	return func(o msg.Origin, res msg.Result) {
		gdir := filepath.Join(stateDir, "s", o.Sid, fmt.Sprintf("g%d", o.Gen))
		if err := hostd.WriteReply(gdir, hostd.Reply{Rid: o.Rid, ID: o.ID, Status: res.Status, Result: res}); err != nil {
			log.Printf("ccsbx-hostd: %s: the result of message %s could not be written to its sender: %v", o.Sid, o.Rid, err)
		}
	}
}

// notifyQuestion is C23: `question` is a dual verb. The cockpit queue has it (the tree gets its
// ❓), and the manager in scope gets it NOW as a message, so a question is never stuck behind a
// cockpit that is down. The message is the daemon's, from the sender's record: the fence only
// supplied the text. A busy manager's pane gets it when its turn ends (the idle rule).
func notifyQuestion(rt *msg.Router, r hostd.Request, data json.RawMessage) {
	var d struct {
		Text    string   `json:"text"`
		Options []string `json:"options"`
	}
	_ = json.Unmarshal(data, &d)
	if strings.TrimSpace(d.Text) == "" {
		return
	}
	text := "QUESTION: " + d.Text
	if len(d.Options) > 0 {
		text += " [options: " + strings.Join(d.Options, " | ") + "]"
	}
	rt.ToManager(msg.Session{Sid: r.Sid}, "question", text, msg.Origin{})
}

// hearFunc is C23's hear for fenced workers: computed here, from journal facts, and sent to the
// worker's manager as a message (the cockpit may be down). A worker that needs a human decision
// (permission prompt, question dialog) or has ended is something its manager should hear.
func hearFunc(rt *msg.Router) func(journal.Record) {
	// One hear per (sid, gen) per interval, decided BEFORE anything is resolved: a fence that
	// writes permission_prompt notifications in a loop must not cost a directory read and a
	// tmux probe per line.
	var mu sync.Mutex
	last := map[string]time.Time{}
	return func(e journal.Record) {
		var what string
		switch e.Type {
		case "hook.Notification":
			var d struct {
				T string `json:"notification_type"`
			}
			_ = json.Unmarshal(e.Data, &d)
			if d.T == "permission_prompt" || d.T == "elicitation_dialog" {
				what = "needs a decision from a human (" + d.T + ")"
			}
		}
		if what == "" {
			return
		}
		k := fmt.Sprintf("%s/%d", e.Sid, e.Gen)
		mu.Lock()
		if t, ok := last[k]; ok && time.Since(t) < hearEvery {
			mu.Unlock()
			return
		}
		last[k] = time.Now()
		mu.Unlock()
		rt.ToManager(msg.Session{Sid: e.Sid}, "hear", "HEAR: "+e.Node+" "+what, msg.Origin{})
	}
}

// hearEvery is the least time between two hears from one (sid, generation).
const hearEvery = 30 * time.Second

// startPolicy is hostd's start configuration (its flags), apart from who is asking.
type startPolicy struct {
	Pins, WorktreeRoots, Image, CCVersion, TmuxSocket, RunDir string
	MaxLive, MaxPerHour, MinUID                               int
	LookupID                                                  func(string) (*user.User, error)
}

// startHandler is the daemon API's session start: the caller is the socket peer (uid), the spec is
// the owner's request. Refused before anything is written: a caller that is root, hostd's own uid
// (the service user: panes and the tmux server run as it) or below --min-uid, a spec whose owner is
// not the caller's own name (the owner is part of the session's id, the journal and token identity),
// and a worktree-root list ccsbx-root would not accept. Then Daemon.Start with a per-call copy of sd.
func startHandler(sd *start.Daemon, p startPolicy) func(uid int, spec []byte) (*start.StartReply, error) {
	return func(uid int, specJSON []byte) (*start.StartReply, error) {
		if uid == 0 || uid == os.Getuid() || uid < p.MinUID {
			return nil, &start.Refusal{Code: start.BadSpec, Msg: fmt.Sprintf("uid %d may not start sessions (root, the service user, or below the minimum uid %d)", uid, p.MinUID)}
		}
		spec, err := start.ParseSpec(specJSON)
		if err != nil {
			return nil, err
		}
		if err := start.CheckOwner(uid, spec.Owner, p.LookupID); err != nil {
			return nil, err
		}
		d := *sd // a copy per call: Uid differs, everything else is hostd's configuration
		d.Uid, d.RunDir = uid, p.RunDir
		// The roots are passed through unchanged: hostd is a service, a relative entry means nothing to
		// it, and CheckWorkdir refuses exactly what ccsbx-root's allowed_roots refuses.
		d.PinsFile, d.WorktreeRoot = p.Pins, p.WorktreeRoots
		d.Image, d.CCVersion, d.Socket = p.Image, p.CCVersion, p.TmuxSocket
		d.MaxLive, d.MaxPerHour = p.MaxLive, p.MaxPerHour
		return d.Start(spec)
	}
}

// startHubSpoke puts hostd on the session bus (B1): its live fenced sessions are tracked on the hub,
// and deliveries to spoke:<machine>/ccsbx are received (hubRemote.onDeliver verifies and scopes them).
// It returns the function that stops it.
func startHubSpoke(get func(string) string, num func(string) int, rt *msg.Router, j appender, lg *log.Logger, url string) func() {
	tokFile := get("hub-token-file")
	token := func() (string, error) {
		if tokFile == "" {
			return "", fmt.Errorf("--hub-token-file is not set")
		}
		return readHubToken(tokFile)
	}
	machine := get("machine")
	client := &hubc.Client{URL: url, Token: token}
	hr := newHubRemote(client, url, machine, rt, j, lg.Printf, rootMinter(), get("state"))
	rt.Uplink = hr // a message to a node of another machine goes through the hub, signed
	sp := hubspoke.New(hubspoke.Config{
		Client:  client,
		Machine: machine, Name: "ccsbx", TTL: num("hub-ttl"),
		Logf: lg.Printf,
		Entries: func() []map[string]any {
			all, err := rt.Dir.Sessions()
			if err != nil {
				return nil
			}
			hr.pruneKeys()
			return fencedEntries(machine, all, rt.Idle.IdleSince)
		},
		OnDeliver: func(ctx context.Context, f hubc.Frame, d hubc.Deliver) hubspoke.Result {
			return hr.onDeliver(ctx, f, d)
		},
		AfterTrack: func(ctx context.Context) {
			if all, err := rt.Dir.Sessions(); err == nil {
				hr.warnNodeless(ctx, fencedEntries(machine, all, rt.Idle.IdleSince))
			}
		},
	})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { sp.Run(ctx); close(done) }()
	lg.Printf("hub: tracking fenced sessions on %s as spoke:%s/ccsbx", url, machine)
	return func() { cancel(); <-done }
}

// fencedEntries is what hostd publishes on the hub for its live fenced sessions: facts the daemon knows,
// never the session's last output line (B5). `node` is the session's tree node from hostd's OWN record
// (the daemon's, never the fence's): without it no fenced sender could be scoped by a receiver and no
// fenced target could be addressed by tree address with the cockpit down (M2b). The hub keeps it only
// for a fenced entry from this machine's own spoke principal.
func fencedEntries(machine string, all []msg.Session, idleSince func(sid string, gen int) (time.Time, bool)) []map[string]any {
	var out []map[string]any
	for _, s := range all {
		if !s.Live {
			continue
		}
		state := "working"
		if _, idle := idleSince(s.Sid, s.Gen); idle {
			state = "waiting"
		}
		e := map[string]any{"key": "ccsbx:" + s.Sid, "uid": s.Sid, "label": s.Label, "node": s.Label, "machine": machine,
			"fenced": true, "downlink": "spoke:" + machine + "/ccsbx", "attach": "ccsbx:" + s.Sid, "state": state}
		// where this session's manager and peers live, from its record (the owner's start spec): the hub holds
		// those nodes for those machines, so another machine's owner token cannot squat an absent manager's node
		binds := map[string]string{}
		if n, m := s.Scope.Manager.Node, s.Scope.Manager.Machine; n != "" && m != "" {
			binds[n] = m
		}
		for _, p := range s.Scope.Peers {
			if m := s.Scope.PeerMachines[p]; p != "" && m != "" && len(binds) < 16 {
				binds[p] = m
			}
		}
		if len(binds) > 0 {
			e["binds"] = binds
		}
		out = append(out, e)
	}
	return out
}
