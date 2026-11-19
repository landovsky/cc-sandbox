// Package ulid mints ULIDs (26 Crockford base32 chars: 48-bit ms timestamp + 80 random bits).
// Within one process they are strictly increasing, so a journal id sorts in arrival order.
// Zero third-party modules, hence this small local copy.
package ulid

import (
	"crypto/rand"
	"sync"
	"time"
)

const alphabet = "0123456789ABCDEFGHJKMNPQRSTVWXYZ"

var (
	mu     sync.Mutex
	lastMs uint64
	last   [10]byte
)

// New returns a ULID for the current time.
func New() string { return At(time.Now()) }

// At returns a ULID for t. A second call in the same millisecond increments the random part,
// so the order is kept; t going backwards is clamped to the last timestamp.
func At(t time.Time) string {
	mu.Lock()
	defer mu.Unlock()
	ms := uint64(t.UnixMilli())
	if ms <= lastMs && lastMs != 0 {
		ms = lastMs
		for i := 9; i >= 0; i-- {
			last[i]++
			if last[i] != 0 {
				break
			}
		}
	} else {
		lastMs = ms
		if _, err := rand.Read(last[:]); err != nil {
			panic("ulid: no randomness: " + err.Error())
		}
	}
	var b [16]byte
	for i := 0; i < 6; i++ {
		b[i] = byte(ms >> (8 * (5 - i)))
	}
	copy(b[6:], last[:])
	return encode(b)
}

func encode(b [16]byte) string {
	// 128 bits -> 26 groups of 5 bits (the first group holds only 3 bits).
	var out [26]byte
	var acc uint
	var bits uint
	j := 25
	for i := 15; i >= 0; i-- {
		acc |= uint(b[i]) << bits
		bits += 8
		for bits >= 5 {
			out[j] = alphabet[acc&31]
			j--
			acc >>= 5
			bits -= 5
		}
	}
	if bits > 0 {
		out[j] = alphabet[acc&31]
	}
	return string(out[:])
}

// Valid reports whether s has the shape of a ULID.
func Valid(s string) bool {
	if len(s) != 26 || s[0] > '7' {
		return false
	}
	for i := 0; i < len(s); i++ {
		ok := false
		for j := 0; j < len(alphabet); j++ {
			if s[i] == alphabet[j] {
				ok = true
				break
			}
		}
		if !ok {
			return false
		}
	}
	return true
}
