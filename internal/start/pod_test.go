// Tests for the pod start path (F2w): a spec for a pod machine is started through the pod verbs, the
// pod's UID is on disk before anything secret can be sent, and every failure after the pod exists ends
// with the pod deleted by that UID. The verbs are a fake; the real ones are tested in internal/rootcmd.
package start

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type fakeRoot struct {
	d               *Daemon
	calls           []string
	uid             string
	createEr        error
	loadEr          error
	stopEr          error
	uidOnDiskAtLoad string
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
