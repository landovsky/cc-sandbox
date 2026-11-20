// machines.go reads the trusted-key list (plan M9): one JSON file per machine in config/machines/,
// shipped to the cluster as the ConfigMap `ccsbx-machines`. It exists so every door, hostd and the
// hub answer one question the same way: which public key may sign a session token for which owner,
// machine, profile and billing. It turns the files into the token package's Keyring, so the doors
// run the very check the signer ran (token.KeyScope.Permits), pairs included (invariant 27, 18).
//
// File config/machines/<machine>.json, in exactly the canonical form `ccsbx machines fmt` prints
// (the loader refuses any other bytes, so what a reviewer reads is what is loaded and rendered; Go's
// JSON reader would otherwise take a duplicate or wrong-case key silently and let the last one win):
//
//	{"machine": "hp-ubuntu",                 must equal the file name without .json
//	 "owners": ["tomas"],                    sub owners this machine's keys may sign for
//	 "public_keys": [{"kid": "…", "key": "<base64 ed25519 PUBLIC key>"}],   one, or two while rolling
//	 "may_sign_machines": ["hp-ubuntu", "future-two"],   must include this machine; may add a machine
//	                                         only if that machine has no file of its own
//	 "max_profile": ["dev-*", "e2e-*"],      profile name globs, the ceiling
//	 "max_billing": ["api", "stub"],         billings, the ceiling
//	 "pairs": {"dev-default": ["api", "stub"]}}   the exact profile→billings the signer enforces
//
// max_profile and max_billing are ceilings; pairs is the rule. Every pair must sit under both
// ceilings. Each key carries its kid and the kid must equal KIDOf(key): a private seed pasted where
// a public key belongs has the same length, and without this check it would be rendered into a public
// ConfigMap. Rotation = add a second key, roll the signer, remove the old one. Nothing here talks to
// a cluster: Render prints the ConfigMap.
//
// Where it is read from: a ConfigMap volume (a ..data link, kubelet-managed) or a plain directory.
// A plain directory and its files must be owned by root or the running uid and not group/other
// writable, so a hot-reloaded list is not a trust root any other local user can edit.
package machines

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"
	"syscall"

	"github.com/landovsky/cc-sandbox/internal/token"
)

const (
	maxFile  = 64 << 10 // one machine file
	maxFiles = 64
)

// KeyRef is one public key and its id.
type KeyRef struct {
	KID string `json:"kid"`
	Key string `json:"key"`
}

// Entry is one machine file.
type Entry struct {
	Machine         string              `json:"machine"`
	Owners          []string            `json:"owners"`
	PublicKeys      []KeyRef            `json:"public_keys"`
	MaySignMachines []string            `json:"may_sign_machines"`
	MaxProfile      []string            `json:"max_profile"`
	MaxBilling      []string            `json:"max_billing"`
	Pairs           map[string][]string `json:"pairs"`
}

// Parse reads one file's bytes. file is its name (hp-ubuntu.json), checked against "machine". The
// bytes must be the canonical form of what they say; see the header.
func Parse(file string, b []byte) (*Entry, error) {
	e, err := ParseLoose(file, b)
	if err != nil {
		return nil, err
	}
	if string(b) != canonical(e) {
		return nil, fmt.Errorf("machines: %s: not in canonical form (a duplicate or wrong-case key, odd spacing or key order): `ccsbx machines fmt %s` prints what it should say", file, file)
	}
	return e, nil
}

// ParseLoose is Parse without the canonical-form rule: what the JSON reader makes of the bytes (the
// last duplicate wins, case is ignored), validated. `ccsbx machines fmt` uses it to show a reviewer
// what a non-canonical file would have said. The loader never does.
func ParseLoose(file string, b []byte) (*Entry, error) {
	if len(b) > maxFile {
		return nil, fmt.Errorf("machines: %s is over %d bytes", file, maxFile)
	}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	var e Entry
	if err := dec.Decode(&e); err != nil {
		return nil, fmt.Errorf("machines: %s: %w", file, err)
	}
	if dec.More() {
		return nil, fmt.Errorf("machines: %s: data after the JSON object", file)
	}
	if err := e.check(file); err != nil {
		return nil, fmt.Errorf("machines: %s: %w", file, err)
	}
	return &e, nil
}

func (e *Entry) check(file string) error {
	if !token.ValidName(e.Machine) || e.Machine+".json" != file {
		return fmt.Errorf("machine %q must be a plain name equal to the file name without .json", e.Machine)
	}
	for _, f := range []struct {
		what string
		l    []string
	}{{"owners", e.Owners}, {"may_sign_machines", e.MaySignMachines}} {
		if len(f.l) == 0 {
			return fmt.Errorf("%s is empty (an empty list allows nothing; remove the file to remove the machine)", f.what)
		}
		for _, n := range f.l {
			if !token.ValidName(n) {
				return fmt.Errorf("%s: %q is not a valid name (letters, digits . _ -)", f.what, n)
			}
		}
	}
	if !contains(e.MaySignMachines, e.Machine) {
		return fmt.Errorf("may_sign_machines must include %s itself", e.Machine)
	}
	if len(e.PublicKeys) == 0 {
		return errors.New("public_keys is empty")
	}
	for _, k := range e.PublicKeys {
		pub, err := pubOf(k.Key)
		if err != nil {
			return err
		}
		if k.KID != token.KIDOf(pub) {
			return fmt.Errorf("public_keys: kid %q does not match the key (is this a private seed? only a public key may be listed)", k.KID)
		}
	}
	if len(e.MaxProfile) == 0 || len(e.MaxBilling) == 0 {
		return errors.New("max_profile and max_billing are required (\"*\" in max_profile means any profile)")
	}
	for _, g := range e.MaxProfile {
		if !validProfileGlob(g) {
			return fmt.Errorf("max_profile: bad pattern %q", g)
		}
	}
	for _, b := range e.MaxBilling {
		if !token.ValidBilling(b) {
			return fmt.Errorf("max_billing: %q is not api, subscription or stub", b)
		}
	}
	if len(e.Pairs) == 0 {
		return errors.New("pairs is required: the exact profile→billings this machine may sign (a ceiling alone lets a stolen key sign any profile under it)")
	}
	for prof, bs := range e.Pairs {
		if !token.ValidName(prof) {
			return fmt.Errorf("pairs: profile %q is not a plain name", prof)
		}
		if !matchesAny(e.MaxProfile, prof) {
			return fmt.Errorf("pairs: profile %q is outside max_profile", prof)
		}
		if len(bs) == 0 {
			return fmt.Errorf("pairs: profile %q has no billings", prof)
		}
		seen := map[string]bool{}
		for _, b := range bs {
			if !token.ValidBilling(b) || !contains(e.MaxBilling, b) {
				return fmt.Errorf("pairs: %s:%s is not a billing, or is outside max_billing", prof, b)
			}
			if seen[b] {
				return fmt.Errorf("pairs: %s lists %s twice", prof, b)
			}
			seen[b] = true
		}
	}
	return nil
}

// validProfileGlob: a profile name with * ? and [] allowed, nothing that needs escaping anywhere.
func validProfileGlob(g string) bool {
	if g == "" {
		return false
	}
	for _, r := range g {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("-_.*?[]", r)) {
			return false
		}
	}
	_, err := path.Match(g, "")
	return err == nil
}

func pubOf(s string) (ed25519.PublicKey, error) {
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil || len(b) != ed25519.PublicKeySize {
		return nil, errors.New("public_keys: not a base64 ed25519 public key (32 bytes)")
	}
	return ed25519.PublicKey(b), nil
}

func matchesAny(globs []string, s string) bool {
	for _, g := range globs {
		if m, err := path.Match(g, s); err == nil && m {
			return true
		}
	}
	return false
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// readDir resolves the directory to read from once: a ConfigMap volume's ..data link is followed a
// single time so every file of one load comes from the same kubelet snapshot. The second result says
// whether this is such a volume (kubelet-managed, so the ownership check does not apply).
func readDir(dir string) (string, bool, error) {
	if _, err := os.Lstat(filepath.Join(dir, "..data")); err == nil {
		real, err := filepath.EvalSymlinks(filepath.Join(dir, "..data"))
		return real, true, err
	}
	return dir, false, nil
}

// trustedPlace refuses a directory or file another local user could edit.
func trustedPlace(p string, fi os.FileInfo) error {
	if fi.Mode().Perm()&0o022 != 0 {
		return fmt.Errorf("%s is group or other writable (mode %o)", p, fi.Mode().Perm())
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || (st.Uid != 0 && int(st.Uid) != os.Getuid()) {
		return fmt.Errorf("%s is not owned by root or uid %d", p, os.Getuid())
	}
	return nil
}

// LoadDir reads every machine file in dir. Any bad file fails the whole load: a half-read list would
// silently drop a machine's key, and a door must not run on a list nobody wrote.
func LoadDir(dir string) ([]*Entry, error) {
	real, volume, err := readDir(dir)
	if err != nil {
		return nil, err
	}
	if !volume {
		fi, err := os.Stat(real)
		if err != nil {
			return nil, err
		}
		if err := trustedPlace(real, fi); err != nil {
			return nil, fmt.Errorf("machines: %w", err)
		}
	}
	des, err := os.ReadDir(real)
	if err != nil {
		return nil, err
	}
	var names []string
	for _, d := range des {
		if n := d.Name(); strings.HasSuffix(n, ".json") && !strings.HasPrefix(n, ".") {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	if len(names) == 0 {
		return nil, fmt.Errorf("machines: no *.json files in %s", dir)
	}
	if len(names) > maxFiles {
		return nil, fmt.Errorf("machines: %s has %d machine files; the cap is %d", dir, len(names), maxFiles)
	}
	var out []*Entry
	for _, n := range names {
		b, err := readCapped(filepath.Join(real, n), !volume)
		if err != nil {
			return nil, fmt.Errorf("machines: %s: %w", n, err)
		}
		e, err := Parse(n, b)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, checkMachines(out)
}

// checkMachines: a file may name another machine in may_sign_machines only if that machine has no file
// of its own (hp signs for future-two). Otherwise one machine's key could sign for another that has
// its own key, and the per-machine files would draw a boundary nothing enforces.
func checkMachines(es []*Entry) error {
	has := map[string]bool{}
	for _, e := range es {
		has[e.Machine] = true
	}
	for _, e := range es {
		for _, m := range e.MaySignMachines {
			if m != e.Machine && has[m] {
				return fmt.Errorf("machines: %s.json may not sign for %s: %s has its own file", e.Machine, m, m)
			}
		}
	}
	return nil
}

// readCapped opens without blocking (a FIFO named x.json would hang the load forever) and accepts a
// regular file only, judged on the opened handle. strict also applies the ownership rule.
func readCapped(p string, strict bool) ([]byte, error) {
	f, err := os.OpenFile(p, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if !fi.Mode().IsRegular() {
		return nil, errors.New("not a regular file")
	}
	if strict {
		if err := trustedPlace(p, fi); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(io.LimitReader(f, maxFile+1)) // one byte over the cap makes Parse refuse it
}

// Ring turns entries into the keyring the doors verify with. A kid in two files is refused: two
// machines must not share a key, or "which machine signed" has no answer.
func Ring(es []*Entry) (token.Keyring, error) {
	ring := token.Keyring{}
	owner := map[string]string{}
	for _, e := range es {
		for _, k := range e.PublicKeys {
			pub, _ := pubOf(k.Key)
			kid := token.KIDOf(pub)
			if o, dup := owner[kid]; dup {
				return nil, fmt.Errorf("machines: key %s is listed by both %s and %s", kid, o, e.Machine)
			}
			owner[kid] = e.Machine
			pairs := map[string][]string{}
			for p, bs := range e.Pairs {
				pairs[p] = append([]string(nil), bs...)
			}
			cp := func(l []string) []string { return append([]string(nil), l...) }
			ring[kid] = token.KeyScope{Pub: pub, Owners: cp(e.Owners), Machines: cp(e.MaySignMachines),
				Profiles: cp(e.MaxProfile), Billings: cp(e.MaxBilling), Pairs: pairs}
		}
	}
	return ring, nil
}

// Lines writes the ring as keyring file lines (the older format the stub and `ccsbx token verify`
// read), one per key, in file order.
func Lines(es []*Entry) ([]string, error) {
	if _, err := Ring(es); err != nil {
		return nil, err
	}
	var out []string
	for _, e := range es {
		for _, k := range e.PublicKeys {
			pub, _ := pubOf(k.Key)
			key := &token.Key{KID: token.KIDOf(pub), Public: pub}
			var union []string
			for _, b := range []string{"api", "subscription", "stub"} {
				if contains(e.MaxBilling, b) {
					union = append(union, b)
				}
			}
			// The file's own ceilings are written, not values derived from the pairs.
			out = append(out, key.KeyringLineScoped(e.Owners, e.MaySignMachines, e.MaxProfile, union)+" pairs="+token.PairsString(e.Pairs))
		}
	}
	return out, nil
}

// canonical is the entry as indented JSON with a fixed field order (the struct's) and sorted pairs,
// ending in one newline. It is the only form Parse accepts.
func canonical(e *Entry) string {
	b, _ := json.MarshalIndent(e, "", "  ")
	return string(b) + "\n"
}

// Canonical is canonical for callers outside the package (`ccsbx machines fmt`).
func Canonical(e *Entry) string { return canonical(e) }
