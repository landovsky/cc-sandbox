// lock.go is the per-session lock. It exists so that "no generation is running" can be KEPT true
// for as long as the host reads a worktree (the stopped proof, held across `git add` and the
// commit) without holding the daemon-wide lock, which every start and replace of every session
// would wait on. A fence holds the lock while it runs (ccsbx-root run, `ccsbx run`), a commit holds
// it while it stages and commits, so each excludes the other; neither waits: the second one is
// refused with ErrBusy. The lock is a flock on <lockdir>/<sid>.lock, per open file description:
// it is NOT re-entrant, a second attempt from the same process is refused too (a caller that
// already holds it must not ask again).
//
// The lock directory is <run dir>/locks. In helper mode it is root's (root creates the file; the
// daemon only opens it read-only, which is enough to flock it); in direct mode the daemon makes it.
package start

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

// ErrBusy: the session's lock is held (a generation is running, or a commit is in progress).
var ErrBusy = errors.New("session is busy: a generation is running or a commit is in progress")

// ErrNoLockFile: helper mode, and root has not made the session's lock file yet (no generation has
// been started for it by ccsbx-root, and ccsbx-root status has not been asked).
var ErrNoLockFile = errors.New("no lock file yet (ccsbx-root creates it)")

func (d *Daemon) lockPath(sid string) string {
	return filepath.Join(d.runRoot(), "locks", sid+".lock")
}

// TrySessionLock takes the session's lock without waiting. The release function drops it.
func (d *Daemon) TrySessionLock(sid string) (func(), error) {
	if !ValidSid(sid) {
		return nil, fmt.Errorf("%q is not a session id", sid)
	}
	p := d.lockPath(sid)
	var f *os.File
	var err error
	if d.Root == "helper" {
		// root's file in root's directory: never create or follow anything here
		f, err = os.OpenFile(p, os.O_RDONLY|syscall.O_NOFOLLOW, 0)
		if os.IsNotExist(err) {
			return nil, fmt.Errorf("%w: %s", ErrNoLockFile, p)
		}
	} else {
		if err = os.MkdirAll(filepath.Dir(p), 0o700); err == nil {
			f, err = os.OpenFile(p, os.O_RDWR|os.O_CREATE|syscall.O_NOFOLLOW, 0o600)
		}
	}
	if err != nil {
		return nil, err
	}
	if st, err := f.Stat(); err != nil || !st.Mode().IsRegular() {
		f.Close()
		return nil, fmt.Errorf("%s is not a regular file", p)
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, ErrBusy
		}
		return nil, err
	}
	return func() { syscall.Flock(int(f.Fd()), syscall.LOCK_UN); f.Close() }, nil
}
