package journal

import (
	"encoding/json"
	"errors"
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
	if _, ok := s.IdleSince("s1", 1); ok {
		t.Fatal("a session we have heard nothing from is not idle (it may be at a trust dialog)")
	}
	s.Append(hook("hook.UserPromptSubmit"))
	if _, ok := s.IdleSince("s1", 1); ok {
		t.Fatal("mid-turn is not idle")
	}
	s.Append(hook("hook.Notification")) // "needs your permission": a menu is up
	if _, ok := s.IdleSince("s1", 1); ok {
		t.Fatal("a permission notification is not idle")
	}
	s.Append(hook("hook.Stop"))
	if _, ok := s.IdleSince("s1", 1); !ok {
		t.Fatal("after Stop it is idle")
	}
	s.Append(hook("hook.UserPromptSubmit"))
	if _, ok := s.IdleSince("s1", 1); ok {
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
	if _, ok := s2.IdleSince("s1", 1); !ok {
		t.Fatal("idle lost on restart")
	}
	s2.Append(Event{Sid: "s2", Src: "host", Type: "hook.Stop"})
	if _, ok := s2.IdleSince("s2", 1); ok {
		t.Fatal("a host event was taken for the fence's word")
	}
}

// ---- T:idle-per-generation, T:idle-notification-type ------------------------------------------

func hookG(typ string, gen int, data string) Event {
	return Event{Sid: "s1", Gen: gen, Src: "fence", Type: typ, Data: json.RawMessage(data)}
}

// context: the session was resumed as generation 2. Its new Claude Code starts at its own
// startup dialogs (trust folder, resume picker); generation 1's last Stop says nothing about
// that pane, and an Enter typed into it would pick the dialog's default.
func TestIdleIsPerGenerationAllow(t *testing.T) {
	s := open(t, t.TempDir())
	s.Append(hookG("hook.Stop", 1, `{}`))
	if _, ok := s.IdleSince("s1", 1); !ok {
		t.Fatal("generation 1 is idle after its Stop")
	}
	s.Append(hookG("hook.Stop", 2, `{}`))
	if _, ok := s.IdleSince("s1", 2); !ok {
		t.Fatal("generation 2 is idle after its own Stop")
	}
}

func TestIdleIsPerGenerationBlock(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Append(hookG("hook.Stop", 1, `{}`))
	s.Append(Event{Sid: "s1", Gen: 2, Src: "host", Type: "session.started", Data: json.RawMessage(`{}`)})
	if _, ok := s.IdleSince("s1", 2); ok {
		t.Fatal("a new generation inherited the old one's idle")
	}
	s.Append(hookG("hook.SessionStart", 2, `{}`))
	if _, ok := s.IdleSince("s1", 2); ok {
		t.Fatal("SessionStart is not idle")
	}
	s.Close()
	s2 := open(t, dir)
	if _, ok := s2.IdleSince("s1", 2); ok {
		t.Fatal("restart mixed the generations up")
	}
	if _, ok := s2.IdleSince("s1", 1); !ok {
		t.Fatal("generation 1's own idle lost on restart")
	}
}

// context: Claude Code fires Notification for several reasons. "Waiting for input" after a Stop
// (idle_prompt) leaves the pane at its prompt; a permission prompt or any other kind means a
// menu is up. An event that doesn't say which is read as the worse one.
func TestIdleNotificationTypeAllow(t *testing.T) {
	s := open(t, t.TempDir())
	s.Append(hookG("hook.Stop", 1, `{}`))
	s.Append(hookG("hook.Notification", 1, `{"notification_type":"idle_prompt","message":"waiting"}`))
	if _, ok := s.IdleSince("s1", 1); !ok {
		t.Fatal("idle_prompt after Stop must stay idle")
	}
}

func TestIdleNotificationTypeBlock(t *testing.T) {
	for _, data := range []string{`{"notification_type":"permission_prompt"}`, `{"notification_type":"elicitation_dialog"}`, `{"message":"x"}`, `{}`, `[]`} {
		s := open(t, t.TempDir())
		s.Append(hookG("hook.Stop", 1, `{}`))
		s.Append(Event{Sid: "s1", Gen: 1, Src: "fence", Type: "hook.Notification", Data: json.RawMessage(data)})
		if _, ok := s.IdleSince("s1", 1); ok {
			t.Fatalf("Notification %s after Stop must make the pane busy", data)
		}
		s.Close()
	}
	// idle_prompt must not promote a pane that is mid-turn
	s := open(t, t.TempDir())
	s.Append(hookG("hook.UserPromptSubmit", 1, `{}`))
	s.Append(hookG("hook.Notification", 1, `{"notification_type":"idle_prompt"}`))
	if _, ok := s.IdleSince("s1", 1); ok {
		t.Fatal("idle_prompt promoted a busy pane")
	}
}

// ---- review of PR #4: a failed write must not corrupt acknowledged records; replay in time order ----

// context: the disk fills mid-record (a fence can make that happen on purpose). The half line must
// not stay: the next good append would run on from it, readers would stall at the merged line, and
// a restart would cut the acknowledged records after it and hand their seq numbers out again.
func TestFailedWriteIsRolledBackAndNoSeqIsReusedBlock(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Append(ev("a"))
	s.Append(ev("b"))
	s.writeFile = func(f *os.File, b []byte) (int, error) {
		n, _ := f.Write(b[:len(b)/2]) // ENOSPC after half a line
		return n, os.ErrInvalid
	}
	if _, err := s.Append(ev("lost")); err == nil {
		t.Fatal("a failed write was acknowledged")
	}
	s.writeFile = nil
	r, err := s.Append(ev("c"))
	if err != nil || r.Seq != 3 {
		t.Fatalf("append after a failed write: seq %d err %v", r.Seq, err)
	}
	got, _ := s.After(0, "", "", 0)
	if len(got) != 3 || got[2].Type != "c" {
		t.Fatalf("readers stall or see the lost record: %v", seqs(got))
	}
	s.Close()
	s2 := open(t, dir) // the restart must keep what was acknowledged
	got, _ = s2.After(0, "", "", 0)
	if len(got) != 3 || got[2].Type != "c" || got[2].Seq != 3 {
		t.Fatalf("restart lost acknowledged records: %v", seqs(got))
	}
}

// context: fsync fails after the whole line was written. The line is in the page cache but was
// never acknowledged; it must not stay (two records with one seq would both replay).
func TestFailedFsyncDoesNotLeaveADuplicateSeqBlock(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Append(ev("a"))
	s.syncFile = func(*os.File) error { return os.ErrInvalid }
	if _, err := s.Append(ev("x")); err == nil {
		t.Fatal("a failed fsync was acknowledged")
	}
	s.syncFile = nil
	s.Append(ev("y"))
	s.Close()
	got, _ := open(t, dir).After(0, "", "", 0)
	if len(got) != 2 || got[1].Type != "y" || got[1].Seq != 2 {
		t.Fatalf("%v", seqs(got))
	}
}

// context: a write failed AND the cut-back failed too: we no longer know what is in the file, so
// appending more would risk the acknowledged records. Refuse everything until a reopen.
func TestUnrecoverableWriteFailureRefusesFurtherAppends(t *testing.T) {
	s := open(t, t.TempDir())
	s.Append(ev("a"))
	s.writeFile = func(f *os.File, b []byte) (int, error) {
		f.Close() // the file is gone: neither the write nor the truncate can work
		return 0, os.ErrInvalid
	}
	s.Append(ev("x"))
	s.writeFile = nil
	if _, err := s.Append(ev("y")); err == nil || !errors.Is(err, ErrBroken) {
		t.Fatalf("appended to a journal in an unknown state: %v", err)
	}
}

// context: hostd restarts just after UTC midnight. The worker's last Stop was yesterday, but today
// it started a turn (or shows a permission menu). Replaying the newest day FIRST let yesterday win,
// so the pane looked idle and a queued message was typed into the menu.
func TestIdleAfterRestartAcrossMidnightUsesTheNewestEventBlock(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Now = func() time.Time { return time.Date(2026, 11, 19, 23, 50, 0, 0, time.UTC) }
	s.Append(hook("hook.Stop"))
	s.Now = func() time.Time { return time.Date(2026, 11, 20, 0, 10, 0, 0, time.UTC) }
	s.Append(hookG("hook.Notification", 1, `{"notification_type":"permission_prompt"}`))
	if _, ok := s.IdleSince("s1", 1); ok {
		t.Fatal("not idle before the restart either")
	}
	s.Close()
	s2 := open(t, dir)
	if _, ok := s2.IdleSince("s1", 1); ok {
		t.Fatal("after the restart yesterday's Stop won: a menu would get a paste")
	}
}

// context: the allow side of the above: today's Stop after yesterday's busy state is idle.
func TestIdleAfterRestartAcrossMidnightAllow(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Now = func() time.Time { return time.Date(2026, 11, 19, 23, 50, 0, 0, time.UTC) }
	s.Append(hook("hook.UserPromptSubmit"))
	s.Now = func() time.Time { return time.Date(2026, 11, 20, 0, 10, 0, 0, time.UTC) }
	s.Append(hook("hook.Stop"))
	s.Close()
	if _, ok := open(t, dir).IdleSince("s1", 1); !ok {
		t.Fatal("idle lost")
	}
}

// context: an I/O hiccup reading the epoch file at startup must not mint a new epoch (every
// consumer would see epoch_changed and replay from scratch).
func TestUnreadableEpochIsNotReplaced(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Close()
	ep := filepath.Join(dir, "epoch")
	os.Remove(ep)
	os.Mkdir(ep, 0o700) // reading a directory fails with EISDIR, not ENOENT
	if _, err := Open(dir, "hp-ubuntu", 0); err == nil {
		t.Fatal("a read error made a new epoch")
	}
}

// context: the clock stepped back across midnight, so higher seqs sit in an older-named segment;
// the next seq after a restart must come from the maximum over all segments, not the last name.
func TestRestartTakesTheHighestSeqOverAllSegments(t *testing.T) {
	dir := t.TempDir()
	s := open(t, dir)
	s.Now = func() time.Time { return time.Date(2026, 11, 20, 0, 5, 0, 0, time.UTC) }
	s.Append(ev("a")) // seq 1 in seg-20261120
	s.Now = func() time.Time { return time.Date(2026, 11, 19, 23, 59, 0, 0, time.UTC) }
	s.Append(ev("b")) // seq 2 in the OLDER-named segment
	s.Close()
	r, err := open(t, dir).Append(ev("c"))
	if err != nil || r.Seq != 3 {
		t.Fatalf("seq %d err %v", r.Seq, err)
	}
}
