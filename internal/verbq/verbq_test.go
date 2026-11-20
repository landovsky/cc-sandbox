// Tests of the verb queue and cockpit lease (M4). Why: a verb a cockpit has not applied must never
// be lost, one realm has one cockpit, and a lease can't be stolen or guessed.

package verbq

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type rig struct {
	q       *Queue
	now     time.Time
	replies []string
	reopen  func() *Queue
}

func newRig(t *testing.T) *rig {
	dir := t.TempDir()
	r := &rig{now: time.Date(2026, 11, 20, 1, 0, 0, 0, time.UTC)}
	mk := func() *Queue {
		q, err := Open(dir)
		if err != nil {
			t.Fatal(err)
		}
		q.Now = func() time.Time { return r.now }
		q.Reply = func(it Item, res Result) bool {
			r.replies = append(r.replies, fmt.Sprintf("%s:%s", it.Rid, res.Status))
			return true
		}
		t.Cleanup(q.Close)
		return q
	}
	r.q, r.reopen = mk(), mk
	return r
}

func item(sid, rid, typ string) Item {
	return Item{ID: "id-" + sid + "-" + rid, Sid: sid, Gen: 1, Rid: rid, Node: "cc-sandbox:" + sid, Type: typ, Body: json.RawMessage(`{}`)}
}

func rids(its []Item) string {
	s := ""
	for _, it := range its {
		s += it.Sid + "/" + it.Rid + " "
	}
	return s
}

// context: no cockpit anywhere. `done` is answered "queued #seq" at once and waits, however
// long the cockpit is away; nothing is dropped.
func TestNoCockpitEverythingQueuesInOrder(t *testing.T) {
	r := newRig(t)
	a, _ := r.q.Enqueue("production", item("s1", "a", "ccf.done"))
	b, _ := r.q.Enqueue("production", item("s1", "b", "ccf.mark"))
	if a.Seq != 1 || b.Seq != 2 {
		t.Fatalf("%d %d", a.Seq, b.Seq)
	}
	r.now = r.now.Add(30 * 24 * time.Hour)
	st, _ := r.q.Status("production")
	if st.Pending != 2 || st.OldestS < 30*24*3600 {
		t.Fatalf("%+v", st)
	}
}

// context: two cockpits on the same realm (Mac and hp) must not both apply verbs: the second
// lease is refused (409) while the first is live; the e2e realm has its own lease.
func TestSecondLeaseInTheSameRealmIsRefused(t *testing.T) {
	r := newRig(t)
	l1, err := r.q.Acquire("production", "hp-ubuntu", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.q.Acquire("production", "mac", "", false); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("second holder: %v", err)
	}
	if _, err := r.q.Acquire("e2e", "mac", "", false); err != nil {
		t.Fatalf("e2e realm is independent: %v", err)
	}
	r.now = r.now.Add(60 * time.Second)
	if l, err := r.q.Acquire("production", "hp-ubuntu", l1.Token, false); err != nil || !l.Expires.After(r.now.Add(80*time.Second)) {
		t.Fatalf("renew: %v %+v", err, l)
	}
}

// context: a cockpit that died stops renewing; after the 90 s TTL the other machine can take over
// without anyone logging in to the daemon.
func TestLeaseExpiresAfterTtlAndCanBeStolenOnPurpose(t *testing.T) {
	r := newRig(t)
	r.q.Acquire("production", "hp", "", false)
	r.now = r.now.Add(89 * time.Second)
	if _, err := r.q.Acquire("production", "mac", "", false); !errors.Is(err, ErrLeaseHeld) {
		t.Fatalf("89s: %v", err)
	}
	r.now = r.now.Add(2 * time.Second)
	if _, err := r.q.Acquire("production", "mac", "", false); err != nil {
		t.Fatalf("after ttl: %v", err)
	}
	if _, err := r.q.Acquire("production", "hp", "", false); !errors.Is(err, ErrLeaseHeld) {
		t.Fatal("hp took a live lease without steal")
	}
	if _, err := r.q.Acquire("production", "hp", "", true); err != nil {
		t.Fatalf("steal: %v", err)
	}
}

// context: only the holder pulls. A stale token (the lease was stolen or expired) learns it.
func TestOnlyTheHolderPullsAndReports(t *testing.T) {
	r := newRig(t)
	r.q.Enqueue("production", item("s1", "a", "ccf.done"))
	if _, err := r.q.Pull("production", "", 10); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("no lease: %v", err)
	}
	l, _ := r.q.Acquire("production", "hp", "", false)
	if _, err := r.q.Pull("production", "wrong", 10); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("wrong token: %v", err)
	}
	got, err := r.q.Pull("production", l.Token, 10)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v %v", err, got)
	}
	r.now = r.now.Add(2 * time.Minute)
	if _, err := r.q.Report("production", l.Token, got[0].ID, Result{Status: "ok"}); !errors.Is(err, ErrLeaseLost) {
		t.Fatalf("expired lease must not report: %v", err)
	}
}

// context: the e2e cockpit runs its own queue; production's requests are not visible to it.
func TestRealmsDoNotSeeEachOthersRequests(t *testing.T) {
	r := newRig(t)
	r.q.Enqueue("production", item("s1", "a", "ccf.done"))
	r.q.Enqueue("e2e", item("s2", "a", "ccf.done"))
	le, _ := r.q.Acquire("e2e", "x", "", false)
	got, _ := r.q.Pull("e2e", le.Token, 0)
	if rids(got) != "s2/a " {
		t.Fatalf("%s", rids(got))
	}
	if _, err := r.q.Enqueue("../etc", item("s", "a", "ccf.done")); !errors.Is(err, ErrBadRealm) {
		t.Fatal("path-like realm accepted")
	}
}

// context: a tree repo that is mid-rebase refuses commits. The verb must wait (⚠ in the view, the
// session not told), keep its place in the session's order, and apply once the rebase is done.
func TestTransientFailureHoldsTheVerbAndTheSessionsLaterOnesNotOthers(t *testing.T) {
	r := newRig(t)
	r.q.Enqueue("production", item("s1", "a", "ccf.outcome"))
	r.q.Enqueue("production", item("s1", "b", "ccf.done"))
	r.q.Enqueue("production", item("s2", "c", "ccf.mark"))
	l, _ := r.q.Acquire("production", "hp", "", false)
	got, _ := r.q.Pull("production", l.Token, 0)
	if rids(got) != "s1/a s2/c " {
		t.Fatalf("one head per session: %s", rids(got))
	}
	if w, err := r.q.Report("production", l.Token, got[0].ID, Result{Status: "transient", Code: "RepoBusy"}); err != nil || w {
		t.Fatalf("%v %v", w, err)
	}
	if len(r.replies) != 0 {
		t.Fatalf("a transient failure told the session: %v", r.replies)
	}
	st, _ := r.q.Status("production")
	if len(st.Warnings) != 1 || st.Warnings[0].LastErr != "RepoBusy" || st.Pending != 3 {
		t.Fatalf("%+v", st)
	}
	again, _ := r.q.Pull("production", l.Token, 0)
	if rids(again) != "s2/c " {
		t.Fatalf("during back-off s1 stays held (b must not overtake a): %s", rids(again))
	}
	r.now = r.now.Add(6 * time.Second)
	l, _ = r.q.Acquire("production", "hp", l.Token, false)
	again, _ = r.q.Pull("production", l.Token, 0)
	if rids(again) != "s1/a s2/c " || again[0].Attempts != 1 {
		t.Fatalf("after back-off (rebase continued): %s %+v", rids(again), again)
	}
	r.q.Report("production", l.Token, again[0].ID, Result{Status: "ok"})
	next, _ := r.q.Pull("production", l.Token, 0)
	if rids(next) != "s1/b s2/c " {
		t.Fatalf("%s", rids(next))
	}
}

// context: a permanent result (the node is gone, the verb is stale) goes back to the session.
func TestPermanentResultGoesBackToTheSession(t *testing.T) {
	r := newRig(t)
	r.q.Enqueue("production", item("s1", "a", "ccf.done"))
	l, _ := r.q.Acquire("production", "hp", "", false)
	got, _ := r.q.Pull("production", l.Token, 0)
	w, err := r.q.Report("production", l.Token, got[0].ID, Result{Status: "stale", Code: "dropped_node"})
	if err != nil || !w || len(r.replies) != 1 || r.replies[0] != "a:stale" {
		t.Fatalf("%v %v %v", w, err, r.replies)
	}
	if st, _ := r.q.Status("production"); st.Pending != 0 {
		t.Fatal("answered request still pending")
	}
	if _, err := r.q.Report("production", l.Token, got[0].ID, Result{Status: "ok"}); !errors.Is(err, ErrNoSuchItem) {
		t.Fatalf("a request is answered once: %v", err)
	}
	if _, err := r.q.Report("production", l.Token, "nope", Result{Status: "maybe"}); err == nil {
		t.Fatal("bad status accepted")
	}
}

// context: the store is not the journal; it is never pruned and survives the daemon restarting.
func TestQueueSurvivesRestartWithOrderAndRetryState(t *testing.T) {
	r := newRig(t)
	r.q.Enqueue("production", item("s1", "a", "ccf.outcome"))
	r.q.Enqueue("production", item("s1", "b", "ccf.done"))
	l, _ := r.q.Acquire("production", "hp", "", false)
	got, _ := r.q.Pull("production", l.Token, 0)
	r.q.Report("production", l.Token, got[0].ID, Result{Status: "transient", Code: "lock"})
	r.q.Close()
	q2 := r.reopen()
	st, _ := q2.Status("production")
	if st.Pending != 2 || len(st.Warnings) != 1 {
		t.Fatalf("%+v", st)
	}
	n, _ := q2.Enqueue("production", item("s1", "c", "ccf.mark"))
	if n.Seq != 3 {
		t.Fatalf("seq continued as %d", n.Seq)
	}
}

// context: a full queue says so (refused: queue_full) instead of silently dropping the oldest.
func TestFullQueueRefusesInsteadOfDropping(t *testing.T) {
	r := newRig(t)
	r.q.MaxPending = 3
	for i := 0; i < 3; i++ {
		if _, err := r.q.Enqueue("production", item("s1", fmt.Sprint("r", i), "ccf.mark")); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := r.q.Enqueue("production", item("s1", "r9", "ccf.mark")); !errors.Is(err, ErrQueueFull) {
		t.Fatalf("%v", err)
	}
}

// context: after an epoch reset hostd re-ingests an unreplied file under a NEW request id. If
// the first attempt had already been queued, the verb must not be queued twice.
func TestReingestedRequestIsNotQueuedTwice(t *testing.T) {
	r := newRig(t)
	a := item("s1", "a", "ccf.done")
	r.q.Enqueue("production", a)
	a.ID = "a-new-ulid"
	b, _ := r.q.Enqueue("production", a)
	if b.ID != "id-s1-a" {
		t.Fatalf("second enqueue created %s", b.ID)
	}
	if st, _ := r.q.Status("production"); st.Pending != 1 {
		t.Fatalf("%d pending", st.Pending)
	}
}

// ---- review of PR #4: replay must never cut away verbs, and a withdrawn verb stays withdrawn ----

// context: --max-bytes was raised and one verb's line is bigger than a bufio.Scanner's 1 MiB: the old
// replay stopped there and truncated every verb queued after it at the next start.
func TestVerbsAfterAVeryLongLineSurviveARestart(t *testing.T) {
	r := newRig(t)
	big := json.RawMessage(`{"x":"` + strings.Repeat("a", 1500<<10) + `"}`)
	for i, body := range []json.RawMessage{json.RawMessage(`{}`), big, json.RawMessage(`{}`)} {
		if _, err := r.q.Enqueue("production", Item{ID: fmt.Sprintf("01ARZ3NDEKTSV4RRFFQ69G5FA%d", i), Sid: "s1", Gen: 1, Rid: fmt.Sprintf("r%d", i), Type: "ccf.mark", Body: body}); err != nil {
			t.Fatal(err)
		}
	}
	q2 := r.reopen()
	got, _ := q2.Pending("production", "", "")
	if len(got) != 3 {
		t.Fatalf("%d of 3 verbs after a restart", len(got))
	}
}

// context: a complete line that does not parse (or an enq without an item) is corruption. It used to
// panic the daemon (crash loop) or truncate the queue; now the open is refused and nothing is cut.
func TestCorruptCompleteLineRefusesToOpenAndCutsNothing(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "production.jsonl")
	good := `{"op":"enq","item":{"id":"01ARZ3NDEKTSV4RRFFQ69G5FAA","sid":"s1","gen":1,"rid":"r","type":"ccf.mark","seq":1}}` + "\n"
	os.WriteFile(p, []byte(good+`{"op":"enq"}`+"\n"+good), 0o600)
	if q, err := Open(dir); err == nil {
		defer q.Close()
		if _, err := q.Pending("production", "", ""); err == nil {
			t.Fatal("a corrupt queue was opened")
		}
	}
	st, _ := os.Stat(p)
	if st.Size() != int64(len(good)*2+len(`{"op":"enq"}`)+1) {
		t.Fatalf("the queue file was cut: %d bytes", st.Size())
	}
}

// context: the journal append failed after the verb was queued, so the session was told io_error and
// will retry under a new rid. The withdrawn verb must not come back after a restart.
func TestWithdrawnVerbStaysWithdrawnAfterARestart(t *testing.T) {
	r := newRig(t)
	it, _ := r.q.Enqueue("production", Item{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAA", Sid: "s1", Gen: 1, Rid: "r", Type: "ccf.mark", Body: json.RawMessage(`{}`)})
	if err := r.q.Withdraw("production", it.ID); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.reopen().Pending("production", "", ""); len(got) != 0 {
		t.Fatalf("%d verbs back after restart", len(got))
	}
}

// context: a verb could not be withdrawn (the disk is full): it stays queued although its session
// was told it failed. The queue can't be trusted, so it refuses everything until a restart.
func TestQueueThatCannotWithdrawIsBroken(t *testing.T) {
	r := newRig(t)
	it, _ := r.q.Enqueue("production", Item{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAA", Sid: "s1", Gen: 1, Rid: "r", Type: "ccf.mark", Body: json.RawMessage(`{}`)})
	r.q.mu.Lock()
	r.q.realms["production"].f.Close() // the file is gone: neither the write nor the cut-back can work
	r.q.mu.Unlock()
	if err := r.q.Withdraw("production", it.ID); err == nil || !errors.Is(err, ErrBroken) {
		t.Fatalf("%v", err)
	}
	if _, err := r.q.Enqueue("production", Item{ID: "01ARZ3NDEKTSV4RRFFQ69G5FAB", Sid: "s1", Gen: 1, Rid: "r2", Type: "ccf.mark", Body: json.RawMessage(`{}`)}); !errors.Is(err, ErrBroken) {
		t.Fatalf("a broken queue accepted a verb: %v", err)
	}
}
