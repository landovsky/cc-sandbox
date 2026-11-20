package backend

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func stubDocker(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "docker")
	os.WriteFile(p, []byte("#!/bin/sh\n"+body+"\n"), 0o755)
	return p
}

// context "the managed settings may wire the client layer only onto an image that has it": the build labels the image with the layer's
// digest, and ImageHasLayer is how a run finds out. An image with no label has no layer; a label that is not a digest, or a docker that
// cannot answer, is an ERROR, never a quiet "no layer" (a run must not lose its reporting because docker failed).
func TestTheLayerIsThereOnlyWhenTheImageSaysSo(t *testing.T) {
	dig := "sha256:" + strings.Repeat("ab", 32)
	for name, c := range map[string]struct {
		out     string
		rc      int
		want    bool
		wantErr bool
	}{
		"labelled":                       {out: dig, want: true},
		"no label (empty)":               {out: ""},
		"no label (docker's <no value>)": {out: "<no value>"},
		"not a digest":                   {out: "latest", wantErr: true},
		"short digest":                   {out: "sha256:abc", wantErr: true},
		"docker fails":                   {rc: 1, wantErr: true},
	} {
		d := stubDocker(t, `[ "$1 $2" = "image inspect" ] || exit 9; echo "`+c.out+`"; exit `+string(rune('0'+c.rc)))
		got, err := ImageHasLayer(d, "ccsbx-fence:x")
		if got != c.want || (err != nil) != c.wantErr {
			t.Errorf("%s: got %v, %v", name, got, err)
		}
	}
}
