package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"os"
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
)

// proof is the wire form: base64url(JSON). The method and path are not sent;
// the door signs-over what it actually received, so a proof for one request
// cannot be moved to another.
type proof struct {
	TS    int64  `json:"ts"`
	Nonce string `json:"nonce"`
	Sig   string `json:"sig"`
}

func proofInput(method, path string, ts int64, nonce string) []byte {
	b, _ := json.Marshal(struct {
		Method string `json:"method"`
		Path   string `json:"path"`
		TS     int64  `json:"ts"`
		Nonce  string `json:"nonce"`
	}{strings.ToUpper(method), path, ts, nonce})
	return append([]byte(DomainPoP+"\x00"), b...)
}

// SignProof makes the ProofHeader value for one request. path is the request
// URI path plus raw query, exactly as the door will see it.
func SignProof(g *GenKey, method, path string, now time.Time) (string, error) {
	nb := make([]byte, 16)
	if _, err := rand.Read(nb); err != nil {
		return "", err
	}
	nonce := b64.EncodeToString(nb)
	sig := ed25519.Sign(g.Private, proofInput(method, path, now.Unix(), nonce))
	pb, _ := json.Marshal(proof{TS: now.Unix(), Nonce: nonce, Sig: b64.EncodeToString(sig)})
	return b64.EncodeToString(pb), nil
}

// Verifier is a door's token check. Safe for concurrent use.
type Verifier struct {
	ring Keyring
	aud  string

	mu      sync.Mutex
	nonces  map[string]time.Time // nonce → expiry
	maxGen  map[string]int       // sid → highest generation ever seen or revoked
	revoked map[string]int       // sid → generations ≤ this are revoked

	statePath string
}

// NewVerifier builds a verifier for one door. aud is the door's name
// (for example "ccsbx-model"); tokens must list it.
func NewVerifier(ring Keyring, aud string) *Verifier {
	return &Verifier{ring: ring, aud: aud, nonces: map[string]time.Time{},
		maxGen: map[string]int{}, revoked: map[string]int{}}
}

// Revoke is hostd's revoke{sid,gen} at generation end: that generation and all
// earlier ones are dead (T:token-after-exit).
func (v *Verifier) Revoke(sid string, gen int) {
	v.mu.Lock()
	defer v.mu.Unlock()
	if cur, ok := v.revoked[sid]; !ok || gen > cur {
		v.revoked[sid] = gen
	}
	if cur, ok := v.maxGen[sid]; !ok || gen > cur {
		v.maxGen[sid] = gen
	}
	v.saveLocked()
}

// MaxGen returns the persisted high-water mark for sid (-1 if unseen).
func (v *Verifier) MaxGen(sid string) int {
	v.mu.Lock()
	defer v.mu.Unlock()
	if g, ok := v.maxGen[sid]; ok {
		return g
	}
	return -1
}

// Verify checks token + proof for one request. Order matters: the cheap
// signature checks come first, and the nonce is recorded only after everything
// else passed, so garbage can't fill the cache.
func (v *Verifier) Verify(tok, proofHdr, method, path string, now time.Time) (*Claims, error) {
	c, err := parse(tok, v.ring)
	if err != nil {
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
	sid, gen := c.SID(), c.Gen()
	if sid == "" || gen < 0 {
		return nil, ErrSub
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
	if err != nil || !ed25519.Verify(ed25519.PublicKey(cnf), proofInput(method, path, p.TS, p.Nonce), sig) {
		return nil, ErrBadProof
	}
	if d := now.Unix() - p.TS; d > int64(ProofWindow/time.Second) || -d > int64(ProofWindow/time.Second) {
		return nil, ErrProofStale
	}

	v.mu.Lock()
	defer v.mu.Unlock()
	if r, ok := v.revoked[sid]; ok && gen <= r {
		return nil, ErrGenRevoked
	}
	if m, ok := v.maxGen[sid]; ok && gen < m { // a newer generation exists (T:token-gen-bump)
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
	v.nonces[key] = now.Add(NonceTTL)
	if cur, ok := v.maxGen[sid]; !ok || gen > cur {
		v.maxGen[sid] = gen
		v.saveLocked()
	}
	return c, nil
}

// PersistTo makes max_gen and revocations survive a door restart (plan U5:
// "persist max_gen[sid]"). It loads the file if present and rewrites it
// atomically on every change. Without it, state is in memory only.
func (v *Verifier) PersistTo(path string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.statePath = path
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
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

func (v *Verifier) saveLocked() {
	if v.statePath == "" {
		return
	}
	b, _ := json.Marshal(persisted{v.maxGen, v.revoked})
	tmp := v.statePath + ".tmp"
	if os.WriteFile(tmp, b, 0o600) == nil {
		os.Rename(tmp, v.statePath)
	}
}
