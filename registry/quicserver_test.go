package registry

import (
	"context"
	"net"
	"testing"
	"time"
)

// newLoopbackUDP binds a fresh UDP socket on 127.0.0.1:<ephemeral> for
// the test and returns it. Failures fatal-fail the test.
func newLoopbackUDP(t *testing.T) *net.UDPConn {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.ParseIP("127.0.0.1")})
	if err != nil {
		t.Fatalf("ListenUDP: %v", err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

// newPunchOnlyListener returns a QuicListener whose srv is nil — useful
// for testing the punching path without spinning up an http3.Server.
// Callers MUST NOT invoke Close on it (it would NPE on srv); the test
// `t.Cleanup` for the underlying UDP socket handles teardown.
func newPunchOnlyListener(conn *net.UDPConn) *QuicListener {
	return &QuicListener{udpConn: conn}
}

func TestPunch_SendsExpectedPacketCount(t *testing.T) {
	recv := newLoopbackUDP(t)
	send := newLoopbackUDP(t)
	ql := newPunchOnlyListener(send)

	const want = 5
	doneCh := make(chan int, 1)
	go func() {
		buf := make([]byte, 1500)
		got := 0
		// Read deadline so a buggy Punch that sends fewer doesn't hang.
		_ = recv.SetReadDeadline(time.Now().Add(2 * time.Second))
		for got < want {
			n, _, err := recv.ReadFromUDP(buf)
			if err != nil {
				break
			}
			if n < 1 || buf[0] != probeMagic {
				t.Errorf("packet %d: first byte=0x%02x, want probeMagic=0x%02x",
					got, buf[0], probeMagic)
			}
			if string(buf[1:n]) != string(probeBody) {
				t.Errorf("packet %d: body=%q, want %q", got,
					string(buf[1:n]), string(probeBody))
			}
			got++
		}
		doneCh <- got
	}()

	if err := ql.Punch(context.Background(), recv.LocalAddr().String(),
		want, 1*time.Millisecond); err != nil {
		t.Fatalf("Punch: %v", err)
	}

	got := <-doneCh
	if got != want {
		t.Errorf("receiver got %d packets, want %d", got, want)
	}
}

func TestPunch_DefaultsAppliedOnZero(t *testing.T) {
	recv := newLoopbackUDP(t)
	send := newLoopbackUDP(t)
	ql := newPunchOnlyListener(send)

	doneCh := make(chan int, 1)
	go func() {
		buf := make([]byte, 1500)
		got := 0
		_ = recv.SetReadDeadline(time.Now().Add(3 * time.Second))
		for got < 10 {
			_, _, err := recv.ReadFromUDP(buf)
			if err != nil {
				break
			}
			got++
		}
		doneCh <- got
	}()

	start := time.Now()
	// count=0 -> default 10, spacing=0 -> default 50ms
	if err := ql.Punch(context.Background(), recv.LocalAddr().String(), 0, 0); err != nil {
		t.Fatalf("Punch: %v", err)
	}
	elapsed := time.Since(start)

	got := <-doneCh
	if got != 10 {
		t.Errorf("receiver got %d packets, want 10 from default count", got)
	}
	// 9 gaps of 50ms = 450ms minimum; allow generous upper bound
	// for slow CI scheduling.
	if elapsed < 400*time.Millisecond {
		t.Errorf("Punch returned in %s, must respect ~50ms spacing "+
			"(9 gaps, expected >=400ms)", elapsed)
	}
}

func TestPunch_BadAddressReturnsError(t *testing.T) {
	send := newLoopbackUDP(t)
	ql := newPunchOnlyListener(send)

	if err := ql.Punch(context.Background(), "not-an-addr", 1, time.Millisecond); err == nil {
		t.Fatal("Punch with malformed dst must return error")
	}
}

func TestAdvertisedAddr_FallsBackToBindAddr(t *testing.T) {
	send := newLoopbackUDP(t)
	ql := newPunchOnlyListener(send)
	if got, want := ql.AdvertisedAddr(), send.LocalAddr().String(); got != want {
		t.Errorf("AdvertisedAddr=%q, want bind addr %q", got, want)
	}
}

func TestAdvertisedAddr_OverridePrefersExplicit(t *testing.T) {
	send := newLoopbackUDP(t)
	ql := newPunchOnlyListener(send)
	ql.SetAdvertisedAddr("external.example.org:12345")
	if got := ql.AdvertisedAddr(); got != "external.example.org:12345" {
		t.Errorf("AdvertisedAddr=%q, want override", got)
	}
}

func TestHasALPN(t *testing.T) {
	if !hasALPN([]string{"h2", "h3", "http/1.1"}, "h3") {
		t.Error("hasALPN must find present token")
	}
	if hasALPN([]string{"h2", "http/1.1"}, "h3") {
		t.Error("hasALPN must miss absent token")
	}
	if hasALPN(nil, "h3") {
		t.Error("hasALPN on nil must return false")
	}
}
