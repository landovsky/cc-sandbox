package pins

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/landovsky/cc-sandbox/internal/profile"
)

const (
	repo  = "ghcr.io/landovsky/ccsbx-fence"
	d1    = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	d2    = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
	dprod = repo + "@" + d1
	dcand = repo + "@" + d2
)

var (
	dev = &profile.Profile{Name: "dev-default"}
	e2e = &profile.Profile{Name: "e2e-default", E2E: true}
	set = []Pin{{Ref: dprod, CC: "2.1.329"}}
)

// T:unpinned-refused — block twin.
func TestUnpinnedRefusedBlock(t *testing.T) {
	t.Run("a mutable tag is not a pin: it can change under us (invariant 26)", func(t *testing.T) {
		if _, err := Check(set, nil, repo+":latest", dev, false); err == nil {
			t.Fatal("tag accepted outside direct mode")
		}
	})
	t.Run("an unlisted digest is refused under a normal profile", func(t *testing.T) {
		if _, err := Check(set, nil, dcand, dev, false); err == nil || !strings.Contains(err.Error(), "e2e") {
			t.Fatalf("got %v", err)
		}
	})
	t.Run("a digest from someone else's repo is refused even for e2e", func(t *testing.T) {
		if _, err := Check(set, nil, "docker.io/evil/img@"+d2, e2e, false); err == nil {
			t.Fatal("foreign repo accepted")
		}
	})
	t.Run("a malformed digest is refused, not treated as a tag", func(t *testing.T) {
		if _, err := Check(set, nil, repo+"@sha256:abc", e2e, true); err == nil {
			t.Fatal("short digest accepted")
		}
	})
	t.Run("no pins.toml means nothing is production", func(t *testing.T) {
		if _, err := Check(nil, nil, dprod, dev, false); err == nil {
			t.Fatal("accepted with no pins")
		}
	})
}

// T:unpinned-refused — allow twin.
func TestUnpinnedRefusedAllow(t *testing.T) {
	t.Run("a pinned digest runs under any profile", func(t *testing.T) {
		for _, p := range []*profile.Profile{dev, e2e} {
			if ch, err := Check(set, nil, dprod, p, false); err != nil || ch != Production {
				t.Fatalf("%s: %v %v", p.Name, ch, err)
			}
		}
	})
	t.Run("a candidate digest runs under e2e-default and is refused under dev-default", func(t *testing.T) {
		if ch, err := Check(set, nil, dcand, e2e, false); err != nil || ch != Candidate {
			t.Fatalf("e2e: %v %v", ch, err)
		}
		if _, err := Check(set, nil, dcand, dev, false); err == nil {
			t.Fatal("candidate ran under dev-default")
		}
	})
	t.Run("a local dev tag runs in direct mode, marked dev", func(t *testing.T) {
		if ch, err := Check(nil, nil, "ccsbx-fence:dev", dev, true); err != nil || ch != Dev {
			t.Fatalf("%v %v", ch, err)
		}
	})
}

func TestPromote(t *testing.T) {
	green := Result{Suite: "full", Ref: dcand, Hosts: map[string]string{"hp-ubuntu": "green", "future-two": "green"}}
	req := []string{"hp-ubuntu", "future-two"}
	path := func(t *testing.T) string { return filepath.Join(t.TempDir(), "pins.toml") }

	t.Run("promote with a red result is refused and pins.toml stays untouched", func(t *testing.T) {
		p := path(t)
		red := green
		red.Hosts = map[string]string{"hp-ubuntu": "green", "future-two": "red"}
		if err := Promote(p, dcand, "2.1.329", red, req); err == nil || !strings.Contains(err.Error(), "future-two") {
			t.Fatalf("got %v", err)
		}
		if _, err := os.Stat(p); err == nil {
			t.Fatal("file was written")
		}
	})
	t.Run("a missing host counts as not green", func(t *testing.T) {
		g := green
		g.Hosts = map[string]string{"hp-ubuntu": "green"}
		if err := Promote(path(t), dcand, "x", g, req); err == nil {
			t.Fatal("accepted a result without future-two")
		}
	})
	t.Run("a green result for another image can't promote this one", func(t *testing.T) {
		g := green
		g.Ref = dprod
		if err := Promote(path(t), dcand, "x", g, req); err == nil {
			t.Fatal("accepted a result for a different ref")
		}
	})
	t.Run("only the full suite promotes", func(t *testing.T) {
		g := green
		g.Suite = "fence"
		if err := Promote(path(t), dcand, "x", g, req); err == nil {
			t.Fatal("accepted a partial suite")
		}
	})
	t.Run("a green full suite pins the digest, after which any profile may run it", func(t *testing.T) {
		p := path(t)
		if err := Promote(p, dcand, "2.1.330", green, req); err != nil {
			t.Fatal(err)
		}
		pins, err := Load(p)
		if err != nil || len(pins) != 1 || pins[0].CC != "2.1.330" {
			t.Fatalf("%v %v", pins, err)
		}
		if ch, err := Check(pins, nil, dcand, dev, false); err != nil || ch != Production {
			t.Fatalf("%v %v", ch, err)
		}
		if err := Promote(p, dcand, "x", green, req); err == nil {
			t.Fatal("promoted twice")
		}
	})
	t.Run("pins.toml with a tag ref is a load error, so a hand edit can't pin a mutable tag", func(t *testing.T) {
		p := path(t)
		os.WriteFile(p, []byte("[[pin]]\nref = \"ghcr.io/landovsky/ccsbx-fence:latest\"\ncc = \"x\"\n"), 0o644)
		if _, err := Load(p); err == nil {
			t.Fatal("tag pin loaded")
		}
	})
}
