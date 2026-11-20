// Tests of the events.jsonl ingest. Why: the fence writes every byte of that file, so each case is
// an abuse (forged identity, oversize lines, a flood, a shrunk file, a dead generation) or a restart
// that must neither lose nor repeat an event.

package hostd

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/start"
)

func (f *fixture) withJournal(dir string) *journal.Store {
	f.t.Helper()
	j, err := journal.Open(dir, "hp-ubuntu", 0)
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { j.Close() })
	f.h.Cfg.Journal = j
	return j
}

func (f *fixture) appendEvents(lines ...string) {
	p := filepath.Join(f.g, "out", "events.jsonl")
	fh, err := os.OpenFile(p, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		f.t.Fatal(err)
	}
	defer fh.Close()
	for _, l := range lines {
		fh.WriteString(l)
	}
}

func types(rs []journal.Record) []string {
	var o []string
	for _, r := range rs {
		o = append(o, r.Src+":"+r.Type)
	}
	return o
}

// context: hook records come from the fence, so the daemon takes only a hook.<Name> type and a
// data object from each line; sid, node and realm come from the daemon, never from the line.
func TestHookEventsAreJournaledWithDaemonIdentity(t *testing.T) {
	f := newFixture(t, nil)
	j := f.withJournal(t.TempDir())
	f.appendEvents(
		`{"type":"hook.Stop","data":{"n":1},"sid":"s-forged","node":"cc-sandbox:boss","src":"host"}`+"\n",
		`{"type":"session.ended","data":{}}`+"\n", // a fence cannot journal a host event type
		`not json`+"\n",
		`{"type":"hook.PostToolUse","data":{"tool":"Bash"}}`+"\n",
		`{"type":"hook.Notif`, // torn: waits for the rest
	)
	st := f.h.Scan()
	got, _ := j.After(0, "", "", 0)
	if strings.Join(types(got), ",") != "host:session.started,fence:hook.Stop,fence:hook.PostToolUse" {
		t.Fatalf("%v", types(got))
	}
	if got[1].Sid != sid || got[1].Node != "cc-sandbox:abcd/worker" || got[1].Src != "fence" || got[1].Seq != 2 {
		t.Fatalf("identity from the line leaked: %+v", got[1])
	}
	if st.Reasons["events_bad_line"] != 2 {
		t.Fatalf("%v", st.Reasons)
	}
	// The rest of the torn line arrives; only now is it a record.
	f.appendEvents(`ication","data":{}}` + "\n")
	f.h.Scan()
	got, _ = j.After(0, "", "", 0)
	if len(got) != 4 || got[3].Type != "hook.Notification" {
		t.Fatalf("%v", types(got))
	}
}

// context: the daemon restarts between scans; it must not journal the same lines again.
func TestRestartDoesNotRejournalEvents(t *testing.T) {
	f := newFixture(t, nil)
	j := f.withJournal(t.TempDir())
	f.appendEvents(`{"type":"hook.Stop","data":{}}` + "\n")
	f.h.Scan()
	h2 := New(f.h.Cfg)
	h2.Scan()
	if got, _ := j.After(0, "", "", 0); len(got) != 2 { // session.start + 1 hook
		t.Fatalf("%v", types(got))
	}
}

// context: the cockpit reads session.end to know the generation is over; it is journaled after
// the final drain, and only once.
func TestSessionEndIsJournaledAfterTheLastEventsAndOnce(t *testing.T) {
	f := newFixture(t, nil)
	j := f.withJournal(t.TempDir())
	f.appendEvents(`{"type":"hook.Stop","data":{}}` + "\n")
	os.WriteFile(filepath.Join(f.g, "CLOSED"), []byte("closed\n"), 0o600)
	f.h.Scan()
	f.h.Scan()
	got, _ := j.After(0, "", "", 0)
	if strings.Join(types(got), ",") != "host:session.started,fence:hook.Stop,host:session.ended" {
		t.Fatalf("%v", types(got))
	}
}

// context: a line past the cap (a fence trying to bloat the journal) is dropped, not truncated.
func TestOverlongEventLineIsDropped(t *testing.T) {
	f := newFixture(t, nil)
	j := f.withJournal(t.TempDir())
	f.appendEvents(`{"type":"hook.Stop","data":{"x":"`+strings.Repeat("a", 5000)+`"}}`+"\n", `{"type":"hook.Stop","data":{}}`+"\n")
	st := f.h.Scan()
	got, _ := j.After(0, "", "", 0)
	if len(got) != 2 || st.Reasons["events_line_too_long"] != 1 {
		t.Fatalf("%v %v", types(got), st.Reasons)
	}
}

// context: events.jsonl is append-only; a fence that replaces it with something shorter must
// not make the daemon re-read old bytes or crash.
func TestShrunkEventsFileIsNotReplayed(t *testing.T) {
	f := newFixture(t, nil)
	j := f.withJournal(t.TempDir())
	f.appendEvents(`{"type":"hook.Stop","data":{}}`+"\n", `{"type":"hook.Stop","data":{}}`+"\n")
	f.h.Scan()
	os.WriteFile(filepath.Join(f.g, "out", "events.jsonl"), []byte(`{"type":"hook.Stop","data":{}}`+"\n"), 0o644)
	st := f.h.Scan()
	got, _ := j.After(0, "", "", 0)
	if len(got) != 3 || st.Reasons["events_truncated"] != 1 {
		t.Fatalf("%v %v", types(got), st.Reasons)
	}
}

// ---- T:epoch-reset-reingest ----------------------------------------------------------------

// context: the daemon restarts with the same journal: nothing is re-ingested.
func TestEpochResetReingestAllow(t *testing.T) {
	f := newFixture(t, nil)
	jd := t.TempDir()
	j := f.withJournal(jd)
	f.req("r1.json", `{"type":"ccf.done"}`)
	f.appendEvents(`{"type":"hook.Stop","data":{}}` + "\n")
	f.h.Scan()
	j.Close()
	f.withJournal(jd)
	h2 := New(f.h.Cfg)
	h2.Scan()
	if len(f.got) != 1 {
		t.Fatalf("request replayed: %d", len(f.got))
	}
	if got, _ := j.After(0, "", "", 0); len(got) != 2 {
		t.Fatalf("%v", types(got))
	}
}

// context: the journal is lost (disk swap, wiped state dir) and a new epoch starts. The
// outboxes still hold the truth: events are re-read from the start, the lifecycle is
// re-emitted, a request that never got a reply is ingested again, and one that was answered is
// not run twice.
func TestEpochResetReingestBlock(t *testing.T) {
	f := newFixture(t, nil)
	jd := t.TempDir()
	j := f.withJournal(jd)
	f.req("answered.json", `{"type":"ccf.done"}`)
	f.req("lost.json", `{"type":"ccf.mark"}`)
	f.appendEvents(`{"type":"hook.Stop","data":{}}`+"\n", `{"type":"hook.PostToolUse","data":{}}`+"\n")
	f.h.Scan()
	if len(f.got) != 2 {
		t.Fatalf("setup: %d", len(f.got))
	}
	oldEpoch := j.Epoch()
	// The daemon died after ingesting "lost" but before its reply reached the fence.
	os.Remove(filepath.Join(f.g, "in", "replies", "lost.json"))
	j.Close()
	os.RemoveAll(jd)
	f.got = nil
	j2 := f.withJournal(jd)
	if j2.Epoch() == oldEpoch {
		t.Fatal("epoch did not change")
	}
	New(f.h.Cfg).Scan()
	if len(f.got) != 1 || f.got[0].Rid != "lost" {
		t.Fatalf("re-ingested %+v", f.got)
	}
	got, _ := j2.After(0, "", "", 0)
	if strings.Join(types(got), ",") != "host:session.started,fence:hook.Stop,fence:hook.PostToolUse" {
		t.Fatalf("journal not rebuilt from the outbox: %v", types(got))
	}
	if _, ok := f.reply("lost"); !ok {
		t.Fatal("no reply for the re-ingested request")
	}
	_ = json.Marshal
}

// ---- review fixes (U3b-N2, N3) ---------------------------------------------------------------

// context: an old generation that is still running keeps writing hook events; they must not
// show up under the node as if the live generation said them.
func TestStaleGenerationsEventsAreNotJournaled(t *testing.T) {
	f := newFixture(t, nil)
	j := f.withJournal(t.TempDir())
	f.setRecord(func(r *start.Record) { r.Gen = 2; r.Reply.ID = "tomas/hp/" + sid + "/g2" })
	f.appendEvents(`{"type":"hook.Stop","data":{}}` + "\n")
	f.h.Scan()
	for _, e := range func() []journal.Record { r, _ := j.After(0, "", "", 0); return r }() {
		if e.Src == "fence" {
			t.Fatalf("a dead generation's hook event was journaled: %+v", e)
		}
	}
}

// context: after a journal reset a request that had no reply is handled again; it must keep the
// id it had, so whatever it reached downstream can dedupe the repeat.
func TestReingestedRequestKeepsItsFirstUlid(t *testing.T) {
	f := newFixture(t, nil)
	jd := t.TempDir()
	j := f.withJournal(jd)
	f.req("lost.json", `{"type":"msg.send"}`)
	f.h.Scan()
	first := f.got[0].ID
	os.Remove(filepath.Join(f.g, "in", "replies", "lost.json"))
	j.Close()
	os.RemoveAll(jd)
	f.got = nil
	f.withJournal(jd)
	New(f.h.Cfg).Scan()
	if len(f.got) != 1 || f.got[0].ID != first {
		t.Fatalf("first %s again %+v", first, f.got)
	}
}

// ---- review of PR #4: budget for fence events, and the last words of an ended generation ----

// context: a fence appends tiny hook lines in a loop. Every journaled event is an fsync and 14 days
// of disk, so one outbox gets a budget; the rest is dropped and counted, never queued.
func TestFenceEventsHaveABudgetBlock(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.EventBurst = 10; c.EventRate = 1; c.MaxEventsPerScan = 10000 })
	j := f.withJournal(t.TempDir())
	var lines []string
	for i := 0; i < 200; i++ {
		lines = append(lines, `{"type":"hook.PreToolUse","data":{}}`+"\n")
	}
	f.appendEvents(lines...)
	st := f.h.Scan()
	recs, _ := j.After(0, "", "", 0)
	fence := 0
	for _, r := range recs {
		if r.Src == "fence" {
			fence++
		}
	}
	if fence > 12 || st.Reasons["events_shed"] < 150 {
		t.Fatalf("journaled %d of 200 fence events, shed %d", fence, st.Reasons["events_shed"])
	}
}

// context: the allow side: a busy but ordinary session (a few events per second) loses nothing.
func TestFenceEventsWithinTheBudgetAreAllJournaledAllow(t *testing.T) {
	f := newFixture(t, nil)
	j := f.withJournal(t.TempDir())
	var lines []string
	for i := 0; i < 40; i++ {
		lines = append(lines, `{"type":"hook.PostToolUse","data":{}}`+"\n")
	}
	f.appendEvents(lines...)
	f.h.Scan()
	recs, _ := j.After(0, "", "", 0)
	fence := 0
	for _, r := range recs {
		if r.Src == "fence" {
			fence++
		}
	}
	if fence != 40 {
		t.Fatalf("journaled %d of 40", fence)
	}
}

// context: hostd was down while the session finished (its record got Ended). The last result and the
// final hook events are real progress (invariant 28); only requests that act with live authority
// (a message, a git write, a grant) are refused as stale.
func TestEndedGenerationsLastWordsAreKeptButItCannotActAllow(t *testing.T) {
	f := newFixture(t, nil)
	j := f.withJournal(t.TempDir())
	f.setRecord(func(r *start.Record) { now := time.Now(); r.Ended = &now })
	f.req("done.json", `{"type":"ccf.done"}`)
	f.req("msg.json", `{"type":"msg.send"}`)
	f.appendEvents(`{"type":"hook.Stop","data":{}}` + "\n")
	f.h.Scan()
	if len(f.got) != 1 || f.got[0].Type != "ccf.done" {
		t.Fatalf("the final done was not handled: %+v", f.got)
	}
	if r, _ := f.reply("msg"); r.Status != "stale" {
		t.Fatalf("an ended generation sent a message: %+v", r)
	}
	if got := types(func() []journal.Record { r, _ := j.After(0, "", "", 0); return r }()); !contains(got, "fence:hook.Stop") {
		t.Fatalf("final events lost: %v", got)
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
