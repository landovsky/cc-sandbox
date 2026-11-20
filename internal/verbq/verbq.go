// Package verbq is the cockpit verb queue and lease (M4, plan §4.2, §3 M4).
//
// A fenced session's ccf.* requests (outcome, subtask, checkpoint, rename, mark, done, cctask,
// question) are applied to the tree by the cockpit, not by the daemon. So the daemon answers
// "queued #seq" at once and keeps the request here until a cockpit applies it:
//
//   - One queue per realm (production, e2e). Realms never see each other's requests, leases
//     or results.
//   - A realm has at most one cockpit lease holder. The lease has a TTL (90 s default); only
//     the holder pulls and reports results; a second holder gets ErrLeaseHeld (HTTP 409).
//   - Per-session order: Pull hands out only the OLDEST pending request of each session, so a
//     stuck request holds that session's later verbs back, never other sessions'.
//   - Apply failures come in two kinds. Transient (RepoBusy, lock, commit failed): the request
//     stays pending, is retried after a back-off, shows ⚠ in Status, and the session is not
//     told. Permanent (ok, refused, error, stale): the result goes back to the session's inbox.
//   - The store is append-only and fsynced, separate from the journal, and never pruned. When
//     MaxPending is reached Enqueue refuses with ErrQueueFull ("refused: queue_full").
//   - Enqueue is idempotent per (sid, gen, rid): a re-ingested outbox file (new epoch) does not
//     queue twice.
package verbq

import (
	"bufio"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"sync"
	"time"
)

var (
	ErrLeaseHeld  = errors.New("lease_held")
	ErrLeaseLost  = errors.New("lease_lost")
	ErrQueueFull  = errors.New("queue_full")
	ErrNoSuchItem = errors.New("no_such_request")
	ErrBadRealm   = errors.New("bad_realm")
	ErrBadResult  = errors.New("bad_result")
)

var realmRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)

// Item is one queued verb.
type Item struct {
	ID       string          `json:"id"`  // the daemon ULID of the request
	Seq      int64           `json:"seq"` // position in this realm's queue
	Realm    string          `json:"realm"`
	Sid      string          `json:"sid"`
	Gen      int             `json:"gen"`
	Rid      string          `json:"rid"`
	Node     string          `json:"node"` // the session's node (label), from the daemon
	Type     string          `json:"type"`
	Queued   time.Time       `json:"queued"`
	Body     json.RawMessage `json:"body"` // untrusted fence text
	Attempts int             `json:"attempts,omitempty"`
	LastErr  string          `json:"last_error,omitempty"`
	NotAfter time.Time       `json:"not_before,omitempty"` // transient back-off
}

// Result is what a cockpit reports for one request.
type Result struct {
	Status string          `json:"status"` // ok | refused | error | stale | transient
	Code   string          `json:"code,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Commit string          `json:"commit,omitempty"` // the tree commit that applying it produced
}

// ReplyFunc writes the final result into the session's inbox (in/replies/<rid>.json). It
// reports whether the reply could be written (false: e.g. the generation has ended, and the
// cockpit puts the result into the node's Outcome instead).
type ReplyFunc func(it Item, res Result) bool

// Lease is a realm's cockpit lease.
type Lease struct {
	Realm   string    `json:"realm"`
	Holder  string    `json:"holder"`
	Token   string    `json:"token"`
	Expires time.Time `json:"expires"`
}

// Status is what the view shows: depth, the oldest wait, and the ⚠ items.
type Status struct {
	Realm    string  `json:"realm"`
	Pending  int     `json:"pending"`
	Warnings []Item  `json:"warnings"` // pending with a transient failure
	Lease    *Lease  `json:"lease,omitempty"`
	OldestS  float64 `json:"oldest_age_s"`
}

type rec struct {
	Op   string    `json:"op"` // enq | res | try
	Item *Item     `json:"item,omitempty"`
	ID   string    `json:"id,omitempty"`
	Res  *Result   `json:"res,omitempty"`
	TS   time.Time `json:"ts"`
	Code string    `json:"code,omitempty"`
	NB   time.Time `json:"nb,omitempty"`
}

type realmQ struct {
	f       *os.File
	seq     int64
	pending map[string]*Item  // by id
	keys    map[string]string // sid/gen/rid -> id
	lease   *Lease
	done    int
}

// Queue is the store for every realm.
type Queue struct {
	Dir        string
	TTL        time.Duration
	MaxPending int
	Backoff    time.Duration
	Reply      ReplyFunc
	Now        func() time.Time

	mu     sync.Mutex
	realms map[string]*realmQ
}

// Open opens (creating) the queue directory and replays every realm's log.
func Open(dir string) (*Queue, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	q := &Queue{Dir: dir, TTL: 90 * time.Second, MaxPending: 10000, Backoff: 5 * time.Second, realms: map[string]*realmQ{}}
	ents, _ := os.ReadDir(dir)
	for _, e := range ents {
		if n := e.Name(); len(n) > 6 && n[len(n)-6:] == ".jsonl" {
			if _, err := q.realm(n[:len(n)-6]); err != nil {
				return nil, err
			}
		}
	}
	return q, nil
}

func (q *Queue) now() time.Time {
	if q.Now != nil {
		return q.Now()
	}
	return time.Now()
}

func (q *Queue) realm(name string) (*realmQ, error) {
	if !realmRe.MatchString(name) {
		return nil, ErrBadRealm
	}
	if r := q.realms[name]; r != nil {
		return r, nil
	}
	r := &realmQ{pending: map[string]*Item{}, keys: map[string]string{}}
	p := filepath.Join(q.Dir, name+".jsonl")
	if f, err := os.Open(p); err == nil {
		// No line-length limit (a large --max-bytes makes a long enq line), and only a torn LAST
		// line (no newline: a crash mid-write) is ever cut off. A complete line that does not
		// parse is corruption: refuse to open rather than truncate away every verb after it.
		br := bufio.NewReaderSize(f, 1<<16)
		var good int64
		for {
			line, rerr := br.ReadBytes('\n')
			if rerr != nil { // EOF: whatever is left has no newline, a torn write
				break
			}
			var rc rec
			if json.Unmarshal(line, &rc) != nil || (rc.Op == "enq" && rc.Item == nil) {
				f.Close()
				return nil, fmt.Errorf("verb queue %s is corrupt at byte %d: refusing to open (nothing was cut)", p, good)
			}
			good += int64(len(line))
			switch rc.Op {
			case "enq":
				it := *rc.Item
				r.pending[it.ID] = &it
				r.keys[fmt.Sprintf("%s/%d/%s", it.Sid, it.Gen, it.Rid)] = it.ID
				if it.Seq > r.seq {
					r.seq = it.Seq
				}
			case "res":
				delete(r.pending, rc.ID)
				r.done++
			case "try":
				if it := r.pending[rc.ID]; it != nil {
					it.Attempts++
					it.LastErr = rc.Code
					it.NotAfter = rc.NB
				}
			}
		}
		f.Close()
		// A torn last line (crash mid-write) is cut off, as in the journal.
		if st, err := os.Stat(p); err == nil && st.Size() != good {
			_ = os.Truncate(p, good)
		}
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	r.f = f
	q.realms[name] = r
	return r, nil
}

// write appends one record and fsyncs it. A failed write or fsync is cut back, so no half line
// stays for the next record to run on from (which a restart would answer by truncating away every
// verb after it).
func (r *realmQ) write(rc rec) error {
	b, _ := json.Marshal(rc)
	st, err := r.f.Stat()
	if err != nil {
		return err
	}
	_, err = r.f.Write(append(b, '\n'))
	if err == nil {
		err = r.f.Sync()
	}
	if err != nil {
		if terr := r.f.Truncate(st.Size()); terr != nil {
			return fmt.Errorf("%w (and the cut-back failed: %v)", err, terr)
		}
		return err
	}
	return nil
}

// Withdraw takes a just-enqueued verb back out: the caller could not complete the step that
// had to go with it (journaling it), told the session it failed, and the session will retry
// under a new rid, so the cockpit must not apply it as well. No reply is written.
func (q *Queue) Withdraw(realm, id string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	r, err := q.realm(realm)
	if err != nil {
		return err
	}
	if r.pending[id] == nil {
		return ErrNoSuchItem
	}
	res := Result{Status: "error", Code: "withdrawn"}
	if err := r.write(rec{Op: "res", ID: id, Res: &res, TS: q.now().UTC()}); err != nil {
		return err
	}
	delete(r.pending, id)
	return nil
}

// Enqueue stores a verb and returns it with its seq. A repeat of the same (sid, gen, rid)
// returns the first one.
func (q *Queue) Enqueue(realm string, it Item) (Item, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	r, err := q.realm(realm)
	if err != nil {
		return it, err
	}
	key := fmt.Sprintf("%s/%d/%s", it.Sid, it.Gen, it.Rid)
	if id, ok := r.keys[key]; ok {
		if old := r.pending[id]; old != nil {
			return *old, nil
		}
		return it, nil // already applied and answered; the reply file exists
	}
	if len(r.pending) >= q.MaxPending {
		return it, ErrQueueFull
	}
	it.Realm, it.Seq, it.Queued = realm, r.seq+1, q.now().UTC()
	if err := r.write(rec{Op: "enq", Item: &it, TS: it.Queued}); err != nil {
		return it, err
	}
	r.seq++
	cp := it
	r.pending[it.ID] = &cp
	r.keys[key] = it.ID
	return it, nil
}

// Acquire takes (or renews) the realm's lease. The same holder with the same token renews; a
// different holder is refused while the lease is live, unless steal is set (an owner's explicit
// `cockpit up --steal`).
func (q *Queue) Acquire(realm, holder, token string, steal bool) (Lease, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	r, err := q.realm(realm)
	if err != nil {
		return Lease{}, err
	}
	now := q.now()
	l := r.lease
	live := l != nil && l.Expires.After(now)
	switch {
	case live && l.Holder == holder && token != "" && tokEq(l.Token, token):
		l.Expires = now.Add(q.TTL)
		return *l, nil
	case live && !steal:
		return *l, ErrLeaseHeld
	}
	var tok [12]byte
	_, _ = rand.Read(tok[:])
	r.lease = &Lease{Realm: realm, Holder: holder, Token: hex.EncodeToString(tok[:]), Expires: now.Add(q.TTL)}
	return *r.lease, nil
}

// Release gives the lease up (cockpit down).
func (q *Queue) Release(realm, token string) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	r, err := q.realm(realm)
	if err != nil {
		return err
	}
	if r.lease == nil || !tokEq(r.lease.Token, token) {
		return ErrLeaseLost
	}
	r.lease = nil
	return nil
}

func (q *Queue) holds(r *realmQ, token string) bool {
	return r.lease != nil && token != "" && tokEq(r.lease.Token, token) && r.lease.Expires.After(q.now())
}

// Pull returns pending requests for the lease holder: at most one per session (its oldest),
// skipping those in back-off, in queue order, up to limit (≤0: no limit).
func (q *Queue) Pull(realm, token string, limit int) ([]Item, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	r, err := q.realm(realm)
	if err != nil {
		return nil, err
	}
	if !q.holds(r, token) {
		return nil, ErrLeaseLost
	}
	now := q.now()
	all := make([]*Item, 0, len(r.pending))
	for _, it := range r.pending {
		all = append(all, it)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].Seq < all[j].Seq })
	head := map[string]bool{}
	out := []Item{}
	for _, it := range all {
		if head[it.Sid] {
			continue // a session's later verbs wait for its first
		}
		head[it.Sid] = true
		if it.NotAfter.After(now) {
			continue // transient back-off: the session's later verbs stay held too
		}
		out = append(out, *it)
		if limit > 0 && len(out) >= limit {
			break
		}
	}
	return out, nil
}

// Report records the cockpit's result for a request. Transient: the request stays, back-off
// starts, nothing goes to the session, and false is returned. Permanent: the request leaves
// the queue and the result is sent to the session; written says whether the reply landed.
func (q *Queue) Report(realm, token, id string, res Result) (written bool, err error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	r, err := q.realm(realm)
	if err != nil {
		return false, err
	}
	if !q.holds(r, token) {
		return false, ErrLeaseLost
	}
	it := r.pending[id]
	if it == nil {
		return false, ErrNoSuchItem
	}
	switch res.Status {
	case "transient":
		nb := q.now().Add(q.Backoff)
		if err := r.write(rec{Op: "try", ID: id, Code: res.Code, NB: nb, TS: q.now().UTC()}); err != nil {
			return false, err
		}
		it.Attempts++
		it.LastErr = res.Code
		it.NotAfter = nb
		return false, nil
	case "ok", "refused", "error", "stale":
	default:
		return false, ErrBadResult
	}
	if err := r.write(rec{Op: "res", ID: id, Res: &res, TS: q.now().UTC()}); err != nil {
		return false, err
	}
	done := *it
	delete(r.pending, id)
	r.done++
	if q.Reply != nil {
		written = q.Reply(done, res)
	}
	return written, nil
}

// Pending lists a realm's pending requests, optionally for one node and one type, oldest
// first. It is read-only and needs no lease: a cockpit that is not the holder (a reseed on
// another machine) still has to know what is waiting for a node.
func (q *Queue) Pending(realm, node, typ string) ([]Item, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	r, err := q.realm(realm)
	if err != nil {
		return nil, err
	}
	out := []Item{}
	for _, it := range r.pending {
		if (node == "" || it.Node == node) && (typ == "" || it.Type == typ) {
			out = append(out, *it)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Seq < out[j].Seq })
	return out, nil
}

// Status summarises a realm for the view.
func (q *Queue) Status(realm string) (Status, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	r, err := q.realm(realm)
	if err != nil {
		return Status{}, err
	}
	st := Status{Realm: realm, Pending: len(r.pending), Warnings: []Item{}}
	now := q.now()
	for _, it := range r.pending {
		if it.LastErr != "" {
			st.Warnings = append(st.Warnings, *it)
		}
		if a := now.Sub(it.Queued).Seconds(); a > st.OldestS {
			st.OldestS = a
		}
	}
	sort.Slice(st.Warnings, func(i, j int) bool { return st.Warnings[i].Seq < st.Warnings[j].Seq })
	if r.lease != nil && r.lease.Expires.After(now) {
		l := *r.lease
		l.Token = "" // never shown
		st.Lease = &l
	}
	return st, nil
}

// Close closes the logs.
func (q *Queue) Close() {
	q.mu.Lock()
	defer q.mu.Unlock()
	for _, r := range q.realms {
		r.f.Close()
	}
}

// tokEq compares lease tokens in constant time.
func tokEq(a, b string) bool { return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1 }
