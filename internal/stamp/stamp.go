// Package stamp is the signed message stamp of the session bus (B3, plan §3), in Go: the same bytes
// the hub's SessionBus.Stamp (Elixir) signs and verifies. It exists so hostd can sign what a fenced
// session sends across machines (M2b) and verify what arrives, instead of taking the hub's word:
//
//	{v, domain: ccsbx-stamp-v1, from, to, kind, msg_id, ts, nonce, sha256(body)}
//
// The sig string is "v1.<msg_id>.<ts>.<nonce>.<b64url Ed25519 signature>". A stamp is good for
// ±120 s and once: the receiver keeps {from, nonce} for 10 minutes, and enters it only AFTER the
// signature verified, so unsigned noise can't fill the cache.
package stamp

import (
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	Domain = "ccsbx-stamp-v1"
	// Skew is how far a stamp's ts may be from the verifier's clock.
	Skew = 120 * time.Second
	// NonceTTL is how long a verifier remembers a nonce.
	NonceTTL = 10 * time.Minute
)

var idRe = regexp.MustCompile(`^[A-Za-z0-9_-]{8,64}$`)

// Fields are what a stamp binds. From is the sender's sid, To the address as sent, Kind "" if none.
type Fields struct {
	From, To, Kind, Body string
	MsgID, Nonce         string
	TS                   int64
}

// Input is the exact byte string that is signed: length-prefixed fields, so nothing is ambiguous at
// the seams (the same as SessionBus.Stamp.signing_input).
func Input(f Fields) []byte {
	sum := sha256.Sum256([]byte(f.Body))
	parts := []string{"1", f.From, f.To, f.Kind, f.MsgID, strconv.FormatInt(f.TS, 10), f.Nonce, hex.EncodeToString(sum[:])}
	var sb strings.Builder
	sb.WriteString(Domain)
	sb.WriteByte(0)
	for _, p := range parts {
		sb.WriteString(strconv.Itoa(len(p)))
		sb.WriteByte(':')
		sb.WriteString(p)
	}
	return []byte(sb.String())
}

func randID() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		panic(err) // no entropy: nothing sensible can be signed
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// Sign makes the sig string; MsgID, Nonce and TS are filled when zero.
func Sign(priv ed25519.PrivateKey, f Fields, now time.Time) string {
	if f.MsgID == "" {
		f.MsgID = randID()
	}
	if f.Nonce == "" {
		f.Nonce = randID()
	}
	if f.TS == 0 {
		f.TS = now.Unix()
	}
	sig := ed25519.Sign(priv, Input(f))
	return fmt.Sprintf("v1.%s.%d.%s.%s", f.MsgID, f.TS, f.Nonce, base64.RawURLEncoding.EncodeToString(sig))
}

// Errors, one per way a stamp is refused (the names are the hub's: no_key, format, skew, signature, replay).
var (
	ErrNoKey     = errors.New("no_key")
	ErrFormat    = errors.New("format")
	ErrSkew      = errors.New("skew")
	ErrSignature = errors.New("signature")
	ErrReplay    = errors.New("replay")
)

// Cache remembers {from, nonce} pairs for NonceTTL.
type Cache struct {
	mu   sync.Mutex
	seen map[string]time.Time
}

// NewCache makes an empty cache.
func NewCache() *Cache { return &Cache{seen: map[string]time.Time{}} }

// claim is true the first time key is seen.
func (c *Cache) claim(key string, now time.Time) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k, t := range c.seen {
		if now.Sub(t) > NonceTTL {
			delete(c.seen, k)
		}
	}
	if _, ok := c.seen[key]; ok {
		return false
	}
	c.seen[key] = now
	return true
}

// Verify checks sig against the sender's public key (url-safe base64, 32 raw bytes) for a message
// whose stamped fields are ctx (From, To, Kind, Body). It returns the msg_id on success.
func Verify(pubB64, sig string, ctx Fields, now time.Time, cache *Cache) (string, error) {
	pub, err := base64.RawURLEncoding.DecodeString(pubB64)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return "", ErrNoKey
	}
	rest, ok := strings.CutPrefix(sig, "v1.")
	if !ok {
		return "", ErrFormat
	}
	p := strings.SplitN(rest, ".", 4)
	if len(p) != 4 || !idRe.MatchString(p[0]) || !idRe.MatchString(p[2]) {
		return "", ErrFormat
	}
	ts, err := strconv.ParseInt(p[1], 10, 64)
	if err != nil {
		return "", ErrFormat
	}
	raw, err := base64.RawURLEncoding.DecodeString(p[3])
	if err != nil || len(raw) != ed25519.SignatureSize {
		return "", ErrFormat
	}
	if d := now.Sub(time.Unix(ts, 0)); d > Skew || d < -Skew {
		return "", ErrSkew
	}
	ctx.MsgID, ctx.TS, ctx.Nonce = p[0], ts, p[2]
	if !ed25519.Verify(ed25519.PublicKey(pub), Input(ctx), raw) {
		return "", ErrSignature
	}
	if cache != nil && !cache.claim(ctx.From+"\x00"+ctx.Nonce, now) {
		return "", ErrReplay
	}
	return p[0], nil
}

// NewKey makes a per-generation keypair; the private half never leaves the process.
func NewKey() (pubB64 string, priv ed25519.PrivateKey, err error) {
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		return "", nil, err
	}
	return base64.RawURLEncoding.EncodeToString(pub), priv, nil
}
