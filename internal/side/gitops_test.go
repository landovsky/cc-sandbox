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
	counted := http.HandlerFunc(func(w http.ResponseWriter, q *http.Request) { r.doorHits++; backend.ServeHTTP(w, q) })
	door := httptest.NewServer(token.NewVerifier(ring, "ccsbx-egress").Middleware(counted, nil))
	t.Cleanup(door.Close)
	du, _ := url.Parse(door.URL)
	r.key, r.doorURL = key, door.URL
	f, err := fwd.New(fwd.Config{Token: tok, Key: gen, Doors: map[string]*url.URL{"egress": du}, MaxBody: 8 << 20})
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
		Work: r.work, Snapshot: r.snap, Sid: "s0worker", Gen: 1, Journal: r.j, Timeout: 30 * time.Second}
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
// host file, a .git inside would redirect git, an oversized file or too many entries would fill the sidecar's
// memory or disk. Each refuses the commit, leaves nothing at the door, and nothing hangs.
func TestAHostileWorktreeNeverBecomesACommit(t *testing.T) {
	cases := map[string]struct {
		mut   func(*GitConfig)
		plant func(work string)
	}{
		"fifo":          {nil, func(w string) { syscall.Mkfifo(filepath.Join(w, "pipe"), 0o644) }},
		"link out":      {nil, func(w string) { os.Symlink("../../../../etc/passwd", filepath.Join(w, "pw")) }},
		"absolute link": {nil, func(w string) { os.Symlink("/etc/passwd", filepath.Join(w, "pw")) }},
		"nested .git dir": {nil, func(w string) {
			os.MkdirAll(filepath.Join(w, "sub", ".git"), 0o755)
			os.WriteFile(filepath.Join(w, "sub", ".git", "config"), []byte("[core]\n"), 0o644)
		}},
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

// context "git's other HTTP paths (dumb protocol, objects, a different repo's receive-pack under another door
// prefix) are not routes": the forwarder answers 403 before the door is asked.
func TestOnlyTheSmartGitRoutesAreOpen(t *testing.T) {
	r := newGitRig(t, nil)
	before := r.doorHits
	for _, c := range []struct{ method, p string }{
		{"GET", "/egress/git/o/r.git/HEAD"}, {"GET", "/egress/git/o/r.git/objects/info/packs"}, {"PUT", "/egress/git/o/r.git/info/refs"},
		{"DELETE", "/egress/git/o/r.git/git-receive-pack"}, {"GET", "/egress/git/o/r.git/git-receive-pack"}, {"GET", "/egress/git/o/r.git/info/refs/extra"},
		{"POST", "/egress/gitx/o/r.git/git-receive-pack"},
	} {
		req, _ := http.NewRequest(c.method, r.fwdURL+c.p, strings.NewReader("{}"))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 403 {
			t.Errorf("%s %s: %d", c.method, c.p, resp.StatusCode)
		}
	}
	if r.doorHits != before {
		t.Errorf("the door was asked %d times", r.doorHits-before)
	}
}

// context "the checkout needs group write for the fence but nothing else of ours does": the umask is widened only
// around the checkout and is the process's own again afterwards.
func TestTheUmaskIsOnlyWidenedAroundTheCheckout(t *testing.T) {
	old := syscall.Umask(0o077)
	defer syscall.Umask(old)
	r := newGitRig(t, nil)
	r.clone()
	if now := syscall.Umask(0o077); now != 0o077 {
		t.Fatalf("the umask is %o after the clone", now)
	}
}

// context "the fence plants a .git file that redirects git to a gitdir of its own, with a hook in it": git is
// told its GIT_DIR and never discovers, so the redirect is ignored, the hook never runs, and no .git entry is
// ever committed (the change next to it still is).
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
		"file bytes":  {func(c *GitConfig) { c.MaxFileBytes = 10 }, func(w string) { os.WriteFile(filepath.Join(w, "a"), make([]byte, 80), 0o644) }, "the cap is 10"},
		"nested .git": {nil, func(w string) { os.MkdirAll(filepath.Join(w, "d", ".git"), 0o755) }, "a .git inside"},
		"fifo":        {nil, func(w string) { syscall.Mkfifo(filepath.Join(w, "p"), 0o644) }, "not a regular file"},
	} {
		t.Run(name, func(t *testing.T) {
			r := newGitRig(t, c.mut)
			c.plant(r.work)
			if _, err := r.g.snapshot(); err == nil || !strings.Contains(err.Error(), c.want) {
				t.Fatalf("got %v, want %q", err, c.want)
			}
		})
	}
}

// context "a link inside the worktree that leaves it must never be followed by the copy": os.Root refuses it, so a
// link to the sidecar's own state (which exists in the sidecar's mount namespace) is a link in the snapshot and
// nothing more; its target is never read into it.
func TestTheSnapshotNeverReadsThroughALinkOutOfTheTree(t *testing.T) {
	r := newGitRig(t, nil)
	secret := filepath.Join(r.root, "side-state-secret")
	os.WriteFile(secret, []byte("LEDGER-CONTENT"), 0o600)
	os.Symlink(secret, filepath.Join(r.work, "l"))
	os.Mkdir(filepath.Join(r.work, "d"), 0o755)
	os.Symlink(r.root, filepath.Join(r.work, "d", "dirlink"))
	if _, err := r.g.snapshot(); err != nil {
		t.Fatal(err)
	}
	filepath.Walk(r.snap, func(p string, fi os.FileInfo, err error) error {
		if err == nil && fi.Mode().IsRegular() {
			if b, _ := os.ReadFile(p); strings.Contains(string(b), "LEDGER") {
				t.Errorf("%s holds a file read through a link", p)
			}
		}
		return nil
	})
	if fi, err := os.Lstat(filepath.Join(r.snap, "l")); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the link should be a link in the snapshot: %v", err)
	}
	if es, _ := os.ReadDir(filepath.Join(r.snap, "d")); len(es) != 1 || !strings.Contains(es[0].Name(), "dirlink") {
		t.Errorf("the directory link was followed: %v", es)
	}
}
