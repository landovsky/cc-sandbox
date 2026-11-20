// Tests of tmuxsock. Why: the unit will pass an absolute shared socket path while a dev run passes
// a bare name, and picking the wrong tmux flag would silently address another server.
package tmuxsock

import (
	"reflect"
	"testing"
)

func TestBareNameIsLAndAbsolutePathIsS(t *testing.T) {
	if got := Args("ccsbx"); !reflect.DeepEqual(got, []string{"-L", "ccsbx"}) {
		t.Fatalf("%v", got)
	}
	if got := Args("/run/ccsbx/tmux.sock"); !reflect.DeepEqual(got, []string{"-S", "/run/ccsbx/tmux.sock"}) {
		t.Fatalf("%v", got)
	}
	if IsPath("ccsbx") || !IsPath("/x") {
		t.Fatal("IsPath")
	}
}
