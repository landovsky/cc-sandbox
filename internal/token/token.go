// Package token mints and verifies ccsbx session tokens (build plan §3 U5).
//
// A session token is an EdDSA JWT signed over the domain prefix "ccsbx-jwt-v1"
// followed by the JWS signing input. The prefix means a signature made for any
// other purpose (message stamps, proofs) can never be replayed as a token.
//
// The token alone is not enough to call a door: its `cnf` claim holds the public
// half of the forwarder's per-generation key, and every request carries a proof
// (see proof.go) made with the private half. A copied token is useless without
// that key (T:token-copied).
//
// Stdlib only (the ccsp rule: zero third-party modules).
//
// Recipes:
//
//	key, _ := token.LoadOrCreateKeyFile(os.Getenv("CCSBX_KEY_FILE")) // dev key
//	gen, _ := token.NewGenKey()                                      // per generation
//	tok, _ := token.Mint(key, claims(gen.Public), now)               // hostd side
//	v := token.NewVerifier(token.Keyring{key.KID: key.Public}, "ccsbx-model")
//	c, err := v.Verify(tok, proofHeader, "POST", "/v1/messages", now) // door side
package token

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	// DomainJWT prefixes every token signature.
	DomainJWT = "ccsbx-jwt-v1"
	// MaxLifetime caps exp - iat (plan U5: exp ≤ 12 h).
	MaxLifetime = 12 * time.Hour
	// Skew tolerated on iat / exp.
	Skew = 60 * time.Second
)

// Errors. Doors map every one of them to 401; the text is for the audit log.
var (
	ErrMalformed   = errors.New("token: malformed")
	ErrAlg         = errors.New("token: alg must be EdDSA")
	ErrUnknownKID  = errors.New("token: unknown kid")
	ErrBadSig      = errors.New("token: bad signature")
	ErrExpired     = errors.New("token: expired")
	ErrNotYet      = errors.New("token: issued in the future")
	ErrLifetime    = errors.New("token: lifetime above 12h")
	ErrAudience    = errors.New("token: wrong audience")
	ErrGenRevoked  = errors.New("token: generation revoked or superseded")
	ErrNoProof     = errors.New("token: proof of possession missing")
	ErrBadProof    = errors.New("token: proof invalid")
	ErrProofStale  = errors.New("token: proof timestamp outside window")
	ErrProofReplay = errors.New("token: proof nonce already used")
	ErrSub         = errors.New("token: sub must be owner/machine/sid/g<gen>")
)

// Claims are the token body. Identity comes from the daemon only (invariant 1).
type Claims struct {
	Sub      string   `json:"sub"` // owner/machine/sid/g<gen>
	Label    string   `json:"label"`
	Billing  string   `json:"billing"` // api|subscription|stub
	Profile  string   `json:"profile"`
	Routes   []string `json:"routes,omitempty"`
	Repos    []string `json:"repos,omitempty"`
	Branch   string   `json:"branch,omitempty"`
	Aud      []string `json:"aud"`
	PlacedBy string   `json:"placed_by,omitempty"`
	Cnf      string   `json:"cnf"` // base64url ed25519 public key of the forwarder (this generation)
	Iat      int64    `json:"iat"`
	Exp      int64    `json:"exp"`
}

// SubParts splits Sub into owner, machine, sid and generation.
func (c *Claims) SubParts() (owner, machine, sid string, gen int, err error) {
	p := strings.Split(c.Sub, "/")
	if len(p) != 4 || p[0] == "" || p[1] == "" || p[2] == "" || !strings.HasPrefix(p[3], "g") {
		return "", "", "", 0, ErrSub
	}
	g, e := strconv.Atoi(p[3][1:])
	if e != nil || g < 0 {
		return "", "", "", 0, ErrSub
	}
	return p[0], p[1], p[2], g, nil
}

// SID returns "<sid>" from Sub (empty if malformed).
func (c *Claims) SID() string { _, _, s, _, _ := c.SubParts(); return s }

// Gen returns the generation from Sub (-1 if malformed).
func (c *Claims) Gen() int {
	_, _, _, g, e := c.SubParts()
	if e != nil {
		return -1
	}
	return g
}

// Key is a signing key plus its key id.
type Key struct {
	KID     string
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
}

// KIDOf is the first 8 bytes of sha256(public key), hex.
func KIDOf(pub ed25519.PublicKey) string {
	h := sha256.Sum256(pub)
	return hex.EncodeToString(h[:8])
}

// KeyFromSeed builds a Key from a 32-byte seed.
func KeyFromSeed(seed []byte) (*Key, error) {
	if len(seed) != ed25519.SeedSize {
		return nil, fmt.Errorf("token: seed must be %d bytes", ed25519.SeedSize)
	}
	priv := ed25519.NewKeyFromSeed(seed)
	pub := priv.Public().(ed25519.PublicKey)
	return &Key{KID: KIDOf(pub), Private: priv, Public: pub}, nil
}

// LoadOrCreateKeyFile reads the dev key file (base64 seed, mode 0600), creating
// it if absent. Direct mode only: under gate M the key lives behind the signer.
// A key file that other users can read is refused.
func LoadOrCreateKeyFile(path string) (*Key, error) {
	if path == "" {
		return nil, errors.New("token: no key file (set CCSBX_KEY_FILE or --key-file)")
	}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		enc := base64.StdEncoding.EncodeToString(seed) + "\n"
		if err := os.WriteFile(path, []byte(enc), 0o600); err != nil {
			return nil, err
		}
		return KeyFromSeed(seed)
	}
	if err != nil {
		return nil, err
	}
	st, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return nil, fmt.Errorf("token: key file %s is accessible by others (mode %o); chmod 600", path, st.Mode().Perm())
	}
	seed, err := base64.StdEncoding.DecodeString(strings.TrimSpace(string(b)))
	if err != nil {
		return nil, fmt.Errorf("token: key file %s: %w", path, err)
	}
	return KeyFromSeed(seed)
}

// GenKey is a forwarder's per-generation keypair. Its public half goes in cnf.
type GenKey struct {
	Private ed25519.PrivateKey
	Public  ed25519.PublicKey
}

// NewGenKey makes a fresh per-generation keypair.
func NewGenKey() (*GenKey, error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return nil, err
	}
	return &GenKey{Private: priv, Public: pub}, nil
}

// CnfOf encodes a public key for the cnf claim.
func CnfOf(pub ed25519.PublicKey) string { return b64.EncodeToString(pub) }

var b64 = base64.RawURLEncoding

type header struct {
	Alg string `json:"alg"`
	Typ string `json:"typ"`
	KID string `json:"kid"`
}

func signingInput(h, p string) []byte {
	return []byte(DomainJWT + "\x00" + h + "." + p)
}

// Mint signs claims. It stamps iat from now and checks the invariants a
// verifier would check, so a bad token is never issued.
func Mint(k *Key, c Claims, now time.Time) (string, error) {
	c.Iat = now.Unix()
	if c.Exp == 0 {
		c.Exp = now.Add(MaxLifetime).Unix()
	}
	if c.Exp-c.Iat > int64(MaxLifetime/time.Second) {
		return "", ErrLifetime
	}
	if c.Exp <= c.Iat {
		return "", ErrExpired
	}
	if _, _, _, _, err := c.SubParts(); err != nil {
		return "", err
	}
	if len(c.Aud) == 0 {
		return "", errors.New("token: aud required")
	}
	if pk, err := b64.DecodeString(c.Cnf); err != nil || len(pk) != ed25519.PublicKeySize {
		return "", errors.New("token: cnf must be a base64url ed25519 public key")
	}
	hb, _ := json.Marshal(header{Alg: "EdDSA", Typ: "JWT", KID: k.KID})
	pb, err := json.Marshal(c)
	if err != nil {
		return "", err
	}
	h, p := b64.EncodeToString(hb), b64.EncodeToString(pb)
	sig := ed25519.Sign(k.Private, signingInput(h, p))
	return h + "." + p + "." + b64.EncodeToString(sig), nil
}

// Keyring maps kid → public key (the machine keys a door trusts).
type Keyring map[string]ed25519.PublicKey

// parse checks structure, alg, kid and signature, and returns the claims.
func parse(tok string, ring Keyring) (*Claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, ErrMalformed
	}
	hb, err := b64.DecodeString(parts[0])
	if err != nil {
		return nil, ErrMalformed
	}
	var h header
	if err := json.Unmarshal(hb, &h); err != nil {
		return nil, ErrMalformed
	}
	if h.Alg != "EdDSA" { // never "none", never HS*: the alg is not negotiable
		return nil, ErrAlg
	}
	pub, ok := ring[h.KID]
	if !ok {
		return nil, ErrUnknownKID
	}
	sig, err := b64.DecodeString(parts[2])
	if err != nil || len(sig) != ed25519.SignatureSize {
		return nil, ErrBadSig
	}
	if !ed25519.Verify(pub, signingInput(parts[0], parts[1]), sig) {
		return nil, ErrBadSig
	}
	pb, err := b64.DecodeString(parts[1])
	if err != nil {
		return nil, ErrMalformed
	}
	var c Claims
	if err := json.Unmarshal(pb, &c); err != nil {
		return nil, ErrMalformed
	}
	return &c, nil
}

// B64 exposes the base64url (unpadded) codec used on the wire, for the forwarder.
func B64() *base64.Encoding { return b64 }
