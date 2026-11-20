// Tests for what the owner's own git trusts inside a fenced worktree: files under core.hooksPath
// and include.path, and nested repositories. steering-check must report them (exit 3) because H1
// guards only the host's git, not the owner's next `git commit`.
package gitroute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func setCfg(t *testing.T, gd, key, val string) {
	t.Helper()
	sh(t, gd, "git", "--git-dir", gd, "config", key, val)
}

func contains2(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Husky: core.hooksPath points at .husky (or .githooks-x) inside the worktree, so a fence's edit to
// a hook there is run by the owner's next commit.
func TestChangesUnderHooksPathAreSteering(t *testing.T) {
	r, gd, wt := repo(t)
	setCfg(t, gd, "core.hooksPath", "tools/hooks")
	write(t, wt, "tools/hooks/pre-commit", "#!/bin/sh\necho hi\n")
	write(t, wt, "tools/other.txt", "x\n")
	write(t, wt, "src/a.go", "package a\n")
	got, err := r.SteeringSince("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !contains2(got, "tools/hooks/pre-commit") {
		t.Errorf("a change under core.hooksPath was not flagged: %v", got)
	}
	for _, no := range []string{"tools/other.txt", "src/a.go"} {
		if contains2(got, no) {
			t.Errorf("%s is not under the hooks path but was flagged: %v", no, got)
		}
	}
	// an absolute hooksPath inside the worktree is the same
	setCfg(t, gd, "core.hooksPath", filepath.Join(wt, "abs-hooks"))
	write(t, wt, "abs-hooks/post-commit", "#!/bin/sh\n")
	if got, _ := r.SteeringSince("HEAD"); !contains2(got, "abs-hooks/post-commit") {
		t.Errorf("an absolute hooksPath inside the worktree was missed: %v", got)
	}
	// one outside the worktree is not the fence's to write
	setCfg(t, gd, "core.hooksPath", t.TempDir())
	if got, _ := r.SteeringSince("HEAD"); contains2(got, "abs-hooks/post-commit") || contains2(got, "tools/hooks/pre-commit") {
		t.Errorf("a hooksPath outside the worktree flagged worktree files: %v", got)
	}
}

// Unchanged files under the hooks path are not news; a committed change since the base is.
func TestCommittedChangesUnderHooksPathAreSteering(t *testing.T) {
	r, gd, wt := repo(t)
	setCfg(t, gd, "core.hooksPath", ".githooks")
	write(t, wt, ".githooks/pre-push", "#!/bin/sh\n")
	if _, err := r.Commit("s", "a hook", quiet); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.SteeringSince("HEAD~1"); !contains2(got, ".githooks/pre-push") {
		t.Errorf("a committed hook change was missed: %v", got)
	}
	if got, _ := r.SteeringSince("HEAD"); contains2(got, ".githooks/pre-push") {
		t.Errorf("an unchanged hook was flagged: %v", got)
	}
}

// include.path pulls a file into the repository's config: the path is relative to the config file,
// and a change to it is a change to the owner's git configuration.
func TestChangesToAnIncludedConfigAreSteering(t *testing.T) {
	r, gd, wt := repo(t)
	inc := filepath.Join(wt, "inc", "extra.cfg")
	write(t, wt, "inc/extra.cfg", "[core]\n\thooksPath = x\n")
	setCfg(t, gd, "include.path", inc)
	// by default the host refuses to run with an include in the config (H1)
	if _, err := r.Run("status"); err == nil || !strings.Contains(err.Error(), "include") {
		t.Fatalf("H1 no longer refuses includes: %v", err)
	}
	// steering-check reads it instead, and flags the file
	r.AllowIncludes = true
	got, err := r.SteeringSince("HEAD")
	if err != nil {
		t.Fatal(err)
	}
	if !contains2(got, "inc/extra.cfg") {
		t.Errorf("a change to an included config was not flagged: %v", got)
	}
	// includeIf too, and a relative path resolves against the config file (into the gitdir's parent)
	setCfg(t, gd, "includeIf.gitdir:/x/.path", inc)
	if got, _ := r.SteeringSince("HEAD"); !contains2(got, "inc/extra.cfg") {
		t.Errorf("includeIf missed: %v", got)
	}
}

func TestHuskyIsSteeringByName(t *testing.T) {
	for _, p := range []string{".husky/pre-commit", ".husky/_/husky.sh", "pkg/.husky/commit-msg"} {
		if !IsSteering(p) {
			t.Errorf("%s should be steering", p)
		}
	}
}

// A nested repository (a .git directory or file below the worktree) has a config and hooks the
// fence wrote. Reported unless it is a submodule HEAD already has; the worktree's own .git, a
// symlinked directory and node_modules are not.
func TestNestedReposAreSteering(t *testing.T) {
	r, _, wt := repo(t)
	sh(t, wt, "git", "init", "-q", "sub")
	os.MkdirAll(filepath.Join(wt, "deep", "er"), 0o755)
	write(t, wt, "deep/er/.git", "gitdir: /somewhere/else\n")
	sh(t, wt, "git", "init", "-q", "node_modules/pkg")
	outside := t.TempDir()
	sh(t, outside, "git", "init", "-q", "ext")
	os.Symlink(filepath.Join(outside, "ext"), filepath.Join(wt, "linked"))
	got, err := r.NestedRepos()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"sub/.git": true, "deep/er/.git": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Errorf("nested repos: %v (want sub/.git and deep/er/.git only)", got)
	}
	if st, _ := r.SteeringSince("HEAD"); !contains2(st, "sub/.git") || !contains2(st, "deep/er/.git") {
		t.Errorf("steering-check would not report them: %v", st)
	}
}

// A submodule the HEAD tree already records is not a surprise.
func TestKnownSubmodulesAreNotReported(t *testing.T) {
	r, gd, wt := repo(t)
	x := filepath.Join(wt, "x")
	sh(t, wt, "git", "init", "-q", "x")
	write(t, x, "f.txt", "1\n")
	sh(t, x, "git", "add", "f.txt")
	sh(t, x, "git", "commit", "-q", "-m", "x")
	sh(t, wt, "git", "--git-dir", gd, "--work-tree", wt, "add", "x")
	sh(t, wt, "git", "--git-dir", gd, "--work-tree", wt, "commit", "-q", "-m", "gitlink")
	got, err := r.NestedRepos()
	if err != nil || len(got) != 0 {
		t.Errorf("a recorded submodule was reported: %v %v", got, err)
	}
	// but a second, new nested repo is
	sh(t, wt, "git", "init", "-q", "newone")
	if got, _ := r.NestedRepos(); len(got) != 1 || got[0] != "newone/.git" {
		t.Errorf("%v", got)
	}
}

func TestOversizedConfigOutputIsRefused(t *testing.T) {
	r, gd, _ := repo(t)
	for i := 0; i < 3000; i++ {
		setCfgBulk(gd, i)
	}
	r.AllowIncludes = true
	if _, err := r.configGuards(); err == nil {
		t.Error("an enormous config was read unbounded")
	}
}

func setCfgBulk(gd string, i int) {
	f, _ := os.OpenFile(filepath.Join(gd, "config"), os.O_APPEND|os.O_WRONLY, 0o644)
	defer f.Close()
	f.WriteString("[include]\n\tpath = /nonexistent/" + strings.Repeat("a", 40) + "/" + string(rune('a'+i%26)) + "\n")
}
