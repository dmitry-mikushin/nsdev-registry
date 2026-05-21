// Package tokenauth wraps an http.Handler with bearer-token verification
// against a session.Store. It is mounted on the QUIC (HTTP/3) listener
// so that data-path requests carry a token issued earlier through the
// TCP-bound /v3/sessions handshake — see internal/session for the
// rationale.
package tokenauth

import (
	"context"
	"net/http"
	"strings"

	"github.com/distribution/distribution/v3/internal/dcontext"
	"github.com/distribution/distribution/v3/internal/session"
)

// ContextKey is the type used for session-entry values attached to
// request contexts. Use Value below to access it.
type contextKey struct{}

// Value extracts the session.Entry that the middleware attached to ctx.
// Returns ok==false if no entry was set (e.g. the request bypassed the
// middleware entirely — should not happen on the QUIC handler).
func Value(ctx context.Context) (session.Entry, bool) {
	e, ok := ctx.Value(contextKey{}).(session.Entry)
	return e, ok
}

// Middleware returns an http.Handler that rejects every request without
// a valid `Authorization: Bearer <token>` header — the token must be
// present in the supplied store and not yet expired.
//
// Health-check / Distribution-version-probe requests on /v2/ are still
// gated: an attacker who can reach the QUIC port but has no token
// learns nothing about repo contents and gets a uniform 401. The TCP
// listener (where podman pull lives) keeps the registry's existing
// auth model (htpasswd, token-issuer, silly, etc.) — this middleware
// only wraps the QUIC mux.
func Middleware(store *session.Store, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := r.Header.Get("Authorization")
		if hdr == "" {
			unauthorised(w, "missing Authorization header")
			return
		}
		const prefix = "Bearer "
		if !strings.HasPrefix(hdr, prefix) {
			unauthorised(w, "Authorization header is not a Bearer token")
			return
		}
		tok := session.Token(strings.TrimSpace(hdr[len(prefix):]))
		entry, ok := store.Verify(tok)
		if !ok {
			unauthorised(w, "token unknown or expired")
			return
		}
		// Attach the resolved identity to the request context so the
		// audit-log middleware downstream can record who pushed what.
		ctx := context.WithValue(r.Context(), contextKey{}, entry)
		dcontext.GetLogger(ctx).Debugf(
			"tokenauth: %s %s by user=%q (client=%s)",
			r.Method, r.URL.Path, entry.User, entry.ClientAddr)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func unauthorised(w http.ResponseWriter, reason string) {
	w.Header().Set("WWW-Authenticate", `Bearer realm="nsdev-registry"`)
	http.Error(w, "tokenauth: "+reason, http.StatusUnauthorized)
}
