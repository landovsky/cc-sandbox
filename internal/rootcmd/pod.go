// pod.go: the pod launcher's verbs (F2w): `pod-create`, `pod-load` and `pod-stop`, each taking one
// session id. They are what lets hostd (uid ccsbx, no kubeconfig, no secrets) start, load and end a
// session that runs as a pod on future-two: root validates the record and every file the keys user will
// read, becomes the keys user (ccsbx-keys, who holds the launcher kubeconfig and token and may ask the
// signer), and does the cluster work in that process.
//
// Why the record is believed. hostd writes `pod_uid` into the record (the helper-mode rule: hostd
// writes records, root derives and checks). A compromised hostd could write any UID there, so the UID
// is never the only protection: every verb here, and attach, looks the pod up again by the name derived
// from sid and generation (never from the record) and requires its live UID to equal the recorded one,
// right before anything is sent to it. A pod someone planted under that name has another UID; the worst
// a forged record can do is make us refuse to talk to the real pod.
//
// The order that keeps secrets safe: pod-create prints the new pod's UID and sends nothing secret; hostd
// records it; pod-load reads it from the record, waits for the pod, and only then asks the signer for
// the token (so none exists for a pod that never came up), mints it in this process (hostd never sees
// the token or the generation key) and sends it to the sidecar on exec stdin.
package rootcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strings"
	"time"

	"github.com/landovsky/cc-sandbox/internal/cli"
	"github.com/landovsky/cc-sandbox/internal/k8s"
	"github.com/landovsky/cc-sandbox/internal/pins"
	"github.com/landovsky/cc-sandbox/internal/start"
	"github.com/landovsky/cc-sandbox/internal/token"
	"github.com/landovsky/cc-sandbox/profiles"
)

// PodAPI is what the pod verbs need from the cluster. Production is a REST client plus kubectl exec;
// tests pass a fake.
type PodAPI interface {
	Create(ctx context.Context, s k8s.Spec) (*k8s.Ref, error)
	Get(ctx context.Context, sid string, gen int) (*k8s.Status, error)
	LiveUID(ctx context.Context, sid string, gen int) (string, error)
	Delete(ctx context.Context, sid string, gen int, uid string) error
	// Load sends the sidecar its secrets (stdin only) after the live pod's UID has been checked against ref.UID.
	Load(ctx context.Context, ref k8s.Ref, secrets []byte) error
}

// ExitNotReady is the exit code of pod-load when the pod did not come up in time (or died): the
// caller deletes the pod; nothing was minted.
const ExitNotReady = 4

// codeError is a failure with its own exit code.
type codeError struct {
	code int
	msg  string
}

func (e *codeError) Error() string { return e.msg }

func podErr(err error) error {
	var ce *k8s.ConflictError
	if errors.As(err, &ce) {
		return refuse("%v", err)
	}
	return err
}

var ownerRe = regexp.MustCompile(`^[a-z][a-z0-9._-]{0,31}$`)

// podSession loads the record for a pod verb: it must be a pod session, of this machine's pod machine.
func (e *env) podSession(verb, sid string) (who, *start.Record, error) {
	w, err := e.prelude(verb)
	if err != nil {
		return w, nil, err
	}
	rec, err := e.sessionAny(w, sid)
	if err != nil {
		return w, nil, err
	}
	if rec.Backend != "k8s" {
		return w, nil, refuse("session %s is not a pod session", sid)
	}
	return w, rec, nil
}

func (e *env) podCreateCmd() *cli.Command {
	return &cli.Command{
		Name:    "pod-create",
		Summary: "create the pod of a pod session's current generation; prints {\"uid\":...} for hostd to record",
		Recipes: `  sudo -n /usr/local/sbin/ccsbx-root pod-create s0k1dry66qg
  Takes only the sid. Refuses a record that already has a pod_uid (one pod per generation), a profile
  or image that is not approved, and a pod already under the name (pod_conflict, naming it). Creates
  the pod as the keys user and sends nothing secret; hostd records the UID, then runs pod-load.`,
		Run: func(_ *cli.Flags, args []string, stdout, stderr io.Writer) int {
			sid, ok := oneSid("pod-create", args, stderr)
			if !ok {
				return cli.ExitUsage
			}
			if err := e.podCreate(sid, stdout); err != nil {
				return fail(stderr, "pod-create", err)
			}
			return cli.ExitOK
		},
	}
}

func (e *env) podCreate(sid string, stdout io.Writer) error {
	w, rec, err := e.podSession("pod-create", sid)
	if err != nil {
		return err
	}
	if rec.Ended != nil {
		return refuse("session %s has ended", sid)
	}
	if rec.PodUID != "" {
		return refuse("session %s generation %d already has a pod (UID recorded): one pod per generation; stop it first", sid, rec.Gen)
	}
	if e.cfg.K8sSideImage == "" {
		return refuse("k8s_side_image is not set in the root config")
	}
	p, err := profiles.Load(rec.Spec.Profile)
	if err != nil {
		return refuse("%v", err)
	}
	if m, b := p.Allows(rec.Spec.Machine, rec.Spec.Billing); !m || !b {
		return refuse("profile %s is not approved for machine %s with billing %s", p.Name, rec.Spec.Machine, rec.Spec.Billing)
	}
	if err := trustedPins(e.cfg.PinsFile, e.cfg.TrustedUID); err != nil {
		return refuse("pins: %v", err)
	}
	pl, err := pins.Load(e.cfg.PinsFile)
	if err != nil {
		return refuse("pins: %v", err)
	}
	image := rec.Reply.Pin.Image
	if _, err := pins.Check(pl, nil, image, p, false); err != nil {
		return refuse("%v", err)
	}
	kc, err := e.k8sSetup(w, rec)
	if err != nil {
		return err
	}
	spec := k8s.Spec{Sid: rec.Sid, Label: rec.Label, Gen: rec.Gen, OwnerUID: rec.Uid, FenceImage: image, SideImage: e.cfg.K8sSideImage,
		MemoryMB: p.MemoryMB, CPU: e.cfg.K8sCPU, EphemeralMB: e.cfg.K8sEphemeralMB, WorktreeMB: e.cfg.K8sWorktreeMB, HostUsersFalse: true}
	// the contract is checked here, as root, before anything is dropped or sent
	if _, err := k8s.DefaultConfig().PodFor(spec); err != nil {
		return refuse("the pod would break the cluster's contract: %v", err)
	}
	if err := e.d.DropPrivs(kc.keysUID, kc.keysGID); err != nil {
		return fmt.Errorf("cannot become %s: %v", e.cfg.KeysUser, err)
	}
	api, err := e.d.NewPod(e.cfg, kc)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	ref, err := api.Create(ctx, spec) // no KnownUID: a pod already under the name is pod_conflict
	if err != nil {
		return podErr(err)
	}
	return json.NewEncoder(stdout).Encode(map[string]string{"uid": ref.UID, "pod": ref.Name})
}

func (e *env) podLoadCmd() *cli.Command {
	return &cli.Command{
		Name:    "pod-load",
		Summary: "wait for the pod, then mint the session token in-process and load it into the sidecar",
		Recipes: `  sudo -n /usr/local/sbin/ccsbx-root pod-load s0k1dry66qg
  Takes only the sid; the record must hold the pod_uid pod-create printed. Waits up to k8s_ready_seconds
  (default 120, max 600) for both containers, then asks the signer for the token as the keys user and
  sends it to the sidecar on exec stdin. Exit 4: the pod did not come up (nothing was minted; delete it
  with pod-stop). Exit 2: the live pod's UID is not the recorded one (pod_conflict).`,
		Run: func(_ *cli.Flags, args []string, stdout, stderr io.Writer) int {
			sid, ok := oneSid("pod-load", args, stderr)
			if !ok {
				return cli.ExitUsage
			}
			if err := e.podLoad(sid, stdout); err != nil {
				return fail(stderr, "pod-load", err)
			}
			return cli.ExitOK
		},
	}
}

func (e *env) podLoad(sid string, stdout io.Writer) error {
	w, rec, err := e.podSession("pod-load", sid)
	if err != nil {
		return err
	}
	if rec.Ended != nil {
		return refuse("session %s has ended", sid)
	}
	if rec.PodUID == "" {
		return refuse("session %s has no pod UID recorded: run pod-create and record its UID first", sid)
	}
	if !ownerRe.MatchString(rec.Spec.Owner) {
		return refuse("owner %q is not a plain name", rec.Spec.Owner)
	}
	p, err := profiles.Load(rec.Spec.Profile)
	if err != nil {
		return refuse("%v", err)
	}
	if m, b := p.Allows(rec.Spec.Machine, rec.Spec.Billing); !m || !b {
		return refuse("profile %s is not approved for machine %s with billing %s", p.Name, rec.Spec.Machine, rec.Spec.Billing)
	}
	kc, err := e.k8sSetup(w, rec)
	if err != nil {
		return err
	}
	if err := e.d.DropPrivs(kc.keysUID, kc.keysGID); err != nil {
		return fmt.Errorf("cannot become %s: %v", e.cfg.KeysUser, err)
	}
	api, err := e.d.NewPod(e.cfg, kc)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(e.cfg.K8sReadySeconds)*time.Second+2*time.Minute)
	defer cancel()
	conflict := func() error {
		return refuse("pod_conflict: pod %s/%s is not the pod created for session %s (UID differs); delete exactly that pod and start again",
			e.cfg.K8sNamespace, k8s.PodName(rec.Sid, rec.Gen), rec.Sid)
	}
	live, err := api.LiveUID(ctx, rec.Sid, rec.Gen) // by the derived name, never the record's
	if err != nil {
		return fmt.Errorf("cannot read the pod of session %s: %v", rec.Sid, err)
	}
	if live != rec.PodUID {
		return conflict()
	}
	if err := e.waitReady(ctx, api, rec); err != nil {
		return err
	}
	// Only now does a token exist. The signer authorises this uid for pod machines alone, and the
	// generation key is made here: hostd holds neither half.
	secrets, err := e.d.Mint(ctx, e.cfg, token.Claims{
		Sub:   fmt.Sprintf("%s/%s/%s/g%d", rec.Spec.Owner, rec.Spec.Machine, rec.Sid, rec.Gen),
		Label: rec.Label, Billing: rec.Spec.Billing, Profile: rec.Spec.Profile, Aud: e.cfg.K8sAudiences, PlacedBy: e.cfg.Machine,
		Exp: e.d.Now().Add(time.Duration(e.cfg.K8sTokenHours) * time.Hour).Unix(),
	})
	if err != nil {
		return fmt.Errorf("mint: %v", err)
	}
	ref := k8s.Ref{Namespace: e.cfg.K8sNamespace, Name: k8s.PodName(rec.Sid, rec.Gen), Sid: rec.Sid, Gen: rec.Gen, UID: rec.PodUID}
	if err := e.loadWithRetry(ctx, api, ref, secrets); err != nil {
		return podErr(err)
	}
	return json.NewEncoder(stdout).Encode(map[string]any{"loaded": true, "pod": ref.Name})
}

// waitReady polls until both containers are ready, the pod ends, or the deadline passes.
func (e *env) waitReady(ctx context.Context, api PodAPI, rec *start.Record) error {
	deadline := e.d.Now().Add(time.Duration(e.cfg.K8sReadySeconds) * time.Second)
	for {
		st, err := api.Get(ctx, rec.Sid, rec.Gen)
		switch {
		case errors.Is(err, k8s.ErrNotFound):
			return &codeError{ExitNotReady, "pod_gone: the pod of " + rec.Sid + " is not there"}
		case err != nil:
			return fmt.Errorf("cannot read the pod: %v", err)
		case st.Phase == "Failed" || st.Phase == "Succeeded":
			return &codeError{ExitNotReady, fmt.Sprintf("pod_not_ready: the pod ended (%s) before it was loaded; delete it with pod-stop", st.Phase)}
		case st.Phase == "Running" && st.FenceReady && st.SideReady:
			return nil
		}
		if !e.d.Now().Before(deadline) {
			return &codeError{ExitNotReady, fmt.Sprintf("pod_not_ready: phase %s, fence ready %v, side ready %v after %ds; delete it with pod-stop",
				st.Phase, st.FenceReady, st.SideReady, e.cfg.K8sReadySeconds)}
		}
		e.d.Sleep(time.Second)
	}
}

// loadWithRetry: the sidecar's control socket appears a moment after the container is ready (there is
// no probe), so a connect failure is retried every 500 ms for 30 s. A refusal by the sidecar is final.
// "already loaded" after an attempt that was cut off midway means the first one got through.
func (e *env) loadWithRetry(ctx context.Context, api PodAPI, ref k8s.Ref, secrets []byte) error {
	end := e.d.Now().Add(30 * time.Second)
	maybeLoaded := false
	for {
		err := api.Load(ctx, ref, secrets)
		if err == nil {
			return nil
		}
		msg := err.Error()
		var ce *k8s.ConflictError
		switch {
		case errors.As(err, &ce):
			return err
		case strings.Contains(msg, "already loaded"):
			if maybeLoaded {
				return nil
			}
			return refuse("the sidecar of %s was already loaded by someone else: delete the pod and start a new generation", ref.Name)
		case isConnectFailure(msg):
			// nothing reached the sidecar: safe to try again
		default:
			maybeLoaded = true // the exec may have been cut after the sidecar took the load
			if !strings.Contains(msg, "refused") {
				// a cut transport, not a refusal: retry once to learn whether it loaded
				if e.d.Now().After(end) {
					return err
				}
				e.d.Sleep(500 * time.Millisecond)
				continue
			}
			return err
		}
		if e.d.Now().After(end) {
			return fmt.Errorf("the sidecar's control socket never came up: %v", err)
		}
		e.d.Sleep(500 * time.Millisecond)
	}
}

func isConnectFailure(msg string) bool {
	return strings.Contains(msg, "no such file") || strings.Contains(msg, "connection refused") || strings.Contains(msg, "dial unix")
}

func (e *env) podStopCmd() *cli.Command {
	return &cli.Command{
		Name:    "pod-stop",
		Summary: "delete the pod of a pod session by exact name and recorded UID (absent is fine)",
		Recipes: `  sudo -n /usr/local/sbin/ccsbx-root pod-stop s0k1dry66qg
  Takes only the sid. Works on an ended record too. A pod with another UID is left alone: pod_conflict
  names it, and only its owner deletes it.`,
		Run: func(_ *cli.Flags, args []string, stdout, stderr io.Writer) int {
			sid, ok := oneSid("pod-stop", args, stderr)
			if !ok {
				return cli.ExitUsage
			}
			if err := e.podStop(sid, stdout); err != nil {
				return fail(stderr, "pod-stop", err)
			}
			return cli.ExitOK
		},
	}
}

func (e *env) podStop(sid string, stdout io.Writer) error {
	w, rec, err := e.podSession("pod-stop", sid)
	if err != nil {
		return err
	}
	if rec.PodUID == "" {
		return refuse("session %s has no pod UID recorded, so no pod can be shown to be its own: if one exists, the owner deletes %s/%s",
			sid, e.cfg.K8sNamespace, k8s.PodName(rec.Sid, rec.Gen))
	}
	kc, err := e.k8sSetup(w, rec)
	if err != nil {
		return err
	}
	if err := e.d.DropPrivs(kc.keysUID, kc.keysGID); err != nil {
		return fmt.Errorf("cannot become %s: %v", e.cfg.KeysUser, err)
	}
	api, err := e.d.NewPod(e.cfg, kc)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := api.Delete(ctx, rec.Sid, rec.Gen, rec.PodUID); err != nil {
		return podErr(err)
	}
	fmt.Fprintln(stdout, "deleted")
	return nil
}
