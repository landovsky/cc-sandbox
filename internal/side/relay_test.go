package side

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hostd"
)

type relayRig struct {
	t         *testing.T
	out, in   string
	state     string
	mu        sync.Mutex
	forwarded []hostd.Request
	now       time.Time
}

func newRelayRig(t *testing.T) *relayRig {
	r := &relayRig{t: t, out: t.TempDir(), in: t.TempDir(), state: t.TempDir(), now: time.Unix(1_800_000_000, 0)}
	os.Mkdir(filepath.Join(r.out, "requests"), 0o700)
	os.Mkdir(filepath.Join(r.in, "replies"), 0o755)
	return r
}

func (r *relayRig) relay(mut func(*RelayConfig)) *Relay {
	cfg := RelayConfig{Out: r.out, In: r.in, State: r.state, Sid: "sAAAAAAAAA", Gen: 2, Label: "worker", FullID: "tomas/future-two/sAAAAAAAAA/g2",
		FenceUID: os.Getuid(), Now: func() time.Time { return r.now },
		Forward: func(_ context.Context, q hostd.Request) hostd.Reply {
			r.mu.Lock()
			r.forwarded = append(r.forwarded, q)
			r.mu.Unlock()
			return hostd.Reply{Status: "ok"}
		}}
	if mut != nil {
		mut(&cfg)
	}
	x, err := NewRelay(cfg)
	if err != nil {
		r.t.Fatal(err)
	}
	return x
}

func (r *relayRig) put(name, body string) {
	if err := os.WriteFile(filepath.Join(r.out, "requests", name), []byte(body), 0o600); err != nil {
		r.t.Fatal(err)
	}
}

func (r *relayRig) replyOf(rid string) hostd.Reply {
	b, err := os.ReadFile(filepath.Join(r.in, "replies", rid+".json"))
	if err != nil {
		r.t.Fatalf("no reply for %s: %v", rid, err)
	}
	var rep hostd.Reply
	json.Unmarshal(b, &rep)
	return rep
}

// context "the fence is untrusted and writes the request files": identity (sid, generation, label) and the
// id come from the sidecar, whatever the file claims, and the reply lands where the fence reads it.
func TestRelayStampsIdentityFromTheSidecarNotTheFile(t *testing.T) {
	r := newRelayRig(t)
	r.put("r1.json", `{"type":"msg.send","sid":"sEVIL","gen":9,"id":"01FORGED","to":"x"}`)
	st := r.relay(nil).Scan(context.Background())
	if st.Accepted != 1 || len(r.forwarded) != 1 {
		t.Fatalf("stats %+v, forwarded %d", st, len(r.forwarded))
	}
	q := r.forwarded[0]
	if q.Sid != "sAAAAAAAAA" || q.Gen != 2 || q.Label != "worker" || q.ID == "01FORGED" || q.ID == "" || q.Type != "msg.send" {
		t.Fatalf("stamped %+v", q)
	}
	if rep := r.replyOf("r1"); rep.Status != "ok" || rep.ID != q.ID {
		t.Fatalf("reply %+v", rep)
	}
}

// context "SEC-7/SEC-8: the fence swaps names for symlinks, FIFOs, hardlinks and sparse files": each is
// refused unread with a code that carries no file bytes, and nothing is forwarded.
func TestRelayRefusesWhatTheOutboxChecksRefuse(t *testing.T) {
	r := newRelayRig(t)
	req := filepath.Join(r.out, "requests")
	secret := filepath.Join(t.TempDir(), "secret.json")
	os.WriteFile(secret, []byte(`{"type":"msg.send"}`), 0o600)
	os.Symlink(secret, filepath.Join(req, "sym.json"))
	syscall.Mkfifo(filepath.Join(req, "fifo.json"), 0o600)
	r.put("hl.json", `{"type":"msg.send"}`)
	os.Link(filepath.Join(req, "hl.json"), filepath.Join(req, "hl2.json"))
	big, _ := os.Create(filepath.Join(req, "big.json"))
	big.Truncate(1 << 30) // sparse
	big.Close()
	r.put("dup.json", `{"type":"msg.send","type":"git.push"}`)
	r.put("unk.json", `{"type":"rm.rf"}`)
	r.put("junk.json", `not json`)
	st := r.relay(nil).Scan(context.Background())
	if len(r.forwarded) != 0 || st.Accepted != 0 {
		t.Fatalf("forwarded %+v (stats %+v)", r.forwarded, st)
	}
	want := map[string]string{"sym": "unsafe_path", "fifo": "not_regular", "hl": "hardlink", "hl2": "hardlink", "big": "too_big",
		"dup": "bad_json", "unk": "unknown_type", "junk": "bad_json"}
	for rid, code := range want {
		rep := r.replyOf(rid)
		got, _ := rep.Result.(map[string]any)
		if rep.Status != "refused" || got["code"] != code {
			t.Errorf("%s: %+v, want refused/%s", rid, rep, code)
		}
	}
}

// context "files owned by someone other than the fence uid": the fence's own uid is the only author a
// request may have, so a file planted by another uid is refused.
func TestRelayRefusesAFileThatIsNotTheFencesOwn(t *testing.T) {
	r := newRelayRig(t)
	r.put("o.json", `{"type":"msg.send"}`)
	r.relay(func(c *RelayConfig) { c.FenceUID = os.Getuid() + 1 }).Scan(context.Background())
	if len(r.forwarded) != 0 {
		t.Fatal("forwarded a file of the wrong owner")
	}
	if rep := r.replyOf("o"); rep.Status != "refused" {
		t.Fatalf("%+v", rep)
	}
}

// context "the sidecar restarts mid-session": a request already accepted must not run twice, because the
// sidecar cannot delete files from the read-only outbox.
func TestRelayDoesNotReplayAfterARestart(t *testing.T) {
	r := newRelayRig(t)
	r.put("a.json", `{"type":"msg.send"}`)
	r.relay(nil).Scan(context.Background())
	r.put("b.json", `{"type":"msg.send"}`)
	r.relay(nil).Scan(context.Background()) // a fresh Relay on the same state dir
	if len(r.forwarded) != 2 {
		t.Fatalf("forwarded %d, want 2 (a once, b once)", len(r.forwarded))
	}
}

// context "a compromised fence floods the outbox": the bucket caps what is forwarded, and the rest is shed
// rather than queued.
func TestRelayShedsAFlood(t *testing.T) {
	r := newRelayRig(t)
	for i := 0; i < 100; i++ {
		r.put(strings.Repeat("f", 1)+string(rune('a'+i%26))+string(rune('a'+i/26))+".json", `{"type":"msg.send"}`)
	}
	st := r.relay(func(c *RelayConfig) { c.Burst = 5; c.Rate = 1 }).Scan(context.Background())
	if st.Accepted != 5 || len(r.forwarded) != 5 || st.Shed == 0 {
		t.Fatalf("stats %+v, forwarded %d", st, len(r.forwarded))
	}
}

// context "the relay must be told who it speaks for": without a sid and generation it would forward
// requests with an empty identity.
func TestRelayNeedsAnIdentityAndAnUpstream(t *testing.T) {
	r := newRelayRig(t)
	for name, mut := range map[string]func(*RelayConfig){
		"no sid":      func(c *RelayConfig) { c.Sid = "" },
		"no upstream": func(c *RelayConfig) { c.Forward = nil },
		"relative in": func(c *RelayConfig) { c.In = "in" },
	} {
		cfg := RelayConfig{Out: r.out, In: r.in, State: r.state, Sid: "s", Gen: 1, Forward: func(context.Context, hostd.Request) hostd.Reply { return hostd.Reply{} }}
		mut(&cfg)
		if _, err := NewRelay(cfg); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// context "the loop scans every few seconds": a request that was forwarded is not forwarded again by the
// next scan of the same process.
func TestRelayScansTwiceForwardOnce(t *testing.T) {
	r := newRelayRig(t)
	r.put("a.json", `{"type":"msg.send"}`)
	x := r.relay(nil)
	x.Scan(context.Background())
	x.Scan(context.Background())
	if len(r.forwarded) != 1 {
		t.Fatalf("forwarded %d times", len(r.forwarded))
	}
}

// context "a fence that writes requests for the whole lifetime of the generation": past the lifetime cap
// the rest are shed unread, so one generation cannot grow the daemon's work without bound.
func TestRelayLifetimeCap(t *testing.T) {
	r := newRelayRig(t)
	for _, n := range []string{"a", "b", "c", "d", "e"} {
		r.put(n+".json", `{"type":"msg.send"}`)
	}
	st := r.relay(func(c *RelayConfig) { c.MaxTotal = 3 }).Scan(context.Background())
	if st.Accepted != 3 || st.Shed != 2 {
		t.Fatalf("stats %+v", st)
	}
}
