"""Running the QueryForge executable and decoding its reply.

This is the whole of the SDK's communication layer: spawn the binary, write one
JSON object to its stdin, read one JSON object from its stdout. There is no HTTP
client, no socket, no daemon, and no retry loop — the engine is a local
subprocess with the caller's own privileges.
"""

from __future__ import annotations

import json
import subprocess
from pathlib import Path
from typing import Any

from ._binary import resolve_binary
from .errors import ProtocolError, error_from_response

#: Wire protocol this SDK was built against. Only the MAJOR component is
#: enforced: the binary is free to add ops and optional response fields (a MINOR
#: bump) because this SDK ignores what it does not recognise, but a MAJOR bump
#: means an existing field changed meaning, and continuing would produce quietly
#: wrong output rather than an error.
PROTOCOL_VERSION = "1.0"

#: Grace period, in seconds, added to the request's own timeout before the
#: subprocess is killed. The engine enforces the real deadline internally and
#: reports it as a structured TIMEOUT error, which is far more useful than a
#: killed process — so this outer bound exists only to catch a binary that has
#: hung badly enough not to honour its own deadline.
_KILL_GRACE_SECONDS = 15.0


def _major(version: str) -> str:
    return version.split(".", 1)[0]


def run_request(request: dict[str, Any], timeout_seconds: float | None = None) -> dict[str, Any]:
    """Send one request to the executable and return the decoded response.

    Raises :class:`~queryforge.errors.QueryForgeError` (or the appropriate
    subclass) when the engine reports a failure, and
    :class:`~queryforge.errors.ProtocolError` when the executable itself
    misbehaves.
    """
    binary = resolve_binary()
    payload = json.dumps(request, separators=(",", ":"), default=_json_default)

    kill_after = None if timeout_seconds is None else timeout_seconds + _KILL_GRACE_SECONDS

    try:
        completed = subprocess.run(
            [str(binary)],
            input=payload,
            capture_output=True,
            text=True,
            timeout=kill_after,
            # No shell, ever: the config and the question are user data, and a
            # shell would make them executable. The argument list form passes
            # them as a single argv entry with no interpretation.
            shell=False,
        )
    except subprocess.TimeoutExpired as exc:
        raise ProtocolError(
            f"The QueryForge executable did not respond within {kill_after:.0f}s and was killed. "
            f"This is a bug in the engine — its own deadline should have produced a TIMEOUT error first.",
            code="PROTOCOL_ERROR",
        ) from exc
    except OSError as exc:
        raise ProtocolError(
            f"Could not run the QueryForge executable at {binary}: {exc}",
            code="PROTOCOL_ERROR",
        ) from exc

    return _decode(completed, binary)


def _decode(completed: subprocess.CompletedProcess[str], binary: Path) -> dict[str, Any]:
    """Turn a finished process into a response dict, or raise.

    Note what is *not* consulted: the exit code. The protocol's contract is that
    stdout carries the answer, and a failed request legitimately exits non-zero
    with a perfectly good error object on stdout. Branching on the exit code
    would throw that structured error away and replace it with "process exited
    1", which is exactly the regression this SDK exists to prevent.
    """
    raw = completed.stdout.strip()
    if not raw:
        # Empty stdout means the process died before it could answer — a crash, a
        # signal, a missing shared library. stderr is the only evidence, so carry
        # it through rather than reporting a bare exit code.
        stderr = (completed.stderr or "").strip()
        detail = f" It wrote to stderr: {stderr}" if stderr else ""
        raise ProtocolError(
            f"The QueryForge executable at {binary} exited with code "
            f"{completed.returncode} and produced no response.{detail}",
            code="PROTOCOL_ERROR",
        )

    try:
        response = json.loads(raw)
    except json.JSONDecodeError as exc:
        # Truncate before quoting: a corrupted stream can be arbitrarily long,
        # and an exception message is going into someone's log.
        excerpt = raw[:500] + ("…" if len(raw) > 500 else "")
        raise ProtocolError(
            f"The QueryForge executable produced output that is not JSON: {excerpt!r}",
            code="PROTOCOL_ERROR",
        ) from exc

    if not isinstance(response, dict):
        raise ProtocolError(
            f"Expected a JSON object from the QueryForge executable, got {type(response).__name__}.",
            code="PROTOCOL_ERROR",
        )

    _check_protocol(response, binary)

    if not response.get("success", False):
        raise error_from_response(response)
    return response


def _check_protocol(response: dict[str, Any], binary: Path) -> None:
    """Refuse a binary speaking an incompatible protocol major version."""
    reported = response.get("protocol", "")
    if not reported:
        raise ProtocolError(
            f"The executable at {binary} returned a response with no protocol version. "
            f"It is probably too old for this SDK, which speaks protocol {PROTOCOL_VERSION}.",
            code="PROTOCOL_ERROR",
        )
    if _major(reported) != _major(PROTOCOL_VERSION):
        raise ProtocolError(
            f"Protocol mismatch: the executable at {binary} speaks {reported}, "
            f"this SDK speaks {PROTOCOL_VERSION}. Install a matching queryforge release, "
            f"or unset QUERYFORGE_BINARY to use the bundled executable.",
            code="PROTOCOL_ERROR",
        )


def _json_default(obj: Any) -> Any:
    """Serialize the handful of non-JSON types that turn up in a scope map.

    Scope values come from the calling application's session — a tenant id, a
    subscription — and arrive as whatever type that application uses. Dates and
    Decimals are the two that reliably appear and that ``json`` refuses. Anything
    else raises the standard TypeError, naming the type, which is a better answer
    than a silent ``str()`` of an object nobody meant to send.
    """
    import datetime
    import decimal
    import uuid

    if isinstance(obj, (datetime.datetime, datetime.date)):
        return obj.isoformat()
    if isinstance(obj, decimal.Decimal):
        return float(obj)
    if isinstance(obj, uuid.UUID):
        return str(obj)
    raise TypeError(
        f"Object of type {type(obj).__name__} is not JSON serializable; "
        f"scope values must be strings, numbers, booleans, or lists of those."
    )
