// Package hostd is ccsbx-hostd v1 (U3a): it watches every live fence outbox and turns the
// request files in them into stamped, daemon-identified requests.
//
// The fence is untrusted. So the daemon
//   - opens each file with openat2(RESOLVE_BENEATH|RESOLVE_NO_SYMLINKS) and O_NONBLOCK, then
//     judges the opened file: regular, one link, owned by the fence uid, within the size cap;
//   - never echoes request bytes (refusals, logs and replies carry rule names, never content);
//   - takes identity (sid, gen, label, id) from the outbox PATH and the session record, never
//     from the request body;
//   - gives every request a daemon-minted ULID (the idempotency key);
//   - caps size and rate per outbox, and sheds a flood instead of queueing it.
//
// Layout read: <state>/s/<sid>/g<gen>/out/requests/<rid>.json; replies are written to
// <state>/s/<sid>/g<gen>/in/replies/<rid>.json (the fence mounts in/ read-only).
//
// Direct mode (tonight): the daemon runs as the same user as everyone else, so the outbox is
// writable by the host and T:outbox-host-write-refused stays red until gate M.
package hostd

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/landovsky/cc-sandbox/internal/start"
	"github.com/landovsky/cc-sandbox/internal/ulid"
)

// Request is a fence request after stamping. Everything except Body comes from the daemon.
type Request struct {
	ID       string          `json:"id"`      // daemon ULID
	Rid      string          `json:"rid"`     // the file's name; advisory, the fence picks it
	Sid      string          `json:"sid"`     // from the outbox path
	Gen      int             `json:"gen"`     // from the outbox path
	Label    string          `json:"label"`   // from the daemon's session record
	FullID   string          `json:"full_id"` // owner/machine/sid/gN, from the record
	Type     string          `json:"type"`
	Received time.Time       `json:"received"`
	Body     json.RawMessage `json:"body"` // untrusted text; never interpreted by the daemon
}

// Reply is what the fence reads back from in/replies/<rid>.json.
type Reply struct {
	Rid    string `json:"rid"`
	ID     string `json:"id"`
	Status string `json:"status"` // queued|ok|refused|error|stale
	Result any    `json:"result,omitempty"`
	Seq    int    `json:"seq,omitempty"`
}

// Handler answers one stamped request. M4's queue and U3b's delivery plug in here.
type Handler func(Request) Reply

// Types are the request types the fence may send (plan §4.2).
var Types = map[string]bool{
	"ccf.outcome": true, "ccf.subtask": true, "ccf.checkpoint": true, "ccf.rename": true,
	"ccf.mark": true, "ccf.done": true, "ccf.cctask": true, "ccf.question": true,
	"msg.send": true, "msg.reply": true,
	"git.commit": true, "git.push": true,
	"grant.request": true,
}

// Config holds every tunable (all reachable as ccsbx-hostd flags / CCSBX_* env vars).
type Config struct {
	StateDir string
	FenceUID int     // owner uid required of every outbox file
	MaxBytes int64   // per request file
	Rate     float64 // requests per second per outbox
	Burst    int     // bucket size per outbox
	MaxList  int     // names read per outbox per scan
	MaxTotal int     // requests per generation in its lifetime; beyond: shed silently
	ShedRefs int     // refusal replies written per scan once the bucket is empty
	Handler  Handler // nil: every known type is answered error/not_built
	Now      func() time.Time
	Logf     func(format string, args ...any)
}

// Defaults fills the zero values.
func (c *Config) Defaults() {
	if c.FenceUID == 0 {
		c.FenceUID = 10001
	}
	if c.MaxBytes == 0 {
		c.MaxBytes = 64 << 10
	}
	if c.Rate == 0 {
		c.Rate = 10
	}
	if c.Burst == 0 {
		c.Burst = 20
	}
	if c.MaxList == 0 {
		c.MaxList = 10000
	}
	if c.MaxTotal == 0 {
		c.MaxTotal = 20000
	}
	if c.ShedRefs == 0 {
		c.ShedRefs = 20
	}
	if c.Now == nil {
		c.Now = time.Now
	}
	if c.Logf == nil {
		c.Logf = func(string, ...any) {}
	}
	if c.Handler == nil {
		c.Handler = func(r Request) Reply {
			return Reply{Rid: r.Rid, ID: r.ID, Status: "error", Result: map[string]string{"code": "not_built"}}
		}
	}
}

// Stats counts what Scan did, for tests and for the status line.
type Stats struct {
	Accepted, Refused, Shed int
	Reasons                 map[string]int
}

type outbox struct {
	seen     map[string]uint64 // name -> inode already processed
	tokens   float64
	last     time.Time
	total    int
	overflow bool
	drained  bool
}

// Hostd is the watcher. Scan is one pass; Run loops it.
type Hostd struct {
	Cfg   Config
	mu    sync.Mutex
	boxes map[string]*outbox
	start *start.Daemon
}

func New(cfg Config) *Hostd {
	cfg.Defaults()
	return &Hostd{Cfg: cfg, boxes: map[string]*outbox{}, start: &start.Daemon{StateDir: cfg.StateDir}}
}

var (
	sidRe  = regexp.MustCompile(`^s[0-9a-z]{4,32}$`)
	genRe  = regexp.MustCompile(`^g[1-9][0-9]{0,6}$`)
	nameRe = regexp.MustCompile(`^([A-Za-z0-9][A-Za-z0-9_-]{0,63})\.json$`)
)

// Scan walks every live outbox once. A generation with a CLOSED marker is drained one last
// time and then forgotten.
func (h *Hostd) Scan() Stats {
	h.mu.Lock()
	defer h.mu.Unlock()
	st := Stats{Reasons: map[string]int{}}
	sess, _ := filepath.Glob(filepath.Join(h.Cfg.StateDir, "s", "s*", "g*"))
	for _, gdir := range sess {
		sid, gen := filepath.Base(filepath.Dir(gdir)), filepath.Base(gdir)
		if !sidRe.MatchString(sid) || !genRe.MatchString(gen) {
			continue
		}
		_, closedErr := os.Stat(filepath.Join(gdir, "CLOSED"))
		box := h.boxes[gdir]
		if box == nil {
			box = &outbox{seen: map[string]uint64{}, tokens: float64(h.Cfg.Burst), last: h.Cfg.Now()}
			h.loadLedger(gdir, box)
			h.boxes[gdir] = box
		}
		if box.drained {
			continue
		}
		h.scanOne(gdir, sid, gen, box, &st)
		if closedErr == nil {
			box.drained = true
			box.seen = nil
		}
	}
	return st
}

func (h *Hostd) scanOne(gdir, sid, gen string, box *outbox, st *Stats) {
	root, err := OpenRoot(filepath.Join(gdir, "out"))
	if err != nil {
		return
	}
	defer root.Close()
	names, _, err := root.ListDir("requests", h.Cfg.MaxList)
	if err != nil {
		if err != nil {
			st.Reasons["requests_dir_"+classify(err)]++
		}
		return
	}
	rec, err := h.start.Load(sid)
	if err != nil {
		st.Reasons["unknown_session"]++
		return // no record: nothing to stamp from; the files stay for a later pass
	}
	var genN int
	fmt.Sscanf(gen, "g%d", &genN)

	now := h.Cfg.Now()
	box.tokens += now.Sub(box.last).Seconds() * h.Cfg.Rate
	if box.tokens > float64(h.Cfg.Burst) {
		box.tokens = float64(h.Cfg.Burst)
	}
	box.last = now
	shedReplies := h.Cfg.ShedRefs

	for _, n := range names {
		m := nameRe.FindStringSubmatch(n)
		if m == nil {
			continue // .tmp files and anything oddly named are not requests
		}
		if _, done := box.seen[n]; done {
			continue
		}
		if box.total >= h.Cfg.MaxTotal {
			if !box.overflow {
				box.overflow = true
				h.Cfg.Logf("hostd: %s/%s: outbox over its lifetime cap (%d); shedding the rest", sid, gen, h.Cfg.MaxTotal)
			}
			box.seen[n] = 0
			st.Shed++
			continue
		}
		box.total++
		rid := m[1]
		if box.tokens < 1 {
			// Flood: answer a few, drop the rest. Nothing is queued, so the backlog is bounded.
			box.seen[n] = 0
			h.ledger(gdir, n, 0, "", "shed")
			if shedReplies > 0 {
				shedReplies--
				h.reply(gdir, Reply{Rid: rid, ID: ulid.New(), Status: "refused", Result: map[string]string{"code": "rate_capped"}})
				st.Refused++
			} else {
				st.Shed++
			}
			continue
		}
		box.tokens--
		id := ulid.New()
		data, ino, err := root.ReadFile("requests/"+n, Limits{Uid: h.Cfg.FenceUID, Max: h.Cfg.MaxBytes})
		if err != nil {
			code := classify(err)
			box.seen[n] = ino
			h.ledger(gdir, n, ino, id, "refused:"+code)
			st.Refused++
			st.Reasons[code]++
			h.reply(gdir, Reply{Rid: rid, ID: id, Status: "refused", Result: map[string]string{"code": code}})
			continue
		}
		var head struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(data, &head) != nil || !json.Valid(data) {
			box.seen[n] = ino
			h.ledger(gdir, n, ino, id, "refused:bad_json")
			st.Refused++
			st.Reasons["bad_json"]++
			h.reply(gdir, Reply{Rid: rid, ID: id, Status: "refused", Result: map[string]string{"code": "bad_json"}})
			continue
		}
		if !Types[head.Type] {
			box.seen[n] = ino
			h.ledger(gdir, n, ino, id, "refused:unknown_type")
			st.Refused++
			st.Reasons["unknown_type"]++
			h.reply(gdir, Reply{Rid: rid, ID: id, Status: "refused", Result: map[string]string{"code": "unknown_type"}})
			continue
		}
		req := Request{
			ID: id, Rid: rid, Sid: sid, Gen: genN, Label: rec.Spec.Label, FullID: rec.Reply.ID,
			Type: head.Type, Received: now.UTC(), Body: json.RawMessage(data),
		}
		box.seen[n] = ino
		h.ledger(gdir, n, ino, id, "accepted")
		st.Accepted++
		rep := h.Cfg.Handler(req)
		rep.Rid, rep.ID = rid, id
		h.reply(gdir, rep)
	}
}

// classify maps an error to a stable rule name; it never contains file content.
func classify(err error) string {
	switch err {
	case ErrNotRegular:
		return "not_regular"
	case ErrHardlink:
		return "hardlink"
	case ErrOwner:
		return "wrong_owner"
	case ErrTooBig:
		return "too_big"
	case ErrUnsafePath:
		return "unsafe_path"
	}
	s := err.Error()
	switch {
	case strings.Contains(s, "too many levels of symbolic links"), strings.Contains(s, "cross-device"):
		return "unsafe_path"
	case strings.Contains(s, "no such file"):
		return "gone"
	}
	return "io_error"
}

// reply writes in/replies/<rid>.json (tmp + rename). rid is already validated by nameRe.
func (h *Hostd) reply(gdir string, r Reply) {
	dir := filepath.Join(gdir, "in", "replies")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	b, _ := json.Marshal(r)
	tmp := filepath.Join(dir, "."+r.Rid+".tmp")
	if os.WriteFile(tmp, b, 0o644) == nil {
		_ = os.Rename(tmp, filepath.Join(dir, r.Rid+".json"))
	}
}

// The ledger is the daemon's own record of which outbox files it has processed, so a restart
// does not re-run requests (the daemon cannot delete files from the outbox).
type ledgerRec struct {
	Name   string `json:"name"`
	Ino    uint64 `json:"ino"`
	ID     string `json:"id,omitempty"`
	Result string `json:"result"`
}

func (h *Hostd) ledgerPath(gdir string) string {
	return filepath.Join(h.Cfg.StateDir, "hostd", "ledger", filepath.Base(filepath.Dir(gdir))+"-"+filepath.Base(gdir)+".jsonl")
}

func (h *Hostd) ledger(gdir, name string, ino uint64, id, result string) {
	p := h.ledgerPath(gdir)
	if os.MkdirAll(filepath.Dir(p), 0o700) != nil {
		return
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	b, _ := json.Marshal(ledgerRec{name, ino, id, result})
	f.Write(append(b, '\n'))
}

func (h *Hostd) loadLedger(gdir string, box *outbox) {
	f, err := os.Open(h.ledgerPath(gdir))
	if err != nil {
		return
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var r ledgerRec
		if json.Unmarshal(sc.Bytes(), &r) == nil {
			box.seen[r.Name] = r.Ino
			box.total++
		}
	}
}

// Run scans every interval until stop is closed.
func (h *Hostd) Run(interval time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		h.Scan()
		select {
		case <-stop:
			return
		case <-t.C:
		}
	}
}
