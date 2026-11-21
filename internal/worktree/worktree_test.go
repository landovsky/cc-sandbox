// Tests of the worktree builder (R-WT). Why each exists: the builder runs git against a repository the owner named but
// somebody else wrote, as the owner, on the owner's machine, so every case is a way a hostile repository or a hostile spec
// could make git run something, write somewhere, or hand the fence more than one worktree.
package worktree

import (
	"crypto/rand"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func git(t *testing.T, dir string, args ...string) string {
	t.Helper()
	c := exec.Command("git", args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_NOSYSTEM=1", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	out, err := c.CombinedOutput()
	if err != nil {
		t.Skipf("git %v: %v %s", args, err, out)
	}
	return strings.TrimSpace(string(out))
}

// rig: a local "remote" with a main branch, a state dir, a worktree root, and a builder that uses the remote instead of github.
func rig(t *testing.T) (*Builder, string, string) {
	t.Helper()
	remote := t.TempDir()
	git(t, remote, "init", "-q", "-b", "main")
	os.WriteFile(filepath.Join(remote, "README"), []byte("hello\n"), 0o644)
	git(t, remote, "add", "README")
	git(t, remote, "commit", "-q", "-m", "first")
	root, state := t.TempDir(), t.TempDir()
	home := t.TempDir()
	b := &Builder{StateDir: state, Root: root, Protocols: []string{"file"}, Env: []string{"HOME=" + home},
		Remote: func(string) string { return "file://" + remote }}
	return b, remote, home
}

const repo = "github.com/acme/widgets"

// context: the point of the package: a worktree of the named base on a new branch, under the root, with the clone cache
// OUTSIDE every root (a start naming the cache as workdir.path must not be able to mount it).
func TestPrepareBuildsAWorktreeOfTheBaseOnANewBranchAndKeepsTheCloneOutOfTheRoot(t *testing.T) {
	b, remote, _ := rig(t)
	want := git(t, remote, "rev-parse", "main")
	p, err := b.Prepare("tomas", "cc-sandbox:Worker 1", repo, "main", "ccsbx/326q")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Dir(p) != b.Root || !strings.HasPrefix(filepath.Base(p), "cc-sandbox-worker-1-") {
		t.Fatalf("worktree at %s", p)
	}
	if got := git(t, p, "rev-parse", "HEAD"); got != want {
		t.Fatalf("HEAD %s, want %s", got, want)
	}
	if br := git(t, p, "rev-parse", "--abbrev-ref", "HEAD"); br != "ccsbx/326q" {
		t.Fatalf("branch %s", br)
	}
	if _, err := os.Stat(filepath.Join(p, "README")); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(b.StateDir, "clones", "tomas", "acme__widgets.git")); err != nil {
		t.Fatalf("the clone is not in the state dir: %v", err)
	}
	ents, _ := os.ReadDir(b.Root)
	if len(ents) != 1 {
		t.Fatalf("the root holds %d entries, want only the worktree", len(ents))
	}
	// a second session reuses the clone (a fetch, not a re-clone) and gets its own worktree
	p2, err := b.Prepare("tomas", "cc-sandbox:w2", repo, "main", "ccsbx/other")
	if err != nil || p2 == p {
		t.Fatalf("%q %v", p2, err)
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
		if p, err := b.Prepare("tomas", "x", c[0], c[1], c[2]); err == nil {
			t.Errorf("%s was accepted: %s", name, p)
		}
	}
	if ents, _ := os.ReadDir(b.Root); len(ents) != 0 {
		t.Fatalf("a refused start left %d entries in the root", len(ents))
	}
	if _, err := os.Stat(filepath.Join(b.StateDir, "clones")); err == nil {
		ents, _ := os.ReadDir(filepath.Join(b.StateDir, "clones"))
		for _, e := range ents {
			if sub, _ := os.ReadDir(filepath.Join(b.StateDir, "clones", e.Name())); len(sub) != 0 {
				t.Fatalf("a refused start created a clone")
			}
		}
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
}

// context: a branch that already exists in the clone is refused (a session gets a NEW branch), and a failed start leaves no
// directory behind.
func TestAnExistingBranchIsRefusedAndNothingIsLeftBehind(t *testing.T) {
	b, _, _ := rig(t)
	if _, err := b.Prepare("tomas", "a", repo, "main", "feature"); err != nil {
		t.Fatal(err)
	}
	before, _ := os.ReadDir(b.Root)
	if _, err := b.Prepare("tomas", "b", repo, "main", "feature"); err == nil {
		t.Fatal("the same branch was handed to a second session")
	}
	after, _ := os.ReadDir(b.Root)
	if len(after) != len(before) {
		t.Fatalf("the root went from %d to %d entries", len(before), len(after))
	}
}

// context: the owner's own git configuration is how access works, but it must not be able to turn hooks on: a hooks path in
// their global config is overridden on the command line, so a post-checkout hook never runs during the worktree's checkout.
func TestTheOwnersConfigCannotTurnHooksOnDuringCheckout(t *testing.T) {
	b, _, home := rig(t)
	hooks := t.TempDir()
	marker := filepath.Join(t.TempDir(), "ran")
	os.WriteFile(filepath.Join(hooks, "post-checkout"), []byte("#!/bin/sh\ntouch "+marker+"\n"), 0o755)
	os.WriteFile(filepath.Join(home, ".gitconfig"), []byte("[core]\n\thooksPath = "+hooks+"\n"), 0o644)
	if _, err := b.Prepare("tomas", "h", repo, "main", "hk"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("a hook from the owner's config ran during the checkout")
	}
}

// context: a hostile repository must not fill the disk: a clone over the cap is refused AND deleted.
func TestAnOversizedCloneIsRefusedAndDeleted(t *testing.T) {
	b, remote, _ := rig(t)
	big := make([]byte, 3<<20)
	rand.Read(big) // incompressible, so the pack is really about 3 MB
	os.WriteFile(filepath.Join(remote, "big.bin"), big, 0o644)
	git(t, remote, "add", "big.bin")
	git(t, remote, "commit", "-q", "-m", "big")
	b.MaxCloneMB = 1
	if _, err := b.Prepare("tomas", "big", repo, "main", "bg"); err == nil {
		t.Fatal("a clone over the cap was accepted")
	}
	if _, err := os.Stat(filepath.Join(b.StateDir, "clones", "tomas", "acme__widgets.git")); err == nil {
		t.Fatal("the oversized clone was kept")
	}
}

// context: the clone's transports are only the allowed ones: a remote that is a file path is refused when file is not allowed
// (the default is https and ssh), so a repository cannot be made to read local files through the daemon.
func TestOnlyHttpsAndSshTransportsAreAllowedByDefault(t *testing.T) {
	b, _, _ := rig(t)
	b.Protocols = nil
	if p, err := b.Prepare("tomas", "p", repo, "main", "pr"); err == nil {
		t.Fatalf("a file:// remote was fetched: %s", p)
	}
}

// context: removal touches only a worktree directly under the root, and unregisters it.
func TestRemoveDeletesOnlyItsOwnWorktreeAndUnregistersIt(t *testing.T) {
	b, _, _ := rig(t)
	p, err := b.Prepare("tomas", "r", repo, "main", "rm1")
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Remove(p); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err == nil {
		t.Fatal("still there")
	}
	clone := filepath.Join(b.StateDir, "clones", "tomas", "acme__widgets.git")
	if out := git(t, clone, "worktree", "list", "--porcelain"); strings.Contains(out, p) {
		t.Fatalf("still registered: %s", out)
	}
	outside := t.TempDir()
	if err := b.Remove(outside); err == nil {
		t.Fatal("a path outside the root was removable")
	}
	if _, err := os.Stat(outside); err != nil {
		t.Fatal("a path outside the root was deleted")
	}
	// the branch name is free again for the next session once the worktree is gone and the branch deleted
	git(t, clone, "branch", "-D", "rm1")
}

// context (cleanup): a worktree of a session that ended long ago is removed with its branch; one a session still uses, and one
// that is recent, stay; nothing outside the root is ever touched.
func TestPruneRemovesOnlyWhatNothingNeedsAndIsOldEnough(t *testing.T) {
	b, _, _ := rig(t)
	old, _ := b.Prepare("tomas", "old", repo, "main", "b-old")
	used, _ := b.Prepare("tomas", "used", repo, "main", "b-used")
	fresh, _ := b.Prepare("tomas", "fresh", repo, "main", "b-fresh")
	long := time.Now().Add(-10 * 24 * time.Hour)
	os.Chtimes(old, long, long)
	os.Chtimes(used, long, long)
	needed := func(p string) bool { return p == used }
	if n := b.Prune(7*24*time.Hour, needed, time.Now()); n != 1 {
		t.Fatalf("pruned %d, want 1", n)
	}
	for p, want := range map[string]bool{old: false, used: true, fresh: true} {
		if _, err := os.Stat(p); (err == nil) != want {
			t.Errorf("%s exists=%v want %v", filepath.Base(p), err == nil, want)
		}
	}
	clone := filepath.Join(b.StateDir, "clones", "tomas", "acme__widgets.git")
	if out := git(t, clone, "branch", "--list", "b-old"); out != "" {
		t.Fatalf("the branch survived: %q", out)
	}
}
