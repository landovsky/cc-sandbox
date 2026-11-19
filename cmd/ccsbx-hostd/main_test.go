package main

import (
	"bytes"
	"strings"
	"testing"
)

// Every subcommand's --help runs nothing (the next agent learns the tool without side effects).
func TestHelpRunsNothing(t *testing.T) {
	var out, errb bytes.Buffer
	if code := app().Main([]string{"run", "--help"}, &out, &errb); code != 0 || !strings.Contains(out.String(), "-fence-uid") {
		t.Fatalf("exit %d out=%q err=%q", code, out.String(), errb.String())
	}
	if strings.Contains(errb.String(), "watching") {
		t.Fatal("--help started the loop")
	}
}

func TestOnceOnEmptyStateTouchesNothing(t *testing.T) {
	var out, errb bytes.Buffer
	dir := t.TempDir()
	if code := app().Main([]string{"run", "--once", "--state", dir}, &out, &errb); code != 0 || !strings.HasPrefix(out.String(), "accepted=0") {
		t.Fatalf("exit %d %q %q", code, out.String(), errb.String())
	}
}
