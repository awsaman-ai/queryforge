"""Running the QueryForge executable and decoding its reply.

This is the whole of the SDK's communication layer: spawn the binary, write one
JSON object to its stdin, read one JSON object from its stdout. There is no HTTP
client, no socket, no daemon, and no retry loop — the engine is a local
subprocess with the caller's own privileges.

It is also the SDK's error boundary. Every failure below is detected, logged
once at ERROR with the context needed to diagnose it, and raised — never
swallowed, never converted into an empty result. See the module docstring of
:mod:`queryforge.errors` for the exception hierarchy that carries them.
"""

from __future__ import annotations

import json
import logging
import os
import re
import subprocess
import time
from pathlib import Path
from typing import Any, Mapping

from ._binary import resolve_binary
from .errors import ProtocolError, error_from_response
from .logging import (
    FIELD_BACKEND,
    FIELD_DURATION_MS,
    FIELD_ENTITY,
    FIELD_ERROR_CODE,
    FIELD_ERROR_TYPE,
    FIELD_OPERATION,
    FIELD_OUTCOME,
    FIELD_REQUEST_ID,
    FIELD_SCOPE_KEYS,
    engine_level,
    get_logger,
    log,
    new_request_id,
    redact,
    scope_keys,
)

#: Wire protocol this SDK was built against. Only the MAJOR component is
#: enforced: the binary is free to add ops and optional response fields (a MINOR
#: bump) because this SDK ignores what it does not recognise, but a MAJOR bump
#: means an existing field changed meaning, and continuing would produce quietly
#: wrong output rather than an error.
PROTOCOL_VERSION = "1.1"

#: Grace period, in seconds, added to the request's own timeout before the
#: subprocess is killed. The engine enforces the real deadline internally and
#: reports it as a structured TIMEOUT error, which is far more useful than a
#: killed process — so this outer bound exists only to catch a binary that has
#: hung badly enough not to honour its own deadline.
_KILL_GRACE_SECONDS = 15.0

_log = get_logger("transport")

#: A POSIX environment variable name: letters, digits, underscores, not leading
#: with a digit. Matches the rule the engine applies to ``model.apiKeyEnv``, so
#: a name the SDK accepts is a name the config can reference.
_ENV_NAME = re.compile(r"^[A-Za-z_][A-Za-z0-9_]*$")


def build_env(credentials: Mapping[str, str] | None) -> dict[str, str] | None:
    """Merge caller-supplied credentials into a copy of the process environment.

    Returns ``None`` when there are none, which lets the caller omit ``env``
    entirely and inherit — the pre-existing behaviour, byte for byte.

    Why the environment and not the request body: the engine reads keys from the
    variable NAMED by ``model.apiKeyEnv``, and the request body is the one
    structure in this SDK that can end up somewhere else. It is JSON-encoded, it
    is what gets dumped when the protocol breaks, and it is the natural thing to
    attach to a bug report. The environment is passed to exactly one process and
    is never serialised by this SDK.

    Why a copy and not a replacement: handing the child a bare credentials dict
    would strip ``PATH``, ``HOME`` and ``TMPDIR``, breaking the engine in ways
    that look nothing like a credentials problem.
    """
    if not credentials:
        return None

    env = dict(os.environ)
    for name, value in credentials.items():
        if not isinstance(name, str) or not _ENV_NAME.match(name):
            # The name is safe to echo — it is a variable name, not a secret.
            raise ValueError(
                f"credentials key {name!r} is not a valid environment variable name "
                "(letters, digits and underscores, not starting with a digit). "
                "The KEY is the name your config's model.apiKeyEnv refers to, "
                'e.g. {"QF_API_KEY": "<your-key>"}.'
            )
        if not isinstance(value, str):
            # Deliberately does NOT include the value: it is the secret.
            raise ValueError(
                f"credentials[{name!r}] must be a string, got {type(value).__name__}"
            )
        env[name] = value
    return env


def _major(version: str) -> str:
    return version.split(".", 1)[0]


def run_request(
    request: dict[str, Any],
    timeout_seconds: float | None = None,
    request_id: str | None = None,
    credentials: Mapping[str, str] | None = None,
) -> dict[str, Any]:
    """Send one request to the executable and return the decoded response.

    Raises :class:`~queryforge.errors.QueryForgeError` (or the appropriate
    subclass) when the engine reports a failure, and
    :class:`~queryforge.errors.ProtocolError` when the executable itself
    misbehaves. It never returns a partial or empty result for a failed call.

    ``request_id`` correlates this call's SDK log lines with the engine's own.
    One is generated when the caller does not supply theirs.
    """
    rid = request_id or new_request_id()
    op = str(request.get("op", ""))
    context = _context(request, rid, op)

    # Ask the engine for logs at the level this SDK is itself configured for,
    # and only then. The field is omitted entirely when logging is off, which
    # keeps the request byte-identical to a protocol-1.0 one — an engine built
    # before this field existed rejects unknown fields outright, so an SDK that
    # always sent it would break anyone pointing QUERYFORGE_BINARY at an older
    # build.
    level = engine_level()
    if level is not None:
        options = dict(request.get("options") or {})
        options.setdefault("logLevel", level)
        options.setdefault("requestId", rid)
        request = {**request, "options": options}

    binary = resolve_binary()
    payload = json.dumps(request, separators=(",", ":"), default=_json_default)

    kill_after = None if timeout_seconds is None else timeout_seconds + _KILL_GRACE_SECONDS
    log(_log, logging.DEBUG, "sending request to the engine", **context)

    started = time.monotonic()
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
            # None inherits this process's environment, which is exactly what
            # happened before credentials existed.
            env=build_env(credentials),
        )
    except subprocess.TimeoutExpired as exc:
        message = (
            f"The QueryForge executable did not respond within {kill_after:.0f}s and was killed. "
            f"This is a bug in the engine — its own deadline should have produced a TIMEOUT error first."
        )
        raise _fail(ProtocolError(message, code="PROTOCOL_ERROR"), context, started) from exc
    except OSError as exc:
        message = f"Could not run the QueryForge executable at {binary}: {exc}"
        raise _fail(ProtocolError(message, code="PROTOCOL_ERROR"), context, started) from exc

    try:
        response = _decode(completed, binary)
    except Exception as exc:  # noqa: BLE001 - re-raised immediately; see _fail
        # Deliberately broad, and deliberately NOT a swallow: every path through
        # here re-raises. The catch exists so that the one ERROR line for this
        # call is written at the boundary regardless of which of the several
        # failure kinds _decode found, rather than being duplicated inside each
        # of them. `raise` preserves the original traceback and the __cause__
        # chain untouched.
        _fail(exc, context, started)
        raise

    _succeeded(context, started, response)
    return response


def _context(request: Mapping[str, Any], rid: str, op: str) -> dict[str, Any]:
    """Build the log fields for one call.

    Note what goes in and what does not. The op, the backend and the SCOPE KEYS
    go in. The question text, the scope VALUES and the config do not — they are
    respectively user data, tenant identifiers, and a customer's physical schema.
    """
    config = request.get("config")
    return {
        FIELD_OPERATION: op or None,
        FIELD_REQUEST_ID: rid,
        FIELD_BACKEND: request.get("backend") or None,
        # The entity name is the config's SHAPE, not its content — enough to tell
        # two configs apart in a log without reproducing a customer's schema.
        FIELD_ENTITY: config.get("entity") if isinstance(config, Mapping) else None,
        FIELD_SCOPE_KEYS: scope_keys(request.get("scope")) or None,
    }


def _fail(exc: BaseException, context: Mapping[str, Any], started: float) -> BaseException:
    """Log the single ERROR line for a failed call and return the exception.

    Returning it rather than raising lets a call site write
    ``raise _fail(...) from exc``, which keeps the chaining visible at the point
    it happens instead of hiding a raise inside a helper.

    ``exc_info`` is on: the traceback belongs in the log exactly once, and this
    boundary is the once.
    """
    log(
        _log,
        logging.ERROR,
        "engine request failed",
        exc_info=True,
        **{
            **context,
            FIELD_OUTCOME: "error",
            FIELD_DURATION_MS: _elapsed_ms(started),
            FIELD_ERROR_TYPE: type(exc).__name__,
            FIELD_ERROR_CODE: getattr(exc, "code", None) or None,
        },
    )
    return exc


def _succeeded(context: Mapping[str, Any], started: float, response: Mapping[str, Any]) -> None:
    """Log the single INFO line for a successful call."""
    log(
        _log,
        logging.INFO,
        "engine request completed",
        **{
            **context,
            FIELD_OUTCOME: "ok",
            FIELD_DURATION_MS: _elapsed_ms(started),
            # Repair count is worth surfacing: a translation that succeeded on
            # the third attempt cost three model calls, and that is almost
            # always a config gap the operator can close.
            "repair_attempts": response.get("repairAttempts") or None,
            "provider": response.get("providerUsed") or None,
        },
    )


def _elapsed_ms(started: float) -> int:
    return int((time.monotonic() - started) * 1000)


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
        #
        # It is redacted first. stderr is the engine's, not ours: a crash can
        # print anything, including a provider error quoting an Authorization
        # header, and this message is on its way into an exception that will be
        # logged.
        stderr = redact((completed.stderr or "").strip())
        detail = f" It wrote to stderr: {stderr}" if stderr else ""
        raise ProtocolError(
            f"The QueryForge executable at {binary} exited with code "
            f"{completed.returncode} and produced no response.{detail}",
            code="PROTOCOL_ERROR",
        )

    try:
        response = json.loads(raw)
    except json.JSONDecodeError as exc:
        # Truncate and scrub before quoting: a corrupted stream can be
        # arbitrarily long and can contain anything, and an exception message is
        # going into someone's log.
        raise ProtocolError(
            f"The QueryForge executable produced output that is not JSON: {redact(raw)!r}",
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
