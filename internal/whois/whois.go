// Package whois asks Tailscale who is on the other end of a connection (M2a). It exists so the
// daemon's tailnet listener derives a caller's class from what the TAILNET says about the peer's
// address (login and device), never from anything the caller wrote: a header, a body field and a
// forwarded address are all ignored. Two ways of asking, the same JSON back:
//
//   - the local tailscaled's LocalAPI over its unix socket (`GET /localapi/v0/whois?addr=ip:port`),
//     which needs no process and no PATH (hp);
//   - `tailscale whois --json <addr>` (the Mac, where the CLI is the supported way in).
//
// Any failure is an error, and the caller treats an error as "unknown": 403.
package whois

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
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Identity is what Tailscale says about a peer address.
type Identity struct {
	Login  string   // the user's login name ("tagged-devices" for a tagged node)
	Device string   // the node's name, lower-cased, without the trailing dot: "hp-ubuntu.skunk-escalator.ts.net"
	Short  string   // its first label: "hp-ubuntu"
	Tags   []string // "tag:ccsbx", ... (a tagged node has no user)
}

// Resolver answers WhoIs for a remote address ("100.x.y.z:port").
type Resolver interface {
	WhoIs(ctx context.Context, remoteAddr string) (Identity, error)
}

// Parse reads the JSON of `tailscale whois --json` / LocalAPI's whois.
func Parse(b []byte) (Identity, error) {
	var r struct {
		Node struct {
			Name string
			Tags []string
		}
		UserProfile struct{ LoginName string }
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return Identity{}, fmt.Errorf("whois answer: %w", err)
	}
	name := strings.ToLower(strings.TrimSuffix(r.Node.Name, "."))
	if name == "" {
		return Identity{}, errors.New("whois: no node name")
	}
	short, _, _ := strings.Cut(name, ".")
	return Identity{Login: r.UserProfile.LoginName, Device: name, Short: short, Tags: r.Node.Tags}, nil
}

// LocalAPI asks the local tailscaled over its unix socket.
type LocalAPI struct {
	Socket string // default /var/run/tailscale/tailscaled.sock
}

func (l LocalAPI) WhoIs(ctx context.Context, addr string) (Identity, error) {
	sock := l.Socket
	if sock == "" {
		sock = "/var/run/tailscale/tailscaled.sock"
	}
	c := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", sock)
	}}}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://local-tailscaled.sock/localapi/v0/whois?addr="+url.QueryEscape(addr), nil)
	if err != nil {
		return Identity{}, err
	}
	resp, err := c.Do(req)
	if err != nil {
		return Identity{}, fmt.Errorf("tailscaled: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return Identity{}, fmt.Errorf("tailscaled whois: %d", resp.StatusCode)
	}
	return Parse(b)
}

// CLI runs `tailscale whois --json <addr>` (the Mac).
type CLI struct {
	Bin     string // default "tailscale"
	Command func(ctx context.Context, name string, args ...string) *exec.Cmd
}

func (c CLI) WhoIs(ctx context.Context, addr string) (Identity, error) {
	bin := c.Bin
	if bin == "" {
		bin = "tailscale"
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	mk := c.Command
	if mk == nil {
		mk = func(ctx context.Context, n string, a ...string) *exec.Cmd { return exec.CommandContext(ctx, n, a...) }
	}
	cmd := mk(ctx, bin, "whois", "--json", addr)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return Identity{}, fmt.Errorf("tailscale whois: %w", err)
	}
	return Parse(out.Bytes())
}

// Cache remembers an answer for a short time per peer IP (not per port: the node behind an address does
// not change between connections), so a burst of requests asks once. Errors are not cached.
type Cache struct {
	R   Resolver
	TTL time.Duration // default 30 s
	Now func() time.Time

	mu sync.Mutex
	m  map[string]cached
}

type cached struct {
	id Identity
	at time.Time
}

func (c *Cache) WhoIs(ctx context.Context, addr string) (Identity, error) {
	ttl := c.TTL
	if ttl == 0 {
		ttl = 30 * time.Second
	}
	now := time.Now
	if c.Now != nil {
		now = c.Now
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return Identity{}, fmt.Errorf("whois: bad address %q", addr)
	}
	c.mu.Lock()
	if v, ok := c.m[host]; ok && now().Sub(v.at) < ttl {
		c.mu.Unlock()
		return v.id, nil
	}
	c.mu.Unlock()
	id, err := c.R.WhoIs(ctx, addr)
	if err != nil {
		return Identity{}, err
	}
	c.mu.Lock()
	if c.m == nil {
		c.m = map[string]cached{}
	}
	c.m[host] = cached{id, now()}
	c.mu.Unlock()
	return id, nil
}
