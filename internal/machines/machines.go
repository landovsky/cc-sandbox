// machines.go reads the trusted-key list (plan M9): one JSON file per machine in config/machines/,
// shipped to the cluster as the ConfigMap `ccsbx-machines`. It exists so every door, hostd and the
// hub answer one question the same way: which public key may sign a session token for which owner,
// machine, profile and billing. It turns the files into the token package's Keyring, so the doors
// run the very check the signer ran (token.KeyScope.Permits), pairs included (invariant 27, 18).
//
// File config/machines/<machine>.json (strict: an unknown field is an error, because a typo'd limit
// would otherwise read as "no limit"):
//
//	{"machine": "hp-ubuntu",                 must equal the file name without .json
//	 "owners": ["tomas"],                    sub owners this machine's keys may sign for
//	 "public_keys": ["<base64 ed25519>"],    one, or two while a key is rolled
//	 "may_sign_machines": ["hp-ubuntu", "future-two"],   sub machines (hp signs for future-two)
//	 "max_profile": ["dev-*", "e2e-*"],      profile name globs, the ceiling
//	 "max_billing": ["api", "stub"],         billings, the ceiling
//	 "pairs": {"dev-default": ["api", "stub"]}}   the exact profile→billings the signer enforces
//
// max_profile and max_billing are ceilings; pairs is the rule. Every pair must sit under both
// ceilings, so a file can't say one thing and mean another. Rotation = add a second public key, roll
// the signer, remove the old one. Nothing here talks to a cluster: Render prints the ConfigMap.
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

	"github.com/landovsky/cc-sandbox/internal/token"
)

const (
	maxFile  = 64 << 10 // one machine file
	maxFiles = 64
)

// Entry is one machine file.
type Entry struct {
	Machine         string              `json:"machine"`
	Owners          []string            `json:"owners"`
	PublicKeys      []string            `json:"public_keys"`
	MaySignMachines []string            `json:"may_sign_machines"`
	MaxProfile      []string            `json:"max_profile"`
	MaxBilling      []string            `json:"max_billing"`
	Pairs           map[string][]string `json:"pairs"`
}

// Parse reads one file's bytes. file is its name (hp-ubuntu.json), checked against "machine".
func Parse(file string, b []byte) (*Entry, error) {
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
	if len(e.PublicKeys) == 0 {
		return errors.New("public_keys is empty")
	}
	for _, k := range e.PublicKeys {
		if _, err := pubOf(k); err != nil {
			return err
		}
	}
	if len(e.MaxProfile) == 0 || len(e.MaxBilling) == 0 {
		return errors.New("max_profile and max_billing are required (\"*\" in max_profile means any profile)")
	}
	for _, g := range e.MaxProfile {
		if g == "" || strings.ContainsAny(g, " ,=\t") {
			return fmt.Errorf("max_profile: bad pattern %q", g)
		}
		if _, err := path.Match(g, ""); err != nil {
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

// files lists the machine files of dir, sorted. A ConfigMap volume is a directory of symlinks plus
// "..data" bookkeeping; only plain names ending in .json count, and ReadFile follows the symlinks.
func files(dir string) ([]string, error) {
	des, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var out []string
	for _, d := range des {
		if n := d.Name(); strings.HasSuffix(n, ".json") && !strings.HasPrefix(n, ".") {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	if len(out) > maxFiles {
		return nil, fmt.Errorf("machines: %s has %d machine files; the cap is %d", dir, len(out), maxFiles)
	}
	return out, nil
}

// LoadDir reads every machine file in dir. Any bad file fails the whole load: a half-read list would
// silently drop a machine's key, and a door must not run on a list nobody wrote.
func LoadDir(dir string) ([]*Entry, error) {
	names, err := files(dir)
	if err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return nil, fmt.Errorf("machines: no *.json files in %s", dir)
	}
	var out []*Entry
	for _, n := range names {
		b, err := readCapped(filepath.Join(dir, n))
		if err != nil {
			return nil, fmt.Errorf("machines: %s: %w", n, err)
		}
		e, err := Parse(n, b)
		if err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, nil
}

func readCapped(p string) ([]byte, error) {
	f, err := os.Open(p)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, maxFile+1)) // one byte over the cap makes Parse refuse it
}

// Ring turns entries into the keyring the doors verify with. A kid in two files is refused: two
// machines must not share a key, or "which machine signed" has no answer.
func Ring(es []*Entry) (token.Keyring, error) {
	ring := token.Keyring{}
	owner := map[string]string{}
	for _, e := range es {
		for _, k := range e.PublicKeys {
			pub, _ := pubOf(k)
			kid := token.KIDOf(pub)
			if o, dup := owner[kid]; dup {
				return nil, fmt.Errorf("machines: key %s is listed by both %s and %s", kid, o, e.Machine)
			}
			owner[kid] = e.Machine
			pairs := map[string][]string{}
			for p, bs := range e.Pairs {
				pairs[p] = append([]string(nil), bs...)
			}
			ring[kid] = token.KeyScope{Pub: pub, Owners: e.Owners, Machines: e.MaySignMachines,
				Profiles: e.MaxProfile, Billings: e.MaxBilling, Pairs: pairs}
		}
	}
	return ring, nil
}

// Lines writes the ring as keyring file lines (the older format the stub and `ccsbx token verify`
// read), one per key, sorted by machine then kid.
func Lines(es []*Entry) ([]string, error) {
	if _, err := Ring(es); err != nil {
		return nil, err
	}
	var out []string
	for _, e := range es {
		for _, k := range e.PublicKeys {
			pub, _ := pubOf(k)
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

// canonical is the entry as indented JSON with a fixed field order (the struct's) and sorted pairs.
func canonical(e *Entry) string {
	b, _ := json.MarshalIndent(e, "", "  ")
	return string(b) + "\n"
}
