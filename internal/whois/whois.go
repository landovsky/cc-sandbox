// Package whois asks Tailscale who is on the other end of a connection (M2a). It exists so the
// daemon's tailnet listener derives a caller's class from what the TAILNET says about the peer's
// address (login and device), never from anything the caller wrote: a header, a body field and a
// forwarded address are all ignored. Two ways of asking, the same JSON back:
//
//   - the local tailscaled's LocalAPI over its unix socket (`GET /localapi/v0/whois?addr=ip:port`),
//     which needs no process and no PATH (hp);
//   - `tailscale whois --json <addr>` (the Mac, where the CLI is the supported way in), by an ABSOLUTE
//     binary path checked once at startup (CheckBinary): never looked up on PATH per call.
//
// Any failure is an error, and the caller treats an error as "unknown": 403. An answer is believed only
// for the asked address (it must be one of the node's own addresses) and never for a subnet router.
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
	"net/netip"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Identity is what Tailscale says about a peer address.
type Identity struct {
	Login  string       // the user's login name ("tagged-devices" for a tagged node)
	Device string       // the node's name, lower-cased, without the trailing dot: "hp-ubuntu.skunk-escalator.ts.net"
	Short  string       // its first label: "hp-ubuntu"
	Tags   []string     // "tag:ccsbx", ... (a tagged node has no user)
	Addrs  []netip.Addr // the node's own tailnet addresses (the asked address must be one of them)
	Router bool         // the node advertises subnet routes (PrimaryRoutes): what is behind it shares its address
}

// Resolver answers WhoIs for a remote address ("100.x.y.z:port").
type Resolver interface {
	WhoIs(ctx context.Context, remoteAddr string) (Identity, error)
}

// Parse reads the JSON of `tailscale whois --json` / LocalAPI's whois.
func Parse(b []byte) (Identity, error) {
	var r struct {
		Node struct {
			Name          string
			Tags          []string
			Addresses     []string
			PrimaryRoutes []string
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
	id := Identity{Login: r.UserProfile.LoginName, Device: name, Short: short, Tags: r.Node.Tags, Router: len(r.Node.PrimaryRoutes) > 0}
	for _, a := range r.Node.Addresses {
		if pf, err := netip.ParsePrefix(a); err == nil {
			id.Addrs = append(id.Addrs, pf.Addr().Unmap())
		} else if ip, err := netip.ParseAddr(a); err == nil {
			id.Addrs = append(id.Addrs, ip.Unmap())
		}
	}
	return id, nil
}

// Owns reports whether ip is one of the node's own addresses: tailscaled answers for an address inside a
// subnet route with the ROUTER's identity, which must never become the identity of what is behind it.
func (id Identity) Owns(ip netip.Addr) bool {
	for _, a := range id.Addrs {
		if a == ip.Unmap() {
			return true
		}
	}
	return false
}

const defaultSock = "/var/run/tailscale/tailscaled.sock"

// LocalAPI asks the local tailscaled over its unix socket. Use a pointer: one http client (and so no
// pile of idle connections) per LocalAPI, built on first use and closing its connection after each call.
type LocalAPI struct {
	Socket string // default /var/run/tailscale/tailscaled.sock

	once sync.Once
	c    *http.Client
}

func (l *LocalAPI) client() *http.Client {
	l.once.Do(func() {
		sock := l.Socket
		if sock == "" {
			sock = defaultSock
		}
		l.c = &http.Client{Transport: &http.Transport{
			DisableKeepAlives: true, // nothing is left idle: each call closes its unix connection
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", sock)
			}}}
	})
	return l.c
}

func (l *LocalAPI) get(ctx context.Context, path string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, "GET", "http://local-tailscaled.sock"+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := l.client().Do(req)
	if err != nil {
		return nil, fmt.Errorf("tailscaled: %w", err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("tailscaled %s: %d", path, resp.StatusCode)
	}
	return b, nil
}

func (l *LocalAPI) WhoIs(ctx context.Context, addr string) (Identity, error) {
	b, err := l.get(ctx, "/localapi/v0/whois?addr="+url.QueryEscape(addr))
	if err != nil {
		return Identity{}, err
	}
	return Parse(b)
}

// SelfIPs are this node's own tailnet addresses (tailscaled's status, Self.TailscaleIPs).
func (l *LocalAPI) SelfIPs(ctx context.Context) ([]netip.Addr, error) {
	b, err := l.get(ctx, "/localapi/v0/status?peers=false")
	if err != nil {
		return nil, err
	}
	var st struct {
		Self struct{ TailscaleIPs []string }
	}
	if err := json.Unmarshal(b, &st); err != nil {
		return nil, fmt.Errorf("tailscaled status: %w", err)
	}
	return parseIPs(st.Self.TailscaleIPs)
}

func parseIPs(l []string) ([]netip.Addr, error) {
	var out []netip.Addr
	for _, x := range l {
		ip, err := netip.ParseAddr(strings.TrimSpace(x))
		if err != nil {
			return nil, err
		}
		out = append(out, ip.Unmap())
	}
	if len(out) == 0 {
		return nil, errors.New("no tailnet address reported")
	}
	return out, nil
}

// CLI runs `tailscale whois --json <addr>` (the Mac). Bin must be an absolute path (see CheckBinary).
type CLI struct {
	Bin     string
	Command func(ctx context.Context, name string, args ...string) *exec.Cmd
}

// capWriter keeps at most n bytes and fails the write beyond that (a runaway binary's output is an error).
type capWriter struct {
	b bytes.Buffer
	n int
}

func (w *capWriter) Write(p []byte) (int, error) {
	if w.b.Len()+len(p) > w.n {
		return 0, errors.New("output over the limit")
	}
	return w.b.Write(p)
}

func (c CLI) run(ctx context.Context, args ...string) ([]byte, error) {
	if !filepath.IsAbs(c.Bin) {
		return nil, fmt.Errorf("tailscale binary %q is not an absolute path", c.Bin)
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	mk := c.Command
	if mk == nil {
		mk = func(ctx context.Context, n string, a ...string) *exec.Cmd { return exec.CommandContext(ctx, n, a...) }
	}
	cmd := mk(ctx, c.Bin, args...)
	out := &capWriter{n: 1 << 20}
	cmd.Stdout = out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("tailscale %s: %w", args[0], err)
	}
	return out.b.Bytes(), nil
}

func (c CLI) WhoIs(ctx context.Context, addr string) (Identity, error) {
	b, err := c.run(ctx, "whois", "--json", addr)
	if err != nil {
		return Identity{}, err
	}
	return Parse(b)
}

// SelfIPs are this node's own tailnet addresses (`tailscale ip`, one per line).
func (c CLI) SelfIPs(ctx context.Context) ([]netip.Addr, error) {
	b, err := c.run(ctx, "ip")
	if err != nil {
		return nil, err
	}
	return parseIPs(strings.Fields(string(b)))
}

// CheckBinary refuses a tailscale binary whose path decides who every caller is, when someone else could
// change it: it must be absolute, a regular file, and neither it nor its directory writable by group or other.
func CheckBinary(path string) error {
	if !filepath.IsAbs(path) {
		return fmt.Errorf("%q is not an absolute path", path)
	}
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if !fi.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", path)
	}
	for _, p := range []string{path, filepath.Dir(path)} {
		st, err := os.Stat(p)
		if err != nil {
			return err
		}
		if st.Mode().Perm()&0o022 != 0 {
			return fmt.Errorf("%s is writable by group or other: whoever can write it decides every caller's identity", p)
		}
	}
	return nil
}

// CheckSocket expects tailscaled's LocalAPI socket: a unix socket owned by root or by the daemon's own user.
func CheckSocket(path string) error {
	fi, err := os.Stat(path)
	if err != nil {
		return err
	}
	if fi.Mode()&os.ModeSocket == 0 {
		return fmt.Errorf("%s is not a unix socket", path)
	}
	if st, ok := fi.Sys().(*syscall.Stat_t); ok && st.Uid != 0 && int(st.Uid) != os.Getuid() {
		return fmt.Errorf("%s is owned by uid %d, not root or this daemon's user", path, st.Uid)
	}
	return nil
}

// Cache remembers an answer per peer IP (not per port: the node behind an address does not change between
// connections), so a burst of requests asks once, and bounds the asking: concurrent lookups for one host
// share one question, at most MaxInflight questions run at once (the rest wait for a slot or their
// deadline), and a failure is remembered for NegTTL (still an error: fail-closed) so that a flood of
// unknown callers cannot fork a process each. Keep TTL short: it is how long a re-tag or a removal takes
// to be noticed. The LocalAPI is an in-memory lookup in tailscaled and needs no cache at all.
type Cache struct {
	R           Resolver
	TTL         time.Duration // default 5 s
	NegTTL      time.Duration // default 2 s
	MaxInflight int           // default 8
	Now         func() time.Time

	mu     sync.Mutex
	m      map[string]cached
	flight map[string]*call
	sem    chan struct{}
}

type cached struct {
	id  Identity
	err error
	at  time.Time
}

type call struct {
	done chan struct{}
	id   Identity
	err  error
}

func (c *Cache) WhoIs(ctx context.Context, addr string) (Identity, error) {
	ttl, neg, max := c.TTL, c.NegTTL, c.MaxInflight
	if ttl == 0 {
		ttl = 5 * time.Second
	}
	if neg == 0 {
		neg = 2 * time.Second
	}
	if max == 0 {
		max = 8
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
	if c.m == nil {
		c.m, c.flight, c.sem = map[string]cached{}, map[string]*call{}, make(chan struct{}, max)
	}
	if v, ok := c.m[host]; ok {
		if (v.err == nil && now().Sub(v.at) < ttl) || (v.err != nil && now().Sub(v.at) < neg) {
			c.mu.Unlock()
			return v.id, v.err
		}
	}
	if f, ok := c.flight[host]; ok {
		c.mu.Unlock()
		select {
		case <-f.done:
			return f.id, f.err
		case <-ctx.Done():
			return Identity{}, ctx.Err()
		}
	}
	f := &call{done: make(chan struct{})}
	c.flight[host] = f
	c.mu.Unlock()

	select {
	case c.sem <- struct{}{}:
		f.id, f.err = c.R.WhoIs(ctx, addr)
		<-c.sem
	case <-ctx.Done():
		f.err = ctx.Err()
	}
	c.mu.Lock()
	delete(c.flight, host)
	if f.err == nil || ctx.Err() == nil { // a caller that gave up says nothing about the peer
		t := now()
		for k, v := range c.m { // evict what is past its time, so the map stays small
			if t.Sub(v.at) >= ttl && t.Sub(v.at) >= neg {
				delete(c.m, k)
			}
		}
		c.m[host] = cached{f.id, f.err, t}
	}
	c.mu.Unlock()
	close(f.done)
	return f.id, f.err
}
