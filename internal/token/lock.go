// lock.go makes a door's state file exclusive to one running door. It exists because two doors with
// the same audience (two stubs) shared one file, and each rewrite could drop the other's revocation.
package token

import (
	"fmt"
	"os"
	"syscall"
)

// Close releases the state file so another door (a restart) may take it.
func (v *Verifier) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.lock != nil {
		v.lock.Close()
		v.lock = nil
	}
}

// lockState takes a non-blocking exclusive lock on path+".lock" and keeps it open until exit.
func lockState(path string) (*os.File, error) {
	f, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		return nil, fmt.Errorf("token: state file %s is in use by another door (give each door its own --state)", path)
	}
	return f, nil
}
