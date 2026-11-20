// mount_root_test.go is the ROOT-ONLY test of the real bind mount, which the ordinary suite cannot
// run (mounting needs root and a mount namespace). It exists so the reviewer can read exactly what
// is not covered by the fake and run it by hand. It is excluded from `go test ./...` by its build
// tag. To run it on a Linux host, as root, in a throwaway mount namespace:
//
//	sudo unshare -m go test -tags rootmount -run TestRealMounter ./internal/rootcmd
//
// It has NOT been run by the author: no root was used while building ccsbx-root.

//go:build linux && rootmount

package rootcmd

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
)

func TestRealMounterBindsTheOpenedInodeNotTheName(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("needs root")
	}
	d := t.TempDir()
	orig, other, stage := filepath.Join(d, "wt"), filepath.Join(d, "other"), filepath.Join(d, "stage")
	for _, p := range []string{orig, other, stage} {
		os.MkdirAll(p, 0o755)
	}
	os.WriteFile(filepath.Join(orig, "mine"), nil, 0o644)
	os.WriteFile(filepath.Join(other, "theirs"), nil, 0o644)
	f, err := OpenDir(orig)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	// the swap: the name now leads somewhere else
	os.Rename(orig, orig+".x")
	os.Symlink(other, orig)
	m := RealMounter()
	if err := m.Bind(f, stage, false); err != nil {
		t.Fatal(err)
	}
	defer m.Unmount(stage)
	if _, err := os.Stat(filepath.Join(stage, "mine")); err != nil {
		t.Error("the staged mount is not the opened directory")
	}
	if _, err := os.Stat(filepath.Join(stage, "theirs")); err == nil {
		t.Error("the staged mount followed the swapped name")
	}
	var st syscall.Statfs_t
	syscall.Statfs(stage, &st)
	if st.Flags&syscall.MS_NOSUID == 0 || st.Flags&syscall.MS_NODEV == 0 {
		t.Errorf("flags %#x: want nosuid and nodev", st.Flags)
	}
	// read-only bind
	ro := filepath.Join(d, "ro")
	os.MkdirAll(ro, 0o755)
	if err := m.Bind(f, ro, true); err != nil {
		t.Fatal(err)
	}
	defer m.Unmount(ro)
	if err := os.WriteFile(filepath.Join(ro, "x"), nil, 0o644); err == nil {
		t.Error("a read-only bind was writable")
	}
}
