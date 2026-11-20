//go:build !linux

// PeerContext for platforms without SO_PEERCRED's Linux form (the Mac): it gives the connection no
// identity, so POST /v1/start answers 500 "no peer credentials" and starts nothing. It exists so the
// daemon still builds for darwin (the Makefile cross-builds ./...), failing closed.
package daemonapi

import (
	"context"
	"net"
)

// PeerContext returns ctx unchanged: no uid is known, so no session can be started.
func PeerContext(ctx context.Context, _ net.Conn) context.Context { return ctx }
