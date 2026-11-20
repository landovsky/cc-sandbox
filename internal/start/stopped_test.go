// Tests for the stopped-fence proof: it must say no while a pane is alive or the session is
// unknown, because a yes lets the host stage files from a worktree an agent is still writing.
package start

import "testing"

func TestStoppedProof(t *testing.T) {
	d, fl := newDaemon(t)
	rep, err := d.Start(spec(nil))
	if err != nil {
		t.Fatal(err)
	}
	if err := d.StoppedProof(rep.Sid).FenceStopped(); err == nil {
		t.Error("a running session was reported as stopped")
	}
	if err := d.StoppedProof("s-unknown").FenceStopped(); err == nil {
		t.Error("an unknown session was reported as stopped")
	}
	fl.mu.Lock()
	fl.alive[rep.Sid] = false
	fl.mu.Unlock()
	if err := d.StoppedProof(rep.Sid).FenceStopped(); err != nil {
		t.Errorf("a session whose pane is gone was not reported as stopped: %v", err)
	}
}
