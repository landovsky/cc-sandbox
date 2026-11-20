// Tests of the remote start (M2c). Why each exists: a remote caller is the owner's other machine, but the
// request is still a request: it must not widen what the profile allows, must not be able to start a second
// generation of a label that lives or is being started on another machine, must be retryable after a lost
// answer without starting twice, and must not be able to use up the owner's local budget.
package remotestart

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/profile"
	"github.com/landovsky/cc-sandbox/internal/start"
)

var by = Caller{StartedBy: "owner:tomas@example.com@mac-tomas", Machine: "mac-tomas.example.ts.net"}

type rig struct {
	t       *testing.T
	s       *Service
	starts  atomic.Int32
	lastBy  string
	live    []View
	liveErr error
	asks    func(ctx context.Context, p Peer, label string) (Answer, error)
	startFn func() (*start.StartReply, error)
	now     time.Time
	mu      sync.Mutex
}

func newRig(t *testing.T, peers ...string) *rig {
	t.Helper()
	r := &rig{t: t, now: time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)}
	names := append([]string{"hp-ubuntu"}, peers...)
	var ps []Peer
	for _, n := range peers {
		ps = append(ps, Peer{Name: n, URL: "http://" + n + ":7480"})
	}
	prof := &profile.Profile{Name: "dev-default", Machines: []string{"hp-ubuntu"}, Billing: []string{"api", "stub"}, Grants: []string{"git.push"}, HarnessArgs: []string{"--model"}}
	set := PeerSetHash(names)
	r.asks = func(ctx context.Context, p Peer, label string) (Answer, error) {
		return Answer{Live: []View{}, PeerSet: set}, nil
	}
	s, err := New(Config{
		Machine: "hp-ubuntu", OwnerUser: "tomas", Peers: ps, PeerSet: set, StateDir: t.TempDir(),
		Profiles: func(n string) (*profile.Profile, error) {
			if n == prof.Name {
				return prof, nil
			}
			return nil, errors.New("no")
		},
		LookupUID: func(string) (int, error) { return 1000, nil },
		Starter: func(uid int, spec []byte, startedBy string) (*start.StartReply, error) {
			r.starts.Add(1)
			r.lastBy = startedBy
			if r.startFn != nil {
				return r.startFn()
			}
			return &start.StartReply{V: 1, Sid: "s0abc123", Gen: 1}, nil
		},
		Live:    func(label string) ([]View, error) { r.mu.Lock(); defer r.mu.Unlock(); return r.live, r.liveErr },
		AskPeer: func(ctx context.Context, p Peer, label string) (Answer, error) { return r.asks(ctx, p, label) },
		Now:     func() time.Time { r.mu.Lock(); defer r.mu.Unlock(); return r.now },
		Rand:    func(n int) int { return 0 },
	})
	if err != nil {
		t.Fatal(err)
	}
	r.s = s
	return r
}

func (r *rig) advance(d time.Duration) { r.mu.Lock(); r.now = r.now.Add(d); r.mu.Unlock() }

func spec(m map[string]any) []byte {
	base := map[string]any{"v": 1, "request_id": "req-1", "owner": "tomas", "machine": "hp-ubuntu", "label": "cc-sandbox:w1", "realm": "production",
		"harness": "claude", "workdir": map[string]any{"path": "/home/tomas/git/x"}, "profile": "dev-default", "billing": "api",
		"brief": map[string]any{"text": "do it"}, "client": map[string]any{"lid": "L1"}}
	for k, v := range m {
		base[k] = v
	}
	b, _ := json.Marshal(base)
	return b
}

func answerOf(s *Service, label string) Answer { a, _ := s.Answer(label); return a }

func code(r *start.Refusal) string {
	if r == nil {
		return ""
	}
	return r.Code
}

// context (C2): grants and harness flags are the PROFILE's to allow; a request can only pick among them.
func TestARemoteCallerCannotAskForWhatTheProfileDoesNotAllowBlock(t *testing.T) {
	r := newRig(t)
	for name, c := range map[string]struct {
		extra map[string]any
		want  string
	}{
		"a grant the profile does not list":      {map[string]any{"grants": []string{"net.any"}}, start.GrantNotAllowed},
		"a flag the profile does not list":       {map[string]any{"harness_args": []string{"--dangerously-skip-permissions"}}, start.BadSpec},
		"a listed flag with an unacceptable val": {map[string]any{"harness_args": []string{"--model", "x; rm -rf"}}, start.BadSpec},
		"a bare word":                            {map[string]any{"harness_args": []string{"model"}}, start.BadSpec},
		"another owner":                          {map[string]any{"owner": "mallory"}, start.BadSpec},
		"a gitdir (a remote start has none)":     {map[string]any{"gitdir": "/home/tomas/.git"}, start.BadSpec},
		"another machine's spec":                 {map[string]any{"machine": "future-two"}, start.WrongMachine},
		"an unapproved profile":                  {map[string]any{"profile": "other"}, start.ProfileNotApproved},
	} {
		if _, ref := r.s.Start(context.Background(), by, spec(c.extra)); code(ref) != c.want {
			t.Errorf("%s: %v, want %s", name, ref, c.want)
		}
	}
	if r.starts.Load() != 0 {
		t.Fatalf("something was started: %d", r.starts.Load())
	}
	if _, ref := r.s.Start(context.Background(), by, spec(map[string]any{"grants": []string{"git.push"}, "harness_args": []string{"--model", "claude-sonnet-5-5"}})); ref != nil {
		t.Fatalf("what the profile lists was refused: %v", ref)
	}
	if r.lastBy != by.StartedBy {
		t.Fatalf("started_by passed to the start: %q", r.lastBy)
	}
}

// context (C7): the answer to a start can be lost; the client retries with the same request_id and must get
// the FIRST reply, not a second session; cc-tree rewrites the brief between the attempts.
func TestARetryAfterALostAnswerStartsNothingAndGetsTheFirstReply(t *testing.T) {
	r := newRig(t)
	first, ref := r.s.Start(context.Background(), by, spec(nil))
	if ref != nil {
		t.Fatal(ref)
	}
	again, ref := r.s.Start(context.Background(), by, spec(map[string]any{"brief": map[string]any{"text": "the brief, rewritten"}}))
	if ref != nil || again.Sid != first.Sid || r.starts.Load() != 1 {
		t.Fatalf("retry: %v %v starts=%d", again, ref, r.starts.Load())
	}
	if _, ref := r.s.Start(context.Background(), by, spec(map[string]any{"profile": "dev-default", "billing": "stub"})); code(ref) != start.RequestIDReused {
		t.Fatalf("the same request_id with another request: %v", ref)
	}
	other := Caller{StartedBy: "owner:tomas@example.com@old-laptop", Machine: "old-laptop"}
	if rep, ref := r.s.Start(context.Background(), other, spec(map[string]any{"label": "cc-sandbox:w2"})); ref != nil || rep == nil {
		t.Fatalf("the same request_id from ANOTHER caller is its own request: %v", ref)
	}
}

// context (C7): a retry while the first attempt is still launching must not be told the label is running
// (which would read as "your start failed"); it is told to come back.
func TestARetryWhileTheFirstAttemptIsStillLaunchingIsToldToComeBackNotAlreadyRunning(t *testing.T) {
	r := newRig(t)
	gate, started := make(chan struct{}), make(chan struct{})
	r.startFn = func() (*start.StartReply, error) {
		close(started)
		<-gate
		return &start.StartReply{V: 1, Sid: "s0abc123"}, nil
	}
	done := make(chan struct{})
	go func() { r.s.Start(context.Background(), by, spec(nil)); close(done) }()
	<-started
	_, ref := r.s.Start(context.Background(), by, spec(nil))
	if code(ref) != start.StartInProgress || ref.RetryAfter < 1 || Status(ref.Code) != 409 {
		t.Fatalf("%v", ref)
	}
	// a different request for the same label meanwhile: also start_in_progress (the label is reserved here)
	if _, ref := r.s.Start(context.Background(), by, spec(map[string]any{"request_id": "req-2"})); code(ref) != start.StartInProgress {
		t.Fatalf("another request for a label being started: %v", ref)
	}
	close(gate)
	<-done
	if r.starts.Load() != 1 {
		t.Fatalf("starts: %d", r.starts.Load())
	}
}

// context: a refused or failed attempt created nothing, so a retry is a fresh attempt, not a replay of a failure.
func TestAFailedAttemptLeavesNoTraceSoTheRetryIsFresh(t *testing.T) {
	r := newRig(t)
	r.startFn = func() (*start.StartReply, error) { return nil, &start.Refusal{Code: start.MachineFull, Msg: "full"} }
	if _, ref := r.s.Start(context.Background(), by, spec(nil)); code(ref) != start.MachineFull {
		t.Fatalf("%v", ref)
	}
	r.startFn = nil
	if rep, ref := r.s.Start(context.Background(), by, spec(nil)); ref != nil || rep == nil || r.starts.Load() != 2 {
		t.Fatalf("%v %v %d", rep, ref, r.starts.Load())
	}
}

// context (C8): remote callers have their own minute budgets, so a runaway client is capped without touching
// anyone else's, and the cap says when to come back.
func TestRemoteStartsAreRateCappedPerCallerAndPerCallingMachine(t *testing.T) {
	r := newRig(t)
	for i := 0; i < 3; i++ {
		if _, ref := r.s.Start(context.Background(), by, spec(map[string]any{"request_id": fmt.Sprint("r", i), "label": fmt.Sprint("cc-sandbox:w", i)})); ref != nil {
			t.Fatalf("start %d: %v", i, ref)
		}
	}
	_, ref := r.s.Start(context.Background(), by, spec(map[string]any{"request_id": "r9", "label": "cc-sandbox:w9"}))
	if code(ref) != start.RateCapped || ref.RetryAfter < 1 || Status(ref.Code) != 429 {
		t.Fatalf("the 4th start in a minute: %v", ref)
	}
	r.advance(61 * time.Second)
	if _, ref := r.s.Start(context.Background(), by, spec(map[string]any{"request_id": "r9", "label": "cc-sandbox:w9"})); ref != nil {
		t.Fatalf("the minute rolled: %v", ref)
	}
	// ten a minute from one calling machine, however many callers it claims to be
	r2 := newRig(t)
	n := 0
	for i := 0; i < 12; i++ {
		c := Caller{StartedBy: fmt.Sprint("owner:tomas@example.com@dev", i), Machine: "mac-tomas.example.ts.net"}
		if _, ref := r2.s.Start(context.Background(), c, spec(map[string]any{"request_id": fmt.Sprint("m", i), "label": fmt.Sprint("cc-sandbox:m", i)})); ref == nil {
			n++
		} else if code(ref) != start.RateCapped {
			t.Fatal(ref)
		}
	}
	if n != 10 {
		t.Fatalf("%d starts from one machine in a minute, want 10", n)
	}
}

// context (C3): one live generation per label across machines. The rule has no tie-break: any peer with the
// label live OR reserved refuses, so two machines starting at once cannot both launch.
func TestAPeerWithTheLabelLiveOrReservedRefusesBlock(t *testing.T) {
	r := newRig(t, "mac-tomas", "future-two")
	set := PeerSetHash([]string{"hp-ubuntu", "mac-tomas", "future-two"})
	r.asks = func(ctx context.Context, p Peer, label string) (Answer, error) {
		switch p.Name {
		case "mac-tomas":
			return Answer{Live: []View{{Machine: "mac-tomas", Sid: "s0live", State: "running"}}, PeerSet: set}, nil
		}
		return Answer{Live: []View{}, PeerSet: set}, nil
	}
	_, ref := r.s.Start(context.Background(), by, spec(nil))
	if code(ref) != start.AlreadyRunning || ref.Machine != "mac-tomas" || ref.Sid != "s0live" || Status(ref.Code) != 409 {
		t.Fatalf("a peer has it live: %+v", ref)
	}
	r.asks = func(ctx context.Context, p Peer, label string) (Answer, error) {
		a := Answer{Live: []View{}, PeerSet: set}
		if p.Name == "future-two" {
			a.Reserved = &Reservation{Machine: "future-two", TS: 1}
		}
		return a, nil
	}
	if _, ref := r.s.Start(context.Background(), by, spec(map[string]any{"request_id": "req-2"})); code(ref) != start.StartInProgress || ref.RetryAfter < 1 {
		t.Fatalf("a peer is starting it (even one whose name sorts after ours): %+v", ref)
	}
	if r.starts.Load() != 0 {
		t.Fatalf("a start was launched: %d", r.starts.Load())
	}
	if _, err := os.Stat(r.s.resPath("cc-sandbox:w1")); err == nil {
		t.Fatal("a refused start left its reservation behind")
	}
}

// context (C3/C4): a listed peer that cannot be asked refuses the start (a guess either way breaks the rule),
// and a peer configured with a different set of machines is not trusted to have asked the same ones.
func TestAnUnreachablePeerOrADifferentPeerSetRefusesTheStart(t *testing.T) {
	r := newRig(t, "mac-tomas")
	r.asks = func(ctx context.Context, p Peer, label string) (Answer, error) {
		return Answer{}, errors.New("timeout")
	}
	if _, ref := r.s.Start(context.Background(), by, spec(nil)); code(ref) != start.PeerUnreachable || Status(ref.Code) != 503 || ref.RetryAfter < 1 {
		t.Fatalf("%+v", ref)
	}
	r.asks = func(ctx context.Context, p Peer, label string) (Answer, error) {
		return Answer{Live: []View{}, PeerSet: PeerSetHash([]string{"hp-ubuntu", "mac-tomas", "future-two"})}, nil
	}
	if _, ref := r.s.Start(context.Background(), by, spec(map[string]any{"request_id": "r2"})); code(ref) != start.PeerSetMismatch {
		t.Fatalf("%+v", ref)
	}
	if r.starts.Load() != 0 {
		t.Fatal("started")
	}
}

// context (C3): the peers are asked in parallel under ONE deadline, not one after another under the lock, so
// several slow peers cost one timeout, not N.
func TestSlowPeersAreAskedInParallelUnderOneDeadline(t *testing.T) {
	r := newRig(t, "a", "b", "c", "d")
	r.s.c.AskTimeout = 300 * time.Millisecond
	r.asks = func(ctx context.Context, p Peer, label string) (Answer, error) {
		select {
		case <-ctx.Done():
			return Answer{}, ctx.Err()
		case <-time.After(10 * time.Second):
			return Answer{}, nil
		}
	}
	t0 := time.Now()
	_, ref := r.s.Start(context.Background(), by, spec(nil))
	if d := time.Since(t0); code(ref) != start.PeerUnreachable || d > 1500*time.Millisecond {
		t.Fatalf("%v after %v", ref, d)
	}
}

// context (C3): two machines asked to start the same label at the same moment. Each reserves, then asks the
// other; whichever order the answers come in, at most one may launch (the safe worst case is both refusing).
func TestTwoMachinesStartingTheSameLabelAtOnceNeverBothLaunch(t *testing.T) {
	for i := 0; i < 40; i++ {
		a, b := newRig(t, "b"), newRig(t, "a")
		set := PeerSetHash([]string{"hp-ubuntu", "a", "b"})
		_ = set
		a.s.c.PeerSet, b.s.c.PeerSet = "S", "S"
		a.s.c.AskPeer = func(ctx context.Context, p Peer, label string) (Answer, error) { return answerOf(b.s, label), nil }
		b.s.c.AskPeer = func(ctx context.Context, p Peer, label string) (Answer, error) { return answerOf(a.s, label), nil }
		hold := make(chan struct{})
		// once launched, the session is live on that machine: what its own answer to a peer then says
		for _, r := range []*rig{a, b} {
			r := r
			r.startFn = func() (*start.StartReply, error) {
				r.mu.Lock()
				r.live = []View{{Machine: "x", Sid: "sa", State: "running"}}
				r.mu.Unlock()
				return &start.StartReply{V: 1, Sid: "sa"}, nil
			}
		}
		var wg sync.WaitGroup
		for _, r := range []*rig{a, b} {
			wg.Add(1)
			go func(r *rig) { defer wg.Done(); <-hold; r.s.Start(context.Background(), by, spec(nil)) }(r)
		}
		close(hold)
		wg.Wait()
		if a.starts.Load()+b.starts.Load() > 1 {
			t.Fatalf("iteration %d: both machines launched the same label", i)
		}
	}
}

// context (C3): the reservation is on disk while the launch runs (a daemon restart can see it), is renewed so
// a long launch keeps it, and is gone when the outcome is known.
func TestTheReservationLivesUntilTheLaunchOutcomeIsKnown(t *testing.T) {
	r := newRig(t)
	r.s.c.ReserveTTL = 90 * time.Millisecond // renewed every 30 ms
	seen := make(chan bool, 1)
	r.startFn = func() (*start.StartReply, error) {
		time.Sleep(250 * time.Millisecond) // longer than the TTL: only renewal keeps it
		seen <- answerOf(r.s, "cc-sandbox:w1").Reserved != nil
		return &start.StartReply{V: 1, Sid: "s0abc123"}, nil
	}
	r.advanceTicker()
	if _, ref := r.s.Start(context.Background(), by, spec(nil)); ref != nil {
		t.Fatal(ref)
	}
	if !<-seen {
		t.Fatal("the reservation lapsed during a long launch")
	}
	if answerOf(r.s, "cc-sandbox:w1").Reserved != nil {
		t.Fatal("the reservation outlived the launch")
	}
}

// advanceTicker makes the rig's clock follow real time, so renewals (which compare against Now) are meaningful.
func (r *rig) advanceTicker() { r.s.c.Now = time.Now }

// context: a daemon that died mid-launch leaves a reservation and an in-flight marker; past their time they
// must not block the label forever.
func TestStaleReservationsAndInFlightMarkersAreReconciledAtStart(t *testing.T) {
	r := newRig(t)
	r.s.writeRes(resFile{Label: "cc-sandbox:w1", Machine: "hp-ubuntu", RequestID: "old", TS: 1, Renewed: 1000})
	r.s.writeIdem("deadbeef", idemRec{Hash: "h", State: "inflight", At: 1})
	r.s.reconcile()
	if _, err := os.Stat(r.s.resPath("cc-sandbox:w1")); err == nil {
		t.Fatal("a stale reservation survived")
	}
	if _, err := os.Stat(r.s.idemPath("deadbeef")); err == nil {
		t.Fatal("a stale in-flight marker survived")
	}
}

// context: the label may already be live HERE; the client gets where and which session, to reattach.
func TestALabelLiveOnThisMachineIsAlreadyRunningWithItsSid(t *testing.T) {
	r := newRig(t)
	r.live = []View{{Machine: "hp-ubuntu", Sid: "s0here", Gen: 2, State: "running"}}
	_, ref := r.s.Start(context.Background(), by, spec(nil))
	if code(ref) != start.AlreadyRunning || ref.Sid != "s0here" || ref.Machine != "hp-ubuntu" {
		t.Fatalf("%+v", ref)
	}
	if !strings.Contains(fmt.Sprint(ref), "already") {
		t.Fatal("message")
	}
}

// context (M2, review): the label check must fail CLOSED. One unreadable session file made hp answer "nothing live"
// for every label, so the Mac launched a second generation. "Could not tell" is an error, never "free".
func TestACheckThatCannotReadTheRecordsAnswersAnErrorNeverFree(t *testing.T) {
	r := newRig(t)
	r.liveErr = errors.New("bad record file")
	if _, err := r.s.Answer("cc-sandbox:w1"); err == nil {
		t.Fatal("an unreadable record answered a label as free")
	}
	if _, ref := r.s.Start(context.Background(), by, spec(nil)); ref == nil || ref.Code != Internal || r.starts.Load() != 0 {
		t.Fatalf("a start went ahead while the local records could not be read: %v", ref)
	}
}

// context (S2, review): a renewal that runs late (a sleeping laptop, a clock step) must not end the renewals: a
// launch in flight stays reserved, answered from memory, whatever the stored timestamp says.
func TestALaunchInFlightStaysReservedEvenWhenTheStampLooksOld(t *testing.T) {
	r := newRig(t)
	gate, started := make(chan struct{}), make(chan struct{})
	r.startFn = func() (*start.StartReply, error) {
		close(started)
		<-gate
		return &start.StartReply{V: 1, Sid: "s0abc123"}, nil
	}
	done := make(chan struct{})
	go func() { r.s.Start(context.Background(), by, spec(nil)); close(done) }()
	<-started
	r.advance(10 * time.Minute) // the laptop slept: every stored stamp is now "dead"
	if a := answerOf(r.s, "cc-sandbox:w1"); a.Reserved == nil {
		t.Fatal("the reservation lapsed during a launch because the clock jumped")
	}
	close(gate)
	<-done
}

// context (S1, review): a request_id is the CALLER's: another caller sending the same id for another label must not be
// handed the first caller's reply, nor share its reservation.
func TestAnotherCallersSameRequestIDGetsItsOwnStart(t *testing.T) {
	r := newRig(t)
	first, _ := r.s.Start(context.Background(), by, spec(nil))
	other := Caller{StartedBy: "owner:tomas@example.com@phone", Machine: "phone"}
	r.startFn = func() (*start.StartReply, error) { return &start.StartReply{V: 1, Sid: "s0second"}, nil }
	rep, ref := r.s.Start(context.Background(), other, spec(map[string]any{"label": "cc-sandbox:other"}))
	if ref != nil || rep.Sid != "s0second" || rep.Sid == first.Sid {
		t.Fatalf("%v %v", rep, ref)
	}
}

// context (S4, review): a refused retry must never delete the stored reply of the start that happened.
func TestARefusedRetryNeverDeletesTheStoredReply(t *testing.T) {
	r := newRig(t)
	first, _ := r.s.Start(context.Background(), by, spec(nil))
	r.live = []View{{Machine: "hp-ubuntu", Sid: first.Sid, State: "running"}}
	if _, ref := r.s.Start(context.Background(), by, spec(map[string]any{"profile": "dev-default", "billing": "stub"})); code(ref) != start.RequestIDReused {
		t.Fatalf("%v", ref)
	}
	again, ref := r.s.Start(context.Background(), by, spec(nil))
	if ref != nil || again.Sid != first.Sid {
		t.Fatalf("the stored reply was lost: %v %v", again, ref)
	}
}

// context (R3-S1, review): hostd and a direct-mode CLI share the reservation directory, so a reservation that cannot be
// WRITTEN must refuse the start (an unreserved launch would be invisible to every peer), and a start must never
// proceed on a reservation another process wrote.
func TestAReservationThatCannotBeWrittenRefusesTheStart(t *testing.T) {
	r := newRig(t)
	os.Chmod(filepath.Join(r.s.c.StateDir, "res"), 0o500)
	defer os.Chmod(filepath.Join(r.s.c.StateDir, "res"), 0o700)
	if os.Geteuid() == 0 {
		t.Skip("root writes anywhere")
	}
	if _, ref := r.s.Start(context.Background(), by, spec(nil)); ref == nil || ref.Code != Internal || r.starts.Load() != 0 {
		t.Fatalf("%v starts=%d", ref, r.starts.Load())
	}
}

// context: another PROCESS (the CLI) holding a live reservation for the label makes this daemon's start wait its turn.
func TestAReservationAnotherProcessHoldsRefusesTheStart(t *testing.T) {
	r := newRig(t)
	now := r.s.c.Now()
	r.s.writeRes(resFile{Label: "cc-sandbox:w1", Machine: "hp-ubuntu", Token: "other-process", TS: now.Unix(), Renewed: now.UnixMilli()})
	if _, ref := r.s.Start(context.Background(), by, spec(nil)); code(ref) != start.StartInProgress {
		t.Fatalf("%v", ref)
	}
}
