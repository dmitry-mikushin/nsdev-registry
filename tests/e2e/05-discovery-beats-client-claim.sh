#!/usr/bin/env bash
#
# 05-discovery-beats-client-claim.sh
#
# WHAT THIS TEST PRESERVES
# ------------------------
# That when both a discovery nonce AND a `client_udp_addr` claim are
# present in the same /v3/sessions request, the registry uses what
# the KERNEL observed (the discovery store entry) and ignores the
# client claim. The client's claim is a guess about its own
# external 5-tuple; the kernel-observed src is ground truth.
#
# This is the entire point of the discovery protocol — to free the
# client from having to guess. If the precedence ever flipped (or
# the resolver started silently merging them), every symmetric-NAT
# client would punch into an invented address.
#
# WHY IT MATTERS
# --------------
# The handler's resolution order is documented as
#     (a) discovery_nonce: lookup observed addr
#     (b) client_udp_addr: fallback when nonce missing/unresolved
# but documentation can drift away from code. This test pins the
# precedence in CI.
#
# WHEN TO BREAK THIS TEST
# -----------------------
# Only with an explicit design change. Likely never: prioritising a
# client claim over kernel observation re-introduces every NAT bug
# the protocol was built to avoid.

set -euo pipefail
LIB="$(dirname "$0")/lib.sh"
# shellcheck source=lib.sh
. "$LIB"

ensure_image
trap stop_registry EXIT
start_registry "discovery-beats-claim"

nonce_hex="$(send_discovery "$NSREG_HOST_UDP")"
sleep 0.1

# Lie about the client's UDP addr. The registry MUST ignore this
# value because the discovery nonce resolves to a real observation.
fake_claim="10.255.255.250:1"

resp="$(post_session \
        "{\"user\":\"mallory\",\"discovery_nonce\":\"$nonce_hex\",\"client_udp_addr\":\"$fake_claim\"}")"

discovery_used="$(extract_json_field "$resp" '.discovery_used')"
issued_to_client="$(extract_json_field "$resp" '.issued_to_client')"

assert_eq "$discovery_used" "true" "discovery path was selected over claim"

if [ "$issued_to_client" = "$fake_claim" ]; then
    echo "[FAIL] registry trusted the client claim ($fake_claim) over its observation" >&2
    exit 1
fi
echo "[OK]   issued_to_client = $issued_to_client (not the fake claim $fake_claim)"
