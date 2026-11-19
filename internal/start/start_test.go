package start

import (
	"os"
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

func newDaemon(t *testing.T) (*Daemon, *fakeLauncher) {
	t.Helper()
	fl := &fakeLauncher{alive: map[string]bool{}}
	return &Daemon{StateDir: t.TempDir(), ProfilesDir: "../../profiles", Machine: "hp-ubuntu", Uid: 1000,
		MaxLive: 8, MaxPerHour: 20, Backend: "docker", Root: "direct", Launcher: fl, Exe: "ccsbx"}, fl
}

func spec(mod func(*LaunchSpec)) *LaunchSpec {
	s := &LaunchSpec{V: 1, RequestID: "rid-1", Owner: "tomas", Machine: "hp-ubuntu", Label: "cc-sandbox:a", Realm: "production",
		Harness: "claude", Workdir: Workdir{Path: os.TempDir()}, Profile: "dev-default", Billing: "stub"}
	if mod != nil {
		mod(s)
	}
	return s
}

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

	t.Run("resume and repo workdirs are refused as not built, not half-done", func(t *testing.T) {
		d, _ := newDaemon(t)
		if _, err := d.Start(spec(func(s *LaunchSpec) { s.Resume = &Resume{Sid: "s123456"} })); code(err) != NotBuilt {
			t.Fatal(err)
		}
		if _, err := d.Start(spec(func(s *LaunchSpec) { s.Workdir = Workdir{Repo: "github.com/x/y"} })); code(err) != NotBuilt {
			t.Fatal(err)
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
