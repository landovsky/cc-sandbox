// Tests for the pod launcher's verbs (F2w): pod-create, pod-load, pod-stop. The cluster, the signer and
// the clock are fakes, and "root" is a fake euid. What is checked is the policy: nothing secret moves
// before the live pod's UID matches the recorded one, no token exists for a pod that never came up,
// one pod per generation, and the secrets reach no output. Each refusal is a block twin.
package rootcmd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/k8s"
	"github.com/landovsky/cc-sandbox/internal/token"
)

const (
	fakeSecrets = `{"token":"TOPSECRET.jwt.value","key":"SEEDSEEDSEED"}` // the relay token is added by the fake Mint
	sideImg     = "ghcr.io/landovsky/ccsbx@sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
)

type fakePod struct {
	order    *[]string
	created  []k8s.Spec
	createEr error
	liveUID  string
	liveErr  error
	phases   []k8s.Status // returned in turn; the last repeats
	gets     int
	loads    []k8s.Ref
	loadErrs []error // per attempt; nil after the list
	loaded   []byte
	deleted  [][3]string
	delErr   error
	uid      string
}

func (p *fakePod) note(s string) { *p.order = append(*p.order, s) }

func (p *fakePod) Create(_ context.Context, s k8s.Spec) (*k8s.Ref, error) {
	p.note("create")
	p.created = append(p.created, s)
	if p.createEr != nil {
		return nil, p.createEr
	}
	return &k8s.Ref{Namespace: "cc-sandbox", Name: k8s.PodName(s.Sid, s.Gen), Sid: s.Sid, Gen: s.Gen, UID: p.uid}, nil
}
func (p *fakePod) Get(context.Context, string, int) (*k8s.Status, error) {
	p.note("get")
	i := p.gets
	if i >= len(p.phases) {
		i = len(p.phases) - 1
	}
	p.gets++
	if p.phases[i].Reason == "notfound" {
		return nil, k8s.ErrNotFound
	}
	st := p.phases[i]
	return &st, nil
}
func (p *fakePod) LiveUID(context.Context, string, int) (string, error) {
	p.note("live")
	return p.liveUID, p.liveErr
}
func (p *fakePod) Delete(_ context.Context, sid string, gen int, uid string) error {
	p.note("delete")
	p.deleted = append(p.deleted, [3]string{sid, fmt.Sprint(gen), uid})
	return p.delErr
}
func (p *fakePod) Load(_ context.Context, ref k8s.Ref, secrets []byte) error {
	p.note("load")
	p.loads = append(p.loads, ref)
	p.loaded = secrets
	if len(p.loadErrs) >= len(p.loads) {
		return p.loadErrs[len(p.loads)-1]
	}
	return nil
}

type podFx struct {
	*fx
	pod    *fakePod
	order  []string
	minted []token.Claims
	mintEr error
	clock  time.Time
	hub    []hubReq // what the fake hub saw
	hubSt  int      // its answer to POST (default 201)
	audit  strings.Builder
}

// hubTransport is the hub: it records each request (and notes it in the order) and answers.
type hubTransport struct{ pf *podFx }

func (h hubTransport) RoundTrip(q *http.Request) (*http.Response, error) {
	var b []byte
	if q.Body != nil {
		b, _ = io.ReadAll(q.Body)
	}
	h.pf.hub = append(h.pf.hub, hubReq{q.Method, q.URL.Path, q.URL.EscapedPath(), q.Header.Get("Authorization"), string(b)})
	h.pf.order = append(h.pf.order, "hub:"+q.Method)
	st, body := http.StatusNoContent, ""
	if q.Method == "POST" {
		st, body = h.pf.hubSt, `{"token":"sbp1.RELAYTOKEN"}`
		if st != 201 {
			body = `{"error":"BODY-OF-THE-REFUSAL"}`
		}
	}
	return &http.Response{StatusCode: st, Body: io.NopCloser(strings.NewReader(body)), Header: http.Header{}}, nil
}

var ready = k8s.Status{Phase: "Running", FenceReady: true, SideReady: true}

func newPodFx(t *testing.T) *podFx {
	t.Helper()
	f, calls := newK8sFx(t)
	_ = calls
	pf := &podFx{fx: f, clock: time.Unix(1_800_000_000, 0), hubSt: 201}
	put(t, f.dir+"/etc/hub.secret", hubSecretText+"\n", 0o600)
	f.writeConfigPod(t, true, "")
	pf.pod = &fakePod{order: &pf.order, liveUID: podUID, uid: "new-uid-1", phases: []k8s.Status{ready}}
	f.rec.PodUID, f.rec.Spec.Owner = podUID, "tomas"
	f.writeRecord()
	pf.wire()
	return pf
}

// writeConfigPod re-writes the config with the pod keys and the verbs on, keeping the k8s files; extra
// adds or overrides keys, and side=false leaves the side image out.
func (f *fx) writeConfigPod(t *testing.T, side bool, extra string) {
	t.Helper()
	old := f.d
	img := ""
	if side {
		img = fmt.Sprintf("k8s_side_image = %q\n", sideImg)
	}
	f.writeConfig(fmt.Sprintf("kubectl = %q\nkubeconfig = %q\nk8s_conn = %q\nkeys_user = \"ccsbx-keys\"\nk8s_namespace = \"cc-sandbox\"\n%s%shub_secret = %q\nverbs = [\"run\", \"stop\", \"attach\", \"pod-create\", \"pod-load\", \"pod-stop\"]\n",
		f.dir+"/bin/kubectl", f.dir+"/keys/launcher.kubeconfig", f.dir+"/etc/k8s-conn.json", img, extra, f.dir+"/etc/hub.secret"))
	// the config rewrite made a fresh Deps: carry over the k8s fakes of newK8sFx
	f.d.LookupUser, f.d.LivePodUID, f.d.DropPrivs, f.d.Exec = old.LookupUser, old.LivePodUID, old.DropPrivs, old.Exec
}

func (pf *podFx) wire() {
	order := &pf.order
	dp := pf.d.DropPrivs
	pf.d.DropPrivs = func(u, g int) error { *order = append(*order, "drop"); return dp(u, g) }
	pf.d.NewPod = func(*Config, *k8sCtx) (PodAPI, error) { return pf.pod, nil }
	pf.d.Hub = &http.Client{Transport: hubTransport{pf}}
	pf.d.Audit = &pf.audit
	pf.d.Mint = func(_ context.Context, _ *Config, c token.Claims, register func(string) (string, error)) ([]byte, error) {
		*order = append(*order, "mint")
		pf.minted = append(pf.minted, c)
		if pf.mintEr != nil {
			return nil, pf.mintEr
		}
		rt, err := register(relayPub) // the pub of the generation key the real Mint makes
		if err != nil {
			return nil, err
		}
		return []byte(strings.Replace(fakeSecrets, "}", `,"relay_token":"`+rt+`"}`, 1)), nil
	}
	pf.d.Now = func() time.Time { return pf.clock }
	pf.d.Sleep = func(d time.Duration) { pf.clock = pf.clock.Add(d) }
}

func (pf *podFx) seq() string { return strings.Join(pf.order, ",") }

func (pf *podFx) refused(why, verb, msg string) {
	pf.t.Helper()
	code, out := pf.call(verb, sid)
	if code != 2 || !strings.Contains(out, msg) {
		pf.t.Errorf("%s: exit %d: %s", why, code, out)
	}
	if strings.Contains(out, "TOPSECRET") || strings.Contains(out, "SEEDSEED") {
		pf.t.Errorf("%s: a secret reached the output: %s", why, out)
	}
}

// ---- pod-create ----

func TestPodCreateMakesOnePodAsTheKeysUserAndPrintsItsUID(t *testing.T) {
	pf := newPodFx(t)
	pf.rec.PodUID = ""
	pf.writeRecord()
	code, out := pf.call("pod-create", sid)
	if code != 0 || !strings.Contains(out, `"uid":"new-uid-1"`) {
		t.Fatalf("%d %s", code, out)
	}
	if pf.seq() != "drop,create" {
		t.Errorf("order %s: the privileges are dropped before the pod is created", pf.seq())
	}
	s := pf.pod.created[0]
	if s.Sid != sid || s.Gen != 2 || s.FenceImage != goodImage || s.SideImage != sideImg || s.KnownUID != "" || !s.HostUsersFalse || s.MemoryMB != 4096 || s.CPU != 2 {
		t.Errorf("spec %+v", s)
	}
	if len(pf.dropped) != 1 || pf.dropped[0] != [2]int{keysUID, keysGID} {
		t.Errorf("dropped %v", pf.dropped)
	}
}

func TestPodCreateRefusals(t *testing.T) {
	type tc struct {
		why, msg string
		mod      func(*podFx)
	}
	for _, c := range []tc{
		{"a record that already has a pod (one per generation)", "already has a pod", func(pf *podFx) {}}, // PodUID is set by newPodFx
		{"a docker session", "not a pod session", func(pf *podFx) {
			pf.rec.Backend, pf.rec.PodUID, pf.rec.Spec.Machine = "", "", "hp-ubuntu"
			pf.writeRecord()
		}},
		{"an ended session", "has ended", func(pf *podFx) {
			t0 := time.Now()
			pf.rec.PodUID, pf.rec.Ended = "", &t0
			pf.writeRecord()
		}},
		{"no side image in the config", "k8s_side_image", func(pf *podFx) {
			pf.rec.PodUID = ""
			pf.writeRecord()
			pf.writeConfigPod(t, false, "")
			pf.wire()
		}},
		{"an image that is not pinned", "not", func(pf *podFx) { pf.rec.PodUID = ""; pf.rec.Reply.Pin.Image = evilImage; pf.writeRecord() }},
		{"a profile not approved for the pod machine", "not approved", func(pf *podFx) { pf.rec.PodUID, pf.rec.Spec.Profile = "", "e2e-default"; pf.writeRecord() }},
		{"another user's session", "no such session", func(pf *podFx) {
			pf.rec.PodUID = ""
			pf.writeRecord()
			pf.env["SUDO_UID"] = fmt.Sprint(os.Getuid() + 99)
		}},
		{"a kubeconfig others can write", "kubeconfig", func(pf *podFx) {
			pf.rec.PodUID = ""
			pf.writeRecord()
			os.Chmod(pf.dir+"/keys/launcher.kubeconfig", 0o666)
		}},
	} {
		pf := newPodFx(t)
		c.mod(pf)
		code, out := pf.call("pod-create", sid)
		if code != 2 || !strings.Contains(out, c.msg) || len(pf.pod.created) != 0 || len(pf.dropped) != 0 {
			t.Errorf("%s: exit %d created=%d dropped=%d: %s", c.why, code, len(pf.pod.created), len(pf.dropped), out)
		}
	}
	// a pod already under the name (not ours) is pod_conflict, naming it, exit 2
	pf := newPodFx(t)
	pf.rec.PodUID = ""
	pf.writeRecord()
	pf.pod.createEr = &k8s.ConflictError{Namespace: "cc-sandbox", Pod: k8s.PodName(sid, 2), Why: "exists"}
	pf.refused("a pod under the name", "pod-create", "pod_conflict: pod cc-sandbox/ccsbx-s-"+sid+"-g2")
}

// ---- pod-load ----

func TestPodLoadWaitsThenMintsThenLoadsWithTheRecordedUID(t *testing.T) {
	pf := newPodFx(t)
	pf.pod.phases = []k8s.Status{{Phase: "Pending"}, {Phase: "Running", FenceReady: true}, ready}
	start0 := pf.clock
	// once privileges are dropped the keys user cannot read root's 0600 secret: simulate that, so a
	// read of it after the drop would fail the verb
	dp := pf.d.DropPrivs
	pf.d.DropPrivs = func(u, g int) error { os.Chmod(pf.dir+"/etc/hub.secret", 0); return dp(u, g) }
	code, out := pf.call("pod-load", sid)
	if code != 0 || strings.Contains(out, "TOPSECRET") || strings.Contains(out, "SEEDSEED") {
		t.Fatalf("%d %s", code, out)
	}
	if pf.seq() != "drop,live,get,get,get,mint,hub:POST,load" {
		t.Errorf("order %s: drop, check the UID, wait until both containers are ready, only then mint (the relay is registered inside it), then load", pf.seq())
	}
	if len(pf.hub) != 1 || pf.hub[0].method != "POST" || pf.hub[0].path != "/v1/principals" || pf.hub[0].auth != "Bearer "+hubSecretText {
		t.Fatalf("hub %+v", pf.hub)
	}
	var rb map[string]any
	json.Unmarshal([]byte(pf.hub[0].body), &rb)
	if rb["role"] != "relay" || rb["sid"] != sid || rb["gen"] != "2" || rb["pub"] != relayPub || fmt.Sprint(rb["may_reach"]) != "[]" {
		t.Errorf("relay request %s", pf.hub[0].body)
	}
	if !strings.Contains(string(pf.pod.loaded), `"relay_token":"sbp1.RELAYTOKEN"`) || strings.Contains(pf.audit.String(), "RELAYTOKEN") || strings.Contains(pf.audit.String(), hubSecretText) {
		t.Errorf("payload %q, audit %q", pf.pod.loaded, pf.audit.String())
	}
	if len(pf.pod.loads) != 1 || pf.pod.loads[0].UID != podUID || pf.pod.loads[0].Name != "ccsbx-s-"+sid+"-g2" || !strings.HasPrefix(string(pf.pod.loaded), fakeSecrets[:len(fakeSecrets)-1]) {
		t.Errorf("load %+v %q", pf.pod.loads, pf.pod.loaded)
	}
	c := pf.minted[0]
	if c.Sub != "tomas/future-two/"+sid+"/g2" || c.Billing != "stub" || c.Profile != "dev-default" || c.PlacedBy != "hp-ubuntu" ||
		strings.Join(c.Aud, ",") != "ccsbx-model,ccsbx-egress" || c.Exp != start0.Add(2*time.Second).Unix()+8*3600 {
		t.Errorf("claims %+v", c)
	}
}

func TestPodLoadRefusesWithoutTheRecordedUIDAndMintsNothing(t *testing.T) {
	pf := newPodFx(t)
	pf.rec.PodUID = ""
	pf.writeRecord()
	pf.refused("no UID recorded", "pod-load", "no pod UID recorded")
	pf = newPodFx(t)
	pf.pod.liveUID = "a-planted-pod"
	pf.refused("another pod under the name", "pod-load", "pod_conflict")
	pf = newPodFx(t)
	pf.pod.liveUID = ""
	pf.refused("a pod with no UID", "pod-load", "pod_conflict")
	for _, p := range []*podFx{pf} {
		if len(p.minted) != 0 || len(p.pod.loads) != 0 {
			t.Error("a token was minted or sent for a pod that is not the recorded one")
		}
	}
	pf = newPodFx(t)
	pf.pod.liveUID = "a-planted-pod"
	pf.call("pod-load", sid)
	if len(pf.minted) != 0 || len(pf.pod.loads) != 0 || strings.Contains(pf.seq(), "mint") {
		t.Errorf("order %s", pf.seq())
	}
}

func TestPodLoadWhenThePodNeverComesUpMintsNothingAndExits4(t *testing.T) {
	pf := newPodFx(t)
	pf.pod.phases = []k8s.Status{{Phase: "Pending"}}
	code, out := pf.call("pod-load", sid)
	if code != 4 || !strings.Contains(out, "pod_not_ready") || len(pf.minted) != 0 || len(pf.pod.loads) != 0 {
		t.Fatalf("%d %s minted=%d", code, out, len(pf.minted))
	}
	if waited := pf.clock.Sub(time.Unix(1_800_000_000, 0)); waited < 120*time.Second || waited > 130*time.Second {
		t.Errorf("waited %s, want the 120 s default", waited)
	}
	pf = newPodFx(t)
	pf.pod.phases = []k8s.Status{{Phase: "Failed"}}
	if code, out := pf.call("pod-load", sid); code != 4 || len(pf.minted) != 0 || !strings.Contains(out, "ended") {
		t.Errorf("a pod that failed: %d %s", code, out)
	}
	pf = newPodFx(t)
	pf.pod.phases = []k8s.Status{{Reason: "notfound"}}
	if code, out := pf.call("pod-load", sid); code != 4 || !strings.Contains(out, "pod_gone") {
		t.Errorf("a pod that vanished: %d %s", code, out)
	}
	// the deadline is the config's, up to 600 s
	pf = newPodFx(t)
	pf.writeConfigPod(t, true, "k8s_ready_seconds = 30\n")
	pf.wire()
	pf.pod.phases = []k8s.Status{{Phase: "Pending"}}
	pf.call("pod-load", sid)
	if w := pf.clock.Sub(time.Unix(1_800_000_000, 0)); w < 30*time.Second || w > 40*time.Second {
		t.Errorf("waited %s with k8s_ready_seconds = 30", w)
	}
}

func TestPodLoadRetriesAMissingSocketButNotARefusal(t *testing.T) {
	noSock := errors.New("dial unix /run/ccsbx/side/ctl/ctl.sock: connect: no such file or directory")
	pf := newPodFx(t)
	pf.pod.loadErrs = []error{noSock, noSock, nil}
	if code, out := pf.call("pod-load", sid); code != 0 || len(pf.pod.loads) != 3 {
		t.Errorf("a socket that comes up late: %d loads=%d %s", code, len(pf.pod.loads), out)
	}
	pf = newPodFx(t)
	pf.pod.loadErrs = []error{noSock}
	for i := 0; i < 200; i++ {
		pf.pod.loadErrs = append(pf.pod.loadErrs, noSock)
	}
	if code, out := pf.call("pod-load", sid); code != 1 || !strings.Contains(out, "never came up") || len(pf.pod.loads) > 70 {
		t.Errorf("a socket that never comes up: %d loads=%d %s", code, len(pf.pod.loads), out)
	}
	pf = newPodFx(t)
	pf.pod.loadErrs = []error{errors.New("side load refused: token expired")}
	if code, out := pf.call("pod-load", sid); code != 1 || len(pf.pod.loads) != 1 || !strings.Contains(out, "token expired") {
		t.Errorf("a refusal is final: %d loads=%d %s", code, len(pf.pod.loads), out)
	}
}

// The exec may be cut after the sidecar took the load: a retry that answers "already loaded" then means
// ours got through. On the first attempt the same answer means somebody else loaded it.
func TestPodLoadAlreadyLoadedIsOursOnlyAfterACutAttempt(t *testing.T) {
	pf := newPodFx(t)
	pf.pod.loadErrs = []error{errors.New("stream closed"), errors.New("side load refused: already loaded")}
	if code, out := pf.call("pod-load", sid); code != 0 {
		t.Errorf("after a cut attempt: %d %s", code, out)
	}
	pf = newPodFx(t)
	pf.pod.loadErrs = []error{errors.New("side load refused: already loaded")}
	pf.refused("already loaded by someone else", "pod-load", "already loaded by someone else")
}

func TestPodLoadFailuresNeverPrintTheSecrets(t *testing.T) {
	pf := newPodFx(t)
	pf.mintEr = errors.New("signer refused: not for this machine")
	code, out := pf.call("pod-load", sid)
	if code != 1 || len(pf.pod.loads) != 0 || strings.Contains(out, "TOPSECRET") {
		t.Errorf("mint failure: %d loads=%d %s", code, len(pf.pod.loads), out)
	}
	pf = newPodFx(t)
	pf.pod.loadErrs = []error{errors.New("side load refused: bad key")}
	if _, out := pf.call("pod-load", sid); strings.Contains(out, "TOPSECRET") || strings.Contains(out, "SEEDSEED") {
		t.Errorf("a secret reached the output: %s", out)
	}
	pf = newPodFx(t)
	pf.rec.Spec.Owner = "to/mallory"
	pf.writeRecord()
	pf.refused("an owner that would change the token's sub", "pod-load", "plain name")
}

// ---- pod-stop ----

func TestPodStopDeletesByNameAndRecordedUID(t *testing.T) {
	pf := newPodFx(t)
	t0 := time.Now()
	pf.rec.Ended = &t0 // an ended record is exactly when this runs
	pf.writeRecord()
	if code, out := pf.call("pod-stop", sid); code != 0 || !strings.Contains(out, "deleted") {
		t.Fatalf("%d %s", code, out)
	}
	if pf.seq() != "hub:DELETE,drop,delete" || len(pf.pod.deleted) != 1 || pf.pod.deleted[0] != [3]string{sid, "2", podUID} {
		t.Errorf("order %s deleted %v", pf.seq(), pf.pod.deleted)
	}
}

func TestPodStopRefusals(t *testing.T) {
	pf := newPodFx(t)
	pf.rec.PodUID = ""
	pf.writeRecord()
	pf.refused("no UID recorded: nothing can be shown to be ours", "pod-stop", "owner deletes")
	if len(pf.dropped) != 0 || len(pf.pod.deleted) != 0 {
		t.Error("acted without a UID")
	}
	pf = newPodFx(t)
	pf.pod.delErr = &k8s.ConflictError{Namespace: "cc-sandbox", Pod: k8s.PodName(sid, 2), Why: "has another UID"}
	pf.refused("another pod under the name", "pod-stop", "pod_conflict")
	pf = newPodFx(t)
	pf.rec.Backend, pf.rec.Spec.Machine = "", "hp-ubuntu"
	pf.writeRecord()
	pf.refused("a docker session", "pod-stop", "not a pod session")
}

// ---- config ----

func TestPodConfigKeysAreValidated(t *testing.T) {
	f := newFx(t)
	p := f.dir + "/etc/c.toml"
	base := "allowed_roots = [\"" + f.roots + "\"]\nmachine = \"m\"\n"
	for why, extra := range map[string]string{
		"a ready deadline of 0":          "k8s_ready_seconds = 0\n",
		"a ready deadline over 600":      "k8s_ready_seconds = 601\n",
		"a token life over 12 hours":     "k8s_token_hours = 13\n",
		"no audiences":                   "k8s_audiences = []\n",
		"a side image by tag":            "k8s_side_image = \"ghcr.io/landovsky/ccsbx:latest\"\n",
		"a side image from another repo": "k8s_side_image = \"ghcr.io/evil/ccsbx@sha256:" + strings.Repeat("d", 64) + "\"\n",
		"the pod machine is this one":    "k8s_machine = \"m\"\n",
		"a relative signer socket":       "signer_socket = \"s.sock\"\n",
	} {
		put(t, p, base+extra, 0o644)
		if _, err := LoadConfig(p, os.Getuid()); err == nil {
			t.Errorf("%s was accepted", why)
		}
	}
	put(t, p, base+"k8s_ready_seconds = 600\nk8s_side_image = \""+sideImg+"\"\n", 0o644)
	if c, err := LoadConfig(p, os.Getuid()); err != nil || c.K8sReadySeconds != 600 {
		t.Errorf("the maximum was refused: %v", err)
	}
	if d := Defaults(); d.K8sReadySeconds != 120 || d.K8sMachine != "future-two" || d.SignerSocket != "/run/ccsbx-signer/sign.sock" {
		t.Errorf("defaults moved: %+v", d)
	}
}

// ---- the relay of a pod session (hub secret read as root before the drop, registered after it) ----

func TestPodLoadNeedsTheHubSecretBeforeAnythingIsDropped(t *testing.T) {
	for why, mod := range map[string]func(pf *podFx){
		"missing":            func(pf *podFx) { os.Remove(pf.dir + "/etc/hub.secret") },
		"readable by others": func(pf *podFx) { os.Chmod(pf.dir+"/etc/hub.secret", 0o644) },
		"readable by group":  func(pf *podFx) { os.Chmod(pf.dir+"/etc/hub.secret", 0o640) },
	} {
		pf := newPodFx(t)
		mod(pf)
		code, out := pf.call("pod-load", sid)
		if code != 3 || len(pf.dropped) != 0 || len(pf.minted) != 0 || len(pf.pod.loads) != 0 || strings.Contains(out, hubSecretText) {
			t.Errorf("%s: exit %d dropped=%d minted=%d loads=%d: %s", why, code, len(pf.dropped), len(pf.minted), len(pf.pod.loads), out)
		}
	}
}

func TestPodLoadIsRateLimitedLikeRelayMint(t *testing.T) {
	pf := newPodFx(t)
	if c, out := pf.call("pod-load", sid); c != 0 {
		t.Fatal(out)
	}
	pf.order = nil
	pf.pod.loads = nil
	pf.hub = nil
	// the secret is read again by the second run; it was only unreadable inside the first (fake) drop
	if c, out := pf.call("pod-load", sid); c != 2 || !strings.Contains(out, "rate") || len(pf.hub) != 0 || len(pf.pod.loads) != 0 {
		t.Errorf("a second load inside the window: %d %s", c, out)
	}
}

func TestAHubThatRefusesTheRelayStopsTheLoadAndLeaksNothing(t *testing.T) {
	pf := newPodFx(t)
	pf.hubSt = 500
	code, out := pf.call("pod-load", sid)
	if code != 3 || !strings.Contains(out, "500") || strings.Contains(out, "BODY-OF-THE-REFUSAL") || len(pf.pod.loads) != 0 {
		t.Errorf("exit %d loads=%d: %s", code, len(pf.pod.loads), out)
	}
}

// A relay that was registered but never delivered must not stay live.
func TestARelayThatWasNeverDeliveredIsRevoked(t *testing.T) {
	pf := newPodFx(t)
	pf.pod.loadErrs = []error{errors.New("side load refused: bad key")}
	if code, _ := pf.call("pod-load", sid); code != 1 {
		t.Fatalf("exit %d", code)
	}
	var del []hubReq
	for _, h := range pf.hub {
		if h.method == "DELETE" {
			del = append(del, h)
		}
	}
	if len(del) != 1 || del[0].rawPath != "/v1/principals/relay:"+sid+"%2F2" || del[0].auth != "Bearer "+hubSecretText {
		t.Errorf("revokes %+v", del)
	}
}

// pod-stop revokes the relay as root, before the drop (the secret is root's), then deletes the pod.
func TestPodStopRevokesTheRelayBeforeTheDrop(t *testing.T) {
	pf := newPodFx(t)
	if code, out := pf.call("pod-stop", sid); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if pf.seq() != "hub:DELETE,drop,delete" {
		t.Errorf("order %s", pf.seq())
	}
	// a hub that is down never stops the pod's deletion
	pf = newPodFx(t)
	pf.d.Hub = &http.Client{Transport: failTransport{}}
	if code, out := pf.call("pod-stop", sid); code != 0 || len(pf.pod.deleted) != 1 {
		t.Errorf("hub down: %d %s deleted=%d", code, out, len(pf.pod.deleted))
	}
}

type failTransport struct{}

func (failTransport) RoundTrip(*http.Request) (*http.Response, error) { return nil, errors.New("down") }
