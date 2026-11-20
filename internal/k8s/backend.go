// backend.go is the interface F3 (the sidecar), ccsbx-root and F5 (attach) share: create and delete
// a generation's pod, say what state it is in, list the session pods, run a command in one of its
// containers, and give the argv that attaches a terminal to its tmux. Every call names the pod by
// the exact name derived from the sid and generation; nothing takes a name or a selector from a
// caller.
package k8s

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
)

// Backend is the k8s backend.
type Backend struct {
	C          Client
	X          Execer
	Cfg        Config
	Kubectl    string // absolute path, for AttachArgv
	Kubeconfig string // the launcher kubeconfig under ccsbx-keys, for AttachArgv
	Server     string // https://host[:port], from ConnConfig, for AttachArgv
	CAFile     string // CA file, from ConnConfig, for AttachArgv
}

// NewBackend wires a backend; kubectl and kubeconfig are only used to build attach argv.
func NewBackend(c Client, x Execer, cfg Config) *Backend { return &Backend{C: c, X: x, Cfg: cfg} }

// Status is what the cluster says about a generation's pod.
type Status struct {
	Ref        Ref
	Phase      string // Pending Running Succeeded Failed Unknown
	FenceReady bool
	SideReady  bool
	FenceExit  *int // set once the fence container has terminated
	Reason     string
}

func statusOf(p *Pod) Status {
	st := Status{Ref: Ref{Namespace: p.Metadata.Namespace, Name: p.Metadata.Name, Sid: p.Metadata.Labels["ccsbx.sid"]}}
	st.Ref.Gen = atoiOr(p.Metadata.Labels["ccsbx.gen"])
	if p.Status == nil {
		return st
	}
	st.Phase, st.Reason = p.Status.Phase, p.Status.Reason
	for _, cs := range p.Status.ContainerStatuses {
		if cs.Name == "fence" {
			st.FenceReady = cs.Ready
			if t := cs.State.Terminated; t != nil {
				ec := t.ExitCode
				st.FenceExit = &ec
			}
		}
	}
	return st
}

// roundTrips: the pod the server returned is the one asked for, with labels that name it.
func roundTrips(p *Pod, sid string, gen int) bool {
	return p.Metadata.Labels["ccsbx.sid"] == sid && p.Metadata.Labels["ccsbx.gen"] == strconv.Itoa(gen) && p.Metadata.Name == PodName(sid, gen)
}

func (b *Backend) ref(sid string, gen int) (Ref, error) {
	if !sidRe.MatchString(sid) || gen < 1 || gen > 9999 {
		return Ref{}, fmt.Errorf("sid %q gen %d", sid, gen)
	}
	return Ref{Namespace: b.Cfg.Namespace, Name: PodName(sid, gen), Sid: sid, Gen: gen}, nil
}

// Create builds the pod (PodFor, so the contract is checked) and creates it. It is idempotent per
// (sid, gen): a pod that is already there for the same sid and generation is the answer, not an error.
func (b *Backend) Create(ctx context.Context, s Spec) (*Ref, error) {
	pod, err := b.Cfg.PodFor(s)
	if err != nil {
		return nil, err
	}
	ref, _ := b.ref(s.Sid, s.Gen)
	_, err = b.C.CreatePod(ctx, b.Cfg.Namespace, pod)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == 409 {
		// A pod of this name is already there. It is "ours" only if it is THIS pod: built from the
		// same spec (the hash annotation), still passing the contract, and not on its way out. The
		// labels alone prove nothing: a pod from an older attempt (another image, other limits) or
		// one someone else made under the name would otherwise be handed the token.
		got, gerr := b.C.GetPod(ctx, b.Cfg.Namespace, pod.Metadata.Name)
		if gerr != nil {
			return nil, gerr
		}
		switch {
		case !roundTrips(got, s.Sid, s.Gen):
			return nil, fmt.Errorf("pod %s exists but is not this generation's", pod.Metadata.Name)
		case got.Metadata.DeletionTimestamp != nil:
			return nil, fmt.Errorf("pod %s exists but is being deleted: wait for it to go, then create again", pod.Metadata.Name)
		case got.Metadata.Annotations[HashAnnotation] != pod.Metadata.Annotations[HashAnnotation]:
			return nil, fmt.Errorf("pod %s exists but was built from another spec", pod.Metadata.Name)
		}
		if err := b.Cfg.CheckFetched(got); err != nil {
			return nil, fmt.Errorf("pod %s exists but breaks the contract: %v", pod.Metadata.Name, err)
		}
		// The annotation is only a claim: anyone who can read it can copy it onto another pod. So the
		// fetched pod is hashed again, and must come out as the hash it claims.
		if h, herr := specHash(got); herr != nil || h != pod.Metadata.Annotations[HashAnnotation] {
			return nil, fmt.Errorf("pod %s exists and carries this spec's hash, but is not this spec", pod.Metadata.Name)
		}
		return &ref, nil
	}
	if err != nil {
		return nil, err
	}
	return &ref, nil
}

// Get reports the pod's state; ErrNotFound if there is none.
func (b *Backend) Get(ctx context.Context, sid string, gen int) (*Status, error) {
	ref, err := b.ref(sid, gen)
	if err != nil {
		return nil, err
	}
	p, err := b.C.GetPod(ctx, ref.Namespace, ref.Name)
	if err != nil {
		return nil, err
	}
	if !roundTrips(p, sid, gen) {
		return nil, fmt.Errorf("the API returned a pod that is not %s", ref.Name)
	}
	st := statusOf(p)
	return &st, nil
}

// Delete removes the pod by exact name with a grace period of at most 60 s; an absent pod is fine.
func (b *Backend) Delete(ctx context.Context, sid string, gen int) error {
	ref, err := b.ref(sid, gen)
	if err != nil {
		return err
	}
	g := b.Cfg.GraceSeconds
	if g < 0 || g > maxGrace {
		g = maxGrace
	}
	return b.C.DeletePod(ctx, ref.Namespace, ref.Name, int(g))
}

// List returns the session pods of the namespace (role=session).
func (b *Backend) List(ctx context.Context) ([]Status, error) {
	ps, err := b.C.ListPods(ctx, b.Cfg.Namespace, RoleLabel+"=session")
	if err != nil {
		return nil, err
	}
	var out []Status
	for i := range ps {
		// the server's selector is not taken on trust: only a pod that round-trips is ours
		sid, gen := ps[i].Metadata.Labels["ccsbx.sid"], atoiOr(ps[i].Metadata.Labels["ccsbx.gen"])
		if ps[i].Metadata.Labels[RoleLabel] == "session" && sidRe.MatchString(sid) && roundTrips(&ps[i], sid, gen) {
			out = append(out, statusOf(&ps[i]))
		}
	}
	return out, nil
}

// Exec runs cmd in container ("fence" or "side") of the generation's pod.
func (b *Backend) Exec(ctx context.Context, ref Ref, container string, cmd []string, io IO) (int, error) {
	if b.X == nil {
		return -1, errors.New("no exec backend configured")
	}
	want, err := b.ref(ref.Sid, ref.Gen)
	if err != nil || want.Name != ref.Name || ref.Namespace != b.Cfg.Namespace {
		return -1, fmt.Errorf("ref %+v is not a pod of this backend", ref)
	}
	if container != "fence" && container != "side" {
		return -1, fmt.Errorf("container %q: fence or side", container)
	}
	if len(cmd) == 0 {
		return -1, errors.New("no command")
	}
	for _, a := range cmd {
		if strings.ContainsRune(a, 0) {
			return -1, errors.New("NUL in a command argument")
		}
	}
	return b.X.Exec(ctx, ref.Namespace, ref.Name, container, cmd, io)
}

// AttachArgv is the command line that attaches a terminal to the fence's tmux (F5): kubectl exec -it
// into the fence container, by exact pod name derived here from the sid and generation (a caller
// names the session, never the pod), with the kubeconfig under ccsbx-keys. The caller execs it as the
// user that holds the kubeconfig; nothing here runs it. It refuses while kubectl or the kubeconfig is
// not an absolute path, because kubectl without a kubeconfig talks to localhost.
func (b *Backend) AttachArgv(sid string, gen int) ([]string, error) {
	ref, err := b.ref(sid, gen)
	if err != nil {
		return nil, err
	}
	for name, p := range map[string]string{"kubectl": b.Kubectl, "kubeconfig": b.Kubeconfig} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return nil, fmt.Errorf("%s %q must be a clean absolute path", name, p)
		}
	}
	if err := checkConn(b.Server, b.CAFile); err != nil {
		return nil, err
	}
	a := append([]string{b.Kubectl}, connArgs(b.Kubeconfig, b.Server, b.CAFile)...)
	return append(a, "-n", ref.Namespace, "exec", "-it", "-c", "fence", ref.Name, "--",
		"tmux", "attach-session", "-t", "="+sid+":"), nil
}
