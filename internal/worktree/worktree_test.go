// Tests of the worktree builder (R-WT). Why each exists: the builder runs git against a repository the owner named but
// somebody else wrote, as the owner, on the owner's machine, so every case is a way a hostile repository or a hostile spec
// could make git run something, write somewhere, fill a disk, destroy another session's files, or leave a trace of a start that
// did not happen.
package worktree

import (
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Fatalf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// rig: a local "remote" with a main branch, a state dir, a worktree root, and a builder that uses the remote instead of github.
func rig(t *testing.T) (*Builder, string, string) {
	t.Helper()
	remote := t.TempDir()
	gitIn(t, remote, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(remote, "README"), []byte("hello\n"), 0o644)
	gitIn(t, remote, "add", "README")
	gitIn(t, remote, "commit", "-q", "-m", "first")
	root, state, home := t.TempDir(), t.TempDir(), t.TempDir()
	b := &Builder{StateDir: state, Root: root, Protocols: []string{"file"}, Env: []string{"HOME=" + home},
		Remote: func(string) string { return "file://" + remote }}
	return b, remote, home
}

const repo = "github.com/acme/widgets"

func cloneOf(b *Builder) string {
	return filepath.Join(b.StateDir, "clones", "tomas", "acme", "widgets.git")
}

func prep(t *testing.T, b *Builder, label, branch string) *Handle {
	t.Helper()
	h, err := b.Prepare("tomas", label, repo, "main", branch)
	if err != nil {
		t.Fatal(err)
	}
	return h
}

// context: the point of the package: a worktree of the named base on a new branch, under the root, with the clone cache
// OUTSIDE every root (a start naming the cache as workdir.path must not be able to mount it).
func TestPrepareBuildsAWorktreeOfTheBaseOnANewBranchAndKeepsTheCloneOutOfTheRoot(t *testing.T) {
	b, remote, _ := rig(t)
	want := gitIn(t, remote, "rev-parse", "main")
	h := prep(t, b, "cc-sandbox:Worker 1", "ccsbx/326q")
	if filepath.Dir(h.Path) != b.root() || !strings.HasPrefix(filepath.Base(h.Path), "cc-sandbox-worker-1-") {
		t.Fatalf("worktree at %s", h.Path)
	}
	if got := gitIn(t, h.Path, "rev-parse", "HEAD"); got != want {
		t.Fatalf("HEAD %s, want %s", got, want)
	}
	if br := gitIn(t, h.Path, "rev-parse", "--abbrev-ref", "HEAD"); br != "ccsbx/326q" {
		t.Fatalf("branch %s", br)
	}
	if _, err := os.Stat(filepath.Join(h.Path, "README")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(cloneOf(b)); err != nil {
		t.Fatalf("the clone is not in the state dir: %v", err)
	}
	if ents, _ := os.ReadDir(b.Root); len(ents) != 1 {
		t.Fatalf("the root holds %d entries, want only the worktree", len(ents))
	}
	if h2 := prep(t, b, "cc-sandbox:w2", "ccsbx/other"); h2.Path == h.Path {
		t.Fatal("two sessions share a worktree")
	}
}

// context (C5): every value is one argv element after the rules; an option, a range, a doubled slash or a bad charset in
// base or branch, or a repo that is not github.com/<o>/<r>, is refused before any git process touches the repository.
func TestHostileSpecValuesAreRefusedBeforeGitSeesThem(t *testing.T) {
	b, _, _ := rig(t)
	for name, c := range map[string][3]string{
		"a base that is an option":     {repo, "--upload-pack=touch /tmp/pwned", "ok"},
		"a branch that is an option":   {repo, "main", "-b"},
		"a revision range as the base": {repo, "main..main", "ok"},
		"a doubled slash in a branch":  {repo, "main", "a//b"},
		"a lock suffix":                {repo, "main", "x.lock"},
		"a reflog form":                {repo, "main", "x@{1}"},
		"a repo that is a url":         {"https://github.com/a/b", "main", "ok"},
		"another host":                 {"gitlab.com/a/b", "main", "ok"},
		"a repo with a path escape":    {"github.com/../b", "main", "ok"},
	} {
		if h, err := b.Prepare("tomas", "x", c[0], c[1], c[2]); err == nil {
			t.Errorf("%s was accepted: %s", name, h.Path)
		}
	}
	if ents, _ := os.ReadDir(b.Root); len(ents) != 0 {
		t.Fatalf("a refused start left %d entries in the root", len(ents))
	}
	if _, err := os.Stat(filepath.Join(b.StateDir, "clones")); err == nil {
		t.Fatal("a refused start created a clone directory")
	}
}

// context: refusals carry fixed text only: no repo name, path or git output reaches the caller.
func TestRefusalsAreFixedTextWithNoPathsOrGitOutput(t *testing.T) {
	b, _, _ := rig(t)
	_, err := b.Prepare("tomas", "x", repo, "no-such-branch", "ok")
	if err == nil {
		t.Fatal("a missing base was accepted")
	}
	for _, leak := range []string{b.Root, b.StateDir, "acme", "widgets", "fatal", "couldn't"} {
		if strings.Contains(err.Error(), leak) {
			t.Errorf("the refusal %q leaks %q", err, leak)
		}
	}
	if _, ok := err.(*Error); !ok {
		t.Fatalf("%T", err)
	}
	// a failed first fetch leaves no empty clone behind (it would otherwise keep a wrong origin for the next start)
	if _, err := os.Stat(cloneOf(b)); err == nil {
		t.Fatal("a clone created by a failed fetch was kept")
	}
}

// context (R-WT review M1): a start refused AFTER its worktree was built used to delete the directory but leave the branch,
// so the same spec was refused "the branch already exists" for good. Removal takes the branch with it.
func TestARemovedWorktreeFreesItsBranchNameForTheNextStart(t *testing.T) {
	b, _, _ := rig(t)
	h := prep(t, b, "a", "feature")
	if err := b.Remove(h); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(h.Path); err == nil {
		t.Fatal("the worktree is still there")
	}
	if out := gitIn(t, cloneOf(b), "branch", "--list", "feature"); out != "" {
		t.Fatalf("the branch survived: %q", out)
	}
	if _, err := b.Prepare("tomas", "a", repo, "main", "feature"); err != nil {
		t.Fatalf("the same spec again: %v", err)
	}
	// and while the branch is in use, a second session cannot take it
	if _, err := b.Prepare("tomas", "b", repo, "main", "feature"); err == nil {
		t.Fatal("the same branch was handed to a second session")
	}
}

// context (R-WT review S3): the owner's global configuration is how a FETCH is authorised, but it must not be able to turn hooks on
// or let a hostile .gitattributes choose one of the owner's filter drivers to run during the checkout.
func TestTheOwnersConfigCannotRunHooksOrFiltersDuringCheckout(t *testing.T) {
	b, remote, home := rig(t)
	hooks := t.TempDir()
	hookMarker, filterMarker := filepath.Join(t.TempDir(), "hook"), filepath.Join(t.TempDir(), "filter")
	os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\ntouch "+hookMarker+"\n"), 0o755)
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\thooksPath = "+hooks+"\n[filter \"foo\"]\n\tsmudge = sh -c 'touch "+filterMarker+"; cat'\n\trequired = false\n"), 0o644)
	os.WriteFile(filepath.Join(remote, ".gitattributes"), []byte("* filter=foo\n"), 0o644)
	gitIn(t, remote, "add", ".gitattributes")
	gitIn(t, remote, "commit", "-q", "-m", "attrs")
	prep(t, b, "h", "hk")
	if _, err := os.Stat(hookMarker); err == nil {
		t.Fatal("a hook from the owner's config ran during the checkout")
	}
	if _, err := os.Stat(filterMarker); err == nil {
		t.Fatal("a filter driver of the owner's config was selected by the repository's .gitattributes")
	}
}

// context (R-WT review M2): the size cap used to delete the whole clone, including the object store and the registration of every
// live worktree of that repo. An oversized fetch into a clone that is in use is rolled back; the live worktree keeps working.
func TestAnOversizedFetchNeverDeletesACloneThatLiveWorktreesUse(t *testing.T) {
	b, remote, _ := rig(t)
	live := prep(t, b, "live", "live-branch")
	big := make([]byte, 3<<20)
	rand.Read(big)
	gitIn(t, remote, "checkout", "-q", "-b", "big")
	os.WriteFile(filepath.Join(remote, "big.bin"), big, 0o644)
	gitIn(t, remote, "add", "big.bin")
	gitIn(t, remote, "commit", "-q", "-m", "big")
	b.Limits.MaxCloneMB = 2
	if _, err := b.Prepare("tomas", "big", repo, "big", "bg"); err == nil {
		t.Fatal("a clone over the cap was accepted")
	}
	if _, err := os.Stat(cloneOf(b)); err != nil {
		t.Fatalf("the shared clone was deleted: %v", err)
	}
	if out := gitIn(t, live.Path, "status", "--porcelain"); out != "" || gitIn(t, live.Path, "rev-parse", "--abbrev-ref", "HEAD") != "live-branch" {
		t.Fatalf("the live worktree is broken: %q", out)
	}
	if out := gitIn(t, cloneOf(b), "for-each-ref", "refs/remotes/origin/big"); out != "" {
		t.Fatalf("the rolled-back ref survived: %q", out)
	}
	// a clone THIS call created and could not keep is deleted
	b2, remote2, _ := rig(t)
	rand.Read(big)
	os.WriteFile(filepath.Join(remote2, "big.bin"), big, 0o644)
	gitIn(t, remote2, "add", "big.bin")
	gitIn(t, remote2, "commit", "-q", "-m", "big")
	b2.Limits.MaxCloneMB = 1
	if _, err := b2.Prepare("tomas", "big", repo, "main", "bg"); err == nil {
		t.Fatal("accepted")
	}
	if _, err := os.Stat(cloneOf(b2)); err == nil {
		t.Fatal("a new oversized clone was kept")
	}
}

// context (R-WT review M3): the cap measured the compressed clone, so a repository of highly compressible blobs (or one blob under
// many paths) was a few KiB as a clone and gigabytes as files. The tree is measured before anything is written.
func TestACheckoutOverTheCapIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	b, remote, _ := rig(t)
	zeros := make([]byte, 4<<20)
	for i := 0; i < 5; i++ {
		os.WriteFile(filepath.Join(remote, "z"+string(rune('a'+i))), zeros, 0o644)
	}
	gitIn(t, remote, "add", ".")
	gitIn(t, remote, "commit", "-q", "-m", "zeros")
	b.Limits.MaxCheckout = 10 // MiB: the clone is a few KiB, the checkout would be 20 MiB
	if _, err := b.Prepare("tomas", "z", repo, "main", "zb"); err == nil {
		t.Fatal("a zip-bomb checkout was accepted")
	}
	if ents, _ := os.ReadDir(b.Root); len(ents) != 0 {
		t.Fatalf("something was written to the root: %v", ents)
	}
	if out := gitIn(t, cloneOf(b), "branch", "--list", "zb"); out != "" {
		t.Fatalf("a branch was made: %q", out)
	}
	b.Limits.MaxCheckout, b.Limits.MaxFiles = 0, 3 // a file-count bomb
	if _, err := b.Prepare("tomas", "z", repo, "main", "zb"); err == nil {
		t.Fatal("a tree of more files than the cap was accepted")
	}
}

// context: the clone's transports are only the allowed ones: a remote that is a file path is refused when file is not allowed
// (the default is https and ssh), so a repository cannot be made to read local files through the daemon.
func TestOnlyHttpsAndSshTransportsAreAllowedByDefault(t *testing.T) {
	b, _, _ := rig(t)
	b.Protocols = nil
	if h, err := b.Prepare("tomas", "p", repo, "main", "pr"); err == nil {
		t.Fatalf("a file:// remote was fetched: %s", h.Path)
	}
}

// context (R-WT review S4): the worktree is mounted read-write into the fence, so its .git pointer is the agent's. Cleanup must run
// in the clone Prepare recorded and never in whatever repository the pointer was rewritten to name.
func TestRemoveNeverTrustsTheWorktreesOwnGitPointer(t *testing.T) {
	b, _, _ := rig(t)
	h := prep(t, b, "r", "rm1")
	other := t.TempDir()
	gitIn(t, other, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(other, "f"), []byte("x"), 0o644)
	gitIn(t, other, "add", "f")
	gitIn(t, other, "commit", "-q", "-m", "x")
	gone := t.TempDir()
	gitIn(t, other, "worktree", "add", "-q", "-b", "elsewhere", gone+"/wt")
	os.RemoveAll(gone + "/wt") // another repository's registration for a worktree that is currently missing
	os.WriteFile(filepath.Join(h.Path, ".git"), []byte("gitdir: "+other+"/.git\n"), 0o644)
	if err := b.Remove(h); err != nil {
		t.Fatal(err)
	}
	if out := gitIn(t, other, "worktree", "list", "--porcelain"); !strings.Contains(out, gone+"/wt") {
		t.Fatal("cleanup pruned another repository's worktree registration")
	}
	outside := &Handle{Clone: cloneOf(b), Path: t.TempDir()}
	if err := b.Remove(outside); err == nil {
		t.Fatal("a path outside the root was removable")
	}
	if _, err := os.Stat(outside.Path); err != nil {
		t.Fatal("a path outside the root was deleted")
	}
}

// context (R-WT review S5): with a worktree root that is a symlink, the path Prepare returns is a real path; cleanup compared it
// with the unresolved root and refused, leaving the tree of a refused start behind.
func TestASymlinkedWorktreeRootStillCleansUp(t *testing.T) {
	b, _, _ := rig(t)
	real := t.TempDir()
	link := filepath.Join(t.TempDir(), "root")
	os.Symlink(real, link)
	b.Root = link
	h := prep(t, b, "s", "sl1")
	if err := b.Remove(h); err != nil {
		t.Fatalf("cleanup under a symlinked root: %v", err)
	}
	if _, err := os.Stat(h.Path); err == nil {
		t.Fatal("still there")
	}
	h2 := prep(t, b, "s2", "sl2")
	long := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(h2.Path, long, long)
	if n := b.Prune(time.Hour, guardOf(func(string) bool { return false }), time.Now()); n != 1 {
		t.Fatalf("Prune under a symlinked root removed %d, want 1", n)
	}
}

// context (cleanup): a worktree of a session that ended long ago is removed with the branch it was MADE for (even if the session
// switched branches); one a session still uses, and one that is recent, stay.
func TestPruneRemovesOnlyWhatNothingNeedsAndIsOldEnough(t *testing.T) {
	b, _, _ := rig(t)
	old, used, fresh := prep(t, b, "old", "b-old"), prep(t, b, "used", "b-used"), prep(t, b, "fresh", "b-fresh")
	gitIn(t, old.Path, "checkout", "-q", "-b", "switched") // the session moved to another branch
	long := time.Now().Add(-10 * 24 * time.Hour)
	os.Chtimes(old.Path, long, long)
	os.Chtimes(used.Path, long, long)
	needed := func(p string) bool { return p == used.Path }
	if n := b.Prune(7*24*time.Hour, guardOf(needed), time.Now()); n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	for p, want := range map[string]bool{old.Path: false, used.Path: true, fresh.Path: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists=%v want %v", filepath.Base(p), err == nil, want)
		}
	}
	if out := gitIn(t, cloneOf(b), "branch", "--list", "b-old"); out != "" {
		t.Fatalf("the branch it was made for survived: %q", out)
	}
}

// context (R-WT review S6): the pass asks again, per worktree, right before removing: a session resumed since the pass began keeps its tree.
func TestPruneAsksAgainRightBeforeEachRemoval(t *testing.T) {
	b, _, _ := rig(t)
	h := prep(t, b, "old", "b1")
	long := time.Now().Add(-10 * 24 * time.Hour)
	os.Chtimes(h.Path, long, long)
	asked := 0
	if n := b.Prune(time.Hour, guardOf(func(string) bool { asked++; return true }), time.Now()); n != 0 || asked != 1 {
		t.Fatalf("removed %d, asked %d", n, asked)
	}
	if _, err := os.Stat(h.Path); err != nil {
		t.Fatal("a needed worktree was removed")
	}
}

// context (R-WT review S8): a hung fetch holds the clone's lock; every other start of that repo used to wait for ever. It now waits a
// bounded time and is refused "busy".
func TestAStartWaitsBoundedlyForABusyClone(t *testing.T) {
	b, _, _ := rig(t)
	prep(t, b, "first", "w1")
	unlock, err := lock(cloneOf(b)+".lock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	b.Limits.LockWait = 300 * time.Millisecond
	t0 := time.Now()
	_, err = b.Prepare("tomas", "second", repo, "main", "w2")
	if err == nil || !strings.Contains(err.Error(), "busy") || time.Since(t0) > 3*time.Second {
		t.Fatalf("%v after %v", err, time.Since(t0))
	}
}

// context (R-WT review S7/S11): concurrent starts of one repo serialise on the clone; two different repos whose names would collide
// under a flattened directory name get different clones.
func TestConcurrentStartsOfOneRepoSerialiseAndNestedCloneDirsDoNotCollide(t *testing.T) {
	b, _, _ := rig(t)
	var wg sync.WaitGroup
	errs := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, err := b.Prepare("tomas", "c", repo, "main", "par"+string(rune('a'+i)))
			errs <- err
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Errorf("%v", err)
		}
	}
	m1, m2 := repoRe.FindStringSubmatch("github.com/a__b/c"), repoRe.FindStringSubmatch("github.com/a/b__c")
	if b.cloneDir("tomas", m1) == b.cloneDir("tomas", m2) {
		t.Fatal("two different repositories share a clone directory")
	}
}

// context (R-WT review S11): an existing clone whose origin is not the expected one (an earlier life, a collision) is corrected before the fetch.
func TestAnExistingClonesOriginIsCorrectedBeforeTheFetch(t *testing.T) {
	b, _, _ := rig(t)
	h := prep(t, b, "a", "o1")
	b.Remove(h)
	gitIn(t, cloneOf(b), "remote", "set-url", "origin", "file:///nonexistent")
	if _, err := b.Prepare("tomas", "a", repo, "main", "o2"); err != nil {
		t.Fatalf("a wrong origin was kept: %v", err)
	}
}

// context (R-WT review S7): on a timeout only git itself used to get the signal; git-remote-https, ssh and index-pack could survive,
// keep writing into the clone after the lock was released, and race the next start. The whole process group is killed.
func TestATimedOutFetchKillsItsWholeProcessGroup(t *testing.T) {
	b, _, _ := rig(t)
	dir := t.TempDir()
	pidfile := filepath.Join(dir, "pid")
	fake := filepath.Join(dir, "git")
	real, _ := exec.LookPath("git")
	os.WriteFile(fake, []byte("#!/bin/sh\nfor a in \"$@\"; do if [ \"$a\" = fetch ]; then (sleep 60 & echo $! > "+pidfile+"; wait); exit 0; fi; done\nexec "+real+" \"$@\"\n"), 0o755)
	b.Git = fake
	b.Limits.FetchTimeout = 700 * time.Millisecond
	t0 := time.Now()
	if _, err := b.Prepare("tomas", "t", repo, "main", "tmo"); err == nil {
		t.Fatal("a fetch that never finished was accepted")
	}
	if time.Since(t0) > 6*time.Second {
		t.Fatalf("took %v", time.Since(t0))
	}
	b2, _ := os.ReadFile(pidfile)
	pid := strings.TrimSpace(string(b2))
	if pid == "" {
		t.Fatal("the fake fetch did not start")
	}
	time.Sleep(300 * time.Millisecond)
	if _, err := os.Stat("/proc/" + pid); err == nil {
		t.Fatalf("the fetch's child process %s outlived the timeout", pid)
	}
}

// guardOf adapts a "is it needed" predicate to Prune's guard (the daemon's guard also decides under its own lock).
func guardOf(needed func(string) bool) func(string, func()) {
	return func(path string, remove func()) {
		if !needed(path) {
			remove()
		}
	}
}

// context (R-WT review round 2, M1): a repository of a few KiB can name billions of paths (nested trees that all point at the level
// below). The listing used to be collected whole before it was counted, so such a repo exhausted the daemon's memory. It is streamed and
// git is killed at the first limit.
func TestATreeBombIsRefusedQuicklyWithoutBufferingTheListing(t *testing.T) {
	b, remote, _ := rig(t)
	blob := gitIn(t, remote, "hash-object", "-w", "README")
	level := blob
	typ := "blob"
	for i := 0; i < 4; i++ { // 300 entries per level: 300^4 = 8.1e9 paths
		var in strings.Builder
		for j := 0; j < 300; j++ {
			in.WriteString("040000 tree " + level + "\te" + strings.Repeat("x", j%7) + string(rune('A'+j/26)) + string(rune('a'+j%26)) + "\n")
		}
		if typ == "blob" {
			in.Reset()
			for j := 0; j < 300; j++ {
				in.WriteString("100644 blob " + level + "\tf" + string(rune('A'+j/26)) + string(rune('a'+j%26)) + "\n")
			}
		}
		c := exec.Command("git", "mktree")
		c.Dir = remote
		c.Stdin = strings.NewReader(in.String())
		out, err := c.Output()
		if err != nil {
			t.Fatal(err)
		}
		level, typ = strings.TrimSpace(string(out)), "tree"
	}
	commit := gitIn(t, remote, "commit-tree", level, "-m", "bomb")
	gitIn(t, remote, "update-ref", "refs/heads/bomb", commit)
	b.Limits.MaxFiles = 1000
	t0 := time.Now()
	var before, after runtime.MemStats
	runtime.GC()
	runtime.ReadMemStats(&before)
	_, err := b.Prepare("tomas", "bomb", repo, "bomb", "bb")
	runtime.ReadMemStats(&after)
	if err == nil {
		t.Fatal("a tree bomb was accepted")
	}
	if d := time.Since(t0); d > 20*time.Second {
		t.Fatalf("took %v", d)
	}
	if grew := int64(after.TotalAlloc-before.TotalAlloc) >> 20; grew > 200 {
		t.Fatalf("the daemon allocated %d MiB measuring the tree", grew)
	}
	if ents, _ := os.ReadDir(b.Root); len(ents) != 0 {
		t.Fatalf("something was written: %v", ents)
	}
}

// context (R-WT review round 2, M2): the rollback and Prune used to run `gc --prune=now`, which deletes any unreferenced object, including
// one a live session's host git commit has just written and not yet referenced. Neither path may touch such an object.
func TestRollbackAndPruneNeverDeleteAFreshUnreferencedObject(t *testing.T) {
	b, remote, _ := rig(t)
	live := prep(t, b, "live", "live1")
	old := prep(t, b, "old", "old1")
	c := exec.Command("git", "hash-object", "-w", "--stdin")
	c.Dir = live.Path
	c.Stdin = strings.NewReader("a commit in progress\n")
	out, err := c.Output()
	if err != nil {
		t.Fatal(err)
	}
	obj := strings.TrimSpace(string(out))
	long := time.Now().Add(-30 * 24 * time.Hour)
	os.Chtimes(old.Path, long, long)
	if n := b.Prune(time.Hour, guardOf(func(p string) bool { return p == live.Path }), time.Now()); n != 1 {
		t.Fatalf("pruned %d", n)
	}
	if _, err := exec.Command("git", "-C", cloneOf(b), "cat-file", "-e", obj).CombinedOutput(); err != nil {
		t.Fatal("Prune deleted a fresh unreferenced object")
	}
	big := make([]byte, 3<<20)
	rand.Read(big)
	os.WriteFile(filepath.Join(remote, "big.bin"), big, 0o644)
	gitIn(t, remote, "add", "big.bin")
	gitIn(t, remote, "commit", "-q", "-m", "big")
	b.Limits.MaxCloneMB = 2
	if _, err := b.Prepare("tomas", "x", repo, "main", "rb"); err == nil {
		t.Fatal("accepted")
	}
	if _, err := exec.Command("git", "-C", cloneOf(b), "cat-file", "-e", obj).CombinedOutput(); err != nil {
		t.Fatal("the rollback deleted a fresh unreferenced object")
	}
}

// context (R-WT review round 2, S1): `main` is fetched by every start, so its ref usually exists. An oversized upstream push used to leave
// the ref on the big commit and every later start of the repo was refused for good. The rollback restores the old value.
func TestAnOversizedUpstreamPushDoesNotLockTheRepoOut(t *testing.T) {
	b, remote, _ := rig(t)
	h := prep(t, b, "a", "first")
	oldSha := gitIn(t, cloneOf(b), "rev-parse", "refs/remotes/origin/main")
	b.Remove(h)
	gitIn(t, remote, "branch", "small", "main")
	big := make([]byte, 3<<20)
	rand.Read(big)
	os.WriteFile(filepath.Join(remote, "big.bin"), big, 0o644)
	gitIn(t, remote, "add", "big.bin")
	gitIn(t, remote, "commit", "-q", "-m", "big")
	b.Limits.MaxCloneMB = 2
	if _, err := b.Prepare("tomas", "b", repo, "main", "second"); err == nil {
		t.Fatal("accepted")
	}
	if got := gitIn(t, cloneOf(b), "rev-parse", "refs/remotes/origin/main"); got != oldSha {
		t.Fatalf("the ref was left on %s, want the old %s", got, oldSha)
	}
	if mb := dirMB(cloneOf(b)); mb > 2 {
		t.Fatalf("the clone still holds %d MiB", mb)
	}
	if _, err := b.Prepare("tomas", "c", repo, "small", "third"); err != nil {
		t.Fatalf("a small base is refused after the oversized one: %v", err)
	}
}

// context (R-WT review round 2, S4): a killed git leaves lock files; a later fetch or branch deletion then fails "File exists". Old ones are
// removed under the clone's lock before a fetch.
func TestAStaleGitLockFileDoesNotBlockTheNextFetch(t *testing.T) {
	b, _, _ := rig(t)
	h := prep(t, b, "a", "l1")
	b.Remove(h)
	lockf := filepath.Join(cloneOf(b), "refs", "remotes", "origin", "main.lock")
	os.WriteFile(lockf, nil, 0o644)
	old := time.Now().Add(-time.Hour)
	os.Chtimes(lockf, old, old)
	if _, err := b.Prepare("tomas", "b", repo, "main", "l2"); err != nil {
		t.Fatalf("a stale lock file blocked the fetch: %v", err)
	}
}

// context (R-WT review round 2, S2): Remove used to give up after the lock wait while a fetch held the lock for up to the fetch timeout,
// leaving the tree and the branch of a refused start behind. It waits as long as the longest holder can run.
func TestRemoveWaitsForALongHolderOfTheCloneLock(t *testing.T) {
	b, _, _ := rig(t)
	h := prep(t, b, "a", "w1")
	b.Limits.FetchTimeout, b.Limits.LockWait = 300*time.Millisecond, 100*time.Millisecond // Remove may wait ~1 s
	unlock, err := lock(cloneOf(b)+".lock", time.Second)
	if err != nil {
		t.Fatal(err)
	}
	go func() { time.Sleep(500 * time.Millisecond); unlock() }()
	if err := b.Remove(h); err != nil {
		t.Fatalf("Remove gave up: %v", err)
	}
	if out := gitIn(t, cloneOf(b), "branch", "--list", "w1"); out != "" {
		t.Fatalf("the branch survived: %q", out)
	}
}
