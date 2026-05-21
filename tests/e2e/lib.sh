#!/usr/bin/env bash
#
# Shared helpers for the nsdev-registry E2E suite. Source from every
# test script:
#
#     #!/usr/bin/env bash
#     set -euo pipefail
#     LIB="$(dirname "$0")/lib.sh"
#     # shellcheck source=lib.sh
#     . "$LIB"
#
# The helpers wrap docker/podman ergonomics (image build, container
# lifecycle, port discovery, log capture) plus the wire-protocol
# primitives our tests need (discovery packet, session POST, curl
# HTTP/3 with bearer token). They are deliberately small and
# explicit; tests should read like specifications, not like glue.

set -euo pipefail

# ---------------------------------------------------------------------------
# Constants

# Container image tag built locally by ensure_image. Bumping this
# forces a rebuild on the next test run.
NSREG_IMAGE_TAG="${NSREG_IMAGE_TAG:-nsdev-registry-e2e:dev}"

# Container name prefix; each test scenario picks its own suffix so
# parallel runs don't collide.
NSREG_NAME_PREFIX="${NSREG_NAME_PREFIX:-nsdev-registry-e2e}"

# Use podman if available (preferred — rootless on dev workstations);
# otherwise fall back to docker.
DOCKER="${DOCKER:-$(command -v podman 2>/dev/null || command -v docker 2>/dev/null || echo docker)}"

# Discovery wire-protocol constants must stay in lockstep with
# internal/discovery/store.go::MagicV1.
NSREG_DISCOVERY_MAGIC=$'\x00NSDEV-DISCOVERY-V1\n'
NSREG_DISCOVERY_NONCE_LEN=32 # bytes

# ---------------------------------------------------------------------------
# Image / container lifecycle

# ensure_image: build the nsdev-registry container image from the
# current working tree if it isn't already present. Re-run after any
# source change to pick up the diff.
ensure_image() {
    if "$DOCKER" image inspect "$NSREG_IMAGE_TAG" >/dev/null 2>&1; then
        return 0
    fi
    local repo_root
    repo_root="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"
    echo "[lib] building image $NSREG_IMAGE_TAG from $repo_root" >&2
    "$DOCKER" build \
        -t "$NSREG_IMAGE_TAG" \
        -f "$repo_root/tests/e2e/Dockerfile" \
        "$repo_root"
}

# start_registry <scenario-name> -> sets NSREG_CONTAINER, NSREG_HOST_TCP,
# NSREG_HOST_UDP, NSREG_LOG_PATH. Caller is responsible for calling
# stop_registry in a trap so the container is reaped even on test
# failure.
start_registry() {
    local scenario="$1"
    NSREG_CONTAINER="${NSREG_NAME_PREFIX}-${scenario}-$$"
    NSREG_LOG_PATH="${TMPDIR:-/tmp}/${NSREG_CONTAINER}.log"

    "$DOCKER" run -d --rm \
        --name "$NSREG_CONTAINER" \
        -p '127.0.0.1::5000/tcp' \
        -p '127.0.0.1::5000/udp' \
        "$NSREG_IMAGE_TAG" >/dev/null

    # Resolve the host-side mapped ports. podman/docker have slightly
    # different output formats so we ask each protocol separately.
    NSREG_HOST_TCP="$("$DOCKER" port "$NSREG_CONTAINER" 5000/tcp | head -1 | awk -F: '{print $NF}')"
    NSREG_HOST_UDP="$("$DOCKER" port "$NSREG_CONTAINER" 5000/udp | head -1 | awk -F: '{print $NF}')"
    if [ -z "$NSREG_HOST_TCP" ] || [ -z "$NSREG_HOST_UDP" ]; then
        echo "[lib] failed to discover host ports" >&2
        "$DOCKER" logs "$NSREG_CONTAINER" >&2 || true
        return 1
    fi

    wait_for_tcp_ready "$NSREG_HOST_TCP"

    export NSREG_CONTAINER NSREG_HOST_TCP NSREG_HOST_UDP NSREG_LOG_PATH
}

# stop_registry: capture the container's log into NSREG_LOG_PATH (so
# tests can grep for expected log lines) and then `kill`+`rm`.
# Idempotent — safe to call from an EXIT trap.
stop_registry() {
    if [ -n "${NSREG_CONTAINER:-}" ] && "$DOCKER" inspect "$NSREG_CONTAINER" >/dev/null 2>&1; then
        "$DOCKER" logs "$NSREG_CONTAINER" >"$NSREG_LOG_PATH" 2>&1 || true
        "$DOCKER" kill "$NSREG_CONTAINER" >/dev/null 2>&1 || true
    fi
}

# wait_for_tcp_ready <host_port>: poll the registry's /v2/ endpoint
# over TCP/HTTPS until we get a 200, or fail after 30 attempts. The
# QUIC listener comes up at roughly the same time as TCP, so this is
# a good proxy for "the registry is fully serving".
wait_for_tcp_ready() {
    local port="$1"
    for _ in $(seq 1 30); do
        if curl -ks -o /dev/null -w '%{http_code}\n' \
               --max-time 2 \
               "https://127.0.0.1:${port}/v2/" 2>/dev/null \
               | grep -q '^200$'; then
            return 0
        fi
        sleep 0.2
    done
    echo "[lib] registry never became ready on 127.0.0.1:$port" >&2
    return 1
}

# ---------------------------------------------------------------------------
# Wire-protocol primitives

# send_discovery <udp_port> [nonce_hex] -> echoes the nonce_hex used.
# Generates a random 32-byte nonce if not supplied, prepends the
# magic, and sends one UDP datagram from an ephemeral local port. The
# registry's quic.Transport non-QUIC packet receiver will record the
# (nonce -> observed_src_addr) mapping; subsequent /v3/sessions calls
# with the same nonce will resolve to that observed address.
send_discovery() {
    local port="$1"
    local nonce_hex="${2:-}"
    if [ -z "$nonce_hex" ]; then
        nonce_hex="$(python3 -c 'import os,sys; sys.stdout.write(os.urandom(32).hex())')"
    fi
    python3 - "$port" "$nonce_hex" <<'PY'
import socket, sys, binascii
port = int(sys.argv[1])
nonce_hex = sys.argv[2]
magic = b'\x00NSDEV-DISCOVERY-V1\n'
nonce = binascii.unhexlify(nonce_hex)
assert len(nonce) == 32
s = socket.socket(socket.AF_INET, socket.SOCK_DGRAM)
s.sendto(magic + nonce, ('127.0.0.1', port))
PY
    echo "$nonce_hex"
}

# post_session <body_json> -> emits the JSON response to stdout. Bails
# on non-2xx. The call is made from INSIDE the registry container via
# `podman exec` rather than from the host, because /v3/sessions
# enforces a loopback-only guard (registry/handlers/session.go) — it
# refuses anything whose RemoteAddr isn't 127.0.0.1 / ::1. In
# production the ssh-launched `nsdev-registry session` subcommand
# runs on the registry host and posts to loopback, so simulating
# that here is the right thing — testing from the docker bridge
# gateway would be testing a NON-supported deployment.
post_session() {
    local body="$1"
    "$DOCKER" exec "$NSREG_CONTAINER" \
        curl -ks --fail-with-body \
        -X POST \
        -H 'Content-Type: application/json' \
        -d "$body" \
        "https://127.0.0.1:5000/v3/sessions"
}

# h3_get <udp_port> <path> [authorization_value] -> writes
# `<http_code>\n<headers>\n\n<body>` to stdout. Uses curl --http3-only
# (requires curl built with HTTP/3 support; the e2e runner checks for
# it once via require_http3_curl).
h3_get() {
    local port="$1"
    local path="$2"
    local auth="${3:-}"
    local extra=()
    if [ -n "$auth" ]; then
        extra+=(-H "Authorization: $auth")
    fi
    curl -ks --http3-only \
        --resolve "localhost:${port}:127.0.0.1" \
        "${extra[@]}" \
        -w '\n--HTTP_CODE--\n%{http_code}\n' \
        "https://localhost:${port}${path}"
}

# require_http3_curl: bails out with a friendly skip message if curl
# wasn't built with HTTP/3 support. Tests should call this early.
require_http3_curl() {
    if ! curl --http3-only --version >/dev/null 2>&1 \
        && ! curl --version 2>/dev/null | grep -qE 'Features:.*HTTP3'; then
        echo "[skip] curl does not support --http3-only; install curl-http3 or libcurl with HTTP/3" >&2
        exit 77 # autotools convention: SKIP
    fi
}

# ---------------------------------------------------------------------------
# Assertions
#
# Bash-friendly assertions that print a clear diagnostic on failure so
# the test reads as a specification:
#
#     assert_eq "$got" "$want" "discovery_used flag"

assert_eq() {
    local got="$1" want="$2" label="${3:-value}"
    if [ "$got" != "$want" ]; then
        echo "[FAIL] $label: got=$got, want=$want" >&2
        return 1
    fi
    echo "[OK]   $label = $want"
}

assert_contains() {
    local haystack="$1" needle="$2" label="${3:-substring}"
    case "$haystack" in
        *"$needle"*) echo "[OK]   $label contains '$needle'" ;;
        *)
            echo "[FAIL] $label does NOT contain '$needle'; haystack=$haystack" >&2
            return 1
            ;;
    esac
}

# extract_json_field <json> <jq-expr>: thin wrapper around jq so
# tests don't have to repeat the pipe boilerplate.
extract_json_field() {
    printf '%s' "$1" | jq -r "$2"
}
