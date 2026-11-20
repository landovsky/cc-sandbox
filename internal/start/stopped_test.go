// Tests for the stopped-fence proof: it must say no while a pane is alive, the session is unknown,
// or a container is running (a session started with --launcher none has no pane at all), and it
// must hold the session lock until the commit is done, because a yes lets the host stage files from
// a worktree an agent might still be writing.
package start

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
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

// R6: no pane (as with --launcher none) but the container runs: not stopped. A runtime that cannot
// answer is a refusal, as is none.
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
	rt.running, rt.err = map[int]bool{}, fmt.Errorf("docker is down")
	if err := try(); err == nil || !strings.Contains(err.Error(), "cannot tell") {
		t.Errorf("a runtime that could not answer was taken as stopped: %v", err)
	}
	rt.err = nil
	rt.asked = nil
	if err := try(); err != nil || len(rt.asked) != 1 {
		t.Errorf("stopped everywhere: %v, the runtime was asked %d times (want 1)", err, len(rt.asked))
	}
	d.Runtime = nil
	if err := try(); err == nil || !strings.Contains(err.Error(), "no way to ask") {
		t.Errorf("a daemon with no runtime gave a proof: %v", err)
	}
}

// R6/S2: the per-session lock is held from the proof until the release. While it is, a second proof
// (a second commit, or the same caller asking again: the lock is NOT re-entrant), a resume of the
// session and a fence of it are refused with a clear error; a start of ANOTHER session, which the
// daemon-wide lock would have stalled, is not affected.
func TestStoppedProofHoldsOnlyTheSessionsLock(t *testing.T) {
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
	if _, err := d.StoppedProof(rep.Sid).Hold(); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("a second hold of the same session did not fail with a clear busy error: %v", err)
	}
	if rel, err := d.TrySessionLock(rep.Sid); err == nil {
		rel()
		t.Error("a fence could take the lock while a commit held it")
	}
	// a resume of this session is refused...
	_, err = d.Start(spec(func(s *LaunchSpec) { s.RequestID = "rid-2"; s.Resume = &Resume{Sid: rep.Sid} }))
	if r, ok := err.(*Refusal); !ok || r.Code != SessionBusy {
		t.Errorf("a resume during a commit: %v", err)
	}
	// ...while another session starts at once (nothing daemon-wide is held)
	done := make(chan error, 1)
	go func() {
		_, err := d.Start(spec(func(s *LaunchSpec) { s.RequestID = "rid-3"; s.Label = "cc-sandbox:b" }))
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("another session's start failed: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("another session's start waited for the commit")
	}
	release()
	rel, err := d.TrySessionLock(rep.Sid)
	if err != nil {
		t.Fatalf("the lock was not released: %v", err)
	}
	// a fence holding the lock refuses the proof, the other way round
	if _, err := d.StoppedProof(rep.Sid).Hold(); err == nil || !strings.Contains(err.Error(), "busy") {
		t.Errorf("a proof was given while a fence held the lock: %v", err)
	}
	rel()
	// and a refused proof leaves nothing held
	d.Runtime = &fakeRuntime{running: map[int]bool{1: true}}
	if _, err := d.StoppedProof(rep.Sid).Hold(); err == nil {
		t.Fatal("expected a refusal")
	}
	if rel, err := d.TrySessionLock(rep.Sid); err != nil {
		t.Errorf("a refused proof left the lock held: %v", err)
	} else {
		rel()
	}
}

// Helper mode: the lock file is root's. The daemon opens it read-only, never creates it and never
// follows a link.
func TestHelperModeLockFileIsOpenedReadOnlyAndNeverCreated(t *testing.T) {
	d, _ := newDaemon(t)
	d.Root, d.RunDir = "helper", t.TempDir()
	if _, err := d.TrySessionLock("s0abcdefgh"); err == nil || !strings.Contains(err.Error(), "ccsbx-root creates it") {
		t.Errorf("a missing lock file in helper mode: %v", err)
	}
	if _, err := os.Stat(d.lockPath("s0abcdefgh")); err == nil {
		t.Error("the daemon created root's lock file")
	}
	os.MkdirAll(filepath.Join(d.RunDir, "locks"), 0o755)
	os.WriteFile(d.lockPath("s0abcdefgh"), nil, 0o644)
	rel, err := d.TrySessionLock("s0abcdefgh")
	if err != nil {
		t.Fatalf("a root-made lock file could not be locked read-only: %v", err)
	}
	rel()
	os.Remove(d.lockPath("s0abcdefgh"))
	os.Symlink("/etc/hostname", d.lockPath("s0abcdefgh"))
	if _, err := d.TrySessionLock("s0abcdefgh"); err == nil {
		t.Error("a symlinked lock file was followed")
	}
}

// NEW-S2: in helper mode a session whose lock file root has not made yet (recorded before the lock
// existed, or its first run was refused, or it never ran) is NOT busy: resume must work, and the
// fence it launches creates and takes the lock itself. The proof, which reads the worktree, still
// refuses without the file.
func TestHelperModeResumeWorksWithoutALockFile(t *testing.T) {
	d, fl := newDaemon(t)
	d.Root, d.RunDir, d.Runtime = "helper", t.TempDir(), &fakeRuntime{}
	d.Image = "ghcr.io/landovsky/ccsbx-fence@sha256:" + strings.Repeat("a", 64)
	os.WriteFile(d.PinsFile, []byte("[[pin]]\nref = \""+d.Image+"\"\ncc = \"2.1\"\n"), 0o644)
	rep, err := d.Start(spec(nil))
	if err != nil {
		t.Fatal(err)
	}
	fl.mu.Lock()
	fl.alive[rep.Sid] = false
	fl.mu.Unlock()
	if _, err := os.Stat(d.lockPath(rep.Sid)); err == nil {
		t.Fatal("the test needs a session with no lock file")
	}
	if _, err := d.Start(spec(func(s *LaunchSpec) { s.RequestID = "rid-2"; s.Resume = &Resume{Sid: rep.Sid} })); err != nil {
		t.Errorf("a resume was refused because root's lock file is missing: %v", err)
	}
	fl.mu.Lock()
	fl.alive[rep.Sid] = false // the resumed generation's pane is gone again
	fl.mu.Unlock()
	if _, err := d.StoppedProof(rep.Sid).Hold(); err == nil || !strings.Contains(err.Error(), "no lock file") {
		t.Errorf("the proof was given without a lock file: %v", err)
	}
	// but a lock file that IS held still refuses the resume
	os.MkdirAll(filepath.Dir(d.lockPath(rep.Sid)), 0o755)
	f, _ := os.OpenFile(d.lockPath(rep.Sid), os.O_RDWR|os.O_CREATE, 0o640)
	defer f.Close()
	syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	_, err = d.Start(spec(func(s *LaunchSpec) { s.RequestID = "rid-3"; s.Resume = &Resume{Sid: rep.Sid} }))
	if r, ok := err.(*Refusal); !ok || r.Code != SessionBusy {
		t.Errorf("a held lock did not refuse the resume: %v", err)
	}
}
