// Tests for `ccsbx start` in helper mode: the spec goes to hostd over its local socket, the answers
// map to the same exits as direct mode, and nothing is written by the client (a record under the
// caller's home is one ccsbx-root would refuse, so a session started that way would die at once).
package main

import (
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// fakeHostd serves /v1/start on a unix socket and records what it was sent.
func fakeHostd(t *testing.T, status int, reply string) (sock string, got *struct{ body, ctype, method string }) {
	t.Helper()
	d, _ := os.MkdirTemp("/var/tmp", "ccsbx-hd-")
	t.Cleanup(func() { os.RemoveAll(d) })
	sock = filepath.Join(d, "h.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	got = &struct{ body, ctype, method string }{}
	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		got.body, got.ctype, got.method = string(b), r.Header.Get("Content-Type"), r.Method+" "+r.URL.Path
		w.WriteHeader(status)
		io.WriteString(w, reply)
	})}
	go srv.Serve(l)
	t.Cleanup(func() { srv.Close() })
	return sock, got
}

func helloSpec(t *testing.T) (path string, body string) {
	t.Helper()
	b, err := os.ReadFile("../../tests/specs/hello.json")
	if err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(t.TempDir(), "spec.json")
	os.WriteFile(p, b, 0o644)
	return p, string(b)
}

const okReply = `{"v":1,"sid":"s0abcdefgh","gen":1,"id":"tomas/hp/s0abcdefgh/g1","backend":"docker","attach":{"local":["sudo","ccsbx-root","attach","s0abcdefgh"]}}`

func TestHelperStartSendsTheSpecToHostdAndWritesNothing(t *testing.T) {
	newStubs(t)
	sock, got := fakeHostd(t, 200, okReply)
	spec, body := helloSpec(t)
	state := t.TempDir()
	code, out := run("start", "--root", "helper", "--socket", sock, "--spec", spec, "--state", state, "--json")
	if code != 0 || !strings.Contains(out, `"sid": "s0abcdefgh"`) {
		t.Fatalf("%d %s", code, out)
	}
	if got.method != "POST /v1/start" || got.ctype != "application/json" || got.body != body {
		t.Errorf("hostd got %q %q, body equal=%v", got.method, got.ctype, got.body == body)
	}
	if ents, _ := os.ReadDir(state); len(ents) != 0 {
		t.Errorf("the client wrote under --state in helper mode: %v", ents)
	}
	if code, out := run("start", "--root", "helper", "--socket", sock, "--spec", spec, "--state", state); code != 0 || !strings.Contains(out, "started s0abcdefgh") {
		t.Errorf("%d %s", code, out)
	}
}

func TestHelperStartMapsHostdAnswersToTheDirectModeExits(t *testing.T) {
	newStubs(t)
	spec, _ := helloSpec(t)
	refusal := `{"refused":"rate_capped","message":"too many starts"}`
	sock, _ := fakeHostd(t, 409, refusal)
	code, out := run("start", "--root", "helper", "--socket", sock, "--spec", spec, "--json")
	if code != 2 || !strings.Contains(out, "rate_capped") || !strings.Contains(out, "ccsbx start: refused: rate_capped: too many starts") {
		t.Errorf("refusal: %d %s", code, out)
	}
	sock, _ = fakeHostd(t, 500, `{"error":"boom"}`)
	if code, out := run("start", "--root", "helper", "--socket", sock, "--spec", spec); code != 1 || !strings.Contains(out, "500") {
		t.Errorf("hostd error: %d %s", code, out)
	}
	sock, _ = fakeHostd(t, 403, `forbidden`)
	if code, _ := run("start", "--root", "helper", "--socket", sock, "--spec", spec); code != 1 {
		t.Errorf("403: %d", code)
	}
	sock, _ = fakeHostd(t, 200, `not json`)
	if code, _ := run("start", "--root", "helper", "--socket", sock, "--spec", spec); code != 1 {
		t.Errorf("a garbled success was accepted: %d", code)
	}
	sock, _ = fakeHostd(t, 409, `garbled`)
	if code, _ := run("start", "--root", "helper", "--socket", sock, "--spec", spec); code != 2 {
		t.Errorf("a garbled refusal: %d", code)
	}
}

// hostd not running is a clear exit 2, never a record written by this process and never direct mode.
func TestHelperStartWithoutHostdIsAClearRefusal(t *testing.T) {
	newStubs(t)
	spec, _ := helloSpec(t)
	state := t.TempDir()
	code, out := run("start", "--root", "helper", "--socket", filepath.Join(t.TempDir(), "none.sock"), "--spec", spec, "--state", state)
	if code != 2 || !strings.Contains(out, "ccsbx-hostd is not running") {
		t.Errorf("%d %s", code, out)
	}
	if ents, _ := os.ReadDir(state); len(ents) != 0 {
		t.Errorf("a record or lock was written: %v", ents)
	}
}

func TestHelperStartRefusesDevFlagsAndBadSpecs(t *testing.T) {
	newStubs(t)
	sock, got := fakeHostd(t, 200, okReply)
	spec, _ := helloSpec(t)
	if code, _ := run("start", "--root", "helper", "--socket", sock, "--spec", spec, "--launcher", "none"); code != 64 {
		t.Errorf("--launcher none in helper mode: %d", code)
	}
	if code, _ := run("start", "--root", "helper", "--socket", sock, "--spec", spec, "--run-cmd", "sh"); code != 64 {
		t.Errorf("--run-cmd in helper mode: %d", code)
	}
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"v":1,"nonsense":true}`), 0o644)
	if code, _ := run("start", "--root", "helper", "--socket", sock, "--spec", bad); code != 2 {
		t.Errorf("a bad spec: %d", code)
	}
	if got.method != "" {
		t.Errorf("hostd was called for a request that is refused here: %s", got.method)
	}
}
