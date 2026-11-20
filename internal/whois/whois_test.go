// Tests of the WhoIs clients. Why: the caller's class rests on these answers, so each way of asking
// (tailscaled's LocalAPI, the CLI) must read the real JSON shape, fail closed when it cannot, and the
// cache must never keep a failure or confuse two peers.
package whois

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
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
	id, err := LocalAPI{Socket: sock}.WhoIs(context.Background(), "100.64.0.1:51234")
	if err != nil || id.Short != "mac-tomas" || gotAddr != "100.64.0.1:51234" {
		t.Fatalf("%+v %v addr=%q", id, err, gotAddr)
	}
	if _, err := (LocalAPI{Socket: filepath.Join(dir, "nope.sock")}).WhoIs(context.Background(), "100.64.0.1:1"); err == nil {
		t.Fatal("a missing tailscaled socket was not an error")
	}
}

// context: on the Mac the supported way in is `tailscale whois --json`.
func TestCLIRunsTailscaleWhoisJSON(t *testing.T) {
	var args []string
	c := CLI{Bin: "tailscale", Command: func(ctx context.Context, name string, a ...string) *exec.Cmd {
		args = append([]string{name}, a...)
		return exec.CommandContext(ctx, "sh", "-c", "printf '%s' '"+sample+"'")
	}}
	id, err := c.WhoIs(context.Background(), "100.64.0.1:2")
	if err != nil || id.Short != "mac-tomas" || len(args) != 4 || args[1] != "whois" || args[2] != "--json" || args[3] != "100.64.0.1:2" {
		t.Fatalf("%+v %v %v", id, err, args)
	}
	bad := CLI{Command: func(ctx context.Context, name string, a ...string) *exec.Cmd {
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

func TestCacheKeepsAnswersPerIPForAShortTimeAndNeverKeepsFailures(t *testing.T) {
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
	f := &countR{err: errors.New("down")}
	c2 := &Cache{R: f}
	c2.WhoIs(context.Background(), "100.64.0.9:1")
	c2.WhoIs(context.Background(), "100.64.0.9:1")
	if f.n != 2 {
		t.Fatalf("a failure was cached: %d", f.n)
	}
	if _, err := c2.WhoIs(context.Background(), "garbage"); err == nil {
		t.Fatal("a bad address was accepted")
	}
}
