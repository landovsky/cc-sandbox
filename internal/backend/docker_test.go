// Tests for the docker backend (U1b): the flags a fence is started with, the per-generation
// directories, the gitdir and worktree path rules, the home sweep and the orphan reaper. They exist
// so that every refusal in this package has a block twin that goes red when the check is removed
// (invariant 23), and none of them needs a docker daemon: they build argv and touch temp dirs only.
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
		Workdir: wt, WorktreeRoot: root, MemoryMB: 4096, Pids: 512, NotEvid: true}
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
	gd, err := ResolveGitDir(wt, os.Getuid())
	if err != nil || gd != filepath.Join(repo, ".git", "worktrees", "wt") {
		t.Fatalf("linked worktree: %q %v", gd, err)
	}
	if gd, err := ResolveGitDir(repo, os.Getuid()); err != nil || gd != filepath.Join(repo, ".git") {
		t.Fatalf("plain repo: %q %v", gd, err)
	}
	if gd, err := ResolveGitDir(vt(t), os.Getuid()); err != nil || gd != "" {
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
		if gd, err := ResolveGitDir(wt, os.Getuid()); err == nil {
			t.Errorf("%s: accepted, gitdir %q", name, gd)
		}
	}
	// a symlinked .git
	os.Remove(filepath.Join(wt, ".git"))
	os.Symlink("/etc", filepath.Join(wt, ".git"))
	if gd, err := ResolveGitDir(wt, os.Getuid()); err == nil {
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

// U1b-N1: the 0777 leaves are a direct-mode compromise (the fence's uid 10001 does not own them).
// Helper mode wants 10001:10001 0700 leaves made by ccsbx-root. Until that exists, Prepare must
// refuse in helper mode rather than quietly ship the direct-mode permissions.
func TestWorldWritableLeavesAreDirectModeOnly(t *testing.T) {
	d := spec(t)
	if err := d.Prepare(); err != nil {
		t.Fatal(err)
	}
	for _, l := range []string{"out", "in", "managed"} {
		st, err := os.Stat(filepath.Join(d.GenDir(), l))
		if err != nil || st.Mode().Perm() != 0o777 {
			t.Fatalf("direct mode leaf %s: %v %v", l, st, err)
		}
	}
	h := spec(t)
	h.NotEvid = false // helper mode
	if err := h.Prepare(); err == nil {
		t.Fatal("helper mode created generation dirs with the direct-mode 0777 leaves")
	}
	if _, err := os.Stat(h.GenDir()); err == nil {
		t.Fatal("helper mode left a generation directory behind after refusing")
	}
}

// RC-1: a gitdir that a fence could write (another uid owns it, or it is world- or foreign-group
// writable) must not be accepted for a new session: it may be a directory the earlier fence
// planted, and mounting it would show this fence that fence's files.
func TestGitDirMustBeHostOwned(t *testing.T) {
	repo, wt := linked(t)
	common := filepath.Join(repo, ".git")
	gd := filepath.Join(common, "worktrees", "wt")
	uid := os.Getuid()
	if _, err := ResolveGitDir(wt, uid); err != nil {
		t.Fatalf("a host-owned gitdir was refused: %v", err)
	}
	hook := filepath.Join(common, "hooks", "pre-commit")
	if err := os.WriteFile(hook, []byte("#!/bin/sh\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	info := filepath.Join(common, "info", "extra")
	if err := os.WriteFile(info, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for name, path := range map[string]string{
		"gitdir": gd, "gitdir HEAD": filepath.Join(gd, "HEAD"), "common config": filepath.Join(common, "config"),
		"common hooks dir": filepath.Join(common, "hooks"), "a hook file": hook, "info dir": filepath.Join(common, "info"),
		"a file in info/": info, "worktrees parent": filepath.Join(common, "worktrees"), "commondir root": common,
	} {
		st, err := os.Stat(path)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		mode := st.Mode().Perm()
		if err := os.Chmod(path, mode|0o002); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveGitDir(wt, uid); err == nil {
			t.Errorf("a world-writable %s was accepted", name)
		}
		if err := os.Chmod(path, mode); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := ResolveGitDir(wt, uid); err != nil {
		t.Fatalf("restored: %v", err)
	}
	// a plain .git directory inside the worktree is the same case
	plain := vt(t)
	sh(t, plain, "git", "init", "-q", "-b", "main", ".")
	if err := os.Chmod(filepath.Join(plain, ".git"), 0o777); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveGitDir(plain, uid); err == nil {
		t.Error("a world-writable .git directory was accepted")
	}
}

// The owner is the caller named by the daemon, not whoever runs the check: in helper mode the
// process is root, and a gitdir of the caller's must be accepted for them and refused for another.
func TestGitDirOwnerIsTheCaller(t *testing.T) {
	if os.Getuid() == 0 {
		t.Skip("needs a non-root user to be a different owner")
	}
	_, wt := linked(t)
	if _, err := ResolveGitDir(wt, 0); err == nil || !strings.Contains(err.Error(), "owned by uid") {
		t.Errorf("a gitdir owned by the test user was not refused as not root's: %v", err)
	}
	if _, err := ResolveGitDir(wt, os.Getuid()); err != nil {
		t.Errorf("the caller's own gitdir was refused: %v", err)
	}
}

// Units for the owner rules with injected owner and group: a foreign uid or group cannot be created
// without root, so the rule is exercised directly.
func TestOwnerRules(t *testing.T) {
	o := owner{uid: 1000, gids: map[int]bool{1000: true, 50: true, FenceUID: true}}
	cases := []struct {
		name     string
		perm     os.FileMode
		uid, gid int
		ok       bool
	}{
		{"own file", 0o644, 1000, 1000, true},
		{"foreign uid", 0o644, 1001, 1000, false},
		{"root-owned", 0o755, 0, 0, false},
		{"world-writable", 0o666, 1000, 1000, false},
		{"group-writable by the caller's group", 0o664, 1000, 50, true},
		{"group-writable by a foreign group", 0o664, 1000, 77, false},
		{"group-writable by the fence's gid", 0o664, 1000, FenceUID, false},
		{"foreign group, not writable", 0o644, 1000, 77, true},
	}
	for _, c := range cases {
		if err := o.checkMode("p", c.perm, c.uid, c.gid); (err == nil) != c.ok {
			t.Errorf("%s: err=%v, want ok=%v", c.name, err, c.ok)
		}
	}
}

// noOwner is a FileInfo whose Sys() carries no uid, as on a platform without Stat_t.
type noOwner struct{ os.FileInfo }

func (noOwner) Sys() any { return nil }

// Fail closed: a stat with no owner information must be refused, not waved through.
func TestOwnerCheckFailsClosedWithoutOwnerInfo(t *testing.T) {
	st, err := os.Lstat(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	o, err := ownerOf(os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	if err := o.check("p", noOwner{st}); err == nil {
		t.Error("a stat without owner information was accepted")
	}
	if err := o.check("p", st); err != nil {
		t.Errorf("a real stat of the user's own dir was refused: %v", err)
	}
}

// A symlink where git expects a file or directory could point the mount's contents anywhere.
func TestGitDirRefusesSymlinksInside(t *testing.T) {
	repo, wt := linked(t)
	common := filepath.Join(repo, ".git")
	uid := os.Getuid()
	elsewhere := filepath.Join(vt(t), "x")
	if err := os.WriteFile(elsewhere, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{filepath.Join(common, "config"), filepath.Join(common, "hooks", "pre-commit"), filepath.Join(common, "info", "exclude")} {
		bak := p + ".bak"
		if err := os.Rename(p, bak); err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err := os.Symlink(elsewhere, p); err != nil {
			t.Fatal(err)
		}
		if _, err := ResolveGitDir(wt, uid); err == nil {
			t.Errorf("a symlink at %s was accepted", p)
		}
		os.Remove(p)
		if _, err := os.Stat(bak); err == nil {
			os.Rename(bak, p)
		}
	}
	if _, err := ResolveGitDir(wt, uid); err != nil {
		t.Fatalf("restored: %v", err)
	}
}

// commondir decides which directory holds the shared config and hooks. If it cannot be read, or
// names something that is not a git directory, the shared part cannot be vouched for.
func TestGitDirCommondirIsVerified(t *testing.T) {
	repo, wt := linked(t)
	gd := filepath.Join(repo, ".git", "worktrees", "wt")
	cd := filepath.Join(gd, "commondir")
	orig, err := os.ReadFile(cd)
	if err != nil {
		t.Fatal(err)
	}
	uid := os.Getuid()
	// unreadable as a file (here: a directory) is an error that is not "does not exist"
	os.Remove(cd)
	if err := os.Mkdir(cd, 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveGitDir(wt, uid); err == nil {
		t.Error("an unreadable commondir was treated as 'not a linked worktree'")
	}
	os.Remove(cd)
	// naming a directory that is not a git dir
	if err := os.WriteFile(cd, []byte(vt(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveGitDir(wt, uid); err == nil {
		t.Error("a commondir that is not a git directory was accepted")
	}
	// reached through a symlink: resolved first, and then judged by what it points at
	link := filepath.Join(vt(t), "common")
	if err := os.Symlink(filepath.Join(repo, ".git"), link); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cd, []byte(link), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveGitDir(wt, uid); err != nil {
		t.Errorf("a commondir reached through a symlink, with a clean gitdir, was refused: %v", err)
	}
	if err := os.Chmod(filepath.Join(repo, ".git", "config"), 0o666); err != nil {
		t.Fatal(err)
	}
	if _, err := ResolveGitDir(wt, uid); err == nil {
		t.Error("a world-writable config behind a symlinked commondir was not seen")
	}
	os.Chmod(filepath.Join(repo, ".git", "config"), 0o644)
	if err := os.WriteFile(cd, orig, 0o644); err != nil {
		t.Fatal(err)
	}
}

// Check and mount are a whole session apart: every later generation re-checks the recorded
// gitdir, so a gitdir that became writable after generation 1 is refused for generation 2.
func TestPrepareRechecksTheRecordedGitDirOnResume(t *testing.T) {
	repo, wt := linked(t)
	gd, err := ResolveGitDir(wt, os.Getuid())
	if err != nil {
		t.Fatal(err)
	}
	s := spec(t)
	s.Workdir, s.WorktreeRoot, s.GitDir, s.Uid = wt, filepath.Dir(wt), gd, os.Getuid()
	if err := s.Prepare(); err != nil {
		t.Fatalf("generation 1: %v", err)
	}
	if err := os.Chmod(filepath.Join(repo, ".git", "hooks"), 0o777); err != nil {
		t.Fatal(err)
	}
	s.Gen = 2
	if err := s.Prepare(); err == nil {
		t.Error("generation 2 mounted a gitdir whose hooks became world-writable")
	}
}
