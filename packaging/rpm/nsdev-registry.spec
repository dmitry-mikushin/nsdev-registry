%{!?pkg_version: %global pkg_version 0.1.0}

# Pre-built static Go binary in the source tarball; disable the
# default debug-package generation that otherwise fails with empty
# debugsourcefiles.list.
%global debug_package %{nil}

# Fallback definitions for hosts that don't ship systemd-rpm-macros
# (Arch's rpm-tools, minimal rpmbuild containers). systemd-equipped
# distros (Rocky, Fedora, openSUSE) override these from their own
# macro files and pick up the canonical paths.
%{!?_unitdir:    %global _unitdir    /usr/lib/systemd/system}
%{!?_libexecdir: %global _libexecdir /usr/libexec}

# systemd scriptlet macro stubs for non-rpm-on-systemd hosts. The
# real macros call systemctl preset/disable/restart on install /
# upgrade / removal; when they're missing we no-op rather than fail
# the build — the actual %post / %preun / %postun work happens on the
# target host's RPM database which DOES carry the macros.
%{!?systemd_post:                %global systemd_post()                :%nil}
%{!?systemd_preun:               %global systemd_preun()               :%nil}
%{!?systemd_postun_with_restart: %global systemd_postun_with_restart() :%nil}

Name:           nsdev-registry
Version:        %{pkg_version}
Release:        1%{?dist}
Summary:        OCI Distribution v2 registry with HTTP/3 + ssh-bridged handshake

License:        Apache-2.0
URL:            https://github.com/dmitry-mikushin/nsdev-registry
Source0:        nsdev-registry-%{version}-linux-amd64.tar.gz

BuildArch:      x86_64

Requires:       openssl
Requires:       shadow-utils
Requires:       systemd

# systemd-rpm-macros provides %_unitdir + the %systemd_post family. On
# distros that ship them (Rocky, Fedora, openSUSE) this pulls them in;
# on rpmbuild-only hosts the macro stubs above keep the build going.
%{?systemd_requires}

%description
nsdev-registry is a fork of distribution/distribution (the upstream
container registry) extended with a parallel HTTP/3-over-QUIC listener
on the same port as the standard TCP listener, an ssh-bridged
handshake endpoint that issues short-lived bearer tokens, and a
STUN-less hole-punching discovery protocol. Together they let
nsdev-push speak HTTP/3 directly to the registry over UDP at full
wire-line bandwidth while standard tooling (podman pull, docker push,
crane, oras) continues to work unchanged over TCP.

This package ships:
  * /usr/bin/nsdev-registry — the daemon binary
  * /usr/libexec/nsdev-registry/init-cert.sh — first-run TLS bootstrap
  * /etc/nsdev-registry/config.yml — example configuration
  * /usr/lib/systemd/system/nsdev-registry.service — systemd unit

%prep
%setup -q -n nsdev-registry-%{version}-linux-amd64

%build
# Pre-built binary in the source tarball; nothing to compile.

%install
install -d %{buildroot}%{_bindir}
install -m 0755 bin/nsdev-registry %{buildroot}%{_bindir}/nsdev-registry

install -d %{buildroot}%{_libexecdir}/%{name}
install -m 0755 libexec/init-cert.sh %{buildroot}%{_libexecdir}/%{name}/init-cert.sh

install -d %{buildroot}%{_sysconfdir}/%{name}
install -m 0640 etc/config.yml.example \
    %{buildroot}%{_sysconfdir}/%{name}/config.yml

install -d %{buildroot}%{_unitdir}
install -m 0644 lib/systemd/system/%{name}.service \
    %{buildroot}%{_unitdir}/%{name}.service

install -d %{buildroot}%{_docdir}/%{name}
install -m 0644 LICENSE   %{buildroot}%{_docdir}/%{name}/LICENSE
install -m 0644 README.md %{buildroot}%{_docdir}/%{name}/README.md

%files
%{_bindir}/nsdev-registry
%dir %{_libexecdir}/%{name}
%{_libexecdir}/%{name}/init-cert.sh
%dir %{_sysconfdir}/%{name}
%config(noreplace) %{_sysconfdir}/%{name}/config.yml
%{_unitdir}/%{name}.service
%license %{_docdir}/%{name}/LICENSE
%doc %{_docdir}/%{name}/README.md

%pre
# Create the unprivileged system user the daemon runs as. The
# StateDirectory= in the unit creates /var/lib/nsdev-registry with
# correct ownership at first start; we only need the user/group here.
getent group nsdev-registry >/dev/null || \
    groupadd --system nsdev-registry
getent passwd nsdev-registry >/dev/null || \
    useradd --system --gid nsdev-registry \
            --home-dir /var/lib/nsdev-registry --shell /sbin/nologin \
            --comment "nsdev-registry daemon" nsdev-registry
exit 0

%post
%systemd_post nsdev-registry.service

%preun
%systemd_preun nsdev-registry.service

%postun
%systemd_postun_with_restart nsdev-registry.service

%changelog
* Thu May 21 2026 Dmitry Mikushin <dmitry@kernelgen.org> - 0.1.0-1
- Initial RPM packaging of nsdev-registry (see CHANGELOG / git log
  for the full feature set: HTTP/3 listener, ssh-bridged handshake,
  bearer-token middleware, STUN-less discovery protocol, config
  split for TCP/QUIC bind + advertise).
