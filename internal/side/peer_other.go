// peer_other.go is the peer check where there is no peer-credential code. The sidecar runs in a Linux
// pod only; this file lets the package build everywhere (make build cross-compiles) and refuses every peer.
//go:build !linux

package side

import (
	"errors"
	"net"
)

func peerUID(net.Conn) (uint32, error) {
	return 0, errors.New("side: peer credentials are only implemented on linux; refusing")
}
