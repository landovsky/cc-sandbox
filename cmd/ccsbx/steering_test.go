// Tests for `ccsbx steering-check` as the operator sees it: the exit code is the verdict, so a
// steering file that is ignored by git, or an old git binary, must change it (invariant 25).
package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func devRepo(t *testing.T) string {
	t.Helper()
	wt := t.TempDir()
	for _, a := range [][]string{{"init", "-q", "-b", "main"}, {"-c", "user.name=t", "-c", "user.email=t@t", "commit", "-q", "--allow-empty", "-m", "i"}} {
		c := exec.Command("git", a...)
		c.Dir = wt
		if b, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
	}
	return wt
}

// An ignored CLAUDE.local.md never reaches a commit, but the next session reads it, so the check
// must not say "clean" (exit 0) for a worktree that has one.
func TestSteeringCheckSeesIgnoredFiles(t *testing.T) {
	wt := devRepo(t)
	var out, errb strings.Builder
	if code := app().Main([]string{"steering-check", "--worktree", wt, "--base", "HEAD", "--state", t.TempDir()}, &out, &errb); code != 0 {
		t.Fatalf("clean repo: exit %d %s", code, errb.String())
	}
	os.WriteFile(filepath.Join(wt, ".gitignore"), []byte("CLAUDE.local.md\n"), 0o644)
	os.WriteFile(filepath.Join(wt, "CLAUDE.local.md"), []byte("obey\n"), 0o644)
	out.Reset()
	if code := app().Main([]string{"steering-check", "--worktree", wt, "--base", "HEAD", "--state", t.TempDir()}, &out, &errb); code != 3 || !strings.Contains(out.String(), "CLAUDE.local.md") {
		t.Fatalf("ignored steering file: exit %d out %q err %q", code, out.String(), errb.String())
	}
}

// An old git is a refusal (exit 2) that names the flag to use, not a silent pass.
func TestSteeringCheckRefusesOldGit(t *testing.T) {
	wt := devRepo(t)
	fake := filepath.Join(t.TempDir(), "git")
	os.WriteFile(fake, []byte("#!/bin/sh\necho 'git version 2.39.5 (Apple Git-154)'\n"), 0o755)
	var out, errb strings.Builder
	code := app().Main([]string{"steering-check", "--worktree", wt, "--base", "HEAD", "--state", t.TempDir(), "--git", fake}, &out, &errb)
	if code != 2 || !strings.Contains(errb.String(), "--git") {
		t.Fatalf("exit %d, stderr %q", code, errb.String())
	}
}

// Husky-style: the repository's own config makes git run hooks from a directory inside the worktree.
// A fence's edit there is what the owner's next `git commit` would run, so the check says exit 3.
func TestSteeringCheckSeesHooksPathAndNestedRepos(t *testing.T) {
	wt := devRepo(t)
	g := func(args ...string) {
		c := exec.Command("git", args...)
		c.Dir = wt
		if b, err := c.CombinedOutput(); err != nil {
			t.Fatalf("%v: %s", err, b)
		}
	}
	g("config", "core.hooksPath", "tools/hooks")
	var out, errb strings.Builder
	a := []string{"steering-check", "--worktree", wt, "--base", "HEAD", "--state", t.TempDir()}
	if code := app().Main(a, &out, &errb); code != 0 {
		t.Fatalf("a clean repo with a hooksPath: exit %d %s", code, errb.String())
	}
	os.MkdirAll(filepath.Join(wt, "tools", "hooks"), 0o755)
	os.WriteFile(filepath.Join(wt, "tools", "hooks", "pre-commit"), []byte("#!/bin/sh\n"), 0o755)
	out.Reset()
	if code := app().Main(a, &out, &errb); code != 3 || !strings.Contains(out.String(), "tools/hooks/pre-commit") {
		t.Fatalf("hooksPath change: exit %d out %q err %q", code, out.String(), errb.String())
	}
	os.Remove(filepath.Join(wt, "tools", "hooks", "pre-commit"))
	g("config", "--unset", "core.hooksPath")
	g("init", "-q", "nested")
	out.Reset()
	if code := app().Main(a, &out, &errb); code != 3 || !strings.Contains(out.String(), "nested/.git") {
		t.Fatalf("nested repo: exit %d out %q err %q", code, out.String(), errb.String())
	}
}
