// layer.go says whether a fence image carries the client layer (plan C3). It exists because the managed settings wire hooks and an MCP
// server to files under /opt/ccf: wired onto an image without them, python exits 2 on the missing script and Claude Code treats a hook
// exit 2 as BLOCKING, so every prompt of the session would be refused. The candidate build (image/build-ccf.sh) labels the image with the
// layer's digest; no label, no layer, and the session gets no hooks, no statusLine and no ccf server.
package backend

import (
	"context"
	"fmt"
	"os/exec"
	"regexp"
	"strings"
	"time"
)

// LayerLabel is the image label image/build-ccf.sh sets to the layer tar's sha256.
const LayerLabel = "org.ccsbx.ccf.digest"

var layerDigestRe = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

// ImageHasLayer reads the image's labels through the docker binary and reports whether LayerLabel holds a digest. An image that cannot
// be inspected is an error, never "no layer": a run must not quietly lose its reporting because docker failed to answer.
func ImageHasLayer(docker, image string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, docker, "image", "inspect", "-f", `{{ index .Config.Labels "`+LayerLabel+`" }}`, image).Output()
	if err != nil {
		return false, fmt.Errorf("cannot inspect image %s: %w", image, err)
	}
	v := strings.TrimSpace(string(out))
	if v == "" || v == "<no value>" {
		return false, nil
	}
	if !layerDigestRe.MatchString(v) {
		return false, fmt.Errorf("image %s has %s=%q, which is not a sha256 digest", image, LayerLabel, v)
	}
	return true, nil
}
