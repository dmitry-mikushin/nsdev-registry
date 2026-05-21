# nsdev-registry

A fork of [distribution/distribution](https://github.com/distribution/distribution)
extended with a QUIC (HTTP/3) listener and NextSilicon-specific endpoints, used
as the receiver of `nsdev push` instead of stock `registry:2`.

## Why fork

The stock registry only speaks OCI Distribution v2 over HTTP/1.1 or HTTP/2
on TCP. NSdev's push pipeline currently tunnels OCI v2 traffic through an
ssh-coordinated relay that proxies QUIC bi-streams into a local TCP socket
of a separate `registry:2` container. That arrangement has three costs:

* **TCP-in-TCP**: ssh tunnels are TCP, so inner TCP retransmits compound
  with outer TCP retransmits and bandwidth collapses cubically under loss.
* **Double crypto**: ssh's ChaCha20-Poly1305 sits on top of the inner
  TLS, doubling per-byte CPU at the same time we want to push more bytes
  faster.
* **Two moving parts on the receiver**: the relay binary AND a separately
  managed `registry:2` container, with no shared identity, no shared GC
  policy, no shared storage layout.

This fork collapses the three into one process. Highlights:

* **TCP:5000** keeps speaking standard OCI v2 + HTTP/2 — `podman pull`,
  `docker push`, `crane`, `oras` all keep working unchanged.
* **UDP:5000** speaks HTTP/3 over QUIC, serving the same OCI v2 handler
  tree. `nsdev push` connects directly with no ssh dance, no relay
  hop, no TCP-in-TCP, no extra crypto layer. Standard tooling
  (`curl --http3`) also works.
* **SSH plays the role of CA, not transport**: operator runs
  `nsdev-registry sign-cert` once via ssh; receiver checks `$SSH_USER`
  against authorized operators and issues a short-lived mTLS client
  certificate signed by the registry's internal CA. Subsequent pushes
  use the cert; ssh is out of the data path.
* **`/v3/` extensions** (planned) for NextSilicon-specific concepts:
  instances, hosts, leases, retention — things the OCI spec has no
  notion of but that we need to coordinate ship/push/cleanup across the
  cluster.

## Upstream relationship

* `main` branch tracks `distribution/distribution`'s `main` and only ever
  receives merges from upstream — no NextSilicon-specific commits land
  there. Use it as the upstream-tracking branch.
* `nsdev-main` carries everything specific to this fork: QUIC listener,
  `/v3/` endpoints, SSH-bootstrapped mTLS, packaging changes. All
  feature branches merge here. Releases are tagged on this branch.
* Periodic `git merge main` into `nsdev-main` to absorb upstream
  improvements.

## Roadmap

| Phase | Status | Scope |
| ----- | ------ | ----- |
| 0     | done   | Fork repo, set up branching layout, write this doc |
| 1     | done   | Add `http3.Server` on UDP:5000 alongside TCP listener; share handler tree |
| 2     | done   | SSH-bridged handshake (`POST /v3/sessions` + `nsdev-registry session` subcommand), bearer-token middleware on the QUIC tree, STUN-less discovery protocol on the same UDP socket, config split for separate TCP/QUIC bind + advertise. Nsdev-push (client) refactored to drop its own ssh+relay+stun code and speak HTTP/3 directly. |
| 3     | tbd    | Optional mTLS for offline operators (long-lived SSH-bootstrapped client cert as an alternative to the per-push bearer token) |
| 4     | tbd    | `/v3/` API: instances, hosts, leases, retention |
| 5     | tbd    | Custom NFS storage driver (atomic tag rename via symlink, hardlink across repos for same digest) |

See `ROADMAP.md` for the upstream roadmap (unchanged).

## Wire protocols at a glance

```
                  ┌─────────────────────────── nsdev-registry ───────────────────────────┐
                  │                                                                       │
                  │   TCP :5000  ── HTTP/1.1, HTTP/2 ──▶  /v2/* (OCI)  ──▶  storage      │
                  │                                       /v3/sessions (loopback only)    │
                  │                                                                       │
                  │   UDP :5000  ── HTTP/3   ─────────▶  /v2/* (OCI, Bearer-auth)        │
                  │                  via quic.Transport                                   │
                  │                  │                                                    │
                  │                  └─ non-QUIC packets ─▶ discovery.Store               │
                  │                                          (nonce → src_addr)           │
                  └──────────────────▲────────────────────────────────────────────────────┘
                                     │ probes back to client (NAT-punch)
                                     │
   nsdev-push  ──── ssh handshake (~300 ms) ────────────────▶ nsdev-registry session
                  ──── HTTP/3 OCI v2 over QUIC ─────────────▶ storage
```

The discovery datagram is the STUN-equivalent: client sends one 55-byte
packet with magic + 32-byte nonce before the ssh handshake, server's
non-QUIC packet receiver records the kernel-observed (ip, port), and
the handshake response carries the bearer token plus confirmation that
the discovery succeeded.

## Quick start (operator)

```bash
# On the bastion / registry host: install + run nsdev-registry as systemd
# (Phase 4 packaging will produce .deb/.rpm; for now build from source).
nsdev-registry serve /etc/nsdev-registry/config.yml &

# On the operator's laptop / build host: just push.
nsdev push --tunnel ns-tun toolchain/rocky9:pr35867
```

No `--stun-server`, no manual port forwards, no relay binary on the
bastion — `nsdev push` runs the discovery + ssh handshake silently and
attaches the bearer token to every OCI request.
