#!/usr/bin/env bash
#
# 06-discovery-miss-falls-back-to-claim.sh
#
# WHAT THIS TEST PRESERVES
# ------------------------
# That when /v3/sessions receives a `discovery_nonce` that the store
# does NOT have an entry for (because the client never managed to
# send a discovery packet — typically because outbound UDP to the
# registry's port is firewalled, or the registry was restarted
# between discovery and handshake), the handler:
#
#   - logs the miss but does NOT fail the request
#   - falls back to the supplied client_udp_addr
#   - returns discovery_used:false
#
# Operators downstream (nsdev-push) interpret discovery_used:false
# as "the STUN-equivalent path didn't work, the punch target is a
# guess; if the QUIC handshake fails, try a port-preserving
# scenario or a VPN".
#
# WHY IT MATTERS
# --------------
# This is the graceful-degradation behaviour. Without it, every
# nsdev-push deployment in a strict outbound-UDP-blocked
# environment would fail at the handshake step with no fallback
# even though the client's own ClientHello might still open the
# NAT mapping (QUIC is client-initiated). Keeping the fallback +
# the discovery_used:false signal gives operators a useful
# diagnostic.
#
# WHEN TO BREAK THIS TEST
# -----------------------
# Only by deliberately removing the fallback ("we require discovery
# to work, full stop"). That would also need a NEXTSILICON.md
# update saying so.

set -euo pipefail
LIB="$(dirname "$0")/lib.sh"
# shellcheck source=lib.sh
. "$LIB"

ensure_image
trap stop_registry EXIT
start_registry "discovery-miss-fallback"

# A nonce we never seeded. The store WILL NOT have an entry, so
# the handler must use client_udp_addr instead.
unknown_nonce="$(python3 -c 'import os,sys; sys.stdout.write(os.urandom(32).hex())')"

claimed_addr="10.0.0.42:55555"

resp="$(post_session \
        "{\"user\":\"carol\",\"discovery_nonce\":\"$unknown_nonce\",\"client_udp_addr\":\"$claimed_addr\"}")"

discovery_used="$(extract_json_field "$resp" '.discovery_used')"
issued_to_client="$(extract_json_field "$resp" '.issued_to_client')"
token="$(extract_json_field "$resp" '.token')"

assert_eq "$discovery_used" "false" "discovery_used flag on store miss"
assert_eq "$issued_to_client" "$claimed_addr" "fallback to client_udp_addr"

# Token must still be issued — the fallback path is functional, not
# a soft-fail. The downstream nsdev-push needs the token regardless
# of which addr was used for the NAT-punch.
if [ -z "$token" ] || [ "$token" = "null" ]; then
    echo "[FAIL] token missing from response: $resp" >&2
    exit 1
fi
echo "[OK]   token issued despite discovery miss"
