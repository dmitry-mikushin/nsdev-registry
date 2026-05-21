package tokenauth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/distribution/distribution/v3/internal/session"
)

// nextStub records whether the next handler was invoked and what
// session.Entry it saw on the request context. Lets each test assert
// "middleware called next with the right identity" without spinning up
// the full distribution App.
type nextStub struct {
	called bool
	entry  session.Entry
	ok     bool
}

func (n *nextStub) handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n.called = true
		n.entry, n.ok = Value(r.Context())
		w.WriteHeader(http.StatusNoContent)
	})
}

func newRequestWithAuth(auth string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, "/v2/", nil)
	if auth != "" {
		r.Header.Set("Authorization", auth)
	}
	return r
}

func TestMiddleware_MissingHeaderRejected(t *testing.T) {
	store := session.NewStore()
	next := &nextStub{}
	h := Middleware(store, next.handler())

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequestWithAuth(""))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", w.Code)
	}
	if got := w.Header().Get("WWW-Authenticate"); got == "" {
		t.Errorf("WWW-Authenticate header missing")
	}
	if next.called {
		t.Errorf("next must not be called when auth is missing")
	}
}

func TestMiddleware_MalformedHeaderRejected(t *testing.T) {
	store := session.NewStore()
	next := &nextStub{}
	h := Middleware(store, next.handler())

	for _, bad := range []string{
		"Basic dXNlcjpwYXNz",            // wrong scheme
		"bearer lowercase-prefix",       // case-sensitive per RFC 6750
		"Bearer",                        // no token
		"BearerNoSpace123",              // glued
	} {
		t.Run(bad, func(t *testing.T) {
			next.called = false
			w := httptest.NewRecorder()
			h.ServeHTTP(w, newRequestWithAuth(bad))
			if w.Code != http.StatusUnauthorized {
				t.Errorf("auth=%q -> %d, want 401", bad, w.Code)
			}
			if next.called {
				t.Errorf("auth=%q must not reach next", bad)
			}
		})
	}
}

func TestMiddleware_UnknownTokenRejected(t *testing.T) {
	store := session.NewStore()
	next := &nextStub{}
	h := Middleware(store, next.handler())

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequestWithAuth("Bearer 0123456789abcdef"))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", w.Code)
	}
	if next.called {
		t.Errorf("next must not be called for unknown token")
	}
}

func TestMiddleware_ExpiredTokenRejected(t *testing.T) {
	store := session.NewStore()
	tok, _, err := store.Issue("bob", "10.0.0.2:9", time.Nanosecond)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(10 * time.Millisecond) // crosses the 1ns TTL

	next := &nextStub{}
	h := Middleware(store, next.handler())

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequestWithAuth("Bearer "+string(tok)))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("status=%d, want 401", w.Code)
	}
	if next.called {
		t.Errorf("next must not be called for an expired token")
	}
}

func TestMiddleware_ValidTokenForwardsWithIdentity(t *testing.T) {
	store := session.NewStore()
	tok, entry, err := store.Issue("alice", "10.0.0.1:1", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	next := &nextStub{}
	h := Middleware(store, next.handler())

	w := httptest.NewRecorder()
	h.ServeHTTP(w, newRequestWithAuth("Bearer "+string(tok)))

	if w.Code != http.StatusNoContent {
		t.Errorf("status=%d, want 204", w.Code)
	}
	if !next.called {
		t.Fatal("next must be called for a valid token")
	}
	if !next.ok {
		t.Fatal("session.Entry must be attached to request context")
	}
	if next.entry.User != entry.User {
		t.Errorf("forwarded entry User=%q, want %q",
			next.entry.User, entry.User)
	}
}

func TestValue_AbsentReturnsNotOK(t *testing.T) {
	// Direct unit test of the context accessor — if a request bypasses
	// the middleware (e.g. health endpoint mounted before it), Value
	// must report ok=false rather than returning a zero Entry that the
	// caller treats as valid.
	if _, ok := Value(context.Background()); ok {
		t.Fatal("Value on plain context must return ok=false")
	}
}
