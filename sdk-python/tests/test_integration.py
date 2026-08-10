"""Tests against the real engine binary.

These are the ones that matter most: they prove the SDK and the Go engine agree
on the wire format. They use only the deterministic ops, so they need no API key
and make no network call.
"""

from __future__ import annotations

import json

import pytest

from queryforge import (
    QueryForge,
    QueryForgeError,
    UnknownBackendError,
    ValidationError,
    engine_version,
)

pytestmark = pytest.mark.usefixtures("real_binary")


# --- handshake ---------------------------------------------------------------


def test_engine_version_reports_protocol_and_backends():
    info = engine_version()
    assert info["success"] is True
    assert info["protocol"].startswith("1.")
    # An SDK uses this list to reject a typo'd backend before spending a model
    # call, so it must actually name the shipped generators.
    assert set(info["backends"]) >= {"sql", "mysql", "mongo"}


# --- the deterministic path --------------------------------------------------


def test_generate_postgres(config, valid_ast):
    result = QueryForge.postgres(config).generate(valid_ast)

    assert result.backend == "sql"
    assert "FROM orders" in result.sql
    # The value must be bound, never inlined — the injection guarantee has to
    # survive the round trip through JSON and into a Python tuple.
    assert result.args == ("DELIVERED",)
    assert "DELIVERED" not in result.sql
    assert result.explain


def test_generate_mysql_uses_its_own_placeholder_style(config, valid_ast):
    postgres = QueryForge.postgres(config).generate(valid_ast)
    mysql = QueryForge.mysql(config).generate(valid_ast)

    assert "$1" in postgres.sql
    assert "?" in mysql.sql
    # Same AST, same bound values — only the dialect differs. This is the
    # promise the whole single-source-of-truth design rests on.
    assert postgres.args == mysql.args


def test_generate_mongo_returns_a_document_not_sql(config, valid_ast):
    result = QueryForge.mongo(config).generate(valid_ast)

    assert result.sql == ""
    assert result.doc is not None
    assert result.doc["collection"] == "orders"
    assert result.doc["filter"]["status"] == "DELIVERED"


def test_non_returnable_field_never_reaches_the_projection(config, valid_ast):
    """A field marked returnable:false must not appear even in the wide form."""
    result = QueryForge.postgres(config).generate(valid_ast)
    assert "internalNote" not in result.sql
    assert "internal_note" not in result.sql


def test_scope_is_applied_and_reported(config, valid_ast):
    result = QueryForge.postgres(config).generate(valid_ast, scope={"customerName": "ACME"})

    assert "customer_name" in result.sql
    assert set(result.args) == {"ACME", "DELIVERED"}

    assert len(result.scope) == 1
    applied = result.scope[0]
    assert applied.field_name == "customerName"
    assert applied.operator == "equals"
    # The tagged-union wrapper is unwrapped for the caller: an audit log wants
    # "ACME", not {"kind": "string", "v": "ACME"}.
    assert applied.value == "ACME"
    assert applied.declared is True


def test_scope_stays_out_of_the_reported_ast_by_default(config, valid_ast):
    """The default AST round-trips: feed it back and get the identical query."""
    forge = QueryForge.postgres(config)
    first = forge.generate(valid_ast, scope={"customerName": "ACME"})
    second = forge.generate(first.ast, scope={"customerName": "ACME"})
    assert first.sql == second.sql
    assert first.args == second.args


def test_scope_in_ast_reports_the_effective_tree(config, valid_ast):
    result = QueryForge.postgres(config).generate(
        valid_ast, scope={"customerName": "ACME"}, scope_in_ast=True
    )
    assert result.ast["filter"]["type"] == "logical"
    assert result.ast["filter"]["op"] == "AND"


def test_warnings_surface_non_indexed_filters(config, valid_ast):
    result = QueryForge.postgres(config).generate(valid_ast, scope={"customerName": "ACME"})
    assert any("customerName" in w for w in result.warnings)


# --- validation --------------------------------------------------------------


def test_validate_accepts_a_legal_ast(config, valid_ast):
    result = QueryForge.postgres(config).validate(valid_ast)
    assert result.explain
    assert result.sql == ""  # validate must not compile anything


def test_unknown_field_raises_with_structured_details(config):
    bad = {
        "version": "1.0",
        "entity": "Order",
        "filter": {
            "type": "comparison",
            "field": "amont",  # near-miss for "amount"
            "operator": "gt",
            "value": {"kind": "number", "v": 100},
        },
    }
    with pytest.raises(ValidationError) as exc:
        QueryForge.postgres(config).generate(bad)

    err = exc.value
    assert err.code == "VALIDATION_FAILED"
    assert err.details, "an SDK must be able to name the offending field"

    detail = err.details[0]
    assert detail.code == "unknown_field"
    assert detail.field == "amont"
    assert detail.path
    # The suggestion list is the difference between "unknown field" and "did you
    # mean amount?", and must arrive as data rather than only as prose.
    assert "amount" in detail.suggestions


def test_out_of_domain_enum_is_rejected(config):
    bad = {
        "version": "1.0",
        "entity": "Order",
        "filter": {
            "type": "comparison",
            "field": "status",
            "operator": "equals",
            "value": {"kind": "enum", "v": "TELEPORTED"},
        },
    }
    with pytest.raises(ValidationError) as exc:
        QueryForge.postgres(config).generate(bad)
    assert exc.value.details[0].code == "value_out_of_domain"


def test_entity_mismatch_is_rejected(config, valid_ast):
    bad = dict(valid_ast, entity="Invoice")
    with pytest.raises(ValidationError):
        QueryForge.postgres(config).generate(bad)


# --- misuse ------------------------------------------------------------------


def test_to_sql_on_a_mongo_forge_explains_itself(config, valid_ast):
    """Returning "" would let the caller execute an empty statement and get an
    opaque driver error several frames from the mistake."""
    result = QueryForge.mongo(config).generate(valid_ast)
    with pytest.raises(QueryForgeError) as exc:
        result.require_sql()
    assert "to_mongo" in str(exc.value)


def test_to_mongo_on_a_sql_forge_explains_itself(config, valid_ast):
    result = QueryForge.postgres(config).generate(valid_ast)
    with pytest.raises(QueryForgeError) as exc:
        result.require_doc()
    assert "to_sql" in str(exc.value)


def test_unknown_backend_fails_before_spawning_anything():
    with pytest.raises(UnknownBackendError):
        QueryForge({"entity": "X", "fields": []}, "oracle")


def test_invalid_scope_is_told_apart_from_a_bad_question(config, valid_ast):
    from queryforge import InvalidScopeError

    with pytest.raises(InvalidScopeError):
        QueryForge.postgres(config).generate(valid_ast, scope={"": "nothing"})


def test_bad_config_is_rejected_by_the_engine(valid_ast):
    """A config that parses as JSON but breaks a structural rule."""
    from queryforge import InvalidConfigError

    broken = {"entity": "Order", "fields": [{"name": "status", "type": "enum"}]}  # enum, no values
    with pytest.raises(InvalidConfigError):
        QueryForge.postgres(broken).generate(valid_ast)


def test_config_error_never_echoes_a_pasted_secret(valid_ast):
    """The engine rejects a key pasted where a variable name belongs; the SDK
    must not reprint the secret into whatever log the exception lands in."""
    from queryforge import InvalidConfigError

    leaky = {
        "entity": "Order",
        "model": {"apiKeyEnv": "sk-ant-supersecret"},
        "fields": [{"name": "status", "type": "string"}],
    }
    with pytest.raises(InvalidConfigError) as exc:
        QueryForge.postgres(leaky).generate(valid_ast)
    assert "supersecret" not in str(exc.value)


# --- config loading ----------------------------------------------------------


def test_config_accepts_a_file_path(tmp_path, config, valid_ast):
    path = tmp_path / "orders.config.json"
    path.write_text(json.dumps(config))

    from_str = QueryForge.postgres(str(path)).generate(valid_ast)
    from_path = QueryForge.postgres(path).generate(valid_ast)
    from_dict = QueryForge.postgres(config).generate(valid_ast)

    assert from_str.sql == from_path.sql == from_dict.sql


def test_config_accepts_json_text(config, valid_ast):
    result = QueryForge.postgres(json.dumps(config)).generate(valid_ast)
    assert "FROM orders" in result.sql
