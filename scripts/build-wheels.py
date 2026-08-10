#!/usr/bin/env python3
"""Build one Python wheel per platform, each carrying that platform's engine binary.

Why per-platform rather than one universal wheel: bundling all five targets would put ~75 MB into
every install so that four binaries could sit unused on disk, and would push the artifact close to
PyPI's per-file limit for no benefit. Each wheel here is ~6 MB and contains exactly the one
executable the machine installing it can run.

The wheels are *pure Python plus a data file* — there is no C extension and nothing to compile —
so they are built once and then retagged, rather than being built five times under different
compilers. That is why the Python tag stays ``py3-none``: any CPython 3.9+ can run the code, and
only the bundled executable is platform-specific.

Usage::

    scripts/build-binaries.sh 1.0.0 dist
    python3 scripts/build-wheels.py dist wheelhouse

Requires ``build`` and ``wheel`` (``pip install build wheel``).
"""

from __future__ import annotations

import argparse
import shutil
import subprocess
import sys
from pathlib import Path

REPO_ROOT = Path(__file__).resolve().parent.parent
SDK_DIR = REPO_ROOT / "sdk-python"
PACKAGE_BIN = SDK_DIR / "queryforge" / "bin"

#: Maps a build platform tag to the wheel platform tags it should carry.
#:
#: Linux gets both manylinux and musllinux because the engine is built with CGO_ENABLED=0 and is
#: therefore statically linked with no libc dependency at all — it genuinely runs on Alpine as
#: well as glibc distributions, and tagging only manylinux would make pip refuse to install it on
#: an Alpine image for no reason.
#:
#: The macOS minimums are the oldest releases each architecture exists on: Apple Silicon starts at
#: 11.0, and Go's amd64 target supports back to 10.13, so 10.9 is a safe floor that pip will match
#: against any newer system.
WHEEL_TAGS = {
    "linux-amd64": ["manylinux2014_x86_64", "musllinux_1_1_x86_64"],
    "linux-arm64": ["manylinux2014_aarch64", "musllinux_1_1_aarch64"],
    "darwin-amd64": ["macosx_10_9_x86_64"],
    "darwin-arm64": ["macosx_11_0_arm64"],
    "windows-amd64": ["win_amd64"],
}


def run(command: list[str], cwd: Path | None = None) -> None:
    print(f"  $ {' '.join(command)}")
    subprocess.run(command, cwd=cwd, check=True)


def build_wheel_for(tag: str, binary_dir: Path, wheelhouse: Path) -> Path:
    """Build and retag a wheel carrying one platform's binary."""
    executable = "queryforge.exe" if tag.startswith("windows") else "queryforge"
    source = binary_dir / tag / executable
    if not source.is_file():
        raise SystemExit(f"missing binary for {tag}: {source}")

    # Stage exactly one binary into the package. The directory is wiped first so a wheel can never
    # accidentally inherit the previous platform's executable — which would produce an artifact
    # that installs cleanly and then fails with "Exec format error" on first use.
    if PACKAGE_BIN.exists():
        shutil.rmtree(PACKAGE_BIN)
    PACKAGE_BIN.mkdir(parents=True)
    shutil.copy2(source, PACKAGE_BIN / executable)
    (PACKAGE_BIN / executable).chmod(0o755)

    staging = SDK_DIR / "build-wheel"
    if staging.exists():
        shutil.rmtree(staging)

    print(f"\n[{tag}] building")
    run([sys.executable, "-m", "build", "--wheel", "--outdir", str(staging)], cwd=SDK_DIR)

    built = list(staging.glob("*.whl"))
    if len(built) != 1:
        raise SystemExit(f"expected exactly one wheel for {tag}, got {built}")

    # Retag rather than rebuild. `python -m build` produces py3-none-any because the package has
    # no compiled extension; the payload is what makes it platform-specific, so the tag has to be
    # corrected afterwards. --remove drops the original file so the wrong tag cannot be uploaded.
    #
    # The platform tags are joined with "." into ONE --platform-tag argument, which is how a wheel
    # carries a compressed tag set. Passing the flag repeatedly does not accumulate — the last one
    # silently wins, which previously produced Linux wheels tagged musllinux only, installable on
    # Alpine and refused by pip on every glibc distribution.
    print(f"[{tag}] retagging")
    run([
        sys.executable, "-m", "wheel", "tags", "--remove",
        "--platform-tag", ".".join(WHEEL_TAGS[tag]),
        str(built[0]),
    ])

    retagged = list(staging.glob("*.whl"))
    if len(retagged) != 1:
        raise SystemExit(f"expected exactly one retagged wheel for {tag}, got {retagged}")

    wheelhouse.mkdir(parents=True, exist_ok=True)
    final = wheelhouse / retagged[0].name
    shutil.move(str(retagged[0]), final)
    shutil.rmtree(staging)
    print(f"[{tag}] -> {final.name} ({final.stat().st_size // 1024} KiB)")
    return final


def main() -> int:
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument(
        "binary_dir",
        type=Path,
        nargs="?",
        default=REPO_ROOT / "dist",
        help="directory produced by scripts/build-binaries.sh",
    )
    parser.add_argument(
        "wheelhouse",
        type=Path,
        nargs="?",
        default=REPO_ROOT / "wheelhouse",
        help="where to write the finished wheels",
    )
    args = parser.parse_args()

    binary_dir = args.binary_dir.resolve()
    wheelhouse = args.wheelhouse.resolve()

    if not binary_dir.is_dir():
        raise SystemExit(f"no such directory: {binary_dir}. Run scripts/build-binaries.sh first.")

    built = [build_wheel_for(tag, binary_dir, wheelhouse) for tag in WHEEL_TAGS]

    # Leave the package directory clean. A stale binary left behind would be picked up by a
    # developer's editable install and silently shadow whatever they build next.
    if PACKAGE_BIN.exists():
        shutil.rmtree(PACKAGE_BIN)

    print(f"\nBuilt {len(built)} wheels in {wheelhouse}:")
    for wheel in sorted(built):
        print(f"  {wheel.name}")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
