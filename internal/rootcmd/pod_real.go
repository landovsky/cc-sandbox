// pod_real.go wires the pod verbs to the real cluster and the real signer: a CA-pinned REST client for
// create/get/delete, kubectl exec (as the keys user) for the sidecar load, and the signer client for
// the token. Only reached after the drop to the keys user; the tests replace all of it.
package rootcmd

import (
	"context"

	"github.com/landovsky/cc-sandbox/internal/k8s"
	"github.com/landovsky/cc-sandbox/internal/side"
	"github.com/landovsky/cc-sandbox/internal/signer"
	"github.com/landovsky/cc-sandbox/internal/token"
)

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

// Load is the sidecar package's LoadInto: the same command, stdin only, 30 s, the sidecar's text back
// and never the secrets. Backend.Exec re-reads the pod and refuses unless its live UID is ref.UID,
// before any stdin is read.
func (p *podImpl) Load(ctx context.Context, ref k8s.Ref, secrets []byte) error {
	return side.LoadInto(ctx, p.b, ref, secrets)
}

func realMint(ctx context.Context, cfg *Config, c token.Claims) ([]byte, error) {
	return signer.MintFwdSecrets(ctx, signer.Client{Socket: cfg.SignerSocket}.Sign, c)
}
