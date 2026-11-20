// sweep_other.go: the helper-mode sweep exists on Linux only (ccsbx-root is a Linux program); on
// other systems it refuses, so a build for them still compiles.

//go:build !linux

package backend

import "fmt"

func (s Spec) sweepHomeGo() ([]string, error) {
	return nil, fmt.Errorf("the helper-mode sweep is linux only")
}
