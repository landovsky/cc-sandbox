// Tests for the stopped-fence proof: it must say no while a pane is alive, the session is unknown,
// or a container is running (a session started with --launcher none has no pane at all), and it
// must hold the session lock until the commit is done, because a yes lets the host stage files from
// a worktree an agent might still be writing.
package start

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeRuntime struct {
	mu      sync.Mutex
	running map[int]bool
	err     error
	asked   []int
}

func (f *fakeRuntime) Running(sid string, gen int) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.asked = append(f.asked, gen)
	return f.running[gen], f.err
}

func TestStoppedProof(t *testing.T) {
	d, fl := newDaemon(t)
	d.Runtime = &fakeRuntime{}
	rep, err := d.Start(spec(nil))
	if err != nil {
		t.Fatal(err)
	}
	hold := func(sid string) error {
		release, err := d.StoppedProof(sid).Hold()
		if err == nil {
			release()
		}
		return err
	}
	if err := hold(rep.Sid); err == nil || !strings.Contains(err.Error(), "pane") {
		t.Errorf("a running session was reported as stopped: %v", err)
	}
	if err := hold("s-unknown"); err == nil {
		t.Error("an unknown session was reported as stopped")
	}
	fl.mu.Lock()
	fl.alive[rep.Sid] = false
	fl.mu.Unlock()
	if err := hold(rep.Sid); err != nil {
		t.Errorf("a session whose pane is gone and container is not running was not reported as stopped: %v", err)
	}
}

// R6: no pane (as with --launcher none) but the container runs: not stopped. The previous
// generation's container counts too, and a runtime that cannot answer is a refusal, as is none.
func TestStoppedProofAsksTheRuntime(t *testing.T) {
	d, fl := newDaemon(t)
	rt := &fakeRuntime{running: map[int]bool{}}
	d.Runtime = rt
	rep, err := d.Start(spec(nil))
	if err != nil {
		t.Fatal(err)
	}
	fl.mu.Lock()
	fl.alive[rep.Sid] = false // no pane
	fl.mu.Unlock()
	try := func() error {
		release, err := d.StoppedProof(rep.Sid).Hold()
		if err == nil {
			release()
		}
		return err
	}
	rt.running[rep.Gen] = true
	if err := try(); err == nil || !strings.Contains(err.Error(), "running container") {
		t.Errorf("a running container with no pane was reported as stopped: %v", err)
	}
	rec, _ := d.Load(rep.Sid)
	rec.Gen = 2 // as after a resume: generation 1 must be gone too
	if err := d.save(rec); err != nil {
		t.Fatal(err)
	}
	rt.running = map[int]bool{1: true}
	if err := try(); err == nil || !strings.Contains(err.Error(), "generation 1") {
		t.Errorf("the previous generation's container was not considered: %v", err)
	}
	rt.running, rt.err = map[int]bool{}, fmt.Errorf("docker is down")
	if err := try(); err == nil || !strings.Contains(err.Error(), "cannot tell") {
		t.Errorf("a runtime that could not answer was taken as stopped: %v", err)
	}
	rt.err = nil
	if err := try(); err != nil {
		t.Errorf("stopped everywhere was refused: %v", err)
	}
	d.Runtime = nil
	if err := try(); err == nil || !strings.Contains(err.Error(), "no way to ask") {
		t.Errorf("a daemon with no runtime gave a proof: %v", err)
	}
}

// R6: the lock is held from the proof until the commit is done: a start or resume waits for it.
func TestStoppedProofHoldsTheSessionLockUntilReleased(t *testing.T) {
	d, fl := newDaemon(t)
	d.Runtime = &fakeRuntime{}
	rep, err := d.Start(spec(nil))
	if err != nil {
		t.Fatal(err)
	}
	fl.mu.Lock()
	fl.alive[rep.Sid] = false
	fl.mu.Unlock()
	release, err := d.StoppedProof(rep.Sid).Hold()
	if err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := d.Start(spec(func(s *LaunchSpec) { s.RequestID = "rid-2"; s.Label = "cc-sandbox:b" }))
		done <- err
	}()
	select {
	case <-done:
		t.Fatal("a start ran while the stopped proof was held")
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("the start failed after the release: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the start never ran after the release")
	}
	// and a refused proof does not leave the lock held
	d.Runtime = &fakeRuntime{running: map[int]bool{1: true}}
	if _, err := d.StoppedProof(rep.Sid).Hold(); err == nil {
		t.Fatal("expected a refusal")
	}
	if _, err := d.Start(spec(func(s *LaunchSpec) { s.RequestID = "rid-3"; s.Label = "cc-sandbox:c" })); err != nil {
		t.Errorf("a refused proof left the lock held: %v", err)
	}
}
