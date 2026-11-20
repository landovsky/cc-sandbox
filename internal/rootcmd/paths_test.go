// Tests that ccsbx-root's allowed_roots and hostd's --worktree-root agree. Why: the installer hands
// both the same list, and a disagreement would let a session start in hostd and fail in the helper
// (or the other way round).
package rootcmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/landovsky/cc-sandbox/internal/backend"
)

// context: hostd's --worktree-root and ccsbx-root's allowed_roots get the same list from the installer.
// They must agree on which lists are valid, so a start that hostd accepts is never one the helper
// refuses at run time because the roots differ.
func TestHostdWorktreeRootsAndAllowedRootsAgreeOnWhatIsAValidList(t *testing.T) {
	mk := func() string { // under /var/tmp: /tmp is a fixed fence mount, so a root there is refused by hostd
		d, err := os.MkdirTemp("/var/tmp", "ccsbx-roots-")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { os.RemoveAll(d) })
		return d
	}
	a, b := mk(), mk()
	a, _ = filepath.EvalSymlinks(a)
	b, _ = filepath.EvalSymlinks(b)
	wd := filepath.Join(a, "w")
	os.MkdirAll(wd, 0o755)
	state := t.TempDir()
	for _, roots := range [][]string{{a, b}, {a}, {a, ""}, {a, "rel"}, {a, "/"}, {a, b + "/"}, {a, b + "/nope"}, {}} {
		c := Defaults() // valid in every other respect, so only the roots decide
		c.Machine = "test-machine"
		rootErr := c.validate(roots)
		_, hostdErr := backend.CheckWorkdir(wd, strings.Join(roots, ","), state)
		if (rootErr == nil) != (hostdErr == nil) {
			t.Errorf("roots %q: ccsbx-root says %v, hostd says %v", roots, rootErr, hostdErr)
		}
	}
}
