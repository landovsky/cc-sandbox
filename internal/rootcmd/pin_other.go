// pin_other.go: ccsbx-root is a Linux program; on other systems the pinning refuses, so a build
// for them still compiles and can never mount anything.

//go:build !linux

package rootcmd

import (
	"fmt"
	"os"
)

type noMounter struct{}

func (noMounter) Bind(*os.File, string, bool) error { return fmt.Errorf("mounts are linux only") }
func (noMounter) Unmount(string) error              { return nil }

// RealMounter is the production Mounter.
func RealMounter() Mounter { return noMounter{} }

// OpenDir is the production directory opener.
func OpenDir(string) (*os.File, error) { return nil, fmt.Errorf("pinned opens are linux only") }
