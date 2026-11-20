// Tests for `ccsbx start` in helper mode: the spec goes to hostd over its local socket, the answers
// map to the same exits as direct mode, and nothing is written by the client (a record under the
// caller's home is one ccsbx-root would refuse, so a session started that way would die at once).
package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
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
	wd, _ := os.MkdirTemp("/var/tmp", "ccsbx-hello-")
	t.Cleanup(func() { os.RemoveAll(wd) })
	wd, _ = filepath.EvalSymlinks(wd)
	spec := strings.Replace(string(b), "/var/tmp/ccsbx-hello", wd, 1)
	p := filepath.Join(t.TempDir(), "spec.json")
	os.WriteFile(p, []byte(spec), 0o644)
	return p, spec
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
	if got.method != "POST /v1/start" || got.ctype != "application/json" {
		t.Errorf("hostd got %q %q", got.method, got.ctype)
	}
	var sent, orig map[string]any
	json.Unmarshal([]byte(got.body), &sent)
	json.Unmarshal([]byte(body), &orig)
	// The client adds gitdir and writes the workdir back in full; everything else must be untouched.
	for _, m := range []map[string]any{sent, orig} {
		delete(m, "gitdir")
		delete(m, "workdir")
		delete(m, "grants")
	}
	if fmt.Sprint(sent) != fmt.Sprint(orig) {
		t.Errorf("the client changed the spec beyond the resolved paths:\n sent %v\n orig %v", sent, orig)
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

// context: hostd, as another uid, cannot enter the owner's home, so the CLIENT must send the paths
// already resolved: a symlinked workdir arrives as its real path, and a git worktree arrives with its
// gitdir. (hostd then only checks their shape; ccsbx-root validates them for real at run time.)
func TestHelperStartSendsTheResolvedWorkdirAndGitdir(t *testing.T) {
	newStubs(t)
	sock, got := fakeHostd(t, 200, okReply)
	real, _ := os.MkdirTemp("/var/tmp", "ccsbx-real-")
	t.Cleanup(func() { os.RemoveAll(real) })
	real, _ = filepath.EvalSymlinks(real)
	link := real + "-link"
	os.Symlink(real, link)
	t.Cleanup(func() { os.Remove(link) })
	if out, err := exec.Command("git", "-C", real, "init", "-q").CombinedOutput(); err != nil {
		t.Skipf("no git: %v %s", err, out)
	}
	b, _ := os.ReadFile("../../tests/specs/hello.json")
	spec := filepath.Join(t.TempDir(), "spec.json")
	os.WriteFile(spec, []byte(strings.Replace(string(b), "/var/tmp/ccsbx-hello", link, 1)), 0o644)
	if code, out := run("start", "--root", "helper", "--socket", sock, "--spec", spec); code != 0 {
		t.Fatalf("%d %s", code, out)
	}
	var sent struct {
		Workdir struct{ Path string }
		Gitdir  string
	}
	json.Unmarshal([]byte(got.body), &sent)
	if sent.Workdir.Path != real || sent.Gitdir != filepath.Join(real, ".git") {
		t.Fatalf("hostd got workdir %q gitdir %q; want %q and its .git", sent.Workdir.Path, sent.Gitdir, real)
	}
}

// context: a spec that already carries a gitdir would let the caller choose what is mounted; the
// client computes it, never takes it from the file.
func TestHelperStartRefusesASpecThatBringsItsOwnGitdir(t *testing.T) {
	newStubs(t)
	sock, got := fakeHostd(t, 200, okReply)
	p, body := helloSpec(t)
	os.WriteFile(p, []byte(strings.Replace(body, `"profile"`, `"gitdir":"/etc","profile"`, 1)), 0o644)
	if code, _ := run("start", "--root", "helper", "--socket", sock, "--spec", p); code != 2 || got.method != "" {
		t.Fatalf("code %d, hostd was called: %q", code, got.method)
	}
}

// context: a workdir that does not exist is refused here, with the path, not left to hostd.
func TestHelperStartRefusesAMissingWorkdirBeforeCallingHostd(t *testing.T) {
	newStubs(t)
	sock, got := fakeHostd(t, 200, okReply)
	p, body := helloSpec(t)
	wd := regexp.MustCompile(`"path":"([^"]+)"`).FindStringSubmatch(body)[1]
	os.WriteFile(p, []byte(strings.Replace(body, wd, wd+"/nope", 1)), 0o644)
	if code, out := run("start", "--root", "helper", "--socket", sock, "--spec", p); code != 2 || got.method != "" || !strings.Contains(out, "nope") {
		t.Fatalf("code %d %s, hostd called: %q", code, out, got.method)
	}
}
