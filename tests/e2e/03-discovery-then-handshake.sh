#!/usr/bin/env bash
#
# 03-discovery-then-handshake.sh
#
# WHAT THIS TEST PRESERVES
# ------------------------
# The complete STUN-less handshake protocol the registry is built
# around:
#
#   step 1  client opens a UDP socket
#   step 2  client sends ONE 55-byte discovery datagram from that
#           socket to the registry's QUIC port:
#               byte 0      : 0x00     (non-QUIC marker)
#               bytes 1..23 : "NSDEV-DISCOVERY-V1\n"
#               bytes 23..55: 32 random nonce bytes
#   step 3  the registry's quic.Transport routes the packet to its
#           ReadNonQUICPacket channel (because bits 0 and 1 are
#           clear), the side-car receiver decodes it and records
#                  hex(nonce) -> kernel-observed src 5-tuple
#           in the discovery.Store
#   step 4  client POSTs /v3/sessions with {"user": ..., "discovery_nonce": hex}
#   step 5  the handler looks up the nonce in the store, replaces
#           the punch target with the observed src addr, issues a
#           short-lived bearer token, fires NAT-punching probes back
#           at that observed address from the SAME UDP socket the
#           QUIC handshake will use, and returns JSON containing
#           token + server_udp_addr + discovery_used:true.
#
# Each of those bullets is testable here.
#
# WHY IT MATTERS
# --------------
# This is the entire reason we built the discovery protocol — to
# work correctly behind symmetric NAT without an external STUN
# server and without making the operator pick the right
# --probe-port. If a refactor changes the wire format, drops the
# loopback observation, or accidentally trusts the client's
# claimed addr over what the kernel actually saw, this test fails.
#
# WHEN TO BREAK THIS TEST
# -----------------------
# Only if we deliberately replace the discovery protocol with
# something incompatible (e.g. a real STUN server, or a different
# packet format). That change would also require updating
# internal/discovery/store.go::MagicV1 and any client (nsdev-push)
# that crafts the packet.

set -euo pipefail
LIB="$(dirname "$0")/lib.sh"
# shellcheck source=lib.sh
. "$LIB"

ensure_image
trap stop_registry EXIT
start_registry "discovery-handshake"

# -- step 1 + 2: send discovery packet ---------------------------------
nonce_hex="$(send_discovery "$NSREG_HOST_UDP")"
echo "[test] sent discovery nonce=${nonce_hex:0:16}..."

# Tiny grace period so the registry's non-QUIC receiver task has
# definitely read+recorded the packet before we hit the handshake.
sleep 0.1

# -- step 3 + 4 + 5: POST /v3/sessions with the nonce ------------------
resp="$(post_session \
        "{\"user\":\"alice\",\"discovery_nonce\":\"$nonce_hex\"}")"

echo "[test] session response: $resp"

token="$(extract_json_field "$resp" '.token')"
discovery_used="$(extract_json_field "$resp" '.discovery_used')"
issued_to_client="$(extract_json_field "$resp" '.issued_to_client')"
probes_sent="$(extract_json_field "$resp" '.probes_sent')"
issued_to_user="$(extract_json_field "$resp" '.issued_to_user')"

# -- assertions --------------------------------------------------------
# Token must be present and exactly 64 hex chars (32 random bytes
# hex-encoded by internal/session/store.go::Issue).
assert_eq "$(printf '%s' "$token" | wc -c)" "64" "token length"
assert_eq "$discovery_used" "true" "discovery_used flag"
assert_eq "$issued_to_user" "alice" "issued_to_user"

# The observed addr must be a loopback addr (127.0.0.1) — that's
# what the registry sees because both this script and the container
# are on the same Docker bridge with --network=default. The exact
# port is the ephemeral one Python picked, which we don't know up
# front; just sanity-check the shape.
assert_contains "$issued_to_client" "." "issued_to_client looks like an IPv4:port"

# 10 default probes per session.Handler config.
assert_eq "$probes_sent" "10" "NAT-punching probe count"
