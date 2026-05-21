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

## Installation

Three delivery paths are maintained from this repo, all producing
the same Go binary + the same systemd unit + the same default
config. Pick whichever matches your host's package management.

### 1. pip / pipx wheel (preferred for workstations and ad-hoc hosts)

Same delivery pattern as `nsdev-server` and `nsdev-push` in the
parent nsdev tree: a single wheel that bundles the binary as a data
file, plus a thin Python shim. Needs Python 3.8+, `pipx`, and a Go
toolchain to build the wheel.

```bash
# Build the wheel from a source checkout (Go required only here).
pipx run --spec build pyproject-build --wheel .
# -> dist/nsdev_registry-<ver>-py3-none-any.whl

# Install (no root, per-user PATH).
pipx install dist/nsdev_registry-<ver>-py3-none-any.whl

# One-shot system bootstrap. Requires root; creates the
# nsdev-registry user, drops the binary into /usr/bin, the systemd
# unit into /lib/systemd/system, the default config into
# /etc/nsdev-registry/config.yml (noreplace — keeps your edits),
# and `systemctl enable --now`s the service. Skips the systemctl
# step under --no-enable for container image builds.
sudo $(pipx environment --value PIPX_BIN_DIR)/nsdev-registry-init
```

After `nsdev-registry-init`, `systemctl status nsdev-registry`
should show the service active and listening on `5000/tcp` +
`5000/udp`.

### 2. Debian / Ubuntu (.deb)

For apt-based clusters where ops prefers their distro's package
manager.

```bash
# On a build host (Debian/Ubuntu/Arch — needs dpkg-deb, Go toolchain).
make -C packaging deb
# -> packaging/debbuild/nsdev-registry_<ver>-1_amd64.deb

# On the target host.
sudo dpkg -i nsdev-registry_*.deb
sudo apt-get install -f             # pull deps if dpkg complained
sudo systemctl status nsdev-registry
```

The postinst creates the `nsdev-registry` system user, runs
`systemctl enable --now`, and leaves a self-signed cert in
`/etc/nsdev-registry/tls.{crt,key}` via the unit's ExecStartPre
hook on first start.

### 3. Rocky / Fedora / RHEL (.rpm)

```bash
# On a build host (needs rpmbuild + Go toolchain).
make -C packaging rpm
# -> packaging/rpmbuild/RPMS/x86_64/nsdev-registry-<ver>-1.x86_64.rpm

# On the target host.
sudo dnf install nsdev-registry-*.rpm
sudo systemctl status nsdev-registry
```

Same lifecycle hooks as the .deb (`%pre` creates the user,
`%systemd_post` enables the service).

### 4. Source (developers)

```bash
git clone https://github.com/dmitry-mikushin/nsdev-registry
cd nsdev-registry
git checkout nsdev-main

# Just the binary, no install.
make -C packaging binary
./packaging/build/nsdev-registry serve packaging/config.yml.example

# Full test loop:
go test -race ./internal/... ./registry/...   # unit tests
bash tests/e2e/run-all.sh                     # containerised E2E
```

## Quick start (operator)

After installing on the bastion/registry host by any of the methods
above:

```bash
# On the operator's laptop / build host: just push.
nsdev push --tunnel ns-tun toolchain/rocky9:pr35867
```

No `--stun-server`, no manual port forwards, no relay binary on the
bastion — `nsdev push` runs the discovery + ssh handshake silently
and attaches the bearer token to every OCI request.

## Config knobs

The default config (`/etc/nsdev-registry/config.yml` after
install) is heavily commented; the knobs operators usually touch
first:

| Field | Default | When to change |
|---|---|---|
| `storage.filesystem.rootdirectory` | `/var/lib/nsdev-registry` | Point at shared NFS for cluster-wide pull |
| `http.tls.certificate` / `.key` | `/etc/nsdev-registry/tls.{crt,key}` | Replace self-signed cert with PKI / ACME |
| `http.addr` | `0.0.0.0:5000` | Lock TCP to `127.0.0.1` to require ssh-tunnel for `podman pull` |
| `http.quic.addr` | `0.0.0.0:5000` | Keep externally reachable for direct `nsdev push` |
| `http.quic.advertise` | (bind addr) | Set when registry is behind 1:1 NAT |
