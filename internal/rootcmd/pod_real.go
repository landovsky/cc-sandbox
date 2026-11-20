// pod_real.go wires the pod verbs to the real cluster and the real signer: a CA-pinned REST client for
// create/get/delete, kubectl exec (as the keys user) for the sidecar load, and the signer client for
// the token. Only reached after the drop to the keys user; the tests replace all of it.
package rootcmd

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/k8s"
	"github.com/landovsky/cc-sandbox/internal/signer"
	"github.com/landovsky/cc-sandbox/internal/token"
)

// sideControl is the sidecar's control socket (internal/k8s mounts side-state at /run/ccsbx/side).
const sideControl = "/run/ccsbx/side/ctl/ctl.sock"

// maxSecrets is the sidecar's limit on what it reads.
const maxSecrets = 64 << 10

type podImpl struct{ b *k8s.Backend }

func realPod(cfg *Config, kc *k8sCtx) (PodAPI, error) {
	r, err := k8s.NewREST(kc.conn)
	if err != nil {
		return nil, err
	}
	x, err := k8s.NewKubectl(kc.kubectl, cfg.Kubeconfig, kc.conn.Server, kc.conn.CAFile)
	if err != nil {
		return nil, err
	}
	kcfg := k8s.DefaultConfig()
	kcfg.Namespace = cfg.K8sNamespace
	return &podImpl{b: k8s.NewBackend(r, x, kcfg)}, nil
}

func (p *podImpl) Create(ctx context.Context, s k8s.Spec) (*k8s.Ref, error) {
	return p.b.Create(ctx, s)
}
func (p *podImpl) Get(ctx context.Context, sid string, gen int) (*k8s.Status, error) {
	return p.b.Get(ctx, sid, gen)
}
func (p *podImpl) LiveUID(ctx context.Context, sid string, gen int) (string, error) {
	return p.b.LiveUID(ctx, sid, gen)
}
func (p *podImpl) Delete(ctx context.Context, sid string, gen int, uid string) error {
	return p.b.Delete(ctx, sid, gen, uid)
}

// Load mirrors the sidecar package's LoadInto (the same command, stdin only, 30 s, the sidecar's text
// back and never the secrets); it is replaced by a call to it once that package is on main. Backend.Exec
// re-reads the pod and refuses unless its live UID is ref.UID, before any stdin is read.
func (p *podImpl) Load(ctx context.Context, ref k8s.Ref, secrets []byte) error {
	if len(secrets) == 0 || len(secrets) > maxSecrets {
		return fmt.Errorf("secrets must be 1..%d bytes", maxSecrets)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var out, errb bytes.Buffer
	code, err := p.b.Exec(ctx, ref, "side", []string{"ccsbx", "side", "load", "--control", sideControl},
		k8s.IO{Stdin: bytes.NewReader(secrets), Stdout: &out, Stderr: &errb})
	if err != nil {
		return err
	}
	if code != 0 {
		msg := strings.ToValidUTF8(errb.String(), "?")
		if len(msg) > 300 {
			msg = strings.ToValidUTF8(msg[:300], "")
		}
		return errors.New("side load refused: " + msg)
	}
	return nil
}

func realMint(ctx context.Context, cfg *Config, c token.Claims) ([]byte, error) {
	return signer.MintFwdSecrets(ctx, signer.Client{Socket: cfg.SignerSocket}.Sign, c)
}
