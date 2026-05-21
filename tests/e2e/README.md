# nsdev-registry — end-to-end test harness

This directory contains the containerised end-to-end tests that act
as the **executable specification** for nsdev-registry. The reason
they exist is consistency-across-time: if six months from now we
forget WHY some design decision was made and reach for a tempting
refactor, one of these tests will fail and force us to stop and ask
"did we mean to break that?"

Each test is therefore not just an assertion — it carries a doc
block explaining:

- **What this test preserves** — the design decision it pins.
- **Why it matters** — the user-visible behaviour that depends on it.
- **When to break this test** — what kind of deliberate decision
  would legitimately invalidate the assertion (and what other parts
  of the codebase you'd need to update alongside).

If you're tempted to delete a test because it fails after a refactor,
read the "When to break" section first. If your refactor isn't in
that list, the test is doing its job and you should revert.

## Test layout

```
tests/e2e/
  Dockerfile                       — multi-stage build of nsdev-registry from src
  entrypoint.sh                    — gens TLS cert + minimal config, execs daemon
  lib.sh                           — shared helpers (image build, container life,
                                     discovery packet, session POST, h3 GET,
                                     assertions)
  run-all.sh                       — orchestrator
  01-tcp-http2-baseline.sh         — standard OCI v2 over TCP/HTTP/2 still works
  02-quic-requires-bearer-token.sh — QUIC handler rejects no-auth requests
  03-discovery-then-handshake.sh   — STUN-less discovery + session handshake
  04-token-allows-http3-access.sh  — issued token actually gates the QUIC path
  05-discovery-beats-client-claim.sh — observation wins over client guess
  06-discovery-miss-falls-back-to-claim.sh — graceful degradation path
  README.md                        — this file
```

## How to run

```bash
# Run every scenario:
tests/e2e/run-all.sh

# Run one scenario:
tests/e2e/03-discovery-then-handshake.sh

# Force a rebuild after touching the source tree:
podman image rm nsdev-registry-e2e:dev
tests/e2e/run-all.sh
```

Requirements on the host:

- `podman` or `docker` (auto-detected; podman preferred).
- `curl` built with HTTP/3 support (`curl --http3-only --version`
  returns a usable invocation). Tests that need HTTP/3 call
  `require_http3_curl` and gracefully **skip** (exit 77) if it's
  absent.
- `python3` (for crafting discovery packets — too low-level to want
  to do in pure bash).
- `jq` (for extracting fields from the session response).

## Adding a new scenario

1. Pick the next free number and copy an existing test as a starting
   point (`05-` or `06-` are good templates).
2. Write the doc block FIRST. "What this test preserves" should be
   one paragraph; if you can't write it, the test isn't ready.
3. Use `lib.sh` helpers (`ensure_image`, `start_registry`,
   `stop_registry`, `send_discovery`, `post_session`, `h3_get`,
   `assert_eq`, `assert_contains`, `extract_json_field`) rather than
   inlining docker/curl/jq invocations — keeps tests reading as
   specifications.
4. `chmod +x` the new file and verify `run-all.sh` picks it up.

## Why containers (not testcontainers-go)

We chose bash + container CLI because each test reads top-to-bottom
like a runbook: a developer can copy-paste any single step into
their own shell and reproduce it manually for debugging. A Go test
framework hides the protocol details behind helper structs, which
is great for tight unit coverage but actively hostile to "I forgot
how the discovery packet works, let me re-derive it from the
test". The audience for this directory is a human who needs to
remember the design, not a CI system trying to be efficient.

If we ever grow the suite to dozens of scenarios where parallel
execution matters more than self-documentation, the natural next
step is to keep this directory AS-IS (it's the spec) and add a
`go test ./tests/e2e-go/...` next to it that re-implements the
scenarios for CI speed.

## What this directory is NOT

- Not a load test (single connection per scenario).
- Not a chaos test (no induced failures, killed processes, slow
  links). Phase 5 might add `06-` something for those.
- Not a packaging test (doesn't verify the .deb/.rpm install).
- Not a multi-host test (everything runs in one container on the
  Docker bridge — symmetric-NAT-equivalent scenarios are *modelled*
  via 05-/06-, not reproduced with two real NAT routers).
