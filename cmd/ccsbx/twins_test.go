// A meta-test of the shell twins: every tests/*.sh that drives bin/ccsbx must pin the mode with
// `export CCSBX_ROOT=direct`. The default mode is auto, which on a machine that has ccsbx-root
// installed is helper; a twin written for direct mode would then test the wrong thing, or act on
// state it did not create (helper-mode gc is machine-wide).
package main

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var (
	drivesBin = regexp.MustCompile(`:-bin/ccsbx`)
	pinsMode  = regexp.MustCompile(`(?m)^export CCSBX_ROOT=direct\b`)
)

func TestEveryTwinThatDrivesCcsbxPinsDirectMode(t *testing.T) {
	files, _ := filepath.Glob("../../tests/*.sh")
	if len(files) == 0 {
		t.Fatal("no tests/*.sh found")
	}
	seen := 0
	for _, f := range files {
		b, _ := os.ReadFile(f)
		if drivesBin.Match(b) {
			seen++
			if !pinsMode.Match(b) {
				t.Errorf("%s drives bin/ccsbx but does not `export CCSBX_ROOT=direct`", f)
			}
		}
	}
	if seen < 4 {
		t.Errorf("only %d twins recognised as driving bin/ccsbx (fence, gc, resume, seccomp-setuid expected): the pattern may have rotted", seen)
	}
}

func TestThePinCheckSeesAMissingPin(t *testing.T) {
	bad := []byte("BIN=${CCSBX_BIN:-bin/ccsbx}\n# export CCSBX_ROOT=direct (commented out)\n")
	if !drivesBin.Match(bad) || pinsMode.Match(bad) {
		t.Error("the pin check would accept a twin whose pin is commented out")
	}
}
