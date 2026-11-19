package profile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestProfiles(t *testing.T) {
	t.Run("the shipped dev-default profile loads and is approved for hp with api billing", func(t *testing.T) {
		p, err := Load("../../profiles", "dev-default")
		if err != nil {
			t.Fatal(err)
		}
		m, b := p.Allows("hp-ubuntu", "api")
		if !m || !b {
			t.Fatal("not approved for hp/api")
		}
		if m, _ := p.Allows("macbook", "api"); m {
			t.Fatal("approved on a machine it was never approved for")
		}
	})

	t.Run("a profile name can't walk out of the profiles dir", func(t *testing.T) {
		if _, err := Load(t.TempDir(), "../../etc/passwd"); err == nil {
			t.Fatal("path traversal accepted")
		}
	})

	t.Run("a misspelt field is an error, not a silently weaker profile", func(t *testing.T) {
		_, err := Parse([]byte(`{"name":"x","machines":["hp"],"billing":["api"],"memory_mb":512,"pids":64,"mode":"plan","denny":[]}`))
		if err == nil || !strings.Contains(err.Error(), "denny") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("a profile approved nowhere is refused", func(t *testing.T) {
		_, err := Parse([]byte(`{"name":"x","machines":[],"billing":["api"],"memory_mb":512,"pids":64,"mode":"plan"}`))
		if err == nil {
			t.Fatal("empty machines accepted")
		}
	})

	t.Run("the e2e flag must agree with the name so a normal profile can't turn on the stub's local mode", func(t *testing.T) {
		_, err := Parse([]byte(`{"name":"dev-x","machines":["hp"],"billing":["stub"],"memory_mb":512,"pids":64,"mode":"plan","e2e":true}`))
		if err == nil {
			t.Fatal("e2e on a non-e2e name accepted")
		}
	})

	t.Run("file name and declared name must match", func(t *testing.T) {
		d := t.TempDir()
		os.WriteFile(filepath.Join(d, "a.json"), []byte(`{"name":"b","machines":["hp"],"billing":["api"],"memory_mb":512,"pids":64,"mode":"plan"}`), 0o600)
		if _, err := Load(d, "a"); err == nil {
			t.Fatal("mismatch accepted")
		}
	})
}

// H2: the candidate channel is for e2e-* profiles only, so the shipped e2e profile must exist,
// carry the flag, and be stub-billed (an e2e run never spends real money).
func TestShippedE2EProfile(t *testing.T) {
	p, err := Load("../../profiles", "e2e-default")
	if err != nil {
		t.Fatal(err)
	}
	if !p.E2E {
		t.Fatal("e2e-default is not flagged e2e")
	}
	if _, b := p.Allows("hp-ubuntu", "api"); b {
		t.Fatal("an e2e profile may bill the api")
	}
}
