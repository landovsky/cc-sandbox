// relay.go is the sidecar's outbox relay (F3, "outbox relay with U3a's file checks"). It exists because in
// a pod the fence's out/ volume is read by the sidecar, not by a host daemon: the fence writes request
// files, the sidecar reads them with the SAME checks hostd uses (one openat2 that cannot leave the tree,
// then regular file, one link, fence uid, size cap, strict JSON, known type), stamps identity from its
// own arguments (never from the file), and hands the request to a Forward func (the upstream hostd).
// Replies go to in/replies/<rid>.json. The ledger lives in the sidecar's own state volume, because the
// out volume is read-only here and a restarted sidecar must not run a request twice.
package side

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/ulid"
)

// RelayConfig holds every tunable; the sidecar's flags (CCSBX_SIDE_RELAY_*) fill it.
type RelayConfig struct {
	Out, In, State string // out/ (read), in/ (replies written), the sidecar-only ledger dir; absolute
	Sid            string
	Gen            int
	Label, FullID  string
	FenceUID       int     // owner uid every request file must have (10001)
	OwnUID         int     // uid in/replies must belong to (default: this process's uid; settable so a test can lie)
	MaxBytes       int64   // per request file (default 64 KiB)
	Rate           float64 // requests per second (default 5)
	Burst          int     // bucket size (default 20)
	MaxList        int     // names read per scan (default 10000)
	MaxTotal       int     // requests per generation lifetime; beyond that they are shed (default 20000)
	ShedReplies    int     // refusal replies per scan once the bucket is empty (default 20)
	Forward        func(context.Context, hostd.Request) hostd.Reply
	// Journal, when set, gets one record per request the relay counts against MaxTotal (relay.forwarded, or
	// relay.refused with the code, including rate_capped when a refusal reply is sent) and a bounded number of
	// relay.shed summaries (cumulative, with a reserved final one from Close). NOT journalled: requests shed
	// silently (the summary counts them) and ledger_io_error, which retries the same name every scan and so
	// would grow the journal without bound; a broken ledger volume is logged instead.
	// It is observability, not the replay guard (the ledger is), so an append failure is logged (rate-limited)
	// and the relay carries on. Append fsyncs inline: the scan loop waits for the disk once per decision,
	// at most MaxTotal times a lifetime, which is cheap next to the Forward call it sits beside.
	// Size its volume: about 300 bytes a record, so the default 20000 is about 6 MB on top of the ledger's 3 MB.
	Journal     Appender
	MaxShedLogs int // relay.shed summaries per lifetime (default 50): a flood must not grow the journal either
	Now         func() time.Time
	Logf        func(string, ...any)
}

// Appender is the part of journal.Store the relay uses.
type Appender interface {
	Append(journal.Event) (journal.Record, error)
}

// RelayStats counts what one Scan did.
type RelayStats struct {
	Accepted, Refused, Shed int
	Reasons                 map[string]int
}

// Relay reads one generation's outbox.
type Relay struct {
	cfg      RelayConfig
	seen     map[string]bool
	total    int
	shedLogs int
	shedTot  int // sheds over the lifetime, for the summary
	shedSeen int // shedTot at the last summary
	failLog  appendFailures
	tokens   float64
	last     time.Time
}

// NewRelay validates cfg and loads the ledger so a restart does not replay.
func NewRelay(cfg RelayConfig) (*Relay, error) {
	for _, p := range []string{cfg.Out, cfg.In, cfg.State} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return nil, fmt.Errorf("side relay: %q must be a clean absolute path", p)
		}
	}
	if cfg.Forward == nil {
		return nil, errors.New("side relay: no upstream to forward to")
	}
	if cfg.FenceUID <= 0 {
		return nil, errors.New("side relay: FenceUID is required (the uid every request file must have)")
	}
	if cfg.Sid == "" || cfg.Gen < 1 {
		return nil, errors.New("side relay: sid and gen are required (identity comes from here, not from a request file)")
	}
	if cfg.MaxBytes == 0 {
		cfg.MaxBytes = 64 << 10
	}
	if cfg.Rate == 0 {
		cfg.Rate = 5
	}
	if cfg.Burst == 0 {
		cfg.Burst = 20
	}
	if cfg.MaxList == 0 {
		cfg.MaxList = 10000
	}
	if cfg.MaxTotal == 0 {
		cfg.MaxTotal = 20000
	}
	if cfg.ShedReplies == 0 {
		cfg.ShedReplies = 20
	}
	if cfg.MaxShedLogs == 0 {
		cfg.MaxShedLogs = 50
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	// In the pod, in/ is an emptyDir only the sidecar can write (the fence mounts it read-only), so the
	// sidecar makes in/replies itself, and checks that what it finds there is its own plain directory.
	if cfg.OwnUID == 0 {
		cfg.OwnUID = os.Getuid()
	}
	rd := filepath.Join(cfg.In, "replies")
	if err := os.Mkdir(rd, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("side relay: %w", err)
	}
	if fi, err := os.Lstat(rd); err != nil || !fi.IsDir() || !ownedBy(fi, cfg.OwnUID) {
		return nil, fmt.Errorf("side relay: %s is not a plain directory of ours", rd)
	}
	if err := os.Chmod(rd, 0o755); err != nil { // the fence reads it: explicit, whatever the umask made of Mkdir
		return nil, fmt.Errorf("side relay: %w", err)
	}
	r := &Relay{cfg: cfg, seen: map[string]bool{}, tokens: float64(cfg.Burst), last: cfg.Now()}
	if f, err := os.Open(r.ledgerPath()); err == nil {
		defer f.Close()
		sc := bufio.NewScanner(f)
		for sc.Scan() {
			var rec struct{ Name string }
			if json.Unmarshal(sc.Bytes(), &rec) == nil && rec.Name != "" {
				r.seen[rec.Name] = true
				r.total++
			}
		}
	}
	return r, nil
}

// note journals one decision. Identity comes from the config; data holds only what the sidecar decided
// (the rid is nameRe-checked, the type is from the closed list), never a byte of the request body.
func (r *Relay) note(id, typ string, data map[string]any) {
	if r.cfg.Journal == nil {
		return
	}
	b, _ := json.Marshal(data)
	if _, err := r.cfg.Journal.Append(journal.Event{ID: id, Sid: r.cfg.Sid, Gen: r.cfg.Gen, Src: "host", Type: typ, Data: b}); err != nil {
		r.failLog.log(r.cfg.Logf, "side relay", err)
	}
}

func (r *Relay) ledgerPath() string { return filepath.Join(r.cfg.State, "relay-ledger.jsonl") }

// ledger appends one line; "accepted" is fsynced (it is what stops a replay).
func (r *Relay) ledger(name, id, result string) error {
	if err := os.MkdirAll(r.cfg.State, 0o700); err != nil {
		return err
	}
	f, err := os.OpenFile(r.ledgerPath(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	b, _ := json.Marshal(map[string]string{"name": name, "id": id, "result": result})
	if _, err := f.Write(append(b, '\n')); err != nil {
		f.Close()
		return err
	}
	if result == "accepted" {
		if err := f.Sync(); err != nil {
			f.Close()
			return err
		}
	}
	return f.Close()
}

// reply writes in/replies/<rid>.json (tmp + rename). It never creates in/replies: the pod builder does.
func (r *Relay) reply(rep hostd.Reply) {
	dir := filepath.Join(r.cfg.In, "replies")
	if st, err := os.Stat(dir); err != nil || !st.IsDir() {
		r.cfg.Logf("side relay: no inbox %s; reply to %s dropped", dir, rep.Rid)
		return
	}
	b, _ := json.Marshal(rep)
	if err := writePublic(filepath.Join(dir, rep.Rid+".json"), b); err != nil {
		r.cfg.Logf("side relay: reply to %s: %v", rep.Rid, err)
	}
}

func refusal(rid, id, code string) hostd.Reply {
	return hostd.Reply{Rid: rid, ID: id, Status: "refused", Result: map[string]string{"code": code}}
}

// Scan reads the outbox once.
func (r *Relay) Scan(ctx context.Context) RelayStats {
	st := RelayStats{Reasons: map[string]int{}}
	root, err := hostd.OpenRoot(r.cfg.Out)
	if err != nil {
		st.Reasons["out_unreadable"]++
		return st
	}
	defer root.Close()
	names, _, err := root.ListDir("requests", r.cfg.MaxList)
	if err != nil {
		st.Reasons["requests_dir_"+hostd.Classify(err)]++
		return st
	}
	sort.Strings(names)
	now := r.cfg.Now()
	r.tokens += now.Sub(r.last).Seconds() * r.cfg.Rate
	if r.tokens > float64(r.cfg.Burst) {
		r.tokens = float64(r.cfg.Burst)
	}
	r.last = now
	shed := r.cfg.ShedReplies
	for _, n := range names {
		rid, ok := hostd.RequestRid(n)
		if !ok || ctx.Err() != nil {
			continue
		}
		if r.seen[n] {
			continue
		}
		if r.total >= r.cfg.MaxTotal {
			// Past the lifetime cap the name is NOT remembered: the fence picks names, and a set that
			// grew with them would be a way to run the sidecar out of memory.
			st.Shed++
			continue
		}
		r.total++
		id := ulid.New() // minted here; the fence never picks an id
		if r.tokens < 1 {
			r.seen[n] = true
			r.ledger(n, id, "shed")
			if shed > 0 {
				shed--
				r.reply(refusal(rid, id, "rate_capped"))
				r.note(id, "relay.refused", map[string]any{"rid": rid, "code": "rate_capped"})
				st.Refused++
			} else {
				st.Shed++
			}
			continue
		}
		r.tokens--
		data, _, err := root.ReadFile("requests/"+n, hostd.Limits{Uid: r.cfg.FenceUID, Max: r.cfg.MaxBytes})
		if err != nil {
			code := hostd.Classify(err)
			r.seen[n] = true
			r.ledger(n, id, "refused:"+code)
			r.note(id, "relay.refused", map[string]any{"rid": rid, "code": code})
			st.Refused++
			st.Reasons[code]++
			r.reply(refusal(rid, id, code))
			continue
		}
		typ, terr := hostd.StrictType(data)
		if terr != nil {
			r.seen[n] = true
			r.ledger(n, id, "refused:bad_json")
			r.note(id, "relay.refused", map[string]any{"rid": rid, "code": "bad_json"})
			st.Refused++
			st.Reasons["bad_json"]++
			r.reply(refusal(rid, id, "bad_json"))
			continue
		}
		if !hostd.Types[typ] {
			r.seen[n] = true
			r.ledger(n, id, "refused:unknown_type")
			r.note(id, "relay.refused", map[string]any{"rid": rid, "code": "unknown_type"})
			st.Refused++
			st.Reasons["unknown_type"]++
			r.reply(refusal(rid, id, "unknown_type"))
			continue
		}
		if err := r.ledger(n, id, "accepted"); err != nil { // write-ahead: no ledger, no forward
			r.total--
			r.tokens++
			st.Reasons["ledger_io_error"]++
			r.cfg.Logf("side relay: ledger write failed (%v); request not forwarded", err)
			r.reply(hostd.Reply{Rid: rid, ID: id, Status: "error", Result: map[string]string{"code": "io_error"}})
			continue
		}
		r.seen[n] = true
		st.Accepted++
		rep := r.cfg.Forward(ctx, hostd.Request{ID: id, Rid: rid, Sid: r.cfg.Sid, Gen: r.cfg.Gen, Label: r.cfg.Label,
			FullID: r.cfg.FullID, Type: typ, Received: now.UTC(), Body: json.RawMessage(data)})
		rep.Rid, rep.ID = rid, id
		r.reply(rep)
		r.note(id, "relay.forwarded", map[string]any{"rid": rid, "type": typ, "status": rep.Status})
	}
	r.shedTot += st.Shed
	if r.shedTot > r.shedSeen && r.shedLogs < r.cfg.MaxShedLogs-1 { // the last slot is Close's
		r.shedSummary()
	}
	return st
}

func (r *Relay) shedSummary() {
	r.shedLogs++
	r.shedSeen = r.shedTot
	r.note("", "relay.shed", map[string]any{"shed_total": r.shedTot})
}

// Close writes the final shed summary (the slot the periodic ones leave free) if anything was shed since
// the last one, so the journal's last word holds the lifetime total. Call it through Run (which does) or after
// the last Scan returned: it is NOT safe to call concurrently with Scan (they share the counters unlocked).
func (r *Relay) Close() {
	if r.shedTot > r.shedSeen && r.shedLogs < r.cfg.MaxShedLogs {
		r.shedSummary()
	}
}

// Run scans every `every` until ctx ends.
func (r *Relay) Run(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		r.Scan(ctx)
		select {
		case <-ctx.Done():
			r.Close()
			return
		case <-t.C:
		}
	}
}
