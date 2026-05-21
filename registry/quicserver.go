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
	"github.com/distribution/distribution/v3/internal/discovery"
)

// quicALPN is the ALPN token for HTTP/3 (RFC 9114 §3.1).
const quicALPN = "h3"

// probeMagic is the first byte of every NAT-punching probe packet we send.
// Real QUIC packets always have bit 0x40 (the "fixed bit") set in their
// first byte (RFC 9000 §17.2/§17.3); 0x00 unambiguously distinguishes a
// probe from a QUIC packet the kernel might deliver to the same socket.
// The client picks the probes off the socket BEFORE handing it to its own
// QUIC stack, so they never reach quic-go.
const probeMagic = byte(0x00)

// probeBody is the human-readable payload of a probe packet; mostly so
// `tcpdump` shows something sensible during debugging.
var probeBody = []byte("NSDEV-REGISTRY-PROBE\n")

// QuicListener owns the UDP socket the QUIC HTTP/3 server uses, plus the
// non-QUIC packet receiver that backs the STUN-less discovery protocol
// in internal/discovery. Layering looks like this:
//
//	          *net.UDPConn  (single UDP socket, e.g. :5000)
//	               │
//	      quic.Transport.Conn                   ← we wrap the conn so the
//	     ┌─────────┴─────────────┐                 same socket can carry
//	     │                       │                 both QUIC traffic AND
//	  QUIC packets         non-QUIC packets        opaque discovery probes
//	     │                       │
//	  http3.Server          discovery.Store
//	  (OCI v2 over h3)      (nonce → src_addr,
//	                         filled in by the
//	                         non-QUIC packet
//	                         receiver goroutine)
//
// The session handler reads the discovery store to look up the REAL
// post-NAT external address the client appeared at, so the bearer-token
// handshake doesn't have to trust the address the client claims for
// itself in --probe-port.
type QuicListener struct {
	transport *quic.Transport
	listener  *quic.EarlyListener
	srv       *http3.Server
	udpConn   *net.UDPConn

	// discovery is the nonce→src_addr lookup table populated by the
	// non-QUIC packet receiver. The session handler dereferences it
	// when it sees `discovery_nonce` in a SessionRequest.
	discovery *discovery.Store

	// advertise overrides the externally reachable UDP endpoint
	// reported to clients via /v3/sessions. Defaults to the bind addr.
	advertise string

	// muProbe serialises Punch invocations so concurrent session
	// handshakes don't interleave their probe bursts on the shared
	// socket.
	muProbe sync.Mutex

	// cancel stops the non-QUIC packet receiver on Close.
	cancel context.CancelFunc
	wg     sync.WaitGroup
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

// Discovery returns the discovery store this listener feeds. The
// session handler queries it to translate `discovery_nonce` into the
// observed post-NAT source address.
func (q *QuicListener) Discovery() *discovery.Store {
	return q.discovery
}

// Punch fires `count` probe datagrams from the QUIC listener's socket
// toward dst, with `spacing` between datagrams. Defaults: count=10,
// spacing=50ms, mirroring relay.rs's behaviour.
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
// the TCP port the registry is already serving on, AND a side-car non-QUIC
// packet receiver that powers the discovery protocol. The provided
// handler is the same handler tree the TCP `http.Server` uses, so OCI v2
// (and the future `/v3/` endpoints) work identically over HTTP/3.
//
// tlsConf must already carry a non-nil GetCertificate or Certificates
// slice — i.e. TLS must be configured. HTTP/3 requires TLS; running plain
// HTTP over QUIC is not part of the spec.
func startQUICServer(ctx context.Context, addr string, tlsConf *tls.Config, handler http.Handler) (*QuicListener, error) {
	if tlsConf == nil {
		return nil, errors.New("quic: TLS configuration is required (HTTP/3 has no plaintext mode)")
	}

	// Clone TLS config so we don't mutate the one the TCP listener uses;
	// HTTP/3 needs ALPN "h3" advertised.
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

	// quic.Transport lets us share the UDP socket between QUIC traffic
	// and our discovery sub-protocol. The transport's
	// ReadNonQUICPacket channel surfaces any packet whose first two
	// bits are zero — exactly what our discovery packets are crafted
	// to look like.
	transport := &quic.Transport{Conn: udpConn}

	listener, err := transport.ListenEarly(http3.ConfigureTLSConfig(quicTLS), defaultQUICConfig())
	if err != nil {
		_ = udpConn.Close()
		return nil, fmt.Errorf("quic: ListenEarly: %w", err)
	}

	srv := &http3.Server{
		Handler:    handler,
		TLSConfig:  http3.ConfigureTLSConfig(quicTLS),
		QUICConfig: defaultQUICConfig(),
	}

	dcontext.GetLoggerWithField(ctx, "addr", udpConn.LocalAddr().String()).
		Info("listening on quic (udp), tls")

	ql := &QuicListener{
		transport: transport,
		listener:  listener,
		srv:       srv,
		udpConn:   udpConn,
		discovery: discovery.NewStore(0),
	}

	receiverCtx, cancel := context.WithCancel(ctx)
	ql.cancel = cancel

	// h3 server goroutine.
	ql.wg.Add(1)
	go func() {
		defer ql.wg.Done()
		if err := srv.ServeListener(listener); err != nil &&
			!errors.Is(err, http.ErrServerClosed) &&
			!errors.Is(err, quic.ErrServerClosed) {
			dcontext.GetLogger(ctx).Errorf("quic server exited with error: %v", err)
		}
	}()

	// Non-QUIC packet receiver. Filters incoming packets through the
	// discovery store; anything that isn't a recognised discovery
	// packet is silently dropped (likely a stray probe, port scan, or
	// truncated QUIC packet that didn't fit quic-go's heuristic).
	ql.wg.Add(1)
	go func() {
		defer ql.wg.Done()
		buf := make([]byte, 2048)
		for {
			n, src, err := transport.ReadNonQUICPacket(receiverCtx, buf)
			if err != nil {
				if errors.Is(err, context.Canceled) {
					return
				}
				if errors.Is(err, net.ErrClosed) || receiverCtx.Err() != nil {
					return
				}
				dcontext.GetLogger(ctx).Warnf(
					"discovery: ReadNonQUICPacket: %v", err)
				return
			}
			if !ql.discovery.HandlePacket(buf[:n], src) {
				// Packet didn't match our discovery format — could
				// be a stray probe from another tool, an old client,
				// or a port scan. Logged at debug only because the
				// public UDP port is a likely target for noise.
				dcontext.GetLoggerWithField(ctx, "src", src).
					Debugf("discovery: ignoring %d-byte non-discovery packet", n)
			}
		}
	}()

	return ql, nil
}

// Close stops the underlying http3.Server, the non-QUIC packet receiver,
// and releases the UDP socket. Idempotent.
func (q *QuicListener) Close() error {
	if q.cancel != nil {
		q.cancel()
	}
	var err error
	if q.srv != nil {
		err = q.srv.Close()
	}
	if q.transport != nil {
		_ = q.transport.Close()
	}
	q.wg.Wait()
	return err
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
