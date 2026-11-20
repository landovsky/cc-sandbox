// Package hubc is a client of the session-bus hub's daemon HTTP surface (B11, plan §3), in plain
// net/http. It exists so that hostd (and a pod's sidecar) can be bus clients without a websocket
// library and without ever touching the owner's bus sockets (decision (1) of 20 Nov: a daemon
// reaches the bus only through its spoke over HTTP :4011):
//
//	POST /v1/track     replace the whole set of tracked entries, under a lease (ttl seconds)
//	GET  /v1/downlink  long-poll one downlink topic (spoke:<machine>/<name>); at-least-once
//	POST /v1/ack       drop frames up to seq and relay the daemon's result to the waiting /deliver
//	POST /deliver      send a message (the sender's stamp, B3, travels in `sig`)
//
// Every call carries `Authorization: Bearer <token>` (a spoke principal for this machine). The token
// is held in memory only; callers hand it over through a Source so it can be re-read after a 401.
package hubc

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client talks to one hub.
type Client struct {
	// URL is the hub's base URL, e.g. http://127.0.0.1:4011 (no trailing path).
	URL string
	// Token returns the bearer credential; called for every request, so a rotated file is picked up.
	Token func() (string, error)
	// HTTP is the transport; nil means a client with no overall timeout (long polls) but with
	// connection and header timeouts.
	HTTP *http.Client
}

// Frame is one downlink message: Seq is the mailbox sequence, Event the broadcast event name
// ("deliver" today), Payload its body as the hub built it.
type Frame struct {
	Seq     int64           `json:"seq"`
	Event   string          `json:"event"`
	Payload json.RawMessage `json:"payload"`
}

// Deliver is the payload of a "deliver" frame. Fields beyond text/ack_* exist only when the hub
// forwards them (M2b); an old hub leaves them empty.
type Deliver struct {
	Pane     *string `json:"pane"`
	UID      string  `json:"uid"`
	Key      string  `json:"key"`
	Text     string  `json:"text"`
	AckTopic string  `json:"ack_topic"`
	AckRef   int64   `json:"ack_ref"`
	// M2b: what the receiver needs to verify the sender and re-check scope itself.
	From  map[string]string `json:"from,omitempty"`
	To    string            `json:"to,omitempty"`
	Kind  string            `json:"kind,omitempty"`
	Sig   string            `json:"sig,omitempty"`
	Trust string            `json:"trust,omitempty"`
	Pub   string            `json:"pub,omitempty"`
	Body  string            `json:"body,omitempty"`
}

// Error is a non-2xx answer from the hub.
type Error struct {
	Status int
	Body   string
}

func (e *Error) Error() string { return fmt.Sprintf("hub answered %d: %s", e.Status, e.Body) }

// IsAuth reports a 401/403: the credential is missing, wrong, or not allowed this.
func IsAuth(err error) bool {
	var e *Error
	return errors.As(err, &e) && (e.Status == 401 || e.Status == 403)
}

// sharedTransport is used by every Client (the spoke's and each relay's): one pool, so connections
// are reused instead of a new transport, idle connection and goroutines per request, and a dial or
// TLS handshake to a black-holed hub fails in 5 s instead of the kernel's ~2 minutes (longer than
// the 60 s lease, which would drop every tracked session).
var sharedTransport = &http.Transport{
	DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	TLSHandshakeTimeout:   5 * time.Second,
	MaxIdleConns:          8,
	MaxIdleConnsPerHost:   4,
	IdleConnTimeout:       90 * time.Second,
	ResponseHeaderTimeout: 45 * time.Second,
}

var sharedHTTP = &http.Client{Transport: sharedTransport}

func (c *Client) http() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return sharedHTTP
}

// Per-call deadlines: a call must not outlive what it is for (a stuck hub must not stall a loop).
var (
	callTimeout = 10 * time.Second // track, ack, sessions
	sendTimeout = 15 * time.Second // deliver: the hub itself waits up to ~8 s for the receiver's ack
)

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body any, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, err
		}
		rd = bytes.NewReader(b)
	}
	u := strings.TrimRight(c.URL, "/") + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != nil {
		tok, err := c.Token()
		if err != nil {
			return nil, fmt.Errorf("hub credential: %w", err)
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	}
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode/100 != 2 {
		return nil, &Error{Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	return b, nil
}

// Track replaces this daemon's whole tracked set with entries and renews the lease. Entries not
// listed are untracked; if Track is not called again within ttl seconds they leave the registry.
// It returns the keys the hub accepted.
func (c *Client) Track(ctx context.Context, entries []map[string]any, ttl int) ([]string, error) {
	if entries == nil {
		entries = []map[string]any{}
	}
	b, err := c.do(ctx, "POST", "/v1/track", nil, map[string]any{"entries": entries, "ttl": ttl}, callTimeout)
	if err != nil {
		return nil, err
	}
	var r struct {
		Tracked []string `json:"tracked"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, fmt.Errorf("hub's track answer: %w", err)
	}
	return r.Tracked, nil
}

// Poll long-polls topic for frames with seq > after, waiting up to wait (the hub caps it at 30 s).
// It returns the frames and the hub's `next` marker.
func (c *Client) Poll(ctx context.Context, topic string, after int64, wait time.Duration) ([]Frame, int64, error) {
	q := url.Values{"topic": {topic}, "after": {strconv.FormatInt(after, 10)}, "wait": {strconv.Itoa(int(wait / time.Second))}}
	b, err := c.do(ctx, "GET", "/v1/downlink", q, nil, wait+callTimeout)
	if err != nil {
		return nil, after, err
	}
	var r struct {
		Messages []Frame `json:"messages"`
		Next     int64   `json:"next"`
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return nil, after, fmt.Errorf("hub's downlink answer: %w", err)
	}
	return r.Messages, r.Next, nil
}

// Ack is what a daemon says about one delivery: OK is "the spoke handled it", Confirmed "it was
// seen to arrive in full", Via how ("http" here). AckTopic/AckRef come from the frame's payload.
type Ack struct {
	Topic     string `json:"topic"`
	Seq       int64  `json:"seq"`
	AckTopic  string `json:"ack_topic,omitempty"`
	AckRef    int64  `json:"ack_ref,omitempty"`
	OK        bool   `json:"ok"`
	Confirmed bool   `json:"confirmed"`
	Via       string `json:"via,omitempty"`
}

// Ack drops every frame up to a.Seq from the mailbox and relays the result to the waiting /deliver.
func (c *Client) Ack(ctx context.Context, a Ack) error {
	if a.Via == "" {
		a.Via = "http"
	}
	_, err := c.do(ctx, "POST", "/v1/ack", nil, a, callTimeout)
	return err
}

// SendReq is a /deliver request: To is a bus id or label, From the structured sender (B2), Kind
// what the message is, Sig the stamp (B3).
type SendReq struct {
	To   string            `json:"to"`
	Text string            `json:"text"`
	From map[string]string `json:"from,omitempty"`
	Kind string            `json:"kind,omitempty"`
	Sig  string            `json:"sig,omitempty"`
}

// SendResult is the hub's answer to /deliver.
type SendResult struct {
	Trust       string `json:"trust"`
	Delivered   int    `json:"delivered"`
	Unconfirmed int    `json:"unconfirmed"`
	Matched     int    `json:"matched"`
	Dispatched  int    `json:"dispatched"`
	Failed      int    `json:"failed"`
}

// Send posts a message and waits for the hub's answer (the hub waits for the receiver's ack, up to
// about 8 s, so the context should allow ~10 s).
func (c *Client) Send(ctx context.Context, r SendReq) (*SendResult, error) {
	b, err := c.do(ctx, "POST", "/deliver", nil, r, sendTimeout)
	if err != nil {
		return nil, err
	}
	var out SendResult
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("hub's deliver answer: %w", err)
	}
	return &out, nil
}

// Sessions reads the hub's presence list (GET /sessions): every tracked session as the hub knows it.
func (c *Client) Sessions(ctx context.Context) ([]map[string]any, error) {
	b, err := c.do(ctx, "GET", "/sessions", nil, nil, callTimeout)
	if err != nil {
		return nil, err
	}
	var out []map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		return nil, fmt.Errorf("hub's sessions answer: %w", err)
	}
	return out, nil
}

// FindNode resolves a tree address to the hub's entry for it, the way ccmsg does: an entry whose
// `node` equals the address, with "tree:#id" and "tree:id" meaning the same. When several match
// (a roll in progress) the most recently updated wins. nil: nobody on the hub has that node.
func FindNode(entries []map[string]any, node string) map[string]any {
	want := strings.Replace(node, ":#", ":", 1)
	var best map[string]any
	var bestAt float64 = -1
	for _, e := range entries {
		n, _ := e["node"].(string)
		if n == "" || (n != node && n != want) {
			continue
		}
		at, _ := e["updated_at"].(float64)
		if best == nil || at > bestAt {
			best, bestAt = e, at
		}
	}
	return best
}

// FindSid is the entry the hub holds for a session id (its `uid`), or nil.
func FindSid(entries []map[string]any, sid string) map[string]any {
	for _, e := range entries {
		if u, _ := e["uid"].(string); u == sid {
			return e
		}
	}
	return nil
}
