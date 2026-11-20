// Tests of the outbox watcher (U3a). Why: each Block twin is one way a fence could make the daemon
// act on something it did not write (symlink, fifo, hardlink, sparse file, flood, a stale
// generation, a replayed request); each Allow twin proves the legitimate case still passes.

package hostd

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/backend"
	"github.com/landovsky/cc-sandbox/internal/start"
)

// fixture is a state dir with one session s0hello1 at generation 1, as `ccsbx start` + the
// backend leave it. Files are made by the test user, so FenceUID is the test user's uid.
type fixture struct {
	t     *testing.T
	state string
	g     string
	h     *Hostd
	got   []Request
}

const sid = "s0hello1"

func newFixture(t *testing.T, mut func(*Config)) *fixture {
	t.Helper()
	state := t.TempDir()
	f := &fixture{t: t, state: state, g: filepath.Join(state, "s", sid, "g1")}
	for _, d := range []string{"out/requests", "in"} {
		if err := os.MkdirAll(filepath.Join(f.g, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	rec := start.Record{Sid: sid, Gen: 1, Label: "cc-sandbox:abcd/worker", Spec: start.LaunchSpec{Label: "cc-sandbox:abcd/worker"},
		Reply: start.StartReply{ID: "tomas/hp/" + sid + "/g1"}}
	os.MkdirAll(filepath.Join(state, "sessions"), 0o700)
	b, _ := json.Marshal(rec)
	if err := os.WriteFile(filepath.Join(state, "sessions", sid+".json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
	cfg := Config{StateDir: state, FenceUID: os.Getuid(), Handler: func(r Request) Reply {
		f.got = append(f.got, r)
		return Reply{Status: "queued"}
	}}
	if mut != nil {
		mut(&cfg)
	}
	f.h = New(cfg)
	return f
}

func (f *fixture) req(name, body string) string {
	p := filepath.Join(f.g, "out", "requests", name)
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		f.t.Fatal(err)
	}
	return p
}

func (f *fixture) reply(rid string) (Reply, bool) {
	b, err := os.ReadFile(filepath.Join(f.g, "in", "replies", rid+".json"))
	if err != nil {
		return Reply{}, false
	}
	var r Reply
	json.Unmarshal(b, &r)
	return r, true
}

func (f *fixture) code(rid string) string {
	r, ok := f.reply(rid)
	if !ok {
		return "(no reply)"
	}
	m, _ := r.Result.(map[string]any)
	c, _ := m["code"].(string)
	return r.Status + ":" + c
}

// dumpAll is everything the daemon wrote anywhere; secrets must not appear in it.
func (f *fixture) dumpAll() string {
	var sb strings.Builder
	filepath.Walk(f.state, func(p string, i os.FileInfo, err error) error {
		if err == nil && i.Mode().IsRegular() && !strings.Contains(p, "/out/") && !strings.Contains(p, "/sessions/") {
			b, _ := os.ReadFile(p)
			sb.Write(b)
		}
		return nil
	})
	return sb.String()
}

// ---- T:stamp-from-outbox -------------------------------------------------------------------

// context: the fence writes the request file, so every byte in it is the agent's. Identity must
// come from where the file lives (the generation's outbox), or one session could speak as another.
func TestStampFromOutboxAllow(t *testing.T) {
	f := newFixture(t, nil)
	f.req("r1.json", `{"type":"ccf.outcome","text":"done"}`)
	st := f.h.Scan()
	if st.Accepted != 1 || len(f.got) != 1 {
		t.Fatalf("accepted=%d got=%d", st.Accepted, len(f.got))
	}
	r := f.got[0]
	if r.Sid != sid || r.Gen != 1 || r.Label != "cc-sandbox:abcd/worker" || r.FullID != "tomas/hp/"+sid+"/g1" || r.Rid != "r1" {
		t.Fatalf("stamp wrong: %+v", r)
	}
	if rep, ok := f.reply("r1"); !ok || rep.Status != "queued" || rep.ID != r.ID {
		t.Fatalf("reply %+v ok=%v", rep, ok)
	}
}

func TestStampFromOutboxBlock(t *testing.T) {
	f := newFixture(t, nil)
	f.req("r1.json", `{"type":"msg.send","sid":"s0victim1","gen":9,"label":"cc-sandbox:boss","id":"01AAAAAAAAAAAAAAAAAAAAAAAA","from":{"fenced":false,"trust":"human"},"id_forge":1}`)
	f.h.Scan()
	if len(f.got) != 1 {
		t.Fatalf("got %d", len(f.got))
	}
	r := f.got[0]
	if r.Sid != sid || r.Gen != 1 || r.Label != "cc-sandbox:abcd/worker" || r.ID == "01AAAAAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("a claim in the body leaked into the stamp: %+v", r)
	}
}

// context: the rid is the fence's choice; the daemon's ULID, not the filename, is the identity.
func TestEveryRequestGetsItsOwnDaemonUlid(t *testing.T) {
	f := newFixture(t, nil)
	f.req("a.json", `{"type":"ccf.mark"}`)
	f.req("b.json", `{"type":"ccf.mark"}`)
	f.h.Scan()
	if len(f.got) != 2 || f.got[0].ID == f.got[1].ID || len(f.got[0].ID) != 26 {
		t.Fatalf("ids: %+v", f.got)
	}
}

// ---- T:outbox-symlink ----------------------------------------------------------------------

const secret = "SECRET-HOST-FILE-CONTENT"

// context: a request file that is a symlink to a host file would make the daemon read (and a
// reply leak) something the fence can't see. The legitimate regular file must still work.
func TestOutboxSymlinkAllow(t *testing.T) { TestStampFromOutboxAllow(t) }

func TestOutboxSymlinkBlock(t *testing.T) {
	f := newFixture(t, nil)
	target := filepath.Join(t.TempDir(), "host-secret")
	os.WriteFile(target, []byte(`{"type":"ccf.outcome","x":"`+secret+`"}`), 0o644)
	if err := os.Symlink(target, filepath.Join(f.g, "out", "requests", "evil.json")); err != nil {
		t.Fatal(err)
	}
	st := f.h.Scan()
	if st.Accepted != 0 || len(f.got) != 0 {
		t.Fatalf("followed a symlink: %+v", st)
	}
	if c := f.code("evil"); c != "refused:unsafe_path" {
		t.Fatalf("reply %s", c)
	}
	if strings.Contains(f.dumpAll(), secret) {
		t.Fatal("host file content reached the daemon's output")
	}
}

// context: a relative link that stays inside the outbox passes RESOLVE_BENEATH; only
// NO_SYMLINKS stops it, so this is the case that proves that flag is set.
func TestOutboxSymlinkInsideOutboxIsStillRefused(t *testing.T) {
	f := newFixture(t, nil)
	os.WriteFile(filepath.Join(f.g, "out", "inside"), []byte(`{"type":"ccf.mark"}`), 0o644)
	os.Symlink("../inside", filepath.Join(f.g, "out", "requests", "rel.json"))
	f.h.Scan()
	if len(f.got) != 0 || f.code("rel") != "refused:unsafe_path" {
		t.Fatalf("followed an in-outbox symlink: %s", f.code("rel"))
	}
}

// context: swapping the requests/ directory itself for a symlink must not redirect the scan.
func TestOutboxSymlinkedRequestsDirIsNotFollowed(t *testing.T) {
	f := newFixture(t, nil)
	elsewhere := t.TempDir()
	os.WriteFile(filepath.Join(elsewhere, "x.json"), []byte(`{"type":"ccf.mark"}`), 0o644)
	os.RemoveAll(filepath.Join(f.g, "out", "requests"))
	os.Symlink(elsewhere, filepath.Join(f.g, "out", "requests"))
	st := f.h.Scan()
	if st.Accepted != 0 || len(f.got) != 0 {
		t.Fatalf("followed a symlinked requests dir: %+v", st)
	}
}

// ---- T:outbox-fifo -------------------------------------------------------------------------

func TestOutboxFifoAllow(t *testing.T) { TestStampFromOutboxAllow(t) }

// context: opening a FIFO for reading blocks until a writer shows up, so a fence that drops a
// FIFO named x.json could freeze the daemon for every session.
func TestOutboxFifoBlock(t *testing.T) {
	f := newFixture(t, nil)
	if err := syscall.Mkfifo(filepath.Join(f.g, "out", "requests", "pipe.json"), 0o644); err != nil {
		t.Fatal(err)
	}
	f.req("ok.json", `{"type":"ccf.mark"}`)
	done := make(chan Stats)
	go func() { done <- f.h.Scan() }()
	select {
	case st := <-done:
		if c := f.code("pipe"); c != "refused:not_regular" {
			t.Fatalf("fifo reply %s", c)
		}
		if st.Accepted != 1 {
			t.Fatalf("the regular request next to it was not served: %+v", st)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("scan hung on a FIFO")
	}
}

// ---- T:outbox-hardlink ---------------------------------------------------------------------

func TestOutboxHardlinkAllow(t *testing.T) { TestStampFromOutboxAllow(t) }

// context: a hard link to a file the fence uid owns elsewhere (another session's outbox) would
// pass an owner check; nlink == 1 is what proves the file is only this outbox's.
func TestOutboxHardlinkBlock(t *testing.T) {
	f := newFixture(t, nil)
	other := f.req("orig.json", `{"type":"ccf.mark"}`)
	os.Rename(other, filepath.Join(f.g, "out", "elsewhere")) // the "other" name outside requests/
	if err := os.Link(filepath.Join(f.g, "out", "elsewhere"), filepath.Join(f.g, "out", "requests", "linked.json")); err != nil {
		t.Fatal(err)
	}
	f.h.Scan()
	if len(f.got) != 0 || f.code("linked") != "refused:hardlink" {
		t.Fatalf("hardlinked file accepted or wrong reply: %s", f.code("linked"))
	}
}

// ---- T:outbox-sparse -----------------------------------------------------------------------

// context: a request exactly at the size cap is still served; only bigger ones are sparse-bombs.
func TestOutboxSparseAllow(t *testing.T) {
	body := `{"type":"ccf.mark","pad":"`
	f := newFixture(t, func(c *Config) { c.MaxBytes = int64(len(body)) + 50 })
	f.req("edge.json", body+strings.Repeat("a", 48)+`"}`)
	if st := f.h.Scan(); st.Accepted != 1 {
		t.Fatalf("%+v", st)
	}
}

// context: a 1 TiB sparse file costs no disk, but reading it would pin the daemon. The apparent
// size from fstat decides, before any read.
func TestOutboxSparseBlock(t *testing.T) {
	f := newFixture(t, nil)
	p := f.req("big.json", `{"type":"ccf.mark"}`)
	if err := os.Truncate(p, 1<<40); err != nil {
		t.Skip("filesystem has no sparse files:", err)
	}
	start := time.Now()
	f.h.Scan()
	if time.Since(start) > 2*time.Second {
		t.Fatal("took too long: it read the file")
	}
	if len(f.got) != 0 || f.code("big") != "refused:too_big" {
		t.Fatalf("reply %s", f.code("big"))
	}
}

// ---- T:outbox-flood ------------------------------------------------------------------------

// context: ten requests in a burst is normal work and must all be served.
func TestOutboxFloodAllow(t *testing.T) {
	f := newFixture(t, nil)
	for i := 0; i < 10; i++ {
		f.req(fmt.Sprintf("r%d.json", i), `{"type":"ccf.mark"}`)
	}
	if st := f.h.Scan(); st.Accepted != 10 {
		t.Fatalf("%+v", st)
	}
}

// context: a fence writing 5000 request files in a loop must cost the daemon bounded work: the
// bucket serves its burst, a few refusals are answered, and the rest is shed, not queued.
func TestOutboxFloodBlock(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.Burst, c.Rate, c.ShedRefs = 20, 10, 20 })
	for i := 0; i < 5000; i++ {
		f.req(fmt.Sprintf("f%d.json", i), `{"type":"ccf.mark"}`)
	}
	st := f.h.Scan()
	if st.Accepted > 25 || len(f.got) > 25 {
		t.Fatalf("the flood was served: %+v", st)
	}
	reps, _ := os.ReadDir(filepath.Join(f.g, "in", "replies"))
	if len(reps) > 60 {
		t.Fatalf("%d replies written for a flood", len(reps))
	}
	if st.Shed < 4000 {
		t.Fatalf("expected most of the flood shed: %+v", st)
	}
	// A second pass does not re-process what was shed.
	again := f.h.Scan()
	if again.Accepted != 0 || again.Refused != 0 {
		t.Fatalf("rescan reprocessed: %+v", again)
	}
}

// ---- restart, caps, names, secrets ---------------------------------------------------------

// context: the daemon can't delete outbox files, so after a restart it must not run the same
// request twice; the ledger remembers.
func TestRestartDoesNotReplayProcessedRequests(t *testing.T) {
	f := newFixture(t, nil)
	f.req("r1.json", `{"type":"ccf.done"}`)
	f.h.Scan()
	h2 := New(f.h.Cfg)
	h2.Scan()
	if len(f.got) != 1 {
		t.Fatalf("replayed after restart: %d", len(f.got))
	}
}

// context: a request file larger than the cap is refused without being read at all, and the
// refusal reply never repeats a byte of it.
func TestOversizeRequestRefusedWithoutEcho(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.MaxBytes = 100 })
	f.req("fat.json", `{"type":"ccf.mark","x":"`+secret+strings.Repeat("A", 200)+`"}`)
	f.h.Scan()
	if f.code("fat") != "refused:too_big" || strings.Contains(f.dumpAll(), secret) {
		t.Fatalf("reply %s", f.code("fat"))
	}
}

// context: garbage and unknown types get a refusal with a rule name, never a parse error that
// quotes the input.
func TestBadJsonAndUnknownTypeRefusedWithoutEcho(t *testing.T) {
	f := newFixture(t, nil)
	f.req("junk.json", `{"type": `+secret)
	f.req("odd.json", `{"type":"session.started","x":"`+secret+`"}`)
	f.h.Scan()
	if f.code("junk") != "refused:bad_json" || f.code("odd") != "refused:unknown_type" {
		t.Fatalf("%s %s", f.code("junk"), f.code("odd"))
	}
	if strings.Contains(f.dumpAll(), secret) || len(f.got) != 0 {
		t.Fatal("request bytes echoed or a bad request was handled")
	}
}

// context: files the fence names oddly (a path-like name, .tmp half-written files) are not
// requests and must not turn into reply paths.
func TestOnlySimpleJsonNamesAreRequests(t *testing.T) {
	f := newFixture(t, nil)
	f.req("half.json.tmp", `{"type":"ccf.mark"}`)
	f.req("..json", `{"type":"ccf.mark"}`)
	f.req("-x.json", `{"type":"ccf.mark"}`)
	if st := f.h.Scan(); st.Accepted != 0 {
		t.Fatalf("%+v", st)
	}
}

// context: a file owned by a different uid than the fence's (e.g. planted by another host
// user) is not the fence's request.
func TestFileOfAnotherUidIsRefused(t *testing.T) {
	f := newFixture(t, func(c *Config) { c.FenceUID = os.Getuid() + 1 })
	f.req("r.json", `{"type":"ccf.mark"}`)
	f.h.Scan()
	if f.code("r") != "refused:wrong_owner" {
		t.Fatalf("%s", f.code("r"))
	}
}

// context: after the generation ends (CLOSED), one last pass drains the outbox and then the
// daemon stops looking at it.
func TestClosedGenerationIsDrainedOnce(t *testing.T) {
	f := newFixture(t, nil)
	f.req("last.json", `{"type":"ccf.done"}`)
	os.WriteFile(filepath.Join(f.g, "CLOSED"), []byte("closed\n"), 0o600)
	f.h.Scan()
	f.req("late.json", `{"type":"ccf.done"}`)
	f.h.Scan()
	if len(f.got) != 1 || f.got[0].Rid != "last" {
		t.Fatalf("got %+v", f.got)
	}
}

// ---- T:outbox-host-write-refused ------------------------------------------------------------

// The twin needs a real privilege split: out/ owned by the fence uid and closed to everyone else.
// In direct mode every writer is tomas, so the block twin is RED by design until gate M.
// It only runs as a twin (CCSBX_TWIN set), so `make test` stays green.
func TestOutboxHostWriteRefusedAllow(t *testing.T) { TestStampFromOutboxAllow(t) }

func TestOutboxHostWriteRefusedBlock(t *testing.T) {
	if os.Getenv("CCSBX_TWIN") == "" {
		t.Skip("twin only: red in direct mode until gate M")
	}
	state := t.TempDir()
	s := backend.Spec{Sid: sid, Gen: 1, Label: "x", StateDir: state, Workdir: "/srv/ccsbx-test-work", NotEvid: true}
	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(s.GenDir(), "out", "planted.json")
	if err := os.WriteFile(p, []byte("{}"), 0o644); err == nil {
		t.Fatalf("direct mode: the host user can write the outbox (%s); the split needs root (gate M)", p)
	}
}

// ---- review fixes (U3a-M1, S1, N3) ----------------------------------------------------------

// setRecordGen rewrites the fixture's session record, as a resume (U7) or an end would.
func (f *fixture) setRecord(mut func(*start.Record)) {
	f.t.Helper()
	p := filepath.Join(f.state, "sessions", sid+".json")
	b, _ := os.ReadFile(p)
	var rec start.Record
	json.Unmarshal(b, &rec)
	mut(&rec)
	b, _ = json.Marshal(rec)
	os.WriteFile(p, b, 0o600)
}

// T:outbox-stale-generation
// context: gen 1's `ccsbx run` was SIGKILLed (no CLOSED marker) and the session resumed as gen 2.
// A request still written into g1/out must not be stamped with gen 2's identity.
func TestOutboxStaleGenerationAllow(t *testing.T) {
	f := newFixture(t, nil)
	f.setRecord(func(r *start.Record) { r.Gen = 2; r.Reply.ID = "tomas/hp/" + sid + "/g2" })
	g2 := filepath.Join(f.state, "s", sid, "g2", "out", "requests")
	os.MkdirAll(g2, 0o755)
	os.WriteFile(filepath.Join(g2, "live.json"), []byte(`{"type":"ccf.mark"}`), 0o644)
	f.h.Scan()
	if len(f.got) != 1 || f.got[0].Gen != 2 || f.got[0].FullID != "tomas/hp/"+sid+"/g2" {
		t.Fatalf("the live generation's request: %+v", f.got)
	}
}

func TestOutboxStaleGenerationBlock(t *testing.T) {
	f := newFixture(t, nil)
	f.setRecord(func(r *start.Record) { r.Gen = 2; r.Reply.ID = "tomas/hp/" + sid + "/g2" })
	f.req("old.json", `{"type":"git.push"}`) // written into g1/out by the dead generation
	st := f.h.Scan()
	if len(f.got) != 0 || st.Accepted != 0 {
		t.Fatalf("old generation acted under gen 2's identity: %+v", f.got)
	}
	if r, ok := f.reply("old"); !ok || r.Status != "stale" {
		t.Fatalf("reply %+v", r)
	}
	// A request that shows up in the dead generation later is not picked up either.
	f.req("later.json", `{"type":"git.push"}`)
	f.h.Scan()
	if len(f.got) != 0 {
		t.Fatal("a drained stale generation was scanned again")
	}
}

// context: the session ended (record has Ended); whatever its outbox still holds is stale.
func TestRequestsOfAnEndedSessionAreStale(t *testing.T) {
	f := newFixture(t, nil)
	f.setRecord(func(r *start.Record) { now := time.Now(); r.Ended = &now })
	f.req("r.json", `{"type":"grant.request"}`)
	f.h.Scan()
	if r, _ := f.reply("r"); len(f.got) != 0 || r.Status != "stale" {
		t.Fatalf("%+v %+v", f.got, r)
	}
}

// T:ledger-fail-closed
// context: the ledger is what stops a restart from running a request twice under a new ULID.
// While it can't be written, the handler must not run; when the disk is back, the request is
// handled exactly once.
func TestLedgerFailClosedAllow(t *testing.T) { TestStampFromOutboxAllow(t) }

func TestLedgerFailClosedBlock(t *testing.T) {
	f := newFixture(t, nil)
	ld := filepath.Join(f.state, "hostd", "ledger")
	os.MkdirAll(ld, 0o700)
	os.Chmod(ld, 0o500) // no new files: the ledger append fails
	defer os.Chmod(ld, 0o700)
	if os.WriteFile(filepath.Join(ld, "probe"), nil, 0o600) == nil {
		t.Skip("running as a user that ignores directory permissions")
	}
	f.req("r1.json", `{"type":"git.push"}`)
	st := f.h.Scan()
	if len(f.got) != 0 || st.Reasons["ledger_io_error"] != 1 {
		t.Fatalf("handled without a ledger entry: %+v %v", f.got, st.Reasons)
	}
	if r, _ := f.reply("r1"); r.Status != "error" {
		t.Fatalf("reply %+v", r)
	}
	os.Chmod(ld, 0o700)
	f.h.Scan()
	New(f.h.Cfg).Scan() // and a restart
	if len(f.got) != 1 {
		t.Fatalf("after the disk came back it ran %d times", len(f.got))
	}
}

// context: Go's json keeps the LAST duplicate key; another reader of the raw body may keep the
// first. A request whose top-level keys repeat is refused, so everyone sees one type.
func TestDuplicateTopLevelKeysAreRefused(t *testing.T) {
	f := newFixture(t, nil)
	f.req("dup.json", `{"type":"ccf.outcome","text":"x","type":"git.push"}`)
	f.req("nested.json", `{"type":"ccf.mark","body":{"a":1,"a":2}}`) // nested duplicates are the handler's data
	f.h.Scan()
	if f.code("dup") != "refused:bad_json" || len(f.got) != 1 || f.got[0].Rid != "nested" {
		t.Fatalf("%s %+v", f.code("dup"), f.got)
	}
}

// context: a session's verbs are applied in the order it wrote them; hostd must hand them to
// the queue in name order (time-ordered rids), not in whatever order the directory lists.
func TestRequestsAreHandledInNameOrder(t *testing.T) {
	f := newFixture(t, nil)
	for _, n := range []string{"0005", "0001", "0004", "0002", "0003"} {
		f.req(n+".json", `{"type":"ccf.mark"}`)
	}
	f.h.Scan()
	var got []string
	for _, r := range f.got {
		got = append(got, r.Rid)
	}
	if strings.Join(got, ",") != "0001,0002,0003,0004,0005" {
		t.Fatalf("%v", got)
	}
}
