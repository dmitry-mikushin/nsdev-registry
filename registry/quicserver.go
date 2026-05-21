package registry

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"

	"github.com/quic-go/quic-go"
	"github.com/quic-go/quic-go/http3"

	"github.com/distribution/distribution/v3/internal/dcontext"
)

// quicALPN is the ALPN token for HTTP/3 (RFC 9114 §3.1).
const quicALPN = "h3"

// startQUICServer brings up an HTTP/3 listener on the same UDP address as
// the TCP port the registry is already serving on. The provided handler is
// the same handler tree the TCP `http.Server` uses, so OCI v2 (and the
// future `/v3/` endpoints) work identically over HTTP/3.
//
// tlsConf must already carry a non-nil GetCertificate or Certificates
// slice — i.e. TLS must be configured. HTTP/3 requires TLS; running plain
// HTTP over QUIC is not part of the spec.
//
// The returned *http3.Server is owned by the caller; the caller is
// responsible for calling .Close on shutdown.
func startQUICServer(ctx context.Context, addr string, tlsConf *tls.Config, handler http.Handler) (*http3.Server, error) {
	if tlsConf == nil {
		return nil, errors.New("quic: TLS configuration is required (HTTP/3 has no plaintext mode)")
	}

	// Clone TLS config so we don't mutate the one the TCP listener uses;
	// HTTP/3 needs ALPN "h3" advertised. quic-go enforces this and will
	// reject incoming connections that don't negotiate it, so we splice it
	// in defensively whether or not the caller remembered.
	quicTLS := tlsConf.Clone()
	if !hasALPN(quicTLS.NextProtos, quicALPN) {
		quicTLS.NextProtos = append([]string{quicALPN}, quicTLS.NextProtos...)
	}

	// Resolve the address so we can bind the same numerical port for UDP
	// that the TCP listener is using. TCP and UDP are independent L4
	// sockets; binding both on `:5000` is fine and standard.
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

	return srv, nil
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
		// Allow many concurrent in-flight blob PUT/PATCH requests on one
		// QUIC connection — every PATCH is its own request, and nsdev-push
		// chunks uploads into 1-64 MB slices. 256 concurrent bidi streams
		// covers a generous push fan-out.
		MaxIncomingStreams: 256,
		// 60s idle keeps NAT mappings alive without leaking ghost
		// connections from clients that disappeared mid-upload.
		MaxIdleTimeout: 60 * 1_000_000_000,
	}
}
