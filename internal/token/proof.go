// proof.go is proof of possession and the door-side Verifier. It exists so a token copied out of a
// fence is useless without the generation key, and so revoked or superseded generations stay dead.

package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const (
	// DomainPoP prefixes every proof signature (distinct from DomainJWT, and from
	// the message-stamp domain ccsbx-stamp-v1).
	DomainPoP = "ccsbx-pop-v1"
	// ProofWindow is how far a proof's ts may be from the door's clock.
	ProofWindow = 120 * time.Second
	// NonceTTL is how long a door remembers a nonce (plan U5: 5-min cache).
	NonceTTL = 5 * time.Minute
	// ProofHeader carries the proof on every door request.
	ProofHeader = "X-Ccsbx-Proof"
	// MaxBody is the largest request body a door hashes for the proof.
	MaxBody = 32 << 20
)

// proof is the wire form: base64url(JSON). Method, path, door, token hash and body hash
// are not sent; the door signs-over what it actually received, so a proof made for one
// request, door, token or body cannot be moved to another.
type proof struct {
	TS    int64  `json:"ts"`
	Nonce string `json:"nonce"`
	Sig   string `json:"sig"`
}

func sha(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }

// proofInput is what the generation key signs. aud binds the proof to one door, ath to one
// token (as DPoP's `ath`), bh to one body (sha256 of the request body; of "" if none).
func proofInput(method, path, aud, tok string, body []byte, ts int64, nonce string) []byte {
	b, _ := json.Marshal(struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		Aud    string `json:"aud"`
		Ath    string `json:"ath"`
		BH     string `json:"bh"`
		TS     int64  `json:"ts"`
		Nonce  string `json:"nonce"`
	}{strings.ToUpper(method), path, aud, sha([]byte(tok)), sha(body), ts, nonce})
	return append([]byte(DomainPoP+"\x00"), b...)
}

// SignProof makes the ProofHeader value for one request to the door named aud. path is the
// request URI path plus raw query, exactly as the door will see it; body is the whole
// request body (nil if none).
func SignProof(g *GenKey, method, path, aud, tok string, body []byte, now time.Time) (string, error) {
	nb := make([]byte, 16)
	if _, err := rand.Read(nb); err != nil {
		return "", err
	}
	nonce := b64.EncodeToString(nb)
	sig := ed25519.Sign(g.Private, proofInput(method, path, aud, tok, body, now.Unix(), nonce))
	pb, _ := json.Marshal(proof{TS: now.Unix(), Nonce: nonce, Sig: b64.EncodeToString(sig)})
	return b64.EncodeToString(pb), nil
}

// Verifier is a door's token check. Safe for concurrent use.
type Verifier struct {
	ringMu sync.RWMutex
	ring   Keyring // swapped whole by SetRing; read through Ring()
	aud    string

	mu     sync.Mutex
	nonces map[string]time.Time // nonce → expiry
	// State is keyed by owner/machine/sid: the same sid string under another machine is a
	// different session, so one machine's key can't move another's generation counter.
	maxGen  map[string]int
	revoked map[string]int // generations ≤ this are revoked

	statePath string
	lock      *os.File // held for the life of the process: one door per state file
	stateErr  error    // set when the last save failed; the door refuses requests until it clears
}

// Ring is the keyring in force now. A door built on the trusted-key list (M9) swaps it with SetRing, so
// a removed key stops verifying at the next request. Callers must not change the returned map.
func (v *Verifier) Ring() Keyring { v.ringMu.RLock(); defer v.ringMu.RUnlock(); return v.ring }

// SetRing replaces the keyring. A nil or empty ring makes every token fail, which is the safe side.
func (v *Verifier) SetRing(r Keyring) { v.ringMu.Lock(); v.ring = r; v.ringMu.Unlock() }

// NewVerifier builds a verifier for one door. aud is the door's name
// (for example "ccsbx-model"); tokens must list it and proofs are bound to it.
func NewVerifier(ring Keyring, aud string) *Verifier {
	return &Verifier{ring: ring, aud: aud, nonces: map[string]time.Time{},
		maxGen: map[string]int{}, revoked: map[string]int{}}
}

func sessKey(owner, machine, sid string) string { return owner + "/" + machine + "/" + sid }

// Revoke is hostd's revoke{sid,gen} at generation end: that generation and all earlier ones
// are dead (T:token-after-exit). It returns an error if the revocation could not be made
// durable; the door then refuses every request until it can (fail closed, invariant 17).
func (v *Verifier) Revoke(owner, machine, sid string, gen int) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	k := sessKey(owner, machine, sid)
	if cur, ok := v.revoked[k]; !ok || gen > cur {
		v.revoked[k] = gen
	}
	if cur, ok := v.maxGen[k]; !ok || gen > cur {
		v.maxGen[k] = gen
	}
	return v.saveLocked()
}

// MaxGen returns the persisted high-water mark for a session (-1 if unseen).
func (v *Verifier) MaxGen(owner, machine, sid string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	if g, ok := v.maxGen[sessKey(owner, machine, sid)]; ok {
		return g
	}
	return -1
}

// Verify checks token + proof for one request to this door. Order matters: the cheap
// signature checks come first, and the nonce is recorded only after everything else
// passed, so garbage can't fill the cache.
func (v *Verifier) Verify(tok, proofHdr, method, path string, body []byte, now time.Time) (*Claims, error) {
	ring := v.Ring() // one snapshot for this request
	c, err := parse(tok, ring)
	if err != nil {
		return nil, err
	}
	owner, machine, sid, gen, err := c.SubParts()
	if err != nil {
		return nil, err
	}
	// Invariant 27: the signing key must be allowed to speak for this owner and machine.
	if sc := ring[c.kid]; sc.Permits(c) != nil { // the same rule the signer applies
		return nil, ErrKeyScope
	}
	if err := c.validBilling(); err != nil { // invariant 18, checked again at the door
		return nil, err
	}
	if c.Exp-c.Iat > int64(MaxLifetime/time.Second) {
		return nil, ErrLifetime
	}
	if now.Unix() >= c.Exp {
		return nil, ErrExpired
	}
	if c.Iat > now.Add(Skew).Unix() {
		return nil, ErrNotYet
	}
	okAud := false
	for _, a := range c.Aud {
		if a == v.aud {
			okAud = true
		}
	}
	if !okAud {
		return nil, ErrAudience
	}

	// Proof of possession.
	if proofHdr == "" {
		return nil, ErrNoProof
	}
	pb, err := b64.DecodeString(proofHdr)
	if err != nil {
		return nil, ErrBadProof
	}
	var p proof
	if err := json.Unmarshal(pb, &p); err != nil || p.Nonce == "" {
		return nil, ErrBadProof
	}
	cnf, err := b64.DecodeString(c.Cnf)
	if err != nil || len(cnf) != ed25519.PublicKeySize {
		return nil, ErrBadProof
	}
	sig, err := b64.DecodeString(p.Sig)
	if err != nil || !ed25519.Verify(ed25519.PublicKey(cnf), proofInput(method, path, v.aud, tok, body, p.TS, p.Nonce), sig) {
		return nil, ErrBadProof
	}
	if d := now.Unix() - p.TS; d > int64(ProofWindow/time.Second) || -d > int64(ProofWindow/time.Second) {
		return nil, ErrProofStale
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if v.stateErr != nil { // a door that can't remember revocations must not serve
		if err := v.saveLocked(); err != nil {
			return nil, ErrState
		}
	}
	k := sessKey(owner, machine, sid)
	if r, ok := v.revoked[k]; ok && gen <= r {
		return nil, ErrGenRevoked
	}
	if m, ok := v.maxGen[k]; ok && gen < m { // a newer generation exists (T:token-gen-bump)
		return nil, ErrGenRevoked
	}
	for n, exp := range v.nonces { // prune
		if now.After(exp) {
			delete(v.nonces, n)
		}
	}
	key := c.Cnf + "." + p.Nonce
	if _, seen := v.nonces[key]; seen {
		return nil, ErrProofReplay
	}
	if cur, ok := v.maxGen[k]; !ok || gen > cur {
		v.maxGen[k] = gen
		if err := v.saveLocked(); err != nil {
			return nil, ErrState
		}
	}
	v.nonces[key] = now.Add(NonceTTL)
	return c, nil
}

// PersistTo makes max_gen and revocations survive a door restart (plan U5:
// "persist max_gen[sid]"). It loads the file if present and rewrites it
// atomically (temp file, fsync, rename, fsync of the directory) on every change.
// Without it, state is in memory only.
func (v *Verifier) PersistTo(path string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	lock, err := lockState(path)
	if err != nil {
		return err
	}
	v.lock, v.statePath = lock, path
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return v.saveLocked() // prove now that we can write, not at the first revocation
	}
	if err != nil {
		return err
	}
	var st persisted
	if err := json.Unmarshal(b, &st); err != nil {
		return err
	}
	for k, g := range st.MaxGen {
		v.maxGen[k] = g
	}
	for k, g := range st.Revoked {
		v.revoked[k] = g
	}
	return nil
}

type persisted struct {
	MaxGen  map[string]int `json:"max_gen"`
	Revoked map[string]int `json:"revoked"`
}

// saveLocked writes the state durably, or records why it could not.
func (v *Verifier) saveLocked() error {
	if v.statePath == "" {
		return nil
	}
	err := writeDurable(v.statePath, v.maxGen, v.revoked)
	v.stateErr = err
	if err != nil {
		return errors.Join(ErrState, err)
	}
	return nil
}

func writeDurable(path string, maxGen, revoked map[string]int) error {
	b, _ := json.Marshal(persisted{maxGen, revoked})
	// A unique temp name: two writers can never rename each other's half-written file into place.
	f, err := os.CreateTemp(filepath.Dir(path), filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmp := f.Name()
	if _, err := f.Write(b); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		os.Remove(tmp)
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
