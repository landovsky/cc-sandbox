// The request handlers of ccsbx-hostd: what a fence's request becomes. It exists apart from main.go so
// that main only wires flags, and these (msg, verb queue + journal, question/hear, replies) can be
// tested without a process: every fence request is untrusted, the daemon's records are the authority.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/url"
	"os"
	"os/user"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/hubc"
	"github.com/landovsky/cc-sandbox/internal/hubspoke"
	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/remotestart"
	"github.com/landovsky/cc-sandbox/internal/start"
	"github.com/landovsky/cc-sandbox/internal/verbq"
	"github.com/landovsky/cc-sandbox/profiles"
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
	// KnownMachines are the machines of this deployment (--known-machines): a msg_scope that places its manager
	// or a peer on any other is refused at start, before anything is published (a typo, or a spec trying to
	// bind someone's node to a machine that is not one of ours). Empty = only the shape is checked.
	KnownMachines []string
	// RemoteMaxPerHour is the hourly cap on starts asked for over the tailnet, counted apart from the local ones.
	RemoteMaxPerHour int
}

var machineIDRe = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

// checkScopeMachines refuses a msg_scope whose manager.machine or peer_machines name a machine that is
// malformed or (when the deployment's machines are listed) unknown.
func checkScopeMachines(raw []byte, known []string) error {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var sc msg.Scope
	if err := json.Unmarshal(raw, &sc); err != nil {
		return &start.Refusal{Code: start.BadSpec, Msg: "msg_scope is not readable"}
	}
	// every node the scope lists must say where it lives: a node placed on no machine reaches nobody (it cannot
	// be told from a decoy), so a spec that lists one is refused here rather than started and silent
	if sc.Manager.Node != "" && sc.Manager.Machine == "" {
		return &start.Refusal{Code: start.BadSpec, Msg: "msg_scope.manager needs its machine"}
	}
	for _, p := range sc.Peers {
		if sc.MachineOf(p) == "" {
			return &start.Refusal{Code: start.BadSpec, Msg: "msg_scope.peers: every peer needs its machine in peer_machines"}
		}
	}
	names := []string{sc.Manager.Machine}
	for _, m := range sc.PeerMachines {
		names = append(names, m)
	}
	for _, m := range names {
		if m == "" {
			continue
		}
		ok := machineIDRe.MatchString(m)
		if ok && len(known) > 0 {
			ok = false
			for _, k := range known {
				if k == m {
					ok = true
				}
			}
		}
		if !ok {
			return &start.Refusal{Code: start.BadSpec, Msg: "msg_scope names a machine this deployment does not know"}
		}
	}
	return nil
}

// warnOnce passes a hub warning to logf at most once per message per ten minutes (a warning repeats with every
// renewal of the session's entry, about every 20 s): "your session's manager node is claimed by <machine>".
func warnOnce(logf func(string, ...any)) func(string) {
	var mu sync.Mutex
	last := map[string]time.Time{}
	return func(w string) {
		mu.Lock()
		defer mu.Unlock()
		if t, ok := last[w]; ok && time.Since(t) < 10*time.Minute {
			return
		}
		last[w] = time.Now()
		logf("hub warning: %s (a session's manager.machine in its start spec, or another daemon's, disagrees with who holds that node; see the runbook: GET/DELETE /v1/nodes)", w)
	}
}

// startHandler is the daemon API's session start: the caller is the socket peer (uid), the spec is
// the owner's request. Refused before anything is written: a caller that is root, hostd's own uid
// (the service user: panes and the tmux server run as it) or below --min-uid, a spec whose owner is
// not the caller's own name (the owner is part of the session's id, the journal and token identity),
// and a worktree-root list ccsbx-root would not accept. Then Daemon.Start with a per-call copy of sd.
func startHandler(sd *start.Daemon, p startPolicy) func(uid int, spec []byte) (*start.StartReply, error) {
	by := startHandlerBy(sd, p, nil)
	return func(uid int, spec []byte) (*start.StartReply, error) { return by(uid, spec, "") }
}

// startHandlerBy is startHandler with the stamped identity of a REMOTE caller ("" for a local one, whose uid is
// the socket peer's). The label check across machines (guard) runs for EVERY start that is not itself the remote
// path (which runs it already): a local `ccsbx start` and a remote start of one label must not both launch.
func startHandlerBy(sd *start.Daemon, p startPolicy, guard *remotestart.Service) func(uid int, spec []byte, startedBy string) (*start.StartReply, error) {
	return func(uid int, specJSON []byte, startedBy string) (*start.StartReply, error) {
		// A REMOTE start (startedBy != "") runs as the operator-configured --owner-user, not as a socket peer: in
		// direct mode that is the user hostd itself runs as (nothing to protect: the panes are that user's too); in
		// helper mode hostd's uid is the service user and the owner must be someone else.
		serviceUser := uid == os.Getuid() && (startedBy == "" || sd.Root == "helper")
		if uid == 0 || serviceUser || uid < p.MinUID {
			return nil, &start.Refusal{Code: start.BadSpec, Msg: fmt.Sprintf("uid %d may not start sessions (root, the service user, or below the minimum uid %d)", uid, p.MinUID)}
		}
		spec, err := start.ParseSpec(specJSON)
		if err != nil {
			return nil, err
		}
		if err := start.CheckOwner(uid, spec.Owner, p.LookupID); err != nil {
			return nil, err
		}
		if err := checkScopeMachines(spec.MsgScope, p.KnownMachines); err != nil {
			return nil, err
		}
		d := *sd // a copy per call: Uid differs, everything else is hostd's configuration
		d.Uid, d.RunDir = uid, p.RunDir
		if d.Root == "helper" && d.WorktreeCheck == nil {
			d.WorktreeCheck = rootWorktreeCheck
		}
		// The roots are passed through unchanged: hostd is a service, a relative entry means nothing to
		// it, and CheckWorkdir refuses exactly what ccsbx-root's allowed_roots refuses.
		d.PinsFile, d.WorktreeRoot = p.Pins, p.WorktreeRoots
		d.Image, d.CCVersion, d.Socket = p.Image, p.CCVersion, p.TmuxSocket
		d.MaxLive, d.MaxPerHour, d.RemoteMaxPerHour, d.StartedBy = p.MaxLive, p.MaxPerHour, p.RemoteMaxPerHour, startedBy
		// A retry already recorded is answered from the record, read-only. Anything else goes through the Guard even
		// if a record vanishes meanwhile (a failed launch rolls its record back): Start's own idempotency is inside it.
		if guard != nil && startedBy == "" {
			if rep, same := d.Recorded(uid, "", spec.RequestID, spec.Label); rep != nil {
				if !same {
					return nil, &start.Refusal{Code: start.RequestIDReused, Msg: "request_id was already used for another session"}
				}
				return rep, nil
			}
			rep, ref := guard.Guard(context.Background(), spec.Label, "local:"+strconv.Itoa(uid), func() (*start.StartReply, error) { return d.Start(spec) })
			if ref != nil {
				return nil, ref
			}
			return rep, nil
		}
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
	client := &hubc.Client{URL: url, Token: token, Warn: warnOnce(lg.Printf)}
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

// startHandlerBy2 is the LOCAL start with the cross-machine label check in front of it.
func startHandlerBy2(sd *start.Daemon, p startPolicy, guard *remotestart.Service) func(uid int, spec []byte) (*start.StartReply, error) {
	by := startHandlerBy(sd, p, guard)
	return func(uid int, spec []byte) (*start.StartReply, error) { return by(uid, spec, "") }
}

// remoteService builds the remote start (M2c) from hostd's flags: nil when --owner-user and --peers are both empty.
// The peer list is validated here (name=url, http or https, no duplicates, never this machine itself), so a typo is
// a startup error and not a start that refuses forever.
func remoteService(get func(string) string, num func(string) int, sd *start.Daemon, p startPolicy, stderr io.Writer) (*remotestart.Service, int) {
	owner, peerList := get("owner-user"), splitList(get("peers"))
	if owner == "" && len(peerList) == 0 {
		remotestart.Unpublish(get("state")) // a stale list must not outlive a config that dropped it
		return nil, 0
	}
	machine := get("machine")
	names := []string{machine}
	var peers []remotestart.Peer
	seen := map[string]bool{machine: true}
	for _, kv := range peerList {
		name, u, ok := strings.Cut(kv, "=")
		pu, err := url.Parse(u)
		if !ok || !machineIDRe.MatchString(name) || seen[name] || err != nil || (pu.Scheme != "http" && pu.Scheme != "https") || pu.Host == "" {
			fmt.Fprintf(stderr, "ccsbx-hostd: --peers %q: want <machine>=<http(s) url>, one per OTHER machine, no repeats\n", kv)
			return nil, cli.ExitUsage
		}
		seen[name] = true
		names = append(names, name)
		peers = append(peers, remotestart.Peer{Name: name, URL: strings.TrimRight(u, "/")})
	}
	timeout, err := time.ParseDuration(get("peer-ask-timeout"))
	if err != nil || timeout <= 0 || timeout > time.Minute {
		fmt.Fprintf(stderr, "ccsbx-hostd: --peer-ask-timeout %q: want a duration up to 1m\n", get("peer-ask-timeout"))
		return nil, cli.ExitUsage
	}
	remoteOn := false
	if owner != "" {
		u, err := user.Lookup(owner)
		if err != nil {
			fmt.Fprintf(stderr, "ccsbx-hostd: --owner-user %q: no such user\n", owner)
			return nil, cli.ExitUsage
		}
		ouid, _ := strconv.Atoi(u.Uid)
		switch {
		case sd.Root == "helper" && ouid == os.Getuid():
			fmt.Fprintln(stderr, "ccsbx-hostd: --owner-user is hostd's own user, which in helper mode is the service user: the owner must be someone else")
			return nil, cli.ExitUsage
		case sd.Root != "helper" && ouid != os.Getuid():
			fmt.Fprintln(stderr, "ccsbx-hostd: in direct mode --owner-user must be the user hostd runs as (the panes and docker are that user's)")
			return nil, cli.ExitUsage
		case sd.Root != "helper" && get("remote-start-direct") != "true":
			fmt.Fprintln(stderr, "ccsbx-hostd: --owner-user in direct mode starts non-evidence fences for anyone on a listed owner device: add --remote-start-direct to say you mean it")
			return nil, cli.ExitUsage
		}
		remoteOn = true
	}
	remoteStart := p
	lookupUID := func(n string) (int, error) {
		u, err := user.Lookup(n)
		if err != nil {
			return 0, err
		}
		return strconv.Atoi(u.Uid)
	}
	svc, err := remotestart.New(remotestart.Config{
		Machine: machine, OwnerUser: owner, Peers: peers, PeerSet: remotestart.PeerSetHash(names), StateDir: filepath.Join(get("state"), "remote"),
		Profiles: profiles.Load,
		LookupUID: func(n string) (int, error) {
			u, err := user.Lookup(n)
			if err != nil {
				return 0, err
			}
			return strconv.Atoi(u.Uid)
		},
		Starter: func(uid int, spec []byte, by string) (*start.StartReply, error) {
			return startHandlerBy(sd, remoteStart, nil)(uid, spec, by)
		},
		Live: func(label string) ([]remotestart.View, error) {
			recs, err := sd.LiveByLabel(label)
			if err != nil {
				return nil, err // could not tell: the answer to a peer is an error, never "nothing live"
			}
			var out []remotestart.View
			for _, r := range recs {
				out = append(out, remotestart.View{Machine: machine, Sid: r.Sid, Gen: r.Gen, State: "running"})
			}
			return out, nil
		},
		Recorded: func(by, rid, label string) (*start.StartReply, bool) {
			uid, err := lookupUID(owner)
			if err != nil {
				return nil, false
			}
			return sd.Recorded(uid, by, rid, label)
		},
		RemoteEnabled: remoteOn,
		Logf:          log.Printf, PerUserMinute: num("remote-per-user-minute"), PerMachineMinute: num("remote-per-machine-minute"), AskTimeout: timeout,
	})
	if err != nil {
		fmt.Fprintf(stderr, "ccsbx-hostd: remote start: %v\n", err)
		return nil, cli.ExitFail
	}
	if err := svc.Publish(); err != nil { // a direct-mode `ccsbx start` reads this and asks the same peers
		fmt.Fprintf(stderr, "ccsbx-hostd: cannot publish the peer list for local starts: %v\n", err)
		return nil, cli.ExitFail
	}
	return svc, 0
}
