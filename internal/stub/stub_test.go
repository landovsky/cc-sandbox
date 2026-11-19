package stub

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/fwd"
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
		Aud: []string{"ccsbx-model"}, Cnf: token.CnfOf(gen.Public)}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	v := token.NewVerifier(token.Keyring{key.KID: key.Public}, "ccsbx-model")
	door := httptest.NewServer(Gate(v, Handler(), t.Logf))
	t.Cleanup(door.Close)
	du, _ := url.Parse(door.URL)
	f, err := fwd.New(fwd.Config{Token: tok, Key: gen, Doors: map[string]*url.URL{"model": du}})
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
	resp, err := http.Post(c.fwdURL+"/model/v1/messages", "application/json", strings.NewReader(body))
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
		{"dev-default", "stub"}, {"e2e-basic", "api"}, {"e2e-basic", "subscription"}, {"", "stub"}, {"xe2e-basic", "stub"},
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
	if Main([]string{"--listen", "127.0.0.1:0", "--key-file", filepath.Join(t.TempDir(), "absent")}, &out, &errb) != 2 {
		t.Fatal("a missing key file must not be created by a verifier")
	}
}
