package journal

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func open(t *testing.T, dir string) *Store {
	t.Helper()
	s, err := Open(dir, "hp-ubuntu", 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func ev(typ string) Event {
	return Event{Sid: "s1", Gen: 1, Node: "cc-sandbox:x", Realm: "production", Src: "host", Type: typ, Data: json.RawMessage(`{"k":1}`)}
}

func seqs(rs []Record) []int64 {
	var o []int64
	for _, r := range rs {
		o = append(o, r.Seq)
	}
	return o
}

// ---- T:journal-durable ---------------------------------------------------------------------

// context: the cockpit is down for hours; the daemon keeps the journal. Every acknowledged
// event must still be there after the daemon restarts.
func TestJournalDurableAllow(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	var ids []string
	for i := 0; i < 50; i++ {
		r, err := s.Append(ev("hook.Stop"))
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, r.ID)
	}
	s.Close()
	s2 := open(t, dir)
	got, err := s2.After(0, s2.Epoch(), "", 0)
	if err != nil || len(got) != 50 || s2.Head() != 50 {
		t.Fatalf("got %d head %d err %v", len(got), s2.Head(), err)
	}
	for i, r := range got {
		if r.Seq != int64(i+1) || r.ID != ids[i] {
			t.Fatalf("record %d: %+v", i, r)
		}
	}
}

// context: the daemon is killed mid-write, leaving half a line. After restart the half line is
// gone, the next seq follows the last COMPLETE record, and there is no gap and no duplicate.
func TestJournalDurableBlock(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	for i := 0; i < 5; i++ {
		s.Append(ev("a"))
	}
	s.Close()
	seg, _ := filepath.Glob(filepath.Join(dir, "seg-*.jsonl"))
	f, _ := os.OpenFile(seg[0], os.O_APPEND|os.O_WRONLY, 0)
	f.WriteString(`{"v":1,"seq":6,"id":"01TORN`)
	f.Close()
	s2 := open(t, dir)
	r, err := s2.Append(ev("b"))
	if err != nil || r.Seq != 6 {
		t.Fatalf("next seq %d err %v", r.Seq, err)
	}
	got, _ := s2.After(0, "", "", 0)
	if len(got) != 6 || got[5].Type != "b" {
		t.Fatalf("%v", seqs(got))
	}
	for i, g := range got {
		if g.Seq != int64(i+1) {
			t.Fatalf("gap: %v", seqs(got))
		}
	}
}

// ---- replay: exactly the missed events -----------------------------------------------------

// context: the consumer applied up to seq 3 and stopped; a session ran to done meanwhile. A
// replay from its cursor returns exactly what it missed, in order, once.
func TestReplayAfterCursorReturnsExactlyTheMissedEvents(t *testing.T) {
	s := open(t, t.TempDir())
	for _, ty := range []string{"session.started", "hook.UserPromptSubmit", "hook.Stop"} {
		s.Append(ev(ty))
	}
	last := s.Head()
	for _, ty := range []string{"hook.PreToolUse", "hook.PostToolUse", "hook.Stop", "session.ended"} {
		s.Append(ev(ty))
	}
	got, err := s.After(last, s.Epoch(), "", 0)
	if err != nil || len(got) != 4 || got[0].Type != "hook.PreToolUse" || got[3].Type != "session.ended" {
		t.Fatalf("%v %+v", err, got)
	}
	again, _ := s.After(s.Head(), s.Epoch(), "", 0)
	if len(again) != 0 {
		t.Fatalf("caught-up consumer got %d", len(again))
	}
}

// context: the e2e realm's events must not leak into the production consumer's replay.
func TestReplayFiltersByRealm(t *testing.T) {
	s := open(t, t.TempDir())
	e := ev("x")
	s.Append(e)
	e.Realm = "e2e"
	s.Append(e)
	got, _ := s.After(0, "", "e2e", 0)
	if len(got) != 1 || got[0].Realm != "e2e" {
		t.Fatalf("%+v", got)
	}
}

// ---- epoch ---------------------------------------------------------------------------------

// context: the journal directory was wiped; the daemon starts a new epoch. A consumer holding
// the old epoch must be told, not handed a different history that happens to share seq numbers.
func TestOldEpochConsumerIsToldToStartOver(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	old := s.Epoch()
	s.Append(ev("a"))
	s.Close()
	os.RemoveAll(dir)
	s2 := open(t, dir)
	if s2.Epoch() == old {
		t.Fatal("epoch did not change")
	}
	if _, err := s2.After(1, old, "", 0); err != ErrEpoch {
		t.Fatalf("err %v", err)
	}
}

// ---- retention -----------------------------------------------------------------------------

// context: retention must never be configured below 14 days — a cockpit can be off for a
// holiday week and still has to replay.
func TestRetentionFloorIsFourteenDays(t *testing.T) {
	if _, err := Open(t.TempDir(), "m", 3); err == nil || !strings.Contains(err.Error(), "14") {
		t.Fatalf("err %v", err)
	}
}

func TestPruneKeepsFourteenDaysAndSeqStaysGapless(t *testing.T) {
	dir := t.TempDir()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	s := open(t, dir)
	s.Now = func() time.Time { return now.AddDate(0, 0, -20) }
	s.Append(ev("old"))
	s.Now = func() time.Time { return now.AddDate(0, 0, -10) }
	s.Append(ev("recent"))
	s.Now = func() time.Time { return now }
	s.Append(ev("today"))
	n, err := s.Prune()
	if err != nil || n != 1 {
		t.Fatalf("pruned %d err %v", n, err)
	}
	if _, err := s.After(0, "", "", 0); err != ErrPruned {
		t.Fatalf("a cursor before the retained range must be told: %v", err)
	}
	got, err := s.After(1, "", "", 0)
	if err != nil || len(got) != 2 || got[0].Seq != 2 {
		t.Fatalf("%v %v", err, seqs(got))
	}
	s.Close()
	s2 := open(t, dir)
	r, _ := s2.Append(ev("next"))
	if r.Seq != 4 {
		t.Fatalf("seq after prune+restart %d", r.Seq)
	}
}

// ---- cursors -------------------------------------------------------------------------------

const sha1_ = "a1b2c3d4e5f60718293a4b5c6d7e8f9012345678"
const sha2_ = "b1b2c3d4e5f60718293a4b5c6d7e8f9012345679"

// context: a write cursor advances with a tree commit SHA (the one its events produced, or the
// HEAD when they changed nothing); the cockpit that takes over reads that SHA and checks it is an
// ancestor of its HEAD before pulling.
func TestCursorAdvancesOnlyWithItsTreeCommit(t *testing.T) {
	s := open(t, t.TempDir())
	for i := 0; i < 5; i++ {
		s.Append(ev("x"))
	}
	if _, err := s.Advance("production", 2, s.Epoch(), ""); err != ErrCursorNoSHA {
		t.Fatalf("no sha: %v", err)
	}
	if _, err := s.Advance("production", 2, s.Epoch(), "not-a-sha"); err != ErrCursorNoSHA {
		t.Fatalf("bad sha: %v", err)
	}
	if c, err := s.Advance("production", 2, s.Epoch(), sha1_); err != nil || c.Seq != 2 {
		t.Fatalf("%v %+v", err, c)
	}
	if _, err := s.Advance("production", 3, s.Epoch(), sha1_); err != nil {
		t.Fatalf("events that changed nothing keep the tree's HEAD: %v", err)
	}
	if _, err := s.Advance("tab-hp", 3, s.Epoch(), ""); err != nil {
		t.Fatalf("a read cursor needs no tree commit: %v", err)
	}
	if _, err := s.Advance("production", 4, s.Epoch(), sha2_); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Advance("production", 3, s.Epoch(), sha2_); err != ErrCursorBack {
		t.Fatalf("back: %v", err)
	}
	if _, err := s.Advance("production", 99, s.Epoch(), sha1_); err != ErrBadCursor {
		t.Fatalf("past head: %v", err)
	}
	if _, err := s.Advance("production", 5, "eOTHER", sha1_); err != ErrEpoch {
		t.Fatalf("epoch: %v", err)
	}
}

// context: the e2e realm's cockpit moves its own cursor; production's must not budge.
func TestRealmsHaveIndependentCursors(t *testing.T) {
	s := open(t, t.TempDir())
	s.Append(ev("x"))
	s.Append(ev("x"))
	s.Advance("production", 1, s.Epoch(), sha1_)
	s.Advance("e2e", 2, s.Epoch(), sha2_)
	p, _, _ := s.Cursor("production")
	e, _, _ := s.Cursor("e2e")
	if p.Seq != 1 || e.Seq != 2 || p.TreeSHA == e.TreeSHA {
		t.Fatalf("%+v %+v", p, e)
	}
	if _, err := s.Advance("../etc", 1, s.Epoch(), sha1_); err != ErrUnknownRealm {
		t.Fatalf("path-like cursor name: %v", err)
	}
}

// context: the hostd ingests several outboxes at once; seq stays unique and gapless.
func TestConcurrentAppendsStayGapless(t *testing.T) {
	s := open(t, t.TempDir())
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 25; i++ {
				s.Append(ev("c"))
			}
		}()
	}
	wg.Wait()
	got, _ := s.After(0, "", "", 0)
	if len(got) != 200 {
		t.Fatalf("%d", len(got))
	}
	for i, r := range got {
		if r.Seq != int64(i+1) {
			t.Fatalf("gap at %d: %d", i, r.Seq)
		}
	}
}

// ---- idle: when may a message be typed into a pane? --------------------------------------------

func hook(typ string) Event {
	return Event{Sid: "s1", Gen: 1, Src: "fence", Type: typ, Data: json.RawMessage(`{}`)}
}

// context: Claude Code can sit in a menu (a permission prompt, a question, a plan approval).
// Only a finished turn (hook.Stop as the last word) means "at the input prompt".
func TestIdleOnlyAfterStopAndNotWhileATurnRuns(t *testing.T) {
	s := open(t, t.TempDir())
	if _, ok := s.IdleSince("s1"); ok {
		t.Fatal("a session we have heard nothing from is not idle (it may be at a trust dialog)")
	}
	s.Append(hook("hook.UserPromptSubmit"))
	if _, ok := s.IdleSince("s1"); ok {
		t.Fatal("mid-turn is not idle")
	}
	s.Append(hook("hook.Notification")) // "needs your permission": a menu is up
	if _, ok := s.IdleSince("s1"); ok {
		t.Fatal("a permission notification is not idle")
	}
	s.Append(hook("hook.Stop"))
	if _, ok := s.IdleSince("s1"); !ok {
		t.Fatal("after Stop it is idle")
	}
	s.Append(hook("hook.UserPromptSubmit"))
	if _, ok := s.IdleSince("s1"); ok {
		t.Fatal("a new prompt makes it busy again")
	}
}

// context: the daemon restarts while a worker is idle; idle must survive, and a host-sourced
// event must never count (only the fence's own hooks say what its pane shows).
func TestIdleSurvivesRestartAndIgnoresHostEvents(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Append(hook("hook.Stop"))
	s.Append(Event{Sid: "s1", Src: "host", Type: "session.ended"})
	s.Close()
	s2 := open(t, dir)
	if _, ok := s2.IdleSince("s1"); !ok {
		t.Fatal("idle lost on restart")
	}
	s2.Append(Event{Sid: "s2", Src: "host", Type: "hook.Stop"})
	if _, ok := s2.IdleSince("s2"); ok {
		t.Fatal("a host event was taken for the fence's word")
	}
}
