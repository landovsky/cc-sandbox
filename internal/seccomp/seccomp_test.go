// Tests for the setid seccomp overlay. They cannot start a container, so they check the profile
// text the kernel would be given: every mode-taking syscall, both bits, EPERM, the right argument
// index, the base rules kept, and a permissive base or a profile without the rules refused. The
// real twin (a fence trying chmod 4755) is tests/seccomp-setuid-test.sh, which needs docker.
package seccomp

import (
	"encoding/json"
	"strings"
	"testing"
)

const base = `{"defaultAction":"SCMP_ACT_ERRNO","architectures":["SCMP_ARCH_X86_64"],
 "syscalls":[{"names":["read","write","chmod","fchmod","fchmodat"],"action":"SCMP_ACT_ALLOW"}]}`

func TestOverlayAddsEightRulesAndKeepsTheBase(t *testing.T) {
	out, err := Overlay([]byte(base))
	if err != nil {
		t.Fatal(err)
	}
	if err := Verify(out); err != nil {
		t.Fatalf("an overlaid profile does not verify: %v", err)
	}
	var p map[string]any
	json.Unmarshal(out, &p)
	if p["defaultAction"] != "SCMP_ACT_ERRNO" || p["architectures"] == nil {
		t.Errorf("base fields lost: %v", p)
	}
	sys := p["syscalls"].([]any)
	if len(sys) != 1+8 {
		t.Errorf("%d rules, want the base's 1 plus 8", len(sys))
	}
	if first := sys[0].(map[string]any); first["action"] != "SCMP_ACT_ALLOW" {
		t.Error("the base rule moved")
	}
	for _, want := range []string{`"fchmodat2"`, `"valueTwo": 2048`, `"valueTwo": 1024`, `"index": 2`} {
		if !strings.Contains(string(out), want) {
			t.Errorf("missing %s", want)
		}
	}
}

func TestOverlayRefusesAPermissiveBase(t *testing.T) {
	for _, b := range []string{`{"defaultAction":"SCMP_ACT_ALLOW"}`, `{}`, `{"defaultAction":"SCMP_ACT_LOG"}`, `not json`} {
		if _, err := Overlay([]byte(b)); err == nil {
			t.Errorf("accepted %s", b)
		}
	}
}

// Verify is what ccsbx-root runs on the installed profile, so each way of weakening it is a case.
func TestVerifyRefusesWeakenedProfiles(t *testing.T) {
	good, _ := Overlay([]byte(base))
	if err := Verify(good); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"no rules at all":            base,
		"a permissive default":       strings.Replace(string(good), `"SCMP_ACT_ERRNO",\n  "syscalls"`, `"SCMP_ACT_ALLOW",\n  "syscalls"`, 1),
		"not EPERM":                  strings.Replace(string(good), `"errnoRet": 1`, `"errnoRet": 0`, 1),
		"a rule for the wrong arg":   strings.Replace(string(good), `"index": 1`, `"index": 0`, 1),
		"one bit only":               strings.ReplaceAll(string(good), `"valueTwo": 1024`, `"valueTwo": 1`),
		"allowed instead of refused": strings.Replace(string(good), `"action": "SCMP_ACT_ERRNO",\n      "args"`, `"action": "SCMP_ACT_ALLOW",\n      "args"`, 1),
	}
	for why, c := range cases {
		if why == "a permissive default" {
			c = strings.Replace(string(good), `"defaultAction": "SCMP_ACT_ERRNO"`, `"defaultAction": "SCMP_ACT_ALLOW"`, 1)
		}
		if why == "allowed instead of refused" {
			c = strings.Replace(string(good), `"action": "SCMP_ACT_ERRNO"`, `"action": "SCMP_ACT_ALLOW"`, 1)
			c = strings.Replace(c, `"defaultAction": "SCMP_ACT_ALLOW"`, `"defaultAction": "SCMP_ACT_ERRNO"`, 1)
		}
		if err := Verify([]byte(c)); err == nil {
			t.Errorf("accepted: %s", why)
		}
	}
}
