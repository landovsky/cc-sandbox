// Tests from the U5 review. They guard against a key signing for a machine it does not own, one
// machine moving another's generation counter, a proof reused on another door or body, and a door that
// holds a seed or keeps serving when it cannot persist a revocation.

package token

import (
	"crypto/ed25519"
	"encoding/json"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newKey(t *testing.T) *Key {
	t.Helper()
	k, err := LoadOrCreateKeyFile(filepath.Join(t.TempDir(), "k"))
	if err != nil {
		t.Fatal(err)
	}
	return k
}

func claimsFor(sub string, g *GenKey, aud ...string) Claims {
	return Claims{Sub: sub, Label: "l", Billing: "api", Profile: "dev-default", Aud: aud, Cnf: CnfOf(g.Public)}
}

// context "both the hp and the Mac key are in a door's ring (T:key-signs-foreign-machine, U5-M1)":
// a key may speak only for the owner/machine it is scoped to. Otherwise the Mac key could mint an
// hp session at g999 and lock the real hp session out by raising its generation counter.
func TestKeySignsForeignMachine(t *testing.T) {
	hp, mac := newKey(t), newKey(t)
	ring := Keyring{
		hp.KID:  {Pub: hp.Public, Owners: []string{"tomas"}, Machines: []string{"hp-ubuntu"}},
		mac.KID: {Pub: mac.Public, Owners: []string{"tomas"}, Machines: []string{"mac"}},
	}
	v := NewVerifier(ring, "ccsbx-model")
	g, _ := NewGenKey()
	call := func(k *Key, sub string) error {
		tok, err := Mint(k, claimsFor(sub, g, "ccsbx-model"), t0)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := SignProof(g, "POST", "/x", "ccsbx-model", tok, nil, t0)
		_, err = v.Verify(tok, p, "POST", "/x", nil, t0)
		return err
	}
	if err := call(mac, "tomas/mac/sMAC/g1"); err != nil {
		t.Fatalf("allow twin: the Mac key for its own machine: %v", err)
	}
	if err := call(hp, "tomas/hp-ubuntu/sHP/g1"); err != nil {
		t.Fatalf("allow twin: the hp key for its own machine: %v", err)
	}
	if err := call(mac, "tomas/hp-ubuntu/sHP/g999"); err != ErrKeyScope {
		t.Fatalf("the Mac key signed an hp session: %v", err)
	}
	if got := v.MaxGen("tomas", "hp-ubuntu", "sHP"); got != 1 {
		t.Fatalf("the refused token moved hp's generation counter to %d", got)
	}
	if err := call(hp, "tomas/hp-ubuntu/sHP/g2"); err != nil {
		t.Fatalf("the real hp session is locked out: %v", err)
	}
	if err := call(hp, "eve/hp-ubuntu/sHP/g3"); err != ErrKeyScope {
		t.Fatalf("a key scoped to owner tomas signed for eve: %v", err)
	}
}

// Same sid on another machine is another session: one machine's generations don't touch another's.
func TestGenerationStateIsPerMachine(t *testing.T) {
	k := newKey(t)
	v := NewVerifier(k.Ring([]string{"tomas"}, []string{"hp-ubuntu", "mac"}), "ccsbx-model")
	g, _ := NewGenKey()
	call := func(sub string) error {
		tok, _ := Mint(k, claimsFor(sub, g, "ccsbx-model"), t0)
		p, _ := SignProof(g, "POST", "/x", "ccsbx-model", tok, nil, t0)
		_, err := v.Verify(tok, p, "POST", "/x", nil, t0)
		return err
	}
	if err := call("tomas/mac/sSAME/g5"); err != nil {
		t.Fatal(err)
	}
	if err := call("tomas/hp-ubuntu/sSAME/g1"); err != nil {
		t.Fatalf("mac's g5 locked out hp's g1: %v", err)
	}
}

// context "something sees one request in transit (U5-S1)": the proof is bound to the door, to this
// token and to this body, so it can't be replayed to another door, with another token, or
// carrying another body.
func TestProofBoundToDoorTokenAndBody(t *testing.T) {
	k := newKey(t)
	ring := k.Ring([]string{"tomas"}, []string{"hp-ubuntu"})
	model, egress := NewVerifier(ring, "ccsbx-model"), NewVerifier(ring, "ccsbx-egress")
	g, _ := NewGenKey()
	tok, _ := Mint(k, claimsFor("tomas/hp-ubuntu/sB/g1", g, "ccsbx-model", "ccsbx-egress"), t0)
	body := []byte(`{"a":1}`)
	p, _ := SignProof(g, "POST", "/v1/x", "ccsbx-model", tok, body, t0)

	if _, err := model.Verify(tok, p, "POST", "/v1/x", body, t0); err != nil {
		t.Fatalf("allow twin: %v", err)
	}
	// the same proof at another door that the same token lists
	if _, err := egress.Verify(tok, p, "POST", "/v1/x", body, t0); err != ErrBadProof {
		t.Fatalf("proof replayed to another door: %v", err)
	}
	// an interceptor keeps the proof and sends its own body
	if _, err := NewVerifier(ring, "ccsbx-model").Verify(tok, p, "POST", "/v1/x", []byte(`{"a":2}`), t0); err != ErrBadProof {
		t.Fatalf("body substituted: %v", err)
	}
	// another token for the same generation key (same cnf)
	c2 := claimsFor("tomas/hp-ubuntu/sB/g1", g, "ccsbx-model")
	c2.Label = "other"
	tok2, _ := Mint(k, c2, t0)
	if _, err := NewVerifier(ring, "ccsbx-model").Verify(tok2, p, "POST", "/v1/x", body, t0); err != ErrBadProof {
		t.Fatalf("proof used with a different token: %v", err)
	}
}

// context "a door must never hold a signing key (U5-S2, invariant 12)": doors load a public
// keyring with scope; the stub no longer has a --key-file at all.
func TestDoorsLoadPublicKeysOnly(t *testing.T) {
	k := newKey(t)
	dir := t.TempDir()
	good := filepath.Join(dir, "ring")
	os.WriteFile(good, []byte("# hp\n"+k.KeyringLine([]string{"tomas"}, []string{"hp-ubuntu"})+"\n"), 0o644)
	ring, err := LoadPublicKeyring(good)
	if err != nil || !ring[k.KID].allows("tomas", "hp-ubuntu") || ring[k.KID].allows("tomas", "mac") {
		t.Fatalf("%v %v", ring, err)
	}
	for name, line := range map[string]string{
		"no scope":       k.KID + " " + b64s(k.Public),
		"empty machines": k.KID + " " + b64s(k.Public) + " owners=tomas machines= profiles=* billings=api",
		"kid mismatch":   "deadbeefdeadbeef " + b64s(k.Public) + " owners=tomas machines=hp-ubuntu profiles=* billings=api",
		"seed not pub":   k.KID + " " + b64s(k.Private.Seed()) + " owners=tomas machines=hp-ubuntu profiles=* billings=api",
	} {
		p := filepath.Join(dir, strings.ReplaceAll(name, " ", "_"))
		os.WriteFile(p, []byte(line+"\n"), 0o644)
		if _, err := LoadPublicKeyring(p); err == nil {
			t.Errorf("%s: keyring accepted", name)
		}
	}
	// The stub package may not even name the functions that read a seed.
	files, _ := filepath.Glob("../stub/*.go")
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, _ := os.ReadFile(f)
		if strings.Contains(string(b), "LoadKeyFile") || strings.Contains(string(b), "LoadOrCreateKeyFile") {
			t.Errorf("%s reads a signing key", f)
		}
	}
}

func b64s(b []byte) string { return base64.StdEncoding.EncodeToString(b) }

// context "the state directory is full or read-only (U5-S3)": a revocation that can't be written
// down must not leave the door serving the revoked token after a restart.
func TestRevocationFailsClosed(t *testing.T) {
	k := newKey(t)
	ring := k.Ring([]string{"tomas"}, []string{"hp-ubuntu"})
	dir := t.TempDir()
	state := filepath.Join(dir, "state.json")
	v := NewVerifier(ring, "ccsbx-model")
	if err := v.PersistTo(state); err != nil {
		t.Fatal(err)
	}
	g, _ := NewGenKey()
	tok, _ := Mint(k, claimsFor("tomas/hp-ubuntu/sR/g1", g, "ccsbx-model"), t0)
	call := func(tm time.Time) error {
		p, _ := SignProof(g, "POST", "/x", "ccsbx-model", tok, nil, tm)
		_, err := v.Verify(tok, p, "POST", "/x", nil, tm)
		return err
	}
	if err := call(t0); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o500); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(dir, 0o700)
	if os.WriteFile(filepath.Join(dir, "probe"), nil, 0o600) == nil {
		t.Skip("directory permissions not enforced for this user")
	}
	if err := v.Revoke("tomas", "hp-ubuntu", "sR", 1); err == nil {
		t.Fatal("Revoke reported success though it could not be written")
	}
	if err := call(t0.Add(time.Second)); err != ErrState && err != ErrGenRevoked {
		t.Fatalf("the door served a request while unable to persist: %v", err)
	}
	// A different session's request is refused too: the door can't promise to remember anything.
	g2, _ := NewGenKey()
	tok2, _ := Mint(k, claimsFor("tomas/hp-ubuntu/sOTHER/g1", g2, "ccsbx-model"), t0)
	p2, _ := SignProof(g2, "POST", "/x", "ccsbx-model", tok2, nil, t0.Add(2*time.Second))
	if _, err := v.Verify(tok2, p2, "POST", "/x", nil, t0.Add(2*time.Second)); err != ErrState {
		t.Fatalf("a door that can't persist served another session: %v", err)
	}
	os.Chmod(dir, 0o700) // disk recovered
	p3, _ := SignProof(g2, "POST", "/x", "ccsbx-model", tok2, nil, t0.Add(3*time.Second))
	if _, err := v.Verify(tok2, p3, "POST", "/x", nil, t0.Add(3*time.Second)); err != nil {
		t.Fatalf("door did not recover: %v", err)
	}
	// And the revocation is on disk now: a restarted door refuses the revoked generation.
	v.Close() // the door stops, a new one starts
	v2 := NewVerifier(ring, "ccsbx-model")
	if err := v2.PersistTo(state); err != nil {
		t.Fatal(err)
	}
	p4, _ := SignProof(g, "POST", "/x", "ccsbx-model", tok, nil, t0.Add(4*time.Second))
	if _, err := v2.Verify(tok, p4, "POST", "/x", nil, t0.Add(4*time.Second)); err != ErrGenRevoked {
		t.Fatalf("revoked generation accepted after restart: %v", err)
	}
}

// NIT U5-N2: a door that proxies upstream must not be handed the session JWT to forward.
func TestMiddlewareStripsCredentialsBeforeNext(t *testing.T) {
	k := newKey(t)
	v := NewVerifier(k.Ring([]string{"tomas"}, []string{"hp-ubuntu"}), "ccsbx-model")
	g, _ := NewGenKey()
	tok, _ := Mint(k, claimsFor("tomas/hp-ubuntu/sM/g1", g, "ccsbx-model"), time.Now())
	var sawAuth, sawProof, sawBody string
	srv := httptest.NewServer(v.Middleware(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sawAuth, sawProof = r.Header.Get("Authorization"), r.Header.Get(ProofHeader)
		b, _ := io.ReadAll(r.Body)
		sawBody = string(b)
	}), nil))
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/x", strings.NewReader("payload"))
	p, _ := SignProof(g, "POST", "/x", "ccsbx-model", tok, []byte("payload"), time.Now())
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set(ProofHeader, p)
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
	if sawAuth != "" || sawProof != "" {
		t.Fatal("the session JWT or proof reached the handler")
	}
	if sawBody != "payload" {
		t.Fatalf("the handler lost the body: %q", sawBody)
	}
}

// NIT U5-N4: invariant 18, no door invents a default billing.
func TestMintRefusesUnknownBillingOrEmptyProfile(t *testing.T) {
	k := newKey(t)
	g, _ := NewGenKey()
	for _, tc := range []struct{ billing, profile string }{{"", "p"}, {"free", "p"}, {"api", ""}} {
		c := claimsFor("tomas/hp-ubuntu/sX/g1", g, "ccsbx-model")
		c.Billing, c.Profile = tc.billing, tc.profile
		if _, err := Mint(k, c, t0); err != ErrClaim {
			t.Errorf("%+v: %v", tc, err)
		}
	}
}

// NIT U5-N3: creating the key never follows a symlink planted at the path.
func TestKeyCreationRefusesSymlink(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	link := filepath.Join(dir, "k")
	os.Symlink(target, link)
	if _, err := LoadOrCreateKeyFile(link); err == nil {
		t.Fatal("key created through a dangling symlink")
	}
	if _, err := os.Stat(target); err == nil {
		t.Fatal("the symlink target was created")
	}
}

// handSigned signs claims WITHOUT Mint's checks, the way a buggy or future signer could.
func handSigned(k *Key, c Claims) string {
	c.Iat, c.Exp = t0.Unix(), t0.Add(time.Hour).Unix()
	hb, _ := json.Marshal(header{Alg: "EdDSA", Typ: "JWT", KID: k.KID})
	pb, _ := json.Marshal(c)
	h, p := b64.EncodeToString(hb), b64.EncodeToString(pb)
	return h + "." + p + "." + b64.EncodeToString(ed25519.Sign(k.Private, signingInput(h, p)))
}

// context "a door must not trust the signer to have checked billing (review S2, invariant 18)":
// a token with an empty billing or profile is refused by Verify even though its signature is good.
func TestDoorRechecksBillingAndProfile(t *testing.T) {
	k := newKey(t)
	v := NewVerifier(k.Ring([]string{"tomas"}, []string{"hp-ubuntu"}), "ccsbx-model")
	g, _ := NewGenKey()
	for _, tc := range []struct{ billing, profile string }{{"", "dev-default"}, {"free", "dev-default"}, {"api", ""}} {
		c := claimsFor("tomas/hp-ubuntu/sX/g1", g, "ccsbx-model")
		c.Billing, c.Profile = tc.billing, tc.profile
		tok := handSigned(k, c)
		p, _ := SignProof(g, "POST", "/x", "ccsbx-model", tok, nil, t0)
		if _, err := v.Verify(tok, p, "POST", "/x", nil, t0); err != ErrClaim {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
	// allow twin: the same hand-signing path with good claims verifies, so the test isn't vacuous.
	tok := handSigned(k, claimsFor("tomas/hp-ubuntu/sX/g1", g, "ccsbx-model"))
	p, _ := SignProof(g, "POST", "/x", "ccsbx-model", tok, nil, t0)
	if _, err := v.Verify(tok, p, "POST", "/x", nil, t0); err != nil {
		t.Fatalf("hand-signed good token refused: %v", err)
	}
}

// context "the Mac key must not sign above the Mac's tier (T:mac-key-above-mac-max, review S1)":
// a key whose keyring line limits profiles and billings can't sign anything outside them.
func TestKeyScopedByProfileAndBilling(t *testing.T) {
	k := newKey(t)
	f := filepath.Join(t.TempDir(), "ring")
	line := k.KeyringLineScoped([]string{"tomas"}, []string{"mac"}, []string{"dev-*", "e2e-*"}, []string{"api", "stub"})
	if err := os.WriteFile(f, []byte(line+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ring, err := LoadPublicKeyring(f)
	if err != nil {
		t.Fatal(err)
	}
	v := NewVerifier(ring, "ccsbx-model")
	g, _ := NewGenKey()
	try := func(profile, billing string) error {
		c := claimsFor("tomas/mac/sM/g1", g, "ccsbx-model")
		c.Profile, c.Billing = profile, billing
		tok, err := Mint(k, c, t0)
		if err != nil {
			t.Fatal(err)
		}
		p, _ := SignProof(g, "POST", "/x", "ccsbx-model", tok, nil, t0)
		_, err = v.Verify(tok, p, "POST", "/x", nil, t0)
		return err
	}
	if err := try("dev-default", "api"); err != nil {
		t.Fatalf("allow twin: %v", err)
	}
	if err := try("e2e-basic", "stub"); err != nil {
		t.Fatalf("allow twin: %v", err)
	}
	if err := try("dev-default", "subscription"); err != ErrKeyScope {
		t.Fatalf("billing above the key's tier: %v", err)
	}
	if err := try("prod-admin", "api"); err != ErrKeyScope {
		t.Fatalf("profile outside the key's tier: %v", err)
	}
	// an empty profiles= is a mistake, not allow-all; a duplicate kid is refused (review N7).
	os.WriteFile(f, []byte(strings.Replace(line, "machines=mac", "machines=mac profiles=", 1)+"\n"), 0o600)
	if _, err := LoadPublicKeyring(f); err == nil {
		t.Fatal("empty profiles= accepted")
	}
	os.WriteFile(f, []byte(line+"\n"+line+"\n"), 0o600)
	if _, err := LoadPublicKeyring(f); err == nil {
		t.Fatal("a kid listed twice was accepted")
	}
}

// context "a generation ends and hostd revokes it over the wire (T:token-after-exit, review S4)":
// only a signed request from a key scoped to that machine kills a generation at the door.
func TestRevokeEndpoint(t *testing.T) {
	hp, mac := newKey(t), newKey(t)
	ring := Keyring{
		hp.KID:  {Pub: hp.Public, Owners: []string{"tomas"}, Machines: []string{"hp-ubuntu"}},
		mac.KID: {Pub: mac.Public, Owners: []string{"tomas"}, Machines: []string{"mac"}},
	}
	v := NewVerifier(ring, "ccsbx-model")
	srv := httptest.NewServer(v.RevokeHandler(func() time.Time { return t0 }))
	defer srv.Close()
	g, _ := NewGenKey()
	tok, _ := Mint(hp, claimsFor("tomas/hp-ubuntu/sR/g2", g, "ccsbx-model"), t0)
	use := func() error {
		p, _ := SignProof(g, "POST", "/x", "ccsbx-model", tok, nil, t0)
		_, err := v.Verify(tok, p, "POST", "/x", nil, t0)
		return err
	}
	send := func(hdr string, body []byte) int {
		req, _ := http.NewRequest("POST", srv.URL+RevokePath, strings.NewReader(string(body)))
		if hdr != "" {
			req.Header.Set(RevokeHeader, hdr)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	body := []byte(`{"owner":"tomas","machine":"hp-ubuntu","sid":"sR","gen":2}`)
	if err := use(); err != nil {
		t.Fatalf("allow twin: %v", err)
	}
	if c := send("", body); c != 401 {
		t.Fatalf("unsigned revoke: %d", c)
	}
	if c := send(SignRevoke(mac, body, t0), body); c != 401 {
		t.Fatalf("the Mac key revoked an hp session: %d", c)
	}
	if c := send(SignRevoke(hp, body, t0.Add(-time.Hour)), body); c != 401 {
		t.Fatalf("stale revoke: %d", c)
	}
	if c := send(SignRevoke(hp, body, t0), []byte(`{"owner":"tomas","machine":"hp-ubuntu","sid":"sR","gen":9}`)); c != 401 {
		t.Fatalf("signature moved to another body: %d", c)
	}
	if err := use(); err != nil {
		t.Fatalf("a refused revoke still killed the token: %v", err)
	}
	if c := send(SignRevoke(hp, body, t0), body); c != 204 {
		t.Fatalf("valid revoke: %d", c)
	}
	if err := use(); err != ErrGenRevoked {
		t.Fatalf("token still works after revoke: %v", err)
	}
}

// Review N-scope: a keyring line without its tier limits is refused, so a key can't sign any tier
// by omission (T:mac-key-above-mac-max).
func TestKeyringLineWithoutTierLimitsIsRefused(t *testing.T) {
	k := newKey(t)
	f := filepath.Join(t.TempDir(), "ring")
	base := k.KID + " " + base64.StdEncoding.EncodeToString(k.Public) + " owners=tomas machines=mac"
	for name, line := range map[string]string{
		"no limits":     base,
		"no billings":   base + " profiles=dev-*",
		"no profiles":   base + " billings=api",
	} {
		os.WriteFile(f, []byte(line+"\n"), 0o600)
		if _, err := LoadPublicKeyring(f); err == nil {
			t.Fatalf("%s: accepted", name)
		}
	}
	os.WriteFile(f, []byte(base+" profiles=dev-* billings=api\n"), 0o600)
	if _, err := LoadPublicKeyring(f); err != nil {
		t.Fatalf("allow twin: %v", err)
	}
}

// Review N-state: two doors must not share one state file. A second PersistTo on a path another
// instance holds is refused, and temp files are unique, so one door can't overwrite the other's
// revocations.
func TestTwoDoorsCannotShareAStateFile(t *testing.T) {
	k := newKey(t)
	ring := k.Ring([]string{"tomas"}, []string{"hp-ubuntu"})
	state := filepath.Join(t.TempDir(), "state.json")
	a, b := NewVerifier(ring, "ccsbx-stub"), NewVerifier(ring, "ccsbx-stub")
	if err := a.PersistTo(state); err != nil {
		t.Fatal(err)
	}
	if err := b.PersistTo(state); err == nil {
		t.Fatal("a second door took over a state file that is in use")
	}
	if err := a.Revoke("tomas", "hp-ubuntu", "sX", 1); err != nil {
		t.Fatal(err)
	}
	if left, _ := filepath.Glob(state + ".*.tmp"); len(left) != 0 {
		t.Fatalf("temp files left behind: %v", left)
	}
}
