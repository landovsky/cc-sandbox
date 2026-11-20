// Tests of `side serve` wiring. Why: each part (forwarder, journal, relay, bus) is tested alone; what can still
// go wrong is how they are put together, so these run the real serve loop and refuse the combinations that
// would start a half-wired sidecar.

package side

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/stamp"
	"github.com/landovsky/cc-sandbox/internal/token"
)

const relayTok = "sbp1.relay-cred-0001"

func TestScopeMustPlaceEveryNode(t *testing.T) {
	good := `{"manager":{"node":"cc-sandbox:mgr","machine":"mac"},"peers":["cc-sandbox:p"],"peer_machines":{"cc-sandbox:p":"mac2"}}`
	if _, err := parseScope(good); err != nil {
		t.Fatalf("a placed scope: %v", err)
	}
	for name, raw := range map[string]string{
		"empty":                "",
		"unplaced manager":     `{"manager":{"node":"cc-sandbox:mgr"}}`,
		"unplaced peer":        `{"manager":{"node":"m","machine":"mac"},"peers":["cc-sandbox:p"]}`,
		"machine with a slash": `{"manager":{"node":"m","machine":"a/b"}}`,
		"unknown field":        `{"manager":{"node":"m","machine":"mac"},"admin":true}`,
		"not json":             `nope`,
	} {
		if _, err := parseScope(raw); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

// context "a half-wired sidecar would look healthy and drop the fence's messages": every combination that
// leaves one part without what it needs is refused at start with exit 2, before anything listens.
func TestServeRefusesAHalfWiredConfiguration(t *testing.T) {
	scope := `{"manager":{"node":"m","machine":"mac"}}`
	u, _ := url.Parse("http://127.0.0.1:1")
	base := Config{Listen: "127.0.0.1:18080", Control: filepath.Join(t.TempDir(), "ctl", "ctl.sock"), Doors: map[string]*url.URL{"model": u}}
	for name, o := range map[string]ServeOptions{
		"hub without relay":    {HubURL: "http://h", Scope: scope, Machine: "hp", Sid: "s"},
		"hub without scope":    {HubURL: "http://h", Out: "/o", In: "/i", State: "/s", Machine: "hp", Sid: "s", Gen: 1},
		"relay without state":  {Out: "/o", In: "/i", Sid: "s", Gen: 1},
		"relay without gen":    {Out: "/o", In: "/i", State: "/s", Sid: "s"},
		"journal without sid":  {JournalDir: "/j"},
		"journal without gen":  {JournalDir: "/j", Sid: "s"},
		"unplaced scope + hub": {HubURL: "http://h", Scope: `{"manager":{"node":"m"}}`, Out: "/o", In: "/i", State: "/s", Machine: "hp", Sid: "s", Gen: 1},
	} {
		o.Config = base // valid, so only the wiring can be what refuses
		var errb bytes.Buffer
		// a deadline, so a refusal that regresses ends the run (exit 0) and fails here instead of serving forever
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		code := runServe(ctx, o, &errb)
		cancel()
		if code != 2 {
			t.Errorf("%s: exit %d (%s)", name, code, errb.String())
		}
	}
}

func TestUsageNamesTheNewFlags(t *testing.T) {
	var out bytes.Buffer
	Main([]string{"--help"}, nil, &out, io.Discard)
	for _, f := range []string{"--sid", "--scope", "--journal-dir", "--out", "--hub-url", "--fence-uid", "relay_token"} {
		if !strings.Contains(out.String(), f) {
			t.Errorf("--help does not mention %s", f)
		}
	}
}

// context "the whole sidecar, as the pod runs it": load the three secrets, drop a msg.send in the outbox, and the
// manager's machine gets a message signed with the generation key while the fence reads an ok reply and the
// session journal holds the lifecycle and the decision; ending the process writes the journal's last word.
func TestServeRunsTheForwarderTheRelayTheBusAndTheJournalTogether(t *testing.T) {
	hub := newFakeHub(t)
	hub.entries = []map[string]any{hubEntry("m1", "cc-sandbox:mgr", "mac", false)}
	door := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write([]byte("ok")) }))
	defer door.Close()
	du, _ := url.Parse(door.URL)

	root, err := os.MkdirTemp("", "sideserve")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	os.Chmod(root, 0o700)
	out, in, state, jd := filepath.Join(root, "out"), filepath.Join(root, "in"), filepath.Join(root, "state"), filepath.Join(root, "journal")
	for _, d := range []string{out, in, state, filepath.Join(out, "requests")} {
		os.MkdirAll(d, 0o700)
	}
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	listen := l.Addr().String()
	l.Close()
	ctl := filepath.Join(root, "ctl", "ctl.sock")

	k, err := token.LoadOrCreateKeyFile(filepath.Join(root, "key"))
	if err != nil {
		t.Fatal(err)
	}
	seed := bytes.Repeat([]byte{7}, ed25519.SeedSize)
	priv := ed25519.NewKeyFromSeed(seed)
	pubB64, _, _ := stamp.NewKey()
	_ = pubB64
	gk := &token.GenKey{Private: priv, Public: priv.Public().(ed25519.PublicKey)}
	now := time.Now()
	tok, err := token.Mint(k, token.Claims{Sub: "tomas/hp/s0worker/g1", Label: "l", Billing: "api", Profile: "dev-default",
		Aud: []string{"ccsbx-model"}, Cnf: token.CnfOf(gk.Public), Exp: now.Add(time.Hour).Unix()}, now)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(map[string]string{"token": tok, "key": base64.StdEncoding.EncodeToString(seed), "relay_token": relayTok})

	o := ServeOptions{
		Config: Config{Listen: listen, Control: ctl, Doors: map[string]*url.URL{"model": du}, Logf: func(string, ...any) {}},
		Sid:    "s0worker", Gen: 1, Label: "cc-sandbox:w", FullID: "tomas/hp/s0worker/g1", Machine: "hp",
		Scope:      `{"manager":{"node":"cc-sandbox:mgr","machine":"mac"}}`,
		JournalDir: jd, Out: out, In: in, State: state, HubURL: hub.URL, FenceUID: os.Getuid(), RelayEvery: 20 * time.Millisecond,
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runServe(ctx, o, io.Discard) }()
	for i := 0; i < 200; i++ {
		if c, err := net.Dial("unix", ctl); err == nil {
			c.Close()
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := Load(ctl, bytes.NewReader(payload)); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(out, "requests", "m.json"), []byte(`{"type":"msg.send","to":{"node":"cc-sandbox:mgr"},"text":"hello"}`), 0o600)
	var rep struct{ Status string }
	for i := 0; i < 300; i++ {
		if b, err := os.ReadFile(filepath.Join(in, "replies", "m.json")); err == nil && json.Unmarshal(b, &rep) == nil {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if rep.Status != "ok" || hub.deliverCalls != 1 || hub.auth != "Bearer "+relayTok {
		t.Fatalf("reply %+v, %d deliveries, auth %q", rep, hub.deliverCalls, hub.auth)
	}
	cancel()
	select {
	case code := <-done:
		if code != 0 {
			t.Fatalf("exit %d", code)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("serve did not end")
	}
	js, err := journal.Open(jd, "hp", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer js.Close()
	recs, _ := js.After(0, js.Epoch(), "", 50)
	got := map[string]bool{}
	for _, r := range recs {
		got[r.Type] = true
		if strings.Contains(string(r.Data), "hello") || strings.Contains(string(r.Data), "relay-cred") {
			t.Errorf("journal record leaks: %s %s", r.Type, r.Data)
		}
	}
	if !got["fwd.loaded"] || !got["relay.forwarded"] {
		t.Fatalf("journal types %v", got)
	}
}
