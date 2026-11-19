package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/start"
	"github.com/landovsky/cc-sandbox/internal/verbq"
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
	os.MkdirAll(filepath.Join(g, "in"), 0o755)
	os.WriteFile(filepath.Join(g, "out", "requests", "d1.json"), []byte(`{"type":"ccf.done"}`), 0o644)

	d := &start.Daemon{StateDir: state}
	q, err := verbq.Open(filepath.Join(state, "queue"))
	if err != nil {
		t.Fatal(err)
	}
	defer q.Close()
	q.Reply = replyWriter(state, d)
	m := &mux{}
	m.on("ccf.", verbHandler(q, d, nil))
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
