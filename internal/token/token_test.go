package token

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 19, 23, 0, 0, 0, time.UTC)

type rig struct {
	key  *Key
	gen  *GenKey
	v    *Verifier
	tok  string
	base Claims
}

func newRig(t *testing.T) *rig {
	t.Helper()
	k, err := LoadOrCreateKeyFile(filepath.Join(t.TempDir(), "dev.key"))
	if err != nil {
		t.Fatal(err)
	}
	g, _ := NewGenKey()
	r := &rig{key: k, gen: g, v: NewVerifier(k.Ring([]string{"tomas"}, []string{"hp-ubuntu"}), "ccsbx-model")}
	r.base = Claims{Sub: "tomas/hp-ubuntu/s01jd7k2m9/g3", Label: "cc-sandbox:326q", Billing: "api",
		Profile: "dev-default", Aud: []string{"ccsbx-model"}, Cnf: CnfOf(g.Public)}
	r.tok = r.mint(t, r.base)
	return r
}

func (r *rig) mint(t *testing.T, c Claims) string {
	t.Helper()
	s, err := Mint(r.key, c, t0)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func (r *rig) call(t *testing.T, tok string, g *GenKey, now time.Time) (*Claims, error) {
	t.Helper()
	p, _ := SignProof(g, "POST", "/v1/messages", "ccsbx-model", tok, nil, now)
	return r.v.Verify(tok, p, "POST", "/v1/messages", nil, now)
}

func TestTokenAccepted(t *testing.T) {
	r := newRig(t)
	c, err := r.call(t, r.tok, r.gen, t0.Add(time.Second))
	if err != nil || c.SID() != "s01jd7k2m9" || c.Gen() != 3 {
		t.Fatalf("got %v %v", c, err)
	}
}

// context "a token copied out of the forwarder (T:token-copied)": the thief has
// the JWT but not the per-generation key, so every call must be 401.
func TestTokenCopied(t *testing.T) {
	r := newRig(t)
	thief, _ := NewGenKey()
	if _, err := r.call(t, r.tok, thief, t0); err != ErrBadProof {
		t.Fatalf("foreign key: %v", err)
	}
	if _, err := r.v.Verify(r.tok, "", "POST", "/v1/messages", nil, t0); err != ErrNoProof {
		t.Fatalf("no proof: %v", err)
	}
	// A proof made for one request can't be moved to another path or method.
	p, _ := SignProof(r.gen, "POST", "/v1/messages", "ccsbx-model", r.tok, nil, t0)
	if _, err := r.v.Verify(r.tok, p, "POST", "/v1/other", nil, t0); err != ErrBadProof {
		t.Fatalf("moved path: %v", err)
	}
	if _, err := r.v.Verify(r.tok, p, "GET", "/v1/messages", nil, t0); err != ErrBadProof {
		t.Fatalf("moved method: %v", err)
	}
}

func TestProofReplayAndStale(t *testing.T) {
	r := newRig(t)
	p, _ := SignProof(r.gen, "POST", "/x", "ccsbx-model", r.tok, nil, t0)
	if _, err := r.v.Verify(r.tok, p, "POST", "/x", nil, t0); err != nil {
		t.Fatal(err)
	}
	if _, err := r.v.Verify(r.tok, p, "POST", "/x", nil, t0.Add(time.Second)); err != ErrProofReplay {
		t.Fatalf("replay: %v", err)
	}
	old, _ := SignProof(r.gen, "POST", "/x", "ccsbx-model", r.tok, nil, t0)
	if _, err := r.v.Verify(r.tok, old, "POST", "/x", nil, t0.Add(3*time.Minute)); err != ErrProofStale {
		t.Fatalf("stale: %v", err)
	}
}

// context "the signer's key id isn't in the door's keyring (T:token-unknown-kid)".
func TestTokenUnknownKID(t *testing.T) {
	r := newRig(t)
	other, _ := LoadOrCreateKeyFile(filepath.Join(t.TempDir(), "other.key"))
	tok, _ := Mint(other, r.base, t0)
	if _, err := r.call(t, tok, r.gen, t0); err != ErrUnknownKID {
		t.Fatalf("got %v", err)
	}
}

// A forged kid pointing at a trusted key, with a signature by another key.
func TestTokenForgedSignature(t *testing.T) {
	r := newRig(t)
	other, _ := LoadOrCreateKeyFile(filepath.Join(t.TempDir(), "other.key"))
	other.KID = r.key.KID
	tok, _ := Mint(other, r.base, t0)
	if _, err := r.call(t, tok, r.gen, t0); err != ErrBadSig {
		t.Fatalf("got %v", err)
	}
}

// The signature domain keeps a token signature from verifying as anything else.
func TestDomainSeparation(t *testing.T) {
	other := signingInput("a", "b")
	if string(other[:len(DomainJWT)]) != DomainJWT {
		t.Fatal("prefix")
	}
}

func TestAlgNone(t *testing.T) {
	r := newRig(t)
	h := b64.EncodeToString([]byte(`{"alg":"none","typ":"JWT","kid":"` + r.key.KID + `"}`))
	_, p, _ := split3(r.tok)
	if _, err := r.call(t, h+"."+p+".", r.gen, t0); err != ErrAlg {
		t.Fatalf("got %v", err)
	}
}

func split3(s string) (string, string, string) {
	var a [3]string
	i := 0
	cur := ""
	for _, ch := range s {
		if ch == '.' && i < 2 {
			a[i] = cur
			cur = ""
			i++
			continue
		}
		cur += string(ch)
	}
	a[2] = cur
	return a[0], a[1], a[2]
}

// context "a generation ended (T:token-after-exit) / a newer one started (T:token-gen-bump)".
func TestTokenAfterExit(t *testing.T) {
	r := newRig(t)
	if _, err := r.call(t, r.tok, r.gen, t0); err != nil {
		t.Fatal(err)
	}
	r.v.Revoke("tomas", "hp-ubuntu", "s01jd7k2m9", 3)
	if _, err := r.call(t, r.tok, r.gen, t0.Add(time.Second)); err != ErrGenRevoked {
		t.Fatalf("got %v", err)
	}
}

func TestTokenGenBump(t *testing.T) {
	r := newRig(t)
	g4, _ := NewGenKey()
	c4 := r.base
	c4.Sub = "tomas/hp-ubuntu/s01jd7k2m9/g4"
	c4.Cnf = CnfOf(g4.Public)
	tok4 := r.mint(t, c4)
	if _, err := r.call(t, tok4, g4, t0); err != nil {
		t.Fatal(err)
	}
	// g3's token is still unexpired and its key still valid, but g4 exists now.
	if _, err := r.call(t, r.tok, r.gen, t0.Add(time.Second)); err != ErrGenRevoked {
		t.Fatalf("old gen: %v", err)
	}
	// A different session is unaffected.
	other := r.base
	other.Sub = "tomas/hp-ubuntu/sOTHER/g1"
	if _, err := r.call(t, r.mint(t, other), r.gen, t0.Add(2*time.Second)); err != nil {
		t.Fatal(err)
	}
}

func TestMaxGenSurvivesRestart(t *testing.T) {
	r := newRig(t)
	state := filepath.Join(t.TempDir(), "gen.json")
	r.v.PersistTo(state)
	g4, _ := NewGenKey()
	c4 := r.base
	c4.Sub = "tomas/hp-ubuntu/s01jd7k2m9/g4"
	c4.Cnf = CnfOf(g4.Public)
	r.call(t, r.mint(t, c4), g4, t0)
	r.v = NewVerifier(r.key.Ring([]string{"tomas"}, []string{"hp-ubuntu"}), "ccsbx-model") // door restarts
	if err := r.v.PersistTo(state); err != nil {
		t.Fatal(err)
	}
	if _, err := r.call(t, r.tok, r.gen, t0.Add(time.Second)); err != ErrGenRevoked {
		t.Fatalf("got %v", err)
	}
}

func TestExpiryAudienceLifetime(t *testing.T) {
	r := newRig(t)
	if _, err := r.call(t, r.tok, r.gen, t0.Add(13*time.Hour)); err != ErrExpired {
		t.Fatalf("expired: %v", err)
	}
	wrong := NewVerifier(r.key.Ring([]string{"tomas"}, []string{"hp-ubuntu"}), "ccsbx-egress")
	p, _ := SignProof(r.gen, "POST", "/x", "ccsbx-egress", r.tok, nil, t0)
	if _, err := wrong.Verify(r.tok, p, "POST", "/x", nil, t0); err != ErrAudience {
		t.Fatalf("aud: %v", err)
	}
	long := r.base
	long.Exp = t0.Add(13 * time.Hour).Unix()
	if _, err := Mint(r.key, long, t0); err != ErrLifetime {
		t.Fatalf("mint refuses >12h: %v", err)
	}
}

func TestKeyFileMode(t *testing.T) {
	p := filepath.Join(t.TempDir(), "k")
	if _, err := LoadOrCreateKeyFile(p); err != nil {
		t.Fatal(err)
	}
	if err := chmod(p, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateKeyFile(p); err == nil {
		t.Fatal("world-readable key file accepted")
	}
}

func chmod(p string, m os.FileMode) error { return os.Chmod(p, m) }
