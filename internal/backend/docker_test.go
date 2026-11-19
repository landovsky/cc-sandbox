package backend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func spec(t *testing.T) Spec {
	return Spec{Sid: "sabc123", Label: "cc-sandbox:x", Gen: 1, Image: "img", StateDir: t.TempDir(),
		Workdir: "/work/wt", MemoryMB: 4096, Pids: 512}
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

func TestGitDirFor(t *testing.T) {
	d := t.TempDir()
	if GitDirFor(d) != "" {
		t.Fatal("no .git should give empty")
	}
	os.Mkdir(filepath.Join(d, ".git"), 0o755)
	if GitDirFor(d) != filepath.Join(d, ".git") {
		t.Fatal(".git dir")
	}
	w := t.TempDir()
	os.WriteFile(filepath.Join(w, ".git"), []byte("gitdir: /repo/.git/worktrees/w\n"), 0o644)
	if GitDirFor(w) != "/repo/.git/worktrees/w" {
		t.Fatal(GitDirFor(w))
	}
}

func TestCheckWorkdir(t *testing.T) {
	for _, bad := range []string{"/", "/tmp", "/tmp/x", "/run/ccsbx", "/etc", "/home", "relative", "/a/../b", "/a/"} {
		if CheckWorkdir(bad) == nil {
			t.Errorf("%q accepted", bad)
		}
	}
	for _, ok := range []string{"/var/tmp", "/home/tomas/git/x", "/work/wt"} {
		if err := CheckWorkdir(ok); err != nil {
			t.Errorf("%q: %v", ok, err)
		}
	}
}
