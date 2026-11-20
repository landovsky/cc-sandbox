// Tests of the hostd handlers. Why: they are where an untrusted fence request turns into a message,
// a queued verb or a journal record, so each case names the abuse it is meant to survive.

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/start"
	"github.com/landovsky/cc-sandbox/internal/verbq"
	"github.com/landovsky/cc-sandbox/profiles"
)

// context: the whole M4 path in one go — a worker's `done` while no cockpit is up is answered
// "queued #1" in its inbox; when a cockpit takes the lease and applies it, the final result
// replaces that reply, so the session sees what happened.
func TestDoneIsQueuedThenAnsweredWhenTheCockpitApplies(t *testing.T) {
	state := t.TempDir()
	const sid = "s0queue1"
	rec := start.Record{Sid: sid, Gen: 1, Label: "cc-sandbox:w", Spec: start.LaunchSpec{Label: "cc-sandbox:w", Realm: "production"}, Reply: start.StartReply{ID: "tomas/hp/" + sid + "/g1"}}
	os.MkdirAll(filepath.Join(state, "sessions"), 0o700)
	b, _ := json.Marshal(rec)
	os.WriteFile(filepath.Join(state, "sessions", sid+".json"), b, 0o600)
	g := filepath.Join(state, "s", sid, "g1")
	os.MkdirAll(filepath.Join(g, "out", "requests"), 0o755)
	os.MkdirAll(filepath.Join(g, "in", "replies"), 0o755)
	os.WriteFile(filepath.Join(g, "out", "requests", "d1.json"), []byte(`{"type":"ccf.done"}`), 0o644)

	d := &start.Daemon{StateDir: state}
	q, err := verbq.Open(filepath.Join(state, "queue"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	q.Reply = replyWriter(state, d)
	m := &mux{}
	m.on("ccf.", verbHandler(q, d, nil, nil))
	h := hostd.New(hostd.Config{StateDir: state, FenceUID: os.Getuid(), Handler: m.handle})
	h.Scan()

	read := func() hostd.Reply {
		var r hostd.Reply
		b, err := os.ReadFile(filepath.Join(g, "in", "replies", "d1.json"))
		if err != nil {
			t.Fatal(err)
		}
		json.Unmarshal(b, &r)
		return r
	}
	if r := read(); r.Status != "queued" || r.Seq != 1 {
		t.Fatalf("%+v", r)
	}
	l, _ := q.Acquire("production", "hp", "", false)
	items, _ := q.Pull("production", l.Token, 0)
	if len(items) != 1 || items[0].Node != "cc-sandbox:w" || items[0].Type != "ccf.done" {
		t.Fatalf("%+v", items)
	}
	if w, err := q.Report("production", l.Token, items[0].ID, verbq.Result{Status: "ok"}); err != nil || !w {
		t.Fatalf("%v %v", w, err)
	}
	if r := read(); r.Status != "ok" {
		t.Fatalf("final result not delivered: %+v", r)
	}
}

// context: the generation ended before the cockpit applied the verb; the result can't go to the
// session's inbox, and the daemon says so, so the cockpit writes it into the node's Outcome.
func TestResultForAnEndedGenerationIsReportedUnwritten(t *testing.T) {
	state := t.TempDir()
	d := &start.Daemon{StateDir: state}
	os.MkdirAll(filepath.Join(state, "sessions"), 0o700)
	rec := start.Record{Sid: "s0gone001", Gen: 2, Spec: start.LaunchSpec{Label: "x"}}
	b, _ := json.Marshal(rec)
	os.WriteFile(filepath.Join(state, "sessions", "s0gone001.json"), b, 0o600)
	if replyWriter(state, d)(verbq.Item{Sid: "s0gone001", Gen: 1, Rid: "r"}, verbq.Result{Status: "ok"}) {
		t.Fatal("wrote into a dead generation's inbox")
	}
}

type recOut struct{ lines map[string][]string }

func (r *recOut) Deliver(sid, line string) error {
	if r.lines == nil {
		r.lines = map[string][]string{}
	}
	r.lines[sid] = append(r.lines[sid], line)
	return nil
}

type recHub struct{ got []string }

func (h *recHub) Send(node, line string) error { h.got = append(h.got, node+" <- "+line); return nil }

type alwaysIdle struct{}

func (alwaysIdle) IdleSince(string, int) (time.Time, bool) { return time.Now().Add(-time.Hour), true }

func writeSession(t *testing.T, state, sid, label, manager string) {
	scope := json.RawMessage(`{"manager":{"node":"` + manager + `"},"peers":[],"reply_window_s":86400}`)
	rec := start.Record{Sid: sid, Gen: 1, Label: label, Started: time.Now(), Spec: start.LaunchSpec{Label: label, Realm: "production", MsgScope: scope},
		Reply: start.StartReply{ID: "tomas/hp/" + sid + "/g1"}}
	os.MkdirAll(filepath.Join(state, "sessions"), 0o700)
	b, _ := json.Marshal(rec)
	os.WriteFile(filepath.Join(state, "sessions", sid+".json"), b, 0o600)
}

func askQuestion(t *testing.T, state, sid string) {
	g := filepath.Join(state, "s", sid, "g1")
	os.MkdirAll(filepath.Join(g, "out", "requests"), 0o755)
	os.MkdirAll(filepath.Join(g, "in", "replies"), 0o755)
	os.WriteFile(filepath.Join(g, "out", "requests", "q1.json"), []byte(`{"type":"ccf.question","data":{"text":"Which database?","options":["pg","sqlite"]}}`), 0o644)
}

// context: C23 — the cockpit is down and a worker asks Tomas a question. The question is queued
// for the tree (the cockpit applies it when it is back) AND the manager in scope gets it now, by
// message, so nothing waits on the cockpit.
func TestQuestionIsQueuedAndTheManagerGetsItNowAllow(t *testing.T) {
	state := t.TempDir()
	writeSession(t, state, "s0worker1", "cc-sandbox:w1", "cc-sandbox:mgr")
	writeSession(t, state, "s0mgr0001", "cc-sandbox:mgr", "")
	askQuestion(t, state, "s0worker1")
	d := &start.Daemon{StateDir: state}
	out := &recOut{}
	rt := &msg.Router{Dir: msg.DaemonDir{D: d, Alive: func(string) bool { return true }}, Out: out, Idle: alwaysIdle{}, StateDir: state}
	q, _ := verbq.Open(filepath.Join(state, "queue"))
	defer q.Close()
	m := &mux{}
	m.on("ccf.", verbHandler(q, d, nil, rt))
	hostd.New(hostd.Config{StateDir: state, FenceUID: os.Getuid(), Handler: m.handle}).Scan()
	if st, _ := q.Status("production"); st.Pending != 1 {
		t.Fatalf("not queued for the tree: %+v", st)
	}
	got := out.lines["s0mgr0001"]
	if len(got) != 1 || !strings.Contains(got[0], "QUESTION: Which database? (options: pg | sqlite)") || !strings.Contains(got[0], "· fenced ·") {
		t.Fatalf("the manager did not get it: %q", got)
	}
}

// context: a manager is always an unfenced session on the host, so it is no ccsbx session. The
// question reaches it through the session bus (ccmsg), stamped as the daemon's.
func TestQuestionReachesAnUnfencedManagerThroughTheHub(t *testing.T) {
	state := t.TempDir()
	writeSession(t, state, "s0worker1", "cc-sandbox:w1", "cc-sandbox:mgr")
	askQuestion(t, state, "s0worker1")
	d := &start.Daemon{StateDir: state}
	hub := &recHub{}
	rt := &msg.Router{Dir: msg.DaemonDir{D: d, Alive: func(string) bool { return true }}, Out: &recOut{}, Idle: alwaysIdle{}, Hub: hub, StateDir: state}
	q, _ := verbq.Open(filepath.Join(state, "queue"))
	defer q.Close()
	m := &mux{}
	m.on("ccf.", verbHandler(q, d, nil, rt))
	hostd.New(hostd.Config{StateDir: state, FenceUID: os.Getuid(), Handler: m.handle}).Scan()
	if len(hub.got) != 1 || !strings.HasPrefix(hub.got[0], "cc-sandbox:mgr <- [MSG from ccsbx:s0worker1 · cc-sandbox:w1 · fenced · #") {
		t.Fatalf("%q", hub.got)
	}
}

// context: other verbs (outcome, done, ...) are queued only; just `question` is dual.
func TestOtherVerbsDoNotMessageTheManager(t *testing.T) {
	state := t.TempDir()
	writeSession(t, state, "s0worker1", "cc-sandbox:w1", "cc-sandbox:mgr")
	g := filepath.Join(state, "s", "s0worker1", "g1")
	os.MkdirAll(filepath.Join(g, "out", "requests"), 0o755)
	os.MkdirAll(filepath.Join(g, "in", "replies"), 0o755)
	os.WriteFile(filepath.Join(g, "out", "requests", "o1.json"), []byte(`{"type":"ccf.outcome","data":{"text":"x"}}`), 0o644)
	d := &start.Daemon{StateDir: state}
	hub := &recHub{}
	rt := &msg.Router{Dir: msg.DaemonDir{D: d, Alive: func(string) bool { return true }}, Out: &recOut{}, Idle: alwaysIdle{}, Hub: hub, StateDir: state}
	q, _ := verbq.Open(filepath.Join(state, "queue"))
	defer q.Close()
	m := &mux{}
	m.on("ccf.", verbHandler(q, d, nil, rt))
	hostd.New(hostd.Config{StateDir: state, FenceUID: os.Getuid(), Handler: m.handle}).Scan()
	if len(hub.got) != 0 {
		t.Fatalf("%q", hub.got)
	}
}

// context: hear — a worker that stops at a permission prompt is something its manager should
// hear about even when the cockpit is down; a quiet Stop is not.
func TestHearTellsTheManagerOfAPermissionPrompt(t *testing.T) {
	state := t.TempDir()
	writeSession(t, state, "s0worker1", "cc-sandbox:w1", "cc-sandbox:mgr")
	hub := &recHub{}
	d := &start.Daemon{StateDir: state}
	rt := &msg.Router{Dir: msg.DaemonDir{D: d, Alive: func(string) bool { return true }}, Out: &recOut{}, Idle: alwaysIdle{}, Hub: hub, StateDir: state}
	hear := hearFunc(rt)
	hear(journal.Record{Type: "hook.Stop", Sid: "s0worker1", Node: "cc-sandbox:w1", Data: json.RawMessage(`{}`)})
	hear(journal.Record{Type: "hook.Notification", Sid: "s0worker1", Node: "cc-sandbox:w1", Data: json.RawMessage(`{"notification_type":"idle_prompt"}`)})
	if len(hub.got) != 0 {
		t.Fatalf("heard nothing worth hearing: %q", hub.got)
	}
	hear(journal.Record{Type: "hook.Notification", Sid: "s0worker1", Node: "cc-sandbox:w1", Data: json.RawMessage(`{"notification_type":"permission_prompt"}`)})
	if len(hub.got) != 1 || !strings.Contains(hub.got[0], "HEAR: cc-sandbox:w1 needs a decision from a human (permission_prompt)") {
		t.Fatalf("%q", hub.got)
	}
}

// ---- review of PR #12: the start handler (owner from the peer, who may start) ---------------------

// wdir makes a real worktree directory under /var/tmp (/tmp is a fixed fence mount, so refused).
func wdir(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/var/tmp", "ccsbx-hostd-wd-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	d, _ = filepath.EvalSymlinks(d)
	return d
}

func policyFor(t *testing.T, lookup string) (*start.Daemon, startPolicy) {
	t.Helper()
	sd := &start.Daemon{StateDir: t.TempDir(), Machine: "hp-ubuntu", Root: "direct", Exe: "ccsbx", Backend: "docker", Launcher: start.NoLauncher{}, ProfileLoader: profiles.Load}
	return sd, startPolicy{Pins: "/nonexistent/pins.toml", WorktreeRoots: "/var/tmp", Image: "ccsbx-fence:dev",
		MaxLive: 8, MaxPerHour: 20, MinUID: 1000,
		LookupID: func(id string) (*user.User, error) { return &user.User{Uid: id, Username: lookup}, nil }}
}

const specOf = `{"v":1,"request_id":"r1","owner":"%s","machine":"hp-ubuntu","label":"cc-sandbox:x","harness":"claude","profile":"dev-default","billing":"stub","workdir":{"path":"%s"}}`

// context: the owner is in the session's id (journal and token identity). Through the REAL handler
// (not the function alone), a spec naming another owner is refused with the owner message and never
// reaches Start; deleting the CheckOwner call from the handler turns this red, because the spec would
// then fail later for another reason.
func TestStartHandlerRefusesASpecWithAnotherOwner(t *testing.T) {
	w := wdir(t)
	sd, p := policyFor(t, "tomas")
	h := startHandler(sd, p)
	_, err := h(4000, []byte(fmt.Sprintf(specOf, "root", w)))
	if r, ok := err.(*start.Refusal); !ok || !strings.Contains(r.Msg, "is not the caller") {
		t.Fatalf("a foreign owner was not refused as such: %v", err)
	}
	_, err = h(4000, []byte(fmt.Sprintf(specOf, "tomas", w)))
	if err != nil && strings.Contains(err.Error(), "is not the caller") {
		t.Fatalf("the caller's own name was refused: %v", err)
	}
}

// context: root, the service user and low uids can't start sessions: ccsbx-root would refuse the pane
// anyway, but only after hostd had held the label and a live slot.
func TestStartHandlerRefusesRootTheServiceUserAndLowUids(t *testing.T) {
	w := wdir(t)
	sd, p := policyFor(t, "tomas")
	h := startHandler(sd, p)
	for name, uid := range map[string]int{"root": 0, "the service user (hostd's own uid)": os.Getuid(), "below min-uid": 999} {
		if uid != 0 && uid != os.Getuid() && uid >= p.MinUID {
			continue
		}
		if _, err := h(uid, []byte(fmt.Sprintf(specOf, "tomas", w))); err == nil || !strings.Contains(err.Error(), "may not start sessions") {
			t.Errorf("%s: %v", name, err)
		}
	}
}

// context: hostd passes --worktree-root through unchanged, so a list ccsbx-root would refuse (a
// relative entry, a trailing slash) is refused by hostd too instead of being silently repaired.
func TestStartHandlerDoesNotRepairTheWorktreeRoots(t *testing.T) {
	w := wdir(t)
	sd, p := policyFor(t, "tomas")
	p.WorktreeRoots = "/var/tmp/,wt"
	_, err := startHandler(sd, p)(4000, []byte(fmt.Sprintf(specOf, "tomas", w)))
	if err == nil || !strings.Contains(err.Error(), "worktree root") {
		t.Fatalf("a list that ccsbx-root refuses was repaired and accepted: %v", err)
	}
}

// context (round 6): a start spec's msg_scope says where its manager and peers live, and hostd publishes that to
// the hub as a binding of a node to a machine. A typo, or a spec trying to bind someone's node to a machine that is
// not one of the deployment's, is refused at start, before anything is published.
func TestAScopeNamingAnUnknownMachineIsRefusedAtStart(t *testing.T) {
	known := []string{"hp-ubuntu", "mac-tomas", "future-two"}
	ok := `{"manager":{"node":"cc-sandbox:m","machine":"mac-tomas"},"peers":["cc-sandbox:p"],"peer_machines":{"cc-sandbox:p":"future-two"}}`
	if err := checkScopeMachines([]byte(ok), known); err != nil {
		t.Fatalf("a scope of known machines: %v", err)
	}
	for name, sc := range map[string]string{
		"an unknown manager machine": `{"manager":{"node":"cc-sandbox:m","machine":"x6"}}`,
		"an unknown peer machine":    `{"peer_machines":{"cc-sandbox:p":"x6"}}`,
		"a malformed machine":        `{"manager":{"node":"cc-sandbox:m","machine":"a b/c"}}`,
		"a manager with no machine":  `{"manager":{"node":"cc-sandbox:m"}}`,
		"a peer with no machine":     `{"manager":{"node":"cc-sandbox:m","machine":"mac-tomas"},"peers":["cc-sandbox:p"]}`,
	} {
		if checkScopeMachines([]byte(sc), known) == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if checkScopeMachines([]byte(`{"manager":{"node":"cc-sandbox:m","machine":"anywhere"}}`), nil) != nil {
		t.Error("with no list configured only the shape is checked")
	}
	if checkScopeMachines(nil, known) != nil || checkScopeMachines([]byte("null"), known) != nil {
		t.Error("no scope is fine")
	}
}

// context: the hub's warning about a binding that did not hold arrives with every renewal (about every 20 s); the
// operator must see it once, not a line every 20 seconds.
func TestAHubWarningIsLoggedOncePerMessage(t *testing.T) {
	var n int
	w := warnOnce(func(string, ...any) { n++ })
	for i := 0; i < 5; i++ {
		w("cc-sandbox:m is claimed by mac")
	}
	w("another")
	if n != 2 {
		t.Fatalf("logged %d times, want 2", n)
	}
}
