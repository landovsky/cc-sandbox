// revoke.go is the door-side endpoint for hostd's revoke{sid,gen}, and the signed request that
// calls it. It exists so T:token-after-exit has a real transport: when a generation ends, a token
// that leaked must die at every door at once, and only the machine's own key may say so.
package token

import (
	"crypto/ed25519"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

const (
	// DomainRevoke prefixes revoke signatures; a token or proof signature can never double as one.
	DomainRevoke = "ccsbx-revoke-v1"
	// RevokeHeader carries "<kid>.<unix ts>.<base64 signature>" over the request body.
	RevokeHeader = "X-Ccsbx-Revoke"
	// RevokePath is served on a door's OWNER listener only, never behind the forwarder (the
	// forwarder's allowlist has no route to it, and "revoke" is a denied segment).
	RevokePath = "/_ccsbx/revoke"
)

// RevokeBody is what hostd sends: that generation and all earlier ones of the session are dead.
type RevokeBody struct {
	Owner   string `json:"owner"`
	Machine string `json:"machine"`
	Sid     string `json:"sid"`
	Gen     int    `json:"gen"`
}

func revokeInput(ts int64, body []byte) []byte {
	return append([]byte(DomainRevoke+"\x00"+strconv.FormatInt(ts, 10)+"\x00"), body...)
}

// SignRevoke makes the RevokeHeader value for body with a machine key.
func SignRevoke(k *Key, body []byte, now time.Time) string {
	ts := now.Unix()
	return k.KID + "." + strconv.FormatInt(ts, 10) + "." + b64.EncodeToString(ed25519.Sign(k.Private, revokeInput(ts, body)))
}

// RevokeHandler serves RevokePath for this verifier. The signing key must be in the door's keyring
// and scoped to the owner and machine named in the body, like a token's key (invariant 27).
func (v *Verifier) RevokeHandler(now func() time.Time) http.Handler {
	if now == nil {
		now = time.Now
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != RevokePath {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 4096))
		if err != nil || v.checkRevoke(r.Header.Get(RevokeHeader), body, now()) != nil {
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		var b RevokeBody
		if json.Unmarshal(body, &b) != nil || b.Sid == "" {
			http.Error(w, "bad request", http.StatusBadRequest)
			return
		}
		if err := v.Revoke(b.Owner, b.Machine, b.Sid, b.Gen); err != nil {
			http.Error(w, "state not durable", http.StatusServiceUnavailable) // fail closed: not revoked, caller retries
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
}

func (v *Verifier) checkRevoke(hdr string, body []byte, now time.Time) error {
	parts := strings.SplitN(hdr, ".", 3)
	if len(parts) != 3 {
		return ErrBadProof
	}
	sc, ok := v.Ring()[parts[0]]
	ts, terr := strconv.ParseInt(parts[1], 10, 64)
	sig, serr := b64.DecodeString(parts[2])
	var b RevokeBody
	if !ok || terr != nil || serr != nil || json.Unmarshal(body, &b) != nil {
		return ErrBadProof
	}
	if !sc.allows(b.Owner, b.Machine) {
		return ErrKeyScope
	}
	if !ed25519.Verify(sc.Pub, revokeInput(ts, body), sig) {
		return ErrBadProof
	}
	if d := now.Unix() - ts; d > int64(ProofWindow/time.Second) || -d > int64(ProofWindow/time.Second) {
		return ErrProofStale
	}
	return nil
}
