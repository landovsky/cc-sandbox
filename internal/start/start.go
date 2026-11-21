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
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/landovsky/cc-sandbox/internal/backend"
	"github.com/landovsky/cc-sandbox/internal/pins"
	"github.com/landovsky/cc-sandbox/internal/profile"
	"github.com/landovsky/cc-sandbox/internal/worktree"
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
	RequestIDs []string `json:"request_ids,omitempty"`
	Uid        int      `json:"uid"` // the caller, from the OS
	// StartedBy is who asked, when it was not a process on this machine: "owner:<login>@<device>", stamped by
	// the daemon from the tailnet's word about the caller (M2c), never from the request.
	StartedBy string     `json:"started_by,omitempty"`
	Started   time.Time  `json:"started"`
	Ended     *time.Time `json:"ended,omitempty"`
	Spec      LaunchSpec `json:"spec"`
	Reply     StartReply `json:"reply"`
	// GitDir is the gitdir the fence may see read-only, decided once at the session's first
	// start. Later generations reuse it: the worktree's .git file is agent-written and is never
	// read again.
	GitDir string `json:"git_dir,omitempty"`
	// Backend is "" for a docker session and "k8s" for one that runs as a pod. PodUID is the UID the
	// API server gave the pod when we created it (k8s only): the identity every later step (exec
	// with secrets, attach, delete) checks against the live pod.
	Backend string `json:"backend,omitempty"`
	PodUID  string `json:"pod_uid,omitempty"`
	// Starting is set from a pod session's first save until its pod is loaded (or the start failed): the
	// generation is being made, and nothing else may act on this session (a resume is refused). Starts are the
	// times this session's generations were started, so a resume counts against the hourly cap.
	Starting bool        `json:"starting,omitempty"`
	Starts   []time.Time `json:"starts,omitempty"`
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
	// WorktreeCheck asks root whether a path is a git worktree root under allowed_roots (helper mode, remote starts only):
	// it returns the resolved path, or an error for ANY doubt. nil: remote starts are refused in helper mode.
	// Worktrees builds a worktree from the repo form (direct mode only); nil: the repo form is refused not_built.
	Worktrees     *worktree.Builder
	WorktreeCheck func(path string) (resolved, gitdir string, err error)
	StartedBy     string // a remote caller's stamped identity, recorded with the session (M2c); "" for a local start
	Image         string // fence image reference recorded in the pin
	PinsFile      string // pins.toml; missing means no production pins
	WorktreeRoot  string // every worktree must be inside this directory (CCSBX_WORKTREE_ROOT)
	CCVersion     string
	MaxLive       int // machine_full above this many live sessions
	MaxPerHour    int // rate_capped above this many starts by one caller in an hour
	// RemoteMaxPerHour is the same cap for starts asked for over the tailnet (StartedBy != ""), counted apart.
	RemoteMaxPerHour int
	Backend          string
	Socket           string // tmux -L name for the attach command; default ccsbx
	Root             string // "direct" or "helper"
	// HelperProven: the helper has actually run on this machine and the docker group is gone. Only
	// then does a reply claim evidence; the mere presence of ccsbx-root proves nothing (invariants 12, 14).
	HelperProven bool
	// Runtime says whether a generation's container is running (the stopped proof asks it).
	Runtime  Runtime
	Launcher Launcher
	// Placements maps a machine name to how sessions are placed on it ("k8s": a pod, started through
	// ccsbx-root's pod verbs). A spec for a machine in the map is a pod session; PodRoot runs the verbs.
	Placements map[string]string
	PodRoot    PodRoot
	// Revoke is called when a pod session's token may exist and the start is abandoned or the
	// generation ends (the doors then refuse it; plan U5). Optional.
	Revoke func(sid string, gen int)
	// Logf receives the daemon's notices; with no Revoke it carries "token may exist for sid X gen N".
	Logf func(format string, a ...any)
	// PodVerbsRevoke says ccsbx-root's pod verbs revoke a pod generation's token and relay themselves (pod-stop, and
	// pod-load when it fails), so hostd has nothing to revoke by hand for pods and says nothing about it; their
	// own audit lines report a revoke that failed.
	PodVerbsRevoke bool
	// StartLimit bounds how long a pod start may be in flight (see startingLimit); 0: 15 minutes.
	StartLimit time.Duration
	// PodMaxLive caps the live pod sessions on their own (a pod holds up to 32 GiB of ephemeral storage and 4 GiB of memory on the shared
	// node, far more than a docker session): machine_full above this many. 0: only MaxLive applies.
	PodMaxLive  int
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
	// The repo form (R-WT, docs/r-wt-scope.md): in direct mode the daemon builds the worktree from {repo, base, branch} first,
	// OUTSIDE its lock (a fetch may take a while), and then starts on the resulting path like any other start. A retry that is
	// already recorded is answered by Start itself without building another. Pods and helper mode keep their own routes.
	if s.Workdir.Repo != "" && d.Worktrees != nil && d.Root != "helper" && d.Placements[s.Machine] != "k8s" {
		if rep, _ := d.Recorded(d.Uid, d.StartedBy, s.RequestID, s.Label); rep == nil {
			return d.startFromRepo(s)
		}
	}
	return d.startInner(s)
}

// repoStarts serialises concurrent starts of one (uid, caller, request_id): a retry that arrives during a long fetch waits for the first
// attempt and is then answered from its record, instead of being refused ("the branch already exists") for a session that exists.
var repoStarts sync.Map

// preflight is the cheap, side-effect-free part of the policy, run BEFORE a fetch: a start that will be refused anyway (label live,
// machine full, hourly cap, profile) must not cost a clone fetch and a checkout. startInner repeats everything after the build.
func (d *Daemon) preflight(s *LaunchSpec) error {
	unlock, err := d.lock()
	if err != nil {
		return err
	}
	defer unlock()
	recs, err := d.records()
	if err != nil {
		return err
	}
	p, err := d.LoadProfile(s.Profile)
	if err != nil {
		return refuse(ProfileNotApproved, "this profile is not approved")
	}
	if m, b := p.Allows(d.Machine, s.Billing); !m {
		return refuse(ProfileNotApproved, "this profile is not approved for this machine")
	} else if !b {
		return refuse(BillingUnavailable, "this billing mode is not allowed for the profile")
	}
	live, started := 0, 0
	for _, r := range recs {
		if r.Backend == "k8s" {
			if r.Ended == nil {
				live++
			}
			continue
		}
		if r.Ended == nil && d.Launcher.Alive(r.Sid) {
			live++
			if r.Spec.Label == s.Label {
				ref := refuse(AlreadyRunning, "%s already_running on %s (sid %s)", s.Label, d.Machine, r.Sid)
				ref.Machine, ref.Sid, ref.State = d.Machine, r.Sid, "running"
				return ref
			}
		}
		if r.Uid == d.Uid && d.now().Sub(r.Started) < time.Hour && (r.StartedBy != "") == (d.StartedBy != "") {
			started++
		}
	}
	hourCap := d.MaxPerHour
	if d.StartedBy != "" {
		hourCap = d.RemoteMaxPerHour
	}
	if hourCap > 0 && started >= hourCap {
		return refuse(RateCapped, "%d starts in the last hour (cap %d)", started, hourCap)
	}
	if d.MaxLive > 0 && live >= d.MaxLive {
		return refuse(MachineFull, "%d live sessions (cap %d)", live, d.MaxLive)
	}
	return nil
}

func (d *Daemon) startFromRepo(s *LaunchSpec) (*StartReply, error) {
	if s.Resume != nil {
		return nil, refuse(BadSpec, "a resume keeps its workdir: it cannot name a repository")
	}
	if s.Machine != d.Machine {
		return nil, refuse(WrongMachine, "spec is for another machine")
	}
	key := fmt.Sprintf("%d\x00%s\x00%s", d.Uid, d.StartedBy, s.RequestID)
	mu, _ := repoStarts.LoadOrStore(key, &sync.Mutex{})
	mu.(*sync.Mutex).Lock()
	defer func() { mu.(*sync.Mutex).Unlock(); repoStarts.Delete(key) }()
	if rep, same := d.Recorded(d.Uid, d.StartedBy, s.RequestID, s.Label); rep != nil { // the attempt we waited for succeeded
		if !same {
			return nil, refuse(RequestIDReused, "request_id was already used for another session")
		}
		return rep, nil
	}
	if err := d.preflight(s); err != nil {
		return nil, err
	}
	h, err := d.Worktrees.Prepare(s.Owner, s.Label, s.Workdir.Repo, s.Workdir.Base, s.Workdir.Branch)
	if err != nil {
		var we *worktree.Error
		if errors.As(err, &we) {
			return nil, refuse(BadSpec, "%s", we.Msg)
		}
		return nil, err
	}
	s2 := *s
	s2.Workdir.Path = h.Path
	s2.built = true
	rep, err := d.startInner(&s2)
	if err != nil {
		d.Worktrees.Remove(h) // a start that did not happen leaves no worktree and no branch behind
		return nil, err
	}
	return rep, nil
}

func (d *Daemon) startInner(s *LaunchSpec) (*StartReply, error) {
	if err := os.MkdirAll(d.path("sessions"), 0o700); err != nil {
		return nil, err
	}
	unlockFn, err := d.lock()
	if err != nil {
		return nil, err
	}
	var once sync.Once
	unlock := func() { once.Do(unlockFn) } // a pod start lets go early (see startPod)
	defer unlock()

	recs, err := d.records()
	if err != nil {
		return nil, err
	}

	// Idempotent per (uid, caller identity, request_id): a retry gets the same reply and starts nothing. A request_id
	// another caller used is not this one's; the same caller reusing it for ANOTHER label is request_id_reused.
	for _, r := range recs {
		if r.Uid == d.Uid && r.StartedBy == d.StartedBy && (r.RequestID == s.RequestID || contains(r.RequestIDs, s.RequestID)) {
			rep := r.Reply
			if r.RequestID != s.RequestID { // an older generation's request: its own reply is not kept
				return nil, refuse(BadSpec, "request_id %s was already used for an earlier generation of %s", s.RequestID, r.Sid)
			}
			if r.Label != s.Label {
				return nil, refuse(RequestIDReused, "request_id was already used for another session")
			}
			return &rep, nil
		}
	}

	if d.Placements[s.Machine] == "k8s" {
		return d.startPod(s, recs, unlock)
	}
	if s.Machine != d.Machine {
		return nil, refuse(WrongMachine, "spec is for %s, this is %s (the pane lives on the session's machine)", s.Machine, d.Machine)
	}
	if s.Workdir.Path == "" || (s.Workdir.Repo != "" && !s.built) {
		return nil, refuse(NotBuilt, "only workdir.path is supported (repo/base/branch needs the git route, H1)")
	}
	var rw string
	remote := d.StartedBy != ""
	// A REMOTE caller's refusal text is fixed: an OS error would carry paths and let the caller probe what exists.
	wdErr := func(what string, err error) *Refusal {
		if remote {
			return refuse(BadSpec, "%s is not acceptable", what)
		}
		return refuse(BadSpec, "%s: %v", what, err)
	}
	if remote && d.Root == "helper" {
		// The owner's home is unreadable to hostd here, so "is this directory a git worktree root" is asked of root
		// (`ccsbx-root worktree-check`, docs/m2c-remote-start.md §4b) BEFORE anything is written. Without a checker
		// the remote start is refused: any directory under the worktree root would otherwise mount read-write.
		if d.WorktreeCheck == nil {
			return nil, refuse(NotBuilt, "a remote start needs the worktree check that helper mode cannot make here")
		}
		// a record whose pane died but is not marked ended would count as a live session on this very workdir
		// (root counts Ended == nil): end the dead ones first, under the lock this Start already holds
		for _, r := range recs {
			if r.Backend != "k8s" && r.Ended == nil && !d.Launcher.Alive(r.Sid) {
				d.markEnded(r)
			}
		}
		got, gitdir, err := d.WorktreeCheck(s.Workdir.Path)
		if errors.Is(err, ErrNoHelper) {
			return nil, refuse(NotBuilt, "a remote start needs ccsbx-root, which is not installed here")
		}
		if errors.Is(err, ErrCannotTell) { // root could not tell (exit 3): our side's trouble, not the caller's spec
			return nil, refuse(Internal, "the worktree check could not be made on this machine (see the daemon's log)")
		}
		if err != nil || got != filepath.Clean(s.Workdir.Path) {
			return nil, refuse(BadSpec, "workdir is not a git worktree root")
		}
		s.GitDir = gitdir // derived by root, never from the caller: `ccsbx-root run` compares it with its own derivation
	}
	if d.Root == "helper" {
		// hostd runs as the service uid and cannot enter the owner's home (0750): it never touches the
		// filesystem here. The CLIENT resolved the workdir and the gitdir as the owner (ResolveClientPaths);
		// this checks their shape, and ccsbx-root, as root, does the real validation at run time.
		var err error
		rw, err = backend.CheckWorkdirLexical(s.Workdir.Path, d.WorktreeRoot, d.StateDir, d.RunDir)
		if err != nil {
			return nil, wdErr("workdir", err)
		}
		if err := backend.CheckGitDirLexical(s.GitDir, rw, d.WorktreeRoot); err != nil {
			return nil, wdErr("gitdir", err)
		}
	} else {
		if s.GitDir != "" {
			return nil, refuse(BadSpec, "gitdir is only accepted in helper mode (here the daemon resolves it itself)")
		}
		var err error
		rw, err = backend.CheckWorkdir(s.Workdir.Path, d.WorktreeRoot, d.StateDir)
		if err != nil {
			return nil, wdErr("workdir", err)
		}
		if st, err := os.Stat(rw); err != nil || !st.IsDir() {
			return nil, wdErr("workdir", err)
		}
		if remote {
			// a remote start must name a git WORKTREE ROOT (its own .git at exactly this path): not a parent of
			// repos, not a subdirectory of one, not a plain directory under the root
			if gd, err := backend.ResolveGitDir(rw, d.Uid); err != nil || gd == "" {
				return nil, refuse(BadSpec, "workdir is not a git worktree root")
			}
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
		if r.Backend == "k8s" {
			if r.Ended == nil {
				live++ // a pod session lives until it is stopped; there is no pane to ask
			}
			continue
		}
		if r.Ended == nil && d.Launcher.Alive(r.Sid) {
			live++
			if r.Spec.Label == s.Label {
				ref := refuse(AlreadyRunning, "%s already_running on %s (sid %s)", s.Label, d.Machine, r.Sid)
				ref.Machine, ref.Sid, ref.State = d.Machine, r.Sid, "running"
				return nil, ref
			}
			// one workdir, one live session: a workdir equal to, containing or inside another live session's is
			// refused (the agent that owns the outer tree could swap the inner directory for a link between the
			// check and the mount). A resume of that very session is exempt.
			if (s.Resume == nil || s.Resume.Sid != r.Sid) && overlaps(r.Spec.Workdir.Path, rw) {
				return nil, refuse(BadSpec, "the workdir overlaps another live session's")
			}
		} else if r.Ended == nil {
			d.markEnded(r) // the pane is gone: a stale record must not block the label
		}
		// remote starts have their own hourly budget (M2c): they cannot use up the owner's local starts
		if r.Uid == d.Uid && d.now().Sub(r.Started) < time.Hour && (r.StartedBy != "") == (d.StartedBy != "") {
			started++
		}
	}
	hourCap := d.MaxPerHour
	if d.StartedBy != "" {
		hourCap = d.RemoteMaxPerHour
	}
	if hourCap > 0 && started >= hourCap {
		return nil, refuse(RateCapped, "%d starts in the last hour (cap %d)", started, hourCap)
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
		case d.StartedBy != "" && old.StartedBy != d.StartedBy:
			return nil, refuse(ResumeNotYours, "resume: session %s was not started by this caller", old.Sid)
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
		rec = &Record{Sid: sid, Gen: 1, Label: s.Label, RequestID: s.RequestID, Uid: d.Uid, Started: d.now().UTC(), Spec: *s, GitDir: gd, StartedBy: d.StartedBy}
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
			_ = d.save(failedGeneration(prev, rec.Gen, d.now().UTC())) // the previous generation's session, the failed one's number
		} else {
			_ = os.Remove(d.path("sessions", rec.Sid+".json"))
		}
		return nil, fmt.Errorf("launch %s: %w", sid, err)
	}
	rep := rec.Reply
	return &rep, nil
}

// LiveByLabel are this machine's live sessions of a label (a record not ended whose pane is alive), read-only.
func (d *Daemon) LiveByLabel(label string) ([]*Record, error) {
	recs, err := d.records()
	if err != nil {
		return nil, err
	}
	var out []*Record
	for _, r := range recs {
		if r.Ended == nil && r.Spec.Label == label && d.Launcher.Alive(r.Sid) {
			out = append(out, r)
		}
	}
	return out, nil
}

// Recorded is the reply of the start this caller already made with this request_id, or nil: read-only, so a caller can
// answer a retry from the record without launching anything.
func (d *Daemon) Recorded(uid int, startedBy, requestID, label string) (*StartReply, bool) {
	recs, err := d.records()
	if err != nil {
		return nil, false
	}
	for _, r := range recs {
		if r.Uid == uid && r.StartedBy == startedBy && r.RequestID == requestID {
			rep := r.Reply
			return &rep, r.Label == label
		}
	}
	return nil, false
}

// Seen reports whether this caller already started a session with this request_id (so a retry is answered by
// Start's own idempotency, not refused because the label it started is now live).
func (d *Daemon) Seen(requestID string) bool {
	recs, err := d.records()
	if err != nil {
		return false
	}
	for _, r := range recs {
		if r.Uid == d.Uid && (r.RequestID == requestID || contains(r.RequestIDs, requestID)) {
			return true
		}
	}
	return false
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

// ErrNoHelper: ccsbx-root is not installed, so no worktree check can be made (the start is not_built, not bad_spec).
var ErrNoHelper = errors.New("ccsbx-root is not installed")

// ErrCannotTell: ccsbx-root worktree-check exited 3 (an unreadable config or record, a read error): no verdict, and not the caller's fault.
var ErrCannotTell = errors.New("the worktree check could not tell")

// overlaps: a and b are the same directory, or one is inside the other.
func overlaps(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == "." || b == "." || a == "" || b == "" {
		return false
	}
	return a == b || strings.HasPrefix(a, b+string(filepath.Separator)) || strings.HasPrefix(b, a+string(filepath.Separator))
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
		if errors.Is(err, os.ErrNotExist) {
			continue // removed since the listing (a failed launch rolls its record back): not a read failure
		}
		if err != nil {
			return nil, err // any other doubt is an error, never "no such session": callers refuse on it
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
		_ = d.save(failedGeneration(&prev, rec.Gen, d.now().UTC())) // the pane is gone and so is the number
		return nil, fmt.Errorf("launch generation %d: %w", rec.Gen, err)
	}
	return rec, nil
}

// failedGeneration is the record of a session whose next generation did not come up: the session as the
// previous generation left it (its request, spec and reply, so a retry of the failed request is not answered
// with a success), ended, but under the NEW generation's number and full id. The number is never given back:
// the doors refuse every token at or below a revoked generation, so the next resume must be the number after
// this one; and the full id names the generation the record is at.
func failedGeneration(prev *Record, gen int, now time.Time) *Record {
	f := *prev
	f.Gen, f.Ended, f.Starting = gen, &now, false
	f.Reply.Gen = gen
	if i := strings.LastIndex(f.Reply.ID, "/g"); i >= 0 {
		f.Reply.ID = f.Reply.ID[:i] + "/g" + strconv.Itoa(gen)
	}
	return &f
}

// NewWorktreeBuilder is the repo-form builder for a direct-mode daemon: clones in the state dir, worktrees directly under the FIRST
// absolute worktree root. nil when there is no absolute root (the repo form then stays not_built).
func NewWorktreeBuilder(stateDir, roots string, lim worktree.Limits) *worktree.Builder {
	first, _, _ := strings.Cut(AbsRoots(roots), ",")
	if stateDir == "" || !filepath.IsAbs(first) {
		return nil
	}
	return &worktree.Builder{StateDir: stateDir, Root: first, Limits: lim}
}

// PruneWorktrees removes the repo-form worktrees (and branches) of sessions that ended more than keep ago, or that no record names
// and that have been untouched that long. A worktree a live session uses, or one of a session that ended recently, stays.
func (d *Daemon) PruneWorktrees(keep time.Duration) int {
	if d.Worktrees == nil {
		return 0
	}
	now := d.now()
	// asked again, with FRESH records under the daemon lock, right before each removal: a session resumed since the pass began keeps its tree
	needed := func(path string) bool {
		unlock, err := d.lock()
		if err != nil {
			return true
		}
		defer unlock()
		recs, err := d.records()
		if err != nil {
			return true // cannot tell what is in use: remove nothing
		}
		for _, r := range recs {
			if r.Spec.Workdir.Path != path {
				continue
			}
			if r.Ended == nil || now.Sub(*r.Ended) <= keep {
				return true
			}
		}
		return false
	}
	return d.Worktrees.Prune(keep, needed, now)
}
