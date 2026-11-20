// Package seccomp adds one rule to the fence's seccomp profile: chmod, fchmod, fchmodat and
// fchmodat2 fail with EPERM when the new mode has the setuid or setgid bit. It exists because the
// fence writes the worktree as uid 10001; a setuid-10001 file left there would let anyone who runs
// it on the host become uid 10001, whose outboxes and homes every fence shares. The fence must
// not be able to set those bits at all.
//
// Docker takes one profile or the built-in default, so the rule cannot be added alone: Overlay
// adds it to a copy of Docker's own default profile (supplied by the operator, version-matched to
// the installed docker), and Verify is what ccsbx-root runs on the installed file before it starts
// a fence, so a profile that lacks the rule, or that allows everything by default, is refused.
package seccomp

import (
	"encoding/json"
	"fmt"
)

// Setid bits.
const (
	sISUID = 0o4000
	sISGID = 0o2000
)

// calls are the syscalls that take a mode, and which argument holds it.
var calls = []struct {
	name string
	arg  int
}{{"chmod", 1}, {"fchmod", 1}, {"fchmodat", 2}, {"fchmodat2", 2}}

// rules are the added entries: for each call and each bit, EPERM when (mode & bit) == bit.
func rules() []map[string]any {
	var out []map[string]any
	for _, c := range calls {
		for _, bit := range []int{sISUID, sISGID} {
			out = append(out, map[string]any{
				"names":    []string{c.name},
				"action":   "SCMP_ACT_ERRNO",
				"errnoRet": 1, // EPERM
				"args":     []map[string]any{{"index": c.arg, "value": bit, "valueTwo": bit, "op": "SCMP_CMP_MASKED_EQ"}},
				"comment":  "ccsbx: the fence may not set setuid or setgid bits",
			})
		}
	}
	return out
}

// Overlay returns base (a Docker seccomp profile) with the setid rules appended. It refuses a base
// that already allows everything by default.
func Overlay(base []byte) ([]byte, error) {
	var p map[string]any
	if err := json.Unmarshal(base, &p); err != nil {
		return nil, fmt.Errorf("seccomp base profile: %w", err)
	}
	if err := defaultDenies(p); err != nil {
		return nil, err
	}
	sys, _ := p["syscalls"].([]any)
	for _, r := range rules() {
		sys = append(sys, r)
	}
	p["syscalls"] = sys
	return json.MarshalIndent(p, "", "  ")
}

func defaultDenies(p map[string]any) error {
	switch a, _ := p["defaultAction"].(string); a {
	case "SCMP_ACT_ERRNO", "SCMP_ACT_KILL", "SCMP_ACT_KILL_PROCESS", "SCMP_ACT_KILL_THREAD":
		return nil
	default:
		return fmt.Errorf("seccomp profile: defaultAction %q allows syscalls by default", a)
	}
}

// Verify checks an installed profile: it denies by default and carries all eight setid rules.
func Verify(profile []byte) error {
	var p struct {
		DefaultAction string `json:"defaultAction"`
		Syscalls      []struct {
			Names    []string `json:"names"`
			Action   string   `json:"action"`
			ErrnoRet *int     `json:"errnoRet"`
			Args     []struct {
				Index    int    `json:"index"`
				Value    int    `json:"value"`
				ValueTwo int    `json:"valueTwo"`
				Op       string `json:"op"`
			} `json:"args"`
		} `json:"syscalls"`
	}
	if err := json.Unmarshal(profile, &p); err != nil {
		return fmt.Errorf("seccomp profile: %w", err)
	}
	if err := defaultDenies(map[string]any{"defaultAction": p.DefaultAction}); err != nil {
		return err
	}
	have := map[string]bool{}
	for _, s := range p.Syscalls {
		if s.Action != "SCMP_ACT_ERRNO" || s.ErrnoRet == nil || *s.ErrnoRet != 1 || len(s.Args) != 1 {
			continue
		}
		a := s.Args[0]
		if a.Op != "SCMP_CMP_MASKED_EQ" || a.Value != a.ValueTwo {
			continue
		}
		for _, n := range s.Names {
			have[fmt.Sprintf("%s/%d/%d", n, a.Index, a.Value)] = true
		}
	}
	for _, c := range calls {
		for _, bit := range []int{sISUID, sISGID} {
			if k := fmt.Sprintf("%s/%d/%d", c.name, c.arg, bit); !have[k] {
				return fmt.Errorf("seccomp profile lacks the rule that refuses %s with mode bit %#o", c.name, bit)
			}
		}
	}
	return nil
}
