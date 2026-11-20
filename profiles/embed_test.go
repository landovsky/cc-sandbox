// Tests for the embedded profile set: root reads caps and the e2e flag from here, so a profile
// that is missing, misnamed or a path must not load.
package profiles

import "testing"

func TestLoad(t *testing.T) {
	p, err := Load("dev-default")
	if err != nil || p.MemoryMB != 4096 || p.E2E {
		t.Fatalf("%+v %v", p, err)
	}
	if e, err := Load("e2e-default"); err != nil || !e.E2E {
		t.Fatalf("%+v %v", e, err)
	}
	for _, bad := range []string{"", "nope", "../profiles/dev-default", "dev-default.json", "DEV", "dev default", "/etc/passwd"} {
		if _, err := Load(bad); err == nil {
			t.Errorf("%q loaded", bad)
		}
	}
}
