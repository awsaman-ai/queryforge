"""Tests of the SDK's own logic, against a scripted fake engine.

Everything here covers behaviour the real binary cannot be asked to produce
offline: each error code, a model call with no API key, a crash, corrupted
output, a protocol mismatch, and the laziness/caching of the fluent builder.
"""

from __future__ import annotations

import json
import uuid
from datetime import date, datetime
from decimal import Decimal

import pytest

from queryforge import (
    BinaryNotFoundError,
    GenerateError,
    InvalidConfigError,
    InvalidRequestError,
    InvalidScopeError,
    ModelOutputError,
    ModelTransportError,
    ProtocolError,
    QueryForge,
    QueryForgeError,
    TimeoutError,
    UnknownBackendError,
    UnsupportedRequestError,
    ValidationError,
)


@pytest.fixture
def forge(config):
    """A forge bound to the SQL backend; the fake binary answers for it."""
    return QueryForge.postgres(config)


# --- the fluent surface ------------------------------------------------------


def test_query_is_lazy_until_a_terminal_call(fake_binary, forge, ok_response):
    fake = fake_binary(ok_response(sql="SELECT 1"))

    pending = forge.query("delivered orders")
    assert fake.call_count == 0, "describing a query must not spawn anything"

    pending.to_sql()
    assert fake.call_count == 1


def test_result_is_cached_across_terminal_calls(fake_binary, forge, ok_response):
    """Reading several facts off one query must cost one model call, not four."""
    fake = fake_binary(ok_response(sql="SELECT 1", explain="prose", warnings=["w"], args=[7]))

    pending = forge.query("delivered orders")
    pending.to_sql()
    pending.explain()
    pending.to_args()
    pending.warnings()

    assert fake.call_count == 1


def test_builders_return_new_objects(fake_binary, forge, ok_response):
    """A partially-configured query must be reusable as a template, which is
    only true if the builders do not mutate in place."""
    fake = fake_binary(ok_response(sql="SELECT 1"))

    base = forge.query("open orders")
    scoped = base.scope({"tenantId": "t1"})

    assert scoped is not base
    base.to_sql()
    scoped.to_sql()

    sent = fake.invocations()
    assert "scope" not in sent[0], "the base query was mutated by .scope()"
    assert sent[1]["scope"] == {"tenantId": "t1"}


def test_scope_merges_rather_than_replaces(fake_binary, forge, ok_response):
    fake = fake_binary(ok_response(sql="SELECT 1"))
    forge.query("orders").scope({"tenantId": "t1"}).scope({"ownerId": "u9"}).to_sql()
    assert fake.invocations()[0]["scope"] == {"tenantId": "t1", "ownerId": "u9"}


def test_constructor_scope_applies_to_every_query(fake_binary, config, ok_response):
    fake = fake_binary(ok_response(sql="SELECT 1"))
    QueryForge.postgres(config, scope={"tenantId": "t1"}).query("orders").to_sql()
    assert fake.invocations()[0]["scope"] == {"tenantId": "t1"}


def test_options_reach_the_wire(fake_binary, forge, ok_response):
    fake = fake_binary(ok_response(sql="SELECT 1"))

    (
        forge.query("orders")
        .timeout(2.5)
        .max_repairs(0)
        .include_raw()
        .scope_in_ast()
        .to_sql()
    )

    options = fake.invocations()[0]["options"]
    assert options["timeoutMs"] == 2500
    assert options["maxRepairs"] == 0  # 0 must survive as a value, not vanish as a default
    assert options["includeRaw"] is True
    assert options["scopeInAst"] is True


def test_request_carries_op_backend_and_config(fake_binary, config, ok_response):
    fake = fake_binary(ok_response(sql="SELECT 1"))
    QueryForge.mysql(config).query("orders").to_sql()

    sent = fake.invocations()[0]
    assert sent["op"] == "translate"
    assert sent["backend"] == "mysql"
    assert sent["config"]["entity"] == "Order"


def test_empty_query_text_is_refused_locally(fake_binary, forge, ok_response):
    """A blank question must not cost a process spawn, let alone a model call."""
    fake = fake_binary(ok_response())
    for text in ("", "   ", "\n\t"):
        with pytest.raises(InvalidRequestError):
            forge.query(text)
    assert fake.call_count == 0


@pytest.mark.parametrize("bad", [0, -1, -0.5])
def test_non_positive_timeout_is_refused(forge, bad):
    with pytest.raises(InvalidRequestError):
        forge.query("orders").timeout(bad)


def test_negative_max_repairs_is_refused(forge):
    with pytest.raises(InvalidRequestError):
        forge.query("orders").max_repairs(-1)


# --- error mapping -----------------------------------------------------------


@pytest.mark.parametrize(
    ("code", "expected"),
    [
        ("INVALID_REQUEST", InvalidRequestError),
        ("UNKNOWN_OP", InvalidRequestError),
        ("INVALID_CONFIG", InvalidConfigError),
        ("UNKNOWN_BACKEND", UnknownBackendError),
        ("INVALID_SCOPE", InvalidScopeError),
        ("VALIDATION_FAILED", ValidationError),
        ("UNSUPPORTED_REQUEST", UnsupportedRequestError),
        ("MODEL_OUTPUT", ModelOutputError),
        ("MODEL_TRANSPORT", ModelTransportError),
        ("GENERATE_FAILED", GenerateError),
        ("TIMEOUT", TimeoutError),
        ("INTERNAL", QueryForgeError),
    ],
)
def test_every_protocol_code_maps_to_an_exception(fake_binary, forge, code, expected, err_response):
    fake_binary(err_response(code, "something went wrong"))
    with pytest.raises(expected) as exc:
        forge.query("orders").to_sql()
    assert exc.value.code == code
    assert "something went wrong" in str(exc.value)


def test_an_unrecognised_code_still_raises_the_base_class(fake_binary, forge, err_response):
    """An SDK talking to a newer binary must degrade, not crash: a code this
    version predates has to surface as QueryForgeError with the code intact."""
    fake_binary(err_response("SOME_FUTURE_CODE", "from a newer engine"))
    with pytest.raises(QueryForgeError) as exc:
        forge.query("orders").to_sql()
    assert exc.value.code == "SOME_FUTURE_CODE"


def test_validation_details_are_preserved(fake_binary, forge, err_response):
    fake_binary(
        err_response(
            "VALIDATION_FAILED",
            "filter: unknown field",
            details=[
                {
                    "code": "unknown_field",
                    "path": "filter",
                    "field": "agee",
                    "message": "filter: unknown field \"agee\"",
                    "suggestions": ["age", "amount"],
                }
            ],
        )
    )
    with pytest.raises(ValidationError) as exc:
        forge.query("orders").to_sql()

    detail = exc.value.details[0]
    assert detail.code == "unknown_field"
    assert detail.field == "agee"
    assert detail.suggestions == ["age", "amount"]


def test_error_without_a_message_still_reads(fake_binary, forge):
    """Defensive: a malformed error object must not produce a blank exception."""
    fake_binary({"success": False, "protocol": "1.0", "code": "INTERNAL"})
    with pytest.raises(QueryForgeError) as exc:
        forge.query("orders").to_sql()
    assert str(exc.value).strip()


# --- a misbehaving executable ------------------------------------------------


def test_a_crash_with_no_output_is_a_protocol_error(fake_binary, forge):
    fake_binary(None, exit_code=3, stderr="segmentation fault")
    with pytest.raises(ProtocolError) as exc:
        forge.query("orders").to_sql()
    # stderr is the only evidence of what happened, so it has to be carried
    # through rather than replaced by a bare exit code.
    assert "segmentation fault" in str(exc.value)
    assert "3" in str(exc.value)


def test_non_json_output_is_a_protocol_error(fake_binary, forge):
    fake_binary("panic: runtime error: index out of range")
    with pytest.raises(ProtocolError) as exc:
        forge.query("orders").to_sql()
    assert "not JSON" in str(exc.value)


def test_a_json_non_object_is_a_protocol_error(fake_binary, forge):
    fake_binary("[1, 2, 3]")
    with pytest.raises(ProtocolError):
        forge.query("orders").to_sql()


def test_a_huge_garbage_stream_is_truncated_in_the_message(fake_binary, forge):
    """A corrupted stream can be arbitrarily long, and the exception message is
    going into someone's log."""
    fake_binary("x" * 100_000)
    with pytest.raises(ProtocolError) as exc:
        forge.query("orders").to_sql()
    assert len(str(exc.value)) < 2000


def test_a_major_protocol_mismatch_is_refused(fake_binary, forge):
    fake_binary({"success": True, "protocol": "2.0", "sql": "SELECT 1"})
    with pytest.raises(ProtocolError) as exc:
        forge.query("orders").to_sql()
    assert "2.0" in str(exc.value)


def test_a_minor_protocol_bump_is_accepted(fake_binary, forge):
    """Additive changes must not break an older SDK — that is the whole point of
    versioning the protocol rather than the binary alone."""
    fake_binary({"success": True, "protocol": "1.7", "backend": "sql", "sql": "SELECT 1"})
    assert forge.query("orders").to_sql() == "SELECT 1"


def test_a_response_with_no_protocol_field_is_refused(fake_binary, forge):
    fake_binary({"success": True, "sql": "SELECT 1"})
    with pytest.raises(ProtocolError) as exc:
        forge.query("orders").to_sql()
    assert "protocol" in str(exc.value)


def test_unknown_response_fields_are_ignored(fake_binary, forge, ok_response):
    """The mirror of the above: a newer binary adding an optional field must not
    break this SDK."""
    fake_binary(ok_response(sql="SELECT 1", somethingNew={"added": "later"}))
    assert forge.query("orders").to_sql() == "SELECT 1"


def test_a_hung_binary_is_killed_and_reported(fake_binary, forge, ok_response):
    """The engine enforces its own deadline; this outer bound only catches an
    engine that has hung badly enough not to honour it."""
    fake_binary(ok_response(sql="SELECT 1"), sleep_seconds=30)

    from queryforge import _transport

    original = _transport._KILL_GRACE_SECONDS
    _transport._KILL_GRACE_SECONDS = 0.5
    try:
        with pytest.raises(ProtocolError) as exc:
            forge.query("orders").timeout(0.1).to_sql()
    finally:
        _transport._KILL_GRACE_SECONDS = original
    assert "killed" in str(exc.value)


# --- binary discovery --------------------------------------------------------


def test_a_missing_override_is_reported_not_silently_ignored(monkeypatch, forge, tmp_path):
    """Falling back to the bundled binary here would run a *different* engine
    than the one the user named — exactly what the variable exists to prevent."""
    monkeypatch.setenv("QUERYFORGE_BINARY", str(tmp_path / "nope"))
    with pytest.raises(BinaryNotFoundError) as exc:
        forge.query("orders").to_sql()
    assert "nope" in str(exc.value)


def test_a_non_executable_override_says_how_to_fix_it(monkeypatch, forge, tmp_path):
    path = tmp_path / "queryforge"
    path.write_text("#!/bin/sh\ntrue\n")
    path.chmod(0o644)
    monkeypatch.setenv("QUERYFORGE_BINARY", str(path))

    with pytest.raises(BinaryNotFoundError) as exc:
        forge.query("orders").to_sql()
    assert "chmod" in str(exc.value)


def test_platform_tag_is_one_of_the_release_targets():
    from queryforge import platform_tag

    assert platform_tag() in {
        "linux-amd64",
        "linux-arm64",
        "darwin-amd64",
        "darwin-arm64",
        "windows-amd64",
        "windows-arm64",
    }


# --- config loading ----------------------------------------------------------


def test_a_missing_config_file_names_the_path(tmp_path):
    with pytest.raises(InvalidConfigError) as exc:
        QueryForge.postgres(tmp_path / "absent.json")
    assert "absent.json" in str(exc.value)


def test_malformed_config_json_is_reported_locally(tmp_path):
    path = tmp_path / "broken.json"
    path.write_text('{"entity": "Order",')
    with pytest.raises(InvalidConfigError) as exc:
        QueryForge.postgres(path)
    assert "not valid JSON" in str(exc.value)


def test_a_config_that_is_not_an_object_is_refused(tmp_path):
    path = tmp_path / "list.json"
    path.write_text("[1, 2, 3]")
    with pytest.raises(InvalidConfigError):
        QueryForge.postgres(path)


def test_a_config_of_the_wrong_type_is_refused():
    with pytest.raises(InvalidConfigError):
        QueryForge.postgres(42)


def test_json_text_is_told_apart_from_a_path(fake_binary, config, ok_response):
    """A string starting with '{' is JSON; anything else is a path. No real path
    begins with a brace, so this is unambiguous in practice."""
    fake_binary(ok_response(sql="SELECT 1"))
    forge = QueryForge.postgres(json.dumps(config))
    forge.query("orders").to_sql()


def test_the_config_is_copied_not_aliased(fake_binary, config, ok_response):
    """Mutating the caller's dict after construction must not change what a
    later query compiles against."""
    fake_binary(ok_response(sql="SELECT 1"))
    forge = QueryForge.postgres(config)
    config["entity"] = "Mutated"
    forge.query("orders").to_sql()


# --- scope serialization -----------------------------------------------------


def test_common_session_types_are_serialized(fake_binary, forge, ok_response):
    """Scope values come from an application session and arrive as whatever type
    that application uses; dates, Decimals and UUIDs are the ones that reliably
    turn up and that json refuses."""
    fake = fake_binary(ok_response(sql="SELECT 1"))

    forge.query("orders").scope(
        {
            "createdAt": datetime(2026, 1, 2, 3, 4, 5),
            "day": date(2026, 1, 2),
            "amount": Decimal("10.50"),
            "tenantId": uuid.UUID("12345678-1234-5678-1234-567812345678"),
        }
    ).to_sql()

    scope = fake.invocations()[0]["scope"]
    assert scope["createdAt"] == "2026-01-02T03:04:05"
    assert scope["day"] == "2026-01-02"
    assert scope["amount"] == 10.5
    assert scope["tenantId"] == "12345678-1234-5678-1234-567812345678"


def test_an_unserializable_scope_value_names_the_type(fake_binary, forge, ok_response):
    fake_binary(ok_response(sql="SELECT 1"))

    class Session:
        pass

    with pytest.raises(TypeError) as exc:
        forge.query("orders").scope({"tenantId": Session()}).to_sql()
    assert "Session" in str(exc.value)


def test_lists_pass_through_for_in_style_filters(fake_binary, forge, ok_response):
    fake = fake_binary(ok_response(sql="SELECT 1"))
    forge.query("orders").scope({"tenantId": ["t1", "t2"]}).to_sql()
    assert fake.invocations()[0]["scope"]["tenantId"] == ["t1", "t2"]


# --- result decoding ---------------------------------------------------------


def test_result_exposes_every_reported_field(fake_binary, forge, ok_response):
    fake_binary(
        ok_response(
            sql="SELECT 1",
            args=[1, "two", True, None],
            explain="prose",
            warnings=["slow"],
            ast={"version": "1.0", "entity": "Order"},
            providerUsed="groq/llama",
            repairAttempts=2,
            raw="{...}",
            scope=[
                {
                    "field": "tenantId",
                    "operator": "equals",
                    "value": {"kind": "string", "v": "t1"},
                    "declared": False,
                }
            ],
        )
    )
    result = forge.query("orders").include_raw().result()

    assert result.sql == "SELECT 1"
    assert result.args == (1, "two", True, None)
    assert result.explain == "prose"
    assert result.warnings == ("slow",)
    assert result.ast["entity"] == "Order"
    assert result.provider_used == "groq/llama"
    assert result.repair_attempts == 2
    assert result.raw == "{...}"
    assert result.scope[0].value == "t1"
    assert result.scope[0].declared is False


def test_a_sparse_response_decodes_to_defaults(fake_binary, forge):
    """Every field but success/protocol is omitempty on the wire, so the decoder
    must not assume any of them are present."""
    fake_binary({"success": True, "protocol": "1.0"})
    result = forge.query("orders").result()

    assert result.sql == ""
    assert result.args == ()
    assert result.warnings == ()
    assert result.scope == ()
    assert result.repair_attempts == 0
