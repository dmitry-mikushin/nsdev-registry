#!/usr/bin/env bash
#
# 01-tcp-http2-baseline.sh
#
# WHAT THIS TEST PRESERVES
# ------------------------
# That the standard OCI Distribution v2 surface over TCP/HTTPS keeps
# working unchanged after we added the QUIC listener. Any client that
# can speak HTTP/2 — `podman pull`, `docker push`, `crane`, `oras`,
# `curl --http2` — must continue to reach `/v2/` and get the
# Distribution API marker.
#
# WHY IT MATTERS
# --------------
# The whole reason we kept the TCP listener alive is so compute hosts
# (vm43, vm-cpu04, ...) can do `podman pull <registry>:5000/...`
# without learning HTTP/3. If a future refactor accidentally makes
# the TCP path require a Bearer token (the QUIC path does), this
# test fails and we get to ask: did we mean to break podman pull?
# Probably not. The QUIC path is the OPT-IN fast lane for nsdev-push;
# TCP stays the default mode.
#
# WHEN TO BREAK THIS TEST
# -----------------------
# Only if we make a deliberate decision that the registry no longer
# wants to be reachable by standard tooling — e.g. moving to a fully
# proprietary protocol. That would also require updating
# NEXTSILICON.md's positioning of the fork.

set -euo pipefail
LIB="$(dirname "$0")/lib.sh"
# shellcheck source=lib.sh
. "$LIB"

ensure_image
trap stop_registry EXIT
start_registry "tcp-baseline"

resp="$(curl -ks -w '\n--CODE--\n%{http_code}\n%{http_version}\n' \
        --http2 \
        "https://127.0.0.1:${NSREG_HOST_TCP}/v2/")"

code="$(printf '%s' "$resp" | awk '/--CODE--/{getline; print}')"
version="$(printf '%s' "$resp" | awk '/--CODE--/{getline; getline; print}')"
api_marker="$(curl -ks -I --http2 \
              "https://127.0.0.1:${NSREG_HOST_TCP}/v2/" \
              | tr -d '\r' | grep -i '^docker-distribution-api-version:' || true)"

assert_eq "$code"    "200"        "GET /v2/ status over TCP/HTTP-2"
assert_eq "$version" "2"          "negotiated HTTP version"
assert_contains "$api_marker" "registry/2.0" \
                "Docker-Distribution-API-Version header"
