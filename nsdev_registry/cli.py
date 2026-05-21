"""nsdev-registry entry point — execvp's the bundled Go daemon."""
import os
import sys
from pathlib import Path


def data_dir():
    return Path(__file__).parent / "data"


def _resolve_binary():
    """Locate the nsdev-registry Go binary.

    Two locations are tried, in order, so that both wheel installs
    (`pipx install nsdev-registry`) and developer editable installs
    (`pip install -e .` from a source tree that has been built once
    via `make -C packaging binary`) work without code changes:

      1. data/bin/nsdev-registry — bundled in the wheel.
      2. packaging/build/nsdev-registry — left behind by the
         packaging Makefile during local development.
    """
    candidates = [
        data_dir() / "bin" / "nsdev-registry",
        Path(__file__).resolve().parents[1]
        / "packaging" / "build" / "nsdev-registry",
    ]
    for c in candidates:
        if c.exists():
            return c
    sys.stderr.write(
        "nsdev-registry binary not found. Build it with\n"
        "    cd $(dirname $(python3 -c 'import nsdev_registry, os;"
        " print(os.path.dirname(nsdev_registry.__file__))')) "
        "&& make -C packaging binary\n"
        "or reinstall the wheel via `pipx reinstall nsdev-registry`.\n"
    )
    sys.exit(1)


def main():
    binary = _resolve_binary()
    os.execvp(str(binary), [str(binary)] + sys.argv[1:])
