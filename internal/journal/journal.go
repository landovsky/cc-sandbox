// Package journal is the daemon's event journal (M3, plan §4.3): an append-only, fsync'ed,
// per-machine sequence of records that a cockpit replays from a cursor.
//
//   - Seq has no gaps within one (machine, epoch); the epoch is minted when the journal
//     directory is created, so a wiped journal is recognisable by a consumer.
//   - id is the daemon's ULID, the idempotency key.
//   - Segments are one file per UTC day; retention prunes whole segments older than
//     RetentionDays, which can never be set below MinRetentionDays (14).
//   - Every Append is fsync'ed before it returns: an acknowledged event survives a crash. A
//     torn last line (crash mid-write) is cut off on open, and the next seq continues from the
//     last complete record, so there is never a gap or a duplicate.
//   - Named cursors (one per realm) live here too, and advance only together with the commit
//     SHA of the tree commit they produced (Store.Advance).
//
// The cockpit's request queue is NOT here: it is a separate store that is never pruned (M4).
package journal

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/landovsky/cc-sandbox/internal/ulid"
)

// MinRetentionDays is the floor for retention (plan M3: ≥ 14 days).
const MinRetentionDays = 14

// Record is one journal line (plan §4.3).
type Record struct {
	V       int             `json:"v"`
	Epoch   string          `json:"epoch"`
	Seq     int64           `json:"seq"`
	ID      string          `json:"id"`
	TS      time.Time       `json:"ts"`
	Machine string          `json:"machine"`
	Sid     string          `json:"sid,omitempty"`
	Gen     int             `json:"gen,omitempty"`
	Node    string          `json:"node,omitempty"`
	Realm   string          `json:"realm,omitempty"`
	Src     string          `json:"src"` // fence | host | door
	Type    string          `json:"type"`
	Data    json.RawMessage `json:"data,omitempty"`
}

// Event is what a caller appends; the store fills v, epoch, seq, id, ts, machine.
type Event struct {
	Sid, Node, Realm, Src, Type string
	Gen                         int
	Data                        json.RawMessage
	// ID, when set (a valid ULID), is used as the record's id instead of a fresh one: a fence
	// request keeps the id the daemon gave it, so the cockpit's result names the same id.
	ID string
}

var (
	ErrPruned       = errors.New("pruned")        // the cursor is older than the oldest retained record
	ErrEpoch        = errors.New("epoch_changed") // the caller's epoch is not this journal's
	ErrBadCursor    = errors.New("bad_cursor")
	ErrCursorNoSHA  = errors.New("cursor_needs_new_tree_sha")
	ErrCursorBack   = errors.New("cursor_moved_back")
	ErrUnknownRealm = errors.New("bad_cursor_name")
)

// Store is a journal directory.
type Store struct {
	Dir           string
	Machine       string
	RetentionDays int
	Now           func() time.Time

	mu      sync.Mutex
	epoch   string
	seq     int64 // last assigned
	oldest  int64 // first retained seq (0 when empty)
	cur     *os.File
	curDay  string
	waiters []chan struct{}
	hooks   map[string]hookState // sid -> the last hook event the fence reported
}

type hookState struct {
	Type string
	TS   time.Time
}

var segRe = regexp.MustCompile(`^seg-(\d{8})\.jsonl$`)

// Open opens (creating if needed) the journal in dir. A torn last line is cut off.
func Open(dir, machine string, retentionDays int) (*Store, error) {
	if retentionDays == 0 {
		retentionDays = MinRetentionDays
	}
	if retentionDays < MinRetentionDays {
		return nil, fmt.Errorf("retention %d days is below the %d-day floor", retentionDays, MinRetentionDays)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	s := &Store{Dir: dir, Machine: machine, RetentionDays: retentionDays}
	ep := filepath.Join(dir, "epoch")
	if b, err := os.ReadFile(ep); err == nil && len(bytes.TrimSpace(b)) > 0 {
		s.epoch = strings.TrimSpace(string(b))
	} else {
		s.epoch = "e" + strings.ToLower(ulid.New()[16:])
		if err := writeAtomic(ep, []byte(s.epoch+"\n")); err != nil {
			return nil, err
		}
	}
	if err := s.recover(); err != nil {
		return nil, err
	}
	s.rebuildHooks()
	return s, nil
}

// rebuildHooks restores the per-session "last hook" from the newest segment, so a daemon
// restart doesn't forget that a session is mid-turn. A session with no hook event in it is
// simply unknown, and unknown is never idle.
func (s *Store) rebuildHooks() {
	s.hooks = map[string]hookState{}
	segs := s.segments()
	for i := len(segs) - 1; i >= 0 && i >= len(segs)-2; i-- {
		recs, _, _ := scan(filepath.Join(s.Dir, segs[i]))
		for _, r := range recs {
			s.noteHook(r)
		}
	}
}

func (s *Store) noteHook(r Record) {
	if r.Src == "fence" && strings.HasPrefix(r.Type, "hook.") && r.Sid != "" {
		s.hooks[r.Sid] = hookState{r.Type, r.TS}
	}
}

// IdleSince reports whether the session's last hook event is Stop, i.e. its turn is over and
// Claude Code is sitting at the input prompt, and when that happened. Anything else (a prompt
// submitted, a tool running, a permission Notification, nothing seen yet) is not idle: the pane
// may be showing a menu, and a message typed into a menu answers it.
func (s *Store) IdleSince(sid string) (time.Time, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	h, ok := s.hooks[sid]
	if !ok || h.Type != "hook.Stop" {
		return time.Time{}, false
	}
	return h.TS, true
}

func (s *Store) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

// Epoch is this journal's epoch.
func (s *Store) Epoch() string { return s.epoch }

// Head returns the last assigned seq (0: empty).
func (s *Store) Head() int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.seq }

func (s *Store) segments() []string {
	ents, _ := os.ReadDir(s.Dir)
	var out []string
	for _, e := range ents {
		if segRe.MatchString(e.Name()) {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

// recover finds the last seq and cuts a torn tail off the newest segment.
func (s *Store) recover() error {
	segs := s.segments()
	for i := len(segs) - 1; i >= 0; i-- {
		p := filepath.Join(s.Dir, segs[i])
		recs, goodLen, err := scan(p)
		if err != nil {
			return err
		}
		if i == len(segs)-1 {
			if st, err := os.Stat(p); err == nil && st.Size() != goodLen {
				if err := os.Truncate(p, goodLen); err != nil {
					return err
				}
			}
		}
		if len(recs) > 0 && s.seq == 0 {
			s.seq = recs[len(recs)-1].Seq
		}
	}
	for _, name := range segs {
		recs, _, _ := scan(filepath.Join(s.Dir, name))
		if len(recs) > 0 {
			s.oldest = recs[0].Seq
			break
		}
	}
	if s.seq == 0 {
		// Segments may have been fully pruned: continue from the persisted high-water mark.
		if b, err := os.ReadFile(filepath.Join(s.Dir, "seq")); err == nil {
			fmt.Sscanf(string(b), "%d", &s.seq)
		}
	}
	return nil
}

// scan reads complete, valid lines of a segment; goodLen is the byte length of that prefix.
func scan(path string) (recs []Record, goodLen int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, 0, err
	}
	defer f.Close()
	r := bufio.NewReaderSize(f, 1<<16)
	for {
		line, err := r.ReadBytes('\n')
		if err != nil || len(line) == 0 {
			break // no trailing newline: a torn write
		}
		var rec Record
		if json.Unmarshal(line, &rec) != nil || rec.Seq == 0 {
			break
		}
		recs = append(recs, rec)
		goodLen += int64(len(line))
	}
	return recs, goodLen, nil
}

// Append assigns the next seq, writes and fsyncs the record, and wakes streamers.
func (s *Store) Append(e Event) (Record, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	t := s.now().UTC()
	id := e.ID
	if !ulid.Valid(id) {
		id = ulid.At(t)
	}
	rec := Record{V: 1, Epoch: s.epoch, Seq: s.seq + 1, ID: id, TS: t, Machine: s.Machine,
		Sid: e.Sid, Gen: e.Gen, Node: e.Node, Realm: e.Realm, Src: e.Src, Type: e.Type, Data: e.Data}
	b, err := json.Marshal(rec)
	if err != nil {
		return rec, err
	}
	day := t.Format("20060102")
	if s.cur == nil || s.curDay != day {
		if s.cur != nil {
			s.cur.Close()
		}
		f, err := os.OpenFile(filepath.Join(s.Dir, "seg-"+day+".jsonl"), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
		if err != nil {
			return rec, err
		}
		s.cur, s.curDay = f, day
	}
	if _, err := s.cur.Write(append(b, '\n')); err != nil {
		return rec, err
	}
	if err := s.cur.Sync(); err != nil {
		return rec, err
	}
	s.seq = rec.Seq
	s.noteHook(rec)
	if s.oldest == 0 {
		s.oldest = rec.Seq
	}
	for _, w := range s.waiters {
		close(w)
	}
	s.waiters = nil
	return rec, nil
}

// Wait returns a channel closed at the next Append (for SSE and long-poll).
func (s *Store) Wait() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	ch := make(chan struct{})
	s.waiters = append(s.waiters, ch)
	return ch
}

// After returns up to limit records with seq > after (limit ≤ 0: all), optionally for one
// realm. epoch, when non-empty, must be this journal's: a consumer that remembers an older
// epoch gets ErrEpoch and must start over. after older than the retained range gives ErrPruned.
func (s *Store) After(after int64, epoch, realm string, limit int) ([]Record, error) {
	s.mu.Lock()
	head, oldest, cur := s.seq, s.oldest, s.epoch
	s.mu.Unlock()
	if epoch != "" && epoch != cur {
		return nil, ErrEpoch
	}
	if after < 0 || after > head {
		return nil, ErrBadCursor
	}
	if oldest > 0 && after+1 < oldest {
		return nil, ErrPruned
	}
	var out []Record
	for _, name := range s.segments() {
		recs, _, err := scan(filepath.Join(s.Dir, name))
		if err != nil {
			return nil, err
		}
		for _, r := range recs {
			if r.Seq <= after || r.Seq > head {
				continue
			}
			if realm != "" && r.Realm != realm {
				continue
			}
			out = append(out, r)
			if limit > 0 && len(out) >= limit {
				return out, nil
			}
		}
	}
	return out, nil
}

// Oldest is the first retained seq (0: empty).
func (s *Store) Oldest() int64 { s.mu.Lock(); defer s.mu.Unlock(); return s.oldest }

// Prune deletes whole segments whose day is older than the retention. The newest segment is
// never pruned. The high-water seq is persisted first so seq stays gapless if everything goes.
func (s *Store) Prune() (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	cutoff := s.now().UTC().AddDate(0, 0, -s.RetentionDays).Format("20060102")
	segs := s.segments()
	if err := writeAtomic(filepath.Join(s.Dir, "seq"), []byte(fmt.Sprint(s.seq)+"\n")); err != nil {
		return 0, err
	}
	n := 0
	for i, name := range segs {
		if i == len(segs)-1 {
			break
		}
		if day := segRe.FindStringSubmatch(name)[1]; day < cutoff {
			if err := os.Remove(filepath.Join(s.Dir, name)); err != nil {
				return n, err
			}
			n++
		}
	}
	s.oldest = 0
	for _, name := range s.segments() {
		if recs, _, _ := scan(filepath.Join(s.Dir, name)); len(recs) > 0 {
			s.oldest = recs[0].Seq
			break
		}
	}
	return n, nil
}

// Close releases the open segment.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.cur != nil {
		err := s.cur.Close()
		s.cur = nil
		return err
	}
	return nil
}

func writeAtomic(path string, b []byte) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	f.Close()
	return os.Rename(tmp, path)
}

// ---- named cursors ----

// Cursor is a consumer's position: the last seq it applied, and the tree commit that applying
// produced (a new cockpit checks that SHA is an ancestor of its HEAD before it pulls).
type Cursor struct {
	Name    string    `json:"name"`
	Seq     int64     `json:"seq"`
	Epoch   string    `json:"epoch"`
	TreeSHA string    `json:"tree_sha"`
	Updated time.Time `json:"updated"`
}

var (
	nameRe = regexp.MustCompile(`^[a-z][a-z0-9-]{0,31}$`)
	shaRe  = regexp.MustCompile(`^[0-9a-f]{7,64}$`)
)

func (s *Store) cursorPath(name string) string { return filepath.Join(s.Dir, "cursors", name+".json") }

// Cursor returns the named cursor; ok=false when it was never advanced.
func (s *Store) Cursor(name string) (Cursor, bool, error) {
	if !nameRe.MatchString(name) {
		return Cursor{}, false, ErrUnknownRealm
	}
	b, err := os.ReadFile(s.cursorPath(name))
	if err != nil {
		return Cursor{}, false, nil
	}
	var c Cursor
	return c, true, json.Unmarshal(b, &c)
}

// Advance moves the named cursor to seq, but only together with the SHA of the tree commit
// that applying the events produced (or, when those events changed nothing, the tree's HEAD
// then): without a valid SHA it refuses, so a cursor never runs ahead of what is committed in
// the tree repo, and a taking-over cockpit can check the SHA is an ancestor of its HEAD. Read
// cursors (tab-<machine>) need none. It never moves back, never past the head of this journal,
// never into another epoch.
func (s *Store) Advance(name string, seq int64, epoch, treeSHA string) (Cursor, error) {
	if !nameRe.MatchString(name) {
		return Cursor{}, ErrUnknownRealm
	}
	if epoch != s.epoch {
		return Cursor{}, ErrEpoch
	}
	// A read cursor (tab-<machine>: tab state only, no tree writes) has no tree commit to show.
	readOnly := strings.HasPrefix(name, "tab-")
	if !readOnly && !shaRe.MatchString(treeSHA) {
		return Cursor{}, ErrCursorNoSHA
	}
	if seq < 0 || seq > s.Head() {
		return Cursor{}, ErrBadCursor
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	prev, had, err := s.Cursor(name)
	if err != nil {
		return Cursor{}, err
	}
	if had {
		if seq < prev.Seq {
			return Cursor{}, ErrCursorBack
		}
	}
	c := Cursor{Name: name, Seq: seq, Epoch: epoch, TreeSHA: treeSHA, Updated: s.now().UTC()}
	if err := os.MkdirAll(filepath.Dir(s.cursorPath(name)), 0o700); err != nil {
		return c, err
	}
	b, _ := json.Marshal(c)
	return c, writeAtomic(s.cursorPath(name), append(b, '\n'))
}
