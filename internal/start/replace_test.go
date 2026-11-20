// Tests of Daemon.Replace (C9b, session.replace): why they exist: replace ends a live pane and
// starts generation+1, so every failure along the way must leave a record that says what is true,
// and two fences must never hold the same workdir and home at once.
package start

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// stopLauncher is a launcher whose panes can be stopped, as TmuxLauncher's can.
type stopLauncher struct {
	fakeLauncher
	d        *Daemon
	stopErr  error
	launchEr error
	closes   bool // the stopped generation's fence writes its CLOSED marker
	stopped  int
}

func (l *stopLauncher) Stop(sid string) error {
	l.stopped++
	if l.stopErr != nil {
		return l.stopErr
	}
	l.mu.Lock()
	l.alive[sid] = false
	l.mu.Unlock()
	if l.closes {
		rec, _ := l.d.Load(sid)
		g := filepath.Join(l.d.runRoot(), "s", sid, "g"+string(rune('0'+rec.Gen)))
		os.MkdirAll(g, 0o700)
		os.WriteFile(filepath.Join(g, "CLOSED"), []byte("closed\n"), 0o600)
	}
	return nil
}
func (l *stopLauncher) Launch(r *Record) error {
	if l.launchEr != nil {
		return l.launchEr
	}
	return l.fakeLauncher.Launch(r)
}

func running(t *testing.T) (*Daemon, *stopLauncher, string) {
	t.Helper()
	d, fl := newDaemon(t)
	sl := &stopLauncher{fakeLauncher: fakeLauncher{alive: fl.alive}, d: d, closes: true}
	d.Launcher, d.ReplaceWait = sl, 300*time.Millisecond
	rep, err := d.Start(spec(nil))
	if err != nil {
		t.Fatal(err)
	}
	return d, sl, rep.Sid
}

func TestReplaceStartsTheNextGenerationAfterTheOldOneClosed(t *testing.T) {
	d, sl, sid := running(t)
	before, _ := d.Load(sid)
	time.Sleep(10 * time.Millisecond)
	rec, err := d.Replace(sid, "new brief")
	if err != nil {
		t.Fatal(err)
	}
	if rec.Gen != 2 || rec.Spec.Brief == nil || rec.Spec.Brief.Text != "new brief" || !rec.Started.After(before.Started) || rec.Ended != nil {
		t.Fatalf("%+v", rec)
	}
	if !sl.Alive(sid) || sl.stopped != 1 {
		t.Fatal("the new pane is not running, or the old one was not stopped once")
	}
}

func TestReplaceWhoseStopFailsChangesNothing(t *testing.T) {
	d, sl, sid := running(t)
	sl.stopErr = errors.New("tmux: boom")
	if _, err := d.Replace(sid, "b"); err == nil {
		t.Fatal("expected failure")
	}
	if rec, _ := d.Load(sid); rec.Gen != 1 || rec.Ended != nil || rec.Spec.Brief != nil {
		t.Fatalf("record changed: %+v", rec)
	}
}

// context: the old fence has not shut down (its container may still hold the workdir and the
// transcript read-write). Starting generation 2 now would put two writers on them.
func TestReplaceDoesNotStartWhileTheOldFenceIsStillUp(t *testing.T) {
	d, sl, sid := running(t)
	sl.closes = false
	launched := sl.launch
	if _, err := d.Replace(sid, "b"); err == nil {
		t.Fatal("expected failure")
	}
	if sl.launch != launched {
		t.Fatal("generation 2 was launched while generation 1's fence was still up")
	}
	if rec, _ := d.Load(sid); rec.Gen != 1 {
		t.Fatalf("gen %d", rec.Gen)
	}
}

// context: the launch of generation 2 fails (binary missing, tmux error). The pane is gone, so
// the record must say the session ended, not claim a generation that has no pane.
func TestReplaceWhoseLaunchFailsMarksTheSessionEnded(t *testing.T) {
	d, sl, sid := running(t)
	sl.launchEr = errors.New("no ccsbx binary")
	if _, err := d.Replace(sid, "b"); err == nil {
		t.Fatal("expected failure")
	}
	rec, _ := d.Load(sid)
	// the number is not given back (the doors refuse every token at or below a revoked generation), and the
	// record's full id names the generation it is at
	if rec.Gen != 2 || rec.Ended == nil || rec.Reply.Gen != 2 || !strings.HasSuffix(rec.Reply.ID, "/g2") {
		t.Fatalf("record after a failed replace: gen %d ended %v reply %d %s", rec.Gen, rec.Ended, rec.Reply.Gen, rec.Reply.ID)
	}
}

// context: in helper mode the generation's CLOSED marker is ccsbx-root's, in the run dir, not in the
// state dir; Replace must wait for it THERE before it starts generation+1 (two fences must never hold
// one workdir and home at once).
func TestReplaceWaitsForTheCloseMarkerInTheRunDir(t *testing.T) {
	d, sl, sid := running(t)
	d.RunDir = t.TempDir()
	if _, err := d.Replace(sid, "b"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(d.RunDir, "s", sid, "g1", "CLOSED")); err != nil {
		t.Fatalf("the marker the launcher wrote is not in the run dir: %v", err)
	}
	if !sl.Alive(sid) {
		t.Fatal("generation 2 is not running")
	}
}
