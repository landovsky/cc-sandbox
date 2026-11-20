// PeerContext for Linux: the connecting process's uid from SO_PEERCRED. It exists as its own file
// because SO_PEERCRED and GetsockoptUcred are Linux-only: the other platforms get peer_other.go, which
// gives a connection no identity, so a session start there fails closed.
package daemonapi

import (
	"context"
	"net"
	"syscall"
)

// PeerContext is an http.Server.ConnContext for the unix socket: it records the connecting
// process's uid (SO_PEERCRED), which is the only identity a start call has.
func PeerContext(ctx context.Context, c net.Conn) context.Context {
	uc, ok := c.(*net.UnixConn)
	if !ok {
		return ctx
	}
	raw, err := uc.SyscallConn()
	if err != nil {
		return ctx
	}
	var uid = -1
	_ = raw.Control(func(fd uintptr) {
		if cr, err := syscall.GetsockoptUcred(int(fd), syscall.SOL_SOCKET, syscall.SO_PEERCRED); err == nil {
			uid = int(cr.Uid)
		}
	})
	if uid < 0 {
		return ctx
	}
	return context.WithValue(ctx, peerKey{}, uid)
}
