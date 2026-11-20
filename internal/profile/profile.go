// Package profile is the approved shape of a fenced session: which machines may run it, which
// billing modes it may use, its resource caps, and the tool rules it adds to the fixed base deny
// list. U2 (start) checks a LaunchSpec against it; U17 (settings) compiles it.
//
// A profile can only ADD denials. The base deny list is compiled in (internal/settings) and no
// profile field can remove an entry from it.
package profile

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
)

// Profile is one profiles/<name>.json.
type Profile struct {
	Name     string   `json:"name"`
	Machines []string `json:"machines"`  // machines this profile is approved for (invariant 15's "approved for this machine")
	Billing  []string `json:"billing"`   // api | subscription | stub
	MemoryMB int      `json:"memory_mb"` // container memory cap
	Pids     int      `json:"pids"`      // container pids cap
	Deny     []string `json:"deny"`      // extra Claude Code permission rules to deny
	Mode     string   `json:"mode"`      // Claude Code default mode inside the fence
	E2E      bool     `json:"e2e"`       // an e2e-* profile: may use the stub's local mode (C18)
	// Grants are the grant names a REMOTE start may ask for with this profile on this machine (M2c): the
	// request names grants, the profile decides which are possible. None listed = a remote start asks for none.
	Grants []string `json:"grants,omitempty"`
	// HarnessArgs are the flags a remote start may pass the harness (e.g. "--model"); a flag not listed, or a
	// bare word that is not the value of one, is refused. None listed = no harness_args from a remote caller.
	HarnessArgs []string `json:"harness_args,omitempty"`
}

var grantRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
var flagRe = regexp.MustCompile(`^--[a-z][a-z0-9-]{0,40}$`)

// GrantAllowed reports whether a remote start may request this grant name with this profile.
func (p *Profile) GrantAllowed(g string) bool {
	for _, x := range p.Grants {
		if x == g {
			return true
		}
	}
	return false
}

// HarnessArgAllowed reports whether a remote start may pass this flag to the harness.
func (p *Profile) HarnessArgAllowed(f string) bool {
	for _, x := range p.HarnessArgs {
		if x == f {
			return true
		}
	}
	return false
}

var nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,40}$`)

// ValidName reports whether name is a legal profile name (it is checked before any lookup).
func ValidName(name string) bool { return nameRe.MatchString(name) }

// Validate returns the first defect, or nil.
func (p *Profile) Validate() error {
	if !nameRe.MatchString(p.Name) {
		return fmt.Errorf("profile name %q must match %s", p.Name, nameRe)
	}
	if len(p.Machines) == 0 {
		return fmt.Errorf("profile %s: machines is empty, so it is approved nowhere", p.Name)
	}
	if len(p.Billing) == 0 {
		return fmt.Errorf("profile %s: billing is empty", p.Name)
	}
	for _, b := range p.Billing {
		if b != "api" && b != "subscription" && b != "stub" {
			return fmt.Errorf("profile %s: unknown billing %q", p.Name, b)
		}
	}
	if p.MemoryMB < 256 || p.MemoryMB > 4096*8 {
		return fmt.Errorf("profile %s: memory_mb %d out of range 256..32768", p.Name, p.MemoryMB)
	}
	if p.Pids < 16 || p.Pids > 4096 {
		return fmt.Errorf("profile %s: pids %d out of range 16..4096", p.Name, p.Pids)
	}
	switch p.Mode {
	case "bypassPermissions", "acceptEdits", "default", "plan":
	default:
		return fmt.Errorf("profile %s: unknown mode %q", p.Name, p.Mode)
	}
	for _, g := range p.Grants {
		if !grantRe.MatchString(g) {
			return fmt.Errorf("profile %s: grant name %q is not valid", p.Name, g)
		}
	}
	for _, f := range p.HarnessArgs {
		if !flagRe.MatchString(f) {
			return fmt.Errorf("profile %s: harness_args entry %q is not a --flag", p.Name, f)
		}
	}
	if p.E2E != (len(p.Name) >= 4 && p.Name[:4] == "e2e-") {
		return fmt.Errorf("profile %s: e2e=%v must match the e2e- name prefix", p.Name, p.E2E)
	}
	return nil
}

// Allows reports whether the profile is approved for machine and billing mode.
func (p *Profile) Allows(machine, billing string) (machineOK, billingOK bool) {
	for _, m := range p.Machines {
		if m == machine {
			machineOK = true
		}
	}
	for _, b := range p.Billing {
		if b == billing {
			billingOK = true
		}
	}
	return
}

// Parse decodes and validates profile JSON. Unknown fields are errors.
func Parse(b []byte) (*Profile, error) {
	var p Profile
	dec := json.NewDecoder(bytesReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return nil, err
	}
	return &p, p.Validate()
}

// Load reads <dir>/<name>.json. The name is checked before it touches the filesystem so a label
// like "../x" can't select a file outside dir.
func Load(dir, name string) (*Profile, error) {
	if !nameRe.MatchString(name) {
		return nil, fmt.Errorf("profile name %q is not valid", name)
	}
	b, err := os.ReadFile(filepath.Join(dir, name+".json"))
	if err != nil {
		return nil, err
	}
	p, err := Parse(b)
	if err != nil {
		return nil, err
	}
	if p.Name != name {
		return nil, fmt.Errorf("profile file %s.json declares name %q", name, p.Name)
	}
	return p, nil
}
