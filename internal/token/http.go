package token

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strings"
	"time"
)

type ctxKey struct{}

// ClaimsFrom returns the verified claims Middleware stored on the request.
func ClaimsFrom(r *http.Request) *Claims { c, _ := r.Context().Value(ctxKey{}).(*Claims); return c }

// Middleware is the door-side gate: Authorization: Bearer <token> plus the X-Ccsbx-Proof
// header, checked against the method, RequestURI and body the door actually received. Every
// failure is a bare 401 (the reason goes to logf only, so a caller learns nothing about which
// check failed). On success the session JWT and the proof are removed from the request before
// the next handler sees it, so a door that proxies upstream can't forward them by accident.
func (v *Verifier) Middleware(next http.Handler, logf func(format string, a ...any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fail := func(code int, err error) {
			if logf != nil {
				logf("token refused: %v (%s %s)", err, r.Method, r.URL.Path)
			}
			http.Error(w, http.StatusText(code), code)
		}
		var body []byte
		if r.Body != nil {
			var err error
			if body, err = io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody)); err != nil {
				fail(http.StatusRequestEntityTooLarge, err)
				return
			}
		}
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		c, err := v.Verify(tok, r.Header.Get(ProofHeader), r.Method, r.URL.RequestURI(), body, time.Now())
		if err != nil {
			fail(http.StatusUnauthorized, err)
			return
		}
		r.Header.Del("Authorization")
		r.Header.Del(ProofHeader)
		r.Body = io.NopCloser(bytes.NewReader(body))
		next.ServeHTTP(w, r.WithContext(context.WithValue(r.Context(), ctxKey{}, c)))
	})
}
