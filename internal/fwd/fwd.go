// Package fwd is the per-session forwarder (build plan §3 P2; `ccsbx fwd`).
//
// It runs OUTSIDE the fence, next to it, and is the only holder of the session
// token and the per-generation key. The fence reaches it over plain HTTP on its
// internal network as http://<fwd>/<door>/<path>; fwd checks the route, strips
// every credential the fence sent, adds `Authorization: Bearer <token>` and the
// proof header (token.SignProof) over the exact method and path the door will
// receive, and relays the call to that door only.
//
// Guarantees (each has a test):
//   - the token and key never appear in a response, a log line, or an error
//   - default deny: only (door, method, path) in the allowlist go out; decision
//     endpoints are refused even if a rules file names them
//   - fwd dials only the configured door addresses; redirects are not followed
//     and proxy env vars are ignored, so a door can't bounce a call elsewhere
//   - it stops when the token expires (dies with the generation)
//
// Secrets arrive on stdin as one JSON object, never as a flag or an env var,
// because both show up in /proc and `ps` (T:no-token-in-fence).
package fwd

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/landovsky/cc-sandbox/internal/token"
)

// Config is everything a Forwarder needs.
type Config struct {
	Token    string
	Key      *token.GenKey
	Doors    map[string]*url.URL // name → base URL (scheme://host:port[/base])
	Rules    []Rule
	CAFile   string // extra root CA for https doors ("" = system roots)
	MaxBody  int64  // request body cap in bytes (0 = 32 MiB)
	Logf     func(format string, a ...any)
	Now      func() time.Time
	dialOnly map[string]bool
}

// Forwarder is an http.Handler.
type Forwarder struct {
	cfg    Config
	proxy  *httputil.ReverseProxy
	counts struct{ ok, denied atomic.Int64 }
}

// New builds a Forwarder. It refuses an unsafe configuration.
func New(cfg Config) (*Forwarder, error) {
	if cfg.Token == "" || cfg.Key == nil {
		return nil, errors.New("fwd: token and generation key are required")
	}
	if len(cfg.Doors) == 0 {
		return nil, errors.New("fwd: no doors configured")
	}
	if cfg.Rules == nil {
		cfg.Rules = DefaultRules
	}
	if err := ValidateRules(cfg.Rules, cfg.Doors); err != nil {
		return nil, err
	}
	if cfg.MaxBody == 0 {
		cfg.MaxBody = 32 << 20
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.Logf == nil {
		cfg.Logf = func(string, ...any) {}
	}
	cfg.dialOnly = map[string]bool{}
	for name, u := range cfg.Doors {
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf("fwd: door %s: scheme must be http or https", name)
		}
		cfg.dialOnly[hostPort(u)] = true
	}
	tr := &http.Transport{
		Proxy: nil, // HTTP_PROXY in the environment must not redirect door calls
		DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
			if !cfg.dialOnly[addr] { // T:fwd-cannot-reach-non-door
				return nil, fmt.Errorf("fwd: %s is not a configured door", addr)
			}
			return (&net.Dialer{Timeout: 10 * time.Second}).DialContext(ctx, network, addr)
		},
		TLSClientConfig:       &tls.Config{MinVersion: tls.VersionTLS12},
		ResponseHeaderTimeout: 10 * time.Minute, // model calls are slow
		DisableCompression:    true,
	}
	if cfg.CAFile != "" {
		pem, err := os.ReadFile(cfg.CAFile)
		if err != nil {
			return nil, err
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("fwd: no certificates in %s", cfg.CAFile)
		}
		tr.TLSClientConfig.RootCAs = pool
	}
	f := &Forwarder{cfg: cfg}
	f.proxy = &httputil.ReverseProxy{
		Transport:     tr,
		FlushInterval: -1, // SSE from the model door
		Rewrite:       f.rewrite,
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			cfg.Logf("fwd: upstream error: %s", scrub(err.Error(), cfg.Token))
			http.Error(w, "bad gateway", http.StatusBadGateway)
		},
		// Redirects are responses to the fence; fwd never follows them (a
		// ReverseProxy doesn't), and Location is dropped below.
		ModifyResponse: func(resp *http.Response) error {
			resp.Header.Del("Location")
			resp.Header.Del("Set-Cookie")
			return nil
		},
	}
	return f, nil
}

func hostPort(u *url.URL) string {
	h := u.Host
	if u.Port() == "" {
		if u.Scheme == "https" {
			return net.JoinHostPort(u.Hostname(), "443")
		}
		return net.JoinHostPort(u.Hostname(), "80")
	}
	return h
}

type ctxKey struct{}

// splitDoor turns "/model/v1/messages" into ("model", "/v1/messages").
func splitDoor(p string) (string, string) {
	rest := strings.TrimPrefix(p, "/")
	i := strings.IndexByte(rest, '/')
	if i < 0 {
		return rest, "/"
	}
	return rest[:i], rest[i:]
}

func (f *Forwarder) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/_fwd/health" && r.Method == http.MethodGet {
		w.Write([]byte("ok\n")) // says nothing about the token, doors or generation
		return
	}
	deny := func(code int, why string) {
		f.counts.denied.Add(1)
		f.cfg.Logf(`{"fwd":"deny","method":%q,"path":%q,"why":%q}`, r.Method, r.URL.Path, why)
		http.Error(w, why, code)
	}
	if r.URL.IsAbs() || r.Host == "" && r.URL.Host != "" { // proxy-style request
		deny(http.StatusForbidden, "not a door route")
		return
	}
	clean, ok := CleanPath(r.URL)
	if !ok {
		deny(http.StatusForbidden, "unclean path")
		return
	}
	door, rest := splitDoor(clean)
	if _, known := f.cfg.Doors[door]; !known {
		deny(http.StatusForbidden, "not a door route")
		return
	}
	if v := Check(f.cfg.Rules, door, r.Method, rest); v != Allow {
		deny(http.StatusForbidden, "refused: "+v.String())
		return
	}
	if f.cfg.Now().Unix() >= f.expiry() {
		deny(http.StatusServiceUnavailable, "generation ended")
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, f.cfg.MaxBody)
	f.counts.ok.Add(1)
	f.proxy.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, [2]string{door, rest})))
}

func (f *Forwarder) expiry() int64 {
	parts := strings.Split(f.cfg.Token, ".")
	if len(parts) != 3 {
		return 0
	}
	b, err := token.B64().DecodeString(parts[1])
	if err != nil {
		return 0
	}
	var c struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(b, &c) != nil {
		return 0
	}
	return c.Exp
}

// Expiry is when the token (and so this forwarder) ends.
func (f *Forwarder) Expiry() time.Time { return time.Unix(f.expiry(), 0) }

func (f *Forwarder) rewrite(pr *httputil.ProxyRequest) {
	dr := pr.In.Context().Value(ctxKey{}).([2]string)
	base := f.cfg.Doors[dr[0]]
	out := pr.Out
	out.URL.Scheme, out.URL.Host = base.Scheme, base.Host
	out.URL.Path = strings.TrimSuffix(base.Path, "/") + dr[1]
	out.URL.RawPath = ""
	out.Host = base.Host

	// Drop everything the fence could use to pose as someone: its own credentials,
	// any proof header, and identity-ish headers. Doors strip them again, but the
	// forwarder must not rely on that.
	for k := range out.Header {
		lk := strings.ToLower(k)
		if lk == "authorization" || lk == "x-api-key" || lk == "proxy-authorization" ||
			lk == "cookie" || lk == strings.ToLower(token.ProofHeader) ||
			strings.HasPrefix(lk, "tailscale-") || strings.HasPrefix(lk, "x-forwarded-") ||
			strings.HasPrefix(lk, "x-ccsbx-") || lk == "forwarded" || lk == "x-real-ip" {
			out.Header.Del(k)
		}
	}
	proof, err := token.SignProof(f.cfg.Key, out.Method, out.URL.RequestURI(), f.cfg.Now())
	if err != nil {
		out.Header.Set(token.ProofHeader, "")
	}
	out.Header.Set("Authorization", "Bearer "+f.cfg.Token)
	out.Header.Set(token.ProofHeader, proof)
}

// Stats are for tests and the health line, never for the fence.
func (f *Forwarder) Stats() (ok, denied int64) { return f.counts.ok.Load(), f.counts.denied.Load() }

func scrub(s, tok string) string {
	if tok == "" {
		return s
	}
	return strings.ReplaceAll(s, tok, "[token]")
}
