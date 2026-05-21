package discovery

import (
	"net"
	"testing"
	"time"
)

// fakeAddr lets us craft non-*UDPAddr addresses to exercise the
// type-assert branch in HandlePacket without setting up a real socket.
type fakeAddr struct{ s string }

func (f fakeAddr) Network() string { return "fake" }
func (f fakeAddr) String() string  { return f.s }

func newUDP(t *testing.T, s string) *net.UDPAddr {
	t.Helper()
	a, err := net.ResolveUDPAddr("udp", s)
	if err != nil {
		t.Fatalf("ResolveUDPAddr(%q): %v", s, err)
	}
	return a
}

// buildPacket constructs a well-formed discovery v1 packet for the
// supplied nonce. Returns the packet bytes; tests use this rather than
// hand-assembling 55-byte slices everywhere.
func buildPacket(nonce []byte) []byte {
	pkt := make([]byte, PacketSize)
	copy(pkt, MagicV1)
	copy(pkt[len(MagicV1):], nonce)
	return pkt
}

func TestPacketSizeIsMagicPlusNonce(t *testing.T) {
	if got, want := PacketSize, len(MagicV1)+NonceSize; got != want {
		t.Errorf("PacketSize=%d, want %d", got, want)
	}
}

func TestHandlePacket_HappyPath(t *testing.T) {
	s := NewStore(time.Minute)
	nonce := make([]byte, NonceSize)
	for i := range nonce {
		nonce[i] = byte(i)
	}
	src := newUDP(t, "203.0.113.7:33000")

	if !s.HandlePacket(buildPacket(nonce), src) {
		t.Fatal("HandlePacket must accept a well-formed discovery packet")
	}

	const nonceHex = "000102030405060708090a0b0c0d0e0f" +
		"101112131415161718191a1b1c1d1e1f"
	entry, ok := s.Lookup(nonceHex)
	if !ok {
		t.Fatal("Lookup of just-recorded nonce failed")
	}
	if entry.SrcAddr.String() != src.String() {
		t.Errorf("Lookup SrcAddr=%s, want %s", entry.SrcAddr, src)
	}
	if entry.ExpiresAt.Before(entry.ObservedAt) {
		t.Errorf("ExpiresAt %s must be after ObservedAt %s",
			entry.ExpiresAt, entry.ObservedAt)
	}
}

func TestHandlePacket_RejectsWrongSize(t *testing.T) {
	s := NewStore(time.Minute)
	cases := map[string][]byte{
		"empty":   nil,
		"short":   make([]byte, PacketSize-1),
		"long":    make([]byte, PacketSize+1),
		"way off": make([]byte, 9),
	}
	for name, pkt := range cases {
		t.Run(name, func(t *testing.T) {
			if s.HandlePacket(pkt, newUDP(t, "127.0.0.1:1")) {
				t.Errorf("HandlePacket(%d-byte %q) must return false",
					len(pkt), name)
			}
		})
	}
	if s.Len() != 0 {
		t.Errorf("Len=%d after rejected packets, want 0", s.Len())
	}
}

func TestHandlePacket_RejectsWrongMagic(t *testing.T) {
	s := NewStore(time.Minute)
	pkt := make([]byte, PacketSize)
	// Right shape, wrong content — e.g. someone scanning the UDP port.
	for i := range pkt {
		pkt[i] = byte(i)
	}
	if s.HandlePacket(pkt, newUDP(t, "127.0.0.1:1")) {
		t.Fatal("HandlePacket must reject packet with mismatched magic")
	}
	if s.Len() != 0 {
		t.Errorf("Len=%d, want 0", s.Len())
	}
}

func TestHandlePacket_RejectsNonUDPAddr(t *testing.T) {
	// quic-go always hands us *net.UDPAddr but the production code
	// type-asserts defensively; cover the negative branch.
	s := NewStore(time.Minute)
	nonce := make([]byte, NonceSize)
	if s.HandlePacket(buildPacket(nonce), fakeAddr{"foo:1"}) {
		t.Fatal("HandlePacket must reject non-UDP src addr")
	}
}

func TestLookup_UnknownReturnsFalse(t *testing.T) {
	s := NewStore(time.Minute)
	if _, ok := s.Lookup("deadbeef"); ok {
		t.Fatal("Lookup of unknown nonce must return ok=false")
	}
}

func TestLookup_ExpiredDroppedLazily(t *testing.T) {
	s := NewStore(time.Nanosecond)
	nonce := make([]byte, NonceSize)
	src := newUDP(t, "127.0.0.1:1")
	if !s.HandlePacket(buildPacket(nonce), src) {
		t.Fatal("HandlePacket: false")
	}
	if s.Len() != 1 {
		t.Fatalf("Len=%d after Handle, want 1", s.Len())
	}
	time.Sleep(10 * time.Millisecond)
	if _, ok := s.Lookup(toHex(nonce)); ok {
		t.Fatal("Lookup of expired nonce must return ok=false")
	}
	if s.Len() != 0 {
		t.Errorf("Lookup must lazy-delete expired entries; Len=%d", s.Len())
	}
}

func TestReap_DropsOnlyExpired(t *testing.T) {
	live := NewStore(time.Minute)
	// Manually push one entry into the past via direct map access is
	// impossible (entries field is unexported); use a separate
	// short-TTL store to produce expired entries instead.
	expiring := NewStore(time.Nanosecond)

	for i := 0; i < 3; i++ {
		nonce := make([]byte, NonceSize)
		nonce[0] = byte(i)
		_ = expiring.HandlePacket(buildPacket(nonce), newUDP(t, "127.0.0.1:1"))
	}
	keep := make([]byte, NonceSize)
	keep[0] = 0xff
	_ = live.HandlePacket(buildPacket(keep), newUDP(t, "127.0.0.1:1"))

	time.Sleep(10 * time.Millisecond)
	if dropped := expiring.Reap(); dropped != 3 {
		t.Errorf("expiring.Reap dropped %d, want 3", dropped)
	}
	if dropped := live.Reap(); dropped != 0 {
		t.Errorf("live.Reap dropped %d, want 0 (entry still fresh)",
			dropped)
	}
	if live.Len() != 1 {
		t.Errorf("live.Len=%d, want 1", live.Len())
	}
}

func TestDefaultTTLApplied(t *testing.T) {
	s := NewStore(0)
	nonce := make([]byte, NonceSize)
	src := newUDP(t, "127.0.0.1:1")
	if !s.HandlePacket(buildPacket(nonce), src) {
		t.Fatal("HandlePacket: false")
	}
	entry, ok := s.Lookup(toHex(nonce))
	if !ok {
		t.Fatal("Lookup: false")
	}
	want := DefaultTTL
	got := entry.ExpiresAt.Sub(entry.ObservedAt)
	delta := got - want
	if delta < -time.Second || delta > time.Second {
		t.Errorf("TTL %s, want ~%s", got, want)
	}
}

// toHex is a local helper duplicating encoding/hex.EncodeToString —
// used so the test file doesn't import encoding/hex just for one call.
func toHex(b []byte) string {
	const digits = "0123456789abcdef"
	out := make([]byte, len(b)*2)
	for i, c := range b {
		out[i*2] = digits[c>>4]
		out[i*2+1] = digits[c&0x0f]
	}
	return string(out)
}
