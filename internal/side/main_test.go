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
	"strconv"
	"strings"
	"syscall"
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

func serveBase(t *testing.T) ServeOptions {
	u, _ := url.Parse("http://127.0.0.1:1")
	return ServeOptions{Config: Config{Listen: "127.0.0.1:18081", Control: filepath.Join(t.TempDir(), "ctl", "ctl.sock"), Doors: map[string]*url.URL{"model": u}},
		RelayEvery: time.Second, FenceWritable: DefaultFenceWritable}
}

func serveCode(o ServeOptions) (int, string) {
	var errb bytes.Buffer
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second) // a refusal that regresses ends the run (0)
	defer cancel()
	return runServe(ctx, o, &errb), errb.String()
}

// context "the launcher points the sidecar's state at a mount the fence writes (a typo for /work/.side, or a
// routes file in the outbox)": the ledger, the journal, the routes allowlist and the CA decide what the fence may
// do, so serve refuses every one of them there, however the path is spelled or linked.
func TestServeRefusesStateWhereTheFenceCanWrite(t *testing.T) {
	fw := t.TempDir()
	for name, mut := range map[string]func(*ServeOptions){
		"state":   func(o *ServeOptions) { o.State = filepath.Join(fw, "state") },
		"journal": func(o *ServeOptions) { o.JournalDir = filepath.Join(fw, "j"); o.Sid, o.Gen = "s", 1 },
		"routes": func(o *ServeOptions) {
			os.WriteFile(filepath.Join(fw, "r.json"), []byte("[]"), 0o600)
			o.RoutesFile = filepath.Join(fw, "r.json")
		},
		"ca":      func(o *ServeOptions) { o.Config.CAFile = filepath.Join(fw, "ca.pem") },
		"control": func(o *ServeOptions) { o.Config.Control = filepath.Join(fw, "ctl", "ctl.sock") },
		"dotdot":  func(o *ServeOptions) { o.State = filepath.Join(fw, "x", "..", "state") },
		"symlinked in": func(o *ServeOptions) {
			link := filepath.Join(t.TempDir(), "link")
			os.Symlink(fw, link)
			o.State = filepath.Join(link, "state")
		},
	} {
		o := serveBase(t)
		o.FenceWritable = []string{fw}
		mut(&o)
		if code, msg := serveCode(o); code != 2 || !strings.Contains(msg, "fence can write") {
			t.Errorf("%s: exit %d (%s)", name, code, msg)
		}
	}
}

// context "state dirs that others could write or swap": the ledger and journal directories get the control
// directory's bar (ours, 0700, not a link), and the routes and CA files must not be writable by others.
func TestServeRequiresItsStateToBePrivate(t *testing.T) {
	root := t.TempDir()
	open := filepath.Join(root, "open")
	os.Mkdir(open, 0o755)
	target := filepath.Join(root, "t")
	os.Mkdir(target, 0o700)
	link := filepath.Join(root, "link")
	os.Symlink(target, link)
	routes := filepath.Join(root, "r.json")
	os.WriteFile(routes, []byte("[]"), 0o666)
	os.Chmod(routes, 0o666)
	for name, mut := range map[string]func(*ServeOptions){
		"state mode 0755":    func(o *ServeOptions) { o.State = open },
		"journal mode 0755":  func(o *ServeOptions) { o.JournalDir = open; o.Sid, o.Gen = "s", 1 },
		"state is a link":    func(o *ServeOptions) { o.State = link },
		"routes world-write": func(o *ServeOptions) { o.RoutesFile = routes },
		"ca missing":         func(o *ServeOptions) { o.Config.CAFile = filepath.Join(root, "nope.pem") },
	} {
		o := serveBase(t)
		mut(&o)
		if code, msg := serveCode(o); code != 2 {
			t.Errorf("%s: exit %d (%s)", name, code, msg)
		}
	}
}

// context "in a pod every volume root is 0777 and root-owned": the sidecar makes its own 0700 directory one
// level below the mount, and a second start finds it again.
func TestServeMakesItsStateDirectoriesUnderAnOpenVolumeRoot(t *testing.T) {
	vol := t.TempDir()
	os.Chmod(vol, 0o777)
	for i := 0; i < 2; i++ {
		d := filepath.Join(vol, "state")
		if err := ownDir(d, os.Getuid()); err != nil {
			t.Fatalf("start %d: %v", i, err)
		}
		if fi, _ := os.Stat(d); fi.Mode().Perm() != 0o700 {
			t.Fatalf("mode %o", fi.Mode().Perm())
		}
	}
}

// context "a launcher typo that turns the whole bus into a silent no-op": a zero scan interval would panic the
// relay, an unparseable hub URL would make every send 'undeliverable', and a manager placed on the pod's own
// machine would answer 'no live session' forever, so each is refused at start.
func TestServeRefusesMisconfigurationsThatWouldFailSilently(t *testing.T) {
	root := t.TempDir()
	wire := func(o *ServeOptions) {
		o.Out, o.In, o.State = filepath.Join(root, "out"), filepath.Join(root, "in"), filepath.Join(root, "state")
		os.MkdirAll(filepath.Join(o.Out, "requests"), 0o700) // real directories, so only the option under test can refuse
		os.MkdirAll(o.In, 0o700)
		o.Sid, o.Gen, o.Machine = "s", 1, "hp"
		o.FenceUID = os.Getuid()
		o.Scope = `{"manager":{"node":"m","machine":"mac"}}`
	}
	for name, mut := range map[string]func(*ServeOptions){
		"relay-every 0":        func(o *ServeOptions) { o.RelayEvery = 0 },
		"relay-every negative": func(o *ServeOptions) { o.RelayEvery = -time.Second },
		"hub url no scheme":    func(o *ServeOptions) { o.HubURL = "hub:4011" },
		"hub url ftp":          func(o *ServeOptions) { o.HubURL = "ftp://hub" },
		"hub url credentials":  func(o *ServeOptions) { o.HubURL = "http://u:p@hub:4011" },
		"manager on own machine": func(o *ServeOptions) {
			o.HubURL = "http://hub:4011"
			o.Scope = `{"manager":{"node":"m","machine":"hp"}}`
		},
	} {
		o := serveBase(t)
		wire(&o)
		if name == "relay-every 0" || name == "relay-every negative" {
			o.RelayEvery = 0
		} else {
			o.RelayEvery = time.Second
			o.HubURL = "http://hub:4011"
		}
		mut(&o)
		if code, msg := serveCode(o); code != 2 {
			t.Errorf("%s: exit %d (%s)", name, code, msg)
		}
	}
}

// context "the relay's last word is written while serve is shutting down": a flood past the bucket is shed, the
// process is cancelled, and the journal must hold the final relay.shed with the lifetime total, which exists only
// if serve waited for the relay before closing the journal.
func TestServeWritesTheRelaysFinalShedSummaryBeforeItEnds(t *testing.T) {
	root, err := os.MkdirTemp("", "sideshed")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(root)
	os.Chmod(root, 0o700)
	out, in, state, jd := filepath.Join(root, "out"), filepath.Join(root, "in"), filepath.Join(root, "state"), filepath.Join(root, "journal")
	os.MkdirAll(filepath.Join(out, "requests"), 0o700)
	os.MkdirAll(in, 0o700)
	for i := 0; i < 60; i++ { // fixed: 20 tokens, 20 shed with a reply, about 20 shed silently
		os.WriteFile(filepath.Join(out, "requests", "f"+strconv.Itoa(i)+".json"), []byte(`{"type":"msg.send"}`), 0o600)
	}
	o := serveBase(t)
	o.Sid, o.Gen, o.Machine = "s0worker", 1, "hp"
	o.JournalDir, o.Out, o.In, o.State, o.FenceUID, o.RelayEvery = jd, out, in, state, os.Getuid(), 20*time.Millisecond
	o.wrapJournal = func(a Appender) Appender { return slowShed{a} } // the relay's last word takes 300 ms
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan int, 1)
	go func() { done <- runServe(ctx, o, io.Discard) }()
	// the silent sheds leave no reply, only a ledger line, so wait until all 60 are ledgered: a cancel in the
	// middle of the scan would skip them and there would be nothing to summarise
	for i := 0; i < 400; i++ {
		if b, _ := os.ReadFile(filepath.Join(state, "relay-ledger.jsonl")); bytes.Count(b, []byte("\n")) >= 60 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	if code := <-done; code != 0 {
		t.Fatalf("exit %d", code)
	}
	js, err := journal.Open(jd, "hp", 0)
	if err != nil {
		t.Fatal(err)
	}
	defer js.Close()
	recs, _ := js.After(0, js.Epoch(), "", 200)
	var last string
	for _, r := range recs {
		if r.Type == "relay.shed" {
			last = string(r.Data)
		}
	}
	var d struct {
		ShedTotal int `json:"shed_total"`
	}
	if json.Unmarshal([]byte(last), &d) != nil || d.ShedTotal < 15 || d.ShedTotal > 20 {
		t.Fatalf("the journal's last relay.shed is %q, want a lifetime total of about 20", last)
	}
}

// slowShed delays the relay.shed write so that a serve which does not wait for the relay closes the journal first.
type slowShed struct{ Appender }

func (s slowShed) Append(e journal.Event) (journal.Record, error) {
	if e.Type == "relay.shed" {
		time.Sleep(300 * time.Millisecond)
	}
	return s.Appender.Append(e)
}

// context "a launcher passes --state /work/s, the commonest mistake": through the real flag parser, with the
// DEFAULT fence-writable list (nothing injected), serve exits 2 and says why, before it creates anything.
func TestMainRefusesStateUnderWorkWithTheDefaultList(t *testing.T) {
	ctl := filepath.Join(t.TempDir(), "ctl", "ctl.sock")
	for _, flag := range []string{"--state", "--journal-dir"} {
		var errb bytes.Buffer
		args := []string{"serve", "--control", ctl, "--door", "model=http://127.0.0.1:1", "--sid", "s", "--gen", "1", flag, "/work/s"}
		if code := Main(args, nil, io.Discard, &errb); code != 2 || !strings.Contains(errb.String(), "fence can write") {
			t.Errorf("%s /work/s: exit %d (%s)", flag, code, errb.String())
		}
	}
	if _, err := os.Stat("/work/s"); err == nil {
		t.Error("serve created /work/s")
	}
}

// context "a path with a '..' or a symlink in it is a different place by the time it is used": each of the five
// paths must already be absolute and clean with no link in it, or serve refuses to start.
func TestServeRefusesAPathThatIsNotCleanOrHasALinkInIt(t *testing.T) {
	root := t.TempDir()
	real := filepath.Join(root, "real")
	os.Mkdir(real, 0o700)
	link := filepath.Join(root, "link")
	os.Symlink(real, link)
	file := filepath.Join(real, "f")
	os.WriteFile(file, []byte("[]"), 0o600)
	paths := map[string]func(o *ServeOptions, p string){
		"--control":     func(o *ServeOptions, p string) { o.Config.Control = p + "/ctl/ctl.sock" },
		"--state":       func(o *ServeOptions, p string) { o.State = p + "/state" },
		"--journal-dir": func(o *ServeOptions, p string) { o.JournalDir = p + "/j"; o.Sid, o.Gen = "s", 1 },
		"--routes":      func(o *ServeOptions, p string) { o.RoutesFile = p + "/f" },
		"--ca-file":     func(o *ServeOptions, p string) { o.Config.CAFile = p + "/f" },
	}
	for name, set := range paths {
		for kind, dir := range map[string]string{"symlink": link, "dotdot": real + "/../real", "relative": "rel"} {
			o := serveBase(t)
			o.FenceWritable = []string{filepath.Join(root, "elsewhere")}
			set(&o, dir)
			if code, msg := serveCode(o); code != 2 || !strings.Contains(msg, "clean") && kind != "relative" {
				t.Errorf("%s via %s: exit %d (%s)", name, kind, code, msg)
			}
		}
	}
}

// context "the routes allowlist and the CA decide what the fence may reach": they must be real files that only
// their owner can write, owned by the sidecar's uid or root.
func TestTrustedFileChecksTypeModeAndOwner(t *testing.T) {
	d := t.TempDir()
	me := os.Getuid()
	good := filepath.Join(d, "good")
	os.WriteFile(good, []byte("x"), 0o640)
	if err := trustedFile(good, me); err != nil {
		t.Fatalf("a plain file of ours: %v", err)
	}
	dir := filepath.Join(d, "dir")
	os.Mkdir(dir, 0o700)
	link := filepath.Join(d, "link")
	os.Symlink(good, link)
	gw := filepath.Join(d, "gw")
	os.WriteFile(gw, []byte("x"), 0o600)
	os.Chmod(gw, 0o620)
	ow := filepath.Join(d, "ow")
	os.WriteFile(ow, []byte("x"), 0o600)
	os.Chmod(ow, 0o602)
	fifo := filepath.Join(d, "fifo")
	syscall.Mkfifo(fifo, 0o600)
	for name, p := range map[string]string{"a directory": dir, "a symlink": link, "group-writable": gw, "other-writable": ow, "a fifo": fifo, "missing": filepath.Join(d, "nope")} {
		if err := trustedFile(p, me); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	// the owner check: this file is ours, so asked for as another uid's (and not root's) it is refused
	if err := trustedFile(good, me+4242); err == nil {
		t.Error("a file owned by neither the sidecar's uid nor root accepted")
	}
}
