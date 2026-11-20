package revoke

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/landovsky/cc-sandbox/internal/token"
)

type seen struct{ method, path, hdr, body string }

func door(t *testing.T, code int) (*httptest.Server, *[]seen) {
	var got []seen
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got = append(got, seen{r.Method, r.URL.Path, r.Header.Get(token.RevokeHeader), string(b)})
		w.WriteHeader(code)
	}))
	t.Cleanup(s.Close)
	return s, &got
}

// context "the signed revoke goes to every door, exactly as signed, and a door that is down does not stop the
// others"
func TestPostGoesToEveryDoorExactlyAsSigned(t *testing.T) {
	a, ga := door(t, 204)
	b, gb := door(t, 204)
	body := []byte(`{"owner":"tomas","machine":"hp-ubuntu","sid":"s0abcdefgh","gen":2}`)
	if err := (Doors{URLs: []string{a.URL, b.URL}}).Post(context.Background(), "kid.1.sig", body); err != nil {
		t.Fatal(err)
	}
	for i, g := range []*[]seen{ga, gb} {
		if len(*g) != 1 || (*g)[0] != (seen{"POST", token.RevokePath, "kid.1.sig", string(body)}) {
			t.Errorf("door %d: %+v", i, *g)
		}
	}
	down := httptest.NewServer(http.NotFoundHandler())
	down.Close()
	c, gc := door(t, 204)
	err := (Doors{URLs: []string{down.URL, c.URL}}).Post(context.Background(), "h", body)
	if err == nil || len(*gc) != 1 {
		t.Errorf("a dead door: %v, the live one got %d", err, len(*gc))
	}
}

func TestPostRefusesWhatItCannotTrust(t *testing.T) {
	if err := (Doors{}).Post(context.Background(), "h", []byte("{}")); err == nil {
		t.Error("no doors configured passed")
	}
	srv, got := door(t, 401)
	err := (Doors{URLs: []string{srv.URL}}).Post(context.Background(), "h", []byte("{}"))
	if err == nil || !strings.Contains(err.Error(), "401") || len(*got) != 1 {
		t.Errorf("a refusal: %v", err)
	}
	// a redirect is not followed: it would carry the signed request somewhere else
	other, og := door(t, 204)
	redir := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { http.Redirect(w, r, other.URL+"/x", 307) }))
	defer redir.Close()
	if err := (Doors{URLs: []string{redir.URL}}).Post(context.Background(), "h", []byte("{}")); err == nil || len(*og) != 0 {
		t.Errorf("a redirect was followed or accepted: %v (%d requests at the target)", err, len(*og))
	}
	for _, bad := range []string{"ftp://x", "http://u:p@x", "http://x/path", "x", "http://x?q=1", "http://"} {
		if CheckURL(bad) == nil {
			t.Errorf("%q passed", bad)
		}
	}
}
