// runtime.go answers "is this session's container running?" for the stopped-fence proof. The pane
// is not enough: a session started with --launcher none has no pane, and a container can outlive its
// pane. Direct mode asks docker by the exact container name; helper mode asks ccsbx-root, read-only
// (`status`), because the caller has no docker. Failing to find out is an error, never "stopped".
package start

import (
	"bytes"
	"fmt"
	"os/exec"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/backend"
)

// Runtime says whether a generation's container is running.
type Runtime interface {
	Running(sid string, gen int) (bool, error)
}

// DockerRuntime asks docker (direct mode).
type DockerRuntime struct{ Docker string }

func (d DockerRuntime) Running(sid string, gen int) (bool, error) {
	return (&backend.Runner{Docker: d.Docker}).Running(backend.Spec{Sid: sid, Gen: gen})
}

// HelperRuntime asks `sudo -n ccsbx-root status -- <sid>` (helper mode). Root answers for the
// record's current generation and the one before it.
type HelperRuntime struct{}

func (HelperRuntime) Running(sid string, _ int) (bool, error) {
	if !ValidSid(sid) {
		return false, fmt.Errorf("%q is not a session id", sid)
	}
	c := exec.Command(SudoBin, "-n", RootBin, "status", "--", sid)
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
