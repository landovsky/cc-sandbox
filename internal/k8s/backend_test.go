// Tests of the backend against a FAKE API server (httptest): it keeps pods in memory, answers the
// four verbs the launcher may use, and, as the cluster's admission policy would, refuses a pod that
// breaks the k3s rules. The admission here is written separately from CheckContract, from the k3s
// side's list, so a wrong rule in one is caught by the other. Nothing here touches a cluster.
package k8s

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type fakeAPI struct {
	mu       sync.Mutex
	pods     map[string]Pod
	log      []string
	auth     []string
	failNext int // answer this status once
}

func newAPI(t *testing.T) (*fakeAPI, *REST) {
	t.Helper()
	f := &fakeAPI{pods: map[string]Pod{}}
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
		var p Pod
		if err := json.NewDecoder(r.Body).Decode(&p); err != nil {
			reply(400, map[string]string{"message": err.Error()})
			return
		}
		if why := admit(&p); why != "" {
			reply(422, map[string]string{"reason": "Invalid", "message": why})
			return
		}
		if _, ok := f.pods[p.Metadata.Name]; ok {
			reply(409, map[string]string{"reason": "AlreadyExists", "message": "exists"})
			return
		}
		p.Status.Phase = "Pending"
		f.pods[p.Metadata.Name] = p
		reply(201, p)
	case r.Method == "GET" && name == "":
		var items []Pod
		for _, p := range f.pods {
			if sel := r.URL.Query().Get("labelSelector"); sel == "" || p.Metadata.Labels[strings.SplitN(sel, "=", 2)[0]] == strings.SplitN(sel, "=", 2)[1] {
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

// admit is the cluster's policy, as the k3s side listed it: a rule per line.
func admit(p *Pod) string {
	s := p.Spec
	switch {
	case !strings.HasPrefix(p.Metadata.Name, "ccsbx-s-"):
		return "name must start with ccsbx-s-"
	case p.Metadata.Labels["cc-sandbox/role"] != "session":
		return "role must be session"
	case s.PriorityClassName != "ccsbx-session":
		return "priorityClassName must be ccsbx-session"
	case s.RuntimeClassName != nil || len(p.Metadata.Finalizers) > 0:
		return "no runtimeClassName or finalizers"
	case s.ShareProcessNamespace != nil && *s.ShareProcessNamespace:
		return "no shareProcessNamespace"
	case s.TerminationGracePeriodSeconds != nil && *s.TerminationGracePeriodSeconds > 60:
		return "grace over 60"
	case len(s.Containers) != 1 || s.Containers[0].Name != "fence" || len(s.InitContainers) > 1:
		return "one container named fence and at most one init container"
	case s.AutomountServiceAccountToken == nil || *s.AutomountServiceAccountToken:
		return "no service account token"
	}
	for _, c := range append(append([]Container{}, s.InitContainers...), s.Containers...) {
		if !strings.HasPrefix(c.Image, "ghcr.io/landovsky/ccsbx-fence@sha256:") && !strings.HasPrefix(c.Image, "ghcr.io/landovsky/ccsbx@sha256:") {
			return "image " + c.Image
		}
		if c.SecurityContext == nil || c.SecurityContext.Capabilities == nil || c.SecurityContext.Capabilities.Drop[0] != "ALL" {
			return "PSS restricted: drop ALL"
		}
	}
	for _, v := range s.Volumes {
		if v.EmptyDir == nil {
			return "only emptyDir volumes"
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
	if ref2, err := b.Create(ctx, goodSpec()); err != nil || ref2.Name != ref.Name || len(f.pods) != 1 {
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
	f.pods["ccsbx-s-door"] = Pod{Metadata: ObjectMeta{Name: "ccsbx-s-door", Labels: map[string]string{"cc-sandbox/role": "door"}}}
	if l, _ := b.List(ctx); len(l) != 0 {
		t.Errorf("a door pod was listed as a session: %v", l)
	}
}

func TestAPIErrorsAreReportedWithTheirStatus(t *testing.T) {
	b, f := newBackend(t)
	f.failNext = 403
	_, err := b.Get(context.Background(), "s0abcdefgh", 1)
	var ae *APIError
	if !errors.As(err, &ae) || ae.Status != 403 || ae.Msg != "boom" {
		t.Errorf("%v", err)
	}
	// a pod that exists but belongs to another generation is not returned as ours
	p, _ := PodFor(goodSpec())
	p.Metadata.Labels["ccsbx.gen"] = "9"
	f.pods[p.Metadata.Name] = *p
	if _, err := b.Create(context.Background(), goodSpec()); err == nil || !strings.Contains(err.Error(), "not this generation") {
		t.Errorf("%v", err)
	}
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
	if _, err := LoadConnConfig(w(`{"server":"https://10.0.0.1:6443","token_file":"/t"}`)); err != nil {
		t.Error(err)
	}
	for _, bad := range []string{`{"server":"http://x","token_file":"/t"}`, `{"server":"https://x"}`, `{"server":"https://x","token_file":"/t","extra":1}`, `not json`, `{"server":"","token_file":"/t"}`} {
		if _, err := LoadConnConfig(w(bad)); err == nil {
			t.Errorf("accepted %s", bad)
		}
	}
	if _, err := NewREST(&ConnConfig{Server: "https://x", TokenFile: "/t", CAFile: w("no pem")}); err == nil {
		t.Error("a CA file with no certificate was accepted")
	}
}

func TestStatusReadsTheFenceContainer(t *testing.T) {
	p, _ := PodFor(goodSpec())
	p.Status.Phase = "Failed"
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
