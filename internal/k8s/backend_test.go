// Tests of the backend against a FAKE API server (httptest): it keeps pods in memory, answers the
// four verbs the launcher may use, and, as the cluster's admission policy would, refuses a pod that
// breaks the k3s rules. The admission here is written separately from CheckContract, from the k3s
// side's list, so a wrong rule in one is caught by the other. Nothing here touches a cluster.
package k8s

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
)

type fakeAPI struct {
	mu       sync.Mutex
	pods     map[string]map[string]any // stored as the server saw them: decoded raw, not through our types
	log      []string
	auth     []string
	failNext int // answer this status once
	nextUID  int
	noUID    bool // the server leaves the UID out (a broken or hostile answer)
}

func newAPI(t *testing.T) (*fakeAPI, *REST) {
	t.Helper()
	f := &fakeAPI{pods: map[string]map[string]any{}}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	tok := filepath.Join(t.TempDir(), "token")
	os.WriteFile(tok, []byte("launcher-token\n"), 0o600)
	return f, &REST{Base: srv.URL, TokenFile: tok, HTTP: srv.Client()}
}

func (f *fakeAPI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.log = append(f.log, r.Method+" "+r.URL.RequestURI())
	f.auth = append(f.auth, r.Header.Get("Authorization"))
	reply := func(code int, v any) { w.WriteHeader(code); json.NewEncoder(w).Encode(v) }
	if f.failNext != 0 {
		c := f.failNext
		f.failNext = 0
		reply(c, map[string]string{"reason": "Forbidden", "message": "boom"})
		return
	}
	const base = "/api/v1/namespaces/cc-sandbox/pods"
	if !strings.HasPrefix(r.URL.Path, base) { // the launcher's RBAC: pods in cc-sandbox, nothing else
		reply(403, map[string]string{"reason": "Forbidden", "message": "namespace not allowed: " + r.URL.Path})
		return
	}
	name := strings.TrimPrefix(strings.TrimPrefix(r.URL.Path, base), "/")
	switch {
	case r.Method == "POST" && name == "":
		var p map[string]any
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			reply(400, map[string]string{"message": err.Error()})
			return
		}
		if why := admit(p); why != "" {
			reply(422, map[string]string{"reason": "Invalid", "message": why})
			return
		}
		n := str(dig(p, "metadata", "name"))
		if _, ok := f.pods[n]; ok {
			reply(409, map[string]string{"reason": "AlreadyExists", "message": "exists"})
			return
		}
		p["status"] = map[string]any{"phase": "Pending"}
		f.nextUID++
		if !f.noUID {
			p["metadata"].(map[string]any)["uid"] = fmt.Sprintf("uid-%d", f.nextUID)
		}
		// what the server's own admission adds: its default tolerations
		spec := p["spec"].(map[string]any)
		spec["tolerations"] = append(spec["tolerations"].([]any),
			map[string]any{"key": "node.kubernetes.io/not-ready", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 300},
			map[string]any{"key": "node.kubernetes.io/unreachable", "operator": "Exists", "effect": "NoExecute", "tolerationSeconds": 300})
		f.pods[n] = p
		reply(201, p)
	case r.Method == "GET" && name == "":
		var items []any
		for _, p := range f.pods {
			sel := r.URL.Query().Get("labelSelector")
			if sel == "" || str(dig(p, "metadata", "labels", strings.SplitN(sel, "=", 2)[0])) == strings.SplitN(sel, "=", 2)[1] {
				items = append(items, p)
			}
		}
		reply(200, map[string]any{"items": items})
	case r.Method == "GET":
		if p, ok := f.pods[name]; ok {
			reply(200, p)
			return
		}
		reply(404, map[string]string{"reason": "NotFound"})
	case r.Method == "DELETE":
		if _, ok := f.pods[name]; !ok {
			reply(404, map[string]string{"reason": "NotFound"})
			return
		}
		delete(f.pods, name)
		reply(200, map[string]string{"status": "Success"})
	default:
		reply(405, nil)
	}
}

// dig walks a raw decoded JSON document; str turns what it finds into a string ("" if absent).
func dig(m any, path ...string) any {
	for _, k := range path {
		mm, ok := m.(map[string]any)
		if !ok {
			return nil
		}
		m = mm[k]
	}
	return m
}
func str(v any) string  { s, _ := v.(string); return s }
func list(v any) []any  { l, _ := v.([]any); return l }
func isTrue(v any) bool { b, _ := v.(bool); return b }

// size reads a quantity the way the cluster does, in MiB; ok is false for anything else.
func size(v any) (mi int64, ok bool) {
	q := str(v)
	var n int64
	switch {
	case strings.HasSuffix(q, "Gi"):
		_, err := fmt.Sscanf(strings.TrimSuffix(q, "Gi"), "%d", &n)
		return n * 1024, err == nil
	case strings.HasSuffix(q, "Mi"):
		_, err := fmt.Sscanf(strings.TrimSuffix(q, "Mi"), "%d", &n)
		return n, err == nil
	}
	return 0, false
}

// admit is the cluster's policy, written here from the k3s side's list and reading the RAW pod JSON,
// not our types: rules 1-8, the launcher VAP, PSS restricted. It never panics on a short list.
func admit(p map[string]any) string {
	name, labels, spec := str(dig(p, "metadata", "name")), dig(p, "metadata", "labels"), dig(p, "spec")
	if !strings.HasPrefix(name, "ccsbx-s-") || len(name) > 63 {
		return "the name must start with ccsbx-s- (1)"
	}
	if ns := str(dig(p, "metadata", "namespace")); ns != "" && ns != "cc-sandbox" {
		return "the namespace of the URL (1)"
	}
	if str(dig(labels, "cc-sandbox/role")) != "session" {
		return "label cc-sandbox/role=session (2)"
	}
	if len(list(dig(p, "metadata", "finalizers"))) > 0 || dig(spec, "runtimeClassName") != nil {
		return "no finalizers or runtimeClassName (3)"
	}
	if isTrue(dig(spec, "shareProcessNamespace")) || isTrue(dig(spec, "hostNetwork")) || isTrue(dig(spec, "hostPID")) || isTrue(dig(spec, "hostIPC")) {
		return "no shared or host namespaces (3)"
	}
	if g, ok := dig(spec, "terminationGracePeriodSeconds").(float64); ok && (g > 60 || g < 0) {
		return "grace 0..60 (3)"
	}
	if str(dig(spec, "priorityClassName")) != "ccsbx-session" {
		return "priorityClassName must be ccsbx-session (4)"
	}
	if str(dig(spec, "nodeSelector", "kubernetes.io/hostname")) != "future-two" {
		return "nodeSelector kubernetes.io/hostname=future-two (5)"
	}
	tol := false
	for _, t := range list(dig(spec, "tolerations")) {
		if str(dig(t, "key")) == "pool" && str(dig(t, "value")) == "shared" && str(dig(t, "effect")) == "NoSchedule" && str(dig(t, "operator")) == "Equal" {
			tol = true
		}
	}
	if !tol {
		return "toleration pool=shared:NoSchedule (5)"
	}
	if v, set := dig(spec, "automountServiceAccountToken").(bool); !set || v {
		return "automountServiceAccountToken false (6)"
	}
	for _, v := range list(dig(spec, "volumes")) {
		if dig(v, "emptyDir") == nil {
			return "only emptyDir volumes: no Secret, projected, hostPath (6)"
		}
		if mi, ok := size(dig(v, "emptyDir", "sizeLimit")); !ok || mi <= 0 || mi > 40*1024 {
			return "every emptyDir needs a sizeLimit of at most 40Gi (6)"
		}
	}
	cs, inits := list(dig(spec, "containers")), list(dig(spec, "initContainers"))
	if len(cs) != 1 || str(dig(cs[0], "name")) != "fence" || len(inits) > 1 || (len(inits) == 1 && str(dig(inits[0], "name")) != "side") {
		return "one container named fence and at most one init container named side (7)"
	}
	for _, c := range append(append([]any{}, inits...), cs...) {
		img := str(dig(c, "image"))
		if !strings.HasPrefix(img, "ghcr.io/landovsky/ccsbx-fence@sha256:") && !strings.HasPrefix(img, "ghcr.io/landovsky/ccsbx@sha256:") {
			return "image from our ghcr repos by digest (7)"
		}
		if h := img[strings.Index(img, "sha256:")+7:]; len(h) != 64 {
			return "a full sha256 digest (7)"
		}
		for key, max := range map[string]int64{"memory": 4 * 1024, "ephemeral-storage": 40 * 1024} {
			if mi, ok := size(dig(c, "resources", "limits", key)); !ok || mi <= 0 || mi > max {
				return "limits: at most 4Gi memory, 40Gi ephemeral (8)"
			}
		}
		if cpu := str(dig(c, "resources", "limits", "cpu")); cpu == "" || cpu == "0" {
			return "a cpu limit (8)"
		} else if n, err := strconv.Atoi(cpu); err != nil && !strings.HasSuffix(cpu, "m") || err == nil && n > 4 {
			return "at most 4 CPU (8)"
		}
		// PSS restricted
		sc := dig(c, "securityContext")
		if isTrue(dig(sc, "allowPrivilegeEscalation")) || dig(sc, "allowPrivilegeEscalation") == nil || isTrue(dig(sc, "privileged")) {
			return "PSS restricted: no privilege escalation, not privileged (8)"
		}
		if d := list(dig(sc, "capabilities", "drop")); len(d) == 0 || str(d[0]) != "ALL" || len(list(dig(sc, "capabilities", "add"))) > 0 {
			return "PSS restricted: drop ALL, add none (8)"
		}
		if !isTrue(dig(sc, "runAsNonRoot")) && !isTrue(dig(spec, "securityContext", "runAsNonRoot")) {
			return "PSS restricted: runAsNonRoot (8)"
		}
		if str(dig(spec, "securityContext", "seccompProfile", "type")) != "RuntimeDefault" && str(dig(sc, "seccompProfile", "type")) != "RuntimeDefault" {
			return "PSS restricted: seccomp RuntimeDefault (8)"
		}
	}
	return ""
}

func newBackend(t *testing.T) (*Backend, *fakeAPI) {
	f, rest := newAPI(t)
	return NewBackend(rest, nil, DefaultConfig()), f
}

func TestCreateGetListDeleteAgainstTheFakeAPI(t *testing.T) {
	b, f := newBackend(t)
	ctx := context.Background()
	ref, err := b.Create(ctx, goodSpec())
	if err != nil || ref.Name != "ccsbx-s-s0abcdefgh-g2" || ref.Namespace != "cc-sandbox" || ref.Gen != 2 {
		t.Fatalf("%+v %v", ref, err)
	}
	for _, a := range f.auth {
		if a != "Bearer launcher-token" {
			t.Errorf("authorization %q", a)
		}
	}
	st, err := b.Get(ctx, "s0abcdefgh", 2)
	if err != nil || st.Phase != "Pending" || st.Ref.Sid != "s0abcdefgh" || st.Ref.Gen != 2 {
		t.Fatalf("%+v %v", st, err)
	}
	// idempotent: the same generation again is the answer, not an error and not a second pod
	again := goodSpec()
	again.KnownUID = ref.UID
	if ref2, err := b.Create(ctx, again); err != nil || ref2.Name != ref.Name || ref2.UID != ref.UID || len(f.pods) != 1 {
		t.Errorf("a second create: %+v %v (%d pods)", ref2, err, len(f.pods))
	}
	other := goodSpec()
	other.Sid, other.Gen = "s0zzzzzzzz", 1
	b.Create(ctx, other)
	if l, err := b.List(ctx); err != nil || len(l) != 2 {
		t.Errorf("list %v %v", l, err)
	}
	if err := b.Delete(ctx, "s0abcdefgh", 2); err != nil || len(f.pods) != 1 {
		t.Errorf("delete: %v (%d pods)", err, len(f.pods))
	}
	if err := b.Delete(ctx, "s0abcdefgh", 2); err != nil {
		t.Errorf("deleting an absent pod is not an error: %v", err)
	}
	if _, err := b.Get(ctx, "s0abcdefgh", 2); !errors.Is(err, ErrNotFound) {
		t.Errorf("get after delete: %v", err)
	}
	last := f.log[len(f.log)-2]
	if !strings.Contains(last, "gracePeriodSeconds=30") && !strings.Contains(strings.Join(f.log, " "), "gracePeriodSeconds=30") {
		t.Errorf("no grace period on delete: %v", f.log)
	}
}

// What Create sends is what the cluster accepts; a spec that breaks the contract never reaches it.
func TestCreateNeverSendsAPodTheContractRefuses(t *testing.T) {
	b, f := newBackend(t)
	s := goodSpec()
	s.MemoryMB = 9000
	if _, err := b.Create(context.Background(), s); err == nil {
		t.Error("an oversized pod was sent")
	}
	if len(f.log) != 0 {
		t.Errorf("the API was called: %v", f.log)
	}
	// and the API's own refusal comes back as an APIError with the status
	b.Cfg.PriorityClass = "other-class" // the fake cluster only admits ccsbx-session
	_, err := b.Create(context.Background(), goodSpec())
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 422 || !strings.Contains(ae.Msg, "priorityClassName") {
		t.Errorf("%v", err)
	}
}

func TestDeleteGraceNeverExceeds60(t *testing.T) {
	b, f := newBackend(t)
	b.Cfg.GraceSeconds = 600
	b.Delete(context.Background(), "s0abcdefgh", 1)
	ok := false
	for _, l := range f.log {
		ok = ok || strings.HasSuffix(l, "gracePeriodSeconds=60")
	}
	if !ok {
		t.Errorf("%v", f.log)
	}
}

// The launcher reaches pods of one namespace by exact name; a bad sid or generation never becomes a
// request, and a pod of another role in the list is not ours.
func TestBackendRefusesBadNamesAndForeignPods(t *testing.T) {
	b, f := newBackend(t)
	ctx := context.Background()
	for _, sid := range []string{"", "../x", "S0ABC", "s0abc/def"} {
		if _, err := b.Get(ctx, sid, 1); err == nil {
			t.Errorf("get %q", sid)
		}
		if err := b.Delete(ctx, sid, 1); err == nil {
			t.Errorf("delete %q", sid)
		}
	}
	if _, err := b.Get(ctx, "s0abcdefgh", 0); err == nil {
		t.Error("generation 0")
	}
	if len(f.log) != 0 {
		t.Errorf("requests were made: %v", f.log)
	}
	f.pods["ccsbx-s-door"] = rawPod(Pod{Metadata: ObjectMeta{Name: "ccsbx-s-door", Labels: map[string]string{"cc-sandbox/role": "door"}}})
	if l, _ := b.List(ctx); len(l) != 0 {
		t.Errorf("a door pod was listed as a session: %v", l)
	}
}

func rawPod(p Pod) map[string]any {
	b, _ := json.Marshal(p)
	var m map[string]any
	json.Unmarshal(b, &m)
	return m
}

func TestAPIErrorsAreReportedWithTheirStatus(t *testing.T) {
	b, f := newBackend(t)
	f.failNext = 403
	_, err := b.Get(context.Background(), "s0abcdefgh", 1)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 403 || ae.Msg != "boom" {
		t.Errorf("%v", err)
	}
}

// S2: a pod already there under the name is "ours" only if it is THIS pod: the same spec hash, still
// within the contract, and not being deleted. The server's own default tolerations do not matter.
func TestCreateOnAConflictAcceptsOnlyTheSamePod(t *testing.T) {
	ctx := context.Background()
	b, f := newBackend(t)
	first, err := b.Create(ctx, goodSpec())
	if err != nil || first.UID == "" {
		t.Fatalf("%+v %v", first, err)
	}
	name := PodName("s0abcdefgh", 2)
	again := goodSpec()
	again.KnownUID = first.UID // what the session record holds after a restart
	if r, err := b.Create(ctx, again); err != nil || r.UID != first.UID {
		t.Errorf("the same pod again (recorded UID, server's default tolerations): %+v %v", r, err)
	}
	cases := map[string]func(m map[string]any){
		"another generation's labels": func(m map[string]any) { dig(m, "metadata", "labels").(map[string]any)["ccsbx.gen"] = "9" },
		"a pod being deleted": func(m map[string]any) {
			dig(m, "metadata").(map[string]any)["deletionTimestamp"] = "2026-11-20T10:00:00Z"
		},
		"a pod from another spec (another hash)": func(m map[string]any) {
			dig(m, "metadata", "annotations").(map[string]any)[HashAnnotation] = strings.Repeat("0", 64)
		},
		"a pod with the right hash copied onto another image": func(m map[string]any) {
			list(dig(m, "spec", "containers"))[0].(map[string]any)["image"] = "ghcr.io/landovsky/ccsbx-fence@sha256:" + strings.Repeat("e", 64)
		},
		"a pod with the right hash and another env value": func(m map[string]any) {
			c := list(dig(m, "spec", "containers"))[0].(map[string]any)
			c["env"] = append(list(c["env"]), map[string]any{"name": "X", "value": "1"})
		},
		"a pod with no hash": func(m map[string]any) { delete(dig(m, "metadata").(map[string]any), "annotations") },
		"a pod that has since broken the contract": func(m map[string]any) {
			list(dig(m, "spec", "containers"))[0].(map[string]any)["securityContext"].(map[string]any)["privileged"] = true
		},
	}
	for why, mod := range cases {
		orig := f.pods[name]
		b2, _ := json.Marshal(orig)
		var cp map[string]any
		json.Unmarshal(b2, &cp)
		mod(cp)
		f.pods[name] = cp
		if _, err := b.Create(ctx, again); err == nil {
			t.Errorf("%s was accepted as ours", why)
		}
		f.pods[name] = orig
	}
}

// S6: every violation of the contract, sent RAW to the fake cluster (past PodFor and
// CheckContract), is refused by the cluster's own rules too, for each rule the k3s side lists.
func TestTheFakeClusterRefusesWhatTheContractRefuses(t *testing.T) {
	covered := 0
	for _, tc := range contractCases() {
		_, r := newAPI(t) // a fresh cluster per case: an admitted pod would make the next one a conflict
		p := goodPod(t)
		tc.mod(p)
		_, err := r.CreatePod(context.Background(), "cc-sandbox", p)
		var ae *APIError
		clusterRefuses := errors.As(err, &ae) && ae.Status == 422
		if clusterRefuses {
			covered++
		}
		// rules that are ours alone (the design: uids, volume ownership, per-container repos, ports,
		// credentials in env, names derived from labels) are not the cluster's to enforce
		if !clusterRefuses && !oursAlone(tc.name) {
			t.Errorf("%s: the cluster's admission let it through (%v)", tc.name, err)
		}
	}
	if covered < 40 {
		t.Errorf("only %d violations were refused by the cluster's rules", covered)
	}
	// the good pod is admitted
	_, r := newAPI(t)
	if _, err := r.CreatePod(context.Background(), "cc-sandbox", goodPod(t)); err != nil {
		t.Errorf("the cluster refused the good pod: %v", err)
	}
	// and an empty capability list, a short list and a bare object do not panic it
	for _, raw := range []string{`{}`, `{"metadata":{"name":"ccsbx-s-x","labels":{"cc-sandbox/role":"session"}},"spec":{"containers":[{"name":"fence","securityContext":{"capabilities":{"drop":[]}}}]}}`} {
		resp, err := http.Post(r.Base+"/api/v1/namespaces/cc-sandbox/pods", "application/json", strings.NewReader(raw))
		if err != nil || resp.StatusCode != 422 {
			t.Errorf("%s: %v %v", raw, resp, err)
		}
	}
}

// oursAlone names the contract cases the cluster's list does not cover: they are this design's.
func oursAlone(name string) bool {
	for _, w := range []string{"uid", "root", "group", "sidecar", "fence", "mount", "write", "port", "credential", "key in", "environment", "derived", "ends in a dash", "state", "relay", "volume that is not an emptyDir", "no worktree", "init container", "unset", "pod not", "extra node selector", "toleration of everything", "hostUsers", "DNS", "nameserver", "dropped with another", "requests below"} {
		if strings.Contains(name, w) {
			return true
		}
	}
	return false
}

// The token is read on every request: a renewal needs no restart, and a missing token is an error
// rather than an anonymous request.
func TestTokenIsReadPerRequest(t *testing.T) {
	_, r := newAPI(t)
	b := NewBackend(r, nil, DefaultConfig())
	os.WriteFile(r.TokenFile, []byte("renewed\n"), 0o600)
	b.List(context.Background())
	os.Remove(r.TokenFile)
	if _, err := b.List(context.Background()); err == nil || !strings.Contains(err.Error(), "token") {
		t.Errorf("a missing token was not an error: %v", err)
	}
}

func TestConnConfig(t *testing.T) {
	d := t.TempDir()
	w := func(body string) string {
		p := filepath.Join(d, "c.json")
		os.WriteFile(p, []byte(body), 0o600)
		return p
	}
	if _, err := LoadConnConfig(w(`{"server":"https://10.0.0.1:6443","token_file":"/t","ca_file":"/ca"}`)); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{
		`{"server":"http://x","token_file":"/t","ca_file":"/ca"}`, `{"server":"https://x","ca_file":"/ca"}`,
		`{"server":"https://x","token_file":"/t","ca_file":"/ca","extra":1}`, `not json`, `{"server":"","token_file":"/t","ca_file":"/ca"}`,
		`{"server":"https://x","token_file":"/t"}`, // S3: no CA, no system roots
		`{"server":"https://x/api","token_file":"/t","ca_file":"/ca"}`, `{"server":"https://u:p@x","token_file":"/t","ca_file":"/ca"}`,
		`{"server":"https://x?a=b","token_file":"/t","ca_file":"/ca"}`,
	} {
		if _, err := LoadConnConfig(w(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := NewREST(&ConnConfig{Server: "https://x", TokenFile: "/t", CAFile: w("no pem")}); err == nil {
		t.Error("a CA file with no certificate was accepted")
	}
	if _, err := NewREST(&ConnConfig{Server: "https://x", TokenFile: "/t"}); err == nil || !strings.Contains(err.Error(), "ca_file is required") {
		t.Errorf("a client with no CA was built (it would trust every public CA): %v", err)
	}
}

// S4: a redirect is never followed (it would replay the body and keep the token); it is an error.
func TestRedirectsAreNotFollowed(t *testing.T) {
	var hit bool
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { hit = true }))
	defer target.Close()
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+r.URL.Path, http.StatusTemporaryRedirect)
	}))
	defer redir.Close()
	_, r := newAPI(t)
	c, err := NewREST(&ConnConfig{Server: "https://x", TokenFile: r.TokenFile, CAFile: writeTestCA(t)})
	if err != nil {
		t.Fatal(err)
	}
	c.Base = redir.URL // plain http for the test; only the redirect policy is under test
	c.HTTP.Transport = nil
	_, err = c.CreatePod(context.Background(), "cc-sandbox", goodPod(t))
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 307 || hit {
		t.Errorf("err=%v, the redirect target was hit: %v", err, hit)
	}
}

func writeTestCA(t *testing.T) string {
	t.Helper()
	srv := httptest.NewTLSServer(http.NotFoundHandler())
	defer srv.Close()
	p := filepath.Join(t.TempDir(), "ca.pem")
	pem := string(pemOf(srv))
	os.WriteFile(p, []byte(pem), 0o600)
	return p
}

// N2/N3: the server's words are clipped and cleaned, an empty token is refused, an oversized answer
// is reported as such.
func TestServerTextAndEmptyTokenAndBigAnswers(t *testing.T) {
	f, r := newAPI(t)
	_ = f
	evil := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		switch req.URL.Query().Get("labelSelector") {
		case "big":
			w.Write(append([]byte(`{"items":[],"pad":"`), make([]byte, 9<<20)...))
		default:
			w.WriteHeader(422)
			json.NewEncoder(w).Encode(map[string]string{"reason": "Invalid\x1b]52;c;Zm9v\x07", "message": "bad\x1b[2J" + strings.Repeat("x", 5000)})
		}
	}))
	defer evil.Close()
	r.Base = evil.URL
	_, err := r.GetPod(context.Background(), "cc-sandbox", "x")
	var ae *APIError
	if !errors.As(err, &ae) || strings.ContainsAny(ae.Msg+ae.Reason, "\x1b\x07") || len(ae.Msg) > 1100 {
		t.Errorf("the server's text was not cleaned: %q", err)
	}
	if _, err := r.ListPods(context.Background(), "cc-sandbox", "big"); err == nil || !strings.Contains(err.Error(), "larger than") {
		t.Errorf("an oversized answer: %v", err)
	}
	os.WriteFile(r.TokenFile, []byte(" \n"), 0o600)
	if _, err := r.ListPods(context.Background(), "cc-sandbox", "x"); err == nil || !strings.Contains(err.Error(), "empty") {
		t.Errorf("an empty token was sent: %v", err)
	}
}

// N4: a server that returns another pod, or a list item that does not round-trip, is not believed.
func TestGetAndListDoNotTrustTheServersLabels(t *testing.T) {
	b, f := newBackend(t)
	ctx := context.Background()
	b.Create(ctx, goodSpec())
	name := PodName("s0abcdefgh", 2)
	m := f.pods[name]
	dig(m, "metadata", "labels").(map[string]any)["ccsbx.gen"] = "2x"
	if _, err := b.Get(ctx, "s0abcdefgh", 2); err == nil {
		t.Error("a pod whose generation label is '2x' was returned for generation 2")
	}
	if l, _ := b.List(ctx); len(l) != 0 {
		t.Errorf("a pod that does not round-trip was listed: %v", l)
	}
}

func TestStatusReadsTheFenceContainer(t *testing.T) {
	p, _ := PodFor(goodSpec())
	p.Status = &PodStatus{Phase: "Failed"}
	cs := ContainerStatus{Name: "fence"}
	cs.State.Terminated = &struct {
		ExitCode int    `json:"exitCode"`
		Reason   string `json:"reason,omitempty"`
	}{ExitCode: 137, Reason: "OOMKilled"}
	p.Status.ContainerStatuses = []ContainerStatus{cs}
	st := statusOf(p)
	if st.Phase != "Failed" || st.FenceExit == nil || *st.FenceExit != 137 || st.Ref.Gen != 2 {
		t.Errorf("%+v", st)
	}
}

// pemOf is the certificate a TLS test server presents, as PEM.
func pemOf(srv *httptest.Server) []byte {
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
}

// The UID from our own 201 is the identity of the pod. A planted pod can copy the labels, the hash
// annotation and every field we model, and carry a lifecycle hook we cannot see; it cannot copy the
// UID. With no recorded UID, or another one, the pod is refused as pod_conflict, naming the pod.
func TestAPlantedPodIsRefusedByUID(t *testing.T) {
	ctx := context.Background()
	b, f := newBackend(t)
	name := PodName("s0abcdefgh", 2)
	// the owner's real pod, made by us, then replaced by a plant that copies everything but the UID
	real, err := b.Create(ctx, goodSpec())
	if err != nil {
		t.Fatal(err)
	}
	plant := map[string]any{}
	raw, _ := json.Marshal(f.pods[name])
	json.Unmarshal(raw, &plant)
	dig(plant, "metadata").(map[string]any)["uid"] = "uid-planted"
	list(dig(plant, "spec", "containers"))[1-1].(map[string]any)["lifecycle"] = map[string]any{
		"postStart": map[string]any{"exec": map[string]any{"command": []any{"x"}}}}
	f.pods[name] = plant

	var ce *ConflictError
	noUID := goodSpec()
	if _, err := b.Create(ctx, noUID); !errors.As(err, &ce) || ce.Pod != name || !strings.Contains(err.Error(), "pod_conflict") || !strings.Contains(err.Error(), name) {
		t.Errorf("no recorded UID: %v", err)
	}
	wrong := goodSpec()
	wrong.KnownUID = real.UID
	if _, err := b.Create(ctx, wrong); !errors.As(err, &ce) {
		t.Errorf("a different UID: %v", err)
	}
	// and nothing is exec'd into it, with its stdin never read
	fx := &fakeExecer{}
	b.X = fx
	in := &readCounter{r: strings.NewReader("TOKEN")}
	if _, err := b.Exec(ctx, *real, "side", []string{"cat"}, IO{Stdin: in}); !errors.As(err, &ce) || fx.calls != 0 || in.n != 0 {
		t.Errorf("exec into a pod with another UID: %v (calls %d, stdin %d)", err, fx.calls, in.n)
	}
	// the restart path: the same pod, UID recorded, goes through and execs
	f.pods[name] = func() map[string]any {
		var m map[string]any
		r, _ := json.Marshal(plant)
		json.Unmarshal(r, &m)
		dig(m, "metadata").(map[string]any)["uid"] = real.UID
		delete(list(dig(m, "spec", "containers"))[0].(map[string]any), "lifecycle")
		return m
	}()
	ok := goodSpec()
	ok.KnownUID = real.UID
	r, err := b.Create(ctx, ok)
	if err != nil || r.UID != real.UID {
		t.Fatalf("restart with the recorded UID: %+v %v", r, err)
	}
	if _, err := b.Exec(ctx, *r, "side", []string{"cat"}, IO{}); err != nil || fx.calls != 1 {
		t.Errorf("exec into our pod: %v (calls %d)", err, fx.calls)
	}
	// a Ref with no UID is never exec'd into
	noRef := *r
	noRef.UID = ""
	if _, err := b.Exec(ctx, noRef, "side", []string{"cat"}, IO{}); err == nil || fx.calls != 1 {
		t.Errorf("a ref with no UID: %v", err)
	}
}

// Empty must never equal empty: a pod with no UID is neither created-by-us nor a match for "no UID recorded".
func TestAnEmptyUIDNeverMatches(t *testing.T) {
	ctx := context.Background()
	b, f := newBackend(t)
	f.noUID = true
	if r, err := b.Create(ctx, goodSpec()); err == nil {
		t.Errorf("a 201 without a UID was accepted: %+v", r)
	}
	// the pod is there now, without a UID; "no UID recorded" does not match it
	if _, err := b.Create(ctx, goodSpec()); err == nil {
		t.Error("a pod with no UID matched an empty recorded UID")
	}
	b.X = &fakeExecer{}
	ref, _ := b.ref("s0abcdefgh", 2)
	if _, err := b.Exec(ctx, ref, "side", []string{"cat"}, IO{}); err == nil {
		t.Error("exec into a pod with no UID, with a Ref with no UID")
	}
}
