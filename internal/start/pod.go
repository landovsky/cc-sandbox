// pod.go is the start path for a session that runs as a pod on another machine (F2w): a spec whose
// machine is in Daemon.Placements is started through ccsbx-root's pod verbs instead of a pane and
// docker. hostd (uid ccsbx) holds no kubeconfig and no secrets: it writes the record, asks root to
// create the pod, writes the pod's UID into the record, and only then asks root to load the sidecar
// (the verb mints the token itself, as the keys user). Every failure after the pod exists deletes it
// by name and UID, so a start either ends with a loaded pod or with no pod and no live record.
package start

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/pins"
)

// PodRoot runs the pod verbs. Production is SudoPodRoot; tests pass a fake.
type PodRoot interface {
	// Create makes the generation's pod and returns its UID (nothing secret is sent).
	Create(sid string) (uid string, err error)
	// Load waits for the pod and loads the sidecar; ErrPodNotReady if the pod never came up (nothing was minted).
	Load(sid string) error
	// Stop deletes the pod by name and the recorded UID; an absent pod is fine.
	Stop(sid string) error
}

// Errors the pod verbs' exit codes map to.
var (
	ErrPodNotReady = errors.New("pod_not_ready")
	ErrPodConflict = errors.New("pod_conflict")
)

// SudoPodRoot asks `sudo -n ccsbx-root pod-<verb> -- <sid>`.
type SudoPodRoot struct {
	// LoadWait bounds pod-load (the verb's own deadline plus loading), default 12 minutes.
	LoadWait time.Duration
}

func (r SudoPodRoot) run(verb, sid string, wait time.Duration) ([]byte, error) {
	if !ValidSid(sid) {
		return nil, fmt.Errorf("%q is not a session id", sid)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	c := exec.CommandContext(ctx, SudoBin, "-n", RootBin, verb, "--", sid)
	c.WaitDelay = time.Second
	c.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	err := c.Run()
	if err == nil {
		return out.Bytes(), nil
	}
	msg := strings.TrimSpace(errb.String())
	if len(msg) > 400 {
		msg = msg[:400]
	}
	var ee *exec.ExitError
	if errors.As(err, &ee) {
		switch {
		case ee.ExitCode() == 4:
			return nil, fmt.Errorf("%w: %s", ErrPodNotReady, msg)
		case ee.ExitCode() == 2 && strings.Contains(msg, "pod_conflict"):
			return nil, fmt.Errorf("%w: %s", ErrPodConflict, msg)
		}
	}
	return nil, fmt.Errorf("ccsbx-root %s: %v: %s", verb, err, msg)
}

func (r SudoPodRoot) Create(sid string) (string, error) {
	out, err := r.run("pod-create", sid, 90*time.Second)
	if err != nil {
		return "", err
	}
	var v struct{ UID string }
	if json.Unmarshal(out, &v) != nil || v.UID == "" {
		return "", fmt.Errorf("pod-create: no UID in its answer")
	}
	return v.UID, nil
}

func (r SudoPodRoot) Load(sid string) error {
	w := r.LoadWait
	if w <= 0 {
		w = 12 * time.Minute
	}
	_, err := r.run("pod-load", sid, w)
	return err
}

func (r SudoPodRoot) Stop(sid string) error {
	_, err := r.run("pod-stop", sid, 90*time.Second)
	return err
}

// startPod is Start for a pod session. The checks that need no cluster are the docker path's (profile
// for the machine, billing, a pinned image, caps, the label); what a pod cannot do yet is refused.
func (d *Daemon) startPod(s *LaunchSpec, recs []*Record) (*StartReply, error) {
	if d.PodRoot == nil {
		return nil, refuse(NotBuilt, "machine %s is a pod machine but this daemon has no pod launcher", s.Machine)
	}
	if s.Resume != nil {
		return nil, refuse(NotBuilt, "resuming a pod session needs the sidecar's journal and clone (F3c)")
	}
	if s.Workdir.Path != "" || s.GitDir != "" {
		return nil, refuse(BadSpec, "a pod has no host directory to mount: give workdir.repo, not workdir.path")
	}
	if s.Workdir.Repo == "" {
		return nil, refuse(BadSpec, "workdir.repo is required for a pod session")
	}
	p, err := d.LoadProfile(s.Profile)
	if err != nil {
		return nil, refuse(ProfileNotApproved, "profile %q: %v", s.Profile, err)
	}
	machineOK, billingOK := p.Allows(s.Machine, s.Billing)
	if !machineOK {
		return nil, refuse(ProfileNotApproved, "profile %s is not approved for %s", p.Name, s.Machine)
	}
	if s.Billing == "" {
		return nil, refuse(BillingUnavailable, "billing is required (api|subscription|stub)")
	}
	if !billingOK {
		return nil, refuse(BillingUnavailable, "profile %s does not allow billing %q", p.Name, s.Billing)
	}
	pl, err := pins.Load(d.PinsFile)
	if err != nil {
		return nil, fmt.Errorf("pins: %w", err)
	}
	channel, err := pins.Check(pl, nil, d.Image, p, d.Root != "helper")
	if err != nil {
		return nil, refuse(ImageNotPinned, "%v", err)
	}
	live, started := 0, 0
	for _, r := range recs {
		if r.Backend == "k8s" && r.Ended == nil {
			live++
			if r.Spec.Label == s.Label {
				return nil, refuse(AlreadyRunning, "%s already_running on %s (sid %s)", s.Label, r.Spec.Machine, r.Sid)
			}
		}
		if r.Uid == d.Uid && d.now().Sub(r.Started) < time.Hour {
			started++
		}
	}
	if d.MaxPerHour > 0 && started >= d.MaxPerHour {
		return nil, refuse(RateCapped, "%d starts in the last hour (cap %d)", started, d.MaxPerHour)
	}
	if d.MaxLive > 0 && live >= d.MaxLive {
		return nil, refuse(MachineFull, "%d live sessions (cap %d)", live, d.MaxLive)
	}
	sid, err := mintSid()
	if err != nil {
		return nil, err
	}
	rec := &Record{Sid: sid, Gen: 1, Label: s.Label, RequestID: s.RequestID, Uid: d.Uid, Started: d.now().UTC(), Spec: *s, Backend: "k8s"}
	rec.Reply = StartReply{
		V: 1, Sid: sid, Gen: 1, ID: fmt.Sprintf("%s/%s/%s/g%d", s.Owner, s.Machine, sid, 1), Backend: "k8s",
		Pin:     Pin{CC: orUnpinned(d.CCVersion), Image: orUnpinned(d.Image), Channel: channel},
		Attach:  Attach{Local: []string{"ccsbx", "attach", sid}, Remote: []string{"ssh", "-t", d.Machine, "ccsbx", "attach", sid}},
		Journal: Journal{Epoch: d.epoch(), Seq: len(recs)},
		NotEv:   !(d.Root == "helper" && d.HelperProven),
	}
	// 1. the record, without a pod: root reads it
	if err := d.save(rec); err != nil {
		return nil, err
	}
	abandon := func(cause error, code string) (*StartReply, error) {
		// Delete whatever pod exists, by the recorded UID. If that fails the record stays (ended), so a
		// later stop or gc can retry; otherwise the start leaves no trace and a retry is a fresh start.
		if rec.PodUID != "" {
			if err := d.PodRoot.Stop(sid); err != nil {
				d.markEnded(rec)
				return nil, fmt.Errorf("%s: %v (and the pod could not be deleted: %v; it is recorded as ended, retry `ccsbx-root pod-stop %s`)", code, cause, err, sid)
			}
		}
		_ = os.Remove(d.path("sessions", sid+".json"))
		if errors.Is(cause, ErrPodConflict) {
			return nil, refuse(PodConflict, "%v", cause)
		}
		return nil, refuse(code, "%v", cause)
	}
	// 2. the pod; its UID goes into the record BEFORE anything secret is sent
	uid, err := d.PodRoot.Create(sid)
	if err != nil {
		return abandon(err, PodLoadFailed)
	}
	rec.PodUID = uid
	if err := d.save(rec); err != nil {
		// the pod exists and the record cannot say so: delete it by the UID we hold; the record has no UID
		// to give root, so root cannot, and the owner is told the name
		return nil, fmt.Errorf("pod created but its UID could not be recorded (%v): delete pod %s by hand", err, sid)
	}
	// 3. wait and load: root mints the token in its own process and loads the sidecar
	if err := d.PodRoot.Load(sid); err != nil {
		if !errors.Is(err, ErrPodNotReady) && d.Revoke != nil {
			d.Revoke(sid, rec.Gen) // a token may exist
		}
		if errors.Is(err, ErrPodNotReady) {
			return abandon(err, PodNotReady)
		}
		return abandon(err, PodLoadFailed)
	}
	rep := rec.Reply
	return &rep, nil
}

// StopPod ends a pod session's generation: the pod is deleted by name and recorded UID, the token is
// revoked and the record is marked ended. It is what a stop and a generation's expiry call.
func (d *Daemon) StopPod(sid string) error {
	if d.PodRoot == nil {
		return errors.New("no pod launcher")
	}
	rec, err := d.Load(sid)
	if err != nil {
		return err
	}
	if rec.Backend != "k8s" {
		return fmt.Errorf("session %s is not a pod session", sid)
	}
	if rec.PodUID != "" {
		if err := d.PodRoot.Stop(sid); err != nil {
			return err
		}
	}
	if d.Revoke != nil {
		d.Revoke(sid, rec.Gen)
	}
	if rec.Ended == nil {
		d.markEnded(rec)
	}
	return nil
}
