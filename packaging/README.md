# nsdev-registry packaging

Everything needed to ship the registry binary as a system package on
any of three formats — `.deb`, `.rpm`, pip wheel. All three carry the
SAME Go binary built from the working tree; you pick the delivery
vehicle based on the target host's package manager.

For end-to-end install instructions including the pip path, see the
"Installation" section in [../NEXTSILICON.md](../NEXTSILICON.md).
This directory holds the build-time pieces.

## Build

```bash
make -C packaging packages   # → debbuild/*.deb + rpmbuild/RPMS/x86_64/*.rpm
make -C packaging deb        # just .deb
make -C packaging rpm        # just .rpm
make -C packaging binary     # just the Go binary into ./build/
make -C packaging clean      # drop all generated artefacts
```

Both package types depend only on `binary`, so a one-line bump of
the registry source → reproducible rebuild.

The version string comes from `git describe --tags --always`; pass
`VERSION=v1.2.3 make -C packaging packages` to override (useful for
release-cuts that pre-date the tag push).

## What goes in each package

```
/usr/bin/nsdev-registry                          ← daemon binary (Go)
/usr/libexec/nsdev-registry/init-cert.sh         ← first-run TLS bootstrap
/etc/nsdev-registry/config.yml                   ← default config (conffile)
/lib/systemd/system/nsdev-registry.service       ← systemd unit
/usr/share/doc/nsdev-registry/{LICENSE,README}   ← reference material
                                                   (LICENSE → /usr/share/doc/<pkg>/copyright on DEB)
```

Both formats:

* create the unprivileged `nsdev-registry` system user/group in their
  pre-install scriptlet (DEB postinst, RPM `%pre`),
* enable + start the systemd unit on a fresh install,
* `try-restart` (not blindly restart) on upgrade so a running
  service picks up the new binary without flipping operator-set
  state.

On purge / `dnf remove`, the systemd unit is stopped + disabled and
the unit file removed. On `--purge` (DEB) only, `/var/lib/nsdev-registry`
and the system user are also dropped. `/etc/nsdev-registry/tls.{crt,key}`
is intentionally kept across uninstalls so a real PKI cert that the
operator dropped in isn't lost.

## File map

```
packaging/
├─ Makefile                          orchestrator: binary → tarball → rpm + deb
├─ nsdev-registry.service            systemd unit shared by both formats
├─ config.yml.example                default config shipped at /etc/...
├─ init-cert.sh                      generates self-signed cert if absent
├─ rpm/
│  └─ nsdev-registry.spec            RPM spec with fallback macro defs
│                                    (works on hosts without systemd-rpm-macros)
└─ deb/
   └─ nsdev-registry/
      ├─ control.in                  @PKG_VERSION@ substituted at build
      ├─ postinst                    create user + systemctl enable --now
      ├─ prerm                       systemctl disable --now on uninstall
      ├─ postrm                      daemon-reload + purge cleanup
      └─ conffiles                   marks config.yml as a conffile
```

## Why the deliberate sprawl

Every site we deploy nsdev-registry to is using some other package
manager. ops teams that already manage their hosts with apt or dnf
should be able to install with one familiar command, get correct
systemd integration, and survive `apt full-upgrade` /
`dnf check-update` cleanly. The pip wheel exists for the
workstation operator who installs ad-hoc and prefers the
[nsdev-server / nsdev-push pattern](https://github.com/dmitry-mikushin/nsdev-registry/blob/nsdev-main/NEXTSILICON.md#installation)
they already know.

Three vehicles, one binary, one config, one systemd unit. Adding a
fourth format (Snap, Flatpak, MSI) would be the same shape — drop a
new subdirectory plus a new Makefile target.

## Not yet shipped

* Signed packages. Both formats produce unsigned artefacts; consumers
  on a trusted network skip verification. Adding `rpm-sign` /
  `dpkg-sig` is a separate decision tied to public-facing
  distribution.
* GitHub release CI. `make -C packaging packages && gh release
  upload` is a 2-line workflow but is left out of this commit so the
  repo doesn't start writing to releases on every push.
* mTLS-based ACME cert provisioning out of the box. The
  init-cert.sh ships a self-signed; real PKI is operator-managed for
  now.
