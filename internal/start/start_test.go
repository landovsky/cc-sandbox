// Tests for start, resume and spec parsing. They guard against a caller-supplied started_by, a resume that
// changes the sid or skips the generation bump, and a failed launch leaving a live record.

package start

import (
	"errors"
	"fmt"
	"github.com/landovsky/cc-sandbox/profiles"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeLauncher struct {
	mu     sync.Mutex
	alive  map[string]bool
	launch int
}

func (f *fakeLauncher) Launch(r *Record) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.launch++
	f.alive[r.Sid] = true
	return nil
}
func (f *fakeLauncher) Alive(sid string) bool { f.mu.Lock(); defer f.mu.Unlock(); return f.alive[sid] }

// vt is a temp dir under /var/tmp: /tmp is a fixed mount point of the fence, so a worktree there
// is refused.
func vt(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/var/tmp", "ccsbx-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	d, _ = filepath.EvalSymlinks(d)
	os.Mkdir(filepath.Join(d, "wt"), 0o755)
	return d
}

func newDaemon(t *testing.T) (*Daemon, *fakeLauncher) {
	t.Helper()
	fl := &fakeLauncher{alive: map[string]bool{}}
	root := vt(t)
	curRoot = root
	return &Daemon{StateDir: t.TempDir(), WorktreeRoot: root, ProfilesDir: "../../profiles", Machine: "hp-ubuntu", Uid: 1000,
		MaxLive: 8, MaxPerHour: 20, Backend: "docker", Root: "direct", Launcher: fl, Exe: "ccsbx", Image: "ccsbx-fence:dev", PinsFile: t.TempDir() + "/pins.toml"}, fl
}

func spec(mod func(*LaunchSpec)) *LaunchSpec {
	s := &LaunchSpec{V: 1, RequestID: "rid-1", Owner: "tomas", Machine: "hp-ubuntu", Label: "cc-sandbox:a", Realm: "production",
		Harness: "claude", Workdir: Workdir{Path: filepath.Join(curRoot, "wt")}, Profile: "dev-default", Billing: "stub"}
	if mod != nil {
		mod(s)
	}
	return s
}

// curRoot is the worktree root of the daemon newDaemon built last; spec() puts its workdir there.
// (These tests run sequentially.)
var curRoot string

func code(err error) string {
	if r, ok := err.(*Refusal); ok {
		return r.Code
	}
	if err != nil {
		return "ERR " + err.Error()
	}
	return ""
}

func TestStart(t *testing.T) {
	t.Run("the daemon mints sid and gen, so a caller can't pick or replay an identity", func(t *testing.T) {
		d, _ := newDaemon(t)
		rep, err := d.Start(spec(nil))
		if err != nil {
			t.Fatal(err)
		}
		if !sidRe.MatchString(rep.Sid) || rep.Gen != 1 || rep.ID != "tomas/hp-ubuntu/"+rep.Sid+"/g1" {
			t.Fatalf("%+v", rep)
		}
		if !rep.NotEv {
			t.Fatal("direct mode must be marked not evidence")
		}
	})

	t.Run("no billing mode is refused: the launcher never picks one silently (invariant 18)", func(t *testing.T) {
		d, fl := newDaemon(t)
		_, err := d.Start(spec(func(s *LaunchSpec) { s.Billing = "" }))
		if code(err) != BillingUnavailable || fl.launch != 0 {
			t.Fatalf("%v launches=%d", err, fl.launch)
		}
	})

	t.Run("a billing mode the profile doesn't allow is refused", func(t *testing.T) {
		d, _ := newDaemon(t)
		if _, err := d.Start(spec(func(s *LaunchSpec) { s.Billing = "subscription" })); code(err) != BillingUnavailable {
			t.Fatal(err)
		}
	})

	t.Run("a profile approved for another machine is refused here", func(t *testing.T) {
		d, _ := newDaemon(t)
		d.Machine = "macbook"
		if _, err := d.Start(spec(func(s *LaunchSpec) { s.Machine = "macbook" })); code(err) != ProfileNotApproved {
			t.Fatal(err)
		}
	})

	t.Run("an unknown profile is not approved, and a traversal name is too", func(t *testing.T) {
		d, _ := newDaemon(t)
		for _, p := range []string{"nope", "../x"} {
			if _, err := d.Start(spec(func(s *LaunchSpec) { s.Profile = p })); code(err) != ProfileNotApproved {
				t.Errorf("%s: %v", p, err)
			}
		}
	})

	t.Run("the same label started twice is already_running on the machine", func(t *testing.T) {
		d, fl := newDaemon(t)
		if _, err := d.Start(spec(nil)); err != nil {
			t.Fatal(err)
		}
		_, err := d.Start(spec(func(s *LaunchSpec) { s.RequestID = "rid-2" }))
		r, ok := err.(*Refusal)
		if !ok || r.Code != AlreadyRunning || !strings.Contains(r.Msg, "already_running on hp-ubuntu") {
			t.Fatalf("%v", err)
		}
		if fl.launch != 1 {
			t.Fatalf("launched %d times", fl.launch)
		}
	})

	t.Run("a label whose pane died can start again: a stale record must not block it", func(t *testing.T) {
		d, fl := newDaemon(t)
		rep, _ := d.Start(spec(nil))
		fl.alive[rep.Sid] = false
		if _, err := d.Start(spec(func(s *LaunchSpec) { s.RequestID = "rid-2" })); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a retry with the same request_id returns the same reply and starts nothing (idempotent)", func(t *testing.T) {
		d, fl := newDaemon(t)
		a, _ := d.Start(spec(nil))
		b, err := d.Start(spec(nil))
		if err != nil || a.Sid != b.Sid || fl.launch != 1 {
			t.Fatalf("%v %v launches=%d", b, err, fl.launch)
		}
	})

	t.Run("a spec for another machine is refused: the pane lives on the session's machine", func(t *testing.T) {
		d, _ := newDaemon(t)
		if _, err := d.Start(spec(func(s *LaunchSpec) { s.Machine = "future-two" })); code(err) != WrongMachine {
			t.Fatal(err)
		}
	})

	t.Run("machine_full: the cap on live sessions holds", func(t *testing.T) {
		d, _ := newDaemon(t)
		d.MaxLive = 2
		for i, l := range []string{"a", "b"} {
			if _, err := d.Start(spec(func(s *LaunchSpec) {
				s.Label = l
				s.RequestID = "r" + string(rune('0'+i))
				s.Workdir.Path = sub(t, spec(nil).Workdir.Path, l)
			})); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := d.Start(spec(func(s *LaunchSpec) {
			s.Label = "c"
			s.RequestID = "r9"
			s.Workdir.Path = sub(t, spec(nil).Workdir.Path, "c")
		})); code(err) != MachineFull {
			t.Fatal(err)
		}
	})

	t.Run("rate_capped: a runaway caller can't start sessions without limit, and the hour rolls", func(t *testing.T) {
		d, fl := newDaemon(t)
		d.MaxPerHour = 2
		now := time.Now()
		d.Now = func() time.Time { return now }
		for i, l := range []string{"a", "b"} {
			d.Start(spec(func(s *LaunchSpec) {
				s.Label = l
				s.RequestID = "r" + string(rune('0'+i))
				s.Workdir.Path = sub(t, spec(nil).Workdir.Path, l)
			}))
		}
		for sid := range fl.alive {
			fl.alive[sid] = false
		}
		if _, err := d.Start(spec(func(s *LaunchSpec) { s.Label = "c"; s.RequestID = "r9" })); code(err) != RateCapped {
			t.Fatal(err)
		}
		now = now.Add(61 * time.Minute)
		if _, err := d.Start(spec(func(s *LaunchSpec) { s.Label = "c"; s.RequestID = "r9" })); err != nil {
			t.Fatal(err)
		}
	})

	t.Run("a refused start leaves no record behind", func(t *testing.T) {
		d, _ := newDaemon(t)
		d.Start(spec(func(s *LaunchSpec) { s.Billing = "" }))
		ents, _ := os.ReadDir(d.StateDir + "/sessions")
		if len(ents) != 0 {
			t.Fatalf("%d records", len(ents))
		}
	})

	t.Run("concurrent starts of one label: exactly one wins (the lock)", func(t *testing.T) {
		d, fl := newDaemon(t)
		var wg sync.WaitGroup
		for i := 0; i < 8; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				d.Start(spec(func(s *LaunchSpec) { s.RequestID = "r" + string(rune('a'+i)) }))
			}(i)
		}
		wg.Wait()
		if fl.launch != 1 {
			t.Fatalf("launched %d", fl.launch)
		}
	})

	t.Run("a repo workdir is refused as not built, not half-done", func(t *testing.T) {
		d, _ := newDaemon(t)
		if _, err := d.Start(spec(func(s *LaunchSpec) { s.Workdir = Workdir{Repo: "github.com/x/y"} })); code(err) != NotBuilt {
			t.Fatal(err)
		}
	})
}

// U7: a resume is the same session, one generation later.
func TestResume(t *testing.T) {
	first := func(t *testing.T) (*Daemon, *fakeLauncher, *StartReply) {
		d, fl := newDaemon(t)
		rep, err := d.Start(spec(nil))
		if err != nil {
			t.Fatal(err)
		}
		fl.alive[rep.Sid] = false // the g1 pane has exited
		return d, fl, rep
	}
	res := func(sid string, mod func(*LaunchSpec)) *LaunchSpec {
		return spec(func(s *LaunchSpec) {
			s.RequestID, s.Resume = "rid-resume", &Resume{Sid: sid}
			if mod != nil {
				mod(s)
			}
		})
	}

	t.Run("resume keeps the sid and bumps the generation, so the home and transcript carry over", func(t *testing.T) {
		d, _, a := first(t)
		b, err := d.Start(res(a.Sid, nil))
		if err != nil {
			t.Fatal(err)
		}
		if b.Sid != a.Sid || b.Gen != 2 || b.ID != "tomas/hp-ubuntu/"+a.Sid+"/g2" {
			t.Fatalf("%+v", b)
		}
		rec, _ := d.Load(a.Sid)
		if rec.Gen != 2 {
			t.Fatalf("record gen %d", rec.Gen)
		}
	})

	t.Run("a session still running can't be resumed: two generations would share one home", func(t *testing.T) {
		d, fl := newDaemon(t)
		a, _ := d.Start(spec(nil))
		fl.alive[a.Sid] = true
		if _, err := d.Start(res(a.Sid, nil)); code(err) != AlreadyRunning {
			t.Fatal(err)
		}
	})

	t.Run("resume under a different label is refused: the label names the node", func(t *testing.T) {
		d, _, a := first(t)
		if _, err := d.Start(res(a.Sid, func(s *LaunchSpec) { s.Label = "other:node" })); code(err) != BadSpec {
			t.Fatal(err)
		}
	})

	t.Run("resume from another worktree is refused: the transcript is keyed by cwd", func(t *testing.T) {
		d, _, a := first(t)
		other := filepath.Join(d.WorktreeRoot, "other")
		os.Mkdir(other, 0o755)
		if _, err := d.Start(res(a.Sid, func(s *LaunchSpec) { s.Workdir.Path = other })); code(err) != BadSpec {
			t.Fatal(err)
		}
	})

	t.Run("an unknown or malformed sid is refused", func(t *testing.T) {
		d, _, _ := first(t)
		for _, sid := range []string{"s000000000", "../x", ""} {
			if _, err := d.Start(res(sid, nil)); code(err) != BadSpec {
				t.Errorf("%q: %v", sid, err)
			}
		}
	})

	t.Run("a retry of the gen-1 request after a resume must not start a new session", func(t *testing.T) {
		d, fl, a := first(t)
		d.Start(res(a.Sid, nil))
		fl.alive[a.Sid] = false
		before := fl.launch
		if _, err := d.Start(spec(nil)); err == nil || fl.launch != before {
			t.Fatalf("err=%v launches %d->%d", err, before, fl.launch)
		}
	})

	t.Run("a resume whose launch fails keeps the failed generation's number, ended, and a consistent full id", func(t *testing.T) {
		d, fl, a := first(t)
		d.Launcher = &failLauncher{fakeLauncher{alive: map[string]bool{}}}
		if _, err := d.Start(res(a.Sid, nil)); err == nil {
			t.Fatal("expected failure")
		}
		rec, _ := d.Load(a.Sid)
		if rec.Gen != 2 || rec.Ended == nil || rec.Reply.Gen != 2 || !strings.HasSuffix(rec.Reply.ID, "/g2") {
			t.Fatalf("gen %d ended %v reply %d %s", rec.Gen, rec.Ended, rec.Reply.Gen, rec.Reply.ID)
		}
		if rec.RequestID != "rid-1" {
			t.Errorf("the failed request %q must not be the record's (a retry would be answered with a success)", rec.RequestID)
		}
		// the number is not reused: the next resume is generation 3
		d.Launcher = fl
		rep, err := d.Start(res(a.Sid, func(s *LaunchSpec) { s.RequestID = "rid-after" }))
		if err != nil || rep.Gen != 3 {
			t.Fatalf("%+v %v", rep, err)
		}
	})
}

func TestParseSpec(t *testing.T) {
	t.Run("started_by is never in the body: the daemon stamps it, so an unknown field is refused", func(t *testing.T) {
		_, err := ParseSpec([]byte(`{"v":1,"request_id":"r","owner":"tomas","machine":"m","label":"l","harness":"claude","profile":"p","started_by":"root"}`))
		if err == nil {
			t.Fatal("accepted")
		}
	})
	t.Run("label with shell or path characters is refused", func(t *testing.T) {
		for _, l := range []string{"a b", "a;b", "../x", "$(x)", ""} {
			_, err := ParseSpec([]byte(`{"v":1,"request_id":"r","owner":"tomas","machine":"m","label":"` + l + `","harness":"claude","profile":"p"}`))
			if err == nil {
				t.Errorf("label %q accepted", l)
			}
		}
	})
}

type failLauncher struct{ fakeLauncher }

func (f *failLauncher) Launch(*Record) error { return os.ErrPermission }

func TestFailedLaunch(t *testing.T) {
	t.Run("a start whose pane never launched leaves no record, so a retry really retries", func(t *testing.T) {
		d, _ := newDaemon(t)
		d.Launcher = &failLauncher{fakeLauncher{alive: map[string]bool{}}}
		if _, err := d.Start(spec(nil)); err == nil {
			t.Fatal("expected failure")
		}
		d.Launcher = &fakeLauncher{alive: map[string]bool{}}
		if _, err := d.Start(spec(nil)); err != nil {
			t.Fatalf("retry: %v", err)
		}
	})
}

func TestImagePins(t *testing.T) {
	t.Run("a mutable tag outside direct mode is refused: unpinned images never run on a real fence", func(t *testing.T) {
		d, fl := newDaemon(t)
		d.Root = "helper"
		if _, err := d.Start(spec(nil)); code(err) != ImageNotPinned || fl.launch != 0 {
			t.Fatal(err)
		}
	})
	t.Run("in direct mode a local dev tag runs and the reply says channel dev", func(t *testing.T) {
		d, _ := newDaemon(t)
		rep, err := d.Start(spec(nil))
		if err != nil || rep.Pin.Channel != "dev" {
			t.Fatalf("%v %v", rep, err)
		}
	})
	t.Run("a candidate digest is refused under dev-default even in direct mode", func(t *testing.T) {
		d, _ := newDaemon(t)
		d.Image = "ghcr.io/landovsky/ccsbx-fence@sha256:" + strings.Repeat("2", 64)
		if _, err := d.Start(spec(nil)); code(err) != ImageNotPinned {
			t.Fatal(err)
		}
	})
}

// T:gitdir-from-record (daemon half). The worktree's `.git` file is written by the agent between
// generations; a resume must keep using the gitdir decided at the first start.
func TestResumeIgnoresTamperedDotGit(t *testing.T) {
	d, fl := newDaemon(t)
	wt := filepath.Join(d.WorktreeRoot, "wt")
	repo := filepath.Join(d.WorktreeRoot, "repo")
	os.Mkdir(repo, 0o755)
	for _, c := range [][]string{{"init", "-q", "-b", "main", "."}, {"commit", "-q", "--allow-empty", "-m", "i"}, {"worktree", "add", "-q", filepath.Join(d.WorktreeRoot, "lw"), "-b", "w"}} {
		cmd := exec.Command("git", c...)
		cmd.Dir = repo
		cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
		if b, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
	}
	wt = filepath.Join(d.WorktreeRoot, "lw")
	first, err := d.Start(spec(func(s *LaunchSpec) { s.Workdir.Path = wt }))
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := d.Load(first.Sid)
	want := filepath.Join(repo, ".git", "worktrees", "lw")
	if rec.GitDir != want {
		t.Fatalf("recorded gitdir %q, want %q", rec.GitDir, want)
	}
	// the agent rewrites .git to a host path
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /etc\n"), 0o644)
	fl.alive[first.Sid] = false
	if _, err := d.Start(spec(func(s *LaunchSpec) {
		s.Workdir.Path, s.RequestID, s.Resume = wt, "rid-resume", &Resume{Sid: first.Sid}
	})); err != nil {
		t.Fatal(err)
	}
	if rec, _ = d.Load(first.Sid); rec.GitDir != want {
		t.Fatalf("after resume the recorded gitdir is %q: the worktree's .git file was trusted", rec.GitDir)
	}
}

func TestFirstStartRefusesAnUnrelatedGitDirFile(t *testing.T) {
	d, fl := newDaemon(t)
	wt := filepath.Join(d.WorktreeRoot, "wt")
	os.WriteFile(filepath.Join(wt, ".git"), []byte("gitdir: /etc\n"), 0o644)
	if _, err := d.Start(spec(nil)); code(err) != BadSpec || fl.launch != 0 {
		t.Fatalf("%v launches=%d", err, fl.launch)
	}
}

func TestWorkdirOutsideRootIsRefused(t *testing.T) {
	d, _ := newDaemon(t)
	for _, p := range []string{"/home", "/var/lib", d.WorktreeRoot, filepath.Dir(d.WorktreeRoot)} {
		if _, err := d.Start(spec(func(s *LaunchSpec) { s.Workdir.Path = p })); code(err) != BadSpec {
			t.Errorf("%s: %v", p, err)
		}
	}
}

// H1-N3: the steering check must not fall back to a gitdir inside a worktree that a session record
// names (the agent may have written it).
func TestSessionForWorkdir(t *testing.T) {
	d, _ := newDaemon(t)
	rep, err := d.Start(spec(nil))
	if err != nil {
		t.Fatal(err)
	}
	wt := filepath.Join(d.WorktreeRoot, "wt")
	if sid, ok := SessionForWorkdir(d.StateDir, wt); !ok || sid != rep.Sid {
		t.Fatalf("worktree of a session not recognised: %q %v", sid, ok)
	}
	if _, ok := SessionForWorkdir(d.StateDir, d.WorktreeRoot); ok {
		t.Fatal("a directory that is not a session's worktree was matched")
	}
	if _, ok := SessionForWorkdir(t.TempDir(), wt); ok {
		t.Fatal("matched in an empty state dir")
	}
}

// context: cc-tree's LaunchSpec carries the node's Grants: line as "grants". The daemon refuses
// unknown fields (a typo must not weaken a spec), so the field must be a known one.
func TestSpecAcceptsGrantsFromCCTree(t *testing.T) {
	b, _ := os.ReadFile("../../tests/specs/hello.json")
	b = []byte(strings.Replace(string(b), `"billing":"stub",`, `"billing":"stub","grants":["g1","g2"],`, 1))
	s, err := ParseSpec(b)
	if err != nil || len(s.Grants) != 2 {
		t.Fatalf("%v %+v", err, s)
	}
}

// context: the cockpit asks `ccsbx ls` on a machine where nothing was ever started: an empty
// list, not an error.
func TestNoSessionsDirIsAnEmptyListNotAnError(t *testing.T) {
	d := &Daemon{StateDir: t.TempDir()}
	recs, err := d.Records()
	if err != nil || len(recs) != 0 {
		t.Fatalf("%v %v", recs, err)
	}
}

// context: a spec without a realm (an older cc-tree, a stub) used to journal session.* events under
// "" and ccf.* under "production", so a cockpit reading one realm missed half of a session. The
// realm is normalised once, at parse time; one the verb queue would refuse is refused at start.
func TestParseSpecNormalisesTheRealm(t *testing.T) {
	base := `{"v":1,"request_id":"r","owner":"tomas","machine":"m","label":"l","harness":"claude","profile":"p"%s}`
	s, err := ParseSpec([]byte(fmt.Sprintf(base, "")))
	if err != nil || s.Realm != "production" {
		t.Fatalf("%v %+v", err, s)
	}
	if _, err := ParseSpec([]byte(fmt.Sprintf(base, `,"realm":"Prod"`))); err == nil {
		t.Fatal("a realm the queue would refuse was accepted")
	}
}

// A reply claims evidence only when the helper has actually run on a machine without the docker
// group; the helper merely being installed (or the mode being "helper") proves nothing.
func TestReplyDoesNotClaimEvidenceFromTheModeAlone(t *testing.T) {
	d, _ := newDaemon(t)
	d.Root, d.Image = "helper", "ghcr.io/landovsky/ccsbx-fence@sha256:"+strings.Repeat("a", 64)
	os.WriteFile(d.PinsFile, []byte("[[pin]]\nref = \""+d.Image+"\"\ncc = \"2.1\"\n"), 0o644)
	rep, err := d.Start(spec(nil))
	if err != nil {
		t.Fatal(err)
	}
	if !rep.NotEv {
		t.Error("a helper-mode reply claimed evidence before the helper was proven")
	}
	d2, _ := newDaemon(t)
	d2.Root, d2.HelperProven, d2.Image, d2.PinsFile = "helper", true, d.Image, d.PinsFile
	if rep2, err := d2.Start(spec(nil)); err != nil || rep2.NotEv {
		t.Errorf("a proven helper does not get evidence: %v %v", rep2, err)
	}
}

// context: the owner is part of a session's id, which the journal and the session token trust. In
// helper mode any process that reaches hostd's socket could otherwise start a session "as" another
// user by writing that name in the spec. The owner is the socket peer's passwd name; a different one
// is refused.
func TestOwnerComesFromThePeerNotTheSpecAllow(t *testing.T) {
	lookup := func(id string) (*user.User, error) { return &user.User{Uid: id, Username: "tomas"}, nil }
	if err := CheckOwner(1000, "tomas", lookup); err != nil {
		t.Fatalf("the caller's own name was refused: %v", err)
	}
}

func TestSpecClaimingAnotherOwnerIsRefusedBlock(t *testing.T) {
	lookup := func(id string) (*user.User, error) { return &user.User{Uid: id, Username: "tomas"}, nil }
	err := CheckOwner(1000, "root", lookup)
	if r, ok := err.(*Refusal); !ok || r.Code != BadSpec {
		t.Fatalf("a spec under another owner's name was accepted: %v", err)
	}
	unknown := func(string) (*user.User, error) { return nil, os.ErrNotExist }
	if err := CheckOwner(4242, "tomas", unknown); err == nil {
		t.Fatal("a caller with no passwd entry was accepted: fail closed")
	}
}

// ---- helper mode: the daemon checks the client-resolved paths by shape ---------------------------

// context: in helper mode hostd can't enter the owner's home, so a workdir that does not exist on ITS
// filesystem must still get past the workdir/gitdir step (the client resolved it, ccsbx-root checks it
// for real). The start then fails LATER (here, at the profile), never at the workdir.
func TestHelperModeNeverStatsTheWorkdir(t *testing.T) {
	d, _ := newDaemon(t)
	d.Root, d.WorktreeRoot, d.ProfilesDir = "helper", "/nonexistent-home/u/git", "/nonexistent-profiles"
	s := spec(func(s *LaunchSpec) {
		s.Workdir.Path = "/nonexistent-home/u/git/proj"
		s.GitDir = "/nonexistent-home/u/git/proj/.git"
	})
	_, err := d.Start(s)
	if err == nil || strings.Contains(err.Error(), "workdir") || strings.Contains(err.Error(), "gitdir") {
		t.Fatalf("the start did not get past the workdir step: %v", err)
	}
}

func TestHelperModeRefusesAnOutOfShapeGitdir(t *testing.T) {
	d, _ := newDaemon(t)
	d.Root, d.WorktreeRoot = "helper", "/nonexistent-home/u/git"
	s := spec(func(s *LaunchSpec) { s.Workdir.Path = "/nonexistent-home/u/git/proj"; s.GitDir = "/etc" })
	if _, err := d.Start(s); err == nil || !strings.Contains(err.Error(), "gitdir") {
		t.Fatalf("%v", err)
	}
}

// context: in direct mode the daemon resolves the gitdir itself; a spec that brings one is refused so
// the caller can never choose what gets mounted.
func TestDirectModeRefusesASpecWithAGitdir(t *testing.T) {
	d, fl := newDaemon(t)
	s := spec(func(s *LaunchSpec) { s.GitDir = "/etc" })
	before := fl.launch
	if _, err := d.Start(s); err == nil || !strings.Contains(err.Error(), "gitdir") || fl.launch != before {
		t.Fatalf("%v", err)
	}
}

func TestResolveClientPathsResolvesSymlinksAndComputesTheGitdir(t *testing.T) {
	root := vt(t)
	real := filepath.Join(root, "real")
	os.Mkdir(real, 0o755)
	os.Symlink(real, filepath.Join(root, "link"))
	s := &LaunchSpec{Workdir: Workdir{Path: filepath.Join(root, "link")}}
	if err := ResolveClientPaths(s, os.Getuid()); err != nil || s.Workdir.Path != real || s.GitDir != "" {
		t.Fatalf("%v %+v", err, s)
	}
	if err := ResolveClientPaths(&LaunchSpec{Workdir: Workdir{Path: filepath.Join(root, "nope")}}, os.Getuid()); err == nil {
		t.Fatal("a missing workdir was accepted")
	}
}

// context: a linked worktree's gitdir has the right SHAPE under /etc too; hostd must refuse it itself
// (it must sit under a worktree root, or be <workdir>/.git), not leave it to ccsbx-root.
func TestHelperModeRefusesAGitdirOutsideTheWorktreeRoots(t *testing.T) {
	d, _ := newDaemon(t)
	d.Root, d.WorktreeRoot = "helper", "/nonexistent-home/u/git"
	s := spec(func(s *LaunchSpec) {
		s.Workdir.Path = "/nonexistent-home/u/git/proj"
		s.GitDir = "/etc/.git/worktrees/x"
	})
	if _, err := d.Start(s); err == nil || !strings.Contains(err.Error(), "not under a worktree root") {
		t.Fatalf("%v", err)
	}
}

// context (docs/m2c-remote-start.md §4): the repo form is refused not_built until the git route exists, but its
// values are validated NOW, at parse time, so that route can never receive an option, a path or a revision
// range in place of a name.
func TestTheRepoFormIsValidatedAtParseTimeBlock(t *testing.T) {
	spec := func(repo, base, branch string) []byte {
		return []byte(fmt.Sprintf(`{"v":1,"request_id":"r1","owner":"tomas","machine":"hp-ubuntu","label":"cc-sandbox:x","harness":"claude","profile":"dev-default","billing":"api","workdir":{"repo":%q,"base":%q,"branch":%q}}`, repo, base, branch))
	}
	if _, err := ParseSpec(spec("github.com/landovsky/cc-sandbox", "main", "ccsbx/326q")); err != nil {
		t.Fatalf("a good repo form: %v", err)
	}
	for name, c := range map[string][3]string{
		"a base that is an option":    {"github.com/a/b", "--upload-pack=x", "br"},
		"a branch that is an option":  {"github.com/a/b", "main", "-b"},
		"a revision range":            {"github.com/a/b", "a..b", "br"},
		"a doubled slash":             {"github.com/a/b", "main", "x//y"},
		"a lock suffix":               {"github.com/a/b", "main", "x.lock"},
		"a trailing slash":            {"github.com/a/b", "main/", "br"},
		"an @{ reflog form":           {"github.com/a/b", "main@{1}", "br"},
		"a repo that is another host": {"gitlab.com/a/b", "main", "br"},
		"a repo that is a url":        {"https://github.com/a/b", "main", "br"},
		"a repo with a path escape":   {"github.com/../b", "main", "br"},
		"an empty base":               {"github.com/a/b", "", "br"},
	} {
		if _, err := ParseSpec(spec(c[0], c[1], c[2])); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// sub makes (and returns) a sibling workdir under base: sessions that are live at once need workdirs that do not overlap.
func sub(t *testing.T, base, name string) string {
	t.Helper()
	p := filepath.Join(base, name)
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// context (one workdir, one live session): an outer tree's agent could swap an inner directory for a link between the
// check and the mount, so a workdir that equals, contains or sits inside another live session's is refused.
func TestOverlappingWorkdirsOfLiveSessionsAreRefused(t *testing.T) {
	d, _ := newDaemon(t)
	base := spec(nil).Workdir.Path
	a := sub(t, base, "outer")
	if _, err := d.Start(spec(func(s *LaunchSpec) { s.Label = "a"; s.RequestID = "r1"; s.Workdir.Path = a })); err != nil {
		t.Fatal(err)
	}
	for name, p := range map[string]string{"the same directory": a, "inside it": sub(t, a, "inner"), "containing it": base} {
		if _, err := d.Start(spec(func(s *LaunchSpec) { s.Label = "b"; s.RequestID = "r2" + name; s.Workdir.Path = p })); code(err) != BadSpec {
			t.Errorf("%s: %v", name, err)
		}
	}
	if _, err := d.Start(spec(func(s *LaunchSpec) { s.Label = "c"; s.RequestID = "r3"; s.Workdir.Path = sub(t, base, "sibling") })); err != nil {
		t.Errorf("a sibling was refused: %v", err)
	}
}

// context (R-H, docs/m2c-remote-start.md §4b): in helper mode hostd cannot look inside the owner's home, so a remote
// start asks root whether the path is a git worktree root. Any doubt is a refusal, and with no checker there is no
// remote start at all; a local start never asks.
func TestAHelperModeRemoteStartAsksRootAndRefusesOnAnyDoubt(t *testing.T) {
	d, _ := newDaemon(t)
	d.Root, d.StartedBy = "helper", "owner:tomas@example.com@mac"
	helperImage(d)
	d.WorktreeRoot = filepath.Dir(spec(nil).Workdir.Path)
	p := spec(nil).Workdir.Path
	if _, err := d.Start(spec(nil)); code(err) != NotBuilt {
		t.Fatalf("no checker: %v", err)
	}
	d.WorktreeCheck = func(string) (string, string, error) { return "", "", ErrNoHelper }
	if _, err := d.Start(spec(func(s *LaunchSpec) { s.RequestID = "r-nohelper" })); code(err) != NotBuilt {
		t.Fatalf("no helper installed must be not_built, not bad_spec: %v", err)
	}
	for name, chk := range map[string]func(string) (string, string, error){
		"root refuses":              func(string) (string, string, error) { return "", "", errors.New("exit 2") },
		"root cannot tell":          func(string) (string, string, error) { return "", "", errors.New("exit 3") },
		"root answers another path": func(string) (string, string, error) { return "/etc", "", nil },
	} {
		d.WorktreeCheck = chk
		if _, err := d.Start(spec(func(s *LaunchSpec) { s.RequestID = "r-" + name })); code(err) != BadSpec {
			t.Errorf("%s: %v", name, err)
		}
	}
	asked := ""
	d.WorktreeCheck = func(q string) (string, string, error) { asked = q; return q, filepath.Join(q, ".git"), nil }
	rep, err := d.Start(spec(func(s *LaunchSpec) { s.RequestID = "r-ok"; s.Profile, s.Billing = "e2e-default", "stub" }))
	if asked != p {
		t.Fatalf("root was asked about %q, not the spec's path %q (err %v)", asked, p, err)
	}
	if err != nil {
		t.Fatalf("an approved path was refused: %v", err)
	}
	// the gitdir root derived is what the record keeps: `ccsbx-root run` compares it with its own derivation
	if rec, lerr := d.Load(rep.Sid); lerr != nil || rec.GitDir != filepath.Join(p, ".git") {
		t.Fatalf("the record's gitdir is %q (%v), want the one root derived", rec.GitDir, lerr)
	}
}

// context (R-H review S4): a session whose pane died but whose record is not marked ended counts as live to root, so a
// remote resume or restart on the same workdir would be refused as an overlap. The dead record is ended BEFORE root is asked.
func TestAStaleLiveRecordIsEndedBeforeRootIsAsked(t *testing.T) {
	d, fl := newDaemon(t)
	d.Root, d.StartedBy = "helper", "owner:tomas@example.com@mac"
	helperImage(d)
	d.WorktreeRoot = filepath.Dir(spec(nil).Workdir.Path)
	d.WorktreeCheck = func(q string) (string, string, error) { return q, filepath.Join(q, ".git"), nil }
	first, err := d.Start(spec(func(s *LaunchSpec) { s.Profile, s.Billing = "e2e-default", "stub" }))
	if err != nil {
		t.Fatal(err)
	}
	for sid := range fl.alive {
		fl.alive[sid] = false // the pane died; the record still says live
	}
	sawLive := false
	d.WorktreeCheck = func(q string) (string, string, error) {
		if r, _ := d.Load(first.Sid); r != nil && r.Ended == nil {
			sawLive = true
		}
		return q, filepath.Join(q, ".git"), nil
	}
	if _, err := d.Start(spec(func(s *LaunchSpec) {
		s.RequestID = "r2"
		s.Label = "cc-sandbox:again"
		s.Profile, s.Billing = "e2e-default", "stub"
	})); err != nil {
		t.Fatal(err)
	}
	if sawLive {
		t.Fatal("root was asked while the dead session's record still said live")
	}
}

// helperImage makes helper mode's start acceptable to the image pin check: an e2e profile may run a candidate digest of our repo.
func helperImage(d *Daemon) {
	d.Image = "ghcr.io/landovsky/ccsbx-fence@sha256:" + strings.Repeat("a", 64)
	d.ProfileLoader = profiles.Load
}

// context (R-H round 2): root's exit 3 ("could not tell") is this machine's trouble, not a bad spec: internal, not bad_spec.
func TestACheckThatCouldNotTellIsInternalNotBadSpec(t *testing.T) {
	d, _ := newDaemon(t)
	d.Root, d.StartedBy = "helper", "owner:tomas@example.com@mac"
	helperImage(d)
	d.WorktreeRoot = filepath.Dir(spec(nil).Workdir.Path)
	d.WorktreeCheck = func(string) (string, string, error) { return "", "", ErrCannotTell }
	if _, err := d.Start(spec(func(s *LaunchSpec) { s.Profile, s.Billing = "e2e-default", "stub" })); code(err) != "internal" {
		t.Fatalf("%v", err)
	}
}
