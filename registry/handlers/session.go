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
	// ClientUDPAddr is the operator's UDP probe endpoint, in "ip:port"
	// form. The session handler fires NAT-punching probes at this
	// address from the QUIC listener's socket so the operator's
	// subsequent ClientHello sees a stable 5-tuple, matching what
	// nsdev-push's old relay.rs did before this rewrite.
	ClientUDPAddr string `json:"client_udp_addr"`
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
}

// QuicPuncher is the slice of the quic listener that the session handler
// needs to reach: the externally reachable UDP endpoint to advertise, and
// the ability to fire probes at the client.
type QuicPuncher interface {
	AdvertisedAddr() string
	Punch(ctx context.Context, dst string, count int, spacing time.Duration) error
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
	if req.ClientUDPAddr == "" {
		http.Error(w, "session: client_udp_addr is required",
			http.StatusBadRequest)
		return
	}
	if _, err := net.ResolveUDPAddr("udp", req.ClientUDPAddr); err != nil {
		http.Error(w, fmt.Sprintf("session: invalid client_udp_addr %q: %v",
			req.ClientUDPAddr, err), http.StatusBadRequest)
		return
	}

	ttl := req.RequestedTTL
	if ttl <= 0 || ttl > h.MaxTTL {
		ttl = h.MaxTTL
	}

	tok, entry, err := h.Store.Issue(req.User, req.ClientUDPAddr, ttl)
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
	if err := h.Quic.Punch(r.Context(), req.ClientUDPAddr,
		h.ProbeCount, h.ProbeStep); err != nil {
		// A probe write failure isn't fatal — the client may still
		// reach us via its own outbound ClientHello opening the NAT
		// mapping. Log + reduce the reported probe count so the
		// client knows to retry sooner if its NAT is symmetric.
		dcontext.GetLogger(r.Context()).Warnf(
			"session: punch error for %s: %v", req.ClientUDPAddr, err)
		probesSent = 0
	}

	resp := SessionResponse{
		Token:          string(tok),
		ExpiresAt:      entry.ExpiresAt,
		ServerUDPAddr:  h.Quic.AdvertisedAddr(),
		ProbesSent:     probesSent,
		IssuedToUser:   entry.User,
		IssuedToClient: entry.ClientAddr,
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
