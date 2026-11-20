// Tests for the docker backend (U1b): the flags a fence is started with, the per-generation
// directories, the gitdir and worktree path rules, the home sweep and the orphan reaper. They exist
// so that every refusal in this package has a block twin that goes red when the check is removed
// (invariant 23), and none of them needs a docker daemon: they build argv and touch temp dirs only.

// Tests for the docker backend. They guard against the fence's run arguments (mounts, network,
// env, one outbox per generation, per-node home, worktree bounds) drifting from what the plan says.

package backend

import (
	"fmt"
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
// Helper mode (ccsbx-root, as root) makes out/ and the home 10001:10001 0700, in/ and managed/
// root-owned 0755, and nothing world-writable.
func TestDirectModeLeavesAre0777(t *testing.T) {
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
}

// Helper mode (the block twin of the above): nothing is 0777 and the fence's directories are the
// fence's, made fresh and chowned only after creation.
func TestWorldWritableLeavesAreDirectModeOnly(t *testing.T) {
	h := spec(t)
	h.NotEvid = false // helper mode
	owners := map[string][2]int{}
	h.Chown = func(p string, uid, gid int) error { owners[p] = [2]int{uid, gid}; return nil }
	if err := h.Prepare(); err != nil {
		t.Fatal(err)
	}
	want := map[string]struct {
		mode os.FileMode
		own  [2]int
	}{
		filepath.Join(h.GenDir(), "out"):     {0o700, [2]int{FenceUID, FenceUID}},
		filepath.Join(h.GenDir(), "in"):      {0o755, [2]int{0, 0}},
		filepath.Join(h.GenDir(), "managed"): {0o755, [2]int{0, 0}},
		h.HomeDir():                          {0o700, [2]int{FenceUID, FenceUID}},
	}
	for p, w := range want {
		st, err := os.Stat(p)
		if err != nil || st.Mode().Perm() != w.mode {
			t.Errorf("%s: %v %v, want %v", p, st, err, w.mode)
		}
		if owners[p] != w.own {
			t.Errorf("%s chowned to %v, want %v", p, owners[p], w.own)
		}
	}
	if st, _ := os.Stat(h.GenDir()); st.Mode().Perm() != 0o711 {
		t.Errorf("the generation dir is %v, want 0711 (traversable by name, not listable)", st.Mode().Perm())
	}
	// a later generation reuses the home and must not chown the fence's files again
	h2 := h
	h2.Gen = 2
	owners = map[string][2]int{}
	if err := h2.Prepare(); err != nil {
		t.Fatal(err)
	}
	if _, again := owners[h.HomeDir()]; again {
		t.Error("generation 2 chowned the existing home")
	}
	// the same generation twice is refused, as in direct mode
	if err := h.Prepare(); err == nil {
		t.Error("a generation directory was reused in helper mode")
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

// Stop removes exactly the container and network named from sid and gen. A fake docker records
// its argv, so no daemon is needed.
func TestStopRemovesExactNames(t *testing.T) {
	d := vt(t)
	log := filepath.Join(d, "log")
	fake := filepath.Join(d, "docker")
	if err := os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" >> "+log+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	r := &Runner{Docker: fake}
	if err := r.Stop(Spec{Sid: "s0abcdefgh", Gen: 3}); err != nil {
		t.Fatal(err)
	}
	b, _ := os.ReadFile(log)
	if string(b) != "rm -f ccsbx-s0abcdefgh-g3\nnetwork rm ccsbx-net-s0abcdefgh-g3\n" {
		t.Fatalf("docker was asked: %q", b)
	}
}

// S6: the node home is keyed by owner, class and label. The same label under another owner, or in
// the e2e class, must be another directory, or a candidate image would read and write a production
// node's transcripts and one owner could open another's.
func TestHomeIsKeyedByOwnerClassAndLabel(t *testing.T) {
	a := spec(t)
	a.Uid, a.Class = 1000, "prod"
	same := a
	if a.HomeDir() != same.HomeDir() {
		t.Fatal("the home is not stable")
	}
	b := a
	b.Uid = 1001
	c := a
	c.Class = "e2e"
	d := a
	d.Label = "other"
	for name, x := range map[string]Spec{"another owner": b, "the e2e class": c, "another label": d} {
		if x.HomeDir() == a.HomeDir() {
			t.Errorf("%s shares the home", name)
		}
	}
	if ClassOf(true) != "e2e" || ClassOf(false) != "prod" {
		t.Error("ClassOf")
	}
}

// M1: docker is given the staging paths when there are any, and the container still sees the
// original paths.
func TestRunArgsMountTheStagedSources(t *testing.T) {
	s := spec(t)
	s.GitDir = "/h/repo/.git/worktrees/x"
	s.MountWorkdir, s.MountGitDir = "/stage/wt", "/stage/gd"
	a := s.RunArgs()
	if !has(a, "--mount", "type=bind,src=/stage/wt,dst="+s.Workdir) || !has(a, "--mount", "type=bind,src=/stage/gd,dst="+s.GitDir+",readonly") {
		t.Errorf("%v", a)
	}
	for _, x := range a {
		if x == "type=bind,src="+s.Workdir+",dst="+s.Workdir {
			t.Error("the original workdir path is still mounted")
		}
	}
}

// M2: helper mode has no container sweep at all: SweepArgs (the root-in-container sweep with
// extra capabilities) is not reachable there.
func TestHelperModeNeverBuildsTheContainerSweep(t *testing.T) {
	h := spec(t)
	h.NotEvid = false
	defer func() {
		if recover() == nil {
			t.Error("SweepArgs was built in helper mode")
		}
	}()
	h.SweepArgs()
}

// M2: the direct-mode sweep script no longer opens the quarantine to everyone.
func TestSweepScriptDoesNotChmod(t *testing.T) {
	if strings.Contains(sweepScript, "chmod") {
		t.Error("the sweep script changes modes")
	}
}

// M2: the helper-mode sweep moves each steering path, and a symlinked parent as the link it is,
// and never follows a link out of the home.
func TestSweepHomeGo(t *testing.T) {
	s := spec(t)
	s.NotEvid = false
	s.Chown = func(string, int, int) error { return nil }
	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}
	h := s.HomeDir()
	outside := filepath.Join(vt(t), "outside")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "settings.json"), []byte("host file"), 0o644)
	os.MkdirAll(filepath.Join(h, ".claude"), 0o755)
	os.WriteFile(filepath.Join(h, ".claude", "CLAUDE.md"), []byte("obey"), 0o644)
	os.WriteFile(filepath.Join(h, ".bashrc"), []byte("evil"), 0o644)
	os.Symlink(outside, filepath.Join(h, ".config")) // a symlink named like steering: moved as a link
	os.MkdirAll(filepath.Join(h, ".claude", "projects"), 0o755)
	os.WriteFile(filepath.Join(h, ".claude", "projects", "t.jsonl"), []byte("transcript"), 0o644)
	m, err := s.sweepHomeGo()
	if err != nil {
		t.Fatal(err)
	}
	q := s.QuarantineDir()
	for _, p := range []string{".claude/CLAUDE.md", ".bashrc"} {
		if _, err := os.Stat(filepath.Join(q, p)); err != nil {
			t.Errorf("%s was not moved: %v", p, err)
		}
		if _, err := os.Lstat(filepath.Join(h, p)); err == nil {
			t.Errorf("%s is still in the home", p)
		}
	}
	if l, err := os.Readlink(filepath.Join(q, ".config")); err != nil || l != outside {
		t.Errorf("the symlink was not moved as a link: %q %v", l, err)
	}
	if b, _ := os.ReadFile(filepath.Join(outside, "settings.json")); string(b) != "host file" {
		t.Error("the sweep wrote through a symlink")
	}
	if _, err := os.Stat(filepath.Join(h, ".claude", "projects", "t.jsonl")); err != nil {
		t.Error("the transcript store was swept (resume needs it)")
	}
	if len(m) != 3 {
		t.Errorf("moved %v", m)
	}
	if st, _ := os.Stat(q); st.Mode().Perm() != 0o700 {
		t.Errorf("the quarantine is %v, want 0700", st.Mode().Perm())
	}
	// a run again finds nothing to move
	if m, err := s.sweepHomeGo(); err != nil || len(m) != 0 {
		t.Errorf("second sweep: %v %v", m, err)
	}
}

// M2: a home whose steering parent was replaced by a symlink (.claude -> elsewhere) is moved as
// that link; the sweep does not descend into it.
func TestSweepHomeGoParentSymlink(t *testing.T) {
	s := spec(t)
	s.NotEvid = false
	s.Chown = func(string, int, int) error { return nil }
	if err := s.Prepare(); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(vt(t), "outside")
	os.MkdirAll(outside, 0o755)
	os.WriteFile(filepath.Join(outside, "CLAUDE.md"), []byte("host"), 0o644)
	os.Symlink(outside, filepath.Join(s.HomeDir(), ".claude"))
	m, err := s.sweepHomeGo()
	if err != nil || len(m) != 1 || m[0] != ".claude" {
		t.Fatalf("%v %v", m, err)
	}
	if _, err := os.Stat(filepath.Join(outside, "CLAUDE.md")); err != nil {
		t.Error("the sweep moved a file out of a directory behind a symlink")
	}
}

// N1: a docker label is not believed on its own: gc ignores a resource whose sid or gen is not
// of the right shape, or whose name is not the one that sid and gen derive.
func TestGcIgnoresForgedLabels(t *testing.T) {
	d := vt(t)
	state := filepath.Join(d, "state")
	os.MkdirAll(filepath.Join(state, "sessions"), 0o700)
	os.WriteFile(filepath.Join(state, "sessions", "s0abcdefgh.json"), []byte("{}"), 0o600)
	fake := filepath.Join(d, "docker")
	script := "#!/bin/sh\n" +
		"if [ \"$1\" = ps ]; then\n" +
		"printf 'ccsbx-s0abcdefgh-g1\\ts0abcdefgh\\t1\\n'\n" + // good
		"printf 'ccsbx-evil\\ts0abcdefgh\\t1\\n'\n" + // name does not match the label
		"printf 'ccsbx-s0abcdefgh-g99999\\ts0abcdefgh\\t99999\\n'\n" + // gen out of range
		"printf 'ccsbx-..-g1\\t..\\t1\\n'\n" + // sid is a path
		"fi\n"
	os.WriteFile(fake, []byte(script), 0o755)
	o, err := (&Runner{Docker: fake}).Orphans(state)
	if err != nil {
		t.Fatal(err)
	}
	if len(o) != 1 || o[0].Name != "ccsbx-s0abcdefgh-g1" {
		t.Fatalf("%+v", o)
	}
}

// M2 end to end: a helper-mode run sweeps in Go and never starts a container for it, and the
// staging hook runs before docker is asked for anything and is undone after.
func TestHelperRunHasNoSweepContainerAndStagesFirst(t *testing.T) {
	d := vt(t)
	log := filepath.Join(d, "log")
	fake := filepath.Join(d, "docker")
	os.WriteFile(fake, []byte("#!/bin/sh\necho \"$@\" >> "+log+"\n"), 0o755)
	s := spec(t)
	s.NotEvid = false
	s.Chown = func(string, int, int) error { return nil }
	var order []string
	r := &Runner{Docker: fake, Stage: func(Spec) (func(), error) {
		order = append(order, "stage")
		b, _ := os.ReadFile(log)
		if len(b) != 0 {
			t.Error("docker was called before the staging")
		}
		return func() { order = append(order, "undo") }, nil
	}}
	if code, err := r.Run(s); err != nil || code != 0 {
		t.Fatalf("%d %v", code, err)
	}
	b, _ := os.ReadFile(log)
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.Contains(l, "--cap-add") || strings.Contains(l, "--user 0:0") || strings.Contains(l, "sweep") {
			t.Errorf("a sweep container was started: %s", l)
		}
	}
	if strings.Join(order, ",") != "stage,undo" {
		t.Errorf("order %v", order)
	}
	// a staging failure stops the run before docker is touched
	s2 := spec(t)
	s2.NotEvid = false
	s2.Chown = func(string, int, int) error { return nil }
	os.Remove(log)
	r2 := &Runner{Docker: fake, Stage: func(Spec) (func(), error) { return nil, os.ErrPermission }}
	if _, err := r2.Run(s2); err == nil {
		t.Error("a failed staging did not stop the run")
	}
	if _, err := os.Stat(log); err == nil {
		t.Error("docker ran after the staging failed")
	}
}

// M2: the helper-mode run moves steering files out of the home by itself (in Go), before the
// container starts, with no sweep container.
func TestHelperRunSweepsTheHomeInGo(t *testing.T) {
	d := vt(t)
	fake := filepath.Join(d, "docker")
	os.WriteFile(fake, []byte("#!/bin/sh\nexit 0\n"), 0o755)
	s := spec(t)
	s.NotEvid = false
	s.Chown = func(string, int, int) error { return nil }
	os.MkdirAll(s.HomeDir(), 0o700)
	os.WriteFile(filepath.Join(s.HomeDir(), ".bashrc"), []byte("evil"), 0o644)
	if _, err := (&Runner{Docker: fake}).Run(s); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(s.QuarantineDir(), ".bashrc")); err != nil {
		t.Errorf("the steering file was not quarantined by the run: %v", err)
	}
	if _, err := os.Lstat(filepath.Join(s.HomeDir(), ".bashrc")); err == nil {
		t.Error("the steering file is still in the home")
	}
}

// S2 host layer: a fence that left a setuid or setgid file in the worktree has the bits cleared,
// by walking with directory fds, without following links and without touching the owner's own files.
func TestStripSetidClearsPlantedBits(t *testing.T) {
	d := vt(t)
	os.MkdirAll(filepath.Join(d, "a", "b"), 0o755)
	planted := []string{"x", "a/y", "a/b/z"}
	for _, p := range planted {
		os.WriteFile(filepath.Join(d, p), []byte("#!/bin/sh\n"), 0o755)
		os.Chmod(filepath.Join(d, p), os.ModeSetuid|os.ModeSetgid|0o755)
	}
	os.WriteFile(filepath.Join(d, "mine"), nil, 0o755)
	os.Chmod(filepath.Join(d, "mine"), os.ModeSetuid|0o755)
	outside := filepath.Join(vt(t), "target")
	os.WriteFile(outside, nil, 0o755)
	os.Chmod(outside, os.ModeSetuid|0o755)
	os.Symlink(outside, filepath.Join(d, "link"))
	os.Symlink(filepath.Dir(outside), filepath.Join(d, "dirlink"))
	os.MkdirAll(filepath.Join(d, "sgdir"), 0o755)
	os.Chmod(filepath.Join(d, "sgdir"), os.ModeSetgid|0o755)
	root, err := os.Open(d)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	// "owner" is someone else, so every file of this test user counts as planted by the fence...
	var logged []string
	n, err := stripSetid(root, os.Getuid()+1, os.Getuid(), func(f string, a ...any) { logged = append(logged, fmt.Sprintf(f, a...)) })
	if err != nil || n != 4 { // x, a/y, a/b/z and mine
		t.Fatalf("cleared %d: %v", n, err)
	}
	for _, p := range append(planted, "mine") {
		if st, _ := os.Stat(filepath.Join(d, p)); st.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
			t.Errorf("%s still has a setid bit", p)
		}
	}
	if len(logged) != 4 || !strings.Contains(logged[0], "cleared setuid/setgid") {
		t.Errorf("not every change was logged: %q", logged)
	}
	if st, _ := os.Stat(outside); st.Mode()&os.ModeSetuid == 0 {
		t.Error("the sweep followed a symlink out of the worktree")
	}
	if st, _ := os.Stat(filepath.Join(d, "sgdir")); st.Mode()&os.ModeSetgid == 0 {
		t.Error("a directory's setgid bit was cleared (only regular files are)")
	}
	if st, _ := os.Stat(filepath.Join(d, "x")); st.Mode().Perm() != 0o755 {
		t.Errorf("the other mode bits changed: %v", st.Mode())
	}
}

// The owner's own files are left alone: a setuid binary the owner built is theirs to keep.
func TestStripSetidLeavesTheOwnersFiles(t *testing.T) {
	d := vt(t)
	os.WriteFile(filepath.Join(d, "mine"), nil, 0o755)
	os.Chmod(filepath.Join(d, "mine"), os.ModeSetuid|0o755)
	root, _ := os.Open(d)
	defer root.Close()
	if n, err := stripSetid(root, os.Getuid(), os.Getuid(), nil); err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if st, _ := os.Stat(filepath.Join(d, "mine")); st.Mode()&os.ModeSetuid == 0 {
		t.Error("the owner's own setuid file was changed")
	}
}

// Run's seccomp flag is present only when a profile is given.
func TestRunArgsSeccomp(t *testing.T) {
	s := spec(t)
	if has(s.RunArgs(), "--security-opt", "seccomp=/etc/ccsbx/seccomp.json") {
		t.Error("a seccomp flag without a profile")
	}
	s.Seccomp = "/etc/ccsbx/seccomp.json"
	if !has(s.RunArgs(), "--security-opt", "seccomp=/etc/ccsbx/seccomp.json") {
		t.Errorf("%v", s.RunArgs())
	}
}

// R3: only what the fence owns is touched. A hardlink in the worktree to a setuid file of someone
// else (with fs.protected_hardlinks=0, /usr/bin/sudo is one) is the same inode: clearing its bit
// would clear it for the whole system.
func TestStripSetidLeavesHardlinkedFilesOfOthers(t *testing.T) {
	d, elsewhere := vt(t), vt(t)
	target := filepath.Join(elsewhere, "sudo")
	os.WriteFile(target, nil, 0o755)
	os.Chmod(target, os.ModeSetuid|0o755)
	if err := os.Link(target, filepath.Join(d, "innocent-name")); err != nil {
		t.Skipf("no hardlinks here: %v", err)
	}
	root, _ := os.Open(d)
	defer root.Close()
	// the fence is "someone else" than the owner of this file, as uid 10001 is not root
	n, err := stripSetid(root, os.Getuid()+1, os.Getuid()+2, nil)
	if err != nil || n != 0 {
		t.Fatalf("%d %v", n, err)
	}
	if st, _ := os.Stat(target); st.Mode()&os.ModeSetuid == 0 {
		t.Error("a hardlinked setuid file of another owner lost its bit")
	}
	if FenceUID != 10001 {
		t.Error("the production strip must be the fence's uid")
	}
}

// context: the installer passes the same list (~/git,~/wt) to hostd and to ccsbx-root's allowed_roots.
// A workdir inside ANY entry is accepted, one inside none is refused, and a bad entry stops every
// start, so hostd never accepts a workdir ccsbx-root would refuse or the other way round.
func TestCheckWorkdirTakesACommaSeparatedListOfRoots(t *testing.T) {
	a, b := vt(t), vt(t)
	state := filepath.Join(a, "state")
	os.MkdirAll(state, 0o755)
	inA := filepath.Join(a, "proj")
	inB := filepath.Join(b, "other")
	os.MkdirAll(inA, 0o755)
	os.MkdirAll(inB, 0o755)
	list := a + "," + b
	for _, w := range []string{inA, inB} {
		if got, err := CheckWorkdir(w, list, state); err != nil || got != w {
			t.Errorf("%s in the list was refused: %v", w, err)
		}
	}
	if _, err := CheckWorkdir(t.TempDir(), list, state); err == nil {
		t.Error("a workdir inside none of the roots was accepted")
	}
	if _, err := CheckWorkdir(a, list, state); err == nil {
		t.Error("a root itself is not strictly inside it")
	}
	for name, bad := range map[string]string{
		"an empty entry":   a + ",",
		"a relative entry": a + ",rel/dir",
		"/ among others":   a + ",/",
		"an unclean entry": a + "," + b + "/",
		"a missing entry":  a + "," + b + "/nope",
		"a space":          a + ", " + b,
	} {
		if _, err := CheckWorkdir(inA, bad, state); err == nil {
			t.Errorf("%s: accepted (%q)", name, bad)
		}
	}
}
