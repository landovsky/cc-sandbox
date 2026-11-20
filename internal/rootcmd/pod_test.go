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
	"github.com/landovsky/cc-sandbox/internal/side"
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
	gone     bool
	goneEr   error
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
	if p.gone {
		return "", k8s.ErrNotFound
	}
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
	pod      *fakePod
	order    []string
	minted   []token.Claims
	mintEr   error
	clock    time.Time
	hub      []hubReq           // what the fake hub saw
	hubSt    int                // its answer to POST (default 201)
	revoked  []token.RevokeBody // what the doors were told
	revokeEr error
	audit    strings.Builder
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
	f.env["SUDO_UID"] = fmt.Sprint(svcUID) // hostd: pod-create and pod-load are its steps
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
	f.writeConfig(fmt.Sprintf("kubectl = %q\nkubeconfig = %q\nk8s_conn = %q\nlaunch_user = \"ccsbx-launch\"\nk8s_namespace = \"cc-sandbox\"\n%s%shub_secret = %q\nhub_user = \"hubuser\"\nverbs = [\"run\", \"stop\", \"attach\", \"pod-create\", \"pod-load\", \"pod-stop\", \"pod-status\"]\n",
		f.dir+"/bin/kubectl", f.dir+"/launch/launcher.kubeconfig", f.dir+"/etc/k8s-conn.json", img, extra, f.dir+"/etc/hub.secret"))
	// the config rewrite made a fresh Deps: carry over the k8s fakes of newK8sFx
	f.d.LookupUser, f.d.LivePodUID, f.d.DropPrivs, f.d.Exec = old.LookupUser, old.LivePodUID, old.DropPrivs, old.Exec
	lookup := f.d.LookupUser
	f.d.LookupUser = func(n string) (int, int, error) {
		if n == "hubuser" {
			return hubUID, hubUID, nil
		}
		return lookup(n)
	}
	f.d.HubListeners = func(port int) ([]TCPListener, error) {
		return []TCPListener{{IP: "0100007F", Port: port, UID: hubUID}}, nil
	}
}

func (pf *podFx) wire() {
	order := &pf.order
	dp := pf.d.DropPrivs
	pf.d.DropPrivs = func(u, g int) error { *order = append(*order, "drop"); return dp(u, g) }
	pf.d.NewPod = func(*Config, *k8sCtx) (PodAPI, error) { return pf.pod, nil }
	pf.d.Hub = &http.Client{Transport: hubTransport{pf}}
	pf.d.RevokeToken = func(_ context.Context, _ *Config, b token.RevokeBody) error {
		*order = append(*order, "revoke-token")
		pf.revoked = append(pf.revoked, b)
		return pf.revokeEr
	}
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

func TestPodCreateMakesOnePodAsTheLaunchUserAndPrintsItsUID(t *testing.T) {
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
	if len(pf.dropped) != 1 || pf.dropped[0] != [2]int{launchUID, launchGID} {
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
			os.Chmod(pf.dir+"/launch/launcher.kubeconfig", 0o666)
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
	// once privileges are dropped the launch user cannot read root's 0600 secret: simulate that, so a
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
	if rb["role"] != "relay" || rb["sid"] != sid || rb["gen"] != "2" || rb["pub"] != relayPub || fmt.Sprint(rb["may_reach"]) != "[]" || rb["manager"] != "" {
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
	for why, uid := range map[string]string{"another pod under the name": "a-planted-pod", "a pod with no UID": ""} {
		pf = newPodFx(t)
		pf.pod.liveUID = uid
		pf.refused(why, "pod-load", "pod_conflict")
		if len(pf.minted) != 0 || len(pf.pod.loads) != 0 || strings.Contains(pf.seq(), "mint") || len(pf.hub) != 0 {
			t.Errorf("%s: a token was minted, a relay registered or a load sent (order %s)", why, pf.seq())
		}
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
	noSock := fmt.Errorf("side load: dial unix x: %w", side.ErrNoSocket)
	pf := newPodFx(t)
	pf.pod.loadErrs = []error{noSock, noSock, nil}
	if code, out := pf.call("pod-load", sid); code != 0 || len(pf.pod.loads) != 3 {
		t.Errorf("a socket that comes up late: %d loads=%d %s", code, len(pf.pod.loads), out)
	}
	pf = newPodFx(t)
	for i := 0; i < 200; i++ {
		pf.pod.loadErrs = append(pf.pod.loadErrs, noSock)
	}
	if code, out := pf.call("pod-load", sid); code != 1 || !strings.Contains(out, "never came up") || len(pf.pod.loads) > 70 {
		t.Errorf("a socket that never comes up: %d loads=%d %s", code, len(pf.pod.loads), out)
	}
	// a refusal is final, whatever words its text holds
	for _, msg := range []string{"token expired", "no such file or directory in the payload", "connection refused by the policy"} {
		pf = newPodFx(t)
		pf.pod.loadErrs = []error{fmt.Errorf("side load refused: %s: %w", msg, side.ErrRefused)}
		if code, out := pf.call("pod-load", sid); code != 1 || len(pf.pod.loads) != 1 || !strings.Contains(out, msg) {
			t.Errorf("%q: %d loads=%d %s", msg, code, len(pf.pod.loads), out)
		}
	}
}

// A cut exec stream may have happened after the sidecar took the load: the retry then answers "already
// loaded" (exit 4), which is our own earlier try. Without such a try the same answer means somebody else
// loaded it. Both are decided by the typed errors, not by text.
func TestPodLoadAlreadyLoadedIsOursOnlyAfterACutAttempt(t *testing.T) {
	pf := newPodFx(t)
	pf.pod.loadErrs = []error{errors.New("stream closed"), fmt.Errorf("x: %w", side.ErrAlreadyLoaded)}
	if code, out := pf.call("pod-load", sid); code != 0 || len(pf.pod.loads) != 2 {
		t.Errorf("after a cut attempt: %d %s", code, out)
	}
	pf = newPodFx(t)
	pf.pod.loadErrs = []error{fmt.Errorf("x: %w", side.ErrAlreadyLoaded)}
	pf.refused("already loaded by someone else", "pod-load", "already loaded by someone else")
	// "not up yet" followed by "already loaded" is NOT ours: nothing of ours ever reached the sidecar
	pf = newPodFx(t)
	pf.pod.loadErrs = []error{fmt.Errorf("x: %w", side.ErrNoSocket), fmt.Errorf("x: %w", side.ErrAlreadyLoaded)}
	pf.refused("a connect failure then already loaded", "pod-load", "already loaded by someone else")
	// text that merely says it is not a classification
	pf = newPodFx(t)
	pf.pod.loadErrs = []error{errors.New("side load: no such file or directory")}
	if code, _ := pf.call("pod-load", sid); code != 1 && code != 0 {
		t.Errorf("an unclassified error: %d", code)
	}
	if len(pf.pod.loads) < 2 {
		t.Errorf("an unclassified error (a cut stream) must be retried to learn whether it loaded: %d tries", len(pf.pod.loads))
	}
}

func TestPodLoadFailuresNeverPrintTheSecrets(t *testing.T) {
	pf := newPodFx(t)
	pf.mintEr = errors.New("signer refused: not for this machine")
	code, out := pf.call("pod-load", sid)
	if code != 1 || len(pf.pod.loads) != 0 || strings.Contains(out, "TOPSECRET") {
		t.Errorf("mint failure: %d loads=%d %s", code, len(pf.pod.loads), out)
	}
	pf = newPodFx(t)
	pf.pod.loadErrs = []error{fmt.Errorf("side load refused: bad key: %w", side.ErrRefused)}
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
	if pf.seq() != "hub:DELETE,drop,revoke-token,delete" || len(pf.pod.deleted) != 1 || pf.pod.deleted[0] != [3]string{sid, "2", podUID} {
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
	if d := Defaults(); d.K8sReadySeconds != 120 || d.K8sMachine != "future-two" || d.SignerSocket != "/run/ccsbx-signer/launch.sock" {
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
	pf.pod.loadErrs = []error{fmt.Errorf("side load refused: bad key: %w", side.ErrRefused)}
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
	if pf.seq() != "hub:DELETE,drop,revoke-token,delete" {
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

// pod-create and pod-load are hostd's (or root's): the owner re-running them would mint tokens at will
// or take the pod's name before hostd does. pod-stop stays open to the owner.
func TestOnlyHostdOrRootCreatesAndLoadsAPod(t *testing.T) {
	for _, verb := range []string{"pod-create", "pod-load"} {
		pf := newPodFx(t)
		if verb == "pod-create" {
			pf.rec.PodUID = ""
			pf.writeRecord()
		}
		pf.env["SUDO_UID"] = fmt.Sprint(pf.rec.Uid) // the owner
		pf.refused("the owner runs "+verb, verb, "for the service user")
		if len(pf.dropped) != 0 || len(pf.pod.created) != 0 || len(pf.minted) != 0 {
			t.Errorf("%s: acted for the owner", verb)
		}
	}
	pf := newPodFx(t)
	pf.env["SUDO_UID"] = fmt.Sprint(pf.rec.Uid)
	if code, out := pf.call("pod-stop", sid); code != 0 {
		t.Errorf("the owner could not stop their pod: %d %s", code, out)
	}
	pf = newPodFx(t)
	delete(pf.env, "SUDO_UID") // root by login
	if code, out := pf.call("pod-load", sid); code != 0 {
		t.Errorf("root by login: %d %s", code, out)
	}
}

// ---- pod-status, and stop for a pod session ----

func TestPodStatusSaysPresentOrAbsentAndNeverActs(t *testing.T) {
	pf := newPodFx(t)
	pf.env["SUDO_UID"] = fmt.Sprint(pf.rec.Uid) // the owner may ask
	if code, out := pf.call("pod-status", sid); code != 0 || strings.TrimSpace(out) != "present" {
		t.Errorf("present: %d %q", code, out)
	}
	pf.pod.gone = true
	if code, out := pf.call("pod-status", sid); code != 0 || strings.TrimSpace(out) != "absent" {
		t.Errorf("absent: %d %q", code, out)
	}
	pf.pod.gone = false
	pf.pod.liveUID = "a-planted-pod"
	pf.refused("another pod under the name", "pod-status", "pod_conflict")
	pf = newPodFx(t)
	pf.rec.PodUID = ""
	pf.writeRecord()
	pf.refused("no UID recorded", "pod-status", "no pod UID")
	if len(pf.pod.deleted)+len(pf.pod.created)+len(pf.pod.loads) != 0 {
		t.Error("pod-status changed something")
	}
}

// The owner's `ccsbx stop` is root's `stop`: for a pod session it deletes the pod by name and UID, and
// docker is not involved.
func TestStopOfAPodSessionDeletesThePodAndTouchesNoDocker(t *testing.T) {
	pf := newPodFx(t)
	pf.env["SUDO_UID"] = fmt.Sprint(pf.rec.Uid)
	if code, out := pf.call("stop", sid); code != 0 || !strings.Contains(out, "deleted") {
		t.Fatalf("%d %s", code, out)
	}
	if len(pf.pod.deleted) != 1 || pf.pod.deleted[0] != [3]string{sid, "2", podUID} || len(pf.fb.stopped) != 0 {
		t.Errorf("deleted %v, docker stops %d", pf.pod.deleted, len(pf.fb.stopped))
	}
}

// ---- the sidecar's command line comes from the record and root's config (F2w, B's final paths) ----

func TestPodCreateBuildsTheSidecarCommandFromTheRecordAndConfig(t *testing.T) {
	pf := newPodFx(t)
	pf.rec.PodUID = ""
	pf.rec.Label = "cc-sandbox:x"
	pf.rec.Spec.MsgScope = json.RawMessage(`{"manager":{"node":"cc-sandbox:#6b3s","machine":"hp-ubuntu"},"peers":["p1"],"peer_machines":{"p1":"mac"}}`)
	pf.rec.Reply.ID = "tomas/future-two/" + sid + "/g2"
	pf.writeRecord()
	pf.writeConfigPod(t, true, "k8s_hub_url = \"http://hub-egress.cc-sandbox.svc:4011\"\nk8s_doors = [\"model=http://ccsp-model.cc-sandbox-doors.svc:8080\"]\n")
	pf.wire()
	if code, out := pf.call("pod-create", sid); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	got := strings.Join(pf.pod.created[0].SideCmd, " ")
	want := "/usr/local/bin/ccsbx side serve --listen 127.0.0.1:8080 --control /run/ccsbx/side/ctl/ctl.sock --state /run/ccsbx/side/state --journal-dir /run/ccsbx/journal/s " +
		"--out /run/ccsbx/out --in /run/ccsbx/in --fence-uid 10001 --sid " + sid + " --gen 2 --label cc-sandbox:x --full-id tomas/future-two/" + sid + "/g2 --machine future-two " +
		`--scope {"manager":{"node":"cc-sandbox:#6b3s","machine":"hp-ubuntu"},"peers":["p1"],"peer_machines":{"p1":"mac"}} ` +
		"--hub-url http://hub-egress.cc-sandbox.svc:4011 --door model=http://ccsp-model.cc-sandbox-doors.svc:8080"
	if got != want {
		t.Errorf("got  %s\nwant %s", got, want)
	}
}

func TestPodCreateRefusesARecordTheSidecarCouldNotBeToldAbout(t *testing.T) {
	for why, mod := range map[string]func(pf *podFx){
		"a scope that is not an object": func(pf *podFx) { pf.rec.Spec.MsgScope = json.RawMessage(`["x"]`) },
		"no scope":                      func(pf *podFx) { pf.rec.Spec.MsgScope = nil },
		"a label with a newline":        func(pf *podFx) { pf.rec.Label = "a\nb" },
		"a full id of another session":  func(pf *podFx) { pf.rec.Reply.ID = "tomas/future-two/s0zzzzzzzz/g2" },
	} {
		pf := newPodFx(t)
		pf.rec.PodUID = ""
		mod(pf)
		pf.writeRecord()
		code, out := pf.call("pod-create", sid)
		if code != 2 || len(pf.pod.created) != 0 || len(pf.dropped) != 0 {
			t.Errorf("%s: exit %d created=%d dropped=%d: %s", why, code, len(pf.pod.created), len(pf.dropped), out)
		}
	}
}

func TestSidecarConfigKeysAreValidated(t *testing.T) {
	f := newFx(t)
	p := f.dir + "/etc/c.toml"
	base := "allowed_roots = [\"" + f.roots + "\"]\nmachine = \"m\"\n"
	for why, extra := range map[string]string{
		"a hub URL with credentials": "k8s_hub_url = \"http://u:p@hub:4011\"\n",
		"a hub URL that is a path":   "k8s_hub_url = \"hub:4011\"\n",
		"a door with no name":        "k8s_doors = [\"http://x\"]\n",
		"a door that is ftp":         "k8s_doors = [\"model=ftp://x\"]\n",
	} {
		put(t, p, base+extra, 0o644)
		if _, err := LoadConfig(p, os.Getuid()); err == nil {
			t.Errorf("%s was accepted", why)
		}
	}
	put(t, p, base+"k8s_hub_url = \"https://hub.example:4011\"\n", 0o644)
	if _, err := LoadConfig(p, os.Getuid()); err != nil {
		t.Errorf("a good hub URL: %v", err)
	}
}

func TestPodLoadReadsTheScopeStrictlyToo(t *testing.T) {
	pf := newPodFx(t)
	pf.rec.Spec.MsgScope = json.RawMessage(`{"manager":{"node":"a","machine":"hp"},"manager":{"node":"b","machine":"mac"}}`)
	pf.writeRecord()
	pf.refused("a duplicate manager", "pod-load", "not well formed")
	if len(pf.dropped) != 0 || len(pf.minted) != 0 || len(pf.hub) != 0 {
		t.Error("acted on a scope that is not well formed")
	}
}

// ---- the doors are told when a pod generation ends (F2x: the revoke client) ----

func TestStoppingAPodRevokesItsTokenAtTheDoorsEvenIfTheDeleteFails(t *testing.T) {
	pf := newPodFx(t)
	if code, out := pf.call("pod-stop", sid); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	if len(pf.revoked) != 1 || pf.revoked[0] != (token.RevokeBody{Owner: "tomas", Machine: "future-two", Sid: sid, Gen: 2}) {
		t.Errorf("revoked %+v", pf.revoked)
	}
	// the delete fails (another pod under the name): the token is revoked all the same, first
	pf = newPodFx(t)
	pf.pod.delErr = &k8s.ConflictError{Namespace: "cc-sandbox", Pod: k8s.PodName(sid, 2), Why: "has another UID"}
	pf.refused("a pod that is not ours", "pod-stop", "pod_conflict")
	if len(pf.revoked) != 1 {
		t.Errorf("the token was not revoked when the delete was refused: %+v", pf.revoked)
	}
	// a revoke that fails never fails the stop; it says so, with what to revoke by hand
	pf = newPodFx(t)
	pf.revokeEr = errors.New("door down")
	code, _ := pf.call("pod-stop", sid)
	if code != 0 || len(pf.pod.deleted) != 1 || !strings.Contains(pf.audit.String(), "token-revoke") || !strings.Contains(pf.audit.String(), "FAILED, revoke by hand") || !strings.Contains(pf.audit.String(), "sid="+sid+" gen=2") {
		t.Errorf("exit %d deleted=%d audit %q", code, len(pf.pod.deleted), pf.audit.String())
	}
}

func TestAPodLoadThatFailsRevokesTheTokenItMayHaveSigned(t *testing.T) {
	pf := newPodFx(t)
	pf.pod.loadErrs = []error{fmt.Errorf("side load refused: bad key: %w", side.ErrRefused)}
	if code, _ := pf.call("pod-load", sid); code != 1 || len(pf.revoked) != 1 || pf.revoked[0].Sid != sid || pf.revoked[0].Gen != 2 {
		t.Errorf("a refused load: revoked %+v", pf.revoked)
	}
	pf = newPodFx(t)
	pf.mintEr = errors.New("signer refused")
	if code, _ := pf.call("pod-load", sid); code != 1 || len(pf.revoked) != 1 {
		t.Errorf("a failed mint: revoked %+v", pf.revoked)
	}
	// a hub that refuses the relay after the token was signed: the signed token is revoked too
	pf = newPodFx(t)
	pf.hubSt = 500
	if code, _ := pf.call("pod-load", sid); code != 3 || len(pf.revoked) != 1 {
		t.Errorf("a refused relay: exit %d revoked %+v", code, pf.revoked)
	}
	// a load that works revokes nothing
	pf = newPodFx(t)
	if code, _ := pf.call("pod-load", sid); code != 0 || len(pf.revoked) != 0 {
		t.Errorf("a good load: %d revoked %+v", code, pf.revoked)
	}
	// a pod that never came up minted nothing: nothing to revoke here (hostd's pod-stop does it)
	pf = newPodFx(t)
	pf.pod.phases = []k8s.Status{{Phase: "Pending"}}
	if code, _ := pf.call("pod-load", sid); code != 4 || len(pf.revoked) != 0 {
		t.Errorf("never ready: %d revoked %+v", code, pf.revoked)
	}
}

func TestRevokeDoorsAreChecked(t *testing.T) {
	f := newFx(t)
	p := f.dir + "/etc/c.toml"
	base := "allowed_roots = [\"" + f.roots + "\"]\nmachine = \"m\"\n"
	for why, extra := range map[string]string{"credentials": "revoke_doors = [\"http://u:p@x:1\"]\n", "a path": "revoke_doors = [\"http://x:1/revoke\"]\n", "ftp": "revoke_doors = [\"ftp://x\"]\n"} {
		put(t, p, base+extra, 0o644)
		if _, err := LoadConfig(p, os.Getuid()); err == nil {
			t.Errorf("%s was accepted", why)
		}
	}
	put(t, p, base+"revoke_doors = [\"http://100.64.0.9:8091\", \"https://doors.example:8091\"]\n", 0o644)
	if c, err := LoadConfig(p, os.Getuid()); err != nil || len(c.RevokeDoors) != 2 {
		t.Errorf("good doors: %v", err)
	}
}
