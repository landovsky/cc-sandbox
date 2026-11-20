// The proof that a session's fence is stopped, which gitroute.Commit demands before the host
// runs git that reads the worktree. The daemon owns the session records, the launcher and the
// session lock, so it is the one place that can say "no generation is running" AND keep it true: the
// proof takes the session lock when Commit asks and gives it back when the commit is done, so a
// start or a resume (which take the same lock) wait for the commit instead of racing it.
//
// "Stopped" means: the record exists, no pane is alive, and the runtime says neither the current
// generation's container nor the previous one's is running. Not being able to find that out is a
// refusal.
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
	unlock, err := d.lock()
	if err != nil {
		return nil, fmt.Errorf("session lock: %v", err)
	}
	fail := func(format string, a ...any) (func(), error) {
		unlock()
		return nil, fmt.Errorf(format, a...)
	}
	r, err := d.Load(p.sid)
	if err != nil {
		return fail("session %s: %v", p.sid, err)
	}
	if r.Ended == nil && d.Launcher != nil && d.Launcher.Alive(p.sid) {
		return fail("session %s still has a running pane", p.sid)
	}
	if d.Runtime == nil {
		return fail("session %s: no way to ask whether its container is running (refusing)", p.sid)
	}
	for g := r.Gen; g >= 1 && g >= r.Gen-1; g-- {
		running, err := d.Runtime.Running(p.sid, g)
		if err != nil {
			return fail("session %s: cannot tell whether generation %d is running: %v", p.sid, g, err)
		}
		if running {
			return fail("session %s: generation %d has a running container", p.sid, g)
		}
	}
	return unlock, nil
}
