// Tests of the hostd bus presence against a fake B11 hub. Why: the loop's promises (the lease is
// renewed, a frame is handled once even if its ack failed, a stopped daemon untracks, a bad
// credential is retried with a re-read token, never a crash) must hold without a real hub.
package hubspoke

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hubc"
)

// fakeHub is the B11 contract: track replaces the set, downlink returns the unacked frames,
// ack drops up to seq.
type fakeHub struct {
	mu        sync.Mutex
	tracked   [][]string // the key set of every track call
	ttls      []int
	queue     []hubc.Frame
	acks      []hubc.Ack
	failAck   int    // fail this many acks with 500
	authOK    string // the token accepted ("" = any)
	seenToken []string
}

func (h *fakeHub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mu.Lock()
		defer h.mu.Unlock()
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		h.seenToken = append(h.seenToken, tok)
		if h.authOK != "" && tok != h.authOK {
			w.WriteHeader(401)
			io.WriteString(w, `{"error":"unauthorized"}`)
			return
		}
		switch r.URL.Path {
		case "/v1/track":
			var b struct {
				Entries []map[string]any
				TTL     int
			}
			json.NewDecoder(r.Body).Decode(&b)
			var keys []string
			for _, e := range b.Entries {
				keys = append(keys, e["key"].(string))
			}
			h.tracked, h.ttls = append(h.tracked, keys), append(h.ttls, b.TTL)
			json.NewEncoder(w).Encode(map[string]any{"tracked": keys, "ttl": b.TTL})
		case "/v1/downlink":
			var out []hubc.Frame
			for _, f := range h.queue {
				out = append(out, f)
			}
			if out == nil {
				out = []hubc.Frame{}
			}
			h.mu.Unlock()
			if len(out) == 0 {
				time.Sleep(50 * time.Millisecond)
			}
			h.mu.Lock()
			json.NewEncoder(w).Encode(map[string]any{"messages": out, "next": 0})
		case "/v1/ack":
			var a hubc.Ack
			json.NewDecoder(r.Body).Decode(&a)
			if h.failAck > 0 {
				h.failAck--
				w.WriteHeader(500)
				return
			}
			h.acks = append(h.acks, a)
			keep := h.queue[:0]
			for _, f := range h.queue {
				if f.Seq > a.Seq {
					keep = append(keep, f)
				}
			}
			h.queue = keep
			json.NewEncoder(w).Encode(map[string]any{"acked": a.Seq})
		default:
			w.WriteHeader(404)
		}
	})
}

func (h *fakeHub) push(seq int64, text string, ref int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	p, _ := json.Marshal(map[string]any{"uid": "s0abc", "text": text, "ack_topic": "deliver_ack:x", "ack_ref": ref})
	h.queue = append(h.queue, hubc.Frame{Seq: seq, Event: "deliver", Payload: p})
}

func rig(t *testing.T, hub *fakeHub, on func(hubc.Deliver) Result) (*Spoke, context.CancelFunc, *[]string) {
	t.Helper()
	ts := httptest.NewServer(hub.handler())
	t.Cleanup(ts.Close)
	var logs []string
	var lm sync.Mutex
	tok := func() (string, error) { return "sbp1.good", nil }
	sp := New(Config{
		Client: &hubc.Client{URL: ts.URL, Token: tok}, Machine: "hp", Name: "ccsbx", TTL: 3, Backoff: 20 * time.Millisecond, PollWait: time.Second,
		Entries: func() []map[string]any {
			return []map[string]any{{"key": "ccsbx:s0abc", "machine": "hp", "fenced": true, "downlink": "spoke:hp/ccsbx", "uid": "s0abc"}}
		},
		OnDeliver: func(_ context.Context, _ hubc.Frame, d hubc.Deliver) Result { return on(d) },
		Logf:      func(f string, a ...any) { lm.Lock(); logs = append(logs, f); lm.Unlock() },
	})
	ctx, cancel := context.WithCancel(context.Background())
	go sp.Run(ctx)
	return sp, cancel, &logs
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 200; i++ {
		if cond() {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// context: hostd must appear on the bus: its fenced sessions are tracked under a lease and the lease
// is renewed before it runs out (a stopped daemon's sessions leave when it does).
func TestTracksFencedSessionsAndRenewsTheLease(t *testing.T) {
	hub := &fakeHub{}
	sp, cancel, _ := rig(t, hub, func(hubc.Deliver) Result { return Result{OK: true} })
	defer cancel()
	waitFor(t, "two renewals", func() bool { hub.mu.Lock(); defer hub.mu.Unlock(); return len(hub.tracked) >= 3 })
	hub.mu.Lock()
	if hub.tracked[0][0] != "ccsbx:s0abc" || hub.ttls[0] != 3 {
		t.Fatalf("%v %v", hub.tracked, hub.ttls)
	}
	hub.mu.Unlock()
	if sp.Status().Tracked != 1 {
		t.Fatalf("%+v", sp.Status())
	}
}

// context: the B1 acceptance: hostd registers and receives a test frame. The frame reaches the
// handler once and its result goes back in the ack, with the ack_topic/ack_ref the hub handed out
// (that is what the waiting /deliver sees).
func TestReceivesATestFrameAndAcksTheResult(t *testing.T) {
	hub := &fakeHub{}
	var got []string
	var mu sync.Mutex
	_, cancel, _ := rig(t, hub, func(d hubc.Deliver) Result {
		mu.Lock()
		got = append(got, d.Text)
		mu.Unlock()
		return Result{OK: true, Confirmed: true}
	})
	defer cancel()
	hub.push(1, "test frame", 77)
	waitFor(t, "the ack", func() bool { hub.mu.Lock(); defer hub.mu.Unlock(); return len(hub.acks) == 1 })
	hub.mu.Lock()
	a := hub.acks[0]
	hub.mu.Unlock()
	if a.Seq != 1 || a.AckTopic != "deliver_ack:x" || a.AckRef != 77 || !a.OK || !a.Confirmed || a.Topic != "spoke:hp/ccsbx" {
		t.Fatalf("%+v", a)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0] != "test frame" {
		t.Fatalf("%v", got)
	}
}

// context: an ack that fails (the network blinks) makes the hub send the frame again. It must be
// acked, never handled twice (a message typed into a pane twice).
func TestAFrameWhoseAckFailedIsNotHandledTwice(t *testing.T) {
	hub := &fakeHub{failAck: 2}
	n := 0
	var mu sync.Mutex
	_, cancel, _ := rig(t, hub, func(hubc.Deliver) Result { mu.Lock(); n++; mu.Unlock(); return Result{OK: true} })
	defer cancel()
	hub.push(5, "once", 9)
	waitFor(t, "the ack after two failures", func() bool { hub.mu.Lock(); defer hub.mu.Unlock(); return len(hub.acks) == 1 })
	mu.Lock()
	defer mu.Unlock()
	if n != 1 {
		t.Fatalf("handled %d times", n)
	}
}

// context: the credential is a file that can be rotated. A 401 is logged and retried (with the token
// read again each time); the daemon never exits.
func TestARejectedCredentialIsRetriedNotFatal(t *testing.T) {
	hub := &fakeHub{authOK: "sbp1.rotated"}
	ts := httptest.NewServer(hub.handler())
	defer ts.Close()
	cur := "sbp1.stale"
	var mu sync.Mutex
	sp := New(Config{
		Client:  &hubc.Client{URL: ts.URL, Token: func() (string, error) { mu.Lock(); defer mu.Unlock(); return cur, nil }},
		Machine: "hp", Name: "ccsbx", TTL: 3, Backoff: 20 * time.Millisecond, PollWait: time.Second,
		Entries: func() []map[string]any { return []map[string]any{{"key": "ccsbx:s0abc", "machine": "hp"}} },
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go sp.Run(ctx)
	waitFor(t, "an auth error", func() bool { return strings.Contains(sp.Status().LastErr, "401") })
	mu.Lock()
	cur = "sbp1.rotated"
	mu.Unlock()
	waitFor(t, "tracking with the rotated token", func() bool { return sp.Status().Tracked == 1 && sp.Status().LastErr == "" })
}

// context: when the daemon stops, its sessions must leave the registry at once (an empty track), not
// linger until the lease ends.
func TestStoppingUntracksEverything(t *testing.T) {
	hub := &fakeHub{}
	_, cancel, _ := rig(t, hub, func(hubc.Deliver) Result { return Result{} })
	waitFor(t, "first track", func() bool { hub.mu.Lock(); defer hub.mu.Unlock(); return len(hub.tracked) >= 1 })
	cancel()
	waitFor(t, "the empty track", func() bool {
		hub.mu.Lock()
		defer hub.mu.Unlock()
		return len(hub.tracked) > 0 && len(hub.tracked[len(hub.tracked)-1]) == 0
	})
}

// context: the hub (or one mailbox) restarts, so its sequence numbers and refs start again. A NEW frame
// that happens to carry the same seq and ref as one whose ack never succeeded must be handled, not
// swallowed with the old result (its sender would be told "delivered" and nothing typed).
func TestANewFrameWithAReusedSeqAfterAHubRestartIsHandled(t *testing.T) {
	hub := &fakeHub{failAck: 1000} // every ack fails: the first frame stays "seen"
	var mu sync.Mutex
	var got []string
	_, cancel, _ := rig(t, hub, func(d hubc.Deliver) Result {
		mu.Lock()
		got = append(got, d.Text)
		mu.Unlock()
		return Result{OK: true}
	})
	defer cancel()
	hub.push(1, "first", 9)
	waitFor(t, "the first frame handled", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 })
	hub.mu.Lock()
	hub.queue = nil // the hub restarted: the queue is gone and the numbering starts again
	hub.mu.Unlock()
	hub.push(1, "second, a different message with the same seq and ref", 9)
	waitFor(t, "the second frame handled", func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 2 })
}

// context: a refused credential on track must not hammer the hub every backoff interval for ever: the
// wait grows.
func TestTrackBacksOffWhileItKeepsFailing(t *testing.T) {
	hub := &fakeHub{authOK: "sbp1.never"}
	ts := httptest.NewServer(hub.handler())
	defer ts.Close()
	sp := New(Config{
		Client:  &hubc.Client{URL: ts.URL, Token: func() (string, error) { return "sbp1.bad", nil }},
		Machine: "hp", Name: "ccsbx", TTL: 3, Backoff: 20 * time.Millisecond, PollWait: time.Second,
		Entries: func() []map[string]any { return []map[string]any{{"key": "k"}} },
	})
	ctx, cancel := context.WithCancel(context.Background())
	go sp.Run(ctx)
	time.Sleep(600 * time.Millisecond)
	cancel()
	hub.mu.Lock()
	n := 0
	for _, tk := range hub.seenToken {
		if tk == "sbp1.bad" {
			n++
		}
	}
	hub.mu.Unlock()
	// track + poll both fail; with a fixed 20 ms wait this would be ~60 calls, with doubling about 10
	if n > 30 {
		t.Fatalf("%d calls in 600 ms: the track wait does not grow", n)
	}
}
