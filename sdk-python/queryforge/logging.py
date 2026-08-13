"""Structured logging for the QueryForge Python SDK.

(The module is deliberately named ``logging`` so the public entry point reads
``queryforge.logging.configure(...)``, which is the first thing anyone guesses.
It does not shadow the standard library: Python 3 resolves ``import logging``
absolutely, so the ``import logging`` below is the stdlib one.)

QueryForge logs through the standard :mod:`logging` module under one namespace,
``queryforge``, and does nothing else to your logging setup. It attaches no
handler you did not ask for, never touches the root logger, and never calls
``basicConfig``. Out of the box the SDK is silent; you turn it on the way you
turn on any other library::

    import logging
    logging.getLogger("queryforge").setLevel(logging.INFO)

**Structured fields.** Every record carries a set of canonical fields — the same
names the Go engine and the Java SDK emit — as attributes on the ``LogRecord``,
so a JSON formatter can pick them up::

    record.library      # "queryforge"
    record.language     # "python"
    record.operation    # "translate"
    record.error_code   # "MODEL_TRANSPORT"

They are also collected under ``record.queryforge`` as one dict, which is easier
for a formatter that wants to nest them rather than flatten them.

**Levels.** The semantics match the other two languages exactly:

======  ======================================================================
DEBUG   Detail while tracing: binary resolution, a request being sent.
INFO    Lifecycle: an operation completed, or was refused by the model. A
        refusal is INFO on purpose — it means the guard rails worked.
WARNING A step failed but the operation continues.
ERROR   The caller is receiving an exception. Exactly one per failed call.
======  ======================================================================

**Privacy.** The natural-language question, the scope VALUES (tenant, user and
subscription ids) and the config contents are never logged, at any level. What
is logged is shape: the entity, the backend, the scope KEYS, a field count. See
:func:`redact` for the one place text of unknown provenance is scrubbed before
it reaches a record.
"""

from __future__ import annotations

import logging
import os
import re
import sys
import uuid
from typing import Any, Mapping

#: The logger namespace. One name for the whole SDK: a caller configures
#: ``queryforge`` and gets everything, or ``queryforge.transport`` and gets only
#: the subprocess layer.
LOGGER_NAME = "queryforge"

#: Environment variable that turns SDK logging on without a code change. It is
#: the same spelling the engine binary honours, so one variable lights up both
#: halves of a call. Absent or empty means "leave logging exactly as the host
#: configured it", which is the default and does nothing at all.
LOG_LEVEL_ENV_VAR = "QUERYFORGE_LOG_LEVEL"

#: Values :data:`LOG_LEVEL_ENV_VAR` and the SDK's own helpers accept.
LEVEL_NAMES = ("off", "error", "warn", "warning", "info", "debug")

_LEVELS: dict[str, int] = {
    "off": logging.CRITICAL + 10,  # above every real level: discards everything
    "none": logging.CRITICAL + 10,
    "silent": logging.CRITICAL + 10,
    "error": logging.ERROR,
    "warn": logging.WARNING,
    "warning": logging.WARNING,
    "info": logging.INFO,
    "debug": logging.DEBUG,
}

#: The root logger for the SDK. A ``NullHandler`` is attached at import, which is
#: the documented way for a library to avoid the "No handlers could be found"
#: warning without imposing any output of its own.
logger = logging.getLogger(LOGGER_NAME)
logger.addHandler(logging.NullHandler())


# ---------------------------------------------------------------------------
# Canonical field names
#
# These are a cross-language contract, not an implementation detail: the Go
# engine and the Java SDK emit the same names, so one saved search in a log
# aggregator works regardless of which SDK produced the line. Renaming one
# breaks somebody's dashboard.
# ---------------------------------------------------------------------------

FIELD_LIBRARY = "library"
FIELD_LANGUAGE = "language"
FIELD_VERSION = "version"
FIELD_COMPONENT = "component"
FIELD_OPERATION = "operation"
FIELD_OUTCOME = "outcome"
FIELD_BACKEND = "backend"
FIELD_ENTITY = "entity"
FIELD_REQUEST_ID = "request_id"
FIELD_DURATION_MS = "duration_ms"
FIELD_ERROR_CODE = "error_code"
FIELD_ERROR_TYPE = "error_type"
FIELD_SCOPE_KEYS = "scope_keys"
FIELD_CONFIG_FIELDS = "config_fields"

LIBRARY_NAME = "queryforge"
LANGUAGE_NAME = "python"

#: Attribute names ``logging.LogRecord`` already uses. A field colliding with one
#: of these makes ``Logger.makeRecord`` raise ``KeyError``, which would turn a
#: logging call — the thing that is supposed to help you debug — into the crash.
#: Colliding names are prefixed rather than dropped, so the value still reaches
#: the record and the collision is visible instead of silent.
_RESERVED = frozenset(
    {
        "args", "asctime", "created", "exc_info", "exc_text", "filename",
        "funcName", "levelname", "levelno", "lineno", "message", "module",
        "msecs", "msg", "name", "pathname", "process", "processName",
        "relativeCreated", "stack_info", "taskName", "thread", "threadName",
    }
)


def new_request_id() -> str:
    """Return a short correlation id for one SDK call.

    Twelve hex characters rather than a full UUID: this id exists to tie a
    handful of log lines together within one process's logs, not to be globally
    unique forever, and a shorter string is markedly easier to eyeball in a
    terminal. Collisions across unrelated requests are irrelevant because the
    id is never used as a key for anything.

    A caller who already has a trace id should pass their own; see
    :meth:`queryforge.QueryForge.with_request_id`.
    """
    return uuid.uuid4().hex[:12]


def parse_level(name: str) -> int:
    """Map a level name onto a :mod:`logging` level number.

    Raises :class:`ValueError` for anything unrecognised. That is deliberate and
    matches the engine: both plausible defaults are harmful. Falling back to
    "off" hides diagnostics from someone who explicitly asked for them and will
    conclude the feature is broken; falling back to "debug" starts writing
    detail that a typo did not authorise.
    """
    try:
        return _LEVELS[name.strip().lower()]
    except KeyError:
        raise ValueError(
            f"unknown log level {name!r} (expected one of: {', '.join(LEVEL_NAMES)})"
        ) from None


def level_name_for_engine(level: int) -> str:
    """Render a :mod:`logging` level as the name the engine binary understands.

    The engine takes ``off | error | warn | info | debug``. Python levels are
    numbers and callers set arbitrary ones, so this rounds DOWN to the nearest
    engine level — a caller asking for level 15 (between DEBUG and INFO) gets
    ``debug``, because the alternative is silently dropping records they
    configured a handler to receive.
    """
    if level <= logging.DEBUG:
        return "debug"
    if level <= logging.INFO:
        return "info"
    if level <= logging.WARNING:
        return "warn"
    if level <= logging.ERROR:
        return "error"
    return "off"


def is_configured() -> bool:
    """Report whether the host has deliberately configured QueryForge logging.

    "Deliberately" means one of two things, both of which name the
    ``queryforge`` logger explicitly:

    * a level was set on it — ``logging.getLogger("queryforge").setLevel(...)``,
      which is what :func:`configure` and the environment variable also do; or
    * a real handler was attached to it. The ``NullHandler`` this module
      installs at import does not count, or the answer would always be yes.

    Inheriting a level from the root logger deliberately does NOT count. A
    ``basicConfig(level=DEBUG)`` in someone's application is a statement about
    their own code, not a request for a subprocess to start producing
    diagnostics — and acting on it would mean spawning every engine in debug
    mode for anyone who ever turned debug on anywhere.
    """
    if logger.level != logging.NOTSET:
        return True
    return any(not isinstance(h, logging.NullHandler) for h in logger.handlers)


def engine_level() -> str | None:
    """Return the level to ask the engine for, or ``None`` to leave it alone.

    ``None`` — the default — means the SDK sends no ``logLevel`` in its request,
    which keeps the request BYTE-IDENTICAL to what a pre-1.1 engine expects.
    That matters more than it looks: the engine decodes requests with unknown
    fields rejected, so an SDK that always sent the field would turn every
    request into ``INVALID_REQUEST`` for anyone pointing ``QUERYFORGE_BINARY``
    at an older build. The field is sent only once the host has asked for
    logging, and that is also the moment they would accept "your engine is too
    old for this" as an answer.
    """
    if not is_configured():
        return None
    name = level_name_for_engine(logger.getEffectiveLevel())
    return None if name == "off" else name


# ---------------------------------------------------------------------------
# Redaction
# ---------------------------------------------------------------------------

#: Patterns for secrets that can turn up inside text the SDK did not author —
#: chiefly the engine's stderr, which is quoted verbatim into a ProtocolError
#: message when the subprocess dies before answering.
#:
#: This is a second line of defence, not the first. The first is that nothing
#: here ever puts a key into a message deliberately; QueryForge reads API keys
#: from the environment and passes them in an HTTP header. But an engine
#: crashing mid-request can print anything, and "anything" heading into a log
#: aggregator is worth scrubbing.
_SECRET_PATTERNS: tuple[tuple[re.Pattern[str], str], ...] = (
    # Authorization: Bearer <token>, and bare "Bearer <token>".
    (re.compile(r"(?i)\b(bearer)\s+[A-Za-z0-9._\-+/=]{8,}"), r"\1 [REDACTED]"),
    # key=value forms: api_key=..., apiKey: "...", password=..., token=...
    (
        re.compile(
            r"(?i)\b([a-z_\-]*(?:api[_\-]?key|secret|password|passwd|token|credential)[a-z_\-]*)"
            r"(\s*[:=]\s*)(\"[^\"]*\"|'[^']*'|[^\s,;&)}\]]+)"
        ),
        r"\1\2[REDACTED]",
    ),
    # Credentials embedded in a URL: scheme://user:pass@host
    (re.compile(r"(?i)\b([a-z][a-z0-9+.\-]*://)([^\s/:@]+):([^\s/@]+)@"), r"\1\2:[REDACTED]@"),
    # Vendor key shapes that appear with no surrounding key name at all.
    (re.compile(r"\bsk-[A-Za-z0-9_\-]{16,}"), "[REDACTED]"),
    (re.compile(r"\bAIza[A-Za-z0-9_\-]{20,}"), "[REDACTED]"),
)

#: Bound on any redacted excerpt. An engine that crashes can emit megabytes, and
#: an exception message is going somewhere it will be stored.
MAX_EXCERPT = 500


def redact(text: str, limit: int = MAX_EXCERPT) -> str:
    """Scrub credentials from text of unknown provenance and bound its length.

    Truncation is announced rather than silent: a reader has to be able to tell
    "the engine said this" from "the engine said this and 900 KB more", or they
    will go hunting for a bug in text that was never the whole story.
    """
    if not text:
        return text
    for pattern, replacement in _SECRET_PATTERNS:
        text = pattern.sub(replacement, text)
    if limit >= 0 and len(text) > limit:
        dropped = len(text) - limit
        text = f"{text[:limit]}… ({dropped} bytes truncated)"
    return text


def scope_keys(scope: Mapping[str, Any] | None) -> list[str]:
    """Return a scope's field NAMES, sorted.

    Names only, never values. Scope values are tenant, subscription, user and
    enterprise ids — the most sensitive thing this SDK handles — while the names
    are exactly what an audit trail needs in order to say which filters were
    forced onto a query.
    """
    return sorted(scope) if scope else []


# ---------------------------------------------------------------------------
# Emission
# ---------------------------------------------------------------------------


def get_logger(component: str | None = None) -> logging.Logger:
    """Return the SDK logger, optionally for one component.

    ``get_logger("transport")`` returns ``queryforge.transport``, which a caller
    can silence or raise independently of the rest.
    """
    return logger if component is None else logging.getLogger(f"{LOGGER_NAME}.{component}")


def log(
    log_: logging.Logger,
    level: int,
    message: str,
    /,
    *,
    exc_info: bool = False,
    **fields: Any,
) -> None:
    """Emit one structured record.

    The first three parameters are positional-only. That is not style: without
    it, a field named ``message`` — a plausible thing to want to log — collides
    with the parameter name and raises ``TypeError: got multiple values``, and
    a logging call that crashes the program it was added to debug is the worst
    possible failure mode for this function.

    ``message`` is a fixed string with nothing interpolated into it — that is
    what lets an aggregator count occurrences of "request failed" without the
    count fragmenting across every distinct value that ever appeared in the
    text. Everything variable goes in ``fields``.

    Fields with a ``None`` value are dropped rather than logged as null, so a
    record's key set says which facts were actually known.
    """
    if not log_.isEnabledFor(level):
        # Checked explicitly so the dict below is not built for a record that
        # will be discarded. The default configuration discards everything.
        return

    payload: dict[str, Any] = {
        FIELD_LIBRARY: LIBRARY_NAME,
        FIELD_LANGUAGE: LANGUAGE_NAME,
    }
    payload.update({k: v for k, v in fields.items() if v is not None})

    extra: dict[str, Any] = {"queryforge": payload}
    for key, value in payload.items():
        # A colliding name is prefixed, not dropped: LogRecord would raise
        # KeyError on the collision, and losing the value silently is worse than
        # an oddly-named attribute.
        extra["qf_" + key if key in _RESERVED else key] = value

    log_.log(level, message, exc_info=exc_info, extra=extra)


# ---------------------------------------------------------------------------
# Optional convenience for hosts
# ---------------------------------------------------------------------------


class JsonFormatter(logging.Formatter):
    """A formatter rendering one JSON object per record.

    Offered because a structured logger whose fields you have to write a
    formatter to see is not much use, and because the JSON it emits matches the
    engine's field-for-field. It is entirely optional: attach it yourself, or
    ignore it and use your own.
    """

    def format(self, record: logging.LogRecord) -> str:
        import json
        import time

        payload: dict[str, Any] = {
            "time": time.strftime("%Y-%m-%dT%H:%M:%S", time.gmtime(record.created))
            + f".{int(record.msecs):03d}Z",
            "level": record.levelname,
            "msg": record.getMessage(),
            "logger": record.name,
        }
        payload.update(getattr(record, "queryforge", {}))
        if record.exc_info:
            payload["exception"] = self.formatException(record.exc_info)
        return json.dumps(payload, default=str)


def configure(level: str | int = "info", stream: Any = None) -> logging.Handler:
    """Attach a JSON handler to the QueryForge logger. **Opt-in only.**

    A library must not configure logging on its own initiative, and this
    function never runs unless you call it. When you do, it touches nothing but
    the ``queryforge`` logger: the root logger, other libraries' loggers and any
    handler you already installed are all left exactly as they were.

    Propagation to the root logger is switched off, so calling this alongside
    an existing ``basicConfig`` gives you one JSON line per event rather than a
    JSON line and a plain-text duplicate.

    Returns the handler, so you can remove it again::

        h = queryforge.logging.configure("debug")
        ...
        logging.getLogger("queryforge").removeHandler(h)
    """
    numeric = parse_level(level) if isinstance(level, str) else level
    handler = logging.StreamHandler(stream if stream is not None else sys.stderr)
    handler.setFormatter(JsonFormatter())
    logger.addHandler(handler)
    logger.setLevel(numeric)
    logger.propagate = False
    return handler


def _configure_from_environment() -> None:
    """Honour :data:`LOG_LEVEL_ENV_VAR` at import, if it is set.

    This is the one place the SDK configures anything without an explicit call,
    and it is gated on an environment variable that only exists because someone
    set it deliberately. The case it serves is real and not otherwise reachable:
    an operator debugging a running deployment cannot edit the application's
    code to add a ``configure`` call, and QueryForge is usually several layers
    down inside somebody else's service.

    An unrecognised value is reported on stderr and then IGNORED rather than
    raised. Failing here would mean a typo in an environment variable prevents
    the application from importing at all, which is a far worse outcome than
    logging staying off — and unlike the engine's own fail-fast on the same
    value, there is no caller here to hand an error to.
    """
    raw = os.environ.get(LOG_LEVEL_ENV_VAR, "").strip()
    if not raw:
        return
    try:
        numeric = parse_level(raw)
    except ValueError as exc:
        print(f"queryforge: ignoring {LOG_LEVEL_ENV_VAR}: {exc}", file=sys.stderr)
        return
    if numeric >= _LEVELS["off"]:
        return
    configure(numeric)


_configure_from_environment()
