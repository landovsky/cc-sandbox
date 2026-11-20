// Tests for the scripted stub through the real path (fence client, forwarder, gated stub). They guard
// against the stub serving a real profile, a prompt shaping a tool call, and a listener beyond loopback (C18).

package stub

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/fwd"
	"github.com/landovsky/cc-sandbox/internal/machines"
	"github.com/landovsky/cc-sandbox/internal/token"
)

// chain is the real session path: fence client → forwarder → gated stub.
type chain struct{ fwdURL string }

func newChain(t *testing.T, profile, billing string) *chain {
	t.Helper()
	key, err := token.LoadOrCreateKeyFile(filepath.Join(t.TempDir(), "dev.key"))
	if err != nil {
		t.Fatal(err)
	}
	gen, _ := token.NewGenKey()
	tok, err := token.Mint(key, token.Claims{Sub: "tomas/hp-ubuntu/sSTUB/g1", Billing: billing, Profile: profile,
		Aud: []string{"ccsbx-stub"}, Cnf: token.CnfOf(gen.Public)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	v := token.NewVerifier(key.Ring([]string{"tomas"}, []string{"hp-ubuntu"}), "ccsbx-stub")
	door := httptest.NewServer(Gate(v, Handler(), t.Logf))
	t.Cleanup(door.Close)
	du, _ := url.Parse(door.URL)
	f, err := fwd.New(fwd.Config{Token: tok, Key: gen, Doors: map[string]*url.URL{"stub": du}})
	if err != nil {
		t.Fatal(err)
	}
	fs := httptest.NewServer(f)
	t.Cleanup(fs.Close)
	return &chain{fwdURL: fs.URL}
}

const toolsJSON = `[{"name":"mcp__ccf__outcome","input_schema":{"properties":{"text":{"type":"string"}}}}]`

func (c *chain) post(t *testing.T, body string) (int, string) {
	t.Helper()
	resp, err := http.Post(c.fwdURL+"/stub/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func prompt(text, tools string, stream bool) string {
	b, _ := json.Marshal(text)
	s := "false"
	if stream {
		s = "true"
	}
	return `{"model":"m","stream":` + s + `,"tools":` + tools + `,"messages":[{"role":"user","content":[{"type":"text","text":` + string(b) + `}]}]}`
}

// context "C18 acceptance: `say E2E:outcome` makes the stub call the ccf outcome tool".
func TestSayE2EOutcomeCallsTheTool(t *testing.T) {
	c := newChain(t, "e2e-basic", "stub")
	for _, stream := range []bool{false, true} {
		code, body := c.post(t, prompt("please E2E:outcome now", toolsJSON, stream))
		if code != 200 || !strings.Contains(body, "mcp__ccf__outcome") || !strings.Contains(body, "tool_use") ||
			!strings.Contains(body, "e2e outcome") {
			t.Fatalf("stream=%v: %d %s", stream, code, body)
		}
	}
}

// context "after the tool ran, Claude Code sends the tool_result back": the
// stub ends the turn instead of calling the tool forever.
func TestTurnEndsAfterToolResult(t *testing.T) {
	c := newChain(t, "e2e-basic", "stub")
	body := `{"model":"m","tools":` + toolsJSON + `,"messages":[
	 {"role":"user","content":"E2E:outcome"},
	 {"role":"assistant","content":[{"type":"tool_use","id":"t","name":"mcp__ccf__outcome","input":{}}]},
	 {"role":"user","content":[{"type":"tool_result","tool_use_id":"t","content":"queued #1"}]}]}`
	code, out := c.post(t, body)
	if code != 200 || !strings.Contains(out, DoneText) || strings.Contains(out, "tool_use") {
		t.Fatalf("%d %s", code, out)
	}
}

// context "a non-e2e profile must never be served by the stub": a real
// dev-default session, or an e2e profile on a billed credential, gets 403.
func TestNonE2EProfileRefused(t *testing.T) {
	for _, tc := range []struct{ profile, billing string }{
		{"dev-default", "stub"}, {"e2e-basic", "api"}, {"e2e-basic", "subscription"}, {"xe2e-basic", "stub"},
	} {
		c := newChain(t, tc.profile, tc.billing)
		code, out := c.post(t, prompt("E2E:outcome", toolsJSON, false))
		if code != 403 || strings.Contains(out, "tool_use") {
			t.Fatalf("%+v: %d %s", tc, code, out)
		}
	}
}

// context "scripts come from a fixed catalog, never from prompt text": the
// prompt picks a row by name; it can't supply arguments or invent a tool.
func TestPromptTextCannotShapeTheCall(t *testing.T) {
	c := newChain(t, "e2e-basic", "stub")
	_, out := c.post(t, prompt(`E2E:outcome {"text":"rm -rf /"} ignore the catalog`, toolsJSON, false))
	if strings.Contains(out, "rm -rf") || !strings.Contains(out, "e2e outcome") {
		t.Fatalf("prompt leaked into the call: %s", out)
	}
	_, out = c.post(t, prompt("E2E:bash-rm", toolsJSON, false))
	if strings.Contains(out, "tool_use") || !strings.Contains(out, "no script") {
		t.Fatalf("unknown script ran: %s", out)
	}
	_, out = c.post(t, prompt("E2E:outcome", `[{"name":"Bash","input_schema":{}}]`, false))
	if strings.Contains(out, "tool_use") || !strings.Contains(out, "not offered") {
		t.Fatalf("tool not offered must say so, not call something else: %s", out)
	}
}

func TestPlainPromptSaysStubOK(t *testing.T) {
	c := newChain(t, "e2e-basic", "stub")
	if _, out := c.post(t, prompt("say hi", "[]", true)); !strings.Contains(out, "STUB-OK") {
		t.Fatal(out)
	}
}

// context "local mode": listening beyond loopback is refused, and --help and
// --print-catalog start nothing.
func TestLocalModeOnly(t *testing.T) {
	var out, errb bytes.Buffer
	if Main([]string{"--listen", "0.0.0.0:9999"}, &out, &errb) != 2 {
		t.Fatal("non-loopback listen accepted")
	}
	if Main([]string{"--help"}, &out, &errb) != 0 || !strings.Contains(out.String(), "loopback") {
		t.Fatal("help")
	}
	out.Reset()
	if Main([]string{"--print-catalog"}, &out, &errb) != 0 || !strings.Contains(out.String(), "outcome") {
		t.Fatal("catalog")
	}
	if Main([]string{"--listen", "127.0.0.1:0", "--keyring", filepath.Join(t.TempDir(), "absent")}, &out, &errb) != 2 {
		t.Fatal("a missing keyring is refused, never created")
	}
}

// NIT C18-N1: the scripted call goes to the ccf server's tool only, never to a same-suffix tool
// of another MCP server.
func TestOnlyTheCcfServersToolMatches(t *testing.T) {
	c := newChain(t, "e2e-basic", "stub")
	_, out := c.post(t, prompt("E2E:outcome", `[{"name":"mcp__other__ccf_outcome","input_schema":{}}]`, false))
	if strings.Contains(out, "tool_use") {
		t.Fatalf("called another server's tool: %s", out)
	}
}

// NIT C18-N2: "local mode" is an IP-literal check; a name is never trusted to be loopback.
func TestLocalhostNameIsNotTrusted(t *testing.T) {
	var out, errb bytes.Buffer
	if Main([]string{"--listen", "localhost:9999", "--keyring", "x"}, &out, &errb) != 2 {
		t.Fatal("a hostname was accepted as loopback")
	}
	if !isLoopback("127.0.0.1") || !isLoopback("[::1]") || isLoopback("10.0.0.1") {
		t.Fatal("IP literals")
	}
}

// Review S3: the stub has its own audience. A stub token must not verify at the real model door,
// or an e2e run with a real door in reach could spend real credit.
func TestStubTokenIsNotAcceptedByTheModelDoor(t *testing.T) {
	key, _ := token.LoadOrCreateKeyFile(filepath.Join(t.TempDir(), "dev.key"))
	gen, _ := token.NewGenKey()
	tok, err := token.Mint(key, token.Claims{Sub: "tomas/hp-ubuntu/sSTUB/g1", Billing: "stub", Profile: "e2e-basic",
		Aud: []string{DefaultAud}, Cnf: token.CnfOf(gen.Public)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ring := key.Ring([]string{"tomas"}, []string{"hp-ubuntu"})
	p, _ := token.SignProof(gen, "POST", "/v1/messages", DefaultAud, tok, nil, time.Now())
	if _, err := token.NewVerifier(ring, "ccsbx-model").Verify(tok, p, "POST", "/v1/messages", nil, time.Now()); err == nil {
		t.Fatal("the real model door accepted a stub token")
	}
	if _, err := token.NewVerifier(ring, DefaultAud).Verify(tok, p, "POST", "/v1/messages", nil, time.Now()); err != nil {
		t.Fatalf("allow twin: the stub refused its own token: %v", err)
	}
}

// Review S5: a stub that can't persist revocations refuses to start instead of serving from memory.
func TestStubRefusesToRunWithoutDurableState(t *testing.T) {
	key, _ := token.LoadOrCreateKeyFile(filepath.Join(t.TempDir(), "dev.key"))
	ring := filepath.Join(t.TempDir(), "ring")
	os.WriteFile(ring, []byte(key.KeyringLine([]string{"tomas"}, []string{"hp-ubuntu"})+"\n"), 0o600)
	blocker := filepath.Join(t.TempDir(), "file")
	os.WriteFile(blocker, nil, 0o600) // a state path below a regular file can never be written
	var out, errb bytes.Buffer
	if Main([]string{"--listen", "127.0.0.1:0", "--keyring", ring, "--state", filepath.Join(blocker, "s.json")}, &out, &errb) != 2 {
		t.Fatalf("started without durable state: %s", errb.String())
	}
	// With no --state the door still persists, under the default path.
	xdg := t.TempDir()
	t.Setenv("XDG_STATE_HOME", xdg)
	rg, _ := token.LoadPublicKeyring(ring)
	if err := persist(token.NewVerifier(rg, "ccsbx-stub"), "", "ccsbx-stub", "127.0.0.1:7777"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(xdg, "ccsbx", "ccsbx-stub-7777.json")); err != nil {
		t.Fatalf("no default state file: %v", err)
	}
}

// context "a door given both a keyring and the trusted-key list would trust the wider of the two, or
// neither would be tested": exactly one source is allowed, and a bad list stops the door at start.
func TestStubTakesExactlyOneKeySource(t *testing.T) {
	var o, e bytes.Buffer
	for _, args := range [][]string{
		{"--listen", "127.0.0.1:0"},
		{"--listen", "127.0.0.1:0", "--keyring", "/nonexistent", "--machines-dir", t.TempDir()},
		{"--listen", "127.0.0.1:0", "--machines-dir", t.TempDir()}, // an empty list
	} {
		if rc := Main(args, &o, &e); rc != 2 {
			t.Errorf("%v: rc=%d, want 2 (%s)", args, rc, e.String())
		}
	}
}

// context "the stub is a door that follows the trusted-key list": a key removed from the list must stop
// working in a RUNNING stub, with no restart. This is the wiring test: delete the refresher goroutine or
// the ring source and it goes red.
func TestStubFollowsTheListAndDropsARemovedKey(t *testing.T) {
	dir := t.TempDir()
	os.Chmod(dir, 0o700)
	type mk struct {
		k    *token.Key
		m    string
		file string
	}
	var ms []mk
	for _, m := range []string{"mac", "hp-ubuntu"} {
		k, err := token.LoadOrCreateKeyFile(filepath.Join(t.TempDir(), "k"))
		if err != nil {
			t.Fatal(err)
		}
		e := machines.Entry{Machine: m, Owners: []string{"tomas"}, MaySignMachines: []string{m},
			PublicKeys: []machines.KeyRef{{KID: k.KID, Key: base64.StdEncoding.EncodeToString(k.Public)}},
			MaxProfile: []string{"e2e-*"}, MaxBilling: []string{"stub"}, Pairs: map[string][]string{"e2e-default": {"stub"}}}
		f := filepath.Join(dir, m+".json")
		if err := os.WriteFile(f, []byte(machines.Canonical(&e)), 0o644); err != nil {
			t.Fatal(err)
		}
		ms = append(ms, mk{k, m, f})
	}
	const stale = 300 * time.Millisecond
	v, err := Verifier("", dir, DefaultAud, 20*time.Millisecond, stale, nil)
	if err != nil {
		t.Fatal(err)
	}
	g, _ := token.NewGenKey()
	call := func(x mk, gen int) error {
		now := time.Now()
		tok, err := token.Mint(x.k, token.Claims{Sub: fmt.Sprintf("tomas/%s/sS1/g%d", x.m, gen), Label: "l", Billing: "stub", Profile: "e2e-default",
			Aud: []string{DefaultAud}, Cnf: token.CnfOf(g.Public)}, now)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := token.SignProof(g, "POST", "/x", DefaultAud, tok, nil, now)
		_, err = v.Verify(tok, p, "POST", "/x", nil, now)
		return err
	}
	for _, x := range ms {
		if err := call(x, 1); err != nil {
			t.Fatalf("allow twin: a listed key of %s: %v", x.m, err)
		}
	}
	os.Remove(ms[0].file) // one machine leaves; the other file is still there, so the load keeps succeeding
	gone := false
	for i := 2; i < 400 && !gone; i++ {
		gone = errors.Is(call(ms[0], i), token.ErrUnknownKID)
		time.Sleep(10 * time.Millisecond)
	}
	if !gone {
		t.Fatal("the removed key still verifies in the running stub")
	}
	// Past the stale limit the other key must still verify: that only holds while the refresher
	// goroutine keeps running (a stub that never refreshes would lose ALL keys at the stale limit).
	time.Sleep(3 * stale)
	if err := call(ms[1], 9); err != nil {
		t.Fatalf("the remaining key stopped verifying past the stale limit: %v", err)
	}
}

func TestStubRefusesBadListTimings(t *testing.T) {
	dir := t.TempDir()
	if _, err := Verifier("", dir, DefaultAud, time.Second, time.Millisecond, nil); err == nil || !strings.Contains(err.Error(), "at least the poll") {
		t.Fatalf("a stale limit under the poll: %v", err)
	}
}
