package token

import (
	"context"
	"net/http"
	"strings"
	"time"
)

type ctxKey struct{}

// ClaimsFrom returns the verified claims Middleware stored on the request.
func ClaimsFrom(r *http.Request) *Claims { c, _ := r.Context().Value(ctxKey{}).(*Claims); return c }

// Middleware is the door-side gate: Authorization: Bearer <token> plus the
// X-Ccsbx-Proof header, checked against the method and RequestURI the door
// actually received. Every failure is a bare 401 (the reason goes to logf only,
// so a caller learns nothing about which check failed).
func (v *Verifier) Middleware(next http.Handler, logf func(format string, a ...any)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		c, err := v.Verify(tok, r.Header.Get(ProofHeader), r.Method, r.URL.RequestURI(), time.Now())
		if err != nil {
			if logf != nil {
				logf("token refused: %v (%s %s)", err, r.Method, r.URL.Path)
			}
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r.WithContext(withClaims(r, c)))
	})
}

func withClaims(r *http.Request, c *Claims) context.Context {
	return context.WithValue(r.Context(), ctxKey{}, c)
}
