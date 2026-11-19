package gitroute

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// T:git-args-allowlist — block: a request that reaches Run (the fence may supply arguments one
// day) can only use the flags listed per verb, by exact name. Each case below is an option of an
// allowed verb that reads a host file, moves refs, runs a driver or rewrites identity, or an
// abbreviation of one.
func TestArgsAllowlistBlock(t *testing.T) {
	r, _, wt := repo(t)
	write(t, wt, "x.txt", "x\n")
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("TOP SECRET"), 0o600)
	for _, args := range [][]string{
		{"commit", "-F", secret}, {"commit", "--fil=" + secret}, {"commit", "--file", secret}, {"commit", "--amend", "--author=x <x@x>"},
		{"add", "--pathspec-from-file=" + secret}, {"add", "--pathspec-fr=" + secret},
		{"branch", "-f", "main", "HEAD"}, {"branch", "newbranch"},
		{"symbolic-ref", "HEAD", "refs/heads/main"},
		{"cat-file", "--filters", "HEAD:README.md"}, {"cat-file", "-p", "HEAD"},
		{"write-tree"},
		{"log", "--outp=/tmp/x"}, {"log", "--ext-d"}, {"diff", "--ext-diff"}, {"diff", "--textc"}, {"log", "-p", "--textconv"},
		{"status", "--por"}, {"status", "-sb"}, {"diff", "--name"}, {"show", "--format=%H", "--output=/tmp/y"},
		{"log", ":(attr:filter)x"}, {"log", "--", ":(top)x", "--oops"}, {"diff", "-c", "core.pager=x"}, {"ls-files", "--with-tree=HEAD", "--recurse-submodules"},
	} {
		if _, err := r.Run(args...); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("git %v was not refused: %v", args, err)
		}
	}
	if b, _ := os.ReadFile(filepath.Join(wt, "x.txt")); string(b) != "x\n" {
		t.Fatal("worktree changed")
	}
	if out, _ := r.Run("log", "--oneline"); strings.Contains(out, "TOP SECRET") {
		t.Fatal("a host file reached a commit message")
	}
}

// T:git-args-allowlist — allow: the read-only verbs the host actually uses still work with their
// listed flags.
func TestArgsAllowlistAllow(t *testing.T) {
	r, gd, wt := repo(t)
	write(t, wt, "n.txt", "n\n")
	for _, args := range [][]string{
		{"status", "--porcelain"}, {"status", "--short", "--branch"}, {"diff", "--name-only", "--no-renames"}, {"diff", "--stat"},
		{"log", "--oneline", "-1"}, {"log", "--format=%B", "--max-count=1"}, {"show", "--name-only", "--format=%H", "HEAD"},
		{"rev-parse", "--absolute-git-dir"}, {"rev-parse", "HEAD"}, {"ls-files", "-z"}, {"ls-tree", "-r", "--name-only", "HEAD"},
		{"rev-list", "--count", "HEAD"}, {"diff", "HEAD", "--", "n.txt"},
	} {
		if _, err := r.Run(args...); err != nil {
			t.Errorf("git %v: %v", args, err)
		}
	}
	if out, _ := r.Run("rev-parse", "--absolute-git-dir"); strings.TrimSpace(out) != gd {
		t.Fatalf("%q", out)
	}
}

// H1-S2: a rename hides the steering file's old path unless renames are off.
func TestRenameDoesNotHideSteeringFile(t *testing.T) {
	r, _, wt := repo(t)
	write(t, wt, ".claude/settings.json", "{\"a\":1,\"b\":2,\"c\":3,\"d\":4}\n")
	if _, err := r.Commit("s", "add settings"); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(wt, ".claude/settings.json"), filepath.Join(wt, "notes.json")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("s", "innocent rename"); err != nil {
		t.Fatal(err)
	}
	msg, _ := r.Run("log", "-1", "--format=%B")
	if !strings.Contains(msg, "Steering-Files: .claude/settings.json") {
		t.Fatalf("rename hid the steering file: %q", msg)
	}
	files, _ := r.ChangedFiles("HEAD~1")
	if len(Steering(files)) != 1 {
		t.Fatalf("steering-check sees %v", files)
	}
}

// H1-S1: the gitdir is only used if the host owns it and nobody else can write it, and no
// info/attributes can name a filter that the empty-tree attribute source doesn't cover.
func TestGitDirMustBeHostOwnedAndPlain(t *testing.T) {
	r, gd, _ := repo(t)
	if _, err := r.Run("status"); err != nil {
		t.Fatal(err)
	}
	os.MkdirAll(filepath.Join(gd, "info"), 0o755)
	os.WriteFile(filepath.Join(gd, "info", "attributes"), []byte("* filter=x\n"), 0o644)
	if _, err := r.Run("status"); err == nil || !strings.Contains(err.Error(), "info/attributes") {
		t.Fatalf("info/attributes accepted: %v", err)
	}
	os.Remove(filepath.Join(gd, "info", "attributes"))
	os.Chmod(filepath.Join(gd, "config"), 0o666)
	if _, err := r.Run("status"); err == nil || !strings.Contains(err.Error(), "world-writable") {
		t.Fatalf("world-writable config accepted: %v", err)
	}
	os.Chmod(filepath.Join(gd, "config"), 0o644)
	os.Chmod(gd, 0o757|os.ModeDir)
	if _, err := r.Run("status"); err == nil {
		t.Fatal("world-writable gitdir accepted")
	}
	os.Chmod(gd, 0o755)
	if _, err := r.Run("status"); err != nil {
		t.Fatalf("restored: %v", err)
	}
}

func TestSteeringCaseInsensitiveAndMore(t *testing.T) {
	for _, p := range []string{"claude.md", "Claude.MD", "CLAUDE.local.md", ".cursorrules", ".vscode/tasks.json", "mise.toml", ".tool-versions", ".npmrc", ".CLAUDE/x", ".GitHub/workflows/a.yml"} {
		if !IsSteering(p) {
			t.Errorf("%s should be steering", p)
		}
	}
}
