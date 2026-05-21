"""nsdev-registry — Python wrapper around the Go daemon.

The actual server lives in cmd/registry (Go). This package exists so
operators can `pipx install nsdev-registry` and get a working CLI on
PATH without learning apt / dnf / rpm. Two entry points are
exported:

  nsdev-registry        — thin execvp around the Go binary; passes
                          every argv straight through, so `serve`,
                          `garbage-collect`, `session`, and any
                          future subcommand work identically to a
                          dpkg/rpm install.

  nsdev-registry-init   — one-shot system bootstrapper that drops
                          the bundled systemd unit + default config
                          + init-cert.sh into their canonical
                          locations and (optionally) enables the
                          service. Needed because pipx itself runs
                          in a per-user venv that cannot touch
                          /etc, /usr/lib/systemd, or /var/lib.

Use `nsdev-registry-init --help` after `pipx install` for the
bootstrap step (typically `sudo $(pipx environment --value
PIPX_BIN_DIR)/nsdev-registry-init`).
"""

__version__ = "0.1.0"
