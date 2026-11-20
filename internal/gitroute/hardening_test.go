// Tests for the second round of H1 (the independent review of PR #3): nested repositories,
// the git version gate, the commit-time scan, symlinked and ignored steering files, trailer and
// terminal injection, signature placeholders, config includes and the git timeout. Each block twin
// was run red against the code without its check (see the PR text); they live apart from
// gitroute_test.go so that file keeps the first round's story readable.
package gitroute

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeGit writes a git wrapper script to a temp dir and returns its path. body runs before the
// real git is exec'd with the same arguments.
func fakeGit(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "git")
	if err := os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\nexec /usr/bin/git \"$@\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// nested makes wt/x a full repo whose own config and info/attributes would run `cmd` as a clean
// filter, recorded as a gitlink in HEAD by plain git, with a .gitmodules saying ignore=none and a
// file in x whose stat data is stale (so any status inside x re-hashes it through the filter).
// It runs as the same uid as the host git, which is the Docker Desktop case where git's ownership
// check does not save us.
func nested(t *testing.T, r *Runner, gd, wt, cmd string) {
	t.Helper()
	x := filepath.Join(wt, "x")
	sh(t, wt, "git", "init", "-q", "x")
	write(t, x, "f.txt", "one\n")
	sh(t, x, "git", "add", "f.txt")
	sh(t, x, "git", "commit", "-q", "-m", "x")
	write(t, x, ".git/info/attributes", "* filter=e\n")
	sh(t, x, "git", "config", "filter.e.clean", cmd+"; cat")
	sh(t, wt, "git", "--git-dir", gd, "--work-tree", wt, "add", "x")
	sh(t, wt, "git", "--git-dir", gd, "--work-tree", wt, "commit", "-q", "-m", "gitlink")
	write(t, wt, ".gitmodules", "[submodule \"x\"]\n\tpath = x\n\turl = https://example.invalid/x.git\n\tignore = none\n")
	// stale stat data: same content, later mtime
	write(t, x, "f.txt", "one\n")
	later := time.Now().Add(time.Hour)
	if err := os.Chtimes(filepath.Join(x, "f.txt"), later, later); err != nil {
		t.Fatal(err)
	}
}

// T:git-nested-repo — block: a nested repo with `.gitmodules ignore=none` must not make host
// status/diff/log run git inside it, where its own attributes and config can start a filter.
// diff.ignoreSubmodules is only a default; the command line is what beats .gitmodules.
func TestNestedRepoStatusBlock(t *testing.T) {
	r, gd, wt := repo(t)
	cmd, ran := marker(t)
	nested(t, r, gd, wt, cmd)
	for _, args := range [][]string{{"status", "--porcelain"}, {"diff"}, {"diff", "--cached"}, {"log", "-p", "-1"}, {"show", "HEAD"}} {
		if _, err := r.Run(args...); err != nil {
			t.Fatalf("git %v: %v", args, err)
		}
	}
	if exists(ran) {
		t.Fatal("host git ran a filter defined by an agent-made nested repository")
	}
	for _, v := range []string{"status", "diff"} {
		c, err := r.Cmd(v)
		if err != nil || !contains(c.Args, "--ignore-submodules=all") {
			t.Errorf("git %s was built without --ignore-submodules=all: %v %v", v, c.Args, err)
		}
	}
	// a caller cannot choose another value
	for _, a := range []string{"--ignore-submodules=none", "--ignore-submodules", "--ignore-submodules=dirty"} {
		if _, err := r.Run("status", a); err == nil {
			t.Errorf("status %s was accepted", a)
		}
	}
}

// T:git-nested-repo — allow: a repo that already has a gitlink in HEAD commits other changes
// normally, and the gitlink is left alone.
func TestNestedRepoAllow(t *testing.T) {
	r, gd, wt := repo(t)
	nested(t, r, gd, wt, "true")
	os.Remove(filepath.Join(wt, ".gitmodules"))
	write(t, wt, "other.txt", "o\n")
	if _, err := r.Commit("s", "plain change next to a gitlink"); err != nil {
		t.Fatal(err)
	}
}

// T:git-nested-repo — block (S6): the agent makes a new embedded repo, or a `.git` file naming a
// host gitdir. Recording it would put that repo's HEAD id into a pushed commit; the commit is
// refused and nothing stays staged.
func TestEmbeddedRepoIsNotRecorded(t *testing.T) {
	r, gd, wt := repo(t)
	sh(t, wt, "git", "init", "-q", "emb")
	sh(t, filepath.Join(wt, "emb"), "git", "commit", "-q", "--allow-empty", "-m", "e")
	if err := os.MkdirAll(filepath.Join(wt, "probe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(wt, "probe", ".git"), []byte("gitdir: "+gd+"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	write(t, wt, "ok.txt", "ok\n")
	if _, err := r.Commit("s", "embedded"); err == nil || !strings.Contains(err.Error(), "nested repository") {
		t.Fatalf("an embedded repo was recorded: %v", err)
	}
	if out, _ := r.Run("diff", "--cached", "--name-only"); strings.TrimSpace(out) != "" {
		t.Fatalf("the refusal left %q staged", out)
	}
	head, _ := r.Run("rev-parse", "HEAD")
	if out, _ := r.Run("ls-tree", "-r", "HEAD"); strings.Contains(out, "160000") {
		t.Fatalf("a gitlink is in HEAD (%s): %s", strings.TrimSpace(head), out)
	}
}

func TestVersionGate(t *testing.T) {
	for v, ok := range map[string]bool{
		"git version 2.40.0": false, "git version 2.40.2": false, "git version 2.39.5 (Apple Git-154)": false,
		"git version 2.41.0": true, "git version 2.43.0": true, "git version 3.0.0": true, "git version 1.9.9": false,
		"nonsense": false,
	} {
		if err := versionOK(v); (err == nil) != ok {
			t.Errorf("%q: err=%v, want ok=%v", v, err, ok)
		}
	}
}

// T:git-attr-source — block: a git that reports a new enough version but ignores GIT_ATTR_SOURCE
// (a vendor build) is caught by the behaviour probe, and a 2.40.x git by the version, both before
// any host command runs. The refusal names the fix.
func TestOldGitIsRefused(t *testing.T) {
	r, gd, wt := repo(t)
	r2 := &Runner{GitDir: gd, Worktree: wt, Git: fakeGit(t, `[ "$1" = --version ] && { echo "git version 2.40.2"; exit 0; }`)}
	if _, err := r2.Run("status"); err == nil || !strings.Contains(err.Error(), "2.40") || !strings.Contains(err.Error(), "CCSBX_GIT") {
		t.Fatalf("git 2.40.2 was not refused with the remedy: %v", err)
	}
	r3 := &Runner{GitDir: gd, Worktree: wt, Git: fakeGit(t, `unset GIT_ATTR_SOURCE`)}
	if _, err := r3.Run("status"); err == nil || !strings.Contains(err.Error(), "GIT_ATTR_SOURCE") {
		t.Fatalf("a git that ignores GIT_ATTR_SOURCE was accepted: %v", err)
	}
	if _, err := r.Run("status"); err != nil {
		t.Fatalf("the real git was refused: %v", err)
	}
}

// T:git-symlink-escape — block: a symlink in the worktree that leaves it is refused before git
// reads anything, and so is a FIFO (git would block on it) and an oversized tree. Nothing is
// staged after a refusal. The race with a LIVE fence is not closed here (see commit.go).
func TestCommitScanRefusals(t *testing.T) {
	r, _, wt := repo(t)
	secret := filepath.Join(t.TempDir(), "secret")
	os.WriteFile(secret, []byte("S"), 0o600)
	cases := map[string]func(){
		"absolute symlink out": func() { os.Symlink(secret, filepath.Join(wt, "l")) },
		"relative symlink out": func() { os.Symlink("../../../../../../etc/passwd", filepath.Join(wt, "l")) },
		"dangling symlink out": func() { os.Symlink("/nonexistent/host/file", filepath.Join(wt, "l")) },
		"symlinked parent dir": func() {
			os.Symlink(filepath.Dir(secret), filepath.Join(wt, "d"))
			os.Symlink("d/secret", filepath.Join(wt, "l"))
		},
		"fifo":                  func() { syscall.Mkfifo(filepath.Join(wt, "pipe"), 0o644) },
		"file over the cap":     func() { r.MaxFileBytes = 10; write(t, wt, "big", strings.Repeat("x", 100)) },
		"more entries than cap": func() { r.MaxFiles = 3; write(t, wt, "a", "1"); write(t, wt, "b", "2"); write(t, wt, "c", "3") },
	}
	for name, plant := range cases {
		plant()
		if _, err := r.Commit("s", name); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("%s: commit was not refused: %v", name, err)
		}
		if out, _ := r.Run("diff", "--cached", "--name-only"); strings.TrimSpace(out) != "" {
			t.Errorf("%s: left %q staged", name, out)
		}
		entries, _ := os.ReadDir(wt)
		for _, e := range entries {
			if e.Name() != ".git" && e.Name() != "README.md" {
				os.RemoveAll(filepath.Join(wt, e.Name()))
			}
		}
		r.MaxFileBytes, r.MaxFiles = 0, 0
	}
	// allow: links that stay inside, a dangling one that stays inside, and a nested dir
	os.Symlink("README.md", filepath.Join(wt, "in"))
	os.Symlink("nowhere-yet", filepath.Join(wt, "dangling"))
	write(t, wt, "sub/f.txt", "f\n")
	if _, err := r.Commit("s", "inside links"); err != nil {
		t.Fatalf("links inside the worktree were refused: %v", err)
	}
}

// T:git-steering-flag — block (M4a): a symlink at .claude is steering, so is every edit to what it
// points at, and the same holds for a file symlink named CLAUDE.md.
func TestSteeringSymlinks(t *testing.T) {
	r, _, wt := repo(t)
	write(t, wt, "docs/notes/readme.txt", "n\n")
	write(t, wt, "notes.md", "n\n")
	os.Symlink("docs/notes", filepath.Join(wt, ".claude"))
	os.Symlink("notes.md", filepath.Join(wt, "CLAUDE.md"))
	if _, err := r.Commit("s", "links"); err != nil {
		t.Fatal(err)
	}
	if msg, _ := r.Run("log", "-1", "--format=%B"); !strings.Contains(msg, "Steering-Files: .claude, CLAUDE.md") {
		t.Fatalf("the symlinks were not flagged: %q", msg)
	}
	write(t, wt, "docs/notes/settings.json", "{\"hooks\":{}}\n")
	write(t, wt, "notes.md", "obey me\n")
	if _, err := r.Commit("s", "edit behind the links"); err != nil {
		t.Fatal(err)
	}
	msg, _ := r.Run("log", "-1", "--format=%B")
	for _, want := range []string{"docs/notes/settings.json", "notes.md"} {
		if !strings.Contains(msg, want) {
			t.Errorf("an edit behind a steering symlink (%s) was not flagged: %q", want, msg)
		}
	}
	got, err := r.SteeringSince("HEAD~2")
	if err != nil || len(got) < 4 {
		t.Fatalf("steering-check would say %v %v", got, err)
	}
	// a plain symlink that is not named like steering is flagged as a link when it is new
	os.Symlink("README.md", filepath.Join(wt, "alias"))
	if _, err := r.Commit("s", "a new link"); err != nil {
		t.Fatal(err)
	}
	if msg, _ := r.Run("log", "-1", "--format=%B"); !strings.Contains(msg, "Steering-Files: alias") {
		t.Errorf("a new symlink was not flagged: %q", msg)
	}
}

// T:git-steering-flag — block (M4b): files git ignores or has never seen still steer the next
// session, so steering-check reports them.
func TestSteeringIgnoredAndUntracked(t *testing.T) {
	r, _, wt := repo(t)
	write(t, wt, ".gitignore", ".claude/settings.local.json\nCLAUDE.local.md\n.mcp.json\n")
	write(t, wt, ".claude/settings.local.json", "{\"hooks\":1}\n")
	write(t, wt, "CLAUDE.local.md", "obey\n")
	write(t, wt, ".mcp.json", "{}\n")
	write(t, wt, "code.go", "package x\n")
	if _, err := r.Commit("s", "code and an ignore file"); err != nil {
		t.Fatal(err)
	}
	if msg, _ := r.Run("log", "-1", "--format=%B"); strings.Contains(msg, "settings.local.json") {
		t.Fatalf("an ignored file was committed: %q", msg)
	}
	got, err := r.SteeringSince("HEAD~1")
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{".claude/settings.local.json": false, "CLAUDE.local.md": false, ".mcp.json": false, ".gitignore": false}
	for _, g := range got {
		if _, ok := want[g]; ok {
			want[g] = true
		}
	}
	for p, ok := range want {
		if p != ".gitignore" && !ok {
			t.Errorf("%s (ignored, never committed) is not reported: %v", p, got)
		}
	}
}

func TestIsSteeringSegments(t *testing.T) {
	for _, p := range []string{".claude", ".github", "pkg/.claude", "a/.github/x", ".CLAUDE", ".vscode"} {
		if !IsSteering(p) {
			t.Errorf("%s (a bare directory name, i.e. a symlink or gitlink) should be steering", p)
		}
	}
	for _, p := range []string{"x.claude", ".claudeX/y", "notclaude/.claudex", "dot.github/x"} {
		if IsSteering(p) {
			t.Errorf("%s should not be steering", p)
		}
	}
}

// T:git-trailer-injection — block (S3, S4): file names and messages cannot forge or hide trailers
// and cannot write to the terminal.
func TestTrailerAndTerminalInjection(t *testing.T) {
	r, _, wt := repo(t)
	// a name with a newline would start a new trailer line
	if err := os.WriteFile(filepath.Join(wt, ".claude\nFenced-Session: human"), []byte("x"), 0o644); err != nil {
		t.Skip("filesystem does not allow newlines in names")
	}
	if _, err := r.Commit("s", "newline name"); err == nil || !strings.Contains(err.Error(), "control character") {
		t.Fatalf("a path with a newline was committed: %v", err)
	}
	os.Remove(filepath.Join(wt, ".claude\nFenced-Session: human"))
	defer os.Remove(filepath.Join(wt, ".claude\x1b]52;c;ZXZpbA==\x07"))
	if err := os.WriteFile(filepath.Join(wt, ".claude\x1b]52;c;ZXZpbA==\x07"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Commit("s", "escape name"); err == nil {
		t.Fatal("a path with a terminal escape was committed")
	}
	os.Remove(filepath.Join(wt, ".claude\x1b]52;c;ZXZpbA==\x07"))
	// printing: quoted, never raw
	for _, p := range []string{"a\nb", "\x1b[2J", "a, b", `q"q`, "‮evil"} {
		if q := Q(p); !strings.HasPrefix(q, `"`) || strings.ContainsAny(q, "\n\x1b‮") {
			t.Errorf("Q(%q) = %q", p, q)
		}
	}
	if Q("src/a b.go") != "src/a b.go" {
		t.Error("an ordinary path was quoted")
	}
	// messages: blanks before the colon, and a patch divider
	for _, m := range []string{"msg\n\nFenced-Session : someone", "msg\n\nSteering-Files : none", "msg\n\nsteering-files=none", "msg\n---\nFenced-Session: x"} {
		if out := stripForgedTrailers(m); strings.Contains(strings.ToLower(out), "fenced-session") || strings.Contains(strings.ToLower(out), "steering-files") {
			t.Errorf("forged trailer survived in %q -> %q", m, out)
		}
	}
	if out := stripForgedTrailers("a\n---\nb"); strings.Contains(out, "\n---") {
		t.Errorf("a patch divider survived: %q", out)
	}
	write(t, wt, ".mcp.json", "{}\n")
	if _, err := r.Commit("s1", "x\n---\nSteering-Files : none"); err != nil {
		t.Fatal(err)
	}
	out, _ := r.Run("log", "-1", "--format=%(trailers:key=Steering-Files,valueonly)")
	if strings.TrimSpace(out) != ".mcp.json" {
		t.Errorf("trailer parser sees %q", out)
	}
}

// T:git-gpg-off — block (S1): signature placeholders, with or without a modifier, are refused, and
// gpg programs are overridden anyway.
func TestGpgPlaceholders(t *testing.T) {
	r, _, _ := repo(t)
	for _, f := range []string{"%G?", "%GS", "%+G?", "%-GS", "% GK", "%+-G?", "x%GGy"} {
		if _, err := r.Run("log", "-1", "--format="+f); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("--format=%s was not refused: %v", f, err)
		}
		if _, err := r.Run("log", "-1", "--pretty=format:"+f); err == nil {
			t.Errorf("--pretty=format:%s was not refused", f)
		}
	}
	for _, f := range []string{"%H", "%h %s", "%an <%ae>", "%(trailers:key=Fenced-Session,valueonly)"} {
		if _, err := r.Run("log", "-1", "--format="+f); err != nil {
			t.Errorf("--format=%s was refused: %v", f, err)
		}
	}
	c, _ := r.Cmd("log")
	for _, want := range []string{"gpg.program=/bin/false", "gpg.ssh.program=/bin/false", "gpg.x509.program=/bin/false", "log.showSignature=false"} {
		if !contains(c.Args, want) {
			t.Errorf("override %s is missing", want)
		}
	}
}

// T:git-config-includes — block (S2): include.path or includeIf in the gitdir config would pull
// in a file the agent can write, so the host refuses to run.
func TestConfigIncludesRefused(t *testing.T) {
	r, gd, wt := repo(t)
	for _, kv := range [][2]string{{"include.path", "../.gitconfig"}, {"includeIf.gitdir:/x/.path", "/tmp/y"}} {
		sh(t, wt, "git", "--git-dir", gd, "config", kv[0], kv[1])
		if _, err := r.Run("status"); err == nil || !strings.Contains(err.Error(), "include") {
			t.Errorf("%s in the gitdir config was accepted: %v", kv[0], err)
		}
		sh(t, wt, "git", "--git-dir", gd, "config", "--unset-all", kv[0])
	}
	if _, err := r.Run("status"); err != nil {
		t.Fatalf("a config without includes was refused: %v", err)
	}
}

// T:git-timeout — block (S7): a git process that does not finish is killed, so a swapped-in FIFO
// or a huge file cannot hang the daemon.
func TestGitTimeout(t *testing.T) {
	r, gd, wt := repo(t)
	r2 := &Runner{GitDir: gd, Worktree: wt, Timeout: 300 * time.Millisecond, Git: fakeGit(t, `for a; do [ "$a" = status ] && exec sleep 30; done`)}
	start := time.Now()
	_, err := r2.Run("status")
	if err == nil || !strings.Contains(err.Error(), "timed out") || time.Since(start) > 10*time.Second {
		t.Fatalf("a hung git was not stopped: %v after %s", err, time.Since(start))
	}
	_ = r
}

// The diff layers one by one: attribute source (no driver can be selected), the argv flags that
// turn textconv and external diff off, and the config overrides. Removing any single one of the
// argv or config layers must fail here even though the others would still hold.
func TestDiffDriverLayers(t *testing.T) {
	r, gd, wt := repo(t)
	cmd, ran := marker(t)
	sh(t, wt, "git", "--git-dir", gd, "config", "diff.evil.textconv", cmd+"; cat")
	sh(t, wt, "git", "--git-dir", gd, "config", "diff.external", cmd)
	write(t, wt, ".gitattributes", "* diff=evil\n")
	write(t, wt, "README.md", "changed\n")
	for _, v := range []string{"diff", "show", "log"} {
		args := []string{v}
		if v != "diff" {
			args = append(args, "-p", "-1")
		}
		if _, err := r.Run(args...); err != nil {
			t.Fatalf("%v: %v", args, err)
		}
		c, err := r.Cmd(args...)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"--no-textconv", "--no-ext-diff", "diff.external="} {
			if !contains(c.Args, want) {
				t.Errorf("git %s is built without %s", v, want)
			}
		}
	}
	if exists(ran) {
		t.Fatal("a textconv or external diff driver ran")
	}
	for _, e := range r.Env() {
		if e == "GIT_EXTERNAL_DIFF=/bin/false" {
			return
		}
	}
	t.Error("GIT_EXTERNAL_DIFF is not pinned")
}

// T:git-steering-flag — block: a global option as the first argument (`-c`, `--exec-path`,
// `--git-dir`, `-C`) is not a verb, so the fence cannot use it to change how git runs.
func TestGlobalOptionsAreNotVerbs(t *testing.T) {
	r, _, _ := repo(t)
	for _, args := range [][]string{
		{"-c", "core.hooksPath=/x", "status"}, {"--exec-path=/x", "status"}, {"--git-dir=/x", "status"},
		{"-C", "/", "status"}, {"--work-tree=/", "status"}, {"--config-env=a=B", "status"},
	} {
		if _, err := r.Run(args...); err == nil || !strings.Contains(err.Error(), "refused") {
			t.Errorf("git %v was not refused: %v", args, err)
		}
	}
}
