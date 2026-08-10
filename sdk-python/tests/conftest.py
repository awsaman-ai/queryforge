"""Shared fixtures.

The suite is split deliberately between two kinds of test:

* Against the **real engine binary**, exercising the offline ops (generate,
  validate, version). These prove the SDK and the engine actually agree — the
  single most valuable thing to test, and the thing a mock cannot tell you.

* Against a **scripted fake binary**, exercising the paths the real engine
  cannot be made to take on demand: every error code, a model call without an
  API key, a crash, corrupted output, a protocol mismatch. These are the SDK's
  own logic, and scripting them is the only way to cover them offline.
"""

from __future__ import annotations

import json
import os
import shutil
import subprocess
import sys
from pathlib import Path

import pytest

REPO_ROOT = Path(__file__).resolve().parents[2]
BUNDLED_BINARY = Path(__file__).resolve().parents[1] / "queryforge" / "bin" / "queryforge"


@pytest.fixture(scope="session")
def engine_binary() -> Path:
    """Build (or reuse) the real engine binary for the integration tests."""
    if BUNDLED_BINARY.is_file():
        return BUNDLED_BINARY

    if shutil.which("go") is None:
        pytest.skip("no bundled binary and no Go toolchain to build one")
    BUNDLED_BINARY.parent.mkdir(parents=True, exist_ok=True)
    subprocess.run(
        ["go", "build", "-o", str(BUNDLED_BINARY), "./cmd/queryforge"],
        cwd=REPO_ROOT,
        check=True,
    )
    return BUNDLED_BINARY


@pytest.fixture
def real_binary(engine_binary: Path, monkeypatch: pytest.MonkeyPatch) -> Path:
    """Point the SDK at the real binary explicitly.

    Setting the override rather than relying on discovery keeps these tests
    working when the suite is run from a source checkout where the package has
    not been installed.
    """
    monkeypatch.setenv("QUERYFORGE_BINARY", str(engine_binary))
    return engine_binary


@pytest.fixture
def config() -> dict:
    """A small config exercising the shapes the protocol must carry.

    Inline rather than read from examples/, so that editing a shipped example
    cannot silently change what these tests assert.
    """
    return {
        "entity": "Order",
        "model": {"provider": "stub", "baseURL": "http://localhost", "model": "test"},
        "backends": {
            "sql": {"table": "orders"},
            "mysql": {"table": "orders"},
            "mongo": {"collection": "orders"},
        },
        "fields": [
            {
                "name": "status",
                "type": "enum",
                "values": ["NEW", "DELIVERED", "CANCELLED"],
                "indexed": True,
            },
            {"name": "amount", "type": "number", "mapping": {"sql": "total_amount", "mysql": "total_amount"}},
            {
                "name": "customerName",
                "type": "string",
                "mapping": {"sql": "customer_name", "mysql": "customer_name"},
            },
            {"name": "internalNote", "type": "string", "returnable": False},
        ],
    }


@pytest.fixture
def valid_ast() -> dict:
    """A legal AST for the `config` fixture."""
    return {
        "version": "1.0",
        "entity": "Order",
        "filter": {
            "type": "comparison",
            "field": "status",
            "operator": "equals",
            "value": {"kind": "enum", "v": "DELIVERED"},
        },
    }


class FakeBinary:
    """A scripted stand-in for the engine executable.

    It writes a Python script that echoes a canned response, records each
    invocation's request to a log file, and can be told to crash or to emit
    garbage. That covers the failure modes the real engine cannot be asked to
    produce without a network and an API key.
    """

    def __init__(self, path: Path, log: Path) -> None:
        self.path = path
        self.log = log

    def invocations(self) -> list[dict]:
        """Return the requests this binary received, in order."""
        if not self.log.is_file():
            return []
        return [json.loads(line) for line in self.log.read_text().splitlines() if line.strip()]

    @property
    def call_count(self) -> int:
        return len(self.invocations())


@pytest.fixture
def fake_binary(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    """Return a factory that installs a scripted fake engine for one test."""

    def make(
        response: dict | str | None = None,
        *,
        exit_code: int = 0,
        stderr: str = "",
        sleep_seconds: float = 0.0,
    ) -> FakeBinary:
        log = tmp_path / "invocations.jsonl"
        script = tmp_path / "fake_queryforge.py"

        # The response is embedded as a literal so the fake needs no arguments
        # and no environment beyond a Python interpreter.
        if response is None:
            body = ""
        elif isinstance(response, str):
            body = response  # raw text, for the "not JSON" cases
        else:
            body = json.dumps(response)

        script.write_text(
            "import json, sys, time\n"
            f"time.sleep({sleep_seconds!r})\n"
            "raw = sys.stdin.read()\n"
            f"open({str(log)!r}, 'a').write(raw.strip() + '\\n')\n"
            f"sys.stderr.write({stderr!r})\n"
            f"sys.stdout.write({body!r})\n"
            f"sys.exit({exit_code})\n"
        )

        # A launcher shim, because the SDK executes the path directly rather
        # than through an interpreter. On Windows a .py file is not executable,
        # so a .cmd wrapper stands in.
        if sys.platform.startswith("win"):
            launcher = tmp_path / "fake_queryforge.cmd"
            launcher.write_text(f'@echo off\r\n"{sys.executable}" "{script}" %*\r\n')
        else:
            launcher = tmp_path / "fake_queryforge"
            launcher.write_text(f'#!/bin/sh\nexec "{sys.executable}" "{script}" "$@"\n')
            launcher.chmod(0o755)

        monkeypatch.setenv("QUERYFORGE_BINARY", str(launcher))
        return FakeBinary(launcher, log)

    return make


def _ok_response(**fields) -> dict:
    """Build a successful response envelope with sensible defaults."""
    base = {"success": True, "protocol": "1.0", "op": "translate", "backend": "sql"}
    base.update(fields)
    return base


def _err_response(code: str, message: str = "boom", **fields) -> dict:
    """Build a failed response envelope."""
    base = {"success": False, "protocol": "1.0", "code": code, "message": message}
    base.update(fields)
    return base


# Exposed as fixtures rather than imported directly: the tests directory is not
# a package, so a `from .conftest import ...` would not resolve.
@pytest.fixture
def ok_response():
    return _ok_response


@pytest.fixture
def err_response():
    return _err_response


@pytest.fixture(autouse=True)
def _clear_binary_cache():
    """Reset the module-level binary cache between tests.

    The cache is keyed on nothing — it is a single module global — so a test that
    resolved the real binary would otherwise leak that path into a test that
    expects the fake one. The SDK already bypasses the cache whenever the
    override variable is set, but clearing it makes the isolation unconditional.
    """
    from queryforge import _binary

    _binary._cached = None
    yield
    _binary._cached = None


@pytest.fixture(autouse=True)
def _isolate_env(monkeypatch: pytest.MonkeyPatch):
    """Ensure a developer's own QUERYFORGE_BINARY does not leak into the suite."""
    monkeypatch.delenv("QUERYFORGE_BINARY", raising=False)
    yield


def pytest_configure(config):
    """Make the SDK importable from a source checkout without installing it."""
    sys.path.insert(0, str(Path(__file__).resolve().parents[1]))
    os.environ.setdefault("PYTHONDONTWRITEBYTECODE", "1")
