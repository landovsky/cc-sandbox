package side

import (
	"bytes"
	"context"
	"io"
	"strings"
	"testing"

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
	if err := LoadInto(context.Background(), f, k8s.Ref{}, sec); err != nil {
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
	err := LoadInto(context.Background(), f, k8s.Ref{}, []byte(`{"token":"T","key":"K"}`))
	if err == nil || !strings.Contains(err.Error(), "already loaded") {
		t.Fatalf("got %v", err)
	}
	if LoadInto(context.Background(), f, k8s.Ref{}, nil) == nil {
		t.Fatal("empty secrets accepted")
	}
}
