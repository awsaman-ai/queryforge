"""The compiled-query object returned by every terminal call."""

from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any

from .errors import QueryForgeError


@dataclass(frozen=True)
class ScopeFilter:
    """One caller-imposed filter that was AND-ed into the query.

    Reported back so an audit log can record exactly what was forced onto the
    query rather than having to re-derive it from the scope map. ``declared`` is
    true when the config registers the field, meaning its type and operator rules
    were enforced.
    """

    field_name: str
    operator: str
    value: Any
    declared: bool

    @classmethod
    def from_json(cls, obj: dict[str, Any]) -> "ScopeFilter":
        raw = obj.get("value") or {}
        return cls(
            field_name=obj.get("field", ""),
            operator=obj.get("operator", ""),
            # Unwrap the AST's tagged-union value into the plain Python payload.
            # A caller auditing scope wants "ACME", not {"kind": "string", "v": "ACME"}.
            value=raw.get("v") if isinstance(raw, dict) else raw,
            declared=bool(obj.get("declared", False)),
        )


@dataclass(frozen=True)
class Result:
    """A compiled query, plus everything the engine can say about it.

    Which fields are populated depends on the backend: SQL backends fill
    :attr:`sql` and :attr:`args`, document backends fill :attr:`doc`. The rest —
    :attr:`ast`, :attr:`explain`, :attr:`warnings`, :attr:`scope` — are filled
    for every backend.

    It is frozen because it describes something that already happened; mutating
    it could only ever make an audit record disagree with what ran.
    """

    backend: str
    sql: str = ""
    args: tuple[Any, ...] = ()
    doc: dict[str, Any] | None = None
    ast: dict[str, Any] | None = None
    explain: str = ""
    warnings: tuple[str, ...] = ()
    scope: tuple[ScopeFilter, ...] = ()
    provider_used: str = ""
    repair_attempts: int = 0
    raw: str = ""

    @classmethod
    def from_json(cls, obj: dict[str, Any]) -> "Result":
        return cls(
            backend=obj.get("backend", ""),
            sql=obj.get("sql", "") or "",
            args=tuple(obj.get("args") or ()),
            doc=obj.get("doc"),
            ast=obj.get("ast"),
            explain=obj.get("explain", "") or "",
            warnings=tuple(obj.get("warnings") or ()),
            scope=tuple(ScopeFilter.from_json(s) for s in obj.get("scope") or ()),
            provider_used=obj.get("providerUsed", "") or "",
            repair_attempts=int(obj.get("repairAttempts") or 0),
            raw=obj.get("raw", "") or "",
        )

    def require_sql(self) -> str:
        """Return the SQL, or explain why there is none.

        The alternative — returning an empty string for a Mongo result — would
        let a caller build ``cursor.execute("")`` and get an opaque driver error
        several frames away from the mistake.
        """
        if not self.sql:
            raise QueryForgeError(
                f"This query compiled to the {self.backend!r} backend, which produces a "
                f"query document rather than SQL. Use .to_mongo() or .result().doc instead.",
                code="WRONG_BACKEND",
            )
        return self.sql

    def require_doc(self) -> dict[str, Any]:
        """Return the query document, or explain why there is none."""
        if self.doc is None:
            raise QueryForgeError(
                f"This query compiled to the {self.backend!r} backend, which produces SQL "
                f"rather than a query document. Use .to_sql() and .to_args() instead.",
                code="WRONG_BACKEND",
            )
        return self.doc
