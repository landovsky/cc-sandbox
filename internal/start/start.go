// start.go is the direct-mode start daemon: it mints sid and gen, checks the spec against the profile
// and pins, and keeps the session records. It exists so every start or refusal is decided in one place (U2).

package start

import (
	"crypto/rand"
	"encoding/base32"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/landovsky/cc-sandbox/internal/tmuxsock"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/backend"
	"github.com/landovsky/cc-sandbox/internal/pins"
	"github.com/landovsky/cc-sandbox/internal/profile"
)

// Launcher starts and probes the pane. Production: tmux (TmuxLauncher). Tests: a fake.
type Launcher interface {
	Launch(rec *Record) error
	Alive(sid string) bool
}

// Record is what the daemon keeps per session; `ccsbx run --sid` reads it back, so the values
// the run uses are the ones the daemon minted and checked, not whatever the caller passes.
type Record struct {
	Sid       string `json:"sid"`
	Gen       int    `json:"gen"`
	Label     string `json:"label"`
	RequestID string `json:"request_id"`
	// RequestIDs is every request that produced a generation of this session, so a retry of
	// the gen-1 start after a resume can't start a second session.
	RequestIDs []string   `json:"request_ids,omitempty"`
	Uid        int        `json:"uid"` // the caller, from the OS
	Started    time.Time  `json:"started"`
	Ended      *time.Time `json:"ended,omitempty"`
	Spec       LaunchSpec `json:"spec"`
	Reply      StartReply `json:"reply"`
	// GitDir is the gitdir the fence may see read-only, decided once at the session's first
	// start. Later generations reuse it: the worktree's .git file is agent-written and is never
	// read again.
	GitDir string `json:"git_dir,omitempty"`
	// Backend is "" for a docker session and "k8s" for one that runs as a pod. PodUID is the UID the
	// API server gave the pod when we created it (k8s only): the identity every later step (exec
	// with secrets, attach, delete) checks against the live pod.
	Backend string `json:"backend,omitempty"`
	PodUID  string `json:"pod_uid,omitempty"`
}

// Daemon holds the policy knobs and the state directory.
type Daemon struct {
	StateDir    string
	RunDir      string // where generations' s/<sid>/g<n>/CLOSED live; empty: StateDir (helper mode: ccsbx-root's run dir)
	ProfilesDir string
	// ProfileLoader, when set, loads an approved profile instead of reading ProfilesDir. hostd sets
	// it to the profiles compiled into the binary, the same copy ccsbx-root uses, so the two can
	// never disagree about what a profile allows.
	ProfileLoader func(name string) (*profile.Profile, error)
	Machine       string // this machine's name; a spec for another machine is refused
	Uid           int    // the caller's uid (from the OS, never from the spec)
	Image         string // fence image reference recorded in the pin
	PinsFile      string // pins.toml; missing means no production pins
	WorktreeRoot  string // every worktree must be inside this directory (CCSBX_WORKTREE_ROOT)
	CCVersion     string
	MaxLive       int // machine_full above this many live sessions
	MaxPerHour    int // rate_capped above this many starts by one caller in an hour
	Backend       string
	Socket        string // tmux -L name for the attach command; default ccsbx
	Root          string // "direct" or "helper"
	// HelperProven: the helper has actually run on this machine and the docker group is gone. Only
	// then does a reply claim evidence; the mere presence of ccsbx-root proves nothing (invariants 12, 14).
	HelperProven bool
	// Runtime says whether a generation's container is running (the stopped proof asks it).
	Runtime     Runtime
	Launcher    Launcher
	ReplaceWait time.Duration // how long Replace waits for the old fence to shut down (default 15s)
	Now         func() time.Time
	// Exe is the ccsbx binary the pane runs ("run --sid ...").
	Exe string
}

func (d *Daemon) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// ResolveClientPaths is what `ccsbx start` does as the OWNER, before it sends a spec to hostd in
// helper mode: resolve workdir.path through symlinks and compute the gitdir exactly as the direct-mode
// daemon would (backend.ResolveGitDir, checked against uid), and put both into the spec. hostd, a
// different uid that cannot enter the owner's home, only checks their shape; ccsbx-root re-validates
// them for real at run time. A failure is a refusal the owner can act on.
func ResolveClientPaths(s *LaunchSpec, uid int) error {
	if s.Workdir.Path == "" {
		return refuse(BadSpec, "workdir.path is required")
	}
	rw, err := filepath.EvalSymlinks(s.Workdir.Path)
	if err != nil {
		return refuse(BadSpec, "workdir %s: %v", s.Workdir.Path, err)
	}
	if st, err := os.Stat(rw); err != nil || !st.IsDir() {
		return refuse(BadSpec, "workdir.path %s is not a directory", s.Workdir.Path)
	}
	gd, err := backend.ResolveGitDir(rw, uid)
	if err != nil {
		return refuse(BadSpec, "workdir git metadata: %v", err)
	}
	s.Workdir.Path, s.GitDir = rw, gd
	return nil
}

// LoadProfile loads an approved profile: from ProfileLoader (the profiles compiled into the binary)
// when set, else from ProfilesDir.
func (d *Daemon) LoadProfile(name string) (*profile.Profile, error) {
	if d.ProfileLoader != nil {
		return d.ProfileLoader(name)
	}
	return profile.Load(d.ProfilesDir, name)
}

// AbsRoots makes every entry of a comma-separated worktree-root list absolute (a relative entry is
// taken from the current directory, as a single --worktree-root always was). Entries are not
// otherwise touched: CheckWorkdir validates them exactly as ccsbx-root validates allowed_roots.
func AbsRoots(list string) string {
	parts := strings.Split(list, ",")
	for i, p := range parts {
		if a, err := filepath.Abs(p); err == nil && p != "" {
			parts[i] = a
		}
	}
	return strings.Join(parts, ",")
}

func (d *Daemon) runRoot() string {
	if d.RunDir != "" {
		return d.RunDir
	}
	return d.StateDir
}

func (d *Daemon) path(parts ...string) string {
	return filepath.Join(append([]string{d.StateDir}, parts...)...)
}

// Start applies policy and launches. It returns the StartReply, or a *Refusal for policy.
// Every refusal happens before anything is created, so a refused start leaves no trace.
func (d *Daemon) Start(s *LaunchSpec) (*StartReply, error) {
	if err := os.MkdirAll(d.path("sessions"), 0o700); err != nil {
		return nil, err
	}
	unlock, err := d.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()

	recs, err := d.records()
	if err != nil {
		return nil, err
	}

	// Idempotent per (caller, request_id): a retry gets the same reply and starts nothing.
	for _, r := range recs {
		if r.Uid == d.Uid && (r.RequestID == s.RequestID || contains(r.RequestIDs, s.RequestID)) {
			rep := r.Reply
			if r.RequestID != s.RequestID { // an older generation's request: its own reply is not kept
				return nil, refuse(BadSpec, "request_id %s was already used for an earlier generation of %s", s.RequestID, r.Sid)
			}
			return &rep, nil
		}
	}

	if s.Machine != d.Machine {
		return nil, refuse(WrongMachine, "spec is for %s, this is %s (the pane lives on the session's machine)", s.Machine, d.Machine)
	}
	if s.Workdir.Path == "" || s.Workdir.Repo != "" {
		return nil, refuse(NotBuilt, "only workdir.path is supported (repo/base/branch needs the git route, H1)")
	}
	var rw string
	if d.Root == "helper" {
		// hostd runs as the service uid and cannot enter the owner's home (0750): it never touches the
		// filesystem here. The CLIENT resolved the workdir and the gitdir as the owner (ResolveClientPaths);
		// this checks their shape, and ccsbx-root, as root, does the real validation at run time.
		var err error
		rw, err = backend.CheckWorkdirLexical(s.Workdir.Path, d.WorktreeRoot, d.StateDir, d.RunDir)
		if err != nil {
			return nil, refuse(BadSpec, "workdir: %v", err)
		}
		if err := backend.CheckGitDirLexical(s.GitDir, rw, d.WorktreeRoot); err != nil {
			return nil, refuse(BadSpec, "gitdir: %v", err)
		}
	} else {
		if s.GitDir != "" {
			return nil, refuse(BadSpec, "gitdir is only accepted in helper mode (here the daemon resolves it itself)")
		}
		var err error
		rw, err = backend.CheckWorkdir(s.Workdir.Path, d.WorktreeRoot, d.StateDir)
		if err != nil {
			return nil, refuse(BadSpec, "workdir: %v", err)
		}
		if st, err := os.Stat(rw); err != nil || !st.IsDir() {
			return nil, refuse(BadSpec, "workdir.path %s is not a directory", s.Workdir.Path)
		}
	}
	s.Workdir.Path = rw // the resolved path is what gets mounted and recorded

	p, err := d.LoadProfile(s.Profile)
	if err != nil {
		return nil, refuse(ProfileNotApproved, "profile %q: %v", s.Profile, err)
	}
	machineOK, billingOK := p.Allows(d.Machine, s.Billing)
	if !machineOK {
		return nil, refuse(ProfileNotApproved, "profile %s is not approved for %s", p.Name, d.Machine)
	}
	// Invariant 18: the launcher picks the billing mode and records it; there is no default.
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
		if r.Ended == nil && d.Launcher.Alive(r.Sid) {
			live++
			if r.Spec.Label == s.Label {
				return nil, refuse(AlreadyRunning, "%s already_running on %s (sid %s)", s.Label, d.Machine, r.Sid)
			}
		} else if r.Ended == nil {
			d.markEnded(r) // the pane is gone: a stale record must not block the label
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

	var rec *Record
	var prev *Record // set on resume, to roll back to if the launch fails
	if s.Resume != nil {
		old, err := d.Load(s.Resume.Sid)
		if err != nil {
			return nil, refuse(BadSpec, "resume: no session %q", s.Resume.Sid)
		}
		switch {
		case old.Uid != d.Uid:
			return nil, refuse(BadSpec, "resume: session %s belongs to another caller", old.Sid)
		case old.Label != s.Label:
			return nil, refuse(BadSpec, "resume: session %s has label %q, not %q (a resume keeps its label)", old.Sid, old.Label, s.Label)
		case old.Spec.Workdir.Path != rw:
			return nil, refuse(BadSpec, "resume: workdir must be the same, the transcript is keyed by it")
		}
		// a commit (or a fence) holds the session: a new generation must not start now
		// A missing lock file (helper mode) is "not held": nothing has ever run under ccsbx-root for
		// this session, and the fence this start launches creates and takes the lock itself.
		if rel, err := d.TrySessionLock(old.Sid); errors.Is(err, ErrNoLockFile) {
			// not busy
		} else if err != nil {
			return nil, refuse(SessionBusy, "resume: %v", err)
		} else {
			rel()
		}
		cp := *old
		prev = &cp
		rec = old
		rec.Gen++
		rec.RequestIDs = append(rec.RequestIDs, old.RequestID)
		rec.RequestID, rec.Started, rec.Ended, rec.Spec = s.RequestID, d.now().UTC(), nil, *s
	} else {
		sid, err := mintSid()
		if err != nil {
			return nil, err
		}
		// First start only: the worktree has not been written by an agent of this session yet.
		gd := s.GitDir // helper mode: resolved by the client as the owner, shape-checked above
		if d.Root != "helper" {
			var err error
			gd, err = backend.ResolveGitDir(rw, d.Uid)
			if err != nil {
				return nil, refuse(BadSpec, "workdir git metadata: %v", err)
			}
		}
		rec = &Record{Sid: sid, Gen: 1, Label: s.Label, RequestID: s.RequestID, Uid: d.Uid, Started: d.now().UTC(), Spec: *s, GitDir: gd}
	}
	sid := rec.Sid
	rec.Reply = StartReply{
		V: 1, Sid: sid, Gen: rec.Gen, ID: fmt.Sprintf("%s/%s/%s/g%d", s.Owner, d.Machine, sid, rec.Gen), Backend: d.Backend,
		Pin:     Pin{CC: orUnpinned(d.CCVersion), Image: orUnpinned(d.Image), Channel: channel},
		Attach:  Attach{Local: append(append([]string{"tmux"}, tmuxsock.Args(orDefault(d.Socket, "ccsbx"))...), "attach", "-t", sid), Remote: []string{"ssh", "-t", d.Machine, "ccsbx", "attach", sid}},
		Journal: Journal{Epoch: d.epoch(), Seq: len(recs)},
		NotEv:   !(d.Root == "helper" && d.HelperProven),
	}
	// Write the record before launching: `ccsbx run --sid` reads it.
	if err := d.save(rec); err != nil {
		return nil, err
	}
	if err := d.Launcher.Launch(rec); err != nil {
		// Remove, not mark ended: a retry with this request_id must not be answered with the
		// reply of a session that never started.
		if prev != nil {
			_ = d.save(prev) // a failed resume leaves the session as it was
		} else {
			_ = os.Remove(d.path("sessions", rec.Sid+".json"))
		}
		return nil, fmt.Errorf("launch %s: %w", sid, err)
	}
	rep := rec.Reply
	return &rep, nil
}

// Load reads one record, for `ccsbx run --sid`.
func (d *Daemon) Load(sid string) (*Record, error) {
	if !sidRe.MatchString(sid) {
		return nil, fmt.Errorf("sid %q is not valid", sid)
	}
	b, err := os.ReadFile(d.path("sessions", sid+".json"))
	if err != nil {
		return nil, err
	}
	var r Record
	return &r, json.Unmarshal(b, &r)
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func orUnpinned(s string) string {
	if s == "" {
		return "unpinned"
	}
	return s
}

func (d *Daemon) lock() (func(), error) {
	f, err := os.OpenFile(d.path("lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}

func (d *Daemon) records() ([]*Record, error) {
	ents, err := os.ReadDir(d.path("sessions"))
	if os.IsNotExist(err) {
		return nil, nil // nothing was ever started here: an empty list, not an error
	}
	if err != nil {
		return nil, err
	}
	var out []*Record
	for _, e := range ents {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		r, err := d.Load(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, nil
}

func (d *Daemon) save(r *Record) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	tmp := d.path("sessions", r.Sid+".json.tmp")
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, d.path("sessions", r.Sid+".json"))
}

func (d *Daemon) markEnded(r *Record) {
	t := d.now().UTC()
	r.Ended = &t
	_ = d.save(r)
}

// epoch is stable per state directory, so journal readers can tell a wiped state from a restart.
func (d *Daemon) epoch() string {
	p := d.path("epoch")
	if b, err := os.ReadFile(p); err == nil && len(b) >= 8 {
		return strings.TrimSpace(string(b))
	}
	var raw [4]byte
	_, _ = rand.Read(raw[:])
	e := fmt.Sprintf("e%x", raw)
	_ = os.WriteFile(p, []byte(e+"\n"), 0o600)
	return e
}

var enc = base32.NewEncoding("0123456789abcdefghjkmnpqrstvwxyz").WithPadding(base32.NoPadding)

func mintSid() (string, error) {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "s" + enc.EncodeToString(b[:]), nil
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// Records lists every session record, for the daemon's resolvers (message delivery, journal).
func (d *Daemon) Records() ([]*Record, error) { return d.records() }

// Stopper is a Launcher that can end a pane (TmuxLauncher).
type Stopper interface{ Stop(sid string) error }

// Replace is session.replace (C9b): generation+1 of a running session with a new brief. The
// caller has already checked the turn is over. Order matters, so that a failure at any step
// leaves a consistent record:
//
//  1. end the old pane (a failure here changes nothing);
//  2. wait until the old `ccsbx run` has torn its container down (its CLOSED marker), so two
//     containers never hold the same workdir and per-node home read-write at once; if it does not
//     in time, nothing new is started and the record stays as it was (the dead pane makes the next
//     start mark it ended);
//  3. only then save generation+1 (same sid, label, workdir, profile; a fresh Started, so
//     "in this generation" checks don't accept messages sent to the previous one) and launch it; a
//     failed launch restores the previous record and marks it ended, since its pane is gone.
func (d *Daemon) Replace(sid, brief string) (*Record, error) {
	unlock, err := d.lock()
	if err != nil {
		return nil, err
	}
	defer unlock()
	rec, err := d.Load(sid)
	if err != nil {
		return nil, err
	}
	if rec.Ended != nil || !d.Launcher.Alive(sid) {
		return nil, refuse(NotRunning, "%s is not running", sid)
	}
	st, ok := d.Launcher.(Stopper)
	if !ok {
		return nil, refuse(NotBuilt, "this launcher cannot end a pane")
	}
	prev := *rec
	if err := st.Stop(sid); err != nil {
		return nil, fmt.Errorf("end the old pane: %w", err)
	}
	closed := filepath.Join(d.runRoot(), "s", sid, fmt.Sprintf("g%d", prev.Gen), "CLOSED")
	wait := d.ReplaceWait
	if wait == 0 {
		wait = 15 * time.Second
	}
	for deadline := time.Now().Add(wait); ; {
		if _, err := os.Stat(closed); err == nil {
			break
		}
		if time.Now().After(deadline) {
			return nil, fmt.Errorf("the old generation's fence did not shut down within %s; nothing was started", wait)
		}
		time.Sleep(50 * time.Millisecond)
	}
	rec.Gen++
	rec.Started = d.now().UTC()
	rec.Spec.Brief = &Brief{Text: brief}
	rec.Reply.Gen = rec.Gen
	rec.Reply.ID = fmt.Sprintf("%s/%s/%s/g%d", rec.Spec.Owner, d.Machine, sid, rec.Gen)
	if err := d.save(rec); err != nil {
		return nil, err
	}
	if err := d.Launcher.Launch(rec); err != nil {
		d.markEnded(&prev)
		return nil, fmt.Errorf("launch generation %d: %w", rec.Gen, err)
	}
	return rec, nil
}
