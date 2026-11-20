// The proof that a session's fence is stopped, which gitroute.Commit demands before the host
// runs git that reads the worktree. The daemon owns the session records and the launcher, so it
// is the one place that can say "no generation is running": a record that has ended, or whose
// pane is gone. Anything else (unknown sid, live pane) is not a proof.
package start

import (
	"fmt"

	"github.com/landovsky/cc-sandbox/internal/gitroute"
)

// StoppedProof returns the proof for sid; the check itself runs when Commit asks for it.
func (d *Daemon) StoppedProof(sid string) gitroute.Stopped {
	return gitroute.StoppedFunc(func() error {
		r, err := d.Load(sid)
		if err != nil {
			return fmt.Errorf("session %s: %v", sid, err)
		}
		if r.Ended == nil && d.Launcher.Alive(sid) {
			return fmt.Errorf("session %s still has a running pane", sid)
		}
		return nil
	})
}
