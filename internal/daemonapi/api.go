// Package daemonapi is the daemon's HTTP surface for the journal (M3, plan §4.4):
//
//	GET  /v1/info
//	GET  /v1/events?after=&epoch=&realm=&limit=&wait=     replay; wait (seconds) long-polls
//	GET  /v1/events/stream?after=&epoch=&realm=           SSE; the id: field is the seq
//	GET  /v1/cursors/<name>      POST /v1/cursors/<name> {seq, epoch, tree_sha}
//	POST /v1/cockpit/lease?realm=            {holder, token?, steal?}; ?release=1 gives it up
//	GET  /v1/cockpit/requests?realm=&limit=  holder only (header X-Lease: <token>)
//	POST /v1/cockpit/requests/<id>/result?realm=  {status, code, result}; holder only
//	GET  /v1/cockpit/status?realm=           depth, ⚠ items, who holds the lease (no token)
//
// It serves on a unix socket (mode 0600: owner class only) and, optionally, on a loopback TCP
// address for curl. There is no non-loopback listener here: the tailnet listener and the
// WhoIs caller classes are M2a. Everything in "data" is untrusted fence text, passed through.
package daemonapi

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/user"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/remotestart"
	"github.com/landovsky/cc-sandbox/internal/start"
	"github.com/landovsky/cc-sandbox/internal/verbq"
)

// Server serves one journal.
type Server struct {
	J   *journal.Store
	Q   *verbq.Queue // nil: the cockpit endpoints answer 404
	Msg *msg.Router  // nil: the deliver endpoints answer 404
	Dir msg.Directory
	// Replace ends a session's pane and starts generation+1 with a brief (C9b); nil: 404.
	Replace func(sid, brief string) (gen int, err error)
	// Start is session start (helper mode): the owner's `ccsbx start` sends its LaunchSpec here and
	// hostd records and launches the session. uid is the CALLER's, from the socket's peer
	// credentials, never from the body. A policy refusal is a *start.Refusal. nil: 404.
	Start func(uid int, spec []byte) (*start.StartReply, error)
	// Remote is the remote start (M2c: POST /v1/sessions on the tailnet, owner only; GET /v1/resolve?v=2 for
	// the peer label check). nil: both answer 404.
	Remote  *remotestart.Service
	Token   string        // bearer token the TCP listener requires (the unix socket needs none: it is 0600)
	MaxWait time.Duration // longest long-poll
	Info    map[string]any
}

type ctxKey struct{}

func isLocal(r *http.Request) bool { v, _ := r.Context().Value(ctxKey{}).(bool); return v }

// Handler is the API for the unix socket: mode 0600, so only the daemon's own user reaches it
// (owner class). Everything is served, including deliver and the lease's steal.
func (s *Server) Handler() http.Handler { return s.build(true) }

// TCPHandler is the API for the loopback listener. Every request needs the bearer token;
// deliver and steal are not served there at all (owner-class operations, socket only).
func (s *Server) TCPHandler() http.Handler { return s.build(false) }

func (s *Server) build(local bool) http.Handler { return s.mkHandler(local, !local) }

// mkHandler: local says whether owner operations are served; tokenGate whether the bearer token is
// required (the loopback port). The tailnet listener serves owner operations with no token, because
// its callers are identified by Tailscale (see TailnetHandler), never by what they send.
func (s *Server) mkHandler(local, tokenGate bool) http.Handler {
	m := s.mux()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if tokenGate {
			got := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if s.Token == "" || subtle.ConstantTimeCompare([]byte(got), []byte(s.Token)) != 1 {
				writeJSON(w, 401, map[string]any{"error": "unauthorized"})
				return
			}
		}
		m.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, local)))
	})
}

// route is one pattern of the API. routes() is the single list the mux is built from, so that a test can
// walk every route and require the tailnet policy (tailnetRoutes) to classify it.
type route struct {
	Pattern string
	H       http.HandlerFunc
}

func (s *Server) routes() []route {
	return []route{
		{"/v1/resolve", s.resolve},
		{"/v1/sessions", s.sessions},
		{"/v1/sessions/", s.deliver},
		{"/v1/start", s.startSession},
		{"/v1/info", s.info},
		{"/v1/events", s.events},
		{"/v1/events/stream", s.stream},
		{"/v1/cursors/", s.cursor},
		{"/v1/cursors", func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, 200, map[string]any{"epoch": s.J.Epoch(), "head": s.J.Head(), "cursors": s.J.Cursors()})
		}},
		{"/v1/cockpit/lease", s.lease},
		{"/v1/cockpit/requests", s.requests},
		{"/v1/cockpit/requests/", s.result},
		{"/v1/cockpit/status", s.status},
		{"/v1/cockpit/pending", s.pending},
	}
}

func (s *Server) mux() *http.ServeMux {
	m := http.NewServeMux()
	for _, r := range s.routes() {
		m.HandleFunc(r.Pattern, r.H)
	}
	return m
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"epoch": s.J.Epoch(), "head": s.J.Head(), "oldest": s.J.Oldest()}
	if c := classOf(r); c != ClassNone {
		out["caller_class"] = c.String()
	}
	for k, v := range s.Info {
		out[k] = v
	}
	writeJSON(w, 200, out)
}

type query struct {
	after int64
	epoch string
	realm string
	limit int
}

func parse(r *http.Request) (query, error) {
	q := query{epoch: r.URL.Query().Get("epoch"), realm: r.URL.Query().Get("realm")}
	if v := r.URL.Query().Get("after"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil || n < 0 {
			return q, fmt.Errorf("after must be a non-negative integer")
		}
		q.after = n
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			return q, fmt.Errorf("limit must be a non-negative integer")
		}
		q.limit = n
	}
	return q, nil
}

func (s *Server) fail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, journal.ErrEpoch):
		writeJSON(w, 409, map[string]any{"error": "epoch_changed", "epoch": s.J.Epoch()})
	case errors.Is(err, journal.ErrPruned):
		writeJSON(w, 410, map[string]any{"error": "pruned", "oldest": s.J.Oldest(), "epoch": s.J.Epoch()})
	case errors.Is(err, journal.ErrBadCursor), errors.Is(err, journal.ErrUnknownRealm), errors.Is(err, journal.ErrCursorNoSHA), errors.Is(err, journal.ErrCursorBack):
		writeJSON(w, 400, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, 500, map[string]any{"error": "internal"})
	}
}

func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	q, err := parse(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	var wait time.Duration
	if v := r.URL.Query().Get("wait"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSON(w, 400, map[string]any{"error": "wait must be seconds"})
			return
		}
		wait = time.Duration(n) * time.Second
		if s.MaxWait > 0 && wait > s.MaxWait {
			wait = s.MaxWait
		}
	}
	for {
		ch := s.J.Wait() // register before reading, so an append in between is not missed
		recs, err := s.J.After(q.after, q.epoch, q.realm, q.limit)
		if err != nil {
			s.fail(w, err)
			return
		}
		if len(recs) > 0 || wait <= 0 {
			if recs == nil {
				recs = []journal.Record{}
			}
			writeJSON(w, 200, map[string]any{"epoch": s.J.Epoch(), "head": s.J.Head(), "events": recs})
			return
		}
		t := time.NewTimer(wait)
		select {
		case <-ch:
			t.Stop()
		case <-t.C:
			writeJSON(w, 200, map[string]any{"epoch": s.J.Epoch(), "head": s.J.Head(), "events": []journal.Record{}})
			return
		case <-r.Context().Done():
			t.Stop()
			return
		}
	}
}

func (s *Server) stream(w http.ResponseWriter, r *http.Request) {
	q, err := parse(r)
	if err != nil {
		writeJSON(w, 400, map[string]any{"error": err.Error()})
		return
	}
	fl, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, 500, map[string]any{"error": "no streaming"})
		return
	}
	// Check the cursor before committing to a stream, so a stale consumer gets a status code.
	if _, err := s.J.After(q.after, q.epoch, q.realm, 1); err != nil {
		s.fail(w, err)
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(200)
	fmt.Fprintf(w, ": epoch %s\n\n", s.J.Epoch())
	fl.Flush()
	tick := time.NewTicker(15 * time.Second)
	defer tick.Stop()
	for {
		ch := s.J.Wait()
		recs, err := s.J.After(q.after, "", q.realm, 500)
		if err != nil {
			return
		}
		for _, rec := range recs {
			b, _ := json.Marshal(rec)
			fmt.Fprintf(w, "id: %d\nevent: journal\ndata: %s\n\n", rec.Seq, b)
			q.after = rec.Seq
		}
		fl.Flush()
		if len(recs) == 500 {
			continue
		}
		select {
		case <-ch:
		case <-tick.C:
			fmt.Fprint(w, ": keepalive\n\n")
			fl.Flush()
		case <-r.Context().Done():
			return
		}
	}
}

func (s *Server) cursor(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimPrefix(r.URL.Path, "/v1/cursors/")
	switch r.Method {
	case http.MethodGet:
		c, ok, err := s.J.Cursor(name)
		if err != nil {
			s.fail(w, err)
			return
		}
		if !ok {
			writeJSON(w, 404, map[string]any{"error": "no_such_cursor", "epoch": s.J.Epoch()})
			return
		}
		writeJSON(w, 200, c)
	case http.MethodPost:
		var b struct {
			Seq     int64  `json:"seq"`
			Epoch   string `json:"epoch"`
			TreeSHA string `json:"tree_sha"`
		}
		body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
		if json.Unmarshal(body, &b) != nil {
			writeJSON(w, 400, map[string]any{"error": "bad_json"})
			return
		}
		c, err := s.J.Advance(name, b.Seq, b.Epoch, b.TreeSHA)
		if err != nil {
			s.fail(w, err)
			return
		}
		writeJSON(w, 200, c)
	default:
		writeJSON(w, 405, map[string]any{"error": "method"})
	}
}

// ListenUnix listens on a unix socket with mode 0600, replacing a stale socket file.
func ListenUnix(path string) (net.Listener, error) { return ListenUnixGroup(path, "") }

// ListenUnixGroup is ListenUnix, except that with a group name the socket is chgrp'ed to it and
// made 0660, so members of that group (the owner's own user, when hostd runs as a service uid) can
// connect. It fails closed: an unknown group, or a chgrp that is refused, means no listener at all,
// never a socket with the wrong group. The socket is created under umask 077 so it is never
// connectable by anyone between creation and the chmod.
func ListenUnixGroup(path, group string) (net.Listener, error) {
	gid := -1
	if group != "" {
		g, err := user.LookupGroup(group)
		if err != nil {
			return nil, fmt.Errorf("--socket-group %q: %v", group, err)
		}
		if gid, err = strconv.Atoi(g.Gid); err != nil {
			return nil, fmt.Errorf("--socket-group %q: bad gid %q", group, g.Gid)
		}
	}
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSocket == 0 {
			return nil, fmt.Errorf("%s exists and is not a socket", path)
		}
		if c, err := net.DialTimeout("unix", path, time.Second); err == nil {
			c.Close()
			return nil, fmt.Errorf("%s: another daemon is serving", path)
		}
		_ = os.Remove(path)
	}
	old := syscall.Umask(0o077)
	l, err := net.Listen("unix", path)
	syscall.Umask(old)
	if err != nil {
		return nil, err
	}
	mode := os.FileMode(0o600)
	if gid >= 0 {
		if err := os.Chown(path, -1, gid); err != nil {
			l.Close()
			_ = os.Remove(path)
			return nil, fmt.Errorf("--socket-group %q: chgrp %s: %v", group, path, err)
		}
		mode = 0o660
	}
	if err := os.Chmod(path, mode); err != nil {
		l.Close()
		_ = os.Remove(path)
		return nil, err
	}
	return l, nil
}

// ListenLoopback listens on a loopback TCP address only (curl and tests).
func ListenLoopback(addr string) (net.Listener, error) {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return nil, err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return nil, fmt.Errorf("--listen %s: only a loopback address is allowed until M2a (tailnet listener)", addr)
	}
	return net.Listen("tcp", addr)
}

// ---- cockpit verb queue and lease (M4) ----

func (s *Server) qfail(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, verbq.ErrLeaseHeld):
		writeJSON(w, 409, map[string]any{"error": "lease_held"})
	case errors.Is(err, verbq.ErrLeaseLost):
		writeJSON(w, 409, map[string]any{"error": "lease_lost"})
	case errors.Is(err, verbq.ErrBadRealm), errors.Is(err, verbq.ErrBadResult):
		writeJSON(w, 400, map[string]any{"error": err.Error()})
	case errors.Is(err, verbq.ErrNoSuchItem):
		writeJSON(w, 404, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, 500, map[string]any{"error": "internal"})
	}
}

func (s *Server) cockpit(w http.ResponseWriter, r *http.Request) (realm string, ok bool) {
	if s.Q == nil {
		writeJSON(w, 404, map[string]any{"error": "no_queue"})
		return "", false
	}
	realm = r.URL.Query().Get("realm")
	if realm == "" {
		writeJSON(w, 400, map[string]any{"error": "realm is required"})
		return "", false
	}
	return realm, true
}

func (s *Server) lease(w http.ResponseWriter, r *http.Request) {
	realm, ok := s.cockpit(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodPost {
		writeJSON(w, 405, map[string]any{"error": "method"})
		return
	}
	var b struct {
		Holder string `json:"holder"`
		Token  string `json:"token"`
		Steal  bool   `json:"steal"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 4096))
	if len(body) > 0 && json.Unmarshal(body, &b) != nil {
		writeJSON(w, 400, map[string]any{"error": "bad_json"})
		return
	}
	if r.URL.Query().Get("release") == "1" {
		if err := s.Q.Release(realm, b.Token); err != nil {
			s.qfail(w, err)
			return
		}
		writeJSON(w, 200, map[string]any{"released": true})
		return
	}
	if b.Holder == "" {
		writeJSON(w, 400, map[string]any{"error": "holder is required"})
		return
	}
	if b.Steal && !isLocal(r) {
		writeJSON(w, 403, map[string]any{"error": "steal is an owner operation: use the unix socket"})
		return
	}
	l, err := s.Q.Acquire(realm, b.Holder, b.Token, b.Steal)
	if errors.Is(err, verbq.ErrLeaseHeld) {
		// Say who holds it and for how long, never the token.
		writeJSON(w, 409, map[string]any{"error": "lease_held", "holder": l.Holder, "expires": l.Expires})
		return
	}
	if err != nil {
		s.qfail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"realm": l.Realm, "holder": l.Holder, "token": l.Token, "expires": l.Expires, "ttl_s": int(s.Q.TTL.Seconds())})
}

func (s *Server) requests(w http.ResponseWriter, r *http.Request) {
	realm, ok := s.cockpit(w, r)
	if !ok {
		return
	}
	limit := 0
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 0 {
			writeJSON(w, 400, map[string]any{"error": "limit must be a non-negative integer"})
			return
		}
		limit = n
	}
	items, err := s.Q.Pull(realm, r.Header.Get("X-Lease"), limit)
	if err != nil {
		s.qfail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"realm": realm, "items": items})
}

func (s *Server) result(w http.ResponseWriter, r *http.Request) {
	realm, ok := s.cockpit(w, r)
	if !ok {
		return
	}
	id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/v1/cockpit/requests/"), "/result")
	if r.Method != http.MethodPost || id == "" || strings.Contains(id, "/") {
		writeJSON(w, 405, map[string]any{"error": "POST /v1/cockpit/requests/<id>/result"})
		return
	}
	var res verbq.Result
	body, _ := io.ReadAll(io.LimitReader(r.Body, 64<<10))
	if json.Unmarshal(body, &res) != nil {
		writeJSON(w, 400, map[string]any{"error": "bad_json"})
		return
	}
	written, err := s.Q.Report(realm, r.Header.Get("X-Lease"), id, res)
	if err != nil {
		s.qfail(w, err)
		return
	}
	writeJSON(w, 200, map[string]any{"applied": res.Status != "transient", "reply_written": written})
}

func (s *Server) status(w http.ResponseWriter, r *http.Request) {
	realm, ok := s.cockpit(w, r)
	if !ok {
		return
	}
	st, err := s.Q.Status(realm)
	if err != nil {
		s.qfail(w, err)
		return
	}
	writeJSON(w, 200, st)
}

// ---- session start (helper mode) ----

type peerKey struct{}

// startSession: POST /v1/start, body = the LaunchSpec JSON. Owner class: the unix socket only,
// never the loopback port (a token holder must not be able to start fenced sessions). 200 with the
// StartReply; 409 with the Refusal; 500 {"error"}.
func (s *Server) startSession(w http.ResponseWriter, r *http.Request) {
	if !isLocal(r) || r.Method != http.MethodPost {
		writeJSON(w, 403, map[string]any{"error": "owner operation: POST on the unix socket"})
		return
	}
	if s.Start == nil {
		writeJSON(w, 404, map[string]any{"error": "no_start"})
		return
	}
	uid, ok := r.Context().Value(peerKey{}).(int)
	if !ok {
		writeJSON(w, 500, map[string]any{"error": "no peer credentials on this connection: refusing to start a session for an unknown caller"})
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 256<<10+1))
	if err != nil || len(body) > 256<<10 {
		writeJSON(w, 400, map[string]any{"error": "the spec is unreadable or larger than 256 KiB"})
		return
	}
	rep, err := s.Start(uid, body)
	var ref *start.Refusal
	switch {
	case errors.As(err, &ref):
		writeJSON(w, 409, ref)
	case err != nil:
		writeJSON(w, 500, map[string]any{"error": err.Error()})
	default:
		writeJSON(w, 200, rep)
	}
}

// ---- local message delivery (U3b, plan §4.4) ----

// resolve: GET /v1/resolve?label= -> the live session for a node, and whether its pane is at the
// input prompt. Owner class (socket only) or, on the tailnet listener, a peer daemon (another machine's
// hostd asks it, M2c), like the session list.
func (s *Server) resolve(w http.ResponseWriter, r *http.Request) {
	if r.URL.Query().Get("v") == "2" {
		s.resolveV2(w, r)
		return
	}
	if s.Msg == nil || s.Dir == nil {
		writeJSON(w, 404, map[string]any{"error": "no_messaging"})
		return
	}
	if !isLocal(r) && classOf(r) < ClassPeer {
		writeJSON(w, 403, map[string]any{"error": "owner operation: use the unix socket"})
		return
	}
	all, err := s.Dir.Sessions()
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "internal"})
		return
	}
	t := msg.Resolve(all, r.URL.Query().Get("label"))
	if t == nil {
		writeJSON(w, 404, map[string]any{"error": "no_live_session"})
		return
	}
	writeJSON(w, 200, map[string]any{"sid": t.Sid, "label": t.Label, "gen": t.Gen, "realm": t.Realm, "idle": s.Msg.IdleNow(t.Sid, t.Gen)})
}

// deliver: POST /v1/sessions/<sid>/deliver {from, text, queue}. The stamp is
// "trust: unfenced (owner device)", never "human". 200 delivered, 202 queued, 404 no live
// session, 409 target_busy (not at its prompt; the text was NOT typed).
func (s *Server) deliver(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	verb := "deliver"
	sid, ok := strings.CutSuffix(rest, "/deliver")
	if !ok {
		verb = "prompt"
		sid, ok = strings.CutSuffix(rest, "/prompt")
	}
	if !ok {
		verb = "replace"
		sid, ok = strings.CutSuffix(rest, "/replace")
	}
	if !ok || sid == "" || strings.Contains(sid, "/") {
		writeJSON(w, 404, map[string]any{"error": "not_found"})
		return
	}
	if s.Msg == nil {
		writeJSON(w, 404, map[string]any{"error": "no_messaging"})
		return
	}
	if !isLocal(r) || r.Method != http.MethodPost {
		writeJSON(w, 403, map[string]any{"error": "owner operation: POST on the unix socket"})
		return
	}
	var b struct {
		From  string `json:"from"`
		Text  string `json:"text"`
		Queue bool   `json:"queue"`
		Kind  string `json:"kind"`
		Brief string `json:"brief"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 256<<10))
	if json.Unmarshal(body, &b) != nil {
		writeJSON(w, 400, map[string]any{"error": "bad_json"})
		return
	}
	switch verb {
	case "prompt": // steering text from the owner: typed only at the prompt, never queued
		b.From, b.Queue = "cc-tree:"+b.Kind, false
	case "replace":
		s.replace(w, sid, b.Brief)
		return
	}
	res := s.Msg.DeliverOwner(b.From, sid, b.Text, b.Queue)
	code := 200
	switch {
	case res.Status == "queued":
		code = 202
	case res.Code == "no_live_session":
		code = 404
	case res.Code == "target_busy":
		code = 409
	case res.Status != "ok":
		code = 502
	}
	writeJSON(w, code, res)
}

// pending: GET /v1/cockpit/pending?realm=&node=&type= — what is queued for a node (read-only, no
// lease: a reseed must not seed from a checkpoint older than a pending one).
func (s *Server) pending(w http.ResponseWriter, r *http.Request) {
	realm, ok := s.cockpit(w, r)
	if !ok {
		return
	}
	items, err := s.Q.Pending(realm, r.URL.Query().Get("node"), r.URL.Query().Get("type"))
	if err != nil {
		s.qfail(w, err)
		return
	}
	writeJSON(w, 200, items)
}

// sessions: GET /v1/sessions — the sessions the daemon runs now, as a JSON array.
func (s *Server) sessions(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodPost {
		s.createSession(w, r)
		return
	}
	if s.Dir == nil {
		writeJSON(w, 404, map[string]any{"error": "no_directory"})
		return
	}
	all, err := s.Dir.Sessions()
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": "internal"})
		return
	}
	out := []map[string]any{}
	for _, x := range all {
		if !x.Live {
			continue
		}
		idle := false
		if s.Msg != nil {
			idle = s.Msg.IdleNow(x.Sid, x.Gen)
		}
		out = append(out, map[string]any{"sid": x.Sid, "label": x.Label, "gen": x.Gen, "realm": x.Realm, "state": "running", "idle": idle})
	}
	writeJSON(w, 200, out)
}

// replace: POST /v1/sessions/<sid>/replace {brief}: session.replace. Only at a turn boundary
// (the pane is at its prompt): 409 target_busy otherwise, nothing touched. 200 {sid, gen}.
func (s *Server) replace(w http.ResponseWriter, sid, brief string) {
	if s.Replace == nil || s.Msg == nil || s.Dir == nil {
		writeJSON(w, 404, map[string]any{"error": "no_replace"})
		return
	}
	if strings.TrimSpace(brief) == "" {
		writeJSON(w, 400, map[string]any{"error": "a brief is required"})
		return
	}
	all, _ := s.Dir.Sessions()
	var cur *msg.Session
	for i := range all {
		if all[i].Sid == sid && all[i].Live {
			cur = &all[i]
		}
	}
	if cur == nil {
		writeJSON(w, 404, map[string]any{"error": "no_live_session"})
		return
	}
	var gen int
	ran, err := s.Msg.WithIdle(sid, cur.Gen, func() (e error) { gen, e = s.Replace(sid, brief); return e })
	if !ran {
		writeJSON(w, 409, map[string]any{"error": "target_busy"})
		return
	}
	if err != nil {
		writeJSON(w, 500, map[string]any{"error": err.Error()})
		return
	}
	writeJSON(w, 200, map[string]any{"v": 1, "sid": sid, "gen": gen})
}
