// peer_other.go is the peer check on platforms where this signer has no peer-credential code. It
// exists so the package builds everywhere (make build cross-compiles) while failing closed: the Mac
// signer is a separate LaunchDaemon piece, and until it exists every peer is refused here.
//go:build !linux

package signer

import (
	"errors"
	"net"
)

func peerCred(net.Conn) (uint32, uint32, error) {
	return 0, 0, errors.New("signer: peer credentials are only implemented on linux; refusing")
}
