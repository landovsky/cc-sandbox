package side

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/landovsky/cc-sandbox/internal/k8s"
)

type fakeX struct {
	cmd   []string
	cont  string
	stdin string
	code  int
	msg   string
}

func (f *fakeX) Exec(_ context.Context, _ k8s.Ref, c string, cmd []string, io_ k8s.IO) (int, error) {
	f.cmd, f.cont = cmd, c
	b, _ := io.ReadAll(io_.Stdin)
	f.stdin = string(b)
	io_.Stderr.Write([]byte(f.msg))
	return f.code, nil
}

// context "the launcher must not leak the secrets into anything `ps` or the API server can see": they go
// to the sidecar container on stdin only, never on the command line.
func TestLoadIntoSendsSecretsOnStdinOnly(t *testing.T) {
	f := &fakeX{}
	sec := []byte(`{"token":"TOKEN-VALUE","key":"KEY-VALUE"}`)
	if err := LoadInto(context.Background(), f, k8s.Ref{UID: "uid-1"}, sec); err != nil {
		t.Fatal(err)
	}
	if f.cont != "side" || f.stdin != string(sec) {
		t.Fatalf("container %q stdin %q", f.cont, f.stdin)
	}
	if j := strings.Join(f.cmd, " "); strings.Contains(j, "TOKEN-VALUE") || strings.Contains(j, "KEY-VALUE") {
		t.Fatalf("secret in argv: %s", j)
	}
	if !bytes.Contains([]byte(strings.Join(f.cmd, " ")), []byte(ControlPath)) {
		t.Fatalf("argv %v lacks the control path", f.cmd)
	}
}

// context "the sidecar refuses (wrong key, expired token, already loaded)": the launcher must see it as an
// error, with the reason, and the reason must not carry the secrets.
func TestLoadIntoReportsARefusal(t *testing.T) {
	f := &fakeX{code: 1, msg: "side load: already loaded"}
	err := LoadInto(context.Background(), f, k8s.Ref{UID: "uid-1"}, []byte(`{"token":"T","key":"K"}`))
	if err == nil || !strings.Contains(err.Error(), "already loaded") {
		t.Fatalf("got %v", err)
	}
	if LoadInto(context.Background(), f, k8s.Ref{UID: "uid-1"}, nil) == nil {
		t.Fatal("empty secrets accepted")
	}
}

// context "Exec checks the live pod UID against the recorded one": a ref with no UID would skip that
// protection, so LoadInto refuses it before anything is sent.
func TestLoadIntoNeedsThePodUID(t *testing.T) {
	f := &fakeX{}
	if err := LoadInto(context.Background(), f, k8s.Ref{}, []byte(`{"token":"T","key":"K"}`)); err == nil || f.stdin != "" {
		t.Fatalf("sent without a UID: %v", err)
	}
}

// context "a launcher bug feeds the wrong file to the load": LoadInto refuses what could never be a
// secrets object before it opens an exec stream.
func TestLoadIntoRefusesOversizedSecrets(t *testing.T) {
	f := &fakeX{}
	if err := LoadInto(context.Background(), f, k8s.Ref{UID: "u"}, bytes.Repeat([]byte("a"), MaxSecrets+1)); err == nil || f.stdin != "" || f.cont != "" {
		t.Fatalf("oversized secrets: %v (exec called: %q)", err, f.cont)
	}
}

// context "the launcher must tell 'not up yet' and 'already loaded' from a refusal by the exit code the
// sidecar's own command returns, not by words in its stderr": text that merely contains those words is
// still a refusal.
func TestLoadIntoClassifiesByExitCode(t *testing.T) {
	for _, c := range []struct {
		code     int
		msg      string
		noSocket bool
		already  bool
	}{
		{ExitNoSocket, "side load: dial unix x: no such file", true, false},
		{ExitAlreadyLoaded, "side load: refused: already loaded", false, true},
		{1, "side load: refused: no such file or directory in the token", false, false},
		{1, "side load: refused: already loaded (so it says)", false, false},
	} {
		f := &fakeX{code: c.code, msg: c.msg}
		err := LoadInto(context.Background(), f, k8s.Ref{UID: "uid-1"}, []byte(`{"token":"T","key":"K"}`))
		if err == nil || errors.Is(err, ErrNoSocket) != c.noSocket || errors.Is(err, ErrAlreadyLoaded) != c.already {
			t.Errorf("exit %d %q: %v", c.code, c.msg, err)
		}
	}
}

// context "the real command's exit codes": nothing listening exits 3; a second load into a loaded
// sidecar exits 4; a bad payload exits 1.
func TestSideLoadExitCodes(t *testing.T) {
	r := newRig(t, nil)
	var out, errb bytes.Buffer
	if c := Main([]string{"load", "--control", r.ctl + ".nope"}, strings.NewReader(`{}`), &out, &errb); c != ExitNoSocket {
		t.Errorf("no socket: exit %d (%s)", c, errb.String())
	}
	_, in := r.secrets(time.Hour)
	if c := Main([]string{"load", "--control", r.ctl}, in, &out, &errb); c != 0 {
		t.Fatalf("first load: exit %d (%s)", c, errb.String())
	}
	_, in2 := r.secrets(time.Hour)
	if c := Main([]string{"load", "--control", r.ctl}, in2, &out, &errb); c != ExitAlreadyLoaded {
		t.Errorf("second load: exit %d", c)
	}
	r2 := newRig(t, nil)
	if c := Main([]string{"load", "--control", r2.ctl}, strings.NewReader(`{"token":"x","key":"y"}`), &out, &errb); c != 1 {
		t.Errorf("a bad payload: exit %d", c)
	}
}
