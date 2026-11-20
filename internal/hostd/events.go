// Ingest of a fence's out/events.jsonl into the journal (M3). It exists in hostd, not the journal,
// because every line is untrusted: only a hook.<Name> type and a data object are kept, identity comes
// from the daemon's record, the volume has a budget, and the offset is saved so a restart does not
// journal a line twice.

package hostd

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/start"
)

// Journal is where the daemon records events (M3). *journal.Store implements it.
type Journal interface {
	Append(journal.Event) (journal.Record, error)
	Epoch() string
}

// genState is what the daemon remembers per generation besides the request ledger.
type genState struct {
	Off     int64 `json:"events_off"` // bytes of events.jsonl already journaled
	Started bool  `json:"started"`
	Ended   bool  `json:"ended"`
}

func (h *Hostd) statePath(gdir string) string {
	return filepath.Join(h.Cfg.StateDir, "hostd", "state", filepath.Base(filepath.Dir(gdir))+"-"+filepath.Base(gdir)+".json")
}

func (h *Hostd) loadState(gdir string, box *outbox) {
	if box.stateOK {
		return
	}
	box.stateOK = true
	if b, err := os.ReadFile(h.statePath(gdir)); err == nil {
		_ = json.Unmarshal(b, &box.state)
	}
}

func (h *Hostd) saveState(gdir string, box *outbox) {
	p := h.statePath(gdir)
	b, _ := json.Marshal(box.state)
	tmp := p + ".tmp"
	err := os.MkdirAll(filepath.Dir(p), 0o700)
	if err == nil {
		if err = os.WriteFile(tmp, b, 0o600); err == nil {
			err = os.Rename(tmp, p)
		}
	}
	if err != nil { // never silent: events may be journaled twice after a restart
		h.Cfg.Logf("hostd: state write failed (%v): events may be journaled again after a restart", err)
	}
}

// lifecycle journals session.start / session.end once per generation (src: host).
func (h *Hostd) lifecycle(gdir string, rec *start.Record, box *outbox, typ string) {
	if h.Cfg.Journal == nil {
		return
	}
	done := &box.state.Started
	if typ == "session.ended" {
		done = &box.state.Ended
	}
	if *done {
		return
	}
	var gen int
	fmt.Sscanf(filepath.Base(gdir), "g%d", &gen)
	// session.started carries what cc-tree's verb writer needs to learn the sid -> node map
	// (sid, node, role, gen, workdir, backend); the daemon fills all of it, the fence none.
	var cl struct {
		Role string `json:"role"`
	}
	_ = json.Unmarshal(rec.Spec.Client, &cl)
	if cl.Role == "" {
		cl.Role = "worker"
	}
	data, _ := json.Marshal(map[string]any{"full_id": rec.Reply.ID, "node": rec.Spec.Label, "role": cl.Role, "gen": gen,
		"workdir": rec.Spec.Workdir.Path, "backend": rec.Reply.Backend, "machine": rec.Spec.Machine})
	if _, err := h.Cfg.Journal.Append(journal.Event{Sid: rec.Sid, Gen: gen, Node: rec.Spec.Label, Realm: rec.Spec.Realm, Src: "host", Type: typ, Data: data}); err != nil {
		h.Cfg.Logf("hostd: journal append failed: %v", err)
		return
	}
	*done = true
	h.saveState(gdir, box)
}

var hookTypeRe = regexp.MustCompile(`^hook\.[A-Za-z][A-Za-z0-9]{0,39}$`)

// ingestEvents journals new complete lines of out/events.jsonl as src:fence events. Each line
// is untrusted: size-capped, it must be a JSON object whose type is "hook.<Name>", and only
// that type and its data object are kept. Identity (sid, gen, node, realm) comes from the
// daemon. Delivery is at-least-once: the offset is saved after the append, so a crash between
// the two journals that one batch again.
func (h *Hostd) ingestEvents(gdir string, rec *start.Record, gen string, box *outbox, st *Stats) {
	if h.Cfg.Journal == nil {
		return
	}
	root, err := OpenRoot(filepath.Join(gdir, "out"))
	if err != nil {
		return
	}
	defer root.Close()
	chunk, size, err := root.ReadRange("events.jsonl", box.state.Off, 256<<10, Limits{Uid: h.Cfg.FenceUID})
	if err != nil {
		if !os.IsNotExist(err) {
			st.Reasons["events_"+classify(err)]++
		}
		return
	}
	if size < box.state.Off {
		// The fence shrank an append-only file. Never go backwards; resume at the new end.
		st.Reasons["events_truncated"]++
		box.state.Off = size
		h.saveState(gdir, box)
		return
	}
	var genN int
	fmt.Sscanf(gen, "g%d", &genN)
	taken, consumed := 0, 0
	now := h.Cfg.Now()
	if box.elast.IsZero() {
		box.etokens, box.elast = float64(h.Cfg.EventBurst), now
	}
	box.etokens += now.Sub(box.elast).Seconds() * h.Cfg.EventRate
	if box.etokens > float64(h.Cfg.EventBurst) {
		box.etokens = float64(h.Cfg.EventBurst)
	}
	box.elast = now
	for consumed < len(chunk) && taken < h.Cfg.MaxEventsPerScan {
		nl := bytes.IndexByte(chunk[consumed:], '\n')
		if nl < 0 {
			if len(chunk) >= 256<<10 { // an unterminated line bigger than the chunk: skip it
				consumed = len(chunk)
				st.Reasons["events_line_too_long"]++
			}
			break // a partial last line: wait for the rest
		}
		line := chunk[consumed : consumed+nl]
		consumed += nl + 1
		if len(line) > h.Cfg.MaxEventLine {
			st.Reasons["events_line_too_long"]++
			continue
		}
		typ, data, ok := parseEvent(line)
		if !ok {
			st.Reasons["events_bad_line"]++
			continue
		}
		if box.etokens < 1 {
			// Over budget: the line is consumed (so the fence can't make a backlog) and counted.
			st.Reasons["events_shed"]++
			continue
		}
		box.etokens--
		jr, err := h.Cfg.Journal.Append(journal.Event{Sid: rec.Sid, Gen: genN, Node: rec.Spec.Label, Realm: rec.Spec.Realm, Src: "fence", Type: typ, Data: data})
		if err != nil {
			h.Cfg.Logf("hostd: journal append failed: %v", err)
			break
		}
		if h.Cfg.OnEvent != nil {
			h.Cfg.OnEvent(jr)
		}
		taken++
	}
	if consumed > 0 {
		box.state.Off += int64(consumed)
		h.saveState(gdir, box)
	}
}

func parseEvent(line []byte) (typ string, data json.RawMessage, ok bool) {
	var e struct {
		Type string          `json:"type"`
		Data json.RawMessage `json:"data"`
	}
	if json.Unmarshal(line, &e) != nil || !hookTypeRe.MatchString(e.Type) {
		return "", nil, false
	}
	d := bytes.TrimSpace(e.Data)
	if len(d) == 0 || d[0] != '{' || !json.Valid(d) {
		d = json.RawMessage(`{}`)
	}
	return e.Type, d, true
}

// checkEpoch runs once, at the first scan. If the journal's epoch differs from the one this
// hostd last ingested under (the journal was wiped or replaced), everything the journal lost
// must be rebuilt from the outboxes: events.jsonl is re-read from 0, lifecycle events are
// re-emitted, and requests that never got a reply are forgotten so they are ingested again.
// Requests that were answered stay answered (T:epoch-reset-reingest).
func (h *Hostd) checkEpoch(gdirs []string) {
	if h.epochDone || h.Cfg.Journal == nil {
		return
	}
	h.epochDone = true
	ep := filepath.Join(h.Cfg.StateDir, "hostd", "epoch")
	cur := h.Cfg.Journal.Epoch()
	if b, err := os.ReadFile(ep); err == nil && strings.TrimSpace(string(b)) == cur {
		return
	} else if err == nil {
		h.Cfg.Logf("hostd: journal epoch changed (%s -> %s): re-ingesting outboxes", strings.TrimSpace(string(b)), cur)
		for _, g := range gdirs {
			h.forgetUnreplied(g)
			_ = os.Remove(h.statePath(g)) // events offset 0, lifecycle re-emitted
		}
	}
	_ = os.MkdirAll(filepath.Dir(ep), 0o700)
	_ = os.WriteFile(ep, []byte(cur+"\n"), 0o600)
}

// forgetUnreplied rewrites the generation's ledger without the entries that have no reply.
func (h *Hostd) forgetUnreplied(gdir string) {
	p := h.ledgerPath(gdir)
	b, err := os.ReadFile(p)
	if err != nil {
		return
	}
	var keep [][]byte
	for _, ln := range bytes.Split(b, []byte{'\n'}) {
		if len(ln) == 0 {
			continue
		}
		var r ledgerRec
		if json.Unmarshal(ln, &r) != nil {
			continue
		}
		rid := strings.TrimSuffix(r.Name, ".json")
		if _, err := os.Stat(filepath.Join(gdir, "in", "replies", rid+".json")); err == nil {
			keep = append(keep, ln)
		} else if r.ID != "" {
			h.reuse[gdir+"/"+r.Name] = r.ID // handled again, but under the id it had: downstream can dedupe
		}
	}
	out := bytes.Join(keep, []byte{'\n'})
	if len(out) > 0 {
		out = append(out, '\n')
	}
	_ = os.WriteFile(p, out, 0o600)
}
