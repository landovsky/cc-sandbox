// The proof that a session's fence is stopped, which gitroute.Commit demands before the host
// runs git that reads the worktree. The daemon owns the session records and the launcher, and the
// per-session lock (lock.go) is what KEEPS the answer true: the proof takes it when Commit asks and
// gives it back when the commit is done, so a fence (ccsbx-root run, ccsbx run) and a start or
// resume of the same session are refused while the host reads the worktree. The daemon-wide lock is
// not held (a commit of a big worktree would stall every other session's start).
//
// "Stopped" means: the record exists, no pane is alive, and the runtime says neither the current
// generation's container nor the previous one's is running; then the session lock is taken without
// waiting. Not being able to find any of that out is a refusal.
package start

import (
	"fmt"

	"github.com/landovsky/cc-sandbox/internal/gitroute"
)

type stoppedProof struct {
	d   *Daemon
	sid string
}

// StoppedProof returns the proof for sid; the check runs when Commit calls Hold.
func (d *Daemon) StoppedProof(sid string) gitroute.Stopped { return stoppedProof{d, sid} }

func (p stoppedProof) Hold() (func(), error) {
	d := p.d
	r, err := d.Load(p.sid)
	if err != nil {
		return nil, fmt.Errorf("no such session %q", p.sid)
	}
	if r.Ended == nil && d.Launcher != nil && d.Launcher.Alive(p.sid) {
		return nil, fmt.Errorf("session %s still has a running pane", p.sid)
	}
	if d.Runtime == nil {
		return nil, fmt.Errorf("session %s: no way to ask whether its container is running (refusing)", p.sid)
	}
	// One question: the runtime answers for the generation and the one before it. It is asked BEFORE
	// the lock is taken, on purpose and not by oversight: in helper mode this is also what makes
	// ccsbx-root create the session's lock file (the daemon may only open it), and root's status
	// reads a lock that is already held as "running", so asking after taking it would always say so.
	// The window between the answer and the lock cannot be used to start a fence: a fence takes the
	// same lock, and a container left behind by a fence that has just exited is removed (deferred)
	// before root lets go of its lock.
	running, err := d.Runtime.Running(p.sid, r.Gen)
	if err != nil {
		return nil, fmt.Errorf("session %s: cannot tell whether it is running: %v", p.sid, err)
	}
	if running {
		return nil, fmt.Errorf("session %s has a running container", p.sid)
	}
	release, err := d.TrySessionLock(p.sid)
	if err != nil {
		return nil, fmt.Errorf("session %s: %v", p.sid, err)
	}
	return release, nil
}
