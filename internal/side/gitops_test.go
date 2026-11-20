// Tests of the pod's git. Why: the sidecar runs git on a tree the fence wrote, with a network path to a repo the
// fence must not steer, so each case is a rule about what that git may do. The door is a real `git http-backend`
// behind the real forwarder and a real proof check, so what git sends must pass what a door would verify.

package side

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cgi"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/fwd"
	"github.com/landovsky/cc-sandbox/internal/hostd"
	"github.com/landovsky/cc-sandbox/internal/token"
)

type gitRig struct {
	t        *testing.T
	root     string
	bare     string // the door's repository
	fwdURL   string
	g        *GitOps
	j        *lockedJournal
	work     string
	snap     string
	doorHits int
	key      *token.Key
	doorURL  string
	gate     string
	doorSaw  []http.Header
}

func sh(t *testing.T, dir string, env []string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append([]string{"HOME=" + t.TempDir(), "PATH=/usr/bin:/bin", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1",
		"GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t"}, env...)
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

func newGitRig(t *testing.T, mut func(*GitConfig)) *gitRig {
	t.Helper()
	root, err := os.MkdirTemp("", "sidegit")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	os.Chmod(root, 0o700)
	r := &gitRig{t: t, root: root, j: &lockedJournal{}}
	// the door's repository: main with a script (exec bit) and a file
	r.bare = filepath.Join(root, "doors", "git", "o", "r.git")
	os.MkdirAll(filepath.Dir(r.bare), 0o755)
	sh(t, root, nil, "init", "--bare", "-q", "-b", "main", r.bare)
	sh(t, r.bare, nil, "config", "http.receivepack", "true")
	scratch := filepath.Join(root, "scratch")
	sh(t, root, nil, "init", "-q", "-b", "main", scratch)
	os.WriteFile(filepath.Join(scratch, "a.txt"), []byte("base\n"), 0o644)
	os.WriteFile(filepath.Join(scratch, "run.sh"), []byte("#!/bin/sh\n"), 0o755)
	os.MkdirAll(filepath.Join(scratch, "sub"), 0o755)
	os.WriteFile(filepath.Join(scratch, "sub", "x.txt"), []byte("x\n"), 0o644)
	sh(t, scratch, nil, "add", "-A")
	sh(t, scratch, nil, "commit", "-q", "-m", "base")
	sh(t, scratch, nil, "push", "-q", r.bare, "main")

	// the door: git http-backend behind a real proof check; the forwarder in front of it
	key, err := token.LoadOrCreateKeyFile(filepath.Join(root, "dev.key"))
	if err != nil {
		t.Fatal(err)
	}
	gen, _ := token.NewGenKey()
	tok, err := token.Mint(key, token.Claims{Sub: "tomas/hp-ubuntu/s0worker/g1", Billing: "stub", Profile: "dev-default",
		Aud: []string{"ccsbx-egress"}, Cnf: token.CnfOf(gen.Public), Exp: time.Now().Add(time.Hour).Unix()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	backend := &cgi.Handler{Path: "/usr/lib/git-core/git-http-backend", Env: []string{"GIT_PROJECT_ROOT=" + filepath.Join(root, "doors"),
		"GIT_HTTP_EXPORT_ALL=1", "REMOTE_USER=door", "PATH=/usr/bin:/bin", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1"}}
	ring := key.Ring([]string{"tomas"}, []string{"hp-ubuntu"})
	counted := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) {
		r.doorHits++
		r.doorSaw = append(r.doorSaw, q.Header.Clone())
		backend.ServeHTTP(w, q)
	})
	door := httptest.NewServer(token.NewVerifier(ring, "ccsbx-egress").Middleware(counted, nil))
	t.Cleanup(door.Close)
	du, _ := url.Parse(door.URL)
	r.key, r.doorURL = key, door.URL
	r.gate = "gate-" + filepath.Base(root)
	f, err := fwd.New(fwd.Config{Token: tok, Key: gen, Doors: map[string]*url.URL{"egress": du}, MaxBody: 8 << 20, GitSecret: r.gate})
	if err != nil {
		t.Fatal(err)
	}
	fs := httptest.NewServer(f)
	t.Cleanup(fs.Close)
	r.fwdURL = fs.URL

	r.work, r.snap = filepath.Join(root, "work"), filepath.Join(root, "side", "snapshot", "s")
	os.MkdirAll(r.work, 0o755)
	os.MkdirAll(filepath.Dir(r.snap), 0o700)
	cfg := GitConfig{Repo: "o/r", Base: "main", Branch: "ccsbx/s1", URL: fs.URL + "/egress/git", GitDir: filepath.Join(root, "side", "git"),
		Work: r.work, Snapshot: r.snap, Sid: "s0worker", Gen: 1, Journal: r.j, Timeout: 30 * time.Second, Gate: func() string { return r.gate }}
	if mut != nil {
		mut(&cfg)
	}
	for _, d := range []string{cfg.GitDir, cfg.Snapshot} {
		if err := ownDir(d, os.Getuid()); err != nil {
			t.Fatal(err)
		}
	}
	r.g, err = NewGitOps(cfg)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func (r *gitRig) clone() {
	r.t.Helper()
	if err := r.g.Clone(context.Background()); err != nil {
		r.t.Fatalf("clone: %v", err)
	}
}

func (r *gitRig) req(typ, body string) hostd.Reply {
	return r.g.Forward(context.Background(), hostd.Request{Type: typ, Body: json.RawMessage(body)})
}

func rcode(r hostd.Reply) string {
	switch m := r.Result.(type) {
	case map[string]string:
		return m["code"]
	}
	return ""
}

func (r *gitRig) door(args ...string) string { return sh(r.t, r.bare, nil, args...) }

// context "a worker's day in a pod: the repo arrives, it edits, the sidecar commits and pushes its branch": the
// whole path through the forwarder and a door that verifies every proof. The base branch at the door is untouched,
// the commit carries the host's trailer, and the executable bit survived the snapshot.
func TestCloneCommitPushRoundTrip(t *testing.T) {
	r := newGitRig(t, nil)
	r.clone()
	if b, _ := os.ReadFile(filepath.Join(r.work, "a.txt")); string(b) != "base\n" {
		t.Fatalf("a.txt: %q", b)
	}
	if fi, _ := os.Stat(filepath.Join(r.work, "run.sh")); fi.Mode()&0o111 == 0 {
		t.Fatal("the exec bit was lost at checkout")
	}
	if fi, _ := os.Stat(filepath.Join(r.work, "a.txt")); fi.Mode().Perm()&0o020 == 0 {
		t.Fatalf("checkout is mode %o: the fence's group cannot write it", fi.Mode().Perm())
	}
	os.WriteFile(filepath.Join(r.work, "new.txt"), []byte("hello\n"), 0o644)
	os.WriteFile(filepath.Join(r.work, "run.sh"), []byte("#!/bin/sh\necho hi\n"), 0o755)
	rep := r.req("git.commit", `{"message":"add new.txt SECRET-MSG"}`)
	if rep.Status != "ok" {
		t.Fatalf("commit: %+v", rep)
	}
	if rep := r.req("git.push", `{}`); rep.Status != "ok" {
		t.Fatalf("push: %+v", rep)
	}
	if tip := r.door("rev-parse", "refs/heads/ccsbx/s1"); tip != rep.Result.(map[string]any)["sha"] {
		t.Errorf("door tip %s, reply %v", tip, rep.Result)
	}
	msg := r.door("log", "-1", "--format=%B", "refs/heads/ccsbx/s1")
	if !strings.Contains(msg, "add new.txt") || !strings.Contains(msg, "Fenced-Session: s0worker") {
		t.Errorf("commit message %q", msg)
	}
	if r.door("rev-parse", "refs/heads/main") != sh(t, r.work, []string{"GIT_DIR=" + r.g.cfg.GitDir}, "rev-parse", "refs/ccsbx/base") {
		t.Error("main at the door moved")
	}
	if ls := r.door("ls-tree", "refs/heads/ccsbx/s1", "run.sh", "new.txt"); !strings.Contains(ls, "100755") || !strings.Contains(ls, "new.txt") {
		t.Errorf("tree at the door: %s", ls)
	}
	// facts: shas and counts, never the message or a path
	for _, e := range r.j.evs {
		if strings.Contains(string(e.Data), "SECRET") || strings.Contains(string(e.Data), "new.txt") {
			t.Errorf("fact leaks content: %s %s", e.Type, e.Data)
		}
	}
	got := map[string]string{}
	for _, e := range r.j.evs {
		got[e.Type] = string(e.Data)
	}
	if got["git.cloned"] == "" || got["git.pushed"] == "" || !strings.Contains(got["git.committed"], `"proof":"snapshot"`) {
		t.Errorf("facts: %v", got)
	}
}

// context "the fence cannot name the repo, the branch or the remote": a request carries a message and nothing
// else, and a push carries nothing.
func TestARequestCannotNameTheBranchOrTheRemote(t *testing.T) {
	r := newGitRig(t, nil)
	r.clone()
	os.WriteFile(filepath.Join(r.work, "x"), []byte("x"), 0o644)
	for _, body := range []string{`{"branch":"main"}`, `{"remote":"https://evil/x.git"}`, `{"force":true}`, `{"refspec":"+HEAD:main"}`, `[]`, `"x"`} {
		if rep := r.req("git.push", body); rep.Status != "refused" || rcode(rep) != "bad_request" {
			t.Errorf("push %s: %+v", body, rep)
		}
	}
	for _, body := range []string{`{"message":"m","branch":"main"}`, `{"message":""}`, `{"message":"a\u0000b"}`, `not json`, `{"message":"m"}{"x":1}`} {
		if rep := r.req("git.commit", body); rep.Status != "refused" || rcode(rep) != "bad_request" {
			t.Errorf("commit %s: %+v", body, rep)
		}
	}
	if r.door("branch", "--list", "ccsbx/*") != "" {
		t.Error("something was pushed")
	}
}

// context "a launcher or a typo names a branch that must never be pushed, or one that smuggles an option": the
// session branch is refused at start, so no request can ever reach it.
func TestTheSessionBranchAndRepoAreValidatedAtStart(t *testing.T) {
	base := GitConfig{Repo: "o/r", Base: "main", Branch: "ccsbx/s1", URL: "http://127.0.0.1:1/egress/git", GitDir: "/g", Work: "/w", Snapshot: "/s", Sid: "s", Gen: 1}
	for name, mut := range map[string]func(*GitConfig){
		"main":             func(c *GitConfig) { c.Branch = "main" },
		"MASTER":           func(c *GitConfig) { c.Branch = "MASTER" },
		"HEAD":             func(c *GitConfig) { c.Branch = "HEAD" },
		"option":           func(c *GitConfig) { c.Branch = "--upload-pack=x" },
		"leading dash":     func(c *GitConfig) { c.Branch = "-f" },
		"dotdot":           func(c *GitConfig) { c.Branch = "a..b" },
		"refs prefix":      func(c *GitConfig) { c.Branch = "refs/heads/x" },
		"lock suffix":      func(c *GitConfig) { c.Branch = "x.lock" },
		"trailing slash":   func(c *GitConfig) { c.Branch = "x/" },
		"hidden component": func(c *GitConfig) { c.Branch = "a/.b" },
		"space":            func(c *GitConfig) { c.Branch = "a b" },
		"same as base":     func(c *GitConfig) { c.Branch = "dev"; c.Base = "dev" },
		"repo dotdot":      func(c *GitConfig) { c.Repo = "../r" },
		"repo option":      func(c *GitConfig) { c.Repo = "-o/r" },
		"repo no owner":    func(c *GitConfig) { c.Repo = "r" },
		"repo .git suffix": func(c *GitConfig) { c.Repo = "o/r.git" },
		"base option":      func(c *GitConfig) { c.Base = "--force" },
		"url https":        func(c *GitConfig) { c.URL = "https://github.com/egress/git" },
		"url other host":   func(c *GitConfig) { c.URL = "http://evil:80/egress/git" },
		"url credentials":  func(c *GitConfig) { c.URL = "http://127.0.0.1:1@evil/x" },
		"dirs nested":      func(c *GitConfig) { c.Snapshot = "/w/snap" },
		"relative dir":     func(c *GitConfig) { c.GitDir = "g" },
	} {
		c := base
		mut(&c)
		if _, err := NewGitOps(c); err == nil {
			t.Errorf("%s accepted", name)
		}
	}
	if _, err := NewGitOps(base); err != nil {
		t.Fatalf("the good config: %v", err)
	}
}

// context "the fence wrote into /work before the clone (or a restart tries a second clone)": a checkout into a
// tree the fence may have planted a symlink in would write wherever the link points, so it is refused.
func TestCloneRefusesAWorktreeTheFenceHasWritten(t *testing.T) {
	r := newGitRig(t, nil)
	os.Symlink("/run/ccsbx/side", filepath.Join(r.work, "planted"))
	if err := r.g.Clone(context.Background()); err == nil || !strings.Contains(err.Error(), "not empty") {
		t.Fatalf("got %v", err)
	}
	if rep := r.req("git.commit", `{"message":"m"}`); rcode(rep) != "not_ready" {
		t.Errorf("commit before a clone: %+v", rep)
	}
	if rep := r.req("git.push", `{}`); rcode(rep) != "not_ready" {
		t.Errorf("push before a clone: %+v", rep)
	}
}

// context "a hostile worktree at commit time": a FIFO would hang a reader, a link out of the tree would read a
// host file, an oversized file or too many entries would fill the sidecar's
// memory or disk. Each refuses the commit, leaves nothing at the door, and nothing hangs.
func TestAHostileWorktreeNeverBecomesACommit(t *testing.T) {
	cases := map[string]struct {
		mut   func(*GitConfig)
		plant func(work string)
	}{
		"link out":       {nil, func(w string) { os.Symlink("../../../../etc/passwd", filepath.Join(w, "pw")) }},
		"absolute link":  {nil, func(w string) { os.Symlink("/etc/passwd", filepath.Join(w, "pw")) }},
		"oversized file": {func(c *GitConfig) { c.MaxFileBytes = 1024 }, func(w string) { os.WriteFile(filepath.Join(w, "big"), make([]byte, 4096), 0o644) }},
		"too many files": {func(c *GitConfig) { c.MaxFiles = 6 }, func(w string) {
			for i := 0; i < 20; i++ {
				os.WriteFile(filepath.Join(w, fmt.Sprintf("f%d", i)), []byte("x"), 0o644)
			}
		}},
		"hard link": {nil, func(w string) { os.Link(filepath.Join(w, "a.txt"), filepath.Join(w, "twin")) }},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			r := newGitRig(t, c.mut)
			r.clone()
			c.plant(r.work)
			done := make(chan hostd.Reply, 1)
			go func() { done <- r.req("git.commit", `{"message":"m"}`) }()
			select {
			case rep := <-done:
				if rep.Status == "ok" {
					t.Fatalf("a hostile tree was committed: %+v", rep)
				}
				t.Logf("refused as %v", rep.Result)
			case <-time.After(20 * time.Second):
				t.Fatal("commit hung")
			}
			if es, _ := os.ReadDir(r.snap); len(es) != 0 {
				t.Errorf("the snapshot was left behind: %d entries", len(es))
			}
			if rep := r.req("git.push", `{}`); rep.Status == "ok" {
				t.Errorf("something was pushed: %+v", rep)
			}
			if r.door("branch", "--list", "ccsbx/*") != "" {
				t.Error("a branch exists at the door")
			}
		})
	}
}

// context "git.push while a snapshot or another push is in progress": the worktree is being copied, so a push (and a
// second commit) answers busy instead of racing it.
func TestPushAndCommitAreBusyWhileAnOperationRuns(t *testing.T) {
	r := newGitRig(t, nil)
	r.clone()
	os.WriteFile(filepath.Join(r.work, "n"), []byte("n"), 0o644)
	r.g.mu.Lock() // what a running snapshot holds
	if rep := r.req("git.push", `{}`); rcode(rep) != "busy" {
		t.Errorf("push: %+v", rep)
	}
	if rep := r.req("git.commit", `{"message":"m"}`); rcode(rep) != "busy" {
		t.Errorf("commit: %+v", rep)
	}
	r.g.mu.Unlock()
	if rep := r.req("git.commit", `{"message":"m"}`); rep.Status != "ok" {
		t.Errorf("after: %+v", rep)
	}
}

// context "the door's compare API cannot take a huge push (U16: 300 files or 250 commits)": refused here with a
// reason, before a pack is built, and a push with nothing new is refused too.
func TestPushRefusesWhatTheDoorWouldAnyway(t *testing.T) {
	r := newGitRig(t, func(c *GitConfig) { c.MaxChangedFiles = 3; c.MaxCommits = 3 })
	r.clone()
	if rep := r.req("git.push", `{}`); rcode(rep) != "nothing_to_push" {
		t.Errorf("nothing new: %+v", rep)
	}
	for i := 0; i < 3; i++ {
		os.WriteFile(filepath.Join(r.work, fmt.Sprintf("f%d", i)), []byte("x"), 0o644)
	}
	if rep := r.req("git.commit", `{"message":"three files"}`); rep.Status != "ok" {
		t.Fatalf("%+v", rep)
	}
	if rep := r.req("git.push", `{}`); rcode(rep) != "too_many_files" {
		t.Errorf("3 files: %+v", rep)
	}
	if r.door("branch", "--list", "ccsbx/*") != "" {
		t.Error("pushed anyway")
	}
}

// context "someone else already put a different commit on the session branch at the door": a push must never
// become a force, so it is rejected and the door's branch is exactly as it was.
func TestAnAlreadyMovedBranchIsRejectedNotForced(t *testing.T) {
	r := newGitRig(t, nil)
	r.clone()
	other := filepath.Join(r.root, "other")
	sh(t, r.root, nil, "clone", "-q", r.bare, other)
	os.WriteFile(filepath.Join(other, "theirs"), []byte("t"), 0o644)
	sh(t, other, nil, "add", "-A")
	sh(t, other, nil, "commit", "-q", "-m", "theirs")
	sh(t, other, nil, "push", "-q", r.bare, "HEAD:refs/heads/ccsbx/s1")
	before := r.door("rev-parse", "refs/heads/ccsbx/s1")
	os.WriteFile(filepath.Join(r.work, "mine"), []byte("m"), 0o644)
	if rep := r.req("git.commit", `{"message":"mine"}`); rep.Status != "ok" {
		t.Fatalf("%+v", rep)
	}
	if rep := r.req("git.push", `{}`); rep.Status == "ok" || rcode(rep) != "push_rejected" {
		t.Fatalf("the diverged push: %+v", rep)
	}
	if after := r.door("rev-parse", "refs/heads/ccsbx/s1"); after != before {
		t.Fatalf("the door's branch moved from %s to %s", before, after)
	}
}

// context "the forwarder the fence uses injects the token for whoever asks": so no /git/ path may be reachable from it,
// whatever the fence sends. Only the sidecar's own git carries the secret, and then only the three smart routes work, and the
// secret never reaches the door.
func TestTheFenceCannotReachGitThroughTheForwarder(t *testing.T) {
	r := newGitRig(t, nil)
	r.clone()
	before := r.doorHits
	do := func(method, p, gate string) int {
		req, _ := http.NewRequest(method, r.fwdURL+p, strings.NewReader("{}"))
		if gate != "" {
			req.Header.Set("X-Ccsbx-Git", gate)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, p := range []string{"/egress/git/o/r.git/git-receive-pack", "/egress/git/o/r.git/git-upload-pack", "/egress/git/o/r.git/info/refs?service=git-receive-pack", "/egress/git/other/repo.git/git-receive-pack"} {
		for _, gate := range []string{"", "guess", strings.Repeat("a", 64), r.gate + "x", r.gate[:len(r.gate)-1]} {
			m := "POST"
			if strings.Contains(p, "info/refs") {
				m = "GET"
			}
			if code := do(m, p, gate); code != 403 {
				t.Errorf("%s %s with gate %q: %d", m, p, gate, code)
			}
		}
	}
	// the gate header alone opens nothing but git, and git only by its three routes
	for _, c := range []struct{ method, p string }{
		{"GET", "/egress/git/o/r.git/HEAD"}, {"GET", "/egress/git/o/r.git/objects/info/packs"}, {"PUT", "/egress/git/o/r.git/info/refs"},
		{"DELETE", "/egress/git/o/r.git/git-receive-pack"}, {"GET", "/egress/git/o/r.git/git-receive-pack"}, {"GET", "/egress/git/o/r.git/info/refs/extra"},
		{"GET", "/egress/github/repos/o/r"}, {"POST", "/egress/gitx/o/r.git/git-receive-pack"},
	} {
		if code := do(c.method, c.p, r.gate); code != 403 {
			t.Errorf("with the gate, %s %s: %d", c.method, c.p, code)
		}
	}
	// a wrong secret is refused even on a path the fence may normally use: a guess is never half-accepted
	if code := do("GET", "/egress/github/repos/o/r", "guess"); code != 403 {
		t.Errorf("a wrong gate on an ordinary route: %d", code)
	}
	if r.doorHits != before {
		t.Errorf("the door was asked %d times by requests that had to be refused", r.doorHits-before)
	}
	// the sidecar's own git (which sends the gate) works, and the door never sees the gate
	os.WriteFile(filepath.Join(r.work, "n"), []byte("n"), 0o644)
	if rep := r.req("git.commit", `{"message":"m"}`); rep.Status != "ok" {
		t.Fatal(rep)
	}
	if rep := r.req("git.push", `{}`); rep.Status != "ok" {
		t.Fatal(rep)
	}
	for _, h := range r.doorSaw {
		for k, v := range h {
			if strings.EqualFold(k, "X-Ccsbx-Git") || strings.Contains(strings.Join(v, ""), r.gate) {
				t.Errorf("the door saw the gate in %s", k)
			}
		}
	}
}

// context "H1 refuses these too, but the snapshot is the first thing that touches the fence's tree and it runs in
// the sidecar's own memory and disk": its caps and refusals stand on their own, without H1 behind them.
func TestTheSnapshotRefusesOnItsOwnWhatItShouldNotCopy(t *testing.T) {
	for name, c := range map[string]struct {
		mut   func(*GitConfig)
		plant func(w string)
		want  string
	}{
		"entries": {func(c *GitConfig) { c.MaxFiles = 5 }, func(w string) {
			for i := 0; i < 9; i++ {
				os.WriteFile(filepath.Join(w, fmt.Sprint("f", i)), nil, 0o644)
			}
		}, "more than 5 entries"},
		"total bytes": {func(c *GitConfig) { c.MaxSnapshotBytes = 100 }, func(w string) {
			os.WriteFile(filepath.Join(w, "a"), make([]byte, 80), 0o644)
			os.WriteFile(filepath.Join(w, "b"), make([]byte, 80), 0o644)
		}, "over 100 bytes"},
		"file bytes": {func(c *GitConfig) { c.MaxFileBytes = 10 }, func(w string) { os.WriteFile(filepath.Join(w, "a"), make([]byte, 80), 0o644) }, "the cap is 10"},
		"depth": {func(c *GitConfig) { c.MaxDepth = 3 }, func(w string) {
			os.MkdirAll(filepath.Join(w, "a", "b", "c", "d"), 0o755)
			os.WriteFile(filepath.Join(w, "a", "b", "c", "d", "f"), []byte("x"), 0o644)
		}, "nested more than 3 levels"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newGitRig(t, c.mut)
			r.clone()
			c.plant(r.work)
			if _, err := r.g.snapshot(context.Background()); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

// context "a directory the listing named is swapped for a link to the sidecar's own state before the copy reads it": os.Root
// refuses the path, so the copy fails and nothing of the target is in the snapshot. (A link that was a link at the Lstat is
// only ever recreated as a link, never read through.)
func TestTheSnapshotNeverReadsThroughALinkSwappedInAfterTheListing(t *testing.T) {
	r := newGitRig(t, nil)
	r.clone()
	secretDir := filepath.Join(r.root, "side-state")
	os.MkdirAll(secretDir, 0o700)
	os.WriteFile(filepath.Join(secretDir, "f"), []byte("LEDGER-CONTENT"), 0o600)
	os.Mkdir(filepath.Join(r.work, "d"), 0o755)
	os.WriteFile(filepath.Join(r.work, "d", "f"), []byte("innocent"), 0o644)
	os.Symlink(secretDir, filepath.Join(r.work, "l")) // a link out that was already a link
	r.g.hookEntry = func(p string) {
		if p == "d/f" { // the fence swaps the directory between the listing and the copy
			os.RemoveAll(filepath.Join(r.work, "d"))
			os.Symlink(secretDir, filepath.Join(r.work, "d"))
		}
	}
	_, err := r.g.snapshot(context.Background())
	if err == nil {
		t.Fatal("the swapped directory was copied through")
	}
	filepath.Walk(r.snap, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "LEDGER") {
				t.Errorf("%s holds a file read through a link", p)
			}
		}
		return nil
	})
}

// context "an ordinary project has trees git ignores (node_modules, build output) that hold hard links, sockets and huge
// files": none of it may block or fill a commit, so only what git would commit is copied.
func TestIgnoredTreesNeverEnterTheSnapshot(t *testing.T) {
	r := newGitRig(t, func(c *GitConfig) { c.MaxFiles = 50; c.MaxFileBytes = 4096; c.MaxSnapshotBytes = 1 << 20 })
	r.clone()
	os.WriteFile(filepath.Join(r.work, ".gitignore"), []byte("node_modules/\ntarget/\n"), 0o644)
	nm := filepath.Join(r.work, "node_modules", "pkg")
	os.MkdirAll(nm, 0o755)
	os.WriteFile(filepath.Join(nm, "a.js"), []byte("a"), 0o644)
	os.Link(filepath.Join(nm, "a.js"), filepath.Join(nm, "b.js")) // a hard-link store
	syscall.Mkfifo(filepath.Join(nm, "pipe"), 0o644)
	for i := 0; i < 200; i++ { // more entries than the cap
		os.WriteFile(filepath.Join(nm, fmt.Sprint("f", i)), nil, 0o644)
	}
	os.MkdirAll(filepath.Join(r.work, "target"), 0o755)
	os.WriteFile(filepath.Join(r.work, "target", "huge.bin"), make([]byte, 1<<20), 0o644) // over the per-file cap
	os.WriteFile(filepath.Join(r.work, "real.txt"), []byte("r"), 0o644)
	if rep := r.req("git.commit", `{"message":"m"}`); rep.Status != "ok" {
		t.Fatalf("%+v", rep)
	}
	if rep := r.req("git.push", `{}`); rep.Status != "ok" {
		t.Fatalf("%+v", rep)
	}
	tree := r.door("ls-tree", "-r", "--name-only", "refs/heads/ccsbx/s1")
	if !strings.Contains(tree, "real.txt") || !strings.Contains(tree, ".gitignore") || strings.Contains(tree, "node_modules") || strings.Contains(tree, "target") {
		t.Errorf("tree at the door:\n%s", tree)
	}
}

// context "a nested .git (file, link or directory) makes git read a path the fence chose, outside the snapshot, while it
// commits": none is ever copied, the commit still succeeds, and the journal says how many were skipped. A .git file that
// points at a gitdir whose HEAD is a FIFO must not hang the commit.
func TestANestedDotGitIsNeverCopied(t *testing.T) {
	r := newGitRig(t, nil)
	r.clone()
	fifoDir := filepath.Join(r.work, "trap")
	os.Mkdir(fifoDir, 0o755)
	os.MkdirAll(filepath.Join(r.work, "a", ".git"), 0o755) // a .git directory that is not a repo
	os.WriteFile(filepath.Join(r.work, "a", ".git", "config"), []byte("[core]\n"), 0o644)
	os.WriteFile(filepath.Join(r.work, "a", "keep.txt"), []byte("k"), 0o644)
	os.Mkdir(filepath.Join(r.work, "b"), 0o755)
	os.WriteFile(filepath.Join(r.work, "b", ".git"), []byte("gitdir: "+fifoDir+"\n"), 0o644) // a .git file pointing at a path of the fence's choice
	os.Mkdir(filepath.Join(r.work, "c"), 0o755)
	os.Symlink(r.g.cfg.GitDir, filepath.Join(r.work, "c", ".git")) // a link to the sidecar's own gitdir
	os.WriteFile(filepath.Join(r.work, "c", "also.txt"), []byte("c"), 0o644)
	nested := filepath.Join(r.work, "vendored") // a REAL nested repository: git lists it as "vendored/"
	sh(t, r.root, nil, "init", "-q", "-b", "main", nested)
	os.WriteFile(filepath.Join(nested, "inner.txt"), []byte("i"), 0o644)
	sh(t, nested, nil, "add", "-A")
	sh(t, nested, nil, "commit", "-q", "-m", "inner")
	syscall.Mkfifo(filepath.Join(r.work, "pipe"), 0o644) // a FIFO next to real work: never listed, never a hang
	done := make(chan hostd.Reply, 1)
	go func() { done <- r.req("git.commit", `{"message":"m"}`) }()
	select {
	case rep := <-done:
		if rep.Status != "ok" {
			t.Fatalf("%+v", rep)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("commit hung")
	}
	if rep := r.req("git.push", `{}`); rep.Status != "ok" {
		t.Fatalf("%+v", rep)
	}
	tree := r.door("ls-tree", "-r", "--name-only", "refs/heads/ccsbx/s1")
	if strings.Contains(tree, ".git") || strings.Contains(tree, "inner.txt") || strings.Contains(tree, "pipe") || !strings.Contains(tree, "a/keep.txt") || !strings.Contains(tree, "c/also.txt") {
		t.Errorf("tree at the door: %s", tree)
	}
	found := false
	for _, e := range r.j.evs {
		if e.Type == "git.committed" && strings.Contains(string(e.Data), `"skipped":1`) {
			found = true
		}
	}
	if !found {
		t.Errorf("the skipped nested repository is not journalled: %+v", r.j.evs)
	}
}

// context "the fence swaps a directory in /work for a link to the sidecar's own volume while the clone copies the
// checkout in (or has put a link where a file goes)": the copy creates everything through os.Root with O_EXCL, so the
// clone fails, and nothing was written outside /work.
func TestTheCloneCannotBeRedirectedOutOfTheWorktree(t *testing.T) {
	for name, plant := range map[string]func(r *gitRig, outside string){
		"directory swapped for a link": func(r *gitRig, outside string) { os.Symlink(outside, filepath.Join(r.work, "sub")) },
		"file replaced by a link": func(r *gitRig, outside string) {
			os.Symlink(filepath.Join(outside, "victim"), filepath.Join(r.work, "a.txt"))
		},
	} {
		t.Run(name, func(t *testing.T) {
			r := newGitRig(t, nil)
			outside := filepath.Join(r.root, "side-state")
			os.MkdirAll(outside, 0o700)
			os.WriteFile(filepath.Join(outside, "victim"), []byte("UNTOUCHED"), 0o600)
			r.g.hookCopy = func() { plant(r, outside) } // after the emptiness check, before the files are created
			if err := r.g.Clone(context.Background()); err == nil {
				t.Fatal("the clone went through a planted link")
			}
			if es, _ := os.ReadDir(outside); len(es) != 1 {
				t.Errorf("something was written outside the worktree: %v", es)
			}
			if b, _ := os.ReadFile(filepath.Join(outside, "victim")); string(b) != "UNTOUCHED" {
				t.Errorf("the victim was rewritten: %q", b)
			}
		})
	}
}

// context "git is run on a gitdir the sidecar made, but a config include or info/attributes could appear in it": H1's repo
// check runs before every command, fetch and push too, so a tampered gitdir stops the push.
func TestGitIsRefusedOnATamperedGitDirBeforeEveryCommand(t *testing.T) {
	r := newGitRig(t, nil)
	r.clone()
	os.WriteFile(filepath.Join(r.work, "n"), []byte("n"), 0o644)
	if rep := r.req("git.commit", `{"message":"m"}`); rep.Status != "ok" {
		t.Fatal(rep)
	}
	cfg, _ := os.OpenFile(filepath.Join(r.g.cfg.GitDir, "config"), os.O_APPEND|os.O_WRONLY, 0)
	cfg.WriteString("[include]\n\tpath = /etc/hostname\n")
	cfg.Close()
	if rep := r.req("git.push", `{}`); rep.Status == "ok" {
		t.Fatalf("pushed from a gitdir with an include: %+v", rep)
	}
	if r.door("branch", "--list", "ccsbx/*") != "" {
		t.Error("something was pushed")
	}
}

// context "a push is asked for while a commit's snapshot is being made": the snapshot is held open by a seam, the push
// is busy, and the commit then finishes normally.
func TestAPushIsBusyWhileARealSnapshotIsBeingMade(t *testing.T) {
	r := newGitRig(t, nil)
	r.clone()
	os.WriteFile(filepath.Join(r.work, "n"), []byte("n"), 0o644)
	inSnap, release := make(chan struct{}), make(chan struct{})
	r.g.hookSnapshot = func() { close(inSnap); <-release }
	done := make(chan hostd.Reply, 1)
	go func() { done <- r.req("git.commit", `{"message":"m"}`) }()
	<-inSnap
	if rep := r.req("git.push", `{}`); rcode(rep) != "busy" {
		t.Errorf("push during a snapshot: %+v", rep)
	}
	if rep := r.req("git.commit", `{"message":"again"}`); rcode(rep) != "busy" {
		t.Errorf("second commit during a snapshot: %+v", rep)
	}
	close(release)
	if rep := <-done; rep.Status != "ok" {
		t.Fatalf("the first commit: %+v", rep)
	}
}

// context "the sidecar container restarts (its volumes do not) or the first fetch fails": a restart finds the work done
// and carries on without touching /work; a failure before anything reached /work leaves the gitdir empty so a retry can
// start clean.
func TestARestartCarriesOnAndAFailedCloneCanBeRetried(t *testing.T) {
	r := newGitRig(t, nil)
	r.clone()
	os.WriteFile(filepath.Join(r.work, "n"), []byte("n"), 0o644)
	if rep := r.req("git.commit", `{"message":"before the restart"}`); rep.Status != "ok" {
		t.Fatal(rep)
	}
	again, err := NewGitOps(r.g.cfg) // a new process, the same volumes
	if err != nil {
		t.Fatal(err)
	}
	if err := again.Clone(context.Background()); err != nil {
		t.Fatalf("a restarted sidecar must carry on, not fail on a non-empty worktree: %v", err)
	}
	r.g = again
	os.WriteFile(filepath.Join(r.work, "m"), []byte("m"), 0o644)
	if rep := r.req("git.commit", `{"message":"after"}`); rep.Status != "ok" {
		t.Fatalf("after the restart: %+v", rep)
	}
	if rep := r.req("git.push", `{}`); rep.Status != "ok" {
		t.Fatalf("push after the restart: %+v", rep)
	}

	// a clone that fails at the fetch leaves nothing behind
	f := newGitRig(t, func(c *GitConfig) { c.Base = "no-such-branch" })
	if err := f.g.Clone(context.Background()); err == nil {
		t.Fatal("a missing base cloned")
	}
	if es, _ := os.ReadDir(f.g.cfg.GitDir); len(es) != 0 {
		t.Errorf("a failed clone left %d entries in the gitdir", len(es))
	}
	if es, _ := os.ReadDir(f.work); len(es) != 0 {
		t.Errorf("a failed clone left %d entries in the worktree", len(es))
	}
	cfg := f.g.cfg
	cfg.Base = "main"
	retry, _ := NewGitOps(cfg)
	if err := retry.Clone(context.Background()); err != nil {
		t.Fatalf("the retry: %v", err)
	}
}

// context "a fence plants a .git file that names a directory whose HEAD is a FIFO, because git opens it to decide whether the
// directory is a repository": the live listing stalls, so it has its own short deadline. The commit is refused, the lock is
// released (a push is not busy afterwards), and once the trap is gone the next commit works.
func TestAGitFileAtAFifoStallsTheListingOnlyBrieflyAndOnlyThatCommit(t *testing.T) {
	r := newGitRig(t, func(c *GitConfig) { c.ListTimeout = time.Second })
	r.clone()
	trap := filepath.Join(r.work, "trap")
	os.Mkdir(trap, 0o755)
	syscall.Mkfifo(filepath.Join(trap, "HEAD"), 0o644)
	os.Mkdir(filepath.Join(r.work, "b"), 0o755)
	os.WriteFile(filepath.Join(r.work, "b", ".git"), []byte("gitdir: "+trap+"\n"), 0o644)
	os.WriteFile(filepath.Join(r.work, "real"), []byte("r"), 0o644)
	start := time.Now()
	rep := r.req("git.commit", `{"message":"m"}`)
	if rep.Status == "ok" || time.Since(start) > 15*time.Second {
		t.Fatalf("%+v after %v", rep, time.Since(start))
	}
	if rep := r.req("git.push", `{}`); rcode(rep) == "busy" {
		t.Error("the lock was not released")
	}
	os.RemoveAll(filepath.Join(r.work, "b"))
	os.RemoveAll(trap)
	if rep := r.req("git.commit", `{"message":"m"}`); rep.Status != "ok" {
		t.Fatalf("after the trap is gone: %+v", rep)
	}
}

// context "a fence provokes refusals in a loop": the journal gets a bounded number of git.refused facts and then ONE fact that
// the rest are not written, so a silent journal is never mistaken for a quiet session.
func TestRefusalFactsAreBoundedAndSaySoOnce(t *testing.T) {
	r := newGitRig(t, func(c *GitConfig) { c.MaxFacts = 3 })
	r.clone()
	for i := 0; i < 10; i++ {
		r.req("git.push", `{"branch":"main"}`)
	}
	refused, suppressed := 0, 0
	for _, e := range r.j.evs {
		switch e.Type {
		case "git.refused":
			refused++
		case "git.facts_suppressed":
			suppressed++
		}
	}
	if refused != 3 || suppressed != 1 {
		t.Fatalf("%d refused facts and %d suppression notes, want 3 and 1", refused, suppressed)
	}
}

// context "the fence plants a TOP-LEVEL .git file that redirects git to a gitdir of its own, with hooks in it": git is told its
// GIT_DIR and never discovers, so the redirect is ignored, no hook runs, and no .git entry is committed (the change next to it
// still is).
func TestAGitFileInTheWorktreeNeverRedirectsOrRunsAnything(t *testing.T) {
	r := newGitRig(t, nil)
	r.clone()
	evil := filepath.Join(r.root, "evil.git")
	sh(t, r.root, nil, "init", "--bare", "-q", evil)
	marker := filepath.Join(r.root, "HOOK-RAN")
	os.WriteFile(filepath.Join(evil, "hooks", "pre-commit"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	os.WriteFile(filepath.Join(evil, "hooks", "post-commit"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	os.WriteFile(filepath.Join(r.work, ".git"), []byte("gitdir: "+evil+"\n"), 0o644)
	os.WriteFile(filepath.Join(r.work, ".gitattributes"), []byte("* filter=evil diff=evil\n"), 0o644)
	os.WriteFile(filepath.Join(r.work, "real.txt"), []byte("r"), 0o644)
	if rep := r.req("git.commit", `{"message":"m"}`); rep.Status != "ok" {
		t.Fatalf("%+v", rep)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook from the planted gitdir ran")
	}
	if rep := r.req("git.push", `{}`); rep.Status != "ok" {
		t.Fatalf("%+v", rep)
	}
	files := r.door("ls-tree", "-r", "--name-only", "refs/heads/ccsbx/s1")
	if strings.Contains(files, ".git\n") || strings.HasSuffix(files, ".git") || !strings.Contains(files, "real.txt") {
		t.Errorf("tree at the door: %q", files)
	}
}
