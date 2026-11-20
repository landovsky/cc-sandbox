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
//	ring, _ := token.LoadPublicKeyring(path)                          // door side: public keys only
//	v := token.NewVerifier(ring, "ccsbx-model")
//	c, err := v.Verify(tok, proofHeader, "POST", "/v1/messages", body, now)
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
	"path"
	"sort"
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
	ErrKeyScope    = errors.New("token: signing key is not allowed to speak for this owner/machine")
	ErrState       = errors.New("token: door cannot persist revocation state; refusing")
	ErrClaim       = errors.New("token: billing must be api|subscription|stub and profile must be set")
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

	kid string // which key signed it; set by parse, never part of the token body
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
	return loadKeyFile(path, true)
}

// LoadKeyFile reads an existing dev key file and never creates one. Verifiers
// use it, so a typo in the path can't silently mint a new, empty trust root.
func LoadKeyFile(path string) (*Key, error) { return loadKeyFile(path, false) }

func loadKeyFile(path string, create bool) (*Key, error) {
	if path == "" {
		return nil, errors.New("token: no key file (set CCSBX_KEY_FILE or --key-file)")
	}
	b, err := os.ReadFile(path)
	if create && errors.Is(err, os.ErrNotExist) {
		seed := make([]byte, ed25519.SeedSize)
		if _, err := rand.Read(seed); err != nil {
			return nil, err
		}
		enc := base64.StdEncoding.EncodeToString(seed) + "\n"
		// O_EXCL: never follow a symlink planted at the path, and if another process created
		// the key a moment ago, use its key instead of overwriting it.
		f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
		if errors.Is(err, os.ErrExist) {
			return loadKeyFile(path, false)
		}
		if err != nil {
			return nil, err
		}
		_, werr := f.WriteString(enc)
		if cerr := f.Close(); werr == nil {
			werr = cerr
		}
		if werr != nil {
			return nil, werr
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
	if err := c.validBilling(); err != nil {
		return "", err
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

// validBilling is invariant 18: billing is one of three values and the profile is named, so no
// door ever picks a default. Mint and Verify both call it; the signer's own check can't be the
// only one, since a signer bug lives on the same side.
func (c *Claims) validBilling() error {
	if (c.Billing != "api" && c.Billing != "subscription" && c.Billing != "stub") || c.Profile == "" {
		return ErrClaim
	}
	return nil
}

// KeyScope is what one trusted key may sign for (invariant 27): sessions whose sub names an
// owner in Owners and a machine in Machines. An empty list allows nothing, so a scope can't be
// "forgotten" into allow-all. Profiles (glob patterns, e.g. "e2e-*") and Billings narrow it
// further: a Mac key limited to profiles=dev-* billings=api,stub can't sign a subscription
// session (T:mac-key-above-mac-max). A keyring file must state both for every key.
type KeyScope struct {
	Pub      ed25519.PublicKey
	Owners   []string
	Machines []string
	Profiles []string
	Billings []string
	// Pairs, when set, is the exact rule: a profile NAME (no globs) and the billings it may be signed
	// with. An unknown profile, or a billing outside its own list, is refused (invariant 18: nothing
	// switches a session onto another billing). The signer's scope always carries it.
	Pairs map[string][]string
}

// Permits reports whether this key may sign the claims c: the owner and machine of its sub, its
// profile and its billing (invariant 27). The signer uses it before it signs; doors use the same
// rule when they verify, so the two can't drift apart.
func (k KeyScope) Permits(c *Claims) error {
	owner, machine, _, _, err := c.SubParts()
	if err != nil {
		return err
	}
	if !k.allows(owner, machine) || !k.allowsClaims(c) {
		return ErrKeyScope
	}
	return nil
}

// allowsClaims is the profile and billing half of the scope check.
func (k KeyScope) allowsClaims(c *Claims) bool {
	if k.Pairs != nil {
		bs, ok := k.Pairs[c.Profile]
		if !ok || !contains(bs, c.Billing) {
			return false
		}
	}
	if k.Billings != nil {
		ok := false
		for _, b := range k.Billings {
			ok = ok || b == c.Billing
		}
		if !ok {
			return false
		}
	}
	if k.Profiles != nil {
		ok := false
		for _, pat := range k.Profiles {
			if m, err := path.Match(pat, c.Profile); err == nil && m {
				ok = true
			}
		}
		if !ok {
			return false
		}
	}
	return true
}

// PermitsRevoke reports whether this key may sign a revoke for a session of owner on machine: the same owner
// and machine rule as for a token (a revoke names no profile or billing).
func (k KeyScope) PermitsRevoke(owner, machine string) error {
	if !k.allows(owner, machine) {
		return ErrKeyScope
	}
	return nil
}

func (k KeyScope) allows(owner, machine string) bool {
	has := func(l []string, v string) bool {
		for _, x := range l {
			if x == v {
				return true
			}
		}
		return false
	}
	return has(k.Owners, owner) && has(k.Machines, machine)
}

// Keyring maps kid → the key and the scope it may sign within (the machine keys a door trusts).
type Keyring map[string]KeyScope

// LoadPublicKeyring reads a keyring file for a door. Lines: `<kid> <base64 public key>
// owners=a,b machines=x,y profiles=P,.. billings=B,..`; `#` starts a comment. Doors load only this, never a seed
// (invariant 12: only the signer holds signing keys). A line whose key is a 32-byte seed
// can't be told from a public key by length, so the kid must equal KIDOf(public key).
func LoadPublicKeyring(path string) (Keyring, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParsePublicKeyring(b, path)
}

// ParsePublicKeyring is LoadPublicKeyring on bytes already read; name is only for messages. The
// signer reads its scope file through an fd it has checked, then parses it here.
func ParsePublicKeyring(b []byte, path string) (Keyring, error) {
	ring := Keyring{}
	for i, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Fields(line)
		if len(f) < 4 {
			return nil, fmt.Errorf("token: keyring %s:%d: want `kid pubkey owners=… machines=…`", path, i+1)
		}
		pub, err := base64.StdEncoding.DecodeString(f[1])
		if err != nil || len(pub) != ed25519.PublicKeySize || KIDOf(pub) != f[0] {
			return nil, fmt.Errorf("token: keyring %s:%d: kid does not match the public key", path, i+1)
		}
		sc := KeyScope{Pub: pub}
		for _, kv := range f[2:] {
			k, v, _ := strings.Cut(kv, "=")
			switch k {
			case "owners":
				sc.Owners = splitList(v)
			case "machines":
				sc.Machines = splitList(v)
			case "profiles":
				if sc.Profiles = splitList(v); len(sc.Profiles) == 0 {
					return nil, fmt.Errorf("token: keyring %s:%d: profiles= is empty (leave it out to allow all)", path, i+1)
				}
			case "billings":
				if sc.Billings = splitList(v); len(sc.Billings) == 0 {
					return nil, fmt.Errorf("token: keyring %s:%d: billings= is empty (leave it out to allow all)", path, i+1)
				}
			case "pairs":
				p, err := ParsePairs(v)
				if err != nil {
					return nil, fmt.Errorf("token: keyring %s:%d: %w", path, i+1, err)
				}
				sc.Pairs = p
			default:
				return nil, fmt.Errorf("token: keyring %s:%d: unknown field %q", path, i+1, k)
			}
		}
		for _, pat := range sc.Profiles { // a typo'd glob would fail closed but silently
			if !validGlob(pat) {
				return nil, fmt.Errorf("token: keyring %s:%d: bad profile pattern %q", path, i+1, pat)
			}
		}
		if len(sc.Owners) == 0 || len(sc.Machines) == 0 {
			return nil, fmt.Errorf("token: keyring %s:%d: owners= and machines= are required", path, i+1)
		}
		// A key with no stated tier would sign any tier by omission, so both limits are required;
		// "profiles=*" says out loud that every profile is meant.
		if sc.Profiles == nil || sc.Billings == nil {
			return nil, fmt.Errorf("token: keyring %s:%d: profiles= and billings= are required (profiles=* for any)", path, i+1)
		}
		if _, dup := ring[f[0]]; dup {
			return nil, fmt.Errorf("token: keyring %s:%d: kid %s appears twice", path, i+1, f[0])
		}
		ring[f[0]] = sc
	}
	if len(ring) == 0 {
		return nil, fmt.Errorf("token: keyring %s is empty", path)
	}
	return ring, nil
}

// KeyringLine is the line a host writes for a key it has the seed of.
// It states the widest tier on purpose (any profile, any billing), for dev keys and tests; a real
// machine key is written with KeyringLineScoped and its own limits.
func (k *Key) KeyringLine(owners, machines []string) string {
	return k.KeyringLineScoped(owners, machines, []string{"*"}, []string{"api", "subscription", "stub"})
}

// KeyringLineScoped is the keyring line for a key limited to these profiles (globs) and billings.
func (k *Key) KeyringLineScoped(owners, machines, profiles, billings []string) string {
	line := fmt.Sprintf("%s %s owners=%s machines=%s", k.KID, base64.StdEncoding.EncodeToString(k.Public),
		strings.Join(owners, ","), strings.Join(machines, ","))
	if len(profiles) > 0 {
		line += " profiles=" + strings.Join(profiles, ",")
	}
	if len(billings) > 0 {
		line += " billings=" + strings.Join(billings, ",")
	}
	return line
}

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
	sc, ok := ring[h.KID]
	if !ok {
		return nil, ErrUnknownKID
	}
	pub := sc.Pub
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
	c.kid = h.KID
	return &c, nil
}

// B64 exposes the base64url (unpadded) codec used on the wire, for the forwarder.
func B64() *base64.Encoding { return b64 }

// Ring is a one-key Keyring for tests and the dev tools, scoped to the given owners/machines.
// Doors load their ring from a file (LoadPublicKeyring) and never from a Key.
func (k *Key) Ring(owners, machines []string) Keyring {
	return Keyring{k.KID: KeyScope{Pub: k.Public, Owners: owners, Machines: machines}}
}

// splitList splits a comma list and drops empty items ("a,,b" and "" must not yield "").
func splitList(v string) []string {
	var out []string
	for _, x := range strings.Split(v, ",") {
		if x = strings.TrimSpace(x); x != "" {
			out = append(out, x)
		}
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// ParsePairs reads "dev-default:api|stub,e2e-default:stub": exact profile names, each with the
// billings it may be signed with. Names are letters, digits, '-', '_' and '.', so a glob or a
// separator can't hide in one; billings must be api, subscription or stub.
func ParsePairs(s string) (map[string][]string, error) {
	out := map[string][]string{}
	for _, item := range splitList(s) {
		name, bs, ok := strings.Cut(item, ":")
		if !ok || name == "" || bs == "" {
			return nil, fmt.Errorf("pairs: %q is not profile:billing|billing", item)
		}
		if !ValidName(name) {
			return nil, fmt.Errorf("pairs: profile name %q has a character outside [A-Za-z0-9._-]", name)
		}
		if _, dup := out[name]; dup {
			return nil, fmt.Errorf("pairs: profile %q listed twice", name)
		}
		for _, b := range strings.Split(bs, "|") {
			if b != "api" && b != "subscription" && b != "stub" {
				return nil, fmt.Errorf("pairs: %q: billing %q is not api, subscription or stub", item, b)
			}
			out[name] = append(out[name], b)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("pairs: empty")
	}
	return out, nil
}

// KeyringLineWithPairs is the scope line the signer writes for its own key: owners, machines and
// the exact profile→billings pairs. profiles= and billings= are derived (the names, and the union of
// their billings) for readers that look at those fields. A door built before pairs= existed rejects
// the line as having an unknown field; only doors from this change on read it, so they must be
// updated before they are given a signer's scope.
func (k *Key) KeyringLineWithPairs(owners, machines []string, pairs string) (string, error) {
	p, err := ParsePairs(pairs)
	if err != nil {
		return "", err
	}
	var names, union []string
	for _, item := range splitList(pairs) { // keep the caller's order
		name, _, _ := strings.Cut(item, ":")
		names = append(names, name)
		for _, b := range p[name] {
			if !contains(union, b) {
				union = append(union, b)
			}
		}
	}
	return k.KeyringLineScoped(owners, machines, names, union) + " pairs=" + pairs, nil
}

func validGlob(pat string) bool { _, err := path.Match(pat, ""); return err == nil }

// ValidName is the one rule for names in a scope (profiles, owners, machines): letters, digits,
// '.', '_' and '-', so no glob, separator, space or '=' can hide in one.
func ValidName(n string) bool {
	if n == "" {
		return false
	}
	for _, r := range n {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-' || r == '_' || r == '.') {
			return false
		}
	}
	return true
}

// UnverifiedClaims decodes a token's payload WITHOUT checking its signature. The sidecar uses it to
// sanity-check what the launcher handed it (the expiry, that the generation key matches cnf) before
// serving; the doors still verify every request. Never use the result to decide what a caller may do.
func UnverifiedClaims(tok string) (*Claims, error) {
	parts := strings.Split(tok, ".")
	if len(parts) != 3 {
		return nil, ErrMalformed
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

// ValidBilling reports whether b is one of the three billings (invariant 18). The trusted-key list
// uses it so a machine file can't name a fourth.
func ValidBilling(b string) bool { return b == "api" || b == "subscription" || b == "stub" }

// PairsString writes pairs back in the keyring's `profile:billing|billing,...` form, sorted, so the
// same pairs always give the same line.
func PairsString(p map[string][]string) string {
	names := make([]string, 0, len(p))
	for n := range p {
		names = append(names, n)
	}
	sort.Strings(names)
	items := make([]string, 0, len(names))
	for _, n := range names {
		items = append(items, n+":"+strings.Join(p[n], "|"))
	}
	return strings.Join(items, ",")
}
