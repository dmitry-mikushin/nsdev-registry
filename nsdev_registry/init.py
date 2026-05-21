"""nsdev-registry-init — one-shot system bootstrap.

Drops the bundled systemd unit, default config, init-cert.sh, and
the binary into their canonical filesystem locations so the daemon
can be managed with `systemctl`. pipx installs the wrapper in a
per-user venv that cannot itself write to /usr, /etc, /var, or
/lib/systemd, so this helper is the bridge.

Typical use after `pipx install nsdev-registry`:

    sudo $(pipx environment --value PIPX_BIN_DIR)/nsdev-registry-init

Idempotent: running it twice is harmless. Add `--no-enable` to skip
the `systemctl enable --now` step (useful when bootstrapping a
container image where systemd isn't up yet).
"""
import argparse
import grp
import os
import pwd
import shutil
import subprocess
import sys
from pathlib import Path


PREFIX_BIN     = Path("/usr/bin")
PREFIX_LIBEXEC = Path("/usr/libexec/nsdev-registry")
PREFIX_ETC     = Path("/etc/nsdev-registry")
PREFIX_UNIT    = Path("/lib/systemd/system")  # alias on most distros
USER_NAME      = "nsdev-registry"
GROUP_NAME     = "nsdev-registry"


def _data_dir():
    return Path(__file__).parent / "data"


def _need_root():
    if os.geteuid() != 0:
        sys.stderr.write(
            "nsdev-registry-init must run as root (writes to /usr, /etc, "
            "/lib/systemd). Re-run with `sudo`.\n"
        )
        sys.exit(1)


def _ensure_user():
    """Create the unprivileged system user the daemon runs as."""
    try:
        grp.getgrnam(GROUP_NAME)
    except KeyError:
        subprocess.run(["groupadd", "--system", GROUP_NAME], check=True)
    try:
        pwd.getpwnam(USER_NAME)
    except KeyError:
        subprocess.run([
            "useradd", "--system",
            "--gid", GROUP_NAME,
            "--home-dir", "/var/lib/nsdev-registry",
            "--shell", "/sbin/nologin",
            "--comment", "nsdev-registry daemon",
            USER_NAME,
        ], check=True)


def _install_file(src: Path, dst: Path, mode: int):
    dst.parent.mkdir(parents=True, exist_ok=True)
    # noreplace-style: keep an existing /etc/nsdev-registry/config.yml
    # if the operator has edited it.
    if dst.exists() and dst.parent == PREFIX_ETC:
        print(f"[init] keeping existing {dst} (operator-managed)")
        return
    shutil.copy2(src, dst)
    dst.chmod(mode)
    print(f"[init] installed {dst}")


def main():
    parser = argparse.ArgumentParser(
        description="Bootstrap the nsdev-registry systemd service on the host."
    )
    parser.add_argument(
        "--no-enable", action="store_true",
        help="Skip `systemctl enable --now nsdev-registry.service` "
             "(useful in container image builds where systemd is not up).",
    )
    args = parser.parse_args()

    _need_root()
    _ensure_user()

    data = _data_dir()
    _install_file(data / "bin" / "nsdev-registry",
                  PREFIX_BIN / "nsdev-registry", 0o755)
    _install_file(data / "system" / "init-cert.sh",
                  PREFIX_LIBEXEC / "init-cert.sh", 0o755)
    _install_file(data / "system" / "config.yml",
                  PREFIX_ETC / "config.yml", 0o640)
    _install_file(data / "system" / "nsdev-registry.service",
                  PREFIX_UNIT / "nsdev-registry.service", 0o644)

    # Make the config readable by the daemon user. The .deb / .rpm
    # postinst hooks the same chown; we mirror it so the pipx path
    # ends in the same state.
    try:
        uid = pwd.getpwnam(USER_NAME).pw_uid
        gid = grp.getgrnam(GROUP_NAME).gr_gid
        os.chown(PREFIX_ETC / "config.yml", 0, gid)
    except Exception as e:  # pragma: no cover — best-effort
        print(f"[init] warning: chown config.yml failed: {e}", file=sys.stderr)

    if args.no_enable:
        print("[init] skipping systemctl enable (--no-enable)")
        return

    if not Path("/run/systemd/system").is_dir():
        print("[init] systemd not running; skipping enable step "
              "(re-run after boot to start the service)")
        return

    subprocess.run(["systemctl", "daemon-reload"], check=True)
    subprocess.run(
        ["systemctl", "enable", "--now", "nsdev-registry.service"],
        check=True,
    )
    print("[init] nsdev-registry.service is enabled and running")
    print("[init] check status:  systemctl status nsdev-registry")
