// Tests for the pod start path (F2w): a spec for a pod machine is started through the pod verbs, the
// pod's UID is on disk before anything secret can be sent, and every failure after the pod exists ends
// with the pod deleted by that UID. The verbs are a fake; the real ones are tested in internal/rootcmd.
package start

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

type fakeRoot struct {
	d               *Daemon
	calls           []string
	uid             string
	createEr        error
	loadEr          error
	stopEr          error
	uidOnDiskAtLoad string
	gone            bool
	stopGens        []int
	goneEr          error
}

func (f *fakeRoot) Create(sid string) (string, error) {
	f.calls = append(f.calls, "create")
	return f.uid, f.createEr
}
func (f *fakeRoot) Load(sid string) error {
	f.calls = append(f.calls, "load")
	if r, err := f.d.Load(sid); err == nil {
		f.uidOnDiskAtLoad = r.PodUID
	}
	return f.loadEr
}
func (f *fakeRoot) Stop(sid string, gen int) error {
	f.calls, f.stopGens = append(f.calls, "stop"), append(f.stopGens, gen)
	return f.stopEr
}
func (f *fakeRoot) Gone(sid string) (bool, error) {
	f.calls = append(f.calls, "gone")
	return f.gone, f.goneEr
}

func podDaemon(t *testing.T) (*Daemon, *fakeRoot, *[]string) {
	t.Helper()
	d, _ := newDaemon(t)
	fr := &fakeRoot{d: d, uid: "pod-uid-1"}
	var revoked []string
	d.Placements = map[string]string{"future-two": "k8s"}
	d.PodRoot = fr
	d.Revoke = func(sid string, gen int) { revoked = append(revoked, sid) }
	return d, fr, &revoked
}

func podSpec(mod func(*LaunchSpec)) *LaunchSpec {
	return spec(func(s *LaunchSpec) {
		s.Machine, s.Workdir = "future-two", Workdir{Repo: "landovsky/cc-sandbox", Branch: "ccsbx/work"}
		if mod != nil {
			mod(s)
		}
	})
}

func (d *Daemon) recordFiles(t *testing.T) []string {
	t.Helper()
	ents, _ := os.ReadDir(filepath.Join(d.StateDir, "sessions"))
	var out []string
	for _, e := range ents {
		out = append(out, e.Name())
	}
	return out
}

func TestPodStart(t *testing.T) {
	t.Run("the pod's UID is on disk before the sidecar is loaded, so every later step can check the live pod against it", func(t *testing.T) {
		d, fr, _ := podDaemon(t)
		rep, err := d.Start(podSpec(nil))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Join(fr.calls, ",") != "create,load" || fr.uidOnDiskAtLoad != "pod-uid-1" {
			t.Fatalf("calls %v, UID on disk at load %q", fr.calls, fr.uidOnDiskAtLoad)
		}
		rec, _ := d.Load(rep.Sid)
		if rec.Backend != "k8s" || rec.PodUID != "pod-uid-1" || rep.Backend != "k8s" || rep.ID != "tomas/future-two/"+rep.Sid+"/g1" || rec.Spec.Machine != "future-two" {
			t.Fatalf("%+v %+v", rec, rep)
		}
	})

	t.Run("with no revoke client the log names the sid and generation of a token that may exist", func(t *testing.T) {
		d, fr, _ := podDaemon(t)
		d.Revoke = nil
		var logged []string
		d.Logf = func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) }
		fr.loadEr = errors.New("side load refused: bad key")
		_, err := d.Start(podSpec(nil))
		if err == nil || len(logged) != 1 || !strings.Contains(logged[0], "revoke sid s") || !strings.Contains(logged[0], "gen 1 by hand") {
			t.Fatalf("%v %v", err, logged)
		}
		fr.loadEr = ErrPodNotReady
		logged = nil
		d.Start(podSpec(func(s *LaunchSpec) { s.RequestID, s.Label = "rid-5", "cc-sandbox:b" }))
		if len(logged) != 0 {
			t.Errorf("a pod that never came up has no token, yet: %v", logged)
		}
	})

	t.Run("a pod that never comes up is deleted by its UID and the start leaves no record", func(t *testing.T) {
		d, fr, revoked := podDaemon(t)
		fr.loadEr = ErrPodNotReady
		_, err := d.Start(podSpec(nil))
		if code(err) != PodNotReady || strings.Join(fr.calls, ",") != "create,load,stop" || len(d.recordFiles(t)) != 0 {
			t.Fatalf("%v calls %v records %v", err, fr.calls, d.recordFiles(t))
		}
		if len(*revoked) != 0 {
			t.Error("a token was revoked that was never minted")
		}
	})

	t.Run("a load that fails after the pod is up deletes the pod and revokes the token that may exist", func(t *testing.T) {
		d, fr, revoked := podDaemon(t)
		fr.loadEr = errors.New("side load refused: bad key")
		_, err := d.Start(podSpec(nil))
		if code(err) != PodLoadFailed || strings.Join(fr.calls, ",") != "create,load,stop" || len(*revoked) != 1 || len(d.recordFiles(t)) != 0 {
			t.Fatalf("%v calls %v revoked %v records %v", err, fr.calls, *revoked, d.recordFiles(t))
		}
	})

	t.Run("a pod under the name that is not ours is pod_conflict and nothing is deleted", func(t *testing.T) {
		d, fr, _ := podDaemon(t)
		fr.createEr = ErrPodConflict
		_, err := d.Start(podSpec(nil))
		if code(err) != PodConflict || strings.Join(fr.calls, ",") != "create" || len(d.recordFiles(t)) != 0 {
			t.Fatalf("%v calls %v", err, fr.calls)
		}
	})

	t.Run("when the pod cannot be deleted the record stays, ended, so a stop can retry it", func(t *testing.T) {
		d, fr, _ := podDaemon(t)
		fr.loadEr, fr.stopEr = ErrPodNotReady, errors.New("api down")
		_, err := d.Start(podSpec(nil))
		if err == nil || !strings.Contains(err.Error(), "pod-stop") {
			t.Fatalf("%v", err)
		}
		files := d.recordFiles(t)
		if len(files) != 1 {
			t.Fatalf("records %v", files)
		}
		rec, _ := d.Load(strings.TrimSuffix(files[0], ".json"))
		if rec.Ended == nil || rec.PodUID != "pod-uid-1" {
			t.Errorf("%+v", rec)
		}
	})

	t.Run("what a pod cannot do yet is refused before anything is created", func(t *testing.T) {
		for why, mod := range map[string]func(*LaunchSpec){
			"a host workdir": func(s *LaunchSpec) {
				s.Workdir = Workdir{Path: filepath.Join(curRoot, "wt"), Repo: "landovsky/cc-sandbox"}
			},
			"no repo":                    func(s *LaunchSpec) { s.Workdir = Workdir{} },
			"a resume":                   func(s *LaunchSpec) { s.Resume = &Resume{Sid: "s0abcdefgh"} },
			"a profile not approved":     func(s *LaunchSpec) { s.Profile = "e2e-default" },
			"no billing":                 func(s *LaunchSpec) { s.Billing = "" },
			"a billing the profile bars": func(s *LaunchSpec) { s.Billing = "subscription" },
		} {
			d, fr, _ := podDaemon(t)
			if _, err := d.Start(podSpec(mod)); err == nil || len(fr.calls) != 0 || len(d.recordFiles(t)) != 0 {
				t.Errorf("%s: %v calls %v", why, err, fr.calls)
			}
		}
		d, fr, _ := podDaemon(t)
		d.PodRoot = nil
		if _, err := d.Start(podSpec(nil)); code(err) != NotBuilt || len(fr.calls) != 0 {
			t.Errorf("no launcher: %v", err)
		}
		d, _, _ = podDaemon(t)
		d.Placements = nil
		if _, err := d.Start(podSpec(nil)); code(err) != WrongMachine {
			t.Errorf("a machine with no placement: %v", err)
		}
	})

	t.Run("a pod session has no pane, so it is live until stopped and its label blocks a second start", func(t *testing.T) {
		d, _, _ := podDaemon(t)
		if _, err := d.Start(podSpec(nil)); err != nil {
			t.Fatal(err)
		}
		_, err := d.Start(podSpec(func(s *LaunchSpec) { s.RequestID = "rid-2" }))
		if code(err) != AlreadyRunning {
			t.Fatalf("%v", err)
		}
	})

	t.Run("a docker start does not mistake a pod session for a dead pane", func(t *testing.T) {
		d, _, _ := podDaemon(t)
		rep, err := d.Start(podSpec(nil))
		if err != nil {
			t.Fatal(err)
		}
		if _, err := d.Start(spec(func(s *LaunchSpec) { s.RequestID, s.Label = "rid-9", "cc-sandbox:other" })); err != nil {
			t.Fatal(err)
		}
		if rec, _ := d.Load(rep.Sid); rec.Ended != nil {
			t.Error("the pod session was marked ended because it has no pane")
		}
	})

	t.Run("a stop deletes the pod by its UID, revokes the token and ends the record", func(t *testing.T) {
		d, fr, revoked := podDaemon(t)
		rep, err := d.Start(podSpec(nil))
		if err != nil {
			t.Fatal(err)
		}
		fr.calls = nil
		if err := d.StopPod(rep.Sid); err != nil {
			t.Fatal(err)
		}
		rec, _ := d.Load(rep.Sid)
		if strings.Join(fr.calls, ",") != "stop" || len(*revoked) != 1 || rec.Ended == nil {
			t.Errorf("calls %v revoked %v ended %v", fr.calls, *revoked, rec.Ended)
		}
		fr.stopEr = errors.New("api down")
		rec.Ended = nil
		d.save(rec)
		if err := d.StopPod(rep.Sid); err == nil {
			t.Error("a failed delete was reported as stopped")
		}
		if rec, _ := d.Load(rep.Sid); rec.Ended != nil {
			t.Error("the record was ended although the pod is still there")
		}
	})

	t.Run("stop refuses a docker session", func(t *testing.T) {
		d, _, _ := podDaemon(t)
		rep, err := d.Start(spec(nil))
		if err != nil {
			t.Fatal(err)
		}
		if err := d.StopPod(rep.Sid); err == nil {
			t.Error("a docker session was stopped as a pod")
		}
	})
}

func TestPodRecordFieldsRoundTrip(t *testing.T) {
	b, _ := json.Marshal(Record{Sid: "s0abcdefgh", Backend: "k8s", PodUID: "u"})
	if !strings.Contains(string(b), `"backend":"k8s"`) || !strings.Contains(string(b), `"pod_uid":"u"`) {
		t.Error(string(b))
	}
}

func logs(d *Daemon) *[]string {
	var l []string
	d.Logf = func(f string, a ...any) { l = append(l, fmt.Sprintf(f, a...)) }
	return &l
}

// A create whose answer is lost may have made a pod nobody recorded: the record stays (ended), and the log
// names the exact pod. A create the root refused made nothing, and the start leaves no trace.
func TestACreateThatMayHaveMadeAPodIsNotForgotten(t *testing.T) {
	d, fr, _ := podDaemon(t)
	l := logs(d)
	fr.createEr = errors.New("ccsbx-root pod-create: signal: terminated")
	_, err := d.Start(podSpec(nil))
	files := d.recordFiles(t)
	if err == nil || len(files) != 1 || len(*l) != 1 || !strings.Contains((*l)[0], "ccsbx-s-"+strings.TrimSuffix(files[0], ".json")+"-g1 may exist") {
		t.Fatalf("%v records %v log %v", err, files, *l)
	}
	if rec, _ := d.Load(strings.TrimSuffix(files[0], ".json")); rec.Ended == nil {
		t.Error("the record holds a live slot for a session that never started")
	}
	d, fr, _ = podDaemon(t)
	l = logs(d)
	fr.createEr = fmt.Errorf("%w: policy", ErrPodRefused)
	d.Start(podSpec(nil))
	if len(d.recordFiles(t)) != 0 || len(*l) != 0 {
		t.Errorf("a refused create left a record %v or a log %v", d.recordFiles(t), *l)
	}
}

// If the UID cannot be written down after the pod was made, the log names the pod and its UID and the
// record no longer holds a slot.
func TestAPodWhoseUIDCannotBeRecordedIsNamedInTheLog(t *testing.T) {
	d, fr, _ := podDaemon(t)
	l := logs(d)
	dir := filepath.Join(d.StateDir, "sessions")
	t.Cleanup(func() { os.Chmod(dir, 0o700) })
	fr2 := &saveBreaker{fakeRoot: fr, dir: dir}
	d.PodRoot = fr2
	_, err := d.Start(podSpec(nil))
	if err == nil || len(*l) == 0 || !strings.Contains((*l)[0], "pod-uid-1") || !strings.Contains((*l)[0], "delete exactly that pod by hand") {
		t.Fatalf("%v log %v", err, *l)
	}
}

// saveBreaker makes the record directory unwritable once the pod exists.
type saveBreaker struct {
	*fakeRoot
	dir string
}

func (s *saveBreaker) Create(sid string) (string, error) {
	uid, err := s.fakeRoot.Create(sid)
	os.Chmod(s.dir, 0o500)
	return uid, err
}

// The daemon's lock is not held through the pod's create and load (which take minutes): another start
// goes through while a pod start waits for its pod.
func TestAPodStartDoesNotHoldTheDaemonsLockWhileItWaits(t *testing.T) {
	d, fr, _ := podDaemon(t)
	entered, free := make(chan struct{}), make(chan struct{})
	d.PodRoot = &blockingLoad{fakeRoot: fr, entered: entered, free: free}
	done := make(chan error, 1)
	go func() { _, err := d.Start(podSpec(nil)); done <- err }()
	<-entered
	got := make(chan error, 1)
	go func() {
		_, err := d.Start(spec(func(s *LaunchSpec) { s.RequestID, s.Label = "rid-7", "cc-sandbox:other" }))
		got <- err
	}()
	select {
	case err := <-got:
		if err != nil {
			t.Errorf("the second start: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Error("a start waited for a pod's load (the lock is held through it)")
	}
	close(free)
	if err := <-done; err != nil {
		t.Error(err)
	}
}

type blockingLoad struct {
	*fakeRoot
	entered, free chan struct{}
}

func (b *blockingLoad) Load(sid string) error { close(b.entered); <-b.free; return nil }

// Pods must not outlive their sessions or hold live slots.
func TestSweepPods(t *testing.T) {
	d, fr, revoked := podDaemon(t)
	l := logs(d)
	now := time.Now()
	d.Now = func() time.Time { return now }
	rep, err := d.Start(podSpec(nil))
	if err != nil {
		t.Fatal(err)
	}
	fr.calls = nil
	d.SweepPods(8 * time.Hour)
	if strings.Join(fr.calls, ",") != "gone" {
		t.Errorf("a fresh pod is only asked about: %v", fr.calls)
	}
	if rec, _ := d.Load(rep.Sid); rec.Ended != nil {
		t.Error("a live pod's record was ended")
	}
	// the owner stopped it: the pod is gone, the record ends and the token is dealt with
	fr.gone, fr.calls = true, nil
	d.SweepPods(8 * time.Hour)
	if rec, _ := d.Load(rep.Sid); rec.Ended == nil || strings.Join(fr.calls, ",") != "gone,stop" || len(*revoked) != 0 {
		t.Errorf("a gone pod: ended %v calls %v (pod-stop revokes it, hostd does not)", rec.Ended, fr.calls)
	}
	// past its token's life it is stopped, not asked about
	d2, fr2, rev2 := podDaemon(t)
	d2.Now = func() time.Time { return now }
	rep2, _ := d2.Start(podSpec(nil))
	d2.Now = func() time.Time { return now.Add(9 * time.Hour) }
	fr2.calls = nil
	d2.SweepPods(8 * time.Hour)
	if rec, _ := d2.Load(rep2.Sid); strings.Join(fr2.calls, ",") != "stop" || rec.Ended == nil || len(*rev2) != 1 {
		t.Errorf("an expired pod: calls %v ended %v revoked %v", fr2.calls, rec.Ended, *rev2)
	}
	// an expired pod that cannot be deleted stays live, loudly
	d3, fr3, _ := podDaemon(t)
	l3 := logs(d3)
	d3.Now = func() time.Time { return now }
	rep3, _ := d3.Start(podSpec(nil))
	d3.Now = func() time.Time { return now.Add(9 * time.Hour) }
	fr3.stopEr = errors.New("api down")
	d3.SweepPods(8 * time.Hour)
	if rec, _ := d3.Load(rep3.Sid); rec.Ended != nil || len(*l3) == 0 {
		t.Errorf("a pod that could not be deleted was ended (%v) or silent (%v)", rec.Ended, *l3)
	}
	// a start that never got a UID is ended after the grace period, naming the pod
	d4, _, _ := podDaemon(t)
	l4 := logs(d4)
	os.MkdirAll(filepath.Join(d4.StateDir, "sessions"), 0o700)
	rec := &Record{Sid: "s0abandone", Gen: 1, Backend: "k8s", Uid: 1000, Started: now.Add(-time.Hour), Spec: *podSpec(nil)}
	d4.save(rec)
	d4.Now = func() time.Time { return now }
	d4.SweepPods(8 * time.Hour)
	if r, _ := d4.Load("s0abandone"); r.Ended == nil || len(*l4) != 1 || !strings.Contains((*l4)[0], "ccsbx-s-s0abandone-g1") {
		t.Errorf("abandoned: %v %v", r.Ended, *l4)
	}
	_ = l
	// and a young one with no UID (a start in flight) is left alone
	d5, _, _ := podDaemon(t)
	os.MkdirAll(filepath.Join(d5.StateDir, "sessions"), 0o700)
	young := &Record{Sid: "s0inflight", Gen: 1, Backend: "k8s", Uid: 1000, Started: now.Add(-time.Minute), Spec: *podSpec(nil)}
	d5.save(young)
	d5.Now = func() time.Time { return now }
	d5.SweepPods(8 * time.Hour)
	if r, _ := d5.Load("s0inflight"); r.Ended != nil {
		t.Error("a start in flight was ended")
	}
}

// ---- resume of a pod session (F2x) ----

func resumeSpec(sid string, mod func(*LaunchSpec)) *LaunchSpec {
	return podSpec(func(s *LaunchSpec) {
		s.RequestID, s.Resume = "rid-resume", &Resume{Sid: sid}
		if mod != nil {
			mod(s)
		}
	})
}

// context "a pod session's next generation": the previous generation's pod is deleted first (by its UID),
// then a new pod is made and loaded for generation+1 of the SAME session.
func TestResumeOfAPodSessionIsTheNextGeneration(t *testing.T) {
	d, fr, revoked := podDaemon(t)
	first, err := d.Start(podSpec(nil))
	if err != nil {
		t.Fatal(err)
	}
	fr.calls, fr.uid = nil, "pod-uid-2"
	rep, err := d.Start(resumeSpec(first.Sid, nil))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(fr.calls, ",") != "stop,create,load" {
		t.Errorf("calls %v: the old pod goes first", fr.calls)
	}
	rec, _ := d.Load(first.Sid)
	if rep.Sid != first.Sid || rep.Gen != 2 || rep.ID != "tomas/future-two/"+first.Sid+"/g2" || rec.Gen != 2 || rec.PodUID != "pod-uid-2" || rec.Ended != nil ||
		rec.RequestID != "rid-resume" || len(rec.RequestIDs) != 1 || rec.RequestIDs[0] != "rid-1" || fr.uidOnDiskAtLoad != "pod-uid-2" {
		t.Errorf("%+v %+v", rep, rec)
	}
	if len(*revoked) != 1 {
		t.Errorf("the old generation's token was not dealt with: %v", *revoked)
	}
	if len(d.recordFiles(t)) != 1 {
		t.Errorf("a resume made a second record: %v", d.recordFiles(t))
	}
}

func TestResumeRefusals(t *testing.T) {
	for why, mod := range map[string]func(*LaunchSpec){
		"another label":      func(s *LaunchSpec) { s.Label = "cc-sandbox:other" },
		"another repository": func(s *LaunchSpec) { s.Workdir.Repo = "landovsky/other" },
	} {
		d, fr, _ := podDaemon(t)
		first, _ := d.Start(podSpec(nil))
		fr.calls = nil
		if _, err := d.Start(resumeSpec(first.Sid, mod)); err == nil || len(fr.calls) != 0 {
			t.Errorf("%s: %v calls %v (nothing may be touched)", why, err, fr.calls)
		}
		if rec, _ := d.Load(first.Sid); rec.Gen != 1 || rec.Ended != nil {
			t.Errorf("%s changed the record: %+v", why, rec)
		}
	}
	d, fr, _ := podDaemon(t)
	if _, err := d.Start(resumeSpec("s0unknownxx", nil)); err == nil || len(fr.calls) != 0 {
		t.Errorf("an unknown session: %v", err)
	}
	// a docker session is not resumed as a pod, and another caller's session is not resumed at all
	d, fr, _ = podDaemon(t)
	dock, _ := d.Start(spec(nil))
	if _, err := d.Start(resumeSpec(dock.Sid, nil)); err == nil || len(fr.calls) != 0 {
		t.Errorf("a docker session: %v", err)
	}
	d, fr, _ = podDaemon(t)
	first, _ := d.Start(podSpec(nil))
	d.Uid = 2000
	fr.calls = nil
	if _, err := d.Start(resumeSpec(first.Sid, nil)); err == nil || len(fr.calls) != 0 {
		t.Errorf("another caller's session: %v", err)
	}
}

// the old pod that cannot be deleted stops the resume with the session as it was
func TestResumeWhenTheOldPodCannotBeDeletedChangesNothing(t *testing.T) {
	d, fr, _ := podDaemon(t)
	first, _ := d.Start(podSpec(nil))
	fr.calls, fr.stopEr = nil, errors.New("api down")
	_, err := d.Start(resumeSpec(first.Sid, nil))
	if code(err) != SessionBusy || strings.Join(fr.calls, ",") != "stop" {
		t.Errorf("%v calls %v", err, fr.calls)
	}
	if rec, _ := d.Load(first.Sid); rec.Gen != 1 || rec.Ended != nil || rec.PodUID != "pod-uid-1" {
		t.Errorf("%+v", rec)
	}
}

// a generation that fails to come up keeps its own number, ended: the doors refuse every token at or below a
// revoked generation, so giving the number back would make the next resume a generation that is dead on arrival
func TestAFailedResumeNeverGivesTheGenerationNumberBack(t *testing.T) {
	d, fr, _ := podDaemon(t)
	first, _ := d.Start(podSpec(nil))
	fr.calls, fr.uid, fr.loadEr = nil, "pod-uid-2", ErrPodNotReady
	_, err := d.Start(resumeSpec(first.Sid, nil))
	if code(err) != PodNotReady || strings.Join(fr.calls, ",") != "stop,create,load,stop" {
		t.Fatalf("%v calls %v", err, fr.calls)
	}
	rec, _ := d.Load(first.Sid)
	if rec.Reply.Gen != 2 || !strings.HasSuffix(rec.Reply.ID, "/g2") {
		t.Errorf("the failed generation's full id names another generation: %d %s", rec.Reply.Gen, rec.Reply.ID)
	}
	if rec.Gen != 2 || rec.Ended == nil || rec.PodUID != "" || rec.Starting || rec.RequestID != "rid-1" {
		t.Errorf("the failed generation is kept as generation 2, ended, its pod gone, its request not answered as a success: %+v", rec)
	}
	fr.calls, fr.loadEr, fr.uid = nil, nil, "pod-uid-3"
	if rep, err := d.Start(resumeSpec(first.Sid, func(s *LaunchSpec) { s.RequestID = "rid-resume-2" })); err != nil || rep.Gen != 3 {
		t.Errorf("the next resume must be generation 3: %+v %v", rep, err)
	}
	// a retry of the FAILED request is a new resume, not the reply of a start that never came up
	d2, fr2, _ := podDaemon(t)
	f2, _ := d2.Start(podSpec(nil))
	fr2.loadEr = ErrPodNotReady
	d2.Start(resumeSpec(f2.Sid, nil))
	fr2.loadEr = nil
	if rep, err := d2.Start(resumeSpec(f2.Sid, nil)); err != nil || rep.Gen != 3 {
		t.Errorf("a retry of the failed request: %+v %v", rep, err)
	}
}

// a resume replaces a generation, it does not add a live session: MaxLive counts the session once
func TestAResumeDoesNotCountTwiceAgainstMaxLive(t *testing.T) {
	d, fr, _ := podDaemon(t)
	d.MaxLive = 1
	first, err := d.Start(podSpec(nil))
	if err != nil {
		t.Fatal(err)
	}
	fr.uid = "pod-uid-2"
	if _, err := d.Start(resumeSpec(first.Sid, nil)); err != nil {
		t.Errorf("the only live session could not be resumed: %v", err)
	}
}

// with the pod verbs revoking the token themselves, hostd does not log "revoke by hand" for a pod
func TestHostdSaysNothingAboutRevokingWhenThePodVerbsDoIt(t *testing.T) {
	d, fr, _ := podDaemon(t)
	d.Revoke = nil
	d.PodVerbsRevoke = true
	l := logs(d)
	fr.loadEr = errors.New("side load refused: bad key")
	if _, err := d.Start(podSpec(nil)); err == nil || len(*l) != 0 {
		t.Errorf("%v logged %v", err, *l)
	}
	d.PodVerbsRevoke = false
	d.Start(podSpec(func(s *LaunchSpec) { s.RequestID, s.Label = "rid-3", "cc-sandbox:c" }))
	if len(*l) != 1 {
		t.Errorf("without the verbs revoking, the by-hand line must come back: %v", *l)
	}
}

// ---- F2x review: a resume must not race a start that is still creating or loading ----

// the first start is blocked inside Load (the pod exists); a resume of the same session is refused, touches
// nothing, and the start then finishes undisturbed
func TestAResumeIsRefusedWhileTheSessionIsStillStarting(t *testing.T) {
	d, fr, _ := podDaemon(t)
	entered, free := make(chan struct{}), make(chan struct{})
	d.PodRoot = &blockingLoad{fakeRoot: fr, entered: entered, free: free}
	done := make(chan *StartReply, 1)
	go func() { rep, _ := d.Start(podSpec(nil)); done <- rep }()
	<-entered
	files := d.recordFiles(t)
	if len(files) != 1 {
		t.Fatalf("records %v", files)
	}
	sid := strings.TrimSuffix(files[0], ".json")
	fr.calls = nil
	_, err := d.Start(resumeSpec(sid, nil))
	if code(err) != SessionBusy || len(fr.calls) != 0 {
		t.Errorf("a resume during the load: %v calls %v", err, fr.calls)
	}
	close(free)
	rep := <-done
	if rep == nil {
		t.Fatal("the start failed")
	}
	if rec, _ := d.Load(sid); rec.Gen != 1 || rec.Starting || rec.PodUID != "pod-uid-1" || rec.Ended != nil {
		t.Errorf("the start was disturbed: %+v", rec)
	}
}

// and during the create, before any UID is recorded
func TestAResumeIsRefusedBetweenTheFirstSaveAndTheCreate(t *testing.T) {
	d, fr, _ := podDaemon(t)
	entered, free := make(chan struct{}), make(chan struct{})
	d.PodRoot = &blockingCreate{fakeRoot: fr, entered: entered, free: free}
	done := make(chan error, 1)
	go func() { _, err := d.Start(podSpec(nil)); done <- err }()
	<-entered
	files := d.recordFiles(t)
	sid := strings.TrimSuffix(files[0], ".json")
	if _, err := d.Start(resumeSpec(sid, nil)); code(err) != SessionBusy {
		t.Errorf("a resume before the create: %v", err)
	}
	close(free)
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if rec, _ := d.Load(sid); rec.Gen != 1 || rec.PodUID != "pod-uid-1" {
		t.Errorf("%+v", rec)
	}
}

type blockingCreate struct {
	*fakeRoot
	entered, free chan struct{}
}

func (b *blockingCreate) Create(sid string) (string, error) {
	close(b.entered)
	<-b.free
	return b.fakeRoot.Create(sid)
}

// the cleanup of a failed start acts on its own generation only: if the record has moved on (a stop, a sweep,
// anything), it neither deletes the pod nor removes or ends the record
func TestTheCleanupOfAFailedStartLeavesAMovedOnRecordAlone(t *testing.T) {
	d, fr, _ := podDaemon(t)
	l := logs(d)
	var sid string
	d.PodRoot = &moveOnLoad{fakeRoot: fr, move: func(s string) {
		sid = s
		rec, _ := d.Load(s)
		rec.Gen, rec.PodUID = 2, "someone-elses-pod" // another generation, made by somebody else, meanwhile
		d.save(rec)
	}}
	fr.loadEr = errors.New("side load refused: bad key")
	if _, err := d.Start(podSpec(nil)); err == nil {
		t.Fatal("the start should have failed")
	}
	for _, c := range fr.calls {
		if c == "stop" {
			t.Errorf("the cleanup deleted a pod of a generation that is not its own: %v", fr.calls)
		}
	}
	if rec, err := d.Load(sid); err != nil || rec.Gen != 2 || rec.PodUID != "someone-elses-pod" || rec.Ended != nil {
		t.Errorf("the record of the other generation was touched: %+v %v", rec, err)
	}
	if len(*l) == 0 || !strings.Contains(strings.Join(*l, "\n"), "no longer that generation's") {
		t.Errorf("nothing said: %v", *l)
	}
}

type moveOnLoad struct {
	*fakeRoot
	move func(sid string)
}

func (m *moveOnLoad) Load(sid string) error {
	err := m.fakeRoot.Load(sid)
	m.move(sid)
	return err
}

// S2: an ended record that still names a pod (it could not be deleted) is stopped by a resume, not overwritten
func TestAResumeOfAnEndedRecordStillStopsThePodItNames(t *testing.T) {
	d, fr, _ := podDaemon(t)
	first, _ := d.Start(podSpec(nil))
	rec, _ := d.Load(first.Sid)
	now := time.Now()
	rec.Ended = &now
	d.save(rec) // ended, but PodUID still names the pod
	fr.calls = nil
	if _, err := d.Start(resumeSpec(first.Sid, nil)); err != nil || strings.Join(fr.calls, ",") != "stop,create,load" {
		t.Errorf("%v calls %v", err, fr.calls)
	}
	// and when that stop fails the resume is refused with the record untouched
	d2, fr2, _ := podDaemon(t)
	f2, _ := d2.Start(podSpec(nil))
	r2, _ := d2.Load(f2.Sid)
	r2.Ended = &now
	d2.save(r2)
	fr2.stopEr = errors.New("api down")
	if _, err := d2.Start(resumeSpec(f2.Sid, nil)); code(err) != SessionBusy {
		t.Errorf("%v", err)
	}
	if r, _ := d2.Load(f2.Sid); r.PodUID != "pod-uid-1" || r.Gen != 1 {
		t.Errorf("the pod's name was lost: %+v", r)
	}
}

// S3: a resume is a start: resuming in a loop is rate-capped like starting in a loop
func TestResumesCountAgainstMaxPerHour(t *testing.T) {
	d, fr, _ := podDaemon(t)
	d.MaxPerHour = 3
	first, err := d.Start(podSpec(nil))
	if err != nil {
		t.Fatal(err)
	}
	var last error
	for i := 0; i < 4; i++ {
		fr.uid = "pod-uid-x"
		_, last = d.Start(resumeSpec(first.Sid, func(s *LaunchSpec) { s.RequestID = "rid-r" + string(rune('a'+i)) }))
		if last != nil {
			break
		}
	}
	if code(last) != RateCapped {
		t.Errorf("the loop was never capped: %v", last)
	}
	if rec, _ := d.Load(first.Sid); rec.Gen != 3 {
		t.Errorf("two resumes were allowed (3 starts in the hour): generation %d", rec.Gen)
	}
}

// S4: a pod that vanished outside pod-stop has its token revoked by pod-stop (which accepts an absent pod)
func TestAPodThatVanishedIsStoppedSoItsTokenIsRevoked(t *testing.T) {
	d, fr, _ := podDaemon(t)
	l := logs(d)
	rep, _ := d.Start(podSpec(nil))
	fr.gone, fr.calls = true, nil
	d.SweepPods(8 * time.Hour)
	if strings.Join(fr.calls, ",") != "gone,stop" {
		t.Errorf("calls %v", fr.calls)
	}
	if rec, _ := d.Load(rep.Sid); rec.Ended == nil {
		t.Error("the record was not ended")
	}
	// when the revoke (pod-stop) fails, the record stays and the line says what to revoke by hand
	d2, fr2, _ := podDaemon(t)
	l2 := logs(d2)
	rep2, _ := d2.Start(podSpec(nil))
	fr2.gone, fr2.stopEr = true, errors.New("signer down")
	d2.SweepPods(8 * time.Hour)
	if rec, _ := d2.Load(rep2.Sid); rec.Ended != nil || len(*l2) == 0 || !strings.Contains((*l2)[0], "by hand") {
		t.Errorf("ended %v log %v", rec.Ended, *l2)
	}
	_ = l
}

// a start that hostd died in the middle of (Starting, a pod recorded) is stopped by the sweep after the grace period,
// and one that is merely in flight is left alone
func TestTheSweepStopsAStuckStartAndSparesAnInFlightOne(t *testing.T) {
	d, fr, _ := podDaemon(t)
	now := time.Now()
	d.Now = func() time.Time { return now }
	os.MkdirAll(filepath.Join(d.StateDir, "sessions"), 0o700)
	stuck := &Record{Sid: "s0stuckone", Gen: 1, Backend: "k8s", Uid: 1000, Started: now.Add(-time.Hour), Starting: true, PodUID: "u1", Spec: *podSpec(nil)}
	young := &Record{Sid: "s0younggg1", Gen: 1, Backend: "k8s", Uid: 1000, Started: now.Add(-time.Minute), Starting: true, PodUID: "u2", Spec: *podSpec(nil)}
	d.save(stuck)
	d.save(young)
	d.SweepPods(8 * time.Hour)
	if r, _ := d.Load("s0stuckone"); r.Ended == nil {
		t.Error("a stuck start was left running")
	}
	if r, _ := d.Load("s0younggg1"); r.Ended != nil {
		t.Error("a start in flight was ended")
	}
	if strings.Contains(strings.Join(fr.calls, ","), "gone") && len(fr.calls) > 3 {
		t.Errorf("calls %v", fr.calls)
	}
}

// the sidecar's own rules for the repository and branch are checked at the start (BadSpec), not found later
func TestAPodStartNeedsAGoodRepositoryAndBranch(t *testing.T) {
	for why, mod := range map[string]func(*LaunchSpec){
		"no branch":                  func(s *LaunchSpec) { s.Workdir.Branch = "" },
		"the branch main":            func(s *LaunchSpec) { s.Workdir.Branch = "main" },
		"the branch HEAD":            func(s *LaunchSpec) { s.Workdir.Branch = "HEAD" },
		"a branch of 101 chars":      func(s *LaunchSpec) { s.Workdir.Branch = strings.Repeat("a", 101) },
		"a branch that is an option": func(s *LaunchSpec) { s.Workdir.Branch = "--upload-pack=x" },
		"a repository that is a URL": func(s *LaunchSpec) { s.Workdir.Repo = "https://x/y" },
		"a base that is an option":   func(s *LaunchSpec) { s.Workdir.Base = "--x" },
	} {
		d, fr, _ := podDaemon(t)
		if _, err := d.Start(podSpec(mod)); code(err) != BadSpec || len(fr.calls) != 0 || len(d.recordFiles(t)) != 0 {
			t.Errorf("%s: %v calls %v", why, err, fr.calls)
		}
	}
}

// the limit on a start in flight follows what the verbs may take (hostd sets it from --pod-load-wait), not a constant
func TestTheStartingLimitIsConfigurable(t *testing.T) {
	d, fr, _ := podDaemon(t)
	first, _ := d.Start(podSpec(nil))
	rec, _ := d.Load(first.Sid)
	rec.Starting = true
	rec.Started = time.Now().Add(-5 * time.Minute) // in flight for five minutes
	d.save(rec)
	fr.calls = nil
	if _, err := d.Start(resumeSpec(first.Sid, nil)); code(err) != SessionBusy {
		t.Errorf("under the default 15 minutes a five-minute-old start is still in flight: %v", err)
	}
	d.StartLimit = 3 * time.Minute // the verbs may take three minutes at most: five means it is stuck
	if _, err := d.Start(resumeSpec(first.Sid, nil)); err != nil {
		t.Errorf("past the configured limit the resume must go ahead: %v", err)
	}
	d.StartLimit = 20 * time.Minute
	r2, _ := d.Load(first.Sid)
	r2.Starting, r2.Started, r2.Ended = true, time.Now().Add(-16*time.Minute), nil
	d.save(r2)
	if _, err := d.Start(resumeSpec(first.Sid, func(s *LaunchSpec) { s.RequestID = "rid-late" })); code(err) != SessionBusy {
		t.Errorf("a longer limit keeps a 16-minute-old start in flight: %v", err)
	}
}

// the delete of a failed start's pod does not hold the daemon's lock: another start goes through meanwhile
func TestTheCleanupOfAFailedStartDoesNotHoldTheDaemonsLockWhileDeleting(t *testing.T) {
	d, fr, _ := podDaemon(t)
	fr.loadEr = errors.New("side load refused: bad key")
	answered := make(chan error, 1)
	d.PodRoot = &stopHook{fakeRoot: fr, during: func() {
		go func() {
			_, err := d.Start(spec(func(s *LaunchSpec) { s.RequestID, s.Label = "rid-9", "cc-sandbox:other" }))
			answered <- err
		}()
		select {
		case err := <-answered:
			if err != nil {
				t.Errorf("the other start: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Error("another start waited for a pod's delete (the lock is held through it)")
			// (that goroutine stays blocked on the lock for good; the test must not wait for it)
		}
	}}
	d.Start(podSpec(nil))
}

type stopHook struct {
	*fakeRoot
	during func()
}

func (s *stopHook) Stop(sid string, gen int) error { s.during(); return s.fakeRoot.Stop(sid, gen) }

// Gate M: pods are far heavier than docker sessions, so they have a cap of their own
func TestPodSessionsHaveACapOfTheirOwn(t *testing.T) {
	d, _, _ := podDaemon(t)
	d.PodMaxLive = 2
	for i := 0; i < 2; i++ {
		if _, err := d.Start(podSpec(func(s *LaunchSpec) {
			s.RequestID, s.Label = "rid-p"+string(rune('a'+i)), "cc-sandbox:p"+string(rune('a'+i))
		})); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := d.Start(podSpec(func(s *LaunchSpec) { s.RequestID, s.Label = "rid-pz", "cc-sandbox:pz" })); code(err) != MachineFull {
		t.Errorf("a third pod: %v", err)
	}
	// docker sessions are not held to it
	if _, err := d.Start(spec(func(s *LaunchSpec) { s.RequestID, s.Label = "rid-d", "cc-sandbox:docker" })); err != nil {
		t.Errorf("a docker session: %v", err)
	}
}

// N6: the unlocked cleanup tells pod-stop which generation it means: every pod-stop of a start names its own
func TestEveryPodStopNamesItsGeneration(t *testing.T) {
	d, fr, _ := podDaemon(t)
	fr.loadEr = ErrPodNotReady
	d.Start(podSpec(nil))
	if len(fr.stopGens) != 1 || fr.stopGens[0] != 1 {
		t.Errorf("the cleanup's pod-stop named generations %v", fr.stopGens)
	}
	d2, fr2, _ := podDaemon(t)
	first, _ := d2.Start(podSpec(nil))
	fr2.stopGens = nil
	fr2.uid = "pod-uid-2"
	d2.Start(resumeSpec(first.Sid, nil))
	d2.StopPod(first.Sid)
	if len(fr2.stopGens) != 2 || fr2.stopGens[0] != 1 || fr2.stopGens[1] != 2 {
		t.Errorf("a resume stops generation 1, a stop generation 2: %v", fr2.stopGens)
	}
}

// root refused because the session moved on (another_generation): the cleanup touches nothing
func TestACleanupRefusedBecauseTheSessionMovedOnChangesNothing(t *testing.T) {
	d, fr, _ := podDaemon(t)
	l := logs(d)
	fr.loadEr = errors.New("side load refused: bad key")
	fr.stopEr = fmt.Errorf("%w: refused", ErrPodStale)
	if _, err := d.Start(podSpec(nil)); err == nil {
		t.Fatal("the start should fail")
	}
	files := d.recordFiles(t)
	if len(files) != 1 {
		t.Fatalf("records %v", files)
	}
	if rec, _ := d.Load(strings.TrimSuffix(files[0], ".json")); rec.Ended != nil || !rec.Starting {
		t.Errorf("the record was changed by a cleanup that root refused as stale: %+v", rec)
	}
	if !strings.Contains(strings.Join(*l, "\n"), "moved on") {
		t.Errorf("not said: %v", *l)
	}
}
