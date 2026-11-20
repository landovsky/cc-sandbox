// strip_other.go: the setid strip exists on Linux only (ccsbx-root is a Linux program); elsewhere
// it refuses, so a build for those systems still compiles.

//go:build !linux

package backend

import (
	"fmt"
	"os"
)

// StripSetid is linux only.
func StripSetid(*os.File, int, func(string, ...any)) (int, error) {
	return 0, fmt.Errorf("the setid strip is linux only")
}
