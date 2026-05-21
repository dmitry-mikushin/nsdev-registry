#!/usr/bin/env bash
#
# 04-token-allows-http3-access.sh
#
# WHAT THIS TEST PRESERVES
# ------------------------
# The full happy-path chain end-to-end on one container:
#
#   discovery packet         -> store records nonce -> src
#   POST /v3/sessions        -> issues a bearer token
#   GET /v2/ over HTTP/3     -> 401 without token
#   GET /v2/ over HTTP/3     -> 200 WITH the issued token
#
# The point of this test is the LAST step: the very token we just
# minted must actually be honoured by the QUIC handler tree, with no
# extra paperwork. Anything that introduces a parallel auth
# requirement (Distribution token-issuer, htpasswd, mTLS) on the
# QUIC path would break the chain and this test surfaces it.
#
# WHY IT MATTERS
# --------------
# Every nsdev push depends on this exact loop closing. If it
# regresses, every push fails — but probably with a 401 deep inside
# layer_stream.rs upload retries, which is opaque. Keeping the test
# means we catch it at "compile + run" time, not at "user
# complaining their push is timing out" time.
#
# WHEN TO BREAK THIS TEST
# -----------------------
# Only by intent — see Phase 3 in NEXTSILICON.md. If we ever
# require BOTH a Bearer token AND a client cert on QUIC, this
# scenario will need to provision both before the GET.

set -euo pipefail
LIB="$(dirname "$0")/lib.sh"
# shellcheck source=lib.sh
. "$LIB"

require_http3_curl
ensure_image
trap stop_registry EXIT
start_registry "token-h3"

# Issue a token via the session handshake. (Discovery path is
# covered by 03-; here it's just a means to get a valid token.)
nonce_hex="$(send_discovery "$NSREG_HOST_UDP")"
sleep 0.1
resp="$(post_session \
        "{\"user\":\"bob\",\"discovery_nonce\":\"$nonce_hex\"}")"
token="$(extract_json_field "$resp" '.token')"

# Sanity: no token => 401 (covered by 02- but worth re-asserting
# here so a failure of this test points at exactly which step
# broke).
unauth_resp="$(h3_get "$NSREG_HOST_UDP" "/v2/")"
unauth_code="$(printf '%s' "$unauth_resp" | awk '/--HTTP_CODE--/{getline; print}')"
assert_eq "$unauth_code" "401" "control: HTTP/3 without token still 401"

# Repeat the same request WITH the token; must flip to 200.
auth_resp="$(h3_get "$NSREG_HOST_UDP" "/v2/" "Bearer $token")"
auth_code="$(printf '%s' "$auth_resp" | awk '/--HTTP_CODE--/{getline; print}')"
assert_eq "$auth_code" "200" "HTTP/3 with token: GET /v2/"
