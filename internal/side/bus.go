// bus.go is the pod's side of messages to another machine (plan F3, "bus relay"): the Forward the outbox
// relay calls for a fence's msg.send. It exists so a fenced pod can reach its manager on another machine the
// same way a docker fence does through hostd, with the same rules, and without the sidecar ever holding more
// than the one relay credential for its own generation.
//
// What it does, in order, for one msg.send:
//   - scope: the target node must be the manager or a peer of the session's own record (Config.Scope, written
//     by the launcher from the owner's start spec, never from the fence); anything else is refused "scope";
//   - credential: the relay token and generation key come from the one `side load`; without them the answer is
//     error "relay_unavailable";
//   - target: internal/hubsend.Resolve picks the ONE hub entry the record places the node on (a squatter on
//     another machine is neither sent to nor allowed to make the real one ambiguous);
//   - send: internal/hubsend.Send stamps (generation key) and posts with the relay token as bearer.
//
// A 401 means the credential expired or was revoked. The pod cannot mint, so there is NO retry: the fence gets
// error "relay_auth" and a journal fact says so (the launcher re-mints and replaces the pod). A 403 is the hub
// refusing THIS message and is never retried. The message text is never logged and never journalled.
package side

import (
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/hubc"
	"github.com/landovsky/cc-sandbox/internal/hubsend"
	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/msg"
)

// BusConfig is what the sender needs besides the credential.
type BusConfig struct {
	HubURL  string // the hub as the pod reaches it (the operator egress Service to hp:4011)
	Machine string // this pod's machine as the hub knows it
	Sid     string // identity comes from here, never from a request
	Label   string
	Scope   msg.Scope // who this session may address, from the record
	// Cred returns the relay credential once loaded (Server.Relay).
	Cred func() (RelayCred, bool)
	// Presence lists the hub's sessions with the relay token. Default: hubc's GET /sessions. Injectable
	// because the hub must allow a relay to read the entries it may reach (see docs: hub change pending).
	Presence func(ctx context.Context, hubURL, token string) ([]map[string]any, error)
	Timeout  time.Duration // one send (default 20s: the hub waits up to ~8s for the receiver's ack)
	Journal  Appender
	MaxFacts int // journal facts about failed sends per lifetime (default 50)
	// PairMax messages per PairWindow to one node (hostd's router allows 6 a minute): the outbox relay's own
	// bucket is about 5 a second, which would otherwise all land in the manager's pane.
	PairMax    int
	PairWindow time.Duration
	Now        func() time.Time
	Logf       func(string, ...any)
}

// Bus is the Forward for msg.send.
type Bus struct {
	cfg   BusConfig
	mu    sync.Mutex
	facts int
	pairs map[string][]time.Time
	fail  appendFailures
}

// NewBus checks the configuration.
func NewBus(cfg BusConfig) (*Bus, error) {
	if cfg.HubURL == "" || cfg.Machine == "" || cfg.Sid == "" || cfg.Cred == nil {
		return nil, errors.New("side bus: hub url, machine, sid and a credential source are required")
	}
	if cfg.Presence == nil {
		cfg.Presence = func(ctx context.Context, hubURL, token string) ([]map[string]any, error) {
			c := &hubc.Client{URL: hubURL, Token: func() (string, error) { return token, nil }}
			return c.Sessions(ctx)
		}
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 20 * time.Second
	}
	if cfg.PairMax == 0 {
		cfg.PairMax = 6
	}
	if cfg.PairWindow == 0 {
		cfg.PairWindow = time.Minute
	}
	if cfg.MaxFacts == 0 {
		cfg.MaxFacts = 50
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	return &Bus{cfg: cfg, pairs: map[string][]time.Time{}}, nil
}

func busReply(status, code string) hostd.Reply {
	return hostd.Reply{Status: status, Result: map[string]string{"code": code}}
}

// pairOK is the sliding window of hostd's router: at most PairMax messages per PairWindow to one node.
func (b *Bus) pairOK(node string) bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.cfg.Now()
	var keep []time.Time
	for _, t := range b.pairs[node] {
		if now.Sub(t) < b.cfg.PairWindow {
			keep = append(keep, t)
		}
	}
	if len(keep) >= b.cfg.PairMax {
		b.pairs[node] = keep
		return false
	}
	b.pairs[node] = append(keep, now)
	return true
}

// fact journals why a send failed, bounded per lifetime. The node is one the scope already named, so it is
// the record's form of it (normalised), not the fence's spelling.
func (b *Bus) fact(typ, node, why string) {
	if b.cfg.Journal == nil {
		return
	}
	b.mu.Lock()
	b.facts++
	over := b.facts > b.cfg.MaxFacts
	b.mu.Unlock()
	if over {
		return
	}
	d, _ := json.Marshal(map[string]string{"node": node, "why": why})
	if _, err := b.cfg.Journal.Append(journal.Event{Sid: b.cfg.Sid, Src: "host", Type: typ, Data: d}); err != nil {
		b.fail.log(b.cfg.Logf, "side bus", err)
	}
}

func norm(n string) string { return strings.Replace(n, ":#", ":", 1) }

func (b *Bus) inScope(node string) bool {
	n := norm(node)
	if n == "" {
		return false
	}
	if n == norm(b.cfg.Scope.Manager.Node) {
		return true
	}
	for _, p := range b.cfg.Scope.Peers {
		if n == norm(p) {
			return true
		}
	}
	return false
}

// Forward handles one request from the outbox relay. Only msg.send goes over the bus; anything else is
// refused here (other types have their own handlers and are composed by the caller).
func (b *Bus) Forward(ctx context.Context, q hostd.Request) hostd.Reply {
	if q.Type != "msg.send" {
		return busReply("refused", "unsupported_in_pod")
	}
	var body struct {
		To struct {
			Node  string `json:"node"`
			BusID string `json:"bus_id"`
		} `json:"to"`
		Kind string `json:"kind"`
		Text string `json:"text"`
	}
	if json.Unmarshal(q.Body, &body) != nil || strings.TrimSpace(body.Text) == "" || body.To.Node == "" || body.To.BusID != "" {
		return busReply("refused", "bad_request")
	}
	node := norm(body.To.Node)
	if !b.inScope(node) {
		return busReply("refused", "scope")
	}
	cred, ok := b.cfg.Cred()
	if !ok || cred.Key == nil {
		b.fact("bus.unavailable", node, "no relay credential was loaded")
		return busReply("error", "relay_unavailable")
	}
	if !b.pairOK(node) { // before presence is read: a flood costs the hub nothing
		b.fact("bus.rate_capped", node, "too many messages to this node in the window")
		return busReply("refused", "rate_capped")
	}
	ctx, cancel := context.WithTimeout(ctx, b.cfg.Timeout)
	defer cancel()
	entries, err := b.cfg.Presence(ctx, b.cfg.HubURL, cred.Token)
	if err != nil {
		if hubc.IsForbidden(err) { // not a refusal of this message: the hub does not let a relay read presence
			b.fact("bus.unavailable", node, "the hub does not let this relay read the sessions it may reach")
			return busReply("error", "presence_forbidden")
		}
		return b.failed(err, node)
	}
	to, err := hubsend.Resolve(entries, node, b.cfg.Scope.MachineOf(node), b.cfg.Machine)
	switch {
	case err == nil:
	case errors.Is(err, msg.ErrAmbiguousNode):
		b.fact("bus.dropped", node, "more than one session holds that node")
		return busReply("error", "dropped")
	case errors.Is(err, msg.ErrWrongMachine):
		b.fact("bus.dropped", node, "no session of that node on the machine the scope binds it to")
		return busReply("error", "dropped")
	case errors.Is(err, msg.ErrUnknownNode):
		b.fact("bus.dropped", node, "no session on the hub holds that node")
		return busReply("error", "dropped")
	case errors.Is(err, msg.ErrLocalNode):
		// a node on this machine is the local bus's, and a pod has none
		return busReply("error", "no_live_session")
	default:
		return busReply("error", "undeliverable")
	}
	priv := append(ed25519.PrivateKey(nil), cred.Key.Private...) // this send's own copy, wiped after
	delivered, err := hubsend.Send(ctx, b.cfg.HubURL, cred.Token, priv, hubsend.From{Sid: b.cfg.Sid, Label: b.cfg.Label, Machine: b.cfg.Machine},
		to, body.Kind, body.Text, b.cfg.Now())
	for i := range priv {
		priv[i] = 0
	}
	if err != nil {
		return b.failed(err, node)
	}
	if !delivered {
		b.fact("bus.dropped", node, "the hub delivered it to nobody")
		return busReply("error", "dropped")
	}
	return hostd.Reply{Status: "ok", Result: map[string]string{"to": "hub:" + node}}
}

// failed maps a hub error the way hostd's router does, except that a 401 is final here.
func (b *Bus) failed(err error, node string) hostd.Reply {
	switch {
	case hubc.IsUnauthorized(err):
		b.fact("bus.relay_auth", node, "the hub no longer accepts this generation's relay credential")
		return busReply("error", "relay_auth")
	case hubc.IsForbidden(err):
		// a refusal of THIS message (scope, stamp, sender): another credential would not change it
		b.fact("bus.dropped", node, "the hub refused the message")
		return busReply("error", "dropped")
	default:
		return busReply("error", "undeliverable")
	}
}
