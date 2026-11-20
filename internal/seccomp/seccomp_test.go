// Tests for the setid seccomp profile. They cannot start a container, so they check the profile
// text the kernel would be given, against a small fixture that has the shapes of Docker's default
// profile (an unconditional allow of the guarded calls, a conditional allow, an allow with a
// capability condition, a denial). The real twin, a fence trying to set the bits, is
// tests/seccomp-setuid-test.sh, which needs docker and is run at Gate M.
package seccomp

import (
	"encoding/json"
	"strings"
	"testing"
)

const base = `{"defaultAction":"SCMP_ACT_ERRNO","defaultErrnoRet":1,"architectures":["SCMP_ARCH_X86_64"],
 "syscalls":[
  {"names":["read","write","chmod","fchmod","fchmodat","fchmodat2","open","openat","openat2","creat","mknod","mknodat"],"action":"SCMP_ACT_ALLOW"},
  {"names":["close","openat"],"action":"SCMP_ACT_ALLOW","includes":{"minKernel":"5.0"}},
  {"names":["clone"],"action":"SCMP_ACT_ALLOW","args":[{"index":0,"value":2114060288,"op":"SCMP_CMP_MASKED_EQ"}]},
  {"names":["mknodat"],"action":"SCMP_ACT_ALLOW","args":[{"index":2,"value":0,"op":"SCMP_CMP_EQ"}],"includes":{"caps":["CAP_MKNOD"]}},
  {"names":["ptrace"],"action":"SCMP_ACT_ERRNO","errnoRet":1}
 ]}`

func TestOverlayLeavesNoUnconditionalAllowOfTheGuardedCalls(t *testing.T) {
	out, err := Overlay([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(out); err != nil {
		t.Fatalf("an overlaid profile does not verify: %v", err)
	}
	var p struct {
		Syscalls []entry `json:"syscalls"`
	}
	json.Unmarshal(out, &p)
	cond := map[string]int{}
	for _, e := range p.Syscalls {
		for _, n := range e.Names {
			if _, g := guarded[n]; g && e.Action == "SCMP_ACT_ALLOW" {
				if len(e.Args) != 1 || e.Args[0].Value != 0o6000 || e.Args[0].ValueTwo != 0 {
					t.Errorf("%s still has an allow that is not the setid-free one: %+v", n, e)
				}
				cond[n]++
			}
			if n == "openat2" && e.Action == "SCMP_ACT_ALLOW" {
				t.Error("openat2 is still allowed")
			}
		}
	}
	for n := range guarded {
		if cond[n] != 1 {
			t.Errorf("%s has %d conditional allows, want exactly 1", n, cond[n])
		}
	}
}

func TestOverlayKeepsTheRestOfTheBase(t *testing.T) {
	out, _ := Overlay([]byte(base))
	s := string(out)
	for _, want := range []string{`"read"`, `"write"`, `"close"`, `"clone"`, `"ptrace"`, `"architectures"`, `"defaultErrnoRet": 1`, `"minKernel": "5.0"`} {
		if !strings.Contains(s, want) {
			t.Errorf("lost %s", want)
		}
	}
	// the capability-conditioned allow of mknodat had only that name: it is gone, not widened
	var p struct {
		Syscalls []entry `json:"syscalls"`
	}
	json.Unmarshal(out, &p)
	for _, e := range p.Syscalls {
		for _, n := range e.Names {
			if n == "mknodat" && len(e.Args) == 1 && e.Args[0].Op == "SCMP_CMP_EQ" {
				t.Error("an old conditional allow of mknodat survived")
			}
		}
	}
}

func TestOverlayRefusesABaseThatDoesNotDenyByDefault(t *testing.T) {
	for _, b := range []string{
		`{"defaultAction":"SCMP_ACT_ALLOW"}`, `{}`, `{"defaultAction":"SCMP_ACT_LOG"}`, `not json`,
		`{"defaultAction":"SCMP_ACT_KILL"}`,                       // a denial, but not EPERM: a setid mode would kill, not fail with EPERM
		`{"defaultAction":"SCMP_ACT_ERRNO","defaultErrnoRet":13}`, // the wrong errno
	} {
		if _, err := Overlay([]byte(b)); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
}

// Verify is what ccsbx-root runs on the installed profile, so each way of weakening it is a case.
func TestVerifyRefusesWeakenedProfiles(t *testing.T) {
	good, err := Overlay([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	g := string(good)
	cases := map[string]string{
		"docker's default as it is (unconditional allows)": base,
		"a permissive default":                             strings.Replace(g, `"defaultAction": "SCMP_ACT_ERRNO"`, `"defaultAction": "SCMP_ACT_ALLOW"`, 1),
		"a default that is not EPERM":                      strings.Replace(g, `"defaultErrnoRet": 1`, `"defaultErrnoRet": 13`, 1),
		"the mask is not 06000":                            strings.Replace(g, `"value": 3072`, `"value": 2048`, 1),
		"the datum is not 0":                               strings.Replace(g, `"valueTwo": 0`, `"valueTwo": 2048`, 1),
		"the wrong argument":                               strings.Replace(g, `"index": 3`, `"index": 2`, 1),
		"an extra unconditional allow of chmod": mutate(t, g, func(sys []any) []any {
			return append(sys, map[string]any{"names": []any{"chmod"}, "action": "SCMP_ACT_ALLOW"})
		}),
		"openat2 allowed": mutate(t, g, func(sys []any) []any {
			return append(sys, map[string]any{"names": []any{"openat2"}, "action": "SCMP_ACT_ALLOW"})
		}),
		"openat2 not refused": mutate(t, g, func(sys []any) []any {
			var out []any
			for _, e := range sys {
				if n, _ := e.(map[string]any)["names"].([]any); len(n) == 1 && n[0] == "openat2" {
					continue
				}
				out = append(out, e)
			}
			return out
		}),
	}
	for why, c := range cases {
		if c == g {
			t.Fatalf("%s: the fixture edit did not change the profile", why)
		}
		if err := Verify([]byte(c)); err == nil {
			t.Errorf("accepted: %s", why)
		}
	}
	// a missing conditional allow for one call
	noMknod := mutate(t, g, func(sys []any) []any {
		for _, e := range sys {
			m := e.(map[string]any)
			if _, has := m["args"]; has && m["action"] == "SCMP_ACT_ALLOW" {
				var keep []any
				for _, n := range m["names"].([]any) {
					if n != "mknod" {
						keep = append(keep, n)
					}
				}
				m["names"] = keep
			}
		}
		return sys
	})
	if err := Verify([]byte(noMknod)); err == nil {
		t.Error("accepted a profile without the conditional allow for mknod")
	}
	if err := Verify([]byte(g)); err != nil {
		t.Errorf("the good profile was refused: %v", err)
	}
}

// mutate edits the syscalls list of a profile and returns the new text.
func mutate(t *testing.T, profile string, f func([]any) []any) string {
	t.Helper()
	var p map[string]any
	if err := json.Unmarshal([]byte(profile), &p); err != nil {
		t.Fatal(err)
	}
	p["syscalls"] = f(p["syscalls"].([]any))
	b, _ := json.Marshal(p)
	return string(b)
}
