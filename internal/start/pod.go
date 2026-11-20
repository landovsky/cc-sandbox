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
	"syscall"
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
	// Gone says whether the generation's pod no longer exists (read-only; the owner's own `stop` leaves
	// the record to be reconciled by the sweep).
	Gone(sid string) (bool, error)
}

// Errors the pod verbs' exit codes map to.
var (
	ErrPodNotReady = errors.New("pod_not_ready")
	ErrPodConflict = errors.New("pod_conflict")
	// ErrPodRefused: the verb refused by policy (exit 2) before anything was created.
	ErrPodRefused = errors.New("pod_refused")
)

// SudoPodRoot asks `sudo -n ccsbx-root pod-<verb> -- <sid>`.
type SudoPodRoot struct {
	// LoadWait bounds pod-load; default is the verb's own worst case (600 s ready deadline, 30 s of load
	// retries, 90 s of API) plus 30 s, so hostd never gives up on a verb that is still within its limits.
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
	// a timeout must reach the verb: SIGKILL to sudo cannot be passed on (the verb would go on, and
	// might still mint), SIGTERM is relayed
	c.Cancel = func() error { return c.Process.Signal(syscall.SIGTERM) }
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
		case ee.ExitCode() == 2:
			return nil, fmt.Errorf("%w: %s", ErrPodRefused, msg)
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
		w = 12*time.Minute + 30*time.Second
	}
	_, err := r.run("pod-load", sid, w)
	return err
}

func (r SudoPodRoot) Gone(sid string) (bool, error) {
	out, err := r.run("pod-status", sid, 60*time.Second)
	if err != nil {
		return false, err
	}
	switch strings.TrimSpace(string(out)) {
	case "present":
		return false, nil
	case "absent":
		return true, nil
	}
	return false, fmt.Errorf("pod-status: unexpected answer %q", strings.TrimSpace(string(out)))
}

func (r SudoPodRoot) Stop(sid string) error {
	_, err := r.run("pod-stop", sid, 90*time.Second)
	return err
}

// startPod is Start for a pod session. The checks that need no cluster are the docker path's (profile
// for the machine, billing, a pinned image, caps, the label); what a pod cannot do yet is refused.
func (d *Daemon) startPod(s *LaunchSpec, recs []*Record, release func()) (*StartReply, error) {
	if d.PodRoot == nil {
		return nil, refuse(NotBuilt, "machine %s is a pod machine but this daemon has no pod launcher", s.Machine)
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
	// a resume is the next generation of an existing pod session: the same session, owner, label, machine and
	// repository, a new pod (the old generation's pod is deleted first)
	var old *Record
	if s.Resume != nil {
		var err error
		if old, err = d.Load(s.Resume.Sid); err != nil {
			return nil, refuse(BadSpec, "resume: no session %q", s.Resume.Sid)
		}
		switch {
		case old.Backend != "k8s":
			return nil, refuse(BadSpec, "resume: session %s is not a pod session", old.Sid)
		case old.Uid != d.Uid:
			return nil, refuse(BadSpec, "resume: session %s belongs to another caller", old.Sid)
		case old.Label != s.Label:
			return nil, refuse(BadSpec, "resume: session %s has label %q, not %q (a resume keeps its label)", old.Sid, old.Label, s.Label)
		case old.Spec.Machine != s.Machine:
			return nil, refuse(BadSpec, "resume: session %s is on %s, not %s", old.Sid, old.Spec.Machine, s.Machine)
		case old.Spec.Workdir.Repo != s.Workdir.Repo:
			return nil, refuse(BadSpec, "resume: the repository must be the same (%s)", old.Spec.Workdir.Repo)
		case old.Gen >= 9999:
			return nil, refuse(BadSpec, "resume: session %s is at its last generation", old.Sid)
		}
	}
	live, started := 0, 0
	for _, r := range recs {
		if old != nil && r.Sid == old.Sid {
			continue // the generation being resumed is replaced, not added to
		}
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
	var prev *Record // set on resume: what the session record is put back to if the new generation fails
	var rec *Record
	if old != nil {
		// The previous generation goes first: its pod is deleted by name and UID, its token and relay dealt
		// with. If that fails the resume is refused and nothing has changed.
		if old.Ended == nil && old.PodUID != "" {
			if err := d.PodRoot.Stop(old.Sid); err != nil {
				return nil, refuse(SessionBusy, "resume: the previous generation's pod could not be deleted: %v", err)
			}
		}
		d.revokeOrSay(old.Sid, old.Gen)
		ended := d.now().UTC()
		pv := *old
		pv.Ended = &ended
		prev = &pv
		cp := *old
		rec = &cp
		rec.Gen++
		rec.RequestIDs = append(append([]string(nil), old.RequestIDs...), old.RequestID)
		rec.RequestID, rec.Started, rec.Ended, rec.Spec, rec.PodUID = s.RequestID, d.now().UTC(), nil, *s, ""
	} else {
		sid, err := mintSid()
		if err != nil {
			return nil, err
		}
		rec = &Record{Sid: sid, Gen: 1, Label: s.Label, RequestID: s.RequestID, Uid: d.Uid, Started: d.now().UTC(), Spec: *s, Backend: "k8s"}
	}
	sid := rec.Sid
	rec.Reply = StartReply{
		V: 1, Sid: sid, Gen: rec.Gen, ID: fmt.Sprintf("%s/%s/%s/g%d", s.Owner, s.Machine, sid, rec.Gen), Backend: "k8s",
		Pin:     Pin{CC: orUnpinned(d.CCVersion), Image: orUnpinned(d.Image), Channel: channel},
		Attach:  Attach{Local: []string{"ccsbx", "attach", sid}, Remote: []string{"ssh", "-t", d.Machine, "ccsbx", "attach", sid}},
		Journal: Journal{Epoch: d.epoch(), Seq: len(recs)},
		NotEv:   !(d.Root == "helper" && d.HelperProven),
	}
	// 1. the record, without a pod: root reads it. It is also the reservation (label, live count), so the
	// daemon's lock is released here: the create and the load can take minutes and must not block every
	// other start and replace.
	if err := d.save(rec); err != nil {
		return nil, err
	}
	release()
	abandon := func(cause error, code string) (*StartReply, error) {
		// Delete whatever pod exists, by the recorded UID. If that fails the record stays (ended), so a
		// later stop or gc can retry; otherwise the start leaves no trace and a retry is a fresh start.
		if rec.PodUID != "" {
			if err := d.PodRoot.Stop(sid); err != nil {
				d.markEnded(rec)
				return nil, fmt.Errorf("%s: %v (and the pod could not be deleted: %v; it is recorded as ended, retry `ccsbx-root pod-stop %s`)", code, cause, err, sid)
			}
		}
		if prev != nil {
			_ = d.save(prev) // the session as it was: its previous generation, ended
		} else {
			_ = os.Remove(d.path("sessions", sid+".json"))
		}
		if errors.Is(cause, ErrPodConflict) {
			return nil, refuse(PodConflict, "%v", cause)
		}
		return nil, refuse(code, "%v", cause)
	}
	// 2. the pod; its UID goes into the record BEFORE anything secret is sent
	uid, err := d.PodRoot.Create(sid)
	if err != nil {
		if errors.Is(err, ErrPodConflict) || errors.Is(err, ErrPodRefused) {
			return abandon(err, PodLoadFailed) // root refused: no pod was made
		}
		// The call may have got as far as the API (a lost answer, a timeout, unreadable output): a pod
		// may exist that no record names. Keep the record, ended, and say which pod to look for.
		d.logf("pod %s may exist (create failed: %v): the launcher's namespace holds it if so; delete exactly that pod by hand", PodName(sid, rec.Gen), err)
		d.markEnded(rec)
		return nil, fmt.Errorf("%s: pod create failed (%v); pod %s may exist, it is recorded as ended", PodLoadFailed, err, PodName(sid, rec.Gen))
	}
	rec.PodUID = uid
	if err := d.save(rec); err != nil {
		// the pod exists and the record cannot say so: root cannot delete it without the UID. Say exactly
		// which pod and what UID, and end the record (best effort) so it holds no slot.
		d.logf("pod %s exists with UID %s but the record could not be written (%v): delete exactly that pod by hand", PodName(sid, rec.Gen), uid, err)
		d.markEnded(rec)
		return nil, fmt.Errorf("pod %s was created (UID %s) but its UID could not be recorded: %v; delete it by hand", PodName(sid, rec.Gen), uid, err)
	}
	// 3. wait and load: root mints the token in its own process and loads the sidecar
	if err := d.PodRoot.Load(sid); err != nil {
		if !errors.Is(err, ErrPodNotReady) {
			d.revokeOrSay(sid, rec.Gen) // a token may exist
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
	unlock, err := d.lock()
	if err != nil {
		return err
	}
	defer unlock()
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
	d.revokeOrSay(sid, rec.Gen)
	if rec.Ended == nil {
		d.markEnded(rec)
	}
	return nil
}

// revokeOrSay revokes the generation's token, or, with no revoke client, logs which token may still
// be valid so the owner can revoke it by hand (it expires within k8s_token_hours in any case).
func (d *Daemon) revokeOrSay(sid string, gen int) {
	if d.Revoke != nil {
		d.Revoke(sid, gen)
		return
	}
	if d.PodVerbsRevoke {
		return
	}
	if d.Logf != nil {
		d.Logf("pod session %s generation %d: a token may exist and is not revoked (no revoke client): revoke sid %s gen %d by hand", sid, gen, sid, gen)
	}
}

// PodName is the pod's name for a generation (what the launcher derives; used in what we log).
func PodName(sid string, gen int) string { return fmt.Sprintf("ccsbx-s-%s-g%d", sid, gen) }

func (d *Daemon) logf(format string, a ...any) {
	if d.Logf != nil {
		d.Logf(format, a...)
	}
}

// abandonedAfter is how long a pod record may sit with no UID before the sweep gives up on it: a start
// that died between its first save and its create.
const abandonedAfter = 15 * time.Minute

// SweepPods keeps pods from outliving their sessions: a pod session past ttl (its token's life) is
// stopped (the pod deleted by UID, the relay and token dealt with, the record ended); one whose pod is
// already gone (the owner stopped it) is ended; one that never got a UID is ended after a grace period
// and logged. Without this a pod keeps running after its token expires and holds a live slot forever.
func (d *Daemon) SweepPods(ttl time.Duration) {
	if d.PodRoot == nil {
		return
	}
	recs, err := d.records()
	if err != nil {
		d.logf("pod sweep: %v", err)
		return
	}
	now := d.now()
	for _, r := range recs {
		if r.Backend != "k8s" || r.Ended != nil {
			continue
		}
		age := now.Sub(r.Started)
		switch {
		case r.PodUID == "":
			if age > abandonedAfter {
				d.logf("pod session %s never recorded a pod UID: pod %s may exist, delete exactly that pod by hand; the record is ended", r.Sid, PodName(r.Sid, r.Gen))
				d.endRecord(r.Sid)
			}
		case age > ttl:
			if err := d.StopPod(r.Sid); err != nil {
				d.logf("pod sweep: stopping %s (expired): %v", r.Sid, err)
			}
		default:
			if gone, err := d.PodRoot.Gone(r.Sid); err != nil {
				d.logf("pod sweep: %s: %v", r.Sid, err)
			} else if gone {
				d.revokeOrSay(r.Sid, r.Gen)
				d.endRecord(r.Sid)
			}
		}
	}
}

// endRecord marks one session's record ended under the daemon's lock (the sweep races Start's saves).
func (d *Daemon) endRecord(sid string) {
	unlock, err := d.lock()
	if err != nil {
		return
	}
	defer unlock()
	if r, err := d.Load(sid); err == nil && r.Ended == nil {
		d.markEnded(r)
	}
}
