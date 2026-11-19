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
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/verbq"
)

// Server serves one journal.
type Server struct {
	J       *journal.Store
	Q       *verbq.Queue  // nil: the cockpit endpoints answer 404
	MaxWait time.Duration // longest long-poll
	Info    map[string]any
}

// Handler returns the mux.
func (s *Server) Handler() http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/v1/info", s.info)
	m.HandleFunc("/v1/events", s.events)
	m.HandleFunc("/v1/events/stream", s.stream)
	m.HandleFunc("/v1/cursors/", s.cursor)
	m.HandleFunc("/v1/cockpit/lease", s.lease)
	m.HandleFunc("/v1/cockpit/requests", s.requests)
	m.HandleFunc("/v1/cockpit/requests/", s.result)
	m.HandleFunc("/v1/cockpit/status", s.status)
	return m
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) info(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"epoch": s.J.Epoch(), "head": s.J.Head(), "oldest": s.J.Oldest()}
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
func ListenUnix(path string) (net.Listener, error) {
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
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		l.Close()
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
