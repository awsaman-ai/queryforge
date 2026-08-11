"""QueryForge — natural language to database queries, as a native Python library.

    from queryforge import QueryForge

    qf = QueryForge.mysql("orders.config.json")
    sql = qf.query("delivered orders over $100 last month").to_sql()

There is no server to run and no Go toolchain to install. The engine ships as a
platform binary inside this package; the SDK spawns it, hands it one JSON
request, and turns the reply into Python objects. Everything that decides what a
query means — parsing, the AST, validation, dialects, SQL and Mongo generation —
lives in that engine, so this SDK and the Java, Node and .NET ones all produce
byte-identical output for the same input.

The two halves of the API:

``query(text)``
    The full pipeline. Costs one model call, so it needs a ``model`` block in the
    config and the API key exported under the name that block's ``apiKeyEnv``
    gives.

``generate(ast)`` / ``validate(ast)``
    Deterministic. No model call, no network, no API key. Use these to re-compile
    a stored AST for a second backend, and in tests.
"""

from __future__ import annotations

import json
from pathlib import Path
from typing import Any, Mapping, Sequence, Union

from ._binary import BINARY_ENV_VAR, find_binary, platform_tag
from ._result import Result, ScopeFilter
from ._transport import PROTOCOL_VERSION, run_request
from .errors import (
    BinaryNotFoundError,
    Detail,
    GenerateError,
    InvalidConfigError,
    InvalidRequestError,
    InvalidScopeError,
    ModelOutputError,
    ModelTransportError,
    ProtocolError,
    QueryForgeError,
    TimeoutError,
    UnknownBackendError,
    UnsupportedRequestError,
    ValidationError,
)

__version__ = "1.1.2"

__all__ = [
    "QueryForge",
    "PendingQuery",
    "Result",
    "ScopeFilter",
    "Detail",
    "QueryForgeError",
    "InvalidRequestError",
    "InvalidConfigError",
    "UnknownBackendError",
    "InvalidScopeError",
    "ValidationError",
    "UnsupportedRequestError",
    "ModelOutputError",
    "ModelTransportError",
    "GenerateError",
    "TimeoutError",
    "BinaryNotFoundError",
    "ProtocolError",
    "engine_version",
    "binary_path",
    "platform_tag",
    "PROTOCOL_VERSION",
    "BINARY_ENV_VAR",
    "__version__",
]

#: Config = a parsed dict, a path to a JSON file, or the JSON text itself.
#:
#: Spelled with ``Union`` rather than ``X | Y``. These two aliases are assigned at
#: import time, not annotations, so ``from __future__ import annotations`` does not
#: defer them — PEP 604 syntax here raises ``TypeError`` on 3.9, which is the floor
#: ``pyproject.toml`` promises.
Config = Union[Mapping[str, Any], str, Path]

#: Scope values the engine accepts: a literal, or a list of literals for an
#: ``in``-style filter.
ScopeValue = Union[str, int, float, bool, Sequence[Any]]


def _load_config(config: Config) -> dict[str, Any]:
    """Normalize the several things a caller may reasonably pass as a config.

    A ``dict`` is used as-is. A ``Path`` is always read as a file. A ``str`` is
    read as a file unless it opens with ``{``, in which case it is JSON text —
    unambiguous in practice, because no filesystem path begins with a brace.
    """
    if isinstance(config, Mapping):
        return dict(config)

    if isinstance(config, Path):
        return _read_config_file(config)

    if isinstance(config, str):
        if config.lstrip().startswith("{"):
            try:
                parsed = json.loads(config)
            except json.JSONDecodeError as exc:
                raise InvalidConfigError(
                    f"The config string is not valid JSON: {exc}", code="INVALID_CONFIG"
                ) from exc
            if not isinstance(parsed, dict):
                raise InvalidConfigError(
                    f"A config must be a JSON object, got {type(parsed).__name__}.",
                    code="INVALID_CONFIG",
                )
            return parsed
        return _read_config_file(Path(config))

    raise InvalidConfigError(
        f"A config must be a dict, a path, or a JSON string — got {type(config).__name__}.",
        code="INVALID_CONFIG",
    )


def _read_config_file(path: Path) -> dict[str, Any]:
    try:
        text = path.expanduser().read_text(encoding="utf-8")
    except OSError as exc:
        raise InvalidConfigError(
            f"Could not read the config file {str(path)!r}: {exc}", code="INVALID_CONFIG"
        ) from exc
    try:
        parsed = json.loads(text)
    except json.JSONDecodeError as exc:
        raise InvalidConfigError(
            f"The config file {str(path)!r} is not valid JSON: {exc}", code="INVALID_CONFIG"
        ) from exc
    if not isinstance(parsed, dict):
        raise InvalidConfigError(
            f"The config file {str(path)!r} must hold a JSON object, "
            f"got {type(parsed).__name__}.",
            code="INVALID_CONFIG",
        )
    return parsed


class PendingQuery:
    """A question that has been described but not yet compiled.

    Nothing runs until a terminal method (:meth:`to_sql`, :meth:`to_mongo`,
    :meth:`result`, …) is called, and the answer is cached afterwards — so
    reading ``.to_sql()`` and then ``.explain()`` off the same object costs one
    model call, not two.

    Every builder method returns a *new* ``PendingQuery`` rather than mutating
    this one. That is what makes a partially-configured query safe to keep as a
    template::

        base = qf.query("open orders").scope({"tenantId": tenant})
        sql = base.to_sql()
        prose = base.explain()      # same cached answer, no second model call
    """

    __slots__ = ("_forge", "_text", "_scope", "_options", "_result")

    def __init__(
        self,
        forge: "QueryForge",
        text: str,
        scope: Mapping[str, ScopeValue] | None,
        options: dict[str, Any],
    ) -> None:
        self._forge = forge
        self._text = text
        self._scope = dict(scope) if scope else None
        self._options = dict(options)
        self._result: Result | None = None

    # --- builders -----------------------------------------------------------

    def _derive(self, **changes: Any) -> "PendingQuery":
        """Return a copy with some fields replaced, and no cached result."""
        return PendingQuery(
            forge=changes.get("forge", self._forge),
            text=changes.get("text", self._text),
            scope=changes.get("scope", self._scope),
            options=changes.get("options", self._options),
        )

    def scope(self, scope: Mapping[str, ScopeValue]) -> "PendingQuery":
        """Add filters the application imposes regardless of what was asked.

        Subscription, tenant, user, enterprise ids — values that come from the
        session, not from the question. They are AND-ed onto the query after
        validation, so they can only narrow the result, and the model is never
        told they exist.

        Calling this more than once merges, so a base query's scope survives::

            base = qf.query("open orders").scope({"tenantId": t})
            mine = base.scope({"ownerId": u})   # both filters apply
        """
        merged = dict(self._scope or {})
        merged.update(scope)
        return self._derive(scope=merged)

    def timeout(self, seconds: float) -> "PendingQuery":
        """Bound the whole operation, model call included."""
        if seconds <= 0:
            raise InvalidRequestError(
                f"timeout must be positive, got {seconds}", code="INVALID_REQUEST"
            )
        options = dict(self._options)
        options["timeoutMs"] = int(seconds * 1000)
        return self._derive(options=options)

    def max_repairs(self, n: int) -> "PendingQuery":
        """Bound the validation-repair retries. 0 means one attempt, no repairs."""
        if n < 0:
            raise InvalidRequestError(
                f"max_repairs cannot be negative, got {n}", code="INVALID_REQUEST"
            )
        options = dict(self._options)
        options["maxRepairs"] = n
        return self._derive(options=options)

    def include_raw(self, enabled: bool = True) -> "PendingQuery":
        """Return the model's verbatim reply on :attr:`Result.raw`.

        Off by default: the reply is derived from the user's question, and
        results tend to end up in logs.
        """
        options = dict(self._options)
        options["includeRaw"] = enabled
        return self._derive(options=options)

    def scope_in_ast(self, enabled: bool = True) -> "PendingQuery":
        """Report the effective AST — scope predicates included — as the result's AST.

        Off by default, in which case the AST is exactly what the model produced
        and can be fed back to :meth:`QueryForge.generate` unchanged. Turn it on
        for an audit log that must store one object proving what ran.
        """
        options = dict(self._options)
        options["scopeInAst"] = enabled
        return self._derive(options=options)

    # --- terminals ----------------------------------------------------------

    def result(self) -> Result:
        """Compile the query and return everything the engine reported."""
        if self._result is None:
            self._result = self._forge._translate(self._text, self._scope, self._options)
        return self._result

    def to_sql(self) -> str:
        """Return the parameterized SQL statement.

        The values are **not** inlined — that is the injection guarantee this
        library rests on. Pass :meth:`to_args` alongside it to your driver::

            cursor.execute(pending.to_sql(), pending.to_args())
        """
        return self.result().require_sql()

    def to_args(self) -> tuple[Any, ...]:
        """Return the bound placeholder values, in the order the SQL expects."""
        return self.result().args

    def to_mongo(self) -> dict[str, Any]:
        """Return the compiled query document for a document backend."""
        return self.result().require_doc()

    def to_ast(self) -> dict[str, Any]:
        """Return the validated intermediate representation."""
        ast = self.result().ast
        return ast if ast is not None else {}

    def explain(self) -> str:
        """Return a deterministic prose rendering of what the query does."""
        return self.result().explain

    def warnings(self) -> tuple[str, ...]:
        """Return non-fatal advisories, e.g. a filter on a non-indexed field."""
        return self.result().warnings

    def __repr__(self) -> str:  # pragma: no cover - debugging aid
        state = "compiled" if self._result is not None else "pending"
        return f"PendingQuery({self._text!r}, backend={self._forge.backend!r}, {state})"


class QueryForge:
    """An engine bound to one config and one backend.

    Construct it with the factory for the backend you want::

        QueryForge.mysql(config)      # MySQL dialect
        QueryForge.postgres(config)   # PostgreSQL dialect
        QueryForge.mongo(config)      # MongoDB query documents

    Instances are immutable and cheap; the executable is not started until a
    query is actually compiled, so building one costs nothing.
    """

    __slots__ = ("_config", "backend", "_scope", "_options")

    #: Backend ids the shipped engine registers. Checked locally so a typo costs
    #: an immediate error rather than a process spawn.
    BACKENDS = ("sql", "mysql", "mongo")

    def __init__(
        self,
        config: Config,
        backend: str,
        *,
        scope: Mapping[str, ScopeValue] | None = None,
        timeout: float | None = None,
        max_repairs: int | None = None,
    ) -> None:
        if backend not in self.BACKENDS:
            raise UnknownBackendError(
                f"Unknown backend {backend!r}. Registered: {', '.join(self.BACKENDS)}.",
                code="UNKNOWN_BACKEND",
            )
        self._config = _load_config(config)
        self.backend = backend
        self._scope = dict(scope) if scope else None

        options: dict[str, Any] = {}
        if timeout is not None:
            if timeout <= 0:
                raise InvalidRequestError(
                    f"timeout must be positive, got {timeout}", code="INVALID_REQUEST"
                )
            options["timeoutMs"] = int(timeout * 1000)
        if max_repairs is not None:
            if max_repairs < 0:
                raise InvalidRequestError(
                    f"max_repairs cannot be negative, got {max_repairs}", code="INVALID_REQUEST"
                )
            options["maxRepairs"] = max_repairs
        self._options = options

    # --- factories ----------------------------------------------------------

    @classmethod
    def mysql(cls, config: Config, **kwargs: Any) -> "QueryForge":
        """Bind to the MySQL dialect (``?`` placeholders, backtick quoting)."""
        return cls(config, "mysql", **kwargs)

    @classmethod
    def postgres(cls, config: Config, **kwargs: Any) -> "QueryForge":
        """Bind to the PostgreSQL dialect (``$1`` placeholders)."""
        return cls(config, "sql", **kwargs)

    #: ``sql`` is the engine's own id for the PostgreSQL dialect; both names are
    #: offered because the config files use ``sql`` while callers think in terms
    #: of the database they are actually pointed at.
    sql = postgres

    @classmethod
    def mongo(cls, config: Config, **kwargs: Any) -> "QueryForge":
        """Bind to MongoDB, producing a query document rather than SQL."""
        return cls(config, "mongo", **kwargs)

    # --- the model path -----------------------------------------------------

    def query(self, text: str) -> PendingQuery:
        """Describe a question. Nothing runs until a terminal method is called.

        Costs one model call (more if a repair is needed) when it does run.
        """
        if not isinstance(text, str) or not text.strip():
            raise InvalidRequestError(
                "query text must be a non-empty string", code="INVALID_REQUEST"
            )
        return PendingQuery(self, text, self._scope, self._options)

    # --- the deterministic path ---------------------------------------------

    def generate(
        self,
        ast: Mapping[str, Any],
        *,
        scope: Mapping[str, ScopeValue] | None = None,
        scope_in_ast: bool = False,
    ) -> Result:
        """Compile a pre-built AST. No model call, no network, no API key.

        The AST is validated against the config first, so an AST from an
        untrusted source cannot compile to something the config forbids.
        """
        options = dict(self._options)
        if scope_in_ast:
            options["scopeInAst"] = True
        payload = self._request("generate", options, scope)
        payload["ast"] = dict(ast)
        return Result.from_json(run_request(payload, self._timeout_seconds(options)))

    def validate(self, ast: Mapping[str, Any]) -> Result:
        """Check an AST against the config without compiling it.

        Returns the explanation on success; raises :class:`ValidationError` with
        a populated ``details`` list otherwise.
        """
        payload = self._request("validate", dict(self._options), None)
        payload["ast"] = dict(ast)
        return Result.from_json(run_request(payload, self._timeout_seconds(self._options)))

    # --- internals ----------------------------------------------------------

    def _translate(
        self,
        text: str,
        scope: Mapping[str, ScopeValue] | None,
        options: dict[str, Any],
    ) -> Result:
        payload = self._request("translate", options, scope)
        payload["query"] = text
        return Result.from_json(run_request(payload, self._timeout_seconds(options)))

    def _request(
        self,
        op: str,
        options: dict[str, Any],
        scope: Mapping[str, ScopeValue] | None,
    ) -> dict[str, Any]:
        payload: dict[str, Any] = {
            "op": op,
            "backend": self.backend,
            "config": self._config,
        }
        effective_scope = scope if scope is not None else self._scope
        if effective_scope:
            payload["scope"] = dict(effective_scope)
        if options:
            payload["options"] = options
        return payload

    @staticmethod
    def _timeout_seconds(options: Mapping[str, Any]) -> float | None:
        """Convert the request's timeout into the subprocess kill deadline."""
        ms = options.get("timeoutMs")
        return None if not ms else ms / 1000.0

    def __repr__(self) -> str:  # pragma: no cover - debugging aid
        entity = self._config.get("entity", "?")
        return f"QueryForge(entity={entity!r}, backend={self.backend!r})"


def engine_version() -> dict[str, Any]:
    """Return the bundled engine's version, protocol version and backends.

    Useful as an installation check: it exercises binary discovery, process
    spawning and JSON decoding without needing a config or an API key.
    """
    return run_request({"op": "version"})


def binary_path() -> str:
    """Return the path of the executable this SDK will run."""
    return str(find_binary())
