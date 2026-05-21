#!/usr/bin/env bash
#
# 02-quic-requires-bearer-token.sh
#
# WHAT THIS TEST PRESERVES
# ------------------------
# That the QUIC (HTTP/3) listener gates EVERY request behind a Bearer
# token. Unlike the TCP listener, which keeps the upstream
# distribution auth model (none/htpasswd/token-issuer/silly), the
# QUIC listener is wrapped in internal/tokenauth.Middleware and
# rejects any request that arrives without `Authorization: Bearer
# <token>`. Tokens are issued only through the loopback-bound
# /v3/sessions handshake, so the QUIC port can be safely exposed to
# the network: an attacker with line-of-sight UDP can do nothing
# more than collect 401s.
#
# WHY IT MATTERS
# --------------
# The whole identity story for the cluster rests on this gate. If a
# future refactor inadvertently mounts the OCI handler on the QUIC
# tree without tokenauth, or accepts a different auth scheme that
# bypasses the session store, this test fails. The diagnostic in the
# 401 body must also stay clear ("missing Authorization header" /
# "token unknown or expired") so operators have a chance to
# debug — we have spent too many cycles on "auth said no, no detail".
#
# WHEN TO BREAK THIS TEST
# -----------------------
# Only if we add a second valid auth path on QUIC (e.g. Phase 3 mTLS
# client certs as an alternative to bearer tokens) AND adopt it. In
# that case this test gets a sibling 02b-mtls-also-works.sh; the
# bearer-required path itself does not change.

set -euo pipefail
LIB="$(dirname "$0")/lib.sh"
# shellcheck source=lib.sh
. "$LIB"

require_http3_curl
ensure_image
trap stop_registry EXIT
start_registry "quic-401"

response="$(h3_get "$NSREG_HOST_UDP" "/v2/")"
code="$(printf '%s' "$response" | awk '/--HTTP_CODE--/{getline; print}')"

assert_eq "$code" "401" "GET /v2/ over HTTP/3 without Authorization"

# The challenge header is part of the contract: anything that speaks
# OCI Distribution v2 looks at WWW-Authenticate to decide whether to
# try again with credentials. Confirm we still emit it.
challenge="$(curl -ksI --http3-only \
             --resolve "localhost:${NSREG_HOST_UDP}:127.0.0.1" \
             "https://localhost:${NSREG_HOST_UDP}/v2/" \
             | tr -d '\r' | grep -i '^www-authenticate:' || true)"

assert_contains "$challenge" 'Bearer' \
                "WWW-Authenticate challenge advertises Bearer scheme"
