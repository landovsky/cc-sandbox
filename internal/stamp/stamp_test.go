// Tests of the Go stamp. Why: it must be byte-compatible with the hub's Elixir stamp (a signature
// made by one is verified by the other), and each refusal (skew, replay, tampered kind or body, a
// wrong key, a malformed sig) must be what the hub says, because the receiver relies on it.
package stamp

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
	"time"
)

// A vector made by the hub's own SessionBus.Stamp.sign (test/support/stamp_sign.exs), with a fixed
// msg_id, nonce and ts: if the signing input ever drifts from the Elixir side this goes red.
const (
	vecPub  = "sIuLtRtiMrvgpjLGGvZ0gBvRkDozcxG9Fj8-sZ-fpkg"
	vecPriv = "fzQLDV8pTVJuFfiR9Z8-w-iAme9ksUNkB2K-POJFPuM"
	vecSig  = "v1.msgid12345.1700000000.nonce12345.nDaCcwv_QJySqZl3689CBPoP9BSJHQ2q_DcxXwSLDm_HJiYh6ZjblJM4JU6Rou-tDGigqPkgBKN-2f0pXcI8DA"
)

var vecCtx = Fields{From: "s0abc12345", To: "cc-sandbox:#3cvt", Kind: "report", Body: "hello fenced · x"}

func at(ts int64) time.Time { return time.Unix(ts, 0) }

func TestElixirSignatureVerifiesInGoAndGoSignsTheSameBytes(t *testing.T) {
	if _, err := Verify(vecPub, vecSig, vecCtx, at(1700000000), NewCache()); err != nil {
		t.Fatalf("the hub's own signature was refused: %v", err)
	}
	seed, _ := base64.RawURLEncoding.DecodeString(vecPriv)
	f := vecCtx
	f.MsgID, f.Nonce, f.TS = "msgid12345", "nonce12345", 1700000000
	if got := Sign(ed25519.NewKeyFromSeed(seed), f, at(0)); got != vecSig {
		t.Fatalf("Go signs different bytes than the hub:\n got %s\nwant %s", got, vecSig)
	}
}

func TestRefusalsAreTheHubsOwn(t *testing.T) {
	now := at(1700000000)
	cases := map[string]struct {
		mut  func(*Fields)
		sig  string
		pub  string
		when time.Time
		want error
	}{
		"kind changed after signing": {func(f *Fields) { f.Kind = "brief" }, vecSig, vecPub, now, ErrSignature},
		"body edited":                {func(f *Fields) { f.Body += "!" }, vecSig, vecPub, now, ErrSignature},
		"another recipient":          {func(f *Fields) { f.To = "cc-sandbox:#other" }, vecSig, vecPub, now, ErrSignature},
		"another sender":             {func(f *Fields) { f.From = "s0other999" }, vecSig, vecPub, now, ErrSignature},
		"5 minutes old":              {func(*Fields) {}, vecSig, vecPub, now.Add(5 * time.Minute), ErrSkew},
		"from the future":            {func(*Fields) {}, vecSig, vecPub, now.Add(-5 * time.Minute), ErrSkew},
		"no key":                     {func(*Fields) {}, vecSig, "", now, ErrNoKey},
		"a short key":                {func(*Fields) {}, vecSig, "AAAA", now, ErrNoKey},
		"a malformed sig":            {func(*Fields) {}, "v2.x", vecPub, now, ErrFormat},
		"a truncated signature":      {func(*Fields) {}, vecSig[:len(vecSig)-10], vecPub, now, ErrFormat},
	}
	for name, c := range cases {
		f := vecCtx
		c.mut(&f)
		if _, err := Verify(c.pub, c.sig, f, c.when, NewCache()); err != c.want {
			t.Errorf("%s: %v, want %v", name, err, c.want)
		}
	}
}

// context: a stamp is good once. The cache is entered only after the signature verified, so a bad
// signature does not use up a nonce, and a nonce is forgotten after 10 minutes.
func TestAStampIsGoodOnceAndABadOneDoesNotSpendTheNonce(t *testing.T) {
	c, now := NewCache(), at(1700000000)
	bad := vecCtx
	bad.Body = "tampered"
	if _, err := Verify(vecPub, vecSig, bad, now, c); err != ErrSignature {
		t.Fatal(err)
	}
	if _, err := Verify(vecPub, vecSig, vecCtx, now, c); err != nil {
		t.Fatalf("the nonce was spent by the forgery: %v", err)
	}
	if _, err := Verify(vecPub, vecSig, vecCtx, now, c); err != ErrReplay {
		t.Fatalf("replay: %v", err)
	}
}

func TestOurOwnKeyRoundTrips(t *testing.T) {
	pub, priv, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	sig := Sign(priv, vecCtx, now)
	if _, err := Verify(pub, sig, vecCtx, now, NewCache()); err != nil {
		t.Fatal(err)
	}
}

// context: the hub keeps a frame until the daemon polls. After an hour of hostd downtime a genuine stamp
// is an hour old by the daemon's clock; judged at the time the HUB received it, it is fine, and the
// nonce cache still ages by the daemon's own clock.
func TestSkewIsJudgedAtTheHubsReceiveTimeButTheCacheAgesByOurClock(t *testing.T) {
	recv := at(1700000000)
	later := recv.Add(time.Hour)
	if _, err := Verify(vecPub, vecSig, vecCtx, later, NewCache()); err != ErrSkew {
		t.Fatalf("by the daemon's clock it is an hour old: %v", err)
	}
	c := NewCache()
	if _, err := VerifyAt(vecPub, vecSig, vecCtx, recv, later, c); err != nil {
		t.Fatalf("judged at the hub's receive time it is good: %v", err)
	}
	if _, err := VerifyAt(vecPub, vecSig, vecCtx, recv, later.Add(time.Minute), c); err != ErrReplay {
		t.Fatalf("replay: %v", err)
	}
}

// context: the skew boundary at the receiver: just inside is good, just outside is refused.
func TestSkewBoundary(t *testing.T) {
	ts := at(1700000000)
	if _, err := Verify(vecPub, vecSig, vecCtx, ts.Add(Skew), NewCache()); err != nil {
		t.Errorf("exactly 120 s: %v", err)
	}
	if _, err := Verify(vecPub, vecSig, vecCtx, ts.Add(Skew+time.Second), NewCache()); err != ErrSkew {
		t.Errorf("121 s: %v", err)
	}
}

// context: the hub's decoder is strict; Go's default one skips line breaks and trailing bits, so a
// signature with a CR/LF inside would verify here and be refused there.
func TestAStampWithLineBreaksInItIsMalformed(t *testing.T) {
	bad := vecSig[:20] + "\n" + vecSig[20:]
	if _, err := Verify(vecPub, bad, vecCtx, at(1700000000), NewCache()); err != ErrFormat {
		t.Fatalf("%v", err)
	}
}
