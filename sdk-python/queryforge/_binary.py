"""Locating the QueryForge executable.

The user installs ``queryforge`` from PyPI and calls a Python API; they never
learn there is a Go binary underneath. This module is what keeps that true: it
detects the platform, finds the executable that was packaged for it, and raises
a message that names the problem when it cannot.

Wheels are built per platform, so a correctly installed package contains exactly
one binary — the one for the machine it was installed on. The platform tag is
still computed and checked, because the failure it catches (a universal wheel
built by hand, a source install, a package copied between machines) otherwise
shows up as ``Exec format error`` from the OS, which tells the user nothing.
"""

from __future__ import annotations

import os
import platform
import stat
import sys
from pathlib import Path

from .errors import BinaryNotFoundError

#: Environment variable that overrides the search entirely. It exists for three
#: real cases: developing against a locally built binary, running in an
#: environment where the packaged one cannot execute (a hardened container with
#: a noexec site-packages mount), and pinning a specific engine version while
#: keeping the SDK where it is.
BINARY_ENV_VAR = "QUERYFORGE_BINARY"

#: Directory inside the installed package holding the bundled executable.
_BIN_DIR = "bin"


def platform_tag() -> str:
    """Return the ``<os>-<arch>`` tag for the running interpreter.

    The tags match the release artifact names, so the string in an error message
    is the same string the user will look for on the releases page.
    """
    system = sys.platform
    if system.startswith("linux"):
        os_name = "linux"
    elif system == "darwin":
        os_name = "darwin"
    elif system in ("win32", "cygwin"):
        os_name = "windows"
    else:
        raise BinaryNotFoundError(
            f"QueryForge has no build for this operating system ({system!r}). "
            f"Supported: Linux, macOS, Windows. Set {BINARY_ENV_VAR} to point at "
            f"a binary you built yourself.",
            code="BINARY_NOT_FOUND",
        )

    # platform.machine() reports the kernel's idea of the architecture, which is
    # what matters for choosing an executable. The aliases are all real: Linux
    # says x86_64/aarch64, macOS says x86_64/arm64, Windows says AMD64/ARM64.
    machine = platform.machine().lower()
    if machine in ("x86_64", "amd64", "x64"):
        arch = "amd64"
    elif machine in ("arm64", "aarch64"):
        arch = "arm64"
    else:
        raise BinaryNotFoundError(
            f"QueryForge has no build for this CPU architecture ({machine!r}). "
            f"Supported: amd64 (x86_64) and arm64 (aarch64). Set {BINARY_ENV_VAR} "
            f"to point at a binary you built yourself.",
            code="BINARY_NOT_FOUND",
        )

    return f"{os_name}-{arch}"


def _executable_name() -> str:
    return "queryforge.exe" if sys.platform in ("win32", "cygwin") else "queryforge"


def _candidates() -> list[Path]:
    """Return the paths to search, in priority order.

    Both the bare name and the platform-tagged name are searched because the two
    packaging modes produce different layouts: a per-platform wheel ships one
    ``bin/queryforge``, while a developer checkout or a hand-built universal
    package may keep several under ``bin/<os>-<arch>/``.
    """
    package_dir = Path(__file__).resolve().parent
    name = _executable_name()
    return [
        package_dir / _BIN_DIR / name,
        package_dir / _BIN_DIR / platform_tag() / name,
    ]


def _is_executable(path: Path) -> bool:
    """Report whether path is a file this process can execute.

    On Windows the executable bit does not exist, so existence is the only
    meaningful check; elsewhere a non-executable file is a real and common
    failure (a wheel unpacked by a tool that dropped the mode bits), and
    distinguishing it from "missing" is what makes the error message actionable.
    """
    if not path.is_file():
        return False
    if sys.platform in ("win32", "cygwin"):
        return True
    return os.access(path, os.X_OK)


def find_binary() -> Path:
    """Locate the QueryForge executable, or raise :class:`BinaryNotFoundError`.

    The result is not cached here — :func:`resolve_binary` does that — so this
    function stays a pure lookup that tests can call repeatedly.
    """
    override = os.environ.get(BINARY_ENV_VAR)
    if override:
        # An explicit override that does not work is always reported, never
        # silently fallen back from. Falling back would run a *different* engine
        # than the one the user named, which is precisely the situation the
        # variable exists to prevent.
        path = Path(override).expanduser()
        if not path.is_file():
            raise BinaryNotFoundError(
                f"{BINARY_ENV_VAR} points at {str(path)!r}, which is not a file.",
                code="BINARY_NOT_FOUND",
            )
        if not _is_executable(path):
            raise BinaryNotFoundError(
                f"{BINARY_ENV_VAR} points at {str(path)!r}, which is not executable. "
                f"Try: chmod +x {path}",
                code="BINARY_NOT_FOUND",
            )
        return path

    found_but_not_executable: list[Path] = []
    for candidate in _candidates():
        if candidate.is_file():
            if _is_executable(candidate):
                return candidate
            found_but_not_executable.append(candidate)

    # Separate the two failures. "Present but not executable" has a one-line fix
    # and is common after a package is copied or unzipped by hand; "absent"
    # means the wrong wheel is installed and needs a reinstall.
    if found_but_not_executable:
        paths = ", ".join(str(p) for p in found_but_not_executable)
        raise BinaryNotFoundError(
            f"The bundled QueryForge executable is present but not executable: {paths}. "
            f"Try: chmod +x {found_but_not_executable[0]}",
            code="BINARY_NOT_FOUND",
        )

    searched = ", ".join(str(p) for p in _candidates())
    raise BinaryNotFoundError(
        f"No QueryForge executable was found for this platform ({platform_tag()}). "
        f"Searched: {searched}. This usually means the installed wheel was built "
        f"for a different platform — reinstall with 'pip install --force-reinstall "
        f"queryforge' — or set {BINARY_ENV_VAR} to a binary you built yourself.",
        code="BINARY_NOT_FOUND",
    )


_cached: Path | None = None


def resolve_binary() -> Path:
    """Return the executable path, resolving it at most once per process.

    Caching matters because the default mode spawns one process per request: the
    filesystem probing would otherwise be repeated on every single query. The
    cache is skipped whenever the override variable is set, so a test or a
    notebook can repoint it between calls without a stale hit.
    """
    global _cached
    if os.environ.get(BINARY_ENV_VAR):
        return find_binary()
    if _cached is None:
        _cached = find_binary()
    return _cached


def ensure_executable(path: Path) -> None:
    """Add the owner-execute bit to a bundled binary that is missing it.

    Wheels do preserve mode bits, but the file can still arrive without them —
    unpacked by a tool that ignores them, or restored from an archive built on
    Windows. This is called at build/install time rather than on the query path,
    because silently chmod-ing a file at query time would hide a broken install.
    """
    if sys.platform in ("win32", "cygwin"):
        return
    mode = path.stat().st_mode
    path.chmod(mode | stat.S_IXUSR | stat.S_IXGRP | stat.S_IXOTH)
