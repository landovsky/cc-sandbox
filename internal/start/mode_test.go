// Tests for how the privilege mode is chosen. The rule is deterministic and has no fallback: an
// explicit choice is taken as given, otherwise the helper is used exactly when it is installed and
// trustworthy (a regular file, owned by root, closed to group and others). A helper the caller
// could replace is not a helper.
package start

import (
	"os"
	"path/filepath"
	"testing"
)

func withHelper(t *testing.T, mode os.FileMode) {
	t.Helper()
	d := t.TempDir()
	p := filepath.Join(d, "ccsbx-root")
	os.WriteFile(p, []byte("#!/bin/sh\n"), 0o755)
	os.Chmod(p, mode)
	old := RootBin
	RootBin = p
	restore := SetTestRootUID(os.Getuid())
	t.Cleanup(func() { RootBin = old; restore() })
}

func TestResolveRoot(t *testing.T) {
	withHelper(t, 0o755)
	cases := map[string]string{"": "helper", "auto": "helper", "helper": "helper", "direct": "direct"}
	for in, want := range cases {
		if got, err := ResolveRoot(in); err != nil || got != want {
			t.Errorf("%q -> %q %v, want %q", in, got, err, want)
		}
	}
	if _, err := ResolveRoot("root"); err == nil {
		t.Error("an unknown mode was accepted")
	}
}

func TestAutoIsDirectWithoutATrustworthyHelper(t *testing.T) {
	// not installed
	old := RootBin
	RootBin = filepath.Join(t.TempDir(), "absent")
	defer func() { RootBin = old }()
	if got, _ := ResolveRoot("auto"); got != "direct" {
		t.Errorf("no helper: %q", got)
	}
	if _, err := ResolveRoot("helper"); err == nil {
		t.Error("--root helper without a helper was accepted (it must refuse, not fall back)")
	}
	if got, err := ResolveRoot("direct"); err != nil || got != "direct" {
		t.Errorf("explicit direct: %q %v", got, err)
	}
	// installed but writable by others
	withHelper(t, 0o777)
	if got, _ := ResolveRoot("auto"); got != "direct" {
		t.Errorf("a world-writable helper was used: %q", got)
	}
	if _, err := ResolveRoot("helper"); err == nil {
		t.Error("--root helper accepted a world-writable helper")
	}
	// installed but not owned by root
	withHelper(t, 0o755)
	restore := SetTestRootUID(os.Getuid() + 1)
	if got, _ := ResolveRoot("auto"); got != "direct" {
		t.Errorf("a helper not owned by root was used: %q", got)
	}
	restore()
	// a symlink to a good helper is not the helper
	withHelper(t, 0o755)
	link := filepath.Join(t.TempDir(), "link")
	os.Symlink(RootBin, link)
	RootBin = link
	if got, _ := ResolveRoot("auto"); got != "direct" {
		t.Errorf("a symlink was used as the helper: %q", got)
	}
}
