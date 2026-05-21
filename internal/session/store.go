// Package session provides the short-lived token store that backs the
// nsdev-registry handshake protocol.
//
// Background: nsdev-registry's QUIC listener serves OCI Distribution v2 over
// HTTP/3 (see registry/quicserver.go). Because the cluster has no externally
// reachable TCP HTTP API — operators always come in via ssh through the
// bastion — clients cannot just present a long-lived client certificate over
// QUIC: they have to first prove their identity over a path that is gated by
// ssh.
//
// The handshake flow is:
//
//	1. Operator runs `ssh <bastion> nsdev-registry session ...` (subcommand
//	   defined in cmd/registry/session.go). The subcommand reads $SSH_USER /
//	   $SSH_CONNECTION from its own env, then makes an HTTP POST to
//	   localhost:<tcp>/v3/sessions with the operator's identity + the
//	   operator-side UDP probe endpoint.
//	2. The session handler (registry/handlers/session.go) issues a 32-byte
//	   random bearer token, stores it here with a short TTL (30 min default),
//	   tells the QUIC listener to fire NAT-punching probes at the operator's
//	   UDP endpoint, and returns JSON containing the token + the server's
//	   externally reachable UDP address.
//	3. The subcommand prints the JSON to its stdout, which the operator's
//	   `nsdev-push` reads via the ssh stdout pipe.
//	4. nsdev-push opens a direct QUIC connection to the advertised UDP
//	   address from the same local UDP socket it used for the probe, sends
//	   `Authorization: Bearer <token>` on every OCI request, and pushes
//	   blobs at full UDP/QUIC speed — no more ssh in the data path.
//
// This file implements ONLY the token storage piece. The HTTP handler, the
// punching glue, and the subcommand live in their respective packages.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

// DefaultTTL is the lifetime applied to a token if Issue is called with a
// non-positive ttl. 30 minutes is long enough for a multi-GB push over QUIC
// at hundreds of MB/s, short enough that a leaked token is useful for only
// one push cycle.
const DefaultTTL = 30 * time.Minute

// Token is a short-lived bearer credential. It is opaque to the client.
type Token string

// Entry is the server-side metadata bound to a Token.
type Entry struct {
	// User is the operator identity learned from $SSH_USER on the server
	// side of the handshake. It is purely informational on the data path
	// (the OCI handler doesn't gate on it for now); the audit log carries
	// it so we know who pushed what.
	User string
	// ClientAddr is the operator's UDP endpoint (as it appears to the
	// server, i.e. post-NAT). The QUIC listener sends NAT-punching probes
	// to this address and the subsequent QUIC ClientHello is expected to
	// come from the same (or a NAT-equivalent) 5-tuple.
	ClientAddr string
	// IssuedAt and ExpiresAt frame the validity window.
	IssuedAt  time.Time
	ExpiresAt time.Time
}

// Store keeps issued tokens in memory. Restarting the registry invalidates
// every outstanding token — operators just re-handshake, no persistence
// needed.
type Store struct {
	mu      sync.RWMutex
	entries map[Token]Entry
}

// NewStore returns an empty in-memory store.
func NewStore() *Store {
	return &Store{entries: make(map[Token]Entry)}
}

// Issue generates a fresh random token (32 bytes of entropy, hex-encoded)
// bound to the given user + client address with the given TTL. ttl <= 0
// falls back to DefaultTTL.
func (s *Store) Issue(user, clientAddr string, ttl time.Duration) (Token, Entry, error) {
	if user == "" {
		return "", Entry{}, errors.New("session: empty user")
	}
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", Entry{}, err
	}
	tok := Token(hex.EncodeToString(raw[:]))
	now := time.Now()
	e := Entry{
		User:       user,
		ClientAddr: clientAddr,
		IssuedAt:   now,
		ExpiresAt:  now.Add(ttl),
	}
	s.mu.Lock()
	s.entries[tok] = e
	s.mu.Unlock()
	return tok, e, nil
}

// Verify returns the Entry bound to tok if it exists AND is not expired.
// Expired entries are removed lazily on verify; a periodic Reap goroutine
// keeps the map from growing without bound when nobody verifies.
func (s *Store) Verify(tok Token) (Entry, bool) {
	s.mu.RLock()
	e, ok := s.entries[tok]
	s.mu.RUnlock()
	if !ok {
		return Entry{}, false
	}
	if time.Now().After(e.ExpiresAt) {
		s.mu.Lock()
		delete(s.entries, tok)
		s.mu.Unlock()
		return Entry{}, false
	}
	return e, true
}

// Revoke drops tok immediately. Idempotent.
func (s *Store) Revoke(tok Token) {
	s.mu.Lock()
	delete(s.entries, tok)
	s.mu.Unlock()
}

// Reap drops every expired entry. Call from a periodic goroutine; the
// caller picks the interval (15s is sensible for DefaultTTL=30min).
func (s *Store) Reap() int {
	now := time.Now()
	var dropped int
	s.mu.Lock()
	for t, e := range s.entries {
		if now.After(e.ExpiresAt) {
			delete(s.entries, t)
			dropped++
		}
	}
	s.mu.Unlock()
	return dropped
}

// Len returns the current count of live entries (best-effort; expired
// entries that haven't been reaped yet still count). Used for metrics /
// debugging.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}
