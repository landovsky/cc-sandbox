package backend

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// vt is a temp dir under /var/tmp: /tmp is a fixed mount point of the fence, so a worktree there
// is (rightly) refused.
func vt(t *testing.T) string {
	t.Helper()
	d, err := os.MkdirTemp("/var/tmp", "ccsbx-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(d) })
	d, _ = filepath.EvalSymlinks(d)
	return d
}

func spec(t *testing.T) Spec {
	root := vt(t)
	wt := filepath.Join(root, "wt")
	os.Mkdir(wt, 0o755)
	return Spec{Sid: "sabc123", Label: "cc-sandbox:x", Gen: 1, Image: "img", StateDir: filepath.Join(root, "state"),
		Workdir: wt, WorktreeRoot: root, MemoryMB: 4096, Pids: 512}
}

func has(args []string, seq ...string) bool {
	for i := 0; i+len(seq) <= len(args); i++ {
		if strings.Join(args[i:i+len(seq)], "\x00") == strings.Join(seq, "\x00") {
			return true
		}
	}
	return false
}

// The fixed flags are the fence. A refactor that drops one must fail here, before it fails on a
// live session.
func TestRunArgsAreTheFence(t *testing.T) {
	a := spec(t).RunArgs()
	for _, want := range [][]string{
		{"--user", "10001:10001"}, {"--read-only"}, {"--cap-drop", "ALL"},
		{"--security-opt", "no-new-privileges"}, {"--pids-limit", "512"},
		{"--memory", "4096m"}, {"--memory-swap", "4096m"}, {"--dns", "127.0.0.1"},
		{"--network", "ccsbx-net-sabc123-g1"}, {"--rm"},
	} {
		if !has(a, want...) {
			t.Errorf("missing %v in %v", want, a)
		}
	}
	for _, bad := range []string{"--privileged", "--network=host", "host", "--pid=host", "--cap-add", "-e=", "--env-file", "docker.sock"} {
		for _, x := range a {
			if x == bad || strings.Contains(x, "docker.sock") {
				t.Errorf("forbidden %q in argv", bad)
			}
		}
	}
	if a[len(a)-1] != "claude" {
		t.Errorf("default command is not claude: %v", a[len(a)-1])
	}
}

// No host env: only the explicit short list reaches the container.
func TestEnvIsExplicit(t *testing.T) {
	a := spec(t).RunArgs()
	var envs []string
	for i, x := range a {
		if x == "-e" {
			envs = append(envs, a[i+1])
		}
	}
	for _, e := range envs {
		k, _, _ := strings.Cut(e, "=")
		switch k {
		case "HOME", "CCSBX_SID", "CCSBX_GEN":
		default:
			t.Errorf("unexpected env %s", e)
		}
	}
}

// Invariant 11: a generation's out/ is created once and never reused.
func TestOneOutboxPerGeneration(t *testing.T) {
	s := spec(t)
	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}
	if err := s.Prepare(); err == nil {
		t.Fatal("the same generation was prepared twice, sharing an outbox")
	}
	s2 := s
	s2.Gen = 2
	if err := s2.Prepare(); err != nil {
		t.Fatal(err)
	}
	if s.GenDir() == s2.GenDir() {
		t.Fatal("generations share a directory")
	}
	out1 := filepath.Join(s.GenDir(), "out")
	os.WriteFile(filepath.Join(out1, "events.jsonl"), []byte("x"), 0o644)
	if _, err := os.Stat(filepath.Join(s2.GenDir(), "out", "events.jsonl")); err == nil {
		t.Fatal("g1's outbox content visible in g2")
	}
}

// Resume needs the same home across generations; a different node must get a different one.
func TestHomeIsPerNode(t *testing.T) {
	a, b := spec(t), spec(t)
	b.StateDir = a.StateDir
	b.Gen = 2
	if a.HomeDir() != b.HomeDir() {
		t.Fatal("same label, different home across generations")
	}
	b.Label = "other:node"
	if a.HomeDir() == b.HomeDir() {
		t.Fatal("two nodes share a home")
	}
}

// sh runs a command in dir (real git for the gitdir tests).
func sh(t *testing.T, dir, name string, args ...string) {
	t.Helper()
	c := exec.Command(name, args...)
	c.Dir = dir
	c.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_AUTHOR_NAME=t", "GIT_AUTHOR_EMAIL=t@t", "GIT_COMMITTER_NAME=t", "GIT_COMMITTER_EMAIL=t@t")
	if b, err := c.CombinedOutput(); err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, b)
	}
}

// linked makes a repo and a linked worktree, the layout cc-flow creates, and returns both.
func linked(t *testing.T) (repo, wt string) {
	t.Helper()
	root := vt(t)
	repo, wt = filepath.Join(root, "repo"), filepath.Join(root, "wt")
	os.Mkdir(repo, 0o755)
	sh(t, repo, "git", "init", "-q", "-b", "main", ".")
	sh(t, repo, "git", "commit", "-q", "--allow-empty", "-m", "i")
	sh(t, repo, "git", "worktree", "add", "-q", wt, "-b", "w")
	return
}

// T:gitdir-from-record — allow: the layouts cc-flow really produces still get their gitdir
// mounted read-only, so host-side git keeps working for the session.
func TestGitDirFromRecordAllow(t *testing.T) {
	repo, wt := linked(t)
	gd, err := ResolveGitDir(wt)
	if err != nil || gd != filepath.Join(repo, ".git", "worktrees", "wt") {
		t.Fatalf("linked worktree: %q %v", gd, err)
	}
	if gd, err := ResolveGitDir(repo); err != nil || gd != filepath.Join(repo, ".git") {
		t.Fatalf("plain repo: %q %v", gd, err)
	}
	if gd, err := ResolveGitDir(vt(t)); err != nil || gd != "" {
		t.Fatalf("no git: %q %v", gd, err)
	}
	s := spec(t)
	s.GitDir = gd
	s.GitDir = filepath.Join(repo, ".git", "worktrees", "wt")
	if !has(s.RunArgs(), "--mount", "type=bind,src="+s.GitDir+",dst="+s.GitDir+",readonly") {
		t.Fatal("the gitdir is not mounted readonly")
	}
}

// T:gitdir-from-record — block: the worktree's `.git` file is written by the agent, so whatever
// it names must never be mounted into a later generation. Each case below is a host path an
// agent might name.
func TestGitDirFromRecordBlock(t *testing.T) {
	_, wt := linked(t)
	host := vt(t) // stands for /var/run/x, /etc, a socket dir...
	write := func(content string) {
		os.Remove(filepath.Join(wt, ".git"))
		os.WriteFile(filepath.Join(wt, ".git"), []byte(content), 0o644)
	}
	otherRepo, otherWt := linked(t)
	_ = otherRepo
	otherGD := filepath.Join(filepath.Dir(otherWt), "repo", ".git", "worktrees", "wt")
	withHEAD := vt(t)
	os.WriteFile(filepath.Join(withHEAD, "HEAD"), []byte("ref: refs/heads/x\n"), 0o644)
	for name, content := range map[string]string{
		"any host directory":                      "gitdir: " + host + "\n",
		"a directory with a HEAD but no back-ref": "gitdir: " + withHEAD + "\n",
		"another worktree's real gitdir":          "gitdir: " + otherGD + "\n",
		"/etc":                                    "gitdir: /etc\n",
		"the root":                                "gitdir: /\n",
		"a relative path out of the worktree":     "gitdir: ../../../../../../etc\n",
		"not a gitdir line at all":                "hello\n",
	} {
		write(content)
		if gd, err := ResolveGitDir(wt); err == nil {
			t.Errorf("%s: accepted, gitdir %q", name, gd)
		}
	}
	// a symlinked .git
	os.Remove(filepath.Join(wt, ".git"))
	os.Symlink("/etc", filepath.Join(wt, ".git"))
	if gd, err := ResolveGitDir(wt); err == nil {
		t.Errorf(".git symlink accepted: %q", gd)
	}
	// mount specs can't be bent by a comma or colon in a path
	for _, p := range []string{"/a,b", "/a:b", "/a\nb"} {
		if CheckPath(p) == nil {
			t.Errorf("%q accepted", p)
		}
	}
}

func TestCheckWorkdir(t *testing.T) {
	root := vt(t)
	state := filepath.Join(root, "state")
	os.MkdirAll(state, 0o755)
	mk := func(p string) string { p = filepath.Join(root, p); os.MkdirAll(p, 0o755); return p }
	good, _ := CheckWorkdir(mk("proj/wt"), root, state)
	if good != filepath.Join(root, "proj/wt") {
		t.Fatalf("good path refused: %q", good)
	}
	link := filepath.Join(root, "sneaky")
	os.Symlink("/etc", link)
	for name, w := range map[string]string{
		"the root itself":      root,
		"above the root":       filepath.Dir(root),
		"outside the root":     t.TempDir(),
		"a symlink to /etc":    link,
		"the state dir":        state,
		"inside the state dir": mk("state/x"),
		"relative":             "wt",
		"unclean":              root + "/a/../proj",
		"a comma":              mk("a,b"),
	} {
		if _, err := CheckWorkdir(w, root, state); err == nil {
			t.Errorf("%s accepted: %s", name, w)
		}
	}
	if _, err := CheckWorkdir(good, "", state); err == nil {
		t.Error("no worktree root configured must refuse, not allow everything")
	}
	if _, err := CheckWorkdir(good, "/", state); err == nil {
		t.Error("root / accepted")
	}
}

// With the home as the worktree root, secrets directories under it are not worktrees.
func TestWorkdirNotInHiddenHomeDirs(t *testing.T) {
	h := vt(t)
	t.Setenv("HOME", h)
	os.MkdirAll(filepath.Join(h, ".ssh", "x"), 0o755)
	os.MkdirAll(filepath.Join(h, "git", "x"), 0o755)
	if _, err := CheckWorkdir(filepath.Join(h, ".ssh", "x"), h, "/nonexistent"); err == nil {
		t.Error(".ssh accepted")
	}
	if _, err := CheckWorkdir(filepath.Join(h, "git", "x"), h, "/nonexistent"); err != nil {
		t.Error(err)
	}
}

// S2 argv: the sweep runs as root, but with no network, a read-only root and only the capabilities
// it needs, and mounts nothing but the home and the quarantine.
func TestSweepArgs(t *testing.T) {
	a := spec(t).SweepArgs()
	for _, want := range [][]string{{"--network", "none"}, {"--read-only"}, {"--cap-drop", "ALL"}, {"--security-opt", "no-new-privileges"}} {
		if !has(a, want...) {
			t.Errorf("missing %v", want)
		}
	}
	mounts := 0
	for _, x := range a {
		if x == "--mount" {
			mounts++
		}
	}
	if mounts != 2 {
		t.Errorf("%d mounts, want home and quarantine only", mounts)
	}
	for _, keep := range []string{".claude/projects", ".claude", ".claude/"} {
		for _, x := range a {
			if x == keep {
				t.Errorf("%s would be swept: resume needs the transcript store", keep)
			}
		}
	}
	if !has(a, ".claude/CLAUDE.md") || !has(a, ".bashrc") {
		t.Error("steering files are not in the sweep list")
	}
}

func TestMovedParsesSweepOutput(t *testing.T) {
	if m := moved("moved .bashrc\nnoise\nmoved .claude/CLAUDE.md\n"); len(m) != 2 || m[1] != ".claude/CLAUDE.md" {
		t.Fatal(m)
	}
}
