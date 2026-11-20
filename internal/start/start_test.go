// Tests for start, resume and spec parsing. They guard against a caller-supplied started_by, a resume that
// changes the sid or skips the generation bump, and a failed launch leaving a live record.

package start

import (
	"os"
	"os/exec"
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
			if _, err := d.Start(spec(func(s *LaunchSpec) { s.Label = l; s.RequestID = "r" + string(rune('0'+i)) })); err != nil {
				t.Fatal(err)
			}
		}
		if _, err := d.Start(spec(func(s *LaunchSpec) { s.Label = "c"; s.RequestID = "r9" })); code(err) != MachineFull {
			t.Fatal(err)
		}
	})

	t.Run("rate_capped: a runaway caller can't start sessions without limit, and the hour rolls", func(t *testing.T) {
		d, fl := newDaemon(t)
		d.MaxPerHour = 2
		now := time.Now()
		d.Now = func() time.Time { return now }
		for i, l := range []string{"a", "b"} {
			d.Start(spec(func(s *LaunchSpec) { s.Label = l; s.RequestID = "r" + string(rune('0'+i)) }))
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

	t.Run("a resume whose launch fails leaves the session at its old generation", func(t *testing.T) {
		d, _, a := first(t)
		d.Launcher = &failLauncher{fakeLauncher{alive: map[string]bool{}}}
		if _, err := d.Start(res(a.Sid, nil)); err == nil {
			t.Fatal("expected failure")
		}
		if rec, _ := d.Load(a.Sid); rec.Gen != 1 {
			t.Fatalf("gen %d", rec.Gen)
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
