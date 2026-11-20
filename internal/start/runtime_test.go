// Tests for the two ways the stopped proof asks "is this container running?": docker by exact name
// (direct mode) and the read-only `ccsbx-root status` (helper mode). Neither may turn a failure to
// ask into "not running".
package start

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func script(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "x")
	os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	return p
}

func TestDockerRuntimeAsksByTheExactName(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	d := script(t, `printf "%s\n" "$*" >> `+log+`; echo true`)
	if ok, err := (DockerRuntime{Docker: d}).Running("s0abcdefgh", 3); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if b, _ := os.ReadFile(log); string(b) != "inspect --type container --format {{.State.Running}} ccsbx-s0abcdefgh-g3\n" {
		t.Errorf("docker was asked: %q", b)
	}
	if ok, err := (DockerRuntime{Docker: script(t, `echo false`)}).Running("s0abcdefgh", 1); err != nil || ok {
		t.Errorf("false: %v %v", ok, err)
	}
	if ok, err := (DockerRuntime{Docker: script(t, `echo "Error: No such object: ccsbx-s0abcdefgh-g1"; exit 1`)}).Running("s0abcdefgh", 1); err != nil || ok {
		t.Errorf("a missing container is not running: %v %v", ok, err)
	}
	for _, bad := range []string{`echo "Cannot connect to the Docker daemon"; exit 1`, `echo maybe`, `exit 3`} {
		if ok, err := (DockerRuntime{Docker: script(t, bad)}).Running("s0abcdefgh", 1); err == nil || ok {
			t.Errorf("%q: a failure to ask was taken as an answer: %v %v", bad, ok, err)
		}
	}
}

func TestHelperRuntimeAsksRootsStatusVerb(t *testing.T) {
	log := filepath.Join(t.TempDir(), "log")
	oldS, oldR := SudoBin, RootBin
	defer func() { SudoBin, RootBin = oldS, oldR }()
	SudoBin = script(t, `printf "%s\n" "$*" >> `+log+`; echo "$ANSWER_DOES_NOT_COME_FROM_ENV" >/dev/null; cat `+filepath.Join(filepath.Dir(log), "answer")+`; exit $(cat `+filepath.Join(filepath.Dir(log), "code")+`)`)
	RootBin = "/usr/local/sbin/ccsbx-root"
	set := func(answer, code string) {
		os.WriteFile(filepath.Join(filepath.Dir(log), "answer"), []byte(answer), 0o644)
		os.WriteFile(filepath.Join(filepath.Dir(log), "code"), []byte(code), 0o644)
	}
	set("running\n", "0")
	if ok, err := (HelperRuntime{}).Running("s0abcdefgh", 2); err != nil || !ok {
		t.Fatalf("%v %v", ok, err)
	}
	if b, _ := os.ReadFile(log); string(b) != "-n /usr/local/sbin/ccsbx-root status -- s0abcdefgh\n" {
		t.Errorf("sudo was called as %q", b)
	}
	set("stopped\n", "0")
	if ok, err := (HelperRuntime{}).Running("s0abcdefgh", 2); err != nil || ok {
		t.Errorf("stopped: %v %v", ok, err)
	}
	for _, c := range [][2]string{{"", "1"}, {"running\n", "2"}, {"banana\n", "0"}, {"", "0"}} {
		set(c[0], c[1])
		if ok, err := (HelperRuntime{}).Running("s0abcdefgh", 2); err == nil || ok {
			t.Errorf("answer %q exit %s was taken as an answer: %v %v", c[0], c[1], ok, err)
		}
	}
	if _, err := (HelperRuntime{}).Running("../x", 1); err == nil {
		t.Error("a bad sid was sent to root")
	}
}

// Docker answers for the generation AND the one before it (one question from the proof).
func TestDockerRuntimeCoversThePreviousGeneration(t *testing.T) {
	d := script(t, `case "$*" in *-g3) echo false;; *-g2) echo true;; *) echo "Error: No such object"; exit 1;; esac`)
	if ok, err := (DockerRuntime{Docker: d}).Running("s0abcdefgh", 3); err != nil || !ok {
		t.Errorf("a running previous generation was missed: %v %v", ok, err)
	}
	if ok, err := (DockerRuntime{Docker: d}).Running("s0abcdefgh", 4); err != nil || ok {
		t.Errorf("%v %v", ok, err)
	}
	// a hung docker is cut off: the question has a deadline
	hang := script(t, `sleep 30`)
	start := time.Now()
	if _, err := (DockerRuntime{Docker: hang}).Running("s0abcdefgh", 1); err == nil || time.Since(start) > 15*time.Second {
		t.Errorf("a hung docker was waited for: %v after %s", err, time.Since(start))
	}
}

func TestHelperRuntimeIsCutOffWhenSudoHangs(t *testing.T) {
	oldS := SudoBin
	defer func() { SudoBin = oldS }()
	SudoBin = script(t, `sleep 30`)
	start := time.Now()
	if _, err := (HelperRuntime{}).Running("s0abcdefgh", 1); err == nil || time.Since(start) > 15*time.Second {
		t.Errorf("a hung sudo was waited for: %v after %s", err, time.Since(start))
	}
}
