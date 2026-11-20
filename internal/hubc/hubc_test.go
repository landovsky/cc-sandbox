// Tests of the hub client. Why: it runs in a daemon loop against a hub that can hang or vanish, so
// every call needs its own deadline and the connections must be shared, or one stuck request stalls
// a lease renewal (and every tracked session drops) and each request leaks a transport.
package hubc

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestEveryClientSharesOneTransport(t *testing.T) {
	a, b := &Client{URL: "http://x"}, &Client{URL: "http://y"}
	if a.http() != b.http() || a.http().Transport != sharedTransport {
		t.Fatal("each client built its own transport: connections are not reused and every request leaks one")
	}
}

// context: a hub that accepts the connection and never answers must not hang a call for longer than
// its own deadline (track, ack, presence: callTimeout; deliver: sendTimeout).
func TestACallToAHubThatNeverAnswersEndsAtItsDeadline(t *testing.T) {
	block := make(chan struct{})
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-block }))
	defer ts.Close()
	defer close(block)
	old := callTimeout
	callTimeout = 150 * time.Millisecond
	defer func() { callTimeout = old }()
	c := &Client{URL: ts.URL}
	t0 := time.Now()
	if _, err := c.Track(context.Background(), nil, 60); err == nil || time.Since(t0) > 3*time.Second {
		t.Fatalf("track: %v after %s", err, time.Since(t0))
	}
	t0 = time.Now()
	if err := c.Ack(context.Background(), Ack{Topic: "spoke:m/x", Seq: 1}); err == nil || time.Since(t0) > 3*time.Second {
		t.Fatalf("ack: %v after %s", err, time.Since(t0))
	}
}

func TestFindNodeResolvesATreeAddressLikeCcmsg(t *testing.T) {
	es := []map[string]any{
		{"id": "a1", "node": "cc-sandbox:3cvt", "updated_at": float64(10)},
		{"id": "a2", "node": "cc-sandbox:3cvt", "updated_at": float64(20)}, // a roll in progress: the newer wins
		{"id": "b1", "node": "cc-sandbox:other"},
		{"id": "c1"}, // no node
	}
	for _, addr := range []string{"cc-sandbox:3cvt", "cc-sandbox:#3cvt"} {
		if e := FindNode(es, addr); e == nil || e["id"] != "a2" {
			t.Errorf("%s -> %v", addr, e)
		}
	}
	if FindNode(es, "cc-sandbox:nobody") != nil || FindNode(es, "") != nil {
		t.Error("a node nobody holds was found")
	}
	if e := FindSid([]map[string]any{{"uid": "s1", "node": "n"}}, "s1"); e == nil || e["node"] != "n" {
		t.Error("FindSid")
	}
}
