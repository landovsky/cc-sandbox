package token

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// context "a test verifier on loopback": the end-to-end shape of T:token-copied,
// T:token-gen-bump and T:token-unknown-kid as a door sees them (HTTP 401s).
func TestMiddlewareEndToEnd(t *testing.T) {
	now := time.Now()
	r := newRig(t)
	r.base.Sub = "tomas/hp-ubuntu/sHTTP/g1"
	tok, _ := Mint(r.key, r.base, now)
	srv := httptest.NewServer(r.v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		w.Write([]byte(ClaimsFrom(q).SID()))
	}), t.Logf))
	defer srv.Close()

	do := func(tok string, g *GenKey) int {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/messages?x=1", nil)
		req.Header.Set("Authorization", "Bearer "+tok)
		if g != nil {
			p, _ := SignProof(g, "POST", "/v1/messages?x=1", time.Now())
			req.Header.Set(ProofHeader, p)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	thief, _ := NewGenKey()
	if c := do(tok, r.gen); c != 200 {
		t.Fatalf("legit: %d", c)
	}
	if c := do(tok, thief); c != 401 {
		t.Fatalf("token-copied with foreign key: %d", c)
	}
	if c := do(tok, nil); c != 401 {
		t.Fatalf("token without proof: %d", c)
	}
	if c := do("", nil); c != 401 {
		t.Fatalf("no token: %d", c)
	}
	other, _ := LoadOrCreateKeyFile(t.TempDir() + "/o")
	bad, _ := Mint(other, r.base, now)
	if c := do(bad, r.gen); c != 401 {
		t.Fatalf("unknown kid: %d", c)
	}
}
