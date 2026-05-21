# nsdev-registry packaging — RPM + DEB from a single pre-built binary.
#
# Targets:
#
#   make -C packaging binary    — build /usr/bin/nsdev-registry into ./build/
#                                 (calls the upstream Makefile under the hood)
#   make -C packaging tarball   — stage the source tree the specs / DEBIAN
#                                 control expect: bin/, libexec/, etc/,
#                                 lib/systemd/system/, plus README + LICENSE
#   make -C packaging rpm       — build the RPM into rpmbuild/RPMS/x86_64/
#   make -C packaging deb       — build the DEB into debbuild/
#   make -C packaging packages  — both
#   make -C packaging clean     — drop build artefacts
#
# `make packages` is the one CI / a release-cutter calls.
#
# Both package types embed THE SAME binary built once; there is no
# arch- or distro-specific compile. The tarball staging is shared so
# rpm and deb see byte-identical content.

ROOT_DIR := $(abspath $(dir $(lastword $(MAKEFILE_LIST)))/..)
PKG_DIR  := $(ROOT_DIR)/packaging

# Single source of truth for the version string. `git describe`
# returns the latest tag's name (e.g. "v0.1.0") plus a commit-count
# suffix on untagged commits; we strip a leading "v" so the RPM /
# DEB version field is a plain semver-ish number.
VERSION     ?= $(shell cd $(ROOT_DIR) && git describe --tags --always --dirty 2>/dev/null || echo 0.1.0)
PKG_VERSION := $(patsubst v%,%,$(VERSION))

ARCH        := amd64
RPM_TOPDIR  := $(PKG_DIR)/rpmbuild
DEB_TOPDIR  := $(PKG_DIR)/debbuild
BUILD_DIR   := $(PKG_DIR)/build
STAGE       := nsdev-registry-$(PKG_VERSION)-linux-$(ARCH)
TARBALL     := $(STAGE).tar.gz

# RPM's Version: field disallows the '-' that `git describe` produces
# between the tag and the trailing commit count (e.g. "0.1.0-15-gabc").
# Replace it with '.' here. For tagged releases this is a no-op.
RPM_VERSION := $(subst -,.,$(PKG_VERSION))

.PHONY: binary tarball rpm deb packages clean

binary:
	@mkdir -p $(BUILD_DIR)
	cd $(ROOT_DIR) && \
	    GO111MODULE=on CGO_ENABLED=0 \
	    go build -mod=vendor -ldflags='-s -w' \
	        -o $(BUILD_DIR)/nsdev-registry \
	        ./cmd/registry
	@ls -la $(BUILD_DIR)/nsdev-registry

# Stage everything the RPM Source0 / DEB scripts expect into
# $(BUILD_DIR)/$(STAGE)/, then tar it up. The layout matches the
# specs verbatim:
#
#   bin/nsdev-registry
#   libexec/init-cert.sh
#   etc/config.yml.example
#   lib/systemd/system/nsdev-registry.service
#   LICENSE
#   README.md
$(BUILD_DIR)/$(TARBALL): binary
	rm -rf $(BUILD_DIR)/$(STAGE) $(BUILD_DIR)/$(TARBALL)
	install -d $(BUILD_DIR)/$(STAGE)/bin \
	           $(BUILD_DIR)/$(STAGE)/libexec \
	           $(BUILD_DIR)/$(STAGE)/etc \
	           $(BUILD_DIR)/$(STAGE)/lib/systemd/system
	install -m 0755 $(BUILD_DIR)/nsdev-registry \
	    $(BUILD_DIR)/$(STAGE)/bin/nsdev-registry
	install -m 0755 $(PKG_DIR)/init-cert.sh \
	    $(BUILD_DIR)/$(STAGE)/libexec/init-cert.sh
	install -m 0644 $(PKG_DIR)/config.yml.example \
	    $(BUILD_DIR)/$(STAGE)/etc/config.yml.example
	install -m 0644 $(PKG_DIR)/nsdev-registry.service \
	    $(BUILD_DIR)/$(STAGE)/lib/systemd/system/nsdev-registry.service
	install -m 0644 $(ROOT_DIR)/LICENSE   $(BUILD_DIR)/$(STAGE)/LICENSE
	install -m 0644 $(ROOT_DIR)/README.md $(BUILD_DIR)/$(STAGE)/README.md
	tar -czf $(BUILD_DIR)/$(TARBALL) -C $(BUILD_DIR) $(STAGE)
	@echo
	@echo "Created: $(BUILD_DIR)/$(TARBALL)"
	@sha256sum $(BUILD_DIR)/$(TARBALL)

tarball: $(BUILD_DIR)/$(TARBALL)

rpm: binary
	rm -rf $(RPM_TOPDIR)
	install -d $(RPM_TOPDIR)/SOURCES $(RPM_TOPDIR)/SPECS \
	           $(RPM_TOPDIR)/BUILD $(RPM_TOPDIR)/RPMS $(RPM_TOPDIR)/SRPMS
	# RPM's Source0 expects tarball named after the version with '.'s.
	# Re-stage a copy with the RPM-friendly name so the spec's
	# `%setup -q -n nsdev-registry-%{version}-linux-amd64` matches.
	$(eval RPM_STAGE := nsdev-registry-$(RPM_VERSION)-linux-$(ARCH))
	rm -rf $(BUILD_DIR)/$(RPM_STAGE) $(BUILD_DIR)/$(RPM_STAGE).tar.gz
	install -d $(BUILD_DIR)/$(RPM_STAGE)/bin \
	           $(BUILD_DIR)/$(RPM_STAGE)/libexec \
	           $(BUILD_DIR)/$(RPM_STAGE)/etc \
	           $(BUILD_DIR)/$(RPM_STAGE)/lib/systemd/system
	install -m 0755 $(BUILD_DIR)/nsdev-registry \
	    $(BUILD_DIR)/$(RPM_STAGE)/bin/nsdev-registry
	install -m 0755 $(PKG_DIR)/init-cert.sh \
	    $(BUILD_DIR)/$(RPM_STAGE)/libexec/init-cert.sh
	install -m 0644 $(PKG_DIR)/config.yml.example \
	    $(BUILD_DIR)/$(RPM_STAGE)/etc/config.yml.example
	install -m 0644 $(PKG_DIR)/nsdev-registry.service \
	    $(BUILD_DIR)/$(RPM_STAGE)/lib/systemd/system/nsdev-registry.service
	install -m 0644 $(ROOT_DIR)/LICENSE   $(BUILD_DIR)/$(RPM_STAGE)/LICENSE
	install -m 0644 $(ROOT_DIR)/README.md $(BUILD_DIR)/$(RPM_STAGE)/README.md
	tar -czf $(BUILD_DIR)/$(RPM_STAGE).tar.gz -C $(BUILD_DIR) $(RPM_STAGE)
	cp $(BUILD_DIR)/$(RPM_STAGE).tar.gz $(RPM_TOPDIR)/SOURCES/
	cp $(PKG_DIR)/rpm/nsdev-registry.spec $(RPM_TOPDIR)/SPECS/
	rpmbuild \
	    --define "_topdir $(RPM_TOPDIR)" \
	    --define "_dbpath $(RPM_TOPDIR)/rpmdb" \
	    --define "pkg_version $(RPM_VERSION)" \
	    -bb $(RPM_TOPDIR)/SPECS/nsdev-registry.spec
	@echo
	@ls -la $(RPM_TOPDIR)/RPMS/x86_64/

# DEB build by hand: stage a Debian root tree with /usr/bin, /lib/systemd
# etc., drop in DEBIAN/{control,preinst,postinst,prerm,postrm,conffiles}
# (control template substituted with the version), then dpkg-deb --build.
# No debhelper, no debuild — minimum toolchain dependency.
deb: binary
	rm -rf $(DEB_TOPDIR)/nsdev-registry
	install -d $(DEB_TOPDIR)/nsdev-registry/DEBIAN \
	           $(DEB_TOPDIR)/nsdev-registry/usr/bin \
	           $(DEB_TOPDIR)/nsdev-registry/usr/libexec/nsdev-registry \
	           $(DEB_TOPDIR)/nsdev-registry/etc/nsdev-registry \
	           $(DEB_TOPDIR)/nsdev-registry/lib/systemd/system \
	           $(DEB_TOPDIR)/nsdev-registry/usr/share/doc/nsdev-registry
	install -m 0755 $(BUILD_DIR)/nsdev-registry \
	    $(DEB_TOPDIR)/nsdev-registry/usr/bin/nsdev-registry
	install -m 0755 $(PKG_DIR)/init-cert.sh \
	    $(DEB_TOPDIR)/nsdev-registry/usr/libexec/nsdev-registry/init-cert.sh
	install -m 0640 $(PKG_DIR)/config.yml.example \
	    $(DEB_TOPDIR)/nsdev-registry/etc/nsdev-registry/config.yml
	install -m 0644 $(PKG_DIR)/nsdev-registry.service \
	    $(DEB_TOPDIR)/nsdev-registry/lib/systemd/system/nsdev-registry.service
	install -m 0644 $(ROOT_DIR)/LICENSE \
	    $(DEB_TOPDIR)/nsdev-registry/usr/share/doc/nsdev-registry/copyright
	install -m 0644 $(ROOT_DIR)/README.md \
	    $(DEB_TOPDIR)/nsdev-registry/usr/share/doc/nsdev-registry/README.md
	sed 's/@PKG_VERSION@/$(PKG_VERSION)/g' \
	    $(PKG_DIR)/deb/nsdev-registry/control.in \
	    > $(DEB_TOPDIR)/nsdev-registry/DEBIAN/control
	for f in postinst prerm postrm; do \
	    install -m 0755 $(PKG_DIR)/deb/nsdev-registry/$$f \
	        $(DEB_TOPDIR)/nsdev-registry/DEBIAN/$$f; \
	done
	install -m 0644 $(PKG_DIR)/deb/nsdev-registry/conffiles \
	    $(DEB_TOPDIR)/nsdev-registry/DEBIAN/conffiles
	dpkg-deb --root-owner-group --build $(DEB_TOPDIR)/nsdev-registry \
	    $(DEB_TOPDIR)/nsdev-registry_$(PKG_VERSION)-1_$(ARCH).deb
	@echo
	@ls -la $(DEB_TOPDIR)/*.deb

packages: rpm deb
	@echo
	@echo 'Built packages:'
	@ls -la $(RPM_TOPDIR)/RPMS/*/*.rpm 2>/dev/null || true
	@ls -la $(DEB_TOPDIR)/*.deb         2>/dev/null || true

clean:
	rm -rf $(BUILD_DIR) $(RPM_TOPDIR) $(DEB_TOPDIR)
