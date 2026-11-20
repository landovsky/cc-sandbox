// mode.go decides which privilege mode ccsbx runs in: direct (the caller drives docker itself, the
// dev mode, results are not evidence) or helper (everything that needs docker goes through
// `sudo -n /usr/local/sbin/ccsbx-root`, which is what is left after Gate M takes the docker group
// away). It exists so every tool (ccsbx, hostd) picks the mode the same way, deterministically,
// and never falls back from helper to direct behind the caller's back.
package start

import (
	"fmt"
	"os"
	"syscall"
)

// Where the helper is. Variables so tests can point them at stubs; production never reads them
// from the environment or a flag (that would let a caller choose which program runs as root).
var (
	SudoBin = "/usr/bin/sudo"
	RootBin = "/usr/local/sbin/ccsbx-root"
	// rootUID is who must own the helper: root. Tests (which cannot create root-owned files) set it.
	rootUID = 0
)

// SetTestRootUID lets a test of another package treat files of its own uid as root-owned. It must
// never be called in production code.
func SetTestRootUID(uid int) func() {
	old := rootUID
	rootUID = uid
	return func() { rootUID = old }
}

// HelperInstalled reports whether the helper is there and trustworthy: a regular file (not a
// symlink) owned by root and not writable by group or others. A file the caller could replace is
// not a helper.
func HelperInstalled() bool { return helperProblem() == "" }

func helperProblem() string {
	st, err := os.Lstat(RootBin)
	if err != nil {
		return fmt.Sprintf("%s does not exist", RootBin)
	}
	if !st.Mode().IsRegular() {
		return fmt.Sprintf("%s is not a regular file", RootBin)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok || int(sys.Uid) != rootUID {
		return fmt.Sprintf("%s is not owned by root", RootBin)
	}
	if st.Mode().Perm()&0o022 != 0 {
		return fmt.Sprintf("%s is writable by group or others", RootBin)
	}
	return ""
}

// ResolveRoot returns "direct" or "helper". explicit is the --root flag / CCSBX_ROOT: "direct" and
// "helper" are taken as given (helper refuses if the helper is not installed and trustworthy),
// "" or "auto" picks helper when the helper is installed and root-owned, else direct.
func ResolveRoot(explicit string) (string, error) {
	switch explicit {
	case "direct":
		return "direct", nil
	case "helper":
		if p := helperProblem(); p != "" {
			return "", fmt.Errorf("--root helper: %s (install it with units/install-linux.sh, or use --root direct)", p)
		}
		return "helper", nil
	case "", "auto":
		if HelperInstalled() {
			return "helper", nil
		}
		return "direct", nil
	}
	return "", fmt.Errorf("--root %q: want auto, direct or helper", explicit)
}
