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
func (f *fakeRoot) Stop(sid string) error { f.calls = append(f.calls, "stop"); return f.stopEr }
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
		s.Machine, s.Workdir = "future-two", Workdir{Repo: "landovsky/cc-sandbox"}
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
	if rec, _ := d.Load(rep.Sid); rec.Ended == nil || len(*revoked) != 1 {
		t.Errorf("a gone pod: ended %v revoked %v", rec.Ended, *revoked)
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

// a new generation that fails to come up leaves the session as it was: the previous generation, ended,
// with its own UID; and the new pod that was made is deleted
func TestAFailedResumePutsTheSessionBack(t *testing.T) {
	d, fr, _ := podDaemon(t)
	first, _ := d.Start(podSpec(nil))
	fr.calls, fr.uid, fr.loadEr = nil, "pod-uid-2", ErrPodNotReady
	_, err := d.Start(resumeSpec(first.Sid, nil))
	if code(err) != PodNotReady || strings.Join(fr.calls, ",") != "stop,create,load,stop" {
		t.Fatalf("%v calls %v", err, fr.calls)
	}
	rec, _ := d.Load(first.Sid)
	if rec.Gen != 1 || rec.Ended == nil || rec.PodUID != "pod-uid-1" || rec.RequestID != "rid-1" {
		t.Errorf("the session was not put back: %+v", rec)
	}
	// and it can be resumed again
	fr.calls, fr.loadEr = nil, nil
	if rep, err := d.Start(resumeSpec(first.Sid, func(s *LaunchSpec) { s.RequestID = "rid-resume-2" })); err != nil || rep.Gen != 2 {
		t.Errorf("a second try: %+v %v", rep, err)
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
