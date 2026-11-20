package start

import (
	"os/exec"
	"strings"
	"testing"
)

// The pane's output is controlled by the fenced agent, so the tmux server must refuse clipboard
// writes and passthrough escapes. They are set in the same invocation as new-session because a
// pane that exits at once takes the server with it.
func TestTmuxHardeningIsInTheSameInvocation(t *testing.T) {
	var got []string
	tl := &TmuxLauncher{Exe: "/bin/ccsbx", Socket: "ccsbx", Command: func(name string, args ...string) *exec.Cmd {
		got = append([]string{name}, args...)
		return exec.Command("true")
	}}
	rec := &Record{Sid: "sabc123", Gen: 1}
	rec.Spec.Workdir.Path = "/var/tmp"
	if err := tl.Launch(rec); err != nil {
		t.Fatal(err)
	}
	line := strings.Join(got, " ")
	for _, want := range []string{"tmux -L ccsbx -f /dev/null new-session -d -s sabc123", "; set-option -g set-clipboard off", "; set-option -g allow-passthrough off", "run --sid sabc123 --gen 1",
		"tmux -L ccsbx -f /dev/null new-session", "; set-option -g status off", "; set-option -g prefix None", "; set-option -g escape-time 0"} {
		if !strings.Contains(line, want) {
			t.Errorf("missing %q in %s", want, line)
		}
	}
}
