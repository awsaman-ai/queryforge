"""Exception hierarchy for the QueryForge Python SDK.

Every failure the engine reports arrives as a JSON object carrying a stable
``code``. This module maps each code onto a distinct exception class, so callers
can write ``except UnsupportedRequestError`` instead of comparing strings.

The hierarchy is deliberately shallow and has one root, :class:`QueryForgeError`.
A caller who does not care why a translation failed catches the root; one who
does — retry the model, fix the config, tell the user to rephrase — catches a
leaf. Every leaf keeps ``.code``, so branching on the code remains available and
a code this SDK version has never heard of still surfaces as the root class
rather than disappearing.
"""

from __future__ import annotations

from typing import Any, Sequence


class Detail:
    """One structured finding inside a failed response.

    Attributes mirror the wire format: ``code`` is the engine's own error code
    (``unknown_field``, ``value_out_of_domain``, …), ``path`` locates the node in
    the AST, ``field`` names the offending field when there is one, and
    ``suggestions`` holds nearest-match field names for a misspelling.
    """

    __slots__ = ("code", "path", "field", "message", "suggestions")

    def __init__(
        self,
        code: str = "",
        path: str = "",
        field: str = "",
        message: str = "",
        suggestions: Sequence[str] = (),
    ) -> None:
        self.code = code
        self.path = path
        self.field = field
        self.message = message
        self.suggestions = list(suggestions)

    @classmethod
    def from_json(cls, obj: dict[str, Any]) -> "Detail":
        return cls(
            code=obj.get("code", ""),
            path=obj.get("path", ""),
            field=obj.get("field", ""),
            message=obj.get("message", ""),
            suggestions=obj.get("suggestions") or (),
        )

    def __repr__(self) -> str:  # pragma: no cover - debugging aid
        return f"Detail(code={self.code!r}, field={self.field!r}, path={self.path!r})"

    def __str__(self) -> str:
        return self.message or self.code


class QueryForgeError(Exception):
    """Base class for every error this SDK raises.

    ``code`` is the protocol-level classification and is always set for a failure
    the engine reported. ``details`` is populated for validation failures and is
    empty otherwise.
    """

    def __init__(
        self,
        message: str,
        code: str = "",
        details: Sequence[Detail] = (),
    ) -> None:
        super().__init__(message)
        self.message = message
        self.code = code
        self.details = list(details)

    def __str__(self) -> str:
        # The code is prefixed because these messages are most often read in a
        # traceback, where knowing the class alone does not tell you which of
        # several codes mapped to it.
        return f"[{self.code}] {self.message}" if self.code else self.message


class InvalidRequestError(QueryForgeError):
    """The request was malformed, or a required argument was missing.

    Always a bug in the calling code — retrying will not help.
    """


class InvalidConfigError(QueryForgeError):
    """The config did not parse, or broke one of the engine's structural rules."""


class UnknownBackendError(QueryForgeError):
    """No generator is registered for the requested backend.

    The message lists the ones that are.
    """


class InvalidScopeError(QueryForgeError):
    """A caller-supplied scope filter was rejected.

    Scope comes from the application (tenant, subscription, user), never from the
    end user's question, so this is an application bug rather than something to
    show a user.
    """


class ValidationError(QueryForgeError):
    """An AST broke a rule the config declares.

    On :meth:`~queryforge.QueryForge.generate` that is the AST you supplied. On a
    translation it means the model could not produce a conforming AST within the
    repair budget — usually because the config does not register a field the
    question needs.

    ``details`` lists every finding; each has a ``field`` and ``suggestions``
    where applicable.
    """


class UnsupportedRequestError(QueryForgeError):
    """The model declined: the question cannot be expressed in this config.

    This is a well-formed answer rather than a failure of the pipeline. The
    message is written to be shown to the person who asked.
    """


class ModelOutputError(QueryForgeError):
    """The model answered, but never with usable JSON.

    Usually transient. Retrying is reasonable; so is switching models.
    """


class ModelTransportError(QueryForgeError):
    """The model was never reached: network failure, missing or rejected API key,
    or a rate limit. Check the key named by ``apiKeyEnv`` is exported."""


class GenerateError(QueryForgeError):
    """A valid AST could not be compiled to the requested backend."""


class TimeoutError(QueryForgeError):  # noqa: A001 - deliberately shadows the builtin
    """The operation exceeded its deadline.

    Named to match the protocol's ``TIMEOUT`` code. It shadows the builtin inside
    this module only; callers importing it get ``queryforge.TimeoutError``, and
    the builtin remains reachable as ``builtins.TimeoutError``.
    """


class BinaryNotFoundError(QueryForgeError):
    """No QueryForge executable could be found for this platform.

    Raised before any request is sent. The message names the platform that was
    detected and how to override the search with ``QUERYFORGE_BINARY``.
    """


class ProtocolError(QueryForgeError):
    """The executable produced something this SDK cannot interpret.

    Reaching this means the binary crashed, wrote non-JSON to stdout, or speaks a
    protocol version this SDK was not built against. It is the one error class
    that indicates a broken installation rather than a bad input.
    """


#: Maps a protocol error code onto its exception class. Codes absent from this
#: table fall back to :class:`QueryForgeError`, which is what keeps an SDK
#: talking to a newer binary from crashing on an error class it predates.
_CODE_TO_ERROR: dict[str, type[QueryForgeError]] = {
    "INVALID_REQUEST": InvalidRequestError,
    "UNKNOWN_OP": InvalidRequestError,
    "INVALID_CONFIG": InvalidConfigError,
    "UNKNOWN_BACKEND": UnknownBackendError,
    "INVALID_SCOPE": InvalidScopeError,
    "VALIDATION_FAILED": ValidationError,
    "UNSUPPORTED_REQUEST": UnsupportedRequestError,
    "MODEL_OUTPUT": ModelOutputError,
    "MODEL_TRANSPORT": ModelTransportError,
    "GENERATE_FAILED": GenerateError,
    "TIMEOUT": TimeoutError,
    "INTERNAL": QueryForgeError,
}


def error_from_response(payload: dict[str, Any]) -> QueryForgeError:
    """Build the right exception from a failed response object."""
    code = payload.get("code", "")
    message = payload.get("message") or "the request failed with no message"
    details = [Detail.from_json(d) for d in payload.get("details") or ()]
    return _CODE_TO_ERROR.get(code, QueryForgeError)(message, code=code, details=details)
