// Package k8s is the k8s backend (F2): it builds the session pod to the live contract of the k3s
// side (plan §4.6), creates, finds and deletes it through a small REST client, and runs exec and
// attach through kubectl. It exists so that a fence on future-two is the same fence as on hp (uid
// 10001, read-only root, no capabilities, no token, nothing but the forwarder port), with one
// builder (PodFor) that refuses anything the cluster's admission policy would refuse, and one
// interface (Backend) that the sidecar (F3), `ccsbx-root` and attach (F5) share.
//
// Zero third-party modules: the pod is our own typed subset of the v1 Pod JSON, the client is
// net/http, and tests run against a fake API server (httptest). Nothing here talks to a cluster
// unless a Client built from a kubeconfig is handed in.
//
// THE INTERFACE (fixed; F3 builds on it):
//
//	type Backend struct{ ... }                                  // NewBackend(Client, Execer, Config)
//	func (b *Backend) Create(ctx, Spec) (*Ref, error)           // PodFor + POST; Ref.UID from the 201 (persist it); on a 409 only Spec.KnownUID matches, else *ConflictError (pod_conflict)
//	func (b *Backend) Get(ctx, sid string, gen int) (*Status, error)
//	func (b *Backend) Delete(ctx, sid string, gen int) error    // by exact name, grace <= 60 s; absent = ok
//	func (b *Backend) List(ctx) ([]Status, error)               // role=session pods of this namespace
//	func (b *Backend) Exec(ctx, ref Ref, container string, cmd []string, io IO) (exit int, err error)  // live pod UID must equal ref.UID, checked just before the exec
//	func (b *Backend) AttachArgv(sid string, gen int) ([]string, error) // kubectl exec -it -c fence ... tmux attach; derives the pod, refuses non-absolute kubectl/kubeconfig
//	func NewKubectl(bin, kubeconfig, server, ca string) (Kubectl, error) // the production Execer; absolute paths, kubeconfig and CA must exist, server https; server+CA are passed on the command line
//	func PodFor(Spec) (*Pod, error)                             // pure; the contract is checked here
//
// The Client and Execer are interfaces too, so a fake API server and a fake kubectl stand in.
package k8s

import (
	"context"
	"fmt"
	"io"
)

// Client is the REST surface the launcher's RBAC allows (K8): pods create/get/list/delete in one
// namespace. Nothing else is reachable through it.
type Client interface {
	CreatePod(ctx context.Context, ns string, p *Pod) (*Pod, error)
	GetPod(ctx context.Context, ns, name string) (*Pod, error) // ErrNotFound if absent
	ListPods(ctx context.Context, ns, labelSelector string) ([]Pod, error)
	DeletePod(ctx context.Context, ns, name, uid string, graceSeconds int) error // uid is a precondition (409 if the live pod has another); absent is not an error
}

// IO is the streams of one exec. Stdin may be nil. TTY joins stderr into stdout.
type IO struct {
	Stdin          io.Reader
	Stdout, Stderr io.Writer
	TTY            bool
}

// Execer runs a command in a container of a pod (kubectl exec, with the kubeconfig under
// ccsbx-keys). Exit is the command's exit code; err is a failure to run it at all. Note kubectl's
// own failures (Forbidden, NotFound, connection refused) also exit 1: a caller that delivers
// secrets must treat any non-zero exit as "not delivered", never as the remote command's answer.
type Execer interface {
	Exec(ctx context.Context, ns, pod, container string, cmd []string, io IO) (exit int, err error)
}

// Ref names one generation's pod.
type Ref struct {
	Namespace, Name string
	Sid             string
	Gen             int
	// UID is the server-assigned identity of THIS pod, taken from the 201 of Create (or checked
	// against the recorded one on a 409). The caller persists it in the session record. Exec sends
	// nothing to a pod whose live UID is not this one.
	UID string
}

// ConflictError is "pod_conflict": a pod under the generation's name that is not the one we created
// (or cannot be shown to be). It names the exact pod so the owner can delete it; nothing deletes it
// for them, and no secret goes to it.
type ConflictError struct {
	Namespace, Pod, Why string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("pod_conflict: pod %s/%s %s; if it is not yours, the owner can delete exactly that pod (kubectl -n %s delete pod %s) and start again",
		e.Namespace, e.Pod, e.Why, e.Namespace, e.Pod)
}
