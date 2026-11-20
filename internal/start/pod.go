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
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/gitname"
	"github.com/landovsky/cc-sandbox/internal/pins"
)

// PodRoot runs the pod verbs. Production is SudoPodRoot; tests pass a fake.
type PodRoot interface {
	// Create makes the generation's pod and returns its UID (nothing secret is sent).
	Create(sid string) (uid string, err error)
	// Load waits for the pod and loads the sidecar; ErrPodNotReady if the pod never came up (nothing was minted).
	Load(sid string) error
	// Stop deletes the pod by name and the recorded UID; an absent pod is fine.
	Stop(sid string, gen int) error
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
	// ErrPodStale: pod-stop was told a generation and the session has moved on: nothing was stopped.
	ErrPodStale = errors.New("another_generation")
)

// DefaultLoadWait is how long hostd waits for pod-load by default: the verb's own worst case (a 600 s ready
// deadline, 30 s of load retries, 90 s of API) plus 30 s.
const DefaultLoadWait = 12*time.Minute + 30*time.Second

// SudoPodRoot asks `sudo -n ccsbx-root pod-<verb> -- <sid>`.
type SudoPodRoot struct {
	// LoadWait bounds pod-load; default is the verb's own worst case (600 s ready deadline, 30 s of load
	// retries, 90 s of API) plus 30 s, so hostd never gives up on a verb that is still within its limits.
	LoadWait time.Duration
}

func (r SudoPodRoot) run(verb, sid string, wait time.Duration, extra ...string) ([]byte, error) {
	if !ValidSid(sid) {
		return nil, fmt.Errorf("%q is not a session id", sid)
	}
	ctx, cancel := context.WithTimeout(context.Background(), wait)
	defer cancel()
	c := exec.CommandContext(ctx, SudoBin, append([]string{"-n", RootBin, verb, "--", sid}, extra...)...)
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
		case ee.ExitCode() == 2 && strings.Contains(msg, "another_generation"):
			return nil, fmt.Errorf("%w: %s", ErrPodStale, msg)
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
		w = DefaultLoadWait
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

func (r SudoPodRoot) Stop(sid string, gen int) error {
	var extra []string
	if gen > 0 {
		extra = []string{strconv.Itoa(gen)}
	}
	_, err := r.run("pod-stop", sid, 90*time.Second, extra...)
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
	// the sidecar's own rules for the repository, branch and base (one definition, internal/gitname): a name
	// it would refuse is refused here, not found minutes later as a pod that never gets ready
	if err := gitname.Pair(s.Workdir.Repo, s.Workdir.Base, s.Workdir.Branch); err != nil {
		return nil, refuse(BadSpec, "workdir: %v", err)
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
		case (old.Starting || (old.Ended == nil && old.PodUID == "")) && d.now().Sub(old.Started) < d.startingLimit():
			return nil, refuse(SessionBusy, "resume: a start of session %s (generation %d) is still in progress", old.Sid, old.Gen)
		}
	}
	live, started := 0, 0
	for _, r := range recs {
		resuming := old != nil && r.Sid == old.Sid // the generation being resumed is replaced in the live count, not added to
		if r.Backend == "k8s" && r.Ended == nil && !resuming {
			live++
			if r.Spec.Label == s.Label {
				return nil, refuse(AlreadyRunning, "%s already_running on %s (sid %s)", s.Label, r.Spec.Machine, r.Sid)
			}
		}
		started += d.startsWithinHour(r) // a resume is a start: it counts, whatever it replaces
	}
	if d.MaxPerHour > 0 && started >= d.MaxPerHour {
		return nil, refuse(RateCapped, "%d starts in the last hour (cap %d)", started, d.MaxPerHour)
	}
	if d.MaxLive > 0 && live >= d.MaxLive {
		return nil, refuse(MachineFull, "%d live sessions (cap %d)", live, d.MaxLive)
	}
	if d.PodMaxLive > 0 && live >= d.PodMaxLive {
		return nil, refuse(MachineFull, "%d live pod sessions (cap %d: each holds up to 32 GiB of the node's ephemeral storage)", live, d.PodMaxLive)
	}
	var rec *Record
	now := d.now().UTC()
	if old != nil {
		// The previous generation's pod goes first, by its recorded UID, whether or not the record says it has
		// ended (an ended record may still name a pod that could not be deleted): its token and relay are dealt
		// with by the verb. If that fails the resume is refused and nothing has changed.
		if old.PodUID != "" {
			if err := d.PodRoot.Stop(old.Sid, old.Gen); err != nil {
				return nil, refuse(SessionBusy, "resume: the previous generation's pod could not be deleted: %v", err)
			}
		}
		d.revokeOrSay(old.Sid, old.Gen)
		cp := *old
		rec = &cp
		rec.Gen++
		rec.RequestIDs = append(append([]string(nil), old.RequestIDs...), old.RequestID)
		rec.RequestID, rec.Started, rec.Ended, rec.Spec, rec.PodUID = s.RequestID, now, nil, *s, ""
		rec.Starts = append(recentStarts(old, now), now)
	} else {
		sid, err := mintSid()
		if err != nil {
			return nil, err
		}
		rec = &Record{Sid: sid, Gen: 1, Label: s.Label, RequestID: s.RequestID, Uid: d.Uid, Started: now, Spec: *s, Backend: "k8s"}
	}
	rec.Starting = true // from here until the pod is loaded, nothing else may act on this session
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
	// other start and replace. Starting keeps a resume of THIS session out meanwhile.
	if err := d.save(rec); err != nil {
		return nil, err
	}
	release()
	// Everything below acts on THIS generation only: it reloads the record under the lock and does nothing
	// if the record has moved on (a sweep, a stop), so it can never delete or end somebody else's pod.
	abandon := func(cause error, code string) (*StartReply, error) {
		unlock, err := d.lock()
		if err != nil {
			return nil, refuse(code, "%v", cause)
		}
		mine := d.owns(rec)
		unlock()
		if !mine {
			d.logf("start of %s generation %d failed (%v) but the session record is no longer that generation's: nothing was deleted", sid, rec.Gen, cause)
			return nil, refuse(code, "%v", cause)
		}
		// The pod is deleted WITHOUT the daemon's lock (a delete can take a minute and every other start waits
		// for it): the record still says Starting, so a sweep or a resume leaves it alone, but a stop or gc may still move the
		// session on, which is why the delete names this generation and root refuses a stale one.
		var stopErr error
		if rec.PodUID != "" {
			stopErr = d.PodRoot.Stop(sid, rec.Gen)
		}
		if errors.Is(stopErr, ErrPodStale) {
			// the session moved on while the delete was asked for: root refused and touched nothing, and the record is no
			// longer this generation's: leave it alone
			d.logf("start of %s generation %d failed (%v) and the session moved on before its pod could be deleted: nothing was deleted or changed", sid, rec.Gen, cause)
			return nil, refuse(code, "%v", cause)
		}
		unlock, err = d.lock()
		if err != nil {
			return nil, refuse(code, "%v", cause)
		}
		if d.owns(rec) {
			d.finishFailed(rec, old, stopErr != nil)
		}
		unlock()
		if stopErr != nil {
			return nil, fmt.Errorf("%s: %v (and the pod could not be deleted: %v; it is recorded as ended, retry `ccsbx-root pod-stop %s`)", code, cause, stopErr, sid)
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
		d.lockedFinish(rec, old)
		return nil, fmt.Errorf("%s: pod create failed (%v); pod %s may exist, it is recorded as ended", PodLoadFailed, err, PodName(sid, rec.Gen))
	}
	rec.PodUID = uid
	if err := d.saveOwn(rec); err != nil {
		// the pod exists and the record cannot say so: root cannot delete it without the UID. Say exactly
		// which pod and what UID, and end the record (best effort) so it holds no slot.
		d.logf("pod %s exists with UID %s but the record could not be written (%v): delete exactly that pod by hand", PodName(sid, rec.Gen), uid, err)
		d.lockedFinish(rec, old)
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
	rec.Starting = false
	for i := 0; i < 2; i++ { // a failed save here leaves a loaded pod marked as starting: try again
		if err = d.saveOwn(rec); err == nil {
			break
		}
	}
	if err != nil {
		d.logf("pod %s is loaded but its record could not be marked started (%v): the sweep will treat it as stuck after %s; stop and start again", PodName(sid, rec.Gen), err, d.startingLimit())
	}
	rep := rec.Reply
	return &rep, nil
}

// startsWithinHour counts this caller's session's starts in the last hour: its record's start, and the
// starts of its earlier generations.
func (d *Daemon) startsWithinHour(r *Record) int {
	if r.Uid != d.Uid {
		return 0
	}
	cut := d.now().Add(-time.Hour)
	seen := map[time.Time]bool{}
	for _, t := range append([]time.Time{r.Started}, r.Starts...) {
		if t.After(cut) {
			seen[t.UTC()] = true
		}
	}
	return len(seen)
}

// recentStarts is the starts worth keeping (the last day, at most 50), for the next generation's record.
func recentStarts(r *Record, now time.Time) []time.Time {
	var out []time.Time
	for _, t := range append([]time.Time{r.Started}, r.Starts...) {
		if now.Sub(t) < 24*time.Hour {
			out = append(out, t)
		}
	}
	if len(out) > 49 {
		out = out[len(out)-49:]
	}
	return out
}

// owns: the session's record on disk is still this generation, with this pod (or none yet).
func (d *Daemon) owns(rec *Record) bool {
	cur, err := d.Load(rec.Sid)
	return err == nil && cur.Gen == rec.Gen && (cur.PodUID == "" || cur.PodUID == rec.PodUID)
}

// saveOwn saves rec only if it is still the generation on disk, under the lock.
func (d *Daemon) saveOwn(rec *Record) error {
	unlock, err := d.lock()
	if err != nil {
		return err
	}
	defer unlock()
	if !d.owns(rec) {
		return fmt.Errorf("the session record is no longer generation %d's", rec.Gen)
	}
	return d.save(rec)
}

// lockedFinish ends this generation's record, kept (a pod may exist), under the lock and only if it is still ours.
func (d *Daemon) lockedFinish(rec, old *Record) {
	unlock, err := d.lock()
	if err != nil {
		return
	}
	defer unlock()
	if d.owns(rec) {
		d.finishFailed(rec, old, true)
	}
}

// finishFailed settles the record of a generation that did not come up. A first start that left no pod is
// removed (a retry is a fresh start). Anything else is kept, ended, at ITS OWN generation number: the number is
// never given back, because the doors refuse every token at or below a revoked generation, so the next
// resume must be the next number. A resume's record is the previous generation's (its request, spec and
// reply), so a retry of the failed request is not answered with the reply of a start that never came up.
// The caller holds the lock.
func (d *Daemon) finishFailed(rec, old *Record, keep bool) {
	now := d.now().UTC()
	switch {
	case old == nil && !keep:
		_ = os.Remove(d.path("sessions", rec.Sid+".json"))
	case old == nil:
		rec.Ended, rec.Starting = &now, false
		_ = d.save(rec)
	default:
		failed := failedGeneration(old, rec.Gen, now)
		failed.Starts = rec.Starts
		if keep {
			failed.PodUID = rec.PodUID // a pod that could not be deleted stays named, for a later pod-stop
		} else {
			failed.PodUID = ""
		}
		_ = d.save(failed)
	}
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
		if err := d.PodRoot.Stop(sid, rec.Gen); err != nil {
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

// startingLimit is how long a start may be in flight before a resume stops being refused and the sweep gives up
// on it: what the verbs may take (hostd sets StartLimit from --pod-load-wait plus the create and a margin),
// abandonedAfter when it has not been set.
func (d *Daemon) startingLimit() time.Duration {
	if d.StartLimit > 0 {
		return d.StartLimit
	}
	return abandonedAfter
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
			if age > d.startingLimit() {
				d.logf("pod session %s never recorded a pod UID: pod %s may exist, delete exactly that pod by hand; the record is ended", r.Sid, PodName(r.Sid, r.Gen))
				d.endRecord(r.Sid)
			}
		case age > ttl || (r.Starting && age > d.startingLimit()):
			// expired, or a start that never finished (hostd died in the middle of it): the pod is deleted
			if err := d.StopPod(r.Sid); err != nil {
				d.logf("pod sweep: stopping %s: %v", r.Sid, err)
			}
		case r.Starting:
			// a start in flight: nothing to reconcile yet
		default:
			if gone, err := d.PodRoot.Gone(r.Sid); err != nil {
				d.logf("pod sweep: %s: %v", r.Sid, err)
			} else if gone {
				// a pod that vanished outside pod-stop (evicted, OOM-killed, deleted by hand): pod-stop revokes
				// its token and relay (an absent pod is fine to it), then the record ends. Only then.
				if err := d.PodRoot.Stop(r.Sid, r.Gen); err != nil {
					d.logf("pod sweep: %s is gone but its token could not be revoked (%v): revoke sid %s gen %d by hand", r.Sid, err, r.Sid, r.Gen)
				} else {
					d.endRecord(r.Sid)
				}
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
