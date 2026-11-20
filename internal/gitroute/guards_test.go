// Tests for what the owner's own git trusts inside a fenced worktree: files under core.hooksPath and
// include.path (in the repository's config, the owner's global config and included files), include
// files that live in the worktree, and nested repositories. steering-check must report them
// (exit 3), and say "incomplete" (exit 4) when it could not look everywhere, because H1 guards only
// the host's git, not the owner's next `git commit`.
package gitroute

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func setCfg(t *testing.T, gd, key, val string) {
	t.Helper()
	sh(t, gd, "git", "--git-dir", gd, "config", key, val)
}

func has(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Husky: core.hooksPath points at a directory inside the worktree, so a fence's edit to a hook
// there is run by the owner's next commit.
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
	if !has(got, "tools/hooks/pre-commit") {
		t.Errorf("a change under core.hooksPath was not flagged: %v", got)
	}
	for _, no := range []string{"tools/other.txt", "src/a.go"} {
		if has(got, no) {
			t.Errorf("%s is not under the hooks path but was flagged: %v", no, got)
		}
	}
	setCfg(t, gd, "core.hooksPath", filepath.Join(wt, "abs-hooks")) // absolute, inside
	write(t, wt, "abs-hooks/post-commit", "#!/bin/sh\n")
	if got, _ := r.SteeringSince("HEAD"); !has(got, "abs-hooks/post-commit") {
		t.Errorf("an absolute hooksPath inside the worktree was missed: %v", got)
	}
	setCfg(t, gd, "core.hooksPath", t.TempDir()) // outside: not the fence's to write
	if got, _ := r.SteeringSince("HEAD"); has(got, "abs-hooks/post-commit") || has(got, "tools/hooks/pre-commit") {
		t.Errorf("a hooksPath outside the worktree flagged worktree files: %v", got)
	}
}

func TestCommittedChangesUnderHooksPathAreSteering(t *testing.T) {
	r, gd, wt := repo(t)
	setCfg(t, gd, "core.hooksPath", ".githooks")
	write(t, wt, ".githooks/pre-push", "#!/bin/sh\n")
	if _, err := r.Commit("s", "a hook", quiet); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.SteeringSince("HEAD~1"); !has(got, ".githooks/pre-push") {
		t.Errorf("a committed hook change was missed: %v", got)
	}
}

// An unchanged hook is not news.
func TestUnchangedHooksAreNotFlagged(t *testing.T) {
	r, gd, wt := repo(t)
	setCfg(t, gd, "core.hooksPath", ".githooks")
	write(t, wt, ".githooks/pre-push", "#!/bin/sh\n")
	if _, err := r.Commit("s", "a hook", quiet); err != nil {
		t.Fatal(err)
	}
	if got, _ := r.SteeringSince("HEAD"); has(got, ".githooks/pre-push") {
		t.Errorf("an unchanged hook was flagged: %v", got)
	}
}

// ST1: an include file that lives in the worktree is flagged ALWAYS, changed or not: the fence
// can write it, and no change detection of git's is relied on for it. With an include in the
// config H1 still refuses every other git command (nothing follows the include).
func TestAnIncludedConfigInTheWorktreeIsAlwaysFlagged(t *testing.T) {
	r, gd, wt := repo(t)
	write(t, wt, "inc/extra.cfg", "[core]\n\tfsmonitor = ./x\n")
	if _, err := r.Commit("s", "the include", quiet); err != nil {
		t.Fatal(err)
	}
	setCfg(t, gd, "include.path", filepath.Join(wt, "inc", "extra.cfg"))
	if _, err := r.Run("status"); err == nil || !strings.Contains(err.Error(), "include") {
		t.Fatalf("H1 no longer refuses includes: %v", err)
	}
	got, err := r.SteeringSince("HEAD")
	if err != nil || !has(got, "inc/extra.cfg") {
		t.Errorf("an unchanged include inside the worktree was not flagged: %v %v", got, err)
	}
	setCfg(t, gd, "includeIf.gitdir:/x/.path", filepath.Join(wt, "inc", "other.cfg"))
	write(t, wt, "inc/other.cfg", "[user]\n\tname = x\n")
	if got, _ := r.SteeringSince("HEAD"); !has(got, "inc/other.cfg") {
		t.Errorf("includeIf missed: %v", got)
	}
}

// ST2: Commit and RunForFence keep refusing a config with an include: nothing fence-facing gets the
// relaxed behaviour, and there is no field to switch it on.
func TestCommitAndRunForFenceRefuseIncludes(t *testing.T) {
	r, gd, wt := repo(t)
	write(t, wt, "a.txt", "a\n")
	setCfg(t, gd, "include.path", filepath.Join(wt, "x.cfg"))
	if _, err := r.Commit("s", "m", quiet); err == nil || !strings.Contains(err.Error(), "include") {
		t.Errorf("Commit ran with an include in the config: %v", err)
	}
	if _, err := r.RunForFence("log", "-1"); err == nil || !strings.Contains(err.Error(), "include") {
		t.Errorf("RunForFence ran with an include in the config: %v", err)
	}
}

// ST1: verdict-relevant keys are pinned on the command line, where no config can steer them.
func TestChangeDetectionKeysArePinned(t *testing.T) {
	r, _, _ := repo(t)
	c, err := r.Cmd("status")
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"core.checkStat=default", "core.trustctime=true", "core.fileMode=true", "core.symlinks=true", "core.sparseCheckout=false"} {
		if !has(c.Args, want) {
			t.Errorf("%s is not pinned", want)
		}
	}
}

// ST4: the INCLUDED file's own hooksPath and includes are read (one level), and so is the owner's
// global config; "~" is expanded with the owner's home.
func TestGuardsReadIncludedAndGlobalConfigs(t *testing.T) {
	r, gd, wt := repo(t)
	home := filepath.Dir(wt) // worktrees live under the home
	r.Home = home
	write(t, wt, "inc/a.cfg", "[core]\n\thooksPath = scripts/hooks\n[include]\n\tpath = b.cfg\n")
	write(t, wt, "inc/b.cfg", "[core]\n\thooksPath = second/level\n[include]\n\tpath = c.cfg\n")
	write(t, wt, "inc/c.cfg", "[core]\n\thooksPath = third/level\n")
	setCfg(t, gd, "include.path", filepath.Join(wt, "inc", "a.cfg"))
	g, err := r.configGuards()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"scripts/hooks", "inc/a.cfg", "inc/b.cfg"} {
		if !has(g.Prefixes, want) {
			t.Errorf("%s was not read from the included files: %v", want, g.Prefixes)
		}
	}
	if has(g.Prefixes, "second/level") || has(g.Prefixes, "third/level") || has(g.Forced, "inc/c.cfg") {
		t.Errorf("a second level of includes was read (one level only): %v", g)
	}
	if !has(g.Forced, "inc/a.cfg") || !has(g.Forced, "inc/b.cfg") {
		t.Errorf("in-worktree includes are not forced: %v", g.Forced)
	}
	// the owner's global config, with "~"
	r2, gd2, wt2 := repo(t)
	_ = gd2
	h2 := filepath.Dir(wt2)
	r2.Home = h2
	os.WriteFile(filepath.Join(h2, ".gitconfig"), []byte("[core]\n\thooksPath = ~/wt/.global-hooks\n"), 0o644)
	write(t, wt2, ".global-hooks/pre-commit", "#!/bin/sh\n")
	got, err := r2.SteeringSince("HEAD")
	if err != nil || !has(got, ".global-hooks/pre-commit") {
		t.Errorf("a global hooksPath with ~ into the worktree was missed: %v %v", got, err)
	}
	os.WriteFile(filepath.Join(h2, ".gitconfig"), []byte("[core]\n\thooksPath = .rel-hooks\n"), 0o644)
	write(t, wt2, ".rel-hooks/pre-commit", "#!/bin/sh\n")
	if got, _ := r2.SteeringSince("HEAD"); !has(got, ".rel-hooks/pre-commit") {
		t.Errorf("a relative global hooksPath was missed: %v", got)
	}
}

// ST3: the bound is enforced while reading: git is stopped when it has produced more than the
// limit, rather than buffering everything first.
func TestConfigReadIsBoundedWhileReading(t *testing.T) {
	r, _, _ := repo(t)
	r.Git = fakeGit(t, `[ "$1" = config ] && exec yes`) // endless output
	start := time.Now()
	_, err := r.configGuards()
	if !errors.Is(err, ErrConfigTooBig) || time.Since(start) > 10*time.Second {
		t.Fatalf("an endless config read was not cut off: %v after %s", err, time.Since(start))
	}
}

// A symlinked config.worktree in the gitdir is refused like config is.
func TestConfigWorktreeIsOwnershipChecked(t *testing.T) {
	r, gd, _ := repo(t)
	real := filepath.Join(t.TempDir(), "cfg")
	os.WriteFile(real, []byte("[core]\n"), 0o644)
	os.Symlink(real, filepath.Join(gd, "config.worktree"))
	if _, err := r.Run("status"); err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Errorf("a symlinked config.worktree was accepted: %v", err)
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
// fence wrote. Reported unless it is a recorded submodule; the worktree's own .git and a symlinked
// directory are not; an ignored node_modules is not entered.
func TestNestedReposAreSteering(t *testing.T) {
	r, _, wt := repo(t)
	write(t, wt, ".gitignore", "node_modules/\n")
	sh(t, wt, "git", "init", "-q", "sub")
	os.MkdirAll(filepath.Join(wt, "deep", "er"), 0o755)
	write(t, wt, "deep/er/.git", "gitdir: /somewhere/else\n")
	sh(t, wt, "git", "init", "-q", "node_modules/pkg")
	outside := t.TempDir()
	sh(t, outside, "git", "init", "-q", "ext")
	os.Symlink(filepath.Join(outside, "ext"), filepath.Join(wt, "linked"))
	if _, err := r.Commit("s", "ignore file", quiet); err == nil {
		t.Fatal("a nested repo was committed")
	}
	got, err := r.NestedRepos()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"sub/.git": true, "deep/er/.git": true}
	if len(got) != 2 || !want[got[0]] || !want[got[1]] {
		t.Errorf("nested repos: %v (want sub/.git and deep/er/.git only)", got)
	}
}

// A node_modules git does NOT ignore is searched: editors and tools still see what is in it.
func TestUnignoredNodeModulesIsSearched(t *testing.T) {
	r, _, wt := repo(t)
	sh(t, wt, "git", "init", "-q", "src/node_modules/x")
	got, err := r.NestedRepos()
	if err != nil || !has(got, "src/node_modules/x/.git") {
		t.Errorf("an unignored node_modules/**/.git was hidden: %v %v", got, err)
	}
}

// A submodule the HEAD tree records, with a proper .git file into modules/, is not a surprise.
func TestKnownSubmodulesAreNotReported(t *testing.T) {
	r, gd, wt := repo(t)
	x := filepath.Join(wt, "x")
	sh(t, wt, "git", "init", "-q", "x")
	write(t, x, "f.txt", "1\n")
	sh(t, x, "git", "add", "f.txt")
	sh(t, x, "git", "commit", "-q", "-m", "x")
	sh(t, wt, "git", "--git-dir", gd, "--work-tree", wt, "add", "x")
	sh(t, wt, "git", "--git-dir", gd, "--work-tree", wt, "commit", "-q", "-m", "gitlink")
	// the superproject's way: x/.git is a FILE naming <gitdir>/modules/x
	os.RemoveAll(filepath.Join(x, ".git"))
	write(t, x, ".git", "gitdir: "+filepath.Join(gd, "modules", "x")+"\n")
	got, err := r.NestedRepos()
	if err != nil || len(got) != 0 {
		t.Errorf("a proper recorded submodule was reported: %v %v", got, err)
	}
	// ST7: the same submodule with its .git swapped for the agent's own repository is reported
	os.Remove(filepath.Join(x, ".git"))
	sh(t, x, "git", "init", "-q", ".")
	if got, _ := r.NestedRepos(); !has(got, "x/.git") {
		t.Errorf("a submodule whose .git became a directory was not reported: %v", got)
	}
	os.RemoveAll(filepath.Join(x, ".git"))
	write(t, x, ".git", "gitdir: /tmp/agent-repo\n")
	if got, _ := r.NestedRepos(); !has(got, "x/.git") {
		t.Errorf("a submodule whose .git names another place was not reported: %v", got)
	}
	// and a second, new nested repo is
	os.Remove(filepath.Join(x, ".git"))
	write(t, x, ".git", "gitdir: "+filepath.Join(gd, "modules", "x")+"\n")
	sh(t, wt, "git", "init", "-q", "newone")
	if got, _ := r.NestedRepos(); len(got) != 1 || got[0] != "newone/.git" {
		t.Errorf("%v", got)
	}
}

// ST5: hitting the walk cap is "incomplete", never a quiet stop.
func TestHittingTheWalkCapIsIncomplete(t *testing.T) {
	r, _, wt := repo(t)
	defer SetNestedWalkCap(5)()
	for i := 0; i < 20; i++ {
		write(t, wt, "aaa/f"+string(rune('a'+i)), "x")
	}
	sh(t, wt, "git", "init", "-q", "zzz")
	got, err := r.NestedRepos()
	if !errors.Is(err, ErrIncomplete) {
		t.Fatalf("the cap ended the walk quietly: %v %v", got, err)
	}
	if _, err := r.SteeringSince("HEAD"); !errors.Is(err, ErrIncomplete) {
		t.Errorf("SteeringSince did not report incomplete: %v", err)
	}
}

// ST6: a directory that cannot be read is reported, not skipped silently.
func TestUnreadableDirectoriesAreReported(t *testing.T) {
	r, _, wt := repo(t)
	d := filepath.Join(wt, "locked")
	os.MkdirAll(filepath.Join(d, "inner"), 0o755)
	os.Chmod(d, 0o000)
	defer os.Chmod(d, 0o755)
	if os.Getuid() == 0 {
		t.Skip("root reads everything")
	}
	got, err := r.NestedRepos()
	if err != nil || !has(got, "locked/ (unreadable)") {
		t.Errorf("an unreadable directory was skipped silently: %v %v", got, err)
	}
}

// NIT: macOS volumes are case-insensitive, so .GIT is a repository there.
func TestDotGitMatchesCaseInsensitivelyWhereTheVolumeDoes(t *testing.T) {
	defer func(old bool) { dotGitFold = old }(dotGitFold)
	dotGitFold = false
	if isDotGit(".GIT") || !isDotGit(".git") {
		t.Error("exact match expected where names are case-sensitive")
	}
	dotGitFold = true
	if !isDotGit(".GIT") || !isDotGit(".Git") {
		t.Error(".GIT is a repository on a case-insensitive volume")
	}
}
