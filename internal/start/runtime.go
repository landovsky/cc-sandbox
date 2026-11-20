// runtime.go answers "is this session's container running?" for the stopped-fence proof. The pane
// is not enough: a session started with --launcher none has no pane, and a container can outlive its
// pane. Direct mode asks docker by the exact container name; helper mode asks ccsbx-root, read-only
// (`status`), because the caller has no docker. Failing to find out is an error, never "stopped".
package start

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/backend"
)

// Runtime says whether a generation's container is running.
type Runtime interface {
	// Running answers for generation gen and the one before it, within a few seconds.
	Running(sid string, gen int) (bool, error)
}

// askTimeout bounds every question to docker or to ccsbx-root: a hung daemon must not hang a commit.
var askTimeout = 5 * time.Second

// DockerRuntime asks docker (direct mode).
type DockerRuntime struct{ Docker string }

func (d DockerRuntime) Running(sid string, gen int) (bool, error) {
	for g := gen; g >= 1 && g >= gen-1; g-- {
		ok, err := (&backend.Runner{Docker: d.Docker}).Running(backend.Spec{Sid: sid, Gen: g})
		if err != nil || ok {
			return ok, err
		}
	}
	return false, nil
}

// HelperRuntime asks `sudo -n ccsbx-root status -- <sid>` (helper mode). Root answers for the
// record's current generation and the one before it.
type HelperRuntime struct{}

func (HelperRuntime) Running(sid string, _ int) (bool, error) {
	if !ValidSid(sid) {
		return false, fmt.Errorf("%q is not a session id", sid)
	}
	ctx, cancel := context.WithTimeout(context.Background(), askTimeout)
	defer cancel()
	c := exec.CommandContext(ctx, SudoBin, "-n", RootBin, "status", "--", sid)
	c.WaitDelay = time.Second // do not wait for a child that keeps the pipe open past the kill
	c.Env = []string{"PATH=/usr/bin:/bin", "LANG=C.UTF-8"}
	var out, errb bytes.Buffer
	c.Stdout, c.Stderr = &out, &errb
	if err := c.Run(); err != nil {
		return false, fmt.Errorf("ccsbx-root status: %v: %s", err, strings.TrimSpace(errb.String()))
	}
	switch strings.TrimSpace(out.String()) {
	case "running":
		return true, nil
	case "stopped":
		return false, nil
	}
	return false, fmt.Errorf("ccsbx-root status: unexpected answer %q", strings.TrimSpace(out.String()))
}
