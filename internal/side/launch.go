// launch.go is the launcher's half of the load: it hands a session's token and generation key to the
// sidecar of one pod through exec stdin. It exists so the secrets never touch the pod spec, an env var or
// an argv (all of which the API server or `ps` would show); the only place they travel is the exec stream.
package side

import (
	"bytes"
	"context"
	"errors"
	"fmt"

	"github.com/landovsky/cc-sandbox/internal/k8s"
)

// ControlPath is where the pod's side-state volume puts the control socket (internal/k8s mounts
// side-state at /run/ccsbx/side, sidecar only).
const ControlPath = "/run/ccsbx/side/ctl.sock"

// Execer is the part of k8s.Backend the launcher needs.
type Execer interface {
	Exec(ctx context.Context, ref k8s.Ref, container string, cmd []string, io k8s.IO) (int, error)
}

// LoadInto sends secrets (the JSON `ccsbx token mint --fwd-secrets` prints) to the side container of ref.
// ref must carry the UID recorded at Create. The command line carries no secret. A non-zero exit is an error: the sidecar's refusal text (stderr)
// is returned, never the secrets.
func LoadInto(ctx context.Context, x Execer, ref k8s.Ref, secrets []byte) error {
	if ref.UID == "" { // Exec re-checks the live pod UID; a ref without the recorded one must not reach a pod
		return errors.New("side: ref has no pod UID (use the one recorded at Create)")
	}
	if len(secrets) == 0 || len(secrets) > MaxSecrets {
		return fmt.Errorf("side: secrets must be 1..%d bytes", MaxSecrets)
	}
	var out, errb bytes.Buffer
	code, err := x.Exec(ctx, ref, "side", []string{"ccsbx", "side", "load", "--control", ControlPath},
		k8s.IO{Stdin: bytes.NewReader(secrets), Stdout: &out, Stderr: &errb})
	if err != nil {
		return err
	}
	if code != 0 {
		msg := errb.String()
		if len(msg) > 300 {
			msg = msg[:300]
		}
		return errors.New("side load refused: " + msg)
	}
	return nil
}
