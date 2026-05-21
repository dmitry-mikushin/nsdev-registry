package registry

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/distribution/distribution/v3/internal/dcontext"
)

// quicALPN is the ALPN token for HTTP/3 (RFC 9114 §3.1).
const quicALPN = "h3"

// probeMagic is the first byte of every NAT-punching probe packet we send.
// Real QUIC packets always have bit 0x40 (the "fixed bit") set in their
// first byte (RFC 9000 §17.2/§17.3), so 0x00 unambiguously distinguishes
// a probe from a QUIC packet the kernel might deliver to the same socket.
// The client picks the probes off the socket BEFORE handing it to its own
// QUIC stack, so they never reach quic-go.
const probeMagic = byte(0x00)

// probeBody is the human-readable payload of a probe packet; mostly so
// `tcpdump` shows something sensible during debugging.
var probeBody = []byte("NSDEV-REGISTRY-PROBE\n")

// QuicListener wraps the UDP socket the QUIC HTTP/3 server uses, so the
// session handler can fire NAT-punching probes through the SAME 5-tuple
// the client's subsequent QUIC ClientHello will use. Without that, a
// symmetric-NAT-bound client would see incoming server packets from a
// different port and drop them.
//
// The pattern mirrors what nsdev-push's relay.rs did before this rewrite:
// 10 small UDP datagrams toward the operator's discovered (ip:port),
// spaced ~50ms, to open the NAT mapping ahead of the QUIC handshake.
type QuicListener struct {
	srv     *http3.Server
	udpConn *net.UDPConn
	// advertise is the externally reachable UDP endpoint we tell clients
	// to dial. Defaults to the bind address; can be overridden when the
	// registry is behind a static NAT and the bind address is private.
	advertise string

	// muProbe serialises Punch invocations so concurrent session handshakes
	// don't interleave their probe bursts on the shared socket. Writes to
	// a UDP socket are thread-safe in Go's net package, so this is purely
	// to keep traffic patterns clean for debugging.
	muProbe sync.Mutex
}

// AdvertisedAddr returns the externally reachable UDP endpoint the
// session handler should hand back to clients.
func (q *QuicListener) AdvertisedAddr() string {
	if q.advertise != "" {
		return q.advertise
	}
	return q.udpConn.LocalAddr().String()
}

// SetAdvertisedAddr overrides the value returned by AdvertisedAddr.
// Useful when the registry is behind a 1:1 NAT and the bind address is
// a private IP that clients can't dial.
func (q *QuicListener) SetAdvertisedAddr(addr string) {
	q.advertise = addr
}

// Punch fires `count` probe datagrams from the QUIC listener's socket
// toward dst, with `spacing` between datagrams. It returns the first
// write error, if any (subsequent errors are logged and ignored).
//
// Reasonable defaults that mirror relay.rs: count=10, spacing=50ms.
func (q *QuicListener) Punch(ctx context.Context, dst string, count int, spacing time.Duration) error {
	if count <= 0 {
		count = 10
	}
	if spacing <= 0 {
		spacing = 50 * time.Millisecond
	}
	udpDst, err := net.ResolveUDPAddr("udp", dst)
	if err != nil {
		return fmt.Errorf("quic punch: resolve %q: %w", dst, err)
	}
	pkt := make([]byte, 1+len(probeBody))
	pkt[0] = probeMagic
	copy(pkt[1:], probeBody)

	q.muProbe.Lock()
	defer q.muProbe.Unlock()

	logger := dcontext.GetLoggerWithField(ctx, "punch_dst", udpDst.String())
	logger.Debugf("nat-punch: sending %d probe(s) every %v", count, spacing)

	var firstErr error
	for i := 0; i < count; i++ {
		if _, err := q.udpConn.WriteToUDP(pkt, udpDst); err != nil {
			if firstErr == nil {
				firstErr = err
			}
			logger.Warnf("nat-punch: probe %d/%d write error: %v", i+1, count, err)
		}
		if i < count-1 {
			time.Sleep(spacing)
		}
	}
	return firstErr
}

// startQUICServer brings up an HTTP/3 listener on the same UDP address as
// the TCP port the registry is already serving on. The provided handler is
// the same handler tree the TCP `http.Server` uses, so OCI v2 (and the
// future `/v3/` endpoints) work identically over HTTP/3.
//
// tlsConf must already carry a non-nil GetCertificate or Certificates
// slice — i.e. TLS must be configured. HTTP/3 requires TLS; running plain
// HTTP over QUIC is not part of the spec.
func startQUICServer(ctx context.Context, addr string, tlsConf *tls.Config, handler http.Handler) (*QuicListener, error) {
	if tlsConf == nil {
		return nil, errors.New("quic: TLS configuration is required (HTTP/3 has no plaintext mode)")
	}

	// Clone TLS config so we don't mutate the one the TCP listener uses;
	// HTTP/3 needs ALPN "h3" advertised. quic-go enforces this and will
	// reject incoming connections that don't negotiate it.
	quicTLS := tlsConf.Clone()
	if !hasALPN(quicTLS.NextProtos, quicALPN) {
		quicTLS.NextProtos = append([]string{quicALPN}, quicTLS.NextProtos...)
	}

	udpAddr, err := net.ResolveUDPAddr("udp", addr)
	if err != nil {
		return nil, err
	}
	udpConn, err := net.ListenUDP("udp", udpAddr)
	if err != nil {
		return nil, err
	}

	srv := &http3.Server{
		Handler:    handler,
		TLSConfig:  http3.ConfigureTLSConfig(quicTLS),
		QUICConfig: defaultQUICConfig(),
	}

	dcontext.GetLoggerWithField(ctx, "addr", udpConn.LocalAddr().String()).
		Info("listening on quic (udp), tls")

	go func() {
		if err := srv.Serve(udpConn); err != nil && !errors.Is(err, http.ErrServerClosed) && !errors.Is(err, quic.ErrServerClosed) {
			dcontext.GetLogger(ctx).Errorf("quic server exited with error: %v", err)
		}
	}()

	return &QuicListener{
		srv:     srv,
		udpConn: udpConn,
	}, nil
}

// Close stops the underlying http3.Server. The UDP socket is closed as a
// side-effect of http3.Server.Close.
func (q *QuicListener) Close() error {
	return q.srv.Close()
}

// hasALPN reports whether `proto` is already present in `protos`.
func hasALPN(protos []string, proto string) bool {
	for _, p := range protos {
		if p == proto {
			return true
		}
	}
	return false
}

// defaultQUICConfig returns conservative defaults that match the
// distribution registry's expectations for long-lived large uploads.
// Tunables (idle timeout, max streams, flow-control windows) can later be
// exposed through configuration.HTTP.QUIC.
func defaultQUICConfig() *quic.Config {
	return &quic.Config{
		MaxIncomingStreams: 256,
		MaxIdleTimeout:     60 * time.Second,
	}
}
