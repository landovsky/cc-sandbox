// Tests of the WhoIs clients. Why: the caller's class rests on these answers, so each way of asking
// (tailscaled's LocalAPI, the CLI) must read the real JSON shape, fail closed when it cannot, and the
// cache must never keep a failure or confuse two peers.
package whois

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"
	"testing"
	"time"
)

const sample = `{"Node":{"ID":123,"Name":"Mac-Tomas.skunk-escalator.ts.net.","Tags":null,"Addresses":["100.64.0.1/32"]},"UserProfile":{"ID":1,"LoginName":"tomas@example.com","DisplayName":"Tomas"},"CapMap":{}}`

func TestParseReadsTheRealShape(t *testing.T) {
	id, err := Parse([]byte(sample))
	if err != nil || id.Login != "tomas@example.com" || id.Device != "mac-tomas.skunk-escalator.ts.net" || id.Short != "mac-tomas" || len(id.Tags) != 0 {
		t.Fatalf("%+v %v", id, err)
	}
	tagged, _ := Parse([]byte(`{"Node":{"Name":"pod.ts.net.","Tags":["tag:ccsbx"]},"UserProfile":{"LoginName":"tagged-devices"}}`))
	if len(tagged.Tags) != 1 || tagged.Tags[0] != "tag:ccsbx" {
		t.Fatalf("%+v", tagged)
	}
	for _, bad := range []string{``, `{}`, `[]`, `{"Node":{"Name":""}}`, `not json`} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Errorf("%q parsed", bad)
		}
	}
}

// context: hp has no tailscale CLI on its PATH for a service uid, but tailscaled's socket answers.
func TestLocalAPIAsksTheDaemonOverItsUnixSocket(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "ts")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "t.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	var gotAddr string
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAddr = r.URL.Query().Get("addr")
		if r.URL.Path != "/localapi/v0/whois" {
			w.WriteHeader(404)
			return
		}
		w.Write([]byte(sample))
	})}
	go srv.Serve(l)
	defer srv.Close()
	id, err := (&LocalAPI{Socket: sock}).WhoIs(context.Background(), "100.64.0.1:51234")
	if err != nil || id.Short != "mac-tomas" || gotAddr != "100.64.0.1:51234" {
		t.Fatalf("%+v %v addr=%q", id, err, gotAddr)
	}
	if _, err := (&LocalAPI{Socket: filepath.Join(dir, "nope.sock")}).WhoIs(context.Background(), "100.64.0.1:1"); err == nil {
		t.Fatal("a missing tailscaled socket was not an error")
	}
}

// context: on the Mac the supported way in is `tailscale whois --json`.
func TestCLIRunsTailscaleWhoisJSON(t *testing.T) {
	var args []string
	c := CLI{Bin: "/usr/bin/tailscale", Command: func(ctx context.Context, name string, a ...string) *exec.Cmd {
		args = append([]string{name}, a...)
		return exec.CommandContext(ctx, "sh", "-c", "printf '%s' '"+sample+"'")
	}}
	id, err := c.WhoIs(context.Background(), "100.64.0.1:2")
	if err != nil || id.Short != "mac-tomas" || len(args) != 4 || args[1] != "whois" || args[2] != "--json" || args[3] != "100.64.0.1:2" {
		t.Fatalf("%+v %v %v", id, err, args)
	}
	bad := CLI{Bin: "/usr/bin/tailscale", Command: func(ctx context.Context, name string, a ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "false")
	}}
	if _, err := bad.WhoIs(context.Background(), "100.64.0.1:2"); err == nil {
		t.Fatal("a failing CLI was not an error")
	}
}

type countR struct {
	n   int
	err error
}

func (c *countR) WhoIs(context.Context, string) (Identity, error) {
	c.n++
	if c.err != nil {
		return Identity{}, c.err
	}
	return Identity{Login: "x", Short: "d"}, nil
}

func TestCacheKeepsAnswersPerIPForAShortTime(t *testing.T) {
	r := &countR{}
	now := time.Now()
	c := &Cache{R: r, TTL: time.Minute, Now: func() time.Time { return now }}
	for _, a := range []string{"100.64.0.1:1", "100.64.0.1:2", "100.64.0.1:3"} {
		if _, err := c.WhoIs(context.Background(), a); err != nil {
			t.Fatal(err)
		}
	}
	if r.n != 1 {
		t.Fatalf("asked %d times for one peer IP", r.n)
	}
	c.WhoIs(context.Background(), "100.64.0.2:1")
	if r.n != 2 {
		t.Fatalf("two peers share an answer: %d", r.n)
	}
	now = now.Add(2 * time.Minute)
	c.WhoIs(context.Background(), "100.64.0.1:9")
	if r.n != 3 {
		t.Fatalf("the answer outlived its TTL: %d", r.n)
	}
	if len(c.m) != 1 {
		t.Fatalf("expired answers were not evicted: %d kept", len(c.m))
	}
	if _, err := c.WhoIs(context.Background(), "garbage"); err == nil {
		t.Fatal("a bad address was accepted")
	}
}

// context: a flood of unknown callers must not fork a process each; a failure is remembered briefly and
// stays an error (fail-closed), then the peer is asked again.
func TestCacheRemembersAFailureBrieflyAndStillFailsClosed(t *testing.T) {
	f := &countR{err: errors.New("down")}
	now := time.Now()
	c := &Cache{R: f, NegTTL: 2 * time.Second, Now: func() time.Time { return now }}
	for i := 0; i < 20; i++ {
		if _, err := c.WhoIs(context.Background(), "100.64.0.9:1"); err == nil {
			t.Fatal("a failure became an answer")
		}
	}
	if f.n != 1 {
		t.Fatalf("20 failing requests asked tailscale %d times", f.n)
	}
	now = now.Add(3 * time.Second)
	c.WhoIs(context.Background(), "100.64.0.9:1")
	if f.n != 2 {
		t.Fatalf("a failure outlived its short memory: %d", f.n)
	}
}

type slowR struct {
	mu          sync.Mutex
	cur, max, n int
	release     chan struct{}
}

func (s *slowR) WhoIs(ctx context.Context, _ string) (Identity, error) {
	s.mu.Lock()
	s.cur++
	s.n++
	if s.cur > s.max {
		s.max = s.cur
	}
	s.mu.Unlock()
	select {
	case <-s.release:
	case <-ctx.Done():
	}
	s.mu.Lock()
	s.cur--
	s.mu.Unlock()
	return Identity{Login: "x", Short: "d"}, nil
}

// context: many callers asking at once (a burst, or a flood from distinct addresses) must neither multiply
// the question for one peer nor run more than MaxInflight questions at the same time.
func TestCacheAsksOncePerPeerAndAtMostMaxInflightAtOnce(t *testing.T) {
	r := &slowR{release: make(chan struct{})}
	c := &Cache{R: r, MaxInflight: 3}
	var wg sync.WaitGroup
	for i := 0; i < 40; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			c.WhoIs(context.Background(), fmt.Sprintf("100.64.0.%d:1", i%10)) // 10 peers, 4 callers each
		}(i)
	}
	time.Sleep(200 * time.Millisecond)
	close(r.release)
	wg.Wait()
	if r.max > 3 {
		t.Fatalf("%d questions ran at once (cap 3)", r.max)
	}
	if r.n != 10 {
		t.Fatalf("%d questions for 10 peers", r.n)
	}
}

// context (M4): every LocalAPI lookup used to build a fresh http.Transport whose idle connection was never
// closed, so a steady poll leaked a connection and goroutines forever.
func TestLocalAPIDoesNotLeakConnectionsOrGoroutines(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "ts")
	defer os.RemoveAll(dir)
	sock := filepath.Join(dir, "t.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) })}
	go srv.Serve(l)
	defer srv.Close()
	a := &LocalAPI{Socket: sock}
	a.WhoIs(context.Background(), "100.64.0.1:1")
	time.Sleep(100 * time.Millisecond)
	before := runtime.NumGoroutine()
	for i := 0; i < 200; i++ {
		a.WhoIs(context.Background(), "100.64.0.1:1")
	}
	time.Sleep(200 * time.Millisecond)
	if after := runtime.NumGoroutine(); after > before+5 {
		t.Fatalf("goroutines grew from %d to %d over 200 lookups", before, after)
	}
}

// context (S5): tailscaled answers for an address inside a subnet route with the ROUTER's identity.
func TestAnAnswerIsOnlyForTheNodesOwnAddressesAndNeverForARouter(t *testing.T) {
	id, _ := Parse([]byte(sample))
	if !id.Owns(netip.MustParseAddr("100.64.0.1")) || id.Owns(netip.MustParseAddr("192.168.1.9")) || id.Owns(netip.MustParseAddr("100.64.0.2")) {
		t.Fatalf("Owns: %+v", id.Addrs)
	}
	r, _ := Parse([]byte(`{"Node":{"Name":"gw.ts.net.","Addresses":["100.64.0.7/32"],"PrimaryRoutes":["192.168.1.0/24"]},"UserProfile":{"LoginName":"tomas@example.com"}}`))
	if !r.Router {
		t.Fatal("a node with PrimaryRoutes is a router")
	}
}

// context (S8): the binary that answers who is who is chosen once, by absolute path, and must not be
// changeable by anyone else.
func TestCheckBinaryRefusesWhatOthersCouldChange(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "tailscale")
	os.WriteFile(bin, []byte("#!/bin/sh\n"), 0o755)
	os.Chmod(dir, 0o755)
	if err := CheckBinary(bin); err != nil {
		t.Fatalf("a private binary was refused: %v", err)
	}
	os.Chmod(bin, 0o775)
	if CheckBinary(bin) == nil {
		t.Fatal("a group-writable binary was accepted")
	}
	os.Chmod(bin, 0o755)
	os.Chmod(dir, 0o777)
	if CheckBinary(bin) == nil {
		t.Fatal("a binary in a world-writable directory was accepted")
	}
	os.Chmod(dir, 0o755)
	if CheckBinary("tailscale") == nil {
		t.Fatal("a bare name (PATH lookup) was accepted")
	}
	if CheckBinary(filepath.Join(dir, "missing")) == nil {
		t.Fatal("a missing binary was accepted")
	}
	if _, err := (CLI{Bin: "tailscale"}).WhoIs(context.Background(), "100.64.0.1:1"); err == nil {
		t.Fatal("a CLI with a bare binary name ran")
	}
}

func TestCLIOutputIsCappedAndSelfIPsAreRead(t *testing.T) {
	big := CLI{Bin: "/usr/bin/tailscale", Command: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "head -c 2000000 /dev/zero")
	}}
	if _, err := big.WhoIs(context.Background(), "100.64.0.1:1"); err == nil {
		t.Fatal("2 MB of output was accepted")
	}
	ip := CLI{Bin: "/usr/bin/tailscale", Command: func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		return exec.CommandContext(ctx, "sh", "-c", "printf '100.64.0.1\\nfd7a:115c:a1e0::1\\n'")
	}}
	got, err := ip.SelfIPs(context.Background())
	if err != nil || len(got) != 2 || got[0] != netip.MustParseAddr("100.64.0.1") {
		t.Fatalf("%v %v", got, err)
	}
}

func TestCheckSocketExpectsASocketOfRootOrTheDaemonUser(t *testing.T) {
	dir, _ := os.MkdirTemp("/tmp", "ts")
	defer os.RemoveAll(dir)
	f := filepath.Join(dir, "plain")
	os.WriteFile(f, nil, 0o600)
	if CheckSocket(f) == nil {
		t.Fatal("a regular file passed as a socket")
	}
	l, _ := net.Listen("unix", filepath.Join(dir, "s"))
	defer l.Close()
	if err := CheckSocket(filepath.Join(dir, "s")); err != nil {
		t.Fatalf("our own socket was refused: %v", err)
	}
}
