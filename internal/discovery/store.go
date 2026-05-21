// Package discovery is the server-side half of the nsdev-registry STUN-less
// hole-punching protocol. The client sends a small UDP datagram with the
// magic prefix + a 32-byte random nonce DIRECTLY to the registry's QUIC
// port BEFORE it does the ssh-bridged session handshake. The
// quic.Transport's non-QUIC packet handler routes those datagrams here;
// each datagram is recorded as `nonce -> observed_src_addr` with a short
// TTL. When the operator's `nsdev-registry session` subcommand later POSTs
// to /v3/sessions with that nonce in its body, the handler swaps the
// client-supplied UDP address for whatever the kernel actually saw — that
// is, the post-NAT external (IP, port) pair, including any port that
// symmetric NAT chose to assign. This is what STUN buys you, just done
// inside the registry process via the same UDP socket the QUIC handshake
// will use a few hundred milliseconds later.
package discovery

import (
	"encoding/hex"
	"net"
	"sync"
	"time"
)

// MagicV1 is the discovery packet identifier. The packet layout is:
//
//	[0]      0x00            (matches non-QUIC packet detection in quic-go;
//	                          the first two bits are zero so quic-go's
//	                          Transport will hand the packet to
//	                          ReadNonQUICPacket rather than process it as a
//	                          QUIC long/short header)
//	[1..23]  "NSDEV-DISCOVERY-V1\n"  (22 ASCII bytes; appears in tcpdump
//	                                   so the protocol is self-documenting
//	                                   on the wire)
//	[23..55] nonce (32 random bytes — 256 bits of entropy is more than
//	                enough to defeat any practical guessing attack)
//
// Total packet size: 55 bytes. Fits in a single UDP datagram trivially.
const MagicV1 = "\x00NSDEV-DISCOVERY-V1\n"

// NonceSize is the byte length of the random nonce that uniquely
// identifies a discovery packet (and the resulting Store entry).
const NonceSize = 32

// PacketSize is the exact wire length of a v1 discovery packet.
const PacketSize = len(MagicV1) + NonceSize

// DefaultTTL governs how long a recorded entry stays in the store. The
// nsdev-push client typically follows up with the ssh-bridged handshake
// within 100-500 ms; 30 seconds is generous enough to absorb a slow ssh
// connection (e.g. through a high-latency bastion) without keeping the
// store full of stale entries.
const DefaultTTL = 30 * time.Second

// Entry is the post-NAT observation captured for one discovery nonce.
type Entry struct {
	// SrcAddr is the source UDP address as the registry's kernel saw it
	// when the discovery datagram arrived. For NATed clients this is
	// the externally reachable address that QUIC traffic from the same
	// client (using the same local socket) will also use.
	SrcAddr *net.UDPAddr
	// ObservedAt is the wall-clock time the packet was received.
	ObservedAt time.Time
	// ExpiresAt = ObservedAt + ttl.
	ExpiresAt time.Time
}

// Store maps discovery nonces to the observation that the QUIC listener
// recorded for them. Entries are dropped lazily on Lookup and eagerly
// by Reap.
type Store struct {
	ttl     time.Duration
	mu      sync.RWMutex
	entries map[string]Entry // key = hex(nonce)
}

// NewStore returns an empty store. ttl <= 0 substitutes DefaultTTL.
func NewStore(ttl time.Duration) *Store {
	if ttl <= 0 {
		ttl = DefaultTTL
	}
	return &Store{
		ttl:     ttl,
		entries: make(map[string]Entry, 16),
	}
}

// HandlePacket inspects one UDP packet read off the registry's QUIC
// socket and, if it matches the discovery wire format, records the
// observation in the store. Returns true if the packet was a discovery
// packet (whether or not the recording succeeded); the caller can use
// the return to keep metrics on how many non-QUIC packets are
// discovery vs noise.
func (s *Store) HandlePacket(buf []byte, src net.Addr) bool {
	if len(buf) != PacketSize {
		return false
	}
	if string(buf[:len(MagicV1)]) != MagicV1 {
		return false
	}
	udpAddr, ok := src.(*net.UDPAddr)
	if !ok {
		// quic-go always hands us *net.UDPAddr here, but be paranoid.
		return false
	}
	nonce := buf[len(MagicV1):]
	now := time.Now()
	s.mu.Lock()
	s.entries[hex.EncodeToString(nonce)] = Entry{
		SrcAddr:    udpAddr,
		ObservedAt: now,
		ExpiresAt:  now.Add(s.ttl),
	}
	s.mu.Unlock()
	return true
}

// Lookup returns the observation for the given hex-encoded nonce if one
// was recorded and is not yet expired. Lazy-deletes expired entries.
func (s *Store) Lookup(nonceHex string) (Entry, bool) {
	s.mu.RLock()
	e, ok := s.entries[nonceHex]
	s.mu.RUnlock()
	if !ok {
		return Entry{}, false
	}
	if time.Now().After(e.ExpiresAt) {
		s.mu.Lock()
		delete(s.entries, nonceHex)
		s.mu.Unlock()
		return Entry{}, false
	}
	return e, true
}

// Reap removes every expired entry. Call from a periodic GC goroutine
// (15 s is sensible for DefaultTTL = 30 s). Returns the number dropped.
func (s *Store) Reap() int {
	now := time.Now()
	var dropped int
	s.mu.Lock()
	for k, e := range s.entries {
		if now.After(e.ExpiresAt) {
			delete(s.entries, k)
			dropped++
		}
	}
	s.mu.Unlock()
	return dropped
}

// Len returns the live entry count (best-effort: expired-but-not-reaped
// entries still count). Useful for /metrics.
func (s *Store) Len() int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return len(s.entries)
}
