package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/distribution/distribution/v3/internal/dcontext"
	"github.com/distribution/distribution/v3/internal/discovery"
	"github.com/distribution/distribution/v3/internal/session"
)

// SessionRequest is the JSON body the operator-side `nsdev-registry session`
// subcommand sends to POST /v3/sessions. The subcommand runs on the
// REGISTRY HOST inside the operator's ssh session, so it can read
// authoritative identity from its own environment ($SSH_USER) and the
// operator's source IP from $SSH_CLIENT — neither of which the operator
// can spoof, because the ssh daemon set them.
type SessionRequest struct {
	// User is the authenticated operator identity, taken from $SSH_USER
	// on the server side of the ssh session.
	User string `json:"user"`
	// ClientUDPAddr is the operator's claimed UDP probe endpoint, in
	// "ip:port" form. Used as the punch target ONLY when DiscoveryNonce
	// is empty or unresolved — see the field below.
	ClientUDPAddr string `json:"client_udp_addr,omitempty"`
	// DiscoveryNonce is the hex-encoded nonce the operator sent in a
	// discovery UDP packet to the registry's QUIC port BEFORE invoking
	// this handshake. If present AND the registry's discovery store
	// has a matching entry, the punch target becomes the address the
	// kernel actually observed for that packet — i.e. the post-NAT
	// 5-tuple — and ClientUDPAddr is ignored. This is the
	// STUN-equivalent path: the registry trusts what it saw on the
	// wire over what the client claims.
	DiscoveryNonce string `json:"discovery_nonce,omitempty"`
	// RequestedTTL, if non-zero, asks for a specific token lifetime.
	// The server clamps to its own MaxTTL.
	RequestedTTL time.Duration `json:"requested_ttl,omitempty"`
}

// SessionResponse is the JSON the handler returns to the subcommand,
// which then prints it to its own stdout for the operator's `nsdev-push`
// to consume over the ssh stdout pipe.
type SessionResponse struct {
	Token          string    `json:"token"`
	ExpiresAt      time.Time `json:"expires_at"`
	ServerUDPAddr  string    `json:"server_udp_addr"`
	ProbesSent     int       `json:"probes_sent"`
	IssuedToUser   string    `json:"issued_to_user"`
	IssuedToClient string    `json:"issued_to_client"`
	// DiscoveryUsed is true when the punch target was sourced from the
	// discovery store rather than the client-supplied ClientUDPAddr.
	// nsdev-push uses this as a positive confirmation that the
	// STUN-less hole-punching path completed end-to-end; if false,
	// the client should suspect a port-translating NAT and fall back
	// to user-attention diagnostics.
	DiscoveryUsed bool `json:"discovery_used"`
}

// QuicPuncher is the slice of the quic listener that the session handler
// needs to reach: the externally reachable UDP endpoint to advertise, the
// ability to fire probes at the client, and the discovery store the
// non-QUIC packet receiver populates.
type QuicPuncher interface {
	AdvertisedAddr() string
	Punch(ctx context.Context, dst string, count int, spacing time.Duration) error
	Discovery() *discovery.Store
}

// SessionHandler issues short-lived bearer tokens to ssh-authenticated
// operators and fires NAT-punching probes at the operator's UDP endpoint
// so the subsequent QUIC ClientHello traverses NAT cleanly.
//
// IMPORTANT: this handler MUST only be reachable through a path that is
// gated by ssh-server-side authentication. In practice that means it is
// mounted on the TCP listener bound to localhost only, and the
// `nsdev-registry session` subcommand POSTs to it after ssh has set
// $SSH_USER + $SSH_CLIENT in its env. Mounting this on the QUIC handler
// tree would let anyone with UDP reach to the registry issue themselves
// tokens — defeats the whole identity story.
type SessionHandler struct {
	Store      *session.Store
	Quic       QuicPuncher
	MaxTTL     time.Duration
	ProbeCount int
	ProbeStep  time.Duration
}

// NewSessionHandler returns a SessionHandler with conservative defaults.
func NewSessionHandler(store *session.Store, q QuicPuncher) *SessionHandler {
	return &SessionHandler{
		Store:      store,
		Quic:       q,
		MaxTTL:     session.DefaultTTL,
		ProbeCount: 10,
		ProbeStep:  50 * time.Millisecond,
	}
}

func (h *SessionHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "session: only POST is supported", http.StatusMethodNotAllowed)
		return
	}

	// Belt-and-braces sanity check: refuse anything that didn't come in
	// on a loopback connection. The bind address SHOULD already be
	// 127.0.0.1 for the TCP listener, but a misconfiguration that opens
	// the TCP listener to the network would otherwise let an attacker
	// issue tokens by guessing $SSH_USER. The check is cheap.
	if !isLoopbackRemote(r.RemoteAddr) {
		dcontext.GetLogger(r.Context()).Warnf(
			"session: rejecting non-loopback caller %s", r.RemoteAddr)
		http.Error(w, "session: handshake endpoint is loopback-only",
			http.StatusForbidden)
		return
	}

	var req SessionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, fmt.Sprintf("session: decode body: %v", err),
			http.StatusBadRequest)
		return
	}
	if req.User == "" {
		http.Error(w, "session: user is required",
			http.StatusBadRequest)
		return
	}

	// Resolve the punch target. Two paths:
	//
	//   (a) discovery_nonce: the client sent a UDP discovery packet to
	//       the registry's QUIC port BEFORE this handshake. The packet
	//       receiver in registry/quicserver.go recorded (nonce -> the
	//       kernel-observed src 5-tuple) in discovery.Store. We look
	//       it up here and use the OBSERVED address, which captures the
	//       client's true post-NAT external (IP, port) — including
	//       symmetric-NAT port remapping that the client itself cannot
	//       know about. This is the STUN-equivalent path.
	//
	//   (b) client_udp_addr fallback: when the client cannot reach the
	//       registry's UDP port directly to send a discovery packet
	//       (e.g. firewall blocks outbound UDP), it instead claims an
	//       address. We dutifully fire probes at it; if the claim was
	//       wrong (NAT translated the port) the probes go nowhere, but
	//       the client's own QUIC ClientHello may still open the path
	//       since QUIC is client-initiated.
	//
	// The client is required to supply at least one of the two.
	var punchTarget string
	discoveryUsed := false
	if req.DiscoveryNonce != "" {
		if entry, ok := h.Quic.Discovery().Lookup(req.DiscoveryNonce); ok {
			punchTarget = entry.SrcAddr.String()
			discoveryUsed = true
			dcontext.GetLogger(r.Context()).Infof(
				"session: discovery nonce %s -> observed %s",
				req.DiscoveryNonce[:8]+"...", punchTarget)
		} else {
			dcontext.GetLogger(r.Context()).Warnf(
				"session: discovery nonce %s not found in store; "+
					"falling back to client_udp_addr",
				req.DiscoveryNonce[:8]+"...")
		}
	}
	if punchTarget == "" {
		if req.ClientUDPAddr == "" {
			http.Error(w,
				"session: either discovery_nonce (resolved) or "+
					"client_udp_addr must be supplied",
				http.StatusBadRequest)
			return
		}
		if _, err := net.ResolveUDPAddr("udp", req.ClientUDPAddr); err != nil {
			http.Error(w, fmt.Sprintf("session: invalid client_udp_addr %q: %v",
				req.ClientUDPAddr, err), http.StatusBadRequest)
			return
		}
		punchTarget = req.ClientUDPAddr
	}

	ttl := req.RequestedTTL
	if ttl <= 0 || ttl > h.MaxTTL {
		ttl = h.MaxTTL
	}

	tok, entry, err := h.Store.Issue(req.User, punchTarget, ttl)
	if err != nil {
		http.Error(w, fmt.Sprintf("session: issue token: %v", err),
			http.StatusInternalServerError)
		return
	}

	// Fire NAT-punching probes BEFORE we return, so by the time the
	// operator's nsdev-push reads the JSON and opens its QUIC connection
	// the probes have already landed at the client's NAT mapping and any
	// firewall state is primed for the inbound ClientHello.
	probesSent := h.ProbeCount
	if err := h.Quic.Punch(r.Context(), punchTarget,
		h.ProbeCount, h.ProbeStep); err != nil {
		// A probe write failure isn't fatal — the client may still
		// reach us via its own outbound ClientHello opening the NAT
		// mapping. Log + reduce the reported probe count so the
		// client knows to retry sooner if its NAT is symmetric.
		dcontext.GetLogger(r.Context()).Warnf(
			"session: punch error for %s: %v", punchTarget, err)
		probesSent = 0
	}

	resp := SessionResponse{
		Token:          string(tok),
		ExpiresAt:      entry.ExpiresAt,
		ServerUDPAddr:  h.Quic.AdvertisedAddr(),
		ProbesSent:     probesSent,
		IssuedToUser:   entry.User,
		IssuedToClient: entry.ClientAddr,
		DiscoveryUsed:  discoveryUsed,
	}
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(&resp); err != nil {
		dcontext.GetLogger(r.Context()).Warnf(
			"session: encode response: %v", err)
	}
	dcontext.GetLogger(r.Context()).Infof(
		"session: issued token for user=%q client=%s ttl=%s",
		entry.User, entry.ClientAddr, ttl)
}

// isLoopbackRemote returns true if remoteAddr (RFC3986 host:port) is a
// loopback IP. Used to gate /v3/sessions to localhost-only callers.
func isLoopbackRemote(remoteAddr string) bool {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		// Some net.Conn implementations report bare addresses; try a
		// fallback that strips brackets for IPv6.
		host = strings.Trim(remoteAddr, "[]")
	}
	ip := net.ParseIP(host)
	if ip == nil {
		return false
	}
	return ip.IsLoopback()
}
