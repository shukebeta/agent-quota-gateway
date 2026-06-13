package backend

import (
	"net/http"
	"strings"
)

// Middleware resolves the inbound selector to a backend and stores it on
// the request context for the proxy director and quota observer. It
// wraps only the proxy handler — the gateway's own /_gateway endpoints
// take no selector.
//
// The selector arrives as the Authorization bearer token: Claude Code
// puts ANTHROPIC_AUTH_TOKEN there, and here that value is a local
// backend name, not a credential. An unknown or missing selector fails
// closed with 403 and never reaches the upstream. The selector value is
// deliberately never logged or echoed — a misconfigured client could
// have put a real token there, and we must not leak it.
func Middleware(reg *Registry, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		selector := bearerToken(r.Header.Get("Authorization"))
		b, ok := reg.Resolve(selector)
		if !ok {
			writeForbidden(w)
			return
		}
		next.ServeHTTP(w, r.WithContext(WithBackend(r.Context(), b)))
	})
}

// bearerToken extracts the token from an "Authorization: Bearer <tok>"
// header value. The scheme is matched case-insensitively per RFC 7235.
// A header without the bearer scheme yields "", which Resolve rejects.
func bearerToken(authHeader string) string {
	const scheme = "bearer "
	if len(authHeader) < len(scheme) || !strings.EqualFold(authHeader[:len(scheme)], scheme) {
		return ""
	}
	return strings.TrimSpace(authHeader[len(scheme):])
}

// writeForbidden emits the fail-closed response. The body is generic on
// purpose: it names neither the rejected selector nor the set of valid
// nicks, so nothing about the gateway's configuration leaks to a client
// that guessed wrong.
func writeForbidden(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusForbidden)
	_, _ = w.Write([]byte(`{"error":"unknown backend selector"}`))
}
