// peer_linux.go reads the uid of the process on the other end of a unix socket (SO_PEERCRED). It exists
// because the sidecar's control socket accepts secrets from one local identity only, and the kernel,
// not the client, says who that is.
package side

import (
	"errors"
	"net"
	"syscall"
)

func peerUID(c net.Conn) (uint32, error) {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return 0, errors.New("side: not a unix connection")
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return 0, err
	}
	var cred *syscall.Ucred
	var serr error
	if err := raw.Control(func(fd uintptr) {
		cred, serr = syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED)
	}); err != nil {
		return 0, err
	}
	if serr != nil {
		return 0, serr
	}
	return cred.Uid, nil
}
