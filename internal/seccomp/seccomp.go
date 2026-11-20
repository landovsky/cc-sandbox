// Package seccomp makes the fence unable to create or mark a file setuid or setgid. It exists
// because the fence writes the worktree as uid 10001, and a setuid-10001 file left there would let
// anyone who runs it on the host become uid 10001, whose outboxes and homes every fence shares.
//
// The bits can be set by chmod, fchmod, fchmodat and fchmodat2, and also at creation by open,
// openat, creat, mknod and mknodat (open with O_CREAT and mode 06755 makes a setuid file; this was
// checked). openat2 takes its mode in a struct a filter cannot read, so it is refused outright.
//
// How, without relying on the order libseccomp applies overlapping rules: those syscalls are
// REMOVED from every allow entry of Docker's default profile and added back as one conditional
// allow each, "mode & 06000 == 0". A mode with a setid bit then matches no allow and falls through
// to the profile's defaultAction, which must be ERRNO (EPERM). Docker takes a whole profile, so
// Overlay is applied to a copy of the default profile of the installed docker; Verify is what
// ccsbx-root runs on the installed file before it starts a fence.
package seccomp

import (
	"encoding/json"
	"fmt"
	"sort"
)

// setidMask is S_ISUID|S_ISGID.
const setidMask = 0o6000

// guarded maps each syscall to the index of its mode argument.
var guarded = map[string]int{
	"chmod": 1, "fchmod": 1, "creat": 1, "mknod": 1,
	"fchmodat": 2, "fchmodat2": 2, "open": 2, "mknodat": 2,
	"openat": 3,
}

// refused are syscalls that cannot be filtered by mode and are denied with ENOSYS (so callers fall
// back to the filterable ones).
var refused = []string{"openat2"}

const enosys = 38

func sortedNames() []string {
	var n []string
	for k := range guarded {
		n = append(n, k)
	}
	sort.Strings(n)
	return n
}

// denies reports whether a defaultAction (and its errno) is a plain EPERM denial.
func defaultDenies(action string, errno *int) error {
	if action != "SCMP_ACT_ERRNO" {
		return fmt.Errorf("seccomp profile: defaultAction %q is not SCMP_ACT_ERRNO, so a setid mode would not be refused with EPERM", action)
	}
	if errno != nil && *errno != 1 {
		return fmt.Errorf("seccomp profile: defaultErrnoRet %d is not EPERM (1)", *errno)
	}
	return nil
}

func isDenial(action string) bool {
	switch action {
	case "SCMP_ACT_ERRNO", "SCMP_ACT_KILL", "SCMP_ACT_KILL_PROCESS", "SCMP_ACT_KILL_THREAD":
		return true
	}
	return false
}

// Overlay returns base (Docker's default profile) with the guarded syscalls taken out of every
// allow entry and added back as conditional allows (and openat2 refused). It refuses a base whose
// defaultAction is not an EPERM denial.
func Overlay(base []byte) ([]byte, error) {
	var p map[string]any
	if err := json.Unmarshal(base, &p); err != nil {
		return nil, fmt.Errorf("seccomp base profile: %w", err)
	}
	da, _ := p["defaultAction"].(string)
	var er *int
	if v, ok := p["defaultErrnoRet"].(float64); ok {
		n := int(v)
		er = &n
	}
	if err := defaultDenies(da, er); err != nil {
		return nil, err
	}
	drop := map[string]bool{"openat2": true}
	for n := range guarded {
		drop[n] = true
	}
	var out []any
	entries, _ := p["syscalls"].([]any)
	for _, e := range entries {
		m, ok := e.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("seccomp base profile: a syscalls entry is not an object")
		}
		act, _ := m["action"].(string)
		names, _ := m["names"].([]any)
		if isDenial(act) || names == nil {
			out = append(out, m)
			continue
		}
		var keep []any
		for _, n := range names {
			if s, _ := n.(string); !drop[s] {
				keep = append(keep, n)
			}
		}
		if len(keep) == 0 {
			continue
		}
		m["names"] = keep
		out = append(out, m)
	}
	// one conditional allow per mode-argument index
	byIdx := map[int][]string{}
	for _, n := range sortedNames() {
		byIdx[guarded[n]] = append(byIdx[guarded[n]], n)
	}
	for _, idx := range []int{1, 2, 3} {
		out = append(out, map[string]any{
			"names":   byIdx[idx],
			"action":  "SCMP_ACT_ALLOW",
			"args":    []map[string]any{{"index": idx, "value": setidMask, "valueTwo": 0, "op": "SCMP_CMP_MASKED_EQ"}},
			"comment": "ccsbx: allowed only when the mode has no setuid or setgid bit",
		})
	}
	out = append(out, map[string]any{"names": refused, "action": "SCMP_ACT_ERRNO", "errnoRet": enosys,
		"comment": "ccsbx: the mode is in a struct a filter cannot read"})
	p["syscalls"] = out
	return json.MarshalIndent(p, "", "  ")
}

type entry struct {
	Names    []string `json:"names"`
	Action   string   `json:"action"`
	ErrnoRet *int     `json:"errnoRet"`
	Args     []struct {
		Index    int    `json:"index"`
		Value    uint64 `json:"value"`
		ValueTwo uint64 `json:"valueTwo"`
		Op       string `json:"op"`
	} `json:"args"`
}

// Verify checks an installed profile: the default action is an EPERM denial; no entry that is not a
// denial lets a guarded syscall through except the one conditional allow (mode & 06000 == 0) on the
// right argument; every guarded syscall has that conditional allow; and openat2 is refused.
func Verify(profile []byte) error {
	var p struct {
		DefaultAction   string  `json:"defaultAction"`
		DefaultErrnoRet *int    `json:"defaultErrnoRet"`
		Syscalls        []entry `json:"syscalls"`
	}
	if err := json.Unmarshal(profile, &p); err != nil {
		return fmt.Errorf("seccomp profile: %w", err)
	}
	if err := defaultDenies(p.DefaultAction, p.DefaultErrnoRet); err != nil {
		return err
	}
	cond := map[string]bool{}
	openat2Refused := false
	for _, e := range p.Syscalls {
		for _, n := range e.Names {
			if n == "openat2" {
				if isDenial(e.Action) {
					openat2Refused = true
				} else {
					return fmt.Errorf("seccomp profile allows openat2 (its mode cannot be filtered)")
				}
			}
			idx, g := guarded[n]
			if !g || isDenial(e.Action) {
				continue
			}
			if e.Action == "SCMP_ACT_ALLOW" && len(e.Args) == 1 {
				a := e.Args[0]
				if a.Index == idx && a.Op == "SCMP_CMP_MASKED_EQ" && a.Value == setidMask && a.ValueTwo == 0 {
					cond[n] = true
					continue
				}
			}
			return fmt.Errorf("seccomp profile lets %s through without the mode & 06000 == 0 condition (action %s)", n, e.Action)
		}
	}
	for _, n := range sortedNames() {
		if !cond[n] {
			return fmt.Errorf("seccomp profile has no conditional allow for %s (mode & 06000 == 0 on argument %d)", n, guarded[n])
		}
	}
	if !openat2Refused {
		return fmt.Errorf("seccomp profile does not refuse openat2")
	}
	return nil
}
