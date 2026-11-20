//go:build !linux

// attach_pty_other.go: a pod attach needs a pty and the Linux ioctls; it is not offered elsewhere.
package rootcmd

import "errors"

func realAttachSession(AttachSpec) (int, error) { return 0, errors.New("pod attach is linux only") }
