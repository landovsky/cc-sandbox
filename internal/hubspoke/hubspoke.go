// Package hubspoke is hostd's presence on the session bus (B1, plan §3): it tracks the daemon's
// fenced sessions on the hub under a lease and long-polls the daemon's downlink topic
// (spoke:<machine>/ccsbx), handing every "deliver" frame to a handler and acking the answer. It
// exists as its own package so the loop (lease renewal, at-least-once with dedupe, backoff, a
// credential re-read after a 401) is tested against a fake hub, and so cmd/ccsbx-hostd only wires it.
//
// There is no cursor on disk: the hub drops what is acked, so a poll with after=0 returns exactly
// the frames not yet acked, and a hub restart (whose sequence numbers start again) loses nothing
// that was acked and re-sends what was not. A frame whose ack failed is therefore delivered again;
// it is handled once per (seq, ack_ref) and only re-acked.
package hubspoke

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hubc"
)

// Result is what the handler says about a frame: OK "handled" (delivered, or accepted into a queue
// that will deliver), Confirmed "seen to arrive in full".
type Result struct{ OK, Confirmed bool }

// seenKey identifies one delivery: the mailbox sequence, the hub's ack reference and a hash of the
// payload. A hub or mailbox restart starts the sequence again and its refs are only unique within a
// VM's life, so (seq, ref) alone could match a NEW frame to an old entry and swallow it; the payload
// hash makes that collision a different message.
type seenKey struct {
	Seq, Ref int64
	Hash     [32]byte
}

type seenVal struct {
	Res Result
	At  time.Time
}

// seenTTL: an entry whose ack never succeeded is forgotten after this, by time and not only by distance.
const seenTTL = 10 * time.Minute

// Config is one spoke.
type Config struct {
	Client  *hubc.Client
	Machine string
	// Name is the downlink's second segment: the topic is spoke:<Machine>/<Name>.
	Name string
	// TTL is the lease in seconds (the hub allows 1..300); the set is re-sent every TTL/3.
	TTL int
	// Entries returns the entries to track now (the daemon's live fenced sessions). Called on every renewal.
	Entries func() []map[string]any
	// OnDeliver handles one deliver frame. d is the decoded payload.
	OnDeliver func(ctx context.Context, f hubc.Frame, d hubc.Deliver) Result
	// PollWait is the long-poll length (default 25 s, the hub caps it at 30).
	PollWait time.Duration
	Logf     func(format string, args ...any)
	// Backoff is the wait after a failed call (default 2 s growing to 30 s). Tests shorten it.
	Backoff time.Duration
}

// Topic is the downlink this spoke polls.
func (c *Config) Topic() string { return "spoke:" + c.Machine + "/" + c.Name }

// Status is a snapshot for logs and tests.
type Status struct {
	Tracked  int    // entries accepted by the last successful track
	Frames   int    // deliver frames handled
	LastErr  string // the last failure, "" when the last call worked
	Renewals int
}

// Spoke runs the loops.
type Spoke struct {
	cfg  Config
	mu   sync.Mutex
	st   Status
	seen map[seenKey]seenVal // frames handled but possibly not yet acked, with the result to re-ack
}

// New makes a spoke; call Run.
func New(cfg Config) *Spoke {
	if cfg.TTL <= 0 {
		cfg.TTL = 60
	}
	if cfg.PollWait <= 0 {
		cfg.PollWait = 25 * time.Second
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	if cfg.Backoff <= 0 {
		cfg.Backoff = 2 * time.Second
	}
	return &Spoke{cfg: cfg, seen: map[seenKey]seenVal{}}
}

// Status returns a copy of the counters.
func (s *Spoke) Status() Status { s.mu.Lock(); defer s.mu.Unlock(); return s.st }

func (s *Spoke) setErr(err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err == nil {
		s.st.LastErr = ""
		return
	}
	msg := err.Error()
	if msg != s.st.LastErr {
		s.cfg.Logf("hub: %v", err)
	}
	s.st.LastErr = msg
}

// Run blocks until ctx is cancelled. On the way out it untracks (an empty set) so the sessions
// leave the registry at once instead of when the lease ends.
func (s *Spoke) Run(ctx context.Context) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.trackLoop(ctx) }()
	go func() { defer wg.Done(); s.pollLoop(ctx) }()
	wg.Wait()
	uctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, _ = s.cfg.Client.Track(uctx, nil, 1)
}

func (s *Spoke) trackLoop(ctx context.Context) {
	every := time.Duration(s.cfg.TTL) * time.Second / 3
	back := s.cfg.Backoff
	for {
		keys, err := s.cfg.Client.Track(ctx, s.cfg.Entries(), s.cfg.TTL)
		s.setErr(err)
		wait := every
		if err == nil {
			s.mu.Lock()
			s.st.Tracked, s.st.Renewals = len(keys), s.st.Renewals+1
			s.mu.Unlock()
			back = s.cfg.Backoff
		} else {
			wait = back // grows while it keeps failing: a refused credential must not hit the hub every 2 s
			if back < 30*time.Second {
				back *= 2
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

func (s *Spoke) pollLoop(ctx context.Context) {
	back := s.cfg.Backoff
	for ctx.Err() == nil {
		frames, _, err := s.cfg.Client.Poll(ctx, s.cfg.Topic(), 0, s.cfg.PollWait)
		if err != nil {
			s.setErr(err)
			select {
			case <-ctx.Done():
				return
			case <-time.After(back):
			}
			if back < 30*time.Second {
				back *= 2
			}
			continue
		}
		back = s.cfg.Backoff
		s.setErr(nil)
		for _, f := range frames {
			s.handle(ctx, f)
		}
	}
}

func (s *Spoke) handle(ctx context.Context, f hubc.Frame) {
	var d hubc.Deliver
	if f.Event == "deliver" {
		if err := json.Unmarshal(f.Payload, &d); err != nil {
			s.cfg.Logf("hub: frame %d: unreadable deliver payload: %v", f.Seq, err)
		}
	}
	key := seenKey{Seq: f.Seq, Ref: d.AckRef, Hash: sha256.Sum256(f.Payload)}
	s.mu.Lock()
	for k, v := range s.seen {
		if time.Since(v.At) > seenTTL {
			delete(s.seen, k)
		}
	}
	prev, done := s.seen[key]
	res := prev.Res
	s.mu.Unlock()
	if !done {
		if f.Event == "deliver" && s.cfg.OnDeliver != nil {
			res = s.cfg.OnDeliver(ctx, f, d)
		}
		s.mu.Lock()
		s.seen[key] = seenVal{Res: res, At: time.Now()}
		s.st.Frames++
		s.mu.Unlock()
	}
	err := s.cfg.Client.Ack(ctx, hubc.Ack{Topic: s.cfg.Topic(), Seq: f.Seq, AckTopic: d.AckTopic, AckRef: d.AckRef, OK: res.OK, Confirmed: res.Confirmed})
	if err != nil {
		s.setErr(fmt.Errorf("ack of frame %d: %w", f.Seq, err))
		return // the frame comes again; it will not be handled twice
	}
	s.mu.Lock()
	delete(s.seen, key)
	s.mu.Unlock()
}
