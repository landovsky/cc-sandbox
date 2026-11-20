// peer_linux.go reads who is on the other end of a unix socket (SO_PEERCRED). It exists because the
// signer's only access control is "which local user is asking": the kernel supplies the answer, so
// the client cannot claim another identity.
package signer

import (
	"errors"
	"net"
	"syscall"
)

// peerCred returns the uid and gid of the process that connected.
func peerCred(c net.Conn) (uid, gid uint32, err error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, 0, errors.New("signer: not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, 0, err
	}
	var cred *syscall.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, 0, err
	}
	if serr != nil {
		return 0, 0, serr
	}
	return cred.Uid, cred.Gid, nil
}
