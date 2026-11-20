// Tests of the hostd handlers. Why: they are where an untrusted fence request turns into a message,
// a queued verb or a journal record, so each case names the abuse it is meant to survive.

package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http/httptest"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/daemonapi"
	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/journal"
	"github.com/landovsky/cc-sandbox/internal/msg"
	"github.com/landovsky/cc-sandbox/internal/profile"
	"github.com/landovsky/cc-sandbox/internal/remotestart"
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

// gitwdir is wdir plus `git init`: a remote start must name a git worktree root.
func gitwdir(t *testing.T) string {
	t.Helper()
	d := wdir(t)
	if out, err := exec.Command("git", "init", "-q", d).CombinedOutput(); err != nil {
		t.Skipf("git init: %v %s", err, out)
	}
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

// context (R2-C1): the cross-machine label check runs for EVERY start, a local `ccsbx start` on the unix socket
// included. Otherwise a remote start from the Mac and a local start on hp, one second apart, would both launch the
// same label, and the reservation protocol would protect nothing.
func TestALocalStartAsksThePeersToo(t *testing.T) {
	w := wdir(t)
	sd, p := policyFor(t, "tomas")
	var asked []string
	svc, err := remotestart.New(remotestart.Config{
		Machine: "hp-ubuntu", OwnerUser: "tomas", PeerSet: "S", StateDir: t.TempDir(),
		Peers:    []remotestart.Peer{{Name: "mac-tomas", URL: "http://mac"}},
		Profiles: profiles.Load, LookupUID: func(string) (int, error) { return os.Getuid(), nil },
		Starter: func(int, []byte, string) (*start.StartReply, error) { return nil, errors.New("unused") },
		Live:    func(string) ([]remotestart.View, error) { return nil, nil },
		AskPeer: func(_ context.Context, pe remotestart.Peer, label string) (remotestart.Answer, error) {
			asked = append(asked, pe.Name+" "+label)
			return remotestart.Answer{Live: []remotestart.View{{Machine: "mac-tomas", Sid: "s0there", State: "running"}}, PeerSet: "S"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	h := startHandlerBy2(sd, p, svc)
	_, err = h(4000, []byte(fmt.Sprintf(specOf, "tomas", w)))
	if r, ok := err.(*start.Refusal); !ok || r.Code != start.AlreadyRunning || r.Machine != "mac-tomas" {
		t.Fatalf("a local start of a label that is live on a peer: %v", err)
	}
	if len(asked) != 1 || !strings.Contains(asked[0], "cc-sandbox:x") {
		t.Fatalf("the peer was not asked: %v", asked)
	}
	// nothing was recorded or launched
	if recs, _ := os.ReadDir(filepath.Join(sd.StateDir, "sessions")); len(recs) != 0 {
		t.Fatalf("a refused start left %d records", len(recs))
	}
}

func remoteRig(t *testing.T) (*start.Daemon, startPolicy, *remotestart.Service) {
	t.Helper()
	sd, p := policyFor(t, "tomas")
	p.RemoteMaxPerHour = 2
	svc, err := remotestart.New(remotestart.Config{
		Machine: "hp-ubuntu", OwnerUser: "tomas", PeerSet: "S", StateDir: t.TempDir(),
		Profiles: profiles.Load, LookupUID: func(string) (int, error) { return os.Getuid(), nil },
		Starter: func(uid int, spec []byte, by string) (*start.StartReply, error) {
			return startHandlerBy(sd, p, nil)(uid, spec, by)
		},
		Live: func(label string) ([]remotestart.View, error) {
			recs, _ := sd.LiveByLabel(label)
			var out []remotestart.View
			for _, r := range recs {
				out = append(out, remotestart.View{Machine: "hp-ubuntu", Sid: r.Sid, Gen: r.Gen, State: "running"})
			}
			return out, nil
		},
		PerUserMinute: 100, PerMachineMinute: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	return sd, p, svc
}

// context (M2c end to end, path form): the owner's other machine starts a session HERE; it goes through the same
// start as a local one (profile, worktree root, pins), is recorded with who asked, and a retry after a lost answer
// is the same session.
func TestARemoteStartIsRecordedWithWhoAskedAndRetriesAreIdempotent(t *testing.T) {
	w := gitwdir(t)
	sd, _, svc := remoteRig(t)
	me := remotestart.Caller{StartedBy: "owner:tomas@example.com@mac-tomas", Machine: "mac-tomas.example.ts.net"}
	body := []byte(fmt.Sprintf(specOf, "tomas", w))
	rep, ref := svc.Start(context.Background(), me, body)
	if ref != nil {
		t.Fatalf("%v", ref)
	}
	rec, err := sd.Load(rep.Sid)
	if err != nil || rec.StartedBy != me.StartedBy {
		t.Fatalf("the record does not say who asked: %+v %v", rec, err)
	}
	again, ref := svc.Start(context.Background(), me, body)
	if ref != nil || again.Sid != rep.Sid {
		t.Fatalf("a retry started a second session: %v %v", again, ref)
	}
	// a path outside the worktree root is refused exactly as a local start refuses it (nothing remote widens it)
	out := []byte(fmt.Sprintf(specOf, "tomas", "/etc"))
	out = []byte(strings.Replace(string(out), `"r1"`, `"r2"`, 1))
	out = []byte(strings.Replace(string(out), "cc-sandbox:x", "cc-sandbox:y", 1))
	if _, ref := svc.Start(context.Background(), me, out); ref == nil || ref.Code != start.BadSpec {
		t.Fatalf("a remote workdir outside the worktree root: %v", ref)
	}
}

// context (C2): a remote caller may resume only a session it started; the owner's local sessions, and another
// device's, are not its to take over.
func TestARemoteCallerCannotResumeASessionItDidNotStart(t *testing.T) {
	w := gitwdir(t)
	sd, _, svc := remoteRig(t)
	a := remotestart.Caller{StartedBy: "owner:tomas@example.com@mac-tomas", Machine: "mac"}
	b := remotestart.Caller{StartedBy: "owner:tomas@example.com@old-laptop", Machine: "laptop"}
	rep, ref := svc.Start(context.Background(), a, []byte(fmt.Sprintf(specOf, "tomas", w)))
	if ref != nil {
		t.Fatal(ref)
	}
	sd.Launcher = start.NoLauncher{}
	resume := strings.Replace(fmt.Sprintf(specOf, "tomas", w), `"request_id":"r1"`, `"request_id":"r2","resume":{"sid":"`+rep.Sid+`"}`, 1)
	if _, ref := svc.Start(context.Background(), b, []byte(resume)); ref == nil || ref.Code != start.ResumeNotYours {
		t.Fatalf("another device resumed it: %v", ref)
	}
}

// context (C8): the remote hourly budget is its own: using it up leaves the owner's local starts alone, and the
// other way round.
func TestTheRemoteHourlyBudgetIsSeparateFromTheLocalOne(t *testing.T) {
	w := gitwdir(t)
	sd, p, svc := remoteRig(t)
	me := remotestart.Caller{StartedBy: "owner:tomas@example.com@mac-tomas", Machine: "mac"}
	label := func(i int) string {
		return strings.NewReplacer(`"r1"`, fmt.Sprintf(`"r%d"`, i), "cc-sandbox:x", fmt.Sprintf("cc-sandbox:x%d", i)).Replace(fmt.Sprintf(specOf, "tomas", w))
	}
	for i := 1; i <= 2; i++ {
		if _, ref := svc.Start(context.Background(), me, []byte(label(i))); ref != nil {
			t.Fatalf("remote start %d: %v", i, ref)
		}
	}
	if _, ref := svc.Start(context.Background(), me, []byte(label(3))); ref == nil || ref.Code != start.RateCapped {
		t.Fatalf("the 3rd remote start in the hour: %v", ref)
	}
	local := strings.Replace(label(4), w, wdir(t), 1) // a local start needs no git worktree: its own plain directory
	if _, err := startHandler(sd, p)(4000, []byte(local)); err != nil {
		t.Fatalf("a LOCAL start was refused because remote starts used up the remote budget: %v", err)
	}
}

// context (C6, C12 review): a remote workdir.path is checked exactly as a local one: symlinks resolved, strictly
// inside a configured root, no hidden home directories. Each case is a way a remote caller could point the fence at
// something that is not a worktree: a symlink out of the root, a sibling that merely starts with the root's name, a
// dot-directory such as ~/.ssh, the root itself.
func TestARemoteWorkdirIsHeldToTheWorktreeRootBlock(t *testing.T) {
	_, _, svc := remoteRig(t)
	me := remotestart.Caller{StartedBy: "owner:tomas@example.com@mac-tomas", Machine: "mac"}
	root := "/var/tmp"
	inside := gitwdir(t) // a real directory under the root
	outside, _ := os.MkdirTemp(os.TempDir(), "ccsbx-outside-")
	t.Cleanup(func() { os.RemoveAll(outside) })
	link := filepath.Join(inside, "escape")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	sibling := root + "2" // /var/tmp2: starts with the root's name, is not inside it
	os.MkdirAll(sibling, 0o700)
	t.Cleanup(func() { os.RemoveAll(sibling) })
	home, _ := os.UserHomeDir()
	hidden := filepath.Join(home, ".ssh") // outside the root, as a hidden home directory is
	plain := wdir(t)                      // a real directory under the root that is NOT a git worktree
	repoSub := filepath.Join(inside, "sub")
	os.MkdirAll(repoSub, 0o755)
	n := 0
	try := func(path string) *start.Refusal {
		n++
		body := strings.NewReplacer(`"r1"`, fmt.Sprintf(`"rw%d"`, n), "cc-sandbox:x", fmt.Sprintf("cc-sandbox:w%d", n)).Replace(fmt.Sprintf(specOf, "tomas", path))
		_, ref := svc.Start(context.Background(), me, []byte(body))
		return ref
	}
	for name, p := range map[string]string{
		"a symlink out of the root":                                   link,
		"a sibling that starts with the root":                         sibling,
		"a hidden directory (~/.ssh-like)":                            hidden,
		"the root itself":                                             root,
		"a path with ..":                                              inside + "/../../etc",
		"a relative path":                                             "tmp/x",
		"a plain directory (not a worktree)":                          plain,
		"a subdirectory of a repo":                                    repoSub,
		"the parent of repos (the root's own child that holds repos)": filepath.Dir(plain),
	} {
		if ref := try(p); ref == nil || ref.Code != start.BadSpec {
			t.Errorf("%s: %v", name, ref)
		}
	}
	if ref := try(inside); ref != nil {
		t.Fatalf("a directory inside the root was refused: %v", ref)
	}
}

// context (M2c, direct mode): in direct mode hostd runs AS the owner, so the owner's own uid is exactly what a remote
// start must run as; in helper mode hostd's uid is the service user, and an owner-user equal to it is a
// misconfiguration that must not start sessions.
func TestARemoteStartMayRunAsHostdsOwnUserOnlyInDirectMode(t *testing.T) {
	w := wdir(t)
	for _, c := range []struct {
		root string
		ok   bool
	}{{"direct", true}, {"helper", false}} {
		sd, p := policyFor(t, "tomas")
		sd.Root = c.root
		_, err := startHandlerBy(sd, p, nil)(os.Getuid(), []byte(fmt.Sprintf(specOf, "tomas", w)), "owner:tomas@example.com@mac")
		refused := err != nil && strings.Contains(err.Error(), "may not start sessions")
		if refused == c.ok {
			t.Errorf("%s mode: refused as the service user = %v (%v)", c.root, refused, err)
		}
	}
}

// context (M2c over real HTTP): two daemons that ask each other through GET /v1/resolve?v=2 (the real routes, JSON and
// all), each with its own state and a launcher that records a live session. One label is live on one machine only,
// whichever asks first, and a start on the other is refused with where it lives.
func TestTwoDaemonsAskingEachOtherOverHTTPNeverStartTheSameLabelTwice(t *testing.T) {
	type machine struct {
		sd  *start.Daemon
		svc *remotestart.Service
		ts  *httptest.Server
	}
	names := []string{"hp-ubuntu", "mac-tomas"}
	set := remotestart.PeerSetHash(names)
	ms := map[string]*machine{}
	srvOf := map[string]*daemonapi.Server{}
	for _, n := range names {
		sd, p := policyFor(t, "tomas")
		sd.Machine = n
		m := &machine{sd: sd}
		ms[n] = m
		srv := &daemonapi.Server{}
		srvOf[n] = srv
		m.ts = httptest.NewServer(srv.Handler())
		t.Cleanup(m.ts.Close)
		_ = p
	}
	for _, n := range names {
		other := names[0]
		if n == names[0] {
			other = names[1]
		}
		m := ms[n]
		sd, p := policyFor(t, "tomas")
		sd.Machine = n
		sd.Launcher = aliveLauncher{}   // a launched session counts as live
		sd.ProfileLoader = bothMachines // the profile is approved on both machines of this test
		m.sd = sd
		nn := n
		svc, err := remotestart.New(remotestart.Config{
			Machine: n, OwnerUser: "tomas", PeerSet: set, StateDir: t.TempDir(),
			Peers:     []remotestart.Peer{{Name: other, URL: ms[other].ts.URL}},
			Profiles:  bothMachines,
			LookupUID: func(string) (int, error) { return os.Getuid(), nil },
			Starter: func(uid int, spec []byte, by string) (*start.StartReply, error) {
				return startHandlerBy(sd, p, nil)(uid, spec, by)
			},
			Live: func(label string) ([]remotestart.View, error) {
				recs, _ := sd.LiveByLabel(label)
				var out []remotestart.View
				for _, r := range recs {
					out = append(out, remotestart.View{Machine: nn, Sid: r.Sid, Gen: r.Gen, State: "running"})
				}
				return out, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		m.svc = svc
		srvOf[n].Remote = svc
	}
	w := gitwdir(t)
	me := remotestart.Caller{StartedBy: "owner:tomas@example.com@mac-tomas", Machine: "mac"}
	body := func(machine, rid string) []byte {
		return []byte(strings.NewReplacer(`"r1"`, `"`+rid+`"`, "hp-ubuntu", machine).Replace(fmt.Sprintf(specOf, "tomas", w)))
	}
	// the same label, started on BOTH machines at the same moment
	var wg sync.WaitGroup
	results := make([]*start.Refusal, 2)
	ok := make([]bool, 2)
	for i, n := range names {
		wg.Add(1)
		go func(i int, n string) {
			defer wg.Done()
			rep, ref := ms[n].svc.Start(context.Background(), me, body(n, "q"+strconv.Itoa(i)))
			results[i], ok[i] = ref, rep != nil
		}(i, n)
	}
	wg.Wait()
	if ok[0] && ok[1] {
		t.Fatal("the same label was started on both machines")
	}
	// afterwards: it lives on exactly one; a start on the other is refused with where
	var live string
	for _, n := range names {
		if recs, _ := ms[n].sd.LiveByLabel("cc-sandbox:x"); len(recs) > 0 {
			live = n
		}
	}
	if live == "" {
		for _, r := range results {
			if r == nil || (r.Code != start.StartInProgress && r.Code != start.AlreadyRunning) {
				t.Fatalf("neither machine started it, and a refusal is not the safe kind: %v", r)
			}
		}
		return // both refused and would retry: the safe worst case
	}
	other := names[0]
	if live == names[0] {
		other = names[1]
	}
	_, ref := ms[other].svc.Start(context.Background(), me, body(other, "later"))
	if ref == nil || ref.Code != start.AlreadyRunning || ref.Machine != live {
		t.Fatalf("a start on %s after it lives on %s: %v", other, live, ref)
	}
}

// aliveLauncher launches nothing and reports every session alive (start.NoLauncher reports none, so its records never
// block a label: right for most tests, wrong for one about a label that IS live).
type aliveLauncher struct{}

func (aliveLauncher) Launch(*start.Record) error { return nil }
func (aliveLauncher) Alive(string) bool          { return true }

func bothMachines(name string) (*profile.Profile, error) {
	p, err := profiles.Load(name)
	if err == nil {
		p.Machines = append(p.Machines, "mac-tomas")
	}
	return p, err
}
