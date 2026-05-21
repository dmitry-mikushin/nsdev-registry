"""Custom build: compile the Go binary, then drop the binary + the
packaging assets (systemd unit, default config, init-cert.sh) into
nsdev_registry/data/ so they end up inside the wheel.

The same flow handles three install modes:

  - ``pip wheel .``          — Python builds nothing; this script
                                shells out to `go build`, copies the
                                produced binary, and the wheel that
                                pip produces carries everything.
  - ``pip install -e .``     — editable install for developers; the
                                Python wrapper checks the in-tree
                                build/ output FIRST so a fresh
                                `make -C packaging binary` is picked
                                up immediately.
  - ``pipx install .``       — operator-facing one-shot. pipx invokes
                                this script identically to `pip wheel`
                                then installs into a venv on PATH.

The wheel is intentionally fat (~12 MB — same as the .deb / .rpm
because all three carry the same Go binary). For most consumers this
is the friendliest distribution: one wheel, one command, no apt /
dnf, no systemd-rpm-macros gymnastics.
"""

import shutil
import subprocess
from pathlib import Path

from setuptools import setup
from setuptools.command.build_py import build_py

ROOT = Path(__file__).resolve().parent
PKG = ROOT / "nsdev_registry"
DATA = PKG / "data"
PACKAGING = ROOT / "packaging"


class BuildPy(build_py):
    def run(self):
        self.compile_go_binary()
        self.stage_packaging_assets()
        super().run()

    def compile_go_binary(self):
        bin_dir = DATA / "bin"
        bin_dir.mkdir(parents=True, exist_ok=True)
        out = bin_dir / "nsdev-registry"
        if out.exists():
            out.unlink()
        # mod=vendor uses the committed vendor/ directory so the
        # build works offline; CGO disabled so the binary is fully
        # static and portable across glibc versions.
        cmd = [
            "go",
            "build",
            "-mod=vendor",
            "-ldflags=-s -w",
            "-o",
            str(out),
            "./cmd/registry",
        ]
        env = {
            **dict(__import__("os").environ),
            "GO111MODULE": "on",
            "CGO_ENABLED": "0",
        }
        print("[setup] compiling Go binary:", " ".join(cmd))
        subprocess.run(cmd, cwd=ROOT, env=env, check=True)
        out.chmod(0o755)
        print(f"[setup] -> {out} ({out.stat().st_size:,} bytes)")

    def stage_packaging_assets(self):
        # The systemd unit, default config, and init-cert.sh are
        # owned by packaging/ (the same pieces .deb / .rpm ship).
        # Copy them into data/ so `nsdev-registry-init` can lay them
        # down on disk at the right system paths.
        assets_dir = DATA / "system"
        if assets_dir.exists():
            shutil.rmtree(assets_dir)
        assets_dir.mkdir(parents=True)
        mapping = {
            "nsdev-registry.service": "nsdev-registry.service",
            "config.yml.example":     "config.yml",
            "init-cert.sh":           "init-cert.sh",
        }
        for src_name, dst_name in mapping.items():
            src = PACKAGING / src_name
            if not src.exists():
                raise FileNotFoundError(f"packaging asset missing: {src}")
            shutil.copy2(src, assets_dir / dst_name)
            print(f"[setup] staged {src.name} -> data/system/{dst_name}")
        (assets_dir / "init-cert.sh").chmod(0o755)


setup(cmdclass={"build_py": BuildPy})
