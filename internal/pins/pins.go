// Package pins is H2 in user space: the production pins, the candidate channel, and the promote
// gate. Invariant 26: the harness version and image are pinned and never auto-updated.
//
// Enforcement tonight is in `ccsbx start`. After gate M `ccsbx-root` and the cluster's VAP
// enforce the same rules, so a user-space check is a convenience, not the control.
//
//	production  a digest listed in pins.toml. Any profile may run it.
//	candidate   any digest of our repos not in pins.toml. Only e2e-* profiles may run it.
//	dev         a local tag (no digest). Direct mode only; results are not evidence.
//	otherwise   refused as unpinned.
package pins

import (
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"

	"github.com/landovsky/cc-sandbox/internal/profile"
	"github.com/landovsky/cc-sandbox/internal/tomlmini"
)

// Channel names.
const (
	Production = "production"
	Candidate  = "candidate"
	Dev        = "dev"
)

// DefaultRepos are the image repositories a candidate may come from.
var DefaultRepos = []string{"ghcr.io/landovsky/ccsbx-fence"}

var digestRe = regexp.MustCompile(`^([a-z0-9][a-z0-9./_-]*)@sha256:([0-9a-f]{64})$`)

// Pin is one production entry.
type Pin struct {
	Ref string // repo@sha256:...
	CC  string // Claude Code version inside the image
}

// Load reads pins.toml. A missing file means no production pins (everything pinned is refused).
func Load(path string) ([]Pin, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	doc, err := tomlmini.Parse(string(b))
	if err != nil {
		return nil, err
	}
	var out []Pin
	for i, row := range doc["pin"] {
		ref, _ := row["ref"].(string)
		cc, _ := row["cc"].(string)
		if !digestRe.MatchString(ref) {
			return nil, fmt.Errorf("pin #%d: ref %q is not repo@sha256:<64 hex>", i+1, ref)
		}
		for k := range row {
			if k != "ref" && k != "cc" && k != "promoted" && k != "evidence" {
				return nil, fmt.Errorf("pin #%d: unknown key %q", i+1, k)
			}
		}
		out = append(out, Pin{Ref: ref, CC: cc})
	}
	return out, nil
}

// Check decides whether ref may run under profile p. directMode allows local dev tags.
// It returns the channel on success.
func Check(pins []Pin, repos []string, ref string, p *profile.Profile, directMode bool) (string, error) {
	if repos == nil {
		repos = DefaultRepos
	}
	m := digestRe.FindStringSubmatch(ref)
	if m == nil {
		if strings.Contains(ref, "@") {
			return "", fmt.Errorf("image %q: malformed digest", ref)
		}
		if directMode {
			return Dev, nil
		}
		return "", fmt.Errorf("image %q is a tag, not a digest: unpinned production images are refused", ref)
	}
	for _, pin := range pins {
		if pin.Ref == ref {
			return Production, nil
		}
	}
	if !contains(repos, m[1]) {
		return "", fmt.Errorf("image %s is not from our repos (%s) and is not pinned", ref, strings.Join(repos, ", "))
	}
	if p == nil || !p.E2E {
		return "", fmt.Errorf("image %s is a candidate digest (not in pins.toml): only e2e-* profiles may run it", ref)
	}
	return Candidate, nil
}

// Result is a suite result file (`{"suite":"full","hosts":{"hp-ubuntu":"green"}}`).
type Result struct {
	Suite string            `json:"suite"`
	Ref   string            `json:"ref"`
	Hosts map[string]string `json:"hosts"`
}

// Promote appends ref to pins.toml if the full suite is green on every required host and was run
// for this very ref. The file is only ever appended to, so git shows each promotion as one hunk.
func Promote(path, ref, cc string, res Result, required []string) error {
	if !digestRe.MatchString(ref) {
		return fmt.Errorf("promote: %q is not a digest ref", ref)
	}
	if res.Suite != "full" {
		return fmt.Errorf("promote: result is for suite %q, not the full suite", res.Suite)
	}
	if res.Ref != ref {
		return fmt.Errorf("promote: result is for %q, not %q", res.Ref, ref)
	}
	var bad []string
	for _, h := range required {
		if res.Hosts[h] != "green" {
			bad = append(bad, fmt.Sprintf("%s=%q", h, res.Hosts[h]))
		}
	}
	if len(required) == 0 {
		return fmt.Errorf("promote: no required hosts given")
	}
	if len(bad) > 0 {
		sort.Strings(bad)
		return fmt.Errorf("promote refused, not green on: %s", strings.Join(bad, ", "))
	}
	cur, err := Load(path)
	if err != nil {
		return err
	}
	for _, p := range cur {
		if p.Ref == ref {
			return fmt.Errorf("promote: %s is already pinned", ref)
		}
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = fmt.Fprintf(f, "\n[[pin]]\nref = %q\ncc = %q\npromoted = %q\n", ref, cc, strings.Join(required, ","))
	return err
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}
