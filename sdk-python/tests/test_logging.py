"""Structured logging and fail-fast behaviour in the Python SDK.

Three properties are under test here, and they are the three the whole change
exists for:

1. **Nothing fails silently.** Every failure path raises, carries a code, and
   preserves the original cause through ``__cause__``.
2. **Failures are logged once, at the boundary, with usable fields.**
3. **Secrets and user data never reach a log**, even at DEBUG.

Assertions are on decoded FIELDS, never on a rendered log line, and never on a
timestamp. A test that string-matches log output fails the moment someone
reorders two attributes, which is how a team learns to distrust logging tests.
"""

from __future__ import annotations

import logging
import subprocess

import pytest

import queryforge
from queryforge import QueryForge
from queryforge.errors import ProtocolError, QueryForgeError
from queryforge.logging import (
    JsonFormatter,
    engine_level,
    is_configured,
    level_name_for_engine,
    new_request_id,
    parse_level,
    redact,
    scope_keys,
)


# ---------------------------------------------------------------------------
# Level plumbing
# ---------------------------------------------------------------------------


def test_parse_level_accepts_the_documented_names():
    assert parse_level("debug") == logging.DEBUG
    assert parse_level("INFO") == logging.INFO
    assert parse_level("  warn ") == logging.WARNING
    assert parse_level("warning") == logging.WARNING
    assert parse_level("error") == logging.ERROR
    assert parse_level("off") > logging.CRITICAL


@pytest.mark.parametrize("bad", ["trace", "verbose", "debgu", "1", ""])
def test_parse_level_rejects_a_typo_rather_than_defaulting(bad):
    # Both plausible defaults are harmful: "off" hides diagnostics from someone
    # who asked for them, "debug" starts writing detail nobody authorised.
    with pytest.raises(ValueError) as caught:
        parse_level(bad)
    assert "unknown log level" in str(caught.value)


def test_engine_level_maps_python_levels_down_not_up():
    # A caller setting a custom level between two standard ones must not have
    # records silently discarded by the engine. Rounding down is the safe way.
    assert level_name_for_engine(logging.DEBUG) == "debug"
    assert level_name_for_engine(15) == "info"  # between DEBUG and INFO
    assert level_name_for_engine(logging.INFO) == "info"
    assert level_name_for_engine(25) == "warn"
    assert level_name_for_engine(logging.ERROR) == "error"
    assert level_name_for_engine(logging.CRITICAL) == "off"


def test_logging_is_off_until_the_host_configures_it():
    # The default must be silence. A library that starts logging on import is a
    # library that ends up in someone's incident review.
    assert not is_configured()
    assert engine_level() is None


def test_setting_the_queryforge_logger_level_counts_as_configuring():
    logger = queryforge.logging.logger
    logger.setLevel(logging.INFO)
    assert is_configured()
    assert engine_level() == "info"


def test_a_root_basicconfig_does_not_count_as_configuring():
    # Someone turning debug on for their own application has not asked
    # QueryForge to spawn every engine in debug mode.
    logging.getLogger().setLevel(logging.DEBUG)
    try:
        assert not is_configured()
        assert engine_level() is None
    finally:
        logging.getLogger().setLevel(logging.WARNING)


# ---------------------------------------------------------------------------
# The wire: what the SDK asks the engine for
# ---------------------------------------------------------------------------


def test_default_requests_carry_no_logging_fields(fake_binary, ok_response, config, valid_ast):
    """The default request must stay byte-identical to a protocol-1.0 one.

    The engine rejects unknown fields, so an SDK that always sent ``logLevel``
    would turn every request into INVALID_REQUEST for anyone pointing
    QUERYFORGE_BINARY at an older build.
    """
    binary = fake_binary(ok_response(op="generate", sql="SELECT 1"))
    QueryForge.postgres(config).generate(valid_ast)

    options = binary.invocations()[0].get("options", {})
    assert "logLevel" not in options
    assert "requestId" not in options


def test_configured_logging_asks_the_engine_for_the_same_level(
    fake_binary, ok_response, config, valid_ast, captured_logs
):
    binary = fake_binary(ok_response(op="generate", sql="SELECT 1"))
    QueryForge.postgres(config).generate(valid_ast)

    options = binary.invocations()[0]["options"]
    assert options["logLevel"] == "debug"  # captured_logs sets DEBUG
    assert options["requestId"]  # generated, so the two halves correlate


def test_a_caller_supplied_request_id_reaches_both_the_log_and_the_engine(
    fake_binary, ok_response, config, captured_logs
):
    binary = fake_binary(ok_response(sql="SELECT 1"))
    QueryForge.postgres(config).query("delivered orders").request_id("trace-abc-123").to_sql()

    assert binary.invocations()[0]["options"]["requestId"] == "trace-abc-123"

    # Every record belonging to the call carries it. Binary resolution is
    # excluded on purpose: it is memoized once per process and belongs to no
    # particular request, so stamping a request id on it would be a lie.
    per_call = [
        r for r in captured_logs.records if r.name == "queryforge.transport"
    ]
    assert per_call, "the transport should have logged something"
    assert all(r.queryforge["request_id"] == "trace-abc-123" for r in per_call)


def test_request_id_rejects_an_empty_value(config):
    with pytest.raises(queryforge.InvalidRequestError):
        QueryForge.postgres(config).query("x").request_id("   ")


# ---------------------------------------------------------------------------
# Fields and levels
# ---------------------------------------------------------------------------


def test_a_successful_call_logs_one_info_record_with_the_canonical_fields(
    fake_binary, ok_response, config, captured_logs
):
    fake_binary(ok_response(sql="SELECT 1", repairAttempts=2, providerUsed="gemini"))
    QueryForge.postgres(config).query("delivered orders").to_sql()

    completed = [
        r for r in captured_logs.records if r.getMessage() == "engine request completed"
    ]
    assert len(completed) == 1, "exactly one outcome line per call"
    record = completed[0]
    assert record.levelno == logging.INFO

    fields = record.queryforge
    assert fields["library"] == "queryforge"
    assert fields["language"] == "python"
    assert fields["operation"] == "translate"
    assert fields["backend"] == "sql"
    assert fields["entity"] == "Order"
    assert fields["outcome"] == "ok"
    assert isinstance(fields["duration_ms"], int)
    assert fields["request_id"]
    # A translation that needed repairs cost extra model calls; that is worth
    # surfacing because it is almost always a closable config gap.
    assert fields["repair_attempts"] == 2
    assert fields["provider"] == "gemini"


@pytest.mark.parametrize(
    "code,expected_type",
    [
        ("MODEL_TRANSPORT", queryforge.ModelTransportError),
        ("VALIDATION_FAILED", queryforge.ValidationError),
        ("UNSUPPORTED_REQUEST", queryforge.UnsupportedRequestError),
        ("INVALID_SCOPE", queryforge.InvalidScopeError),
        ("GENERATE_FAILED", queryforge.GenerateError),
        ("TIMEOUT", queryforge.TimeoutError),
        ("INTERNAL", QueryForgeError),
    ],
)
def test_every_engine_failure_is_raised_and_logged_once(
    fake_binary, err_response, config, captured_logs, code, expected_type
):
    """No silent failures, and no duplicate stack traces.

    Both halves matter: the exception proves the caller cannot mistake the
    failure for success, and the single ERROR record proves the same problem is
    not being reported at three successive layers.
    """
    fake_binary(err_response(code, "the engine says no"))

    with pytest.raises(expected_type) as caught:
        QueryForge.postgres(config).query("delivered orders").to_sql()

    assert caught.value.code == code

    errors = [r for r in captured_logs.records if r.levelno >= logging.ERROR]
    assert len(errors) == 1, f"expected exactly 1 ERROR record, got {len(errors)}"
    fields = errors[0].queryforge
    assert fields["error_code"] == code
    assert fields["error_type"] == expected_type.__name__
    assert fields["outcome"] == "error"
    assert isinstance(fields["duration_ms"], int)
    # The traceback belongs in the log exactly once, and this is the once.
    assert errors[0].exc_info is not None


def test_an_unknown_error_code_degrades_to_the_base_class_and_is_still_logged(
    fake_binary, err_response, config, captured_logs
):
    # An SDK talking to a newer engine must degrade, not crash — and must not
    # lose the failure while degrading.
    fake_binary(err_response("SOMETHING_NEW_IN_2027", "from the future"))

    with pytest.raises(QueryForgeError) as caught:
        QueryForge.postgres(config).query("x").to_sql()

    assert caught.value.code == "SOMETHING_NEW_IN_2027"
    errors = [r for r in captured_logs.records if r.levelno >= logging.ERROR]
    assert len(errors) == 1
    assert errors[0].queryforge["error_code"] == "SOMETHING_NEW_IN_2027"


def test_a_crashed_engine_raises_rather_than_returning_nothing(
    fake_binary, config, captured_logs
):
    """A process that dies before answering is the worst silent-failure risk:
    stdout is empty, so a naive implementation returns an empty result and the
    caller compiles a query from nothing."""
    fake_binary(None, exit_code=3, stderr="segmentation fault")

    with pytest.raises(ProtocolError) as caught:
        QueryForge.postgres(config).query("delivered orders").to_sql()

    assert "produced no response" in str(caught.value)
    assert "segmentation fault" in str(caught.value)  # the only evidence there is
    assert len([r for r in captured_logs.records if r.levelno >= logging.ERROR]) == 1


def test_a_non_json_reply_raises_with_the_original_decode_error_chained(
    fake_binary, config
):
    fake_binary("this is not JSON")

    with pytest.raises(ProtocolError) as caught:
        QueryForge.postgres(config).query("x").to_sql()

    # The root cause must survive. Losing it turns "the engine wrote HTML" into
    # "something went wrong".
    assert caught.value.__cause__ is not None
    assert type(caught.value.__cause__).__name__ == "JSONDecodeError"


def test_a_hung_engine_is_killed_and_reported_with_the_cause_chained(
    fake_binary, config, monkeypatch
):
    """An engine that ignores its own deadline must still not hang the caller.

    The grace period is patched down rather than waited out: it is 15 seconds in
    production, and a test that sleeps through it would add 15 seconds to every
    CI run to prove one branch.
    """
    monkeypatch.setattr("queryforge._transport._KILL_GRACE_SECONDS", 0.05)
    fake_binary({"success": True, "protocol": "1.1"}, sleep_seconds=3.0)

    with pytest.raises(ProtocolError) as caught:
        QueryForge.postgres(config, timeout=0.05).query("x").to_sql()

    assert "did not respond" in str(caught.value)
    assert isinstance(caught.value.__cause__, subprocess.TimeoutExpired)


# ---------------------------------------------------------------------------
# Redaction and privacy canaries
#
# These are the tests that matter most. Each plants a distinctive string
# somewhere a naive implementation would echo it, and asserts it never appears
# in any record — at DEBUG, the most verbose the SDK gets.
# ---------------------------------------------------------------------------


@pytest.mark.parametrize(
    "raw,must_not_contain",
    [
        ("Authorization: Bearer sk-abcdefghijklmnop", "sk-abcdefghijklmnop"),
        ('{"api_key": "AIzaSyDsecretsecretsecret1234"}', "AIzaSyDsecretsecretsecret1234"),
        ("apiKey=super-secret-value", "super-secret-value"),
        ("password: hunter2hunter2", "hunter2hunter2"),
        ("token = abc123def456ghi", "abc123def456ghi"),
        ("postgres://user:topsecret@db.internal:5432/x", "topsecret"),
        ("plain sk-0123456789abcdefXYZ text", "sk-0123456789abcdefXYZ"),
    ],
)
def test_redact_scrubs_credentials(raw, must_not_contain):
    cleaned = redact(raw)
    assert must_not_contain not in cleaned
    assert "REDACTED" in cleaned


def test_redact_bounds_and_announces_truncation():
    # Silent truncation would make a 500-byte prefix look like the whole reply
    # and send someone hunting for a bug in text that was never the full story.
    cleaned = redact("x" * 5000)
    assert len(cleaned) < 5000
    assert "truncated" in cleaned


def test_redact_leaves_ordinary_text_alone():
    text = "unknown field 'agee' (did you mean: age?)"
    assert redact(text) == text


def test_engine_stderr_is_scrubbed_before_it_reaches_an_exception(
    fake_binary, config, captured_logs
):
    """The engine's stderr is quoted verbatim into a ProtocolError when the
    process dies. It is the engine's text, not ours — a crash can print
    anything, including a provider error echoing an Authorization header."""
    fake_binary(None, exit_code=1, stderr="fatal: Authorization: Bearer sk-LEAKED-TOKEN-9911")

    with pytest.raises(ProtocolError) as caught:
        QueryForge.postgres(config).query("x").to_sql()

    assert "sk-LEAKED-TOKEN-9911" not in str(caught.value)
    assert "sk-LEAKED-TOKEN-9911" not in captured_logs.text()


def test_the_question_never_reaches_a_log(fake_binary, ok_response, config, captured_logs):
    canary = "CANARY-QUESTION-py-3f9a"
    fake_binary(ok_response(sql="SELECT 1"))

    QueryForge.postgres(config).query(f"orders for {canary}").to_sql()

    assert canary not in captured_logs.text()


def test_the_question_never_reaches_a_log_on_the_failure_path(
    fake_binary, err_response, config, captured_logs
):
    canary = "CANARY-QUESTION-py-fail-71cd"
    fake_binary(err_response("MODEL_TRANSPORT", "dial tcp: connection refused"))

    with pytest.raises(queryforge.ModelTransportError):
        QueryForge.postgres(config).query(f"orders for {canary}").to_sql()

    assert canary not in captured_logs.text()


def test_scope_values_never_reach_a_log_but_the_keys_do(
    fake_binary, ok_response, config, captured_logs
):
    """Scope values are tenant, user and subscription ids — the most sensitive
    thing the SDK handles. The keys are exactly what an audit trail needs."""
    canary = "CANARY-TENANT-py-88ab"
    fake_binary(ok_response(sql="SELECT 1"))

    QueryForge.postgres(config).query("delivered orders").scope(
        {"customerName": canary}
    ).to_sql()

    assert canary not in captured_logs.text()
    assert any(f.get("scope_keys") == ["customerName"] for f in captured_logs.fields())


def test_the_config_never_reaches_a_log(fake_binary, ok_response, config, captured_logs):
    """A config carries physical table and column names, which plenty of
    organisations treat as confidential. Only its shape is logged."""
    fake_binary(ok_response(sql="SELECT 1"))
    QueryForge.postgres(config).query("delivered orders").to_sql()

    text = captured_logs.text()
    for secret in ("total_amount", "customer_name", "internalNote", "orders"):
        assert secret not in text, f"config content {secret!r} reached a log"
    # The shape must be there, or the log cannot tell two configs apart.
    assert any(f.get("entity") == "Order" for f in captured_logs.fields())


def test_scope_keys_helper_never_returns_values():
    assert scope_keys({"b": "secret", "a": "also-secret"}) == ["a", "b"]
    assert scope_keys(None) == []
    assert scope_keys({}) == []


# ---------------------------------------------------------------------------
# The opt-in helpers
# ---------------------------------------------------------------------------


def test_configure_touches_only_the_queryforge_logger():
    root = logging.getLogger()
    before = list(root.handlers), root.level

    handler = queryforge.logging.configure("debug")
    try:
        assert (list(root.handlers), root.level) == before, "the root logger must not be touched"
        assert handler in queryforge.logging.logger.handlers
        assert queryforge.logging.logger.propagate is False
    finally:
        queryforge.logging.logger.removeHandler(handler)


def test_json_formatter_emits_the_canonical_fields():
    import io
    import json as json_module

    stream = io.StringIO()
    handler = queryforge.logging.configure("info", stream=stream)
    try:
        queryforge.logging.log(
            queryforge.logging.logger,
            logging.INFO,
            "test event",
            operation="translate",
            error_code="MODEL_TRANSPORT",
        )
    finally:
        queryforge.logging.logger.removeHandler(handler)

    payload = json_module.loads(stream.getvalue().strip())
    assert payload["level"] == "INFO"
    assert payload["msg"] == "test event"
    assert payload["library"] == "queryforge"
    assert payload["language"] == "python"
    assert payload["operation"] == "translate"
    assert payload["error_code"] == "MODEL_TRANSPORT"
    # Not a brittle equality: the exact timestamp is never asserted anywhere in
    # this file, only that the field exists.
    assert "time" in payload


def test_a_field_colliding_with_a_logrecord_attribute_does_not_crash_logging(
    captured_logs,
):
    """``Logger.makeRecord`` raises KeyError when ``extra`` shadows a built-in
    attribute. A logging call that crashes the program it was added to debug is
    the worst possible outcome, so collisions are prefixed instead."""
    queryforge.logging.log(
        queryforge.logging.logger,
        logging.INFO,
        "collision",
        module="not-the-real-module",
        message="not-the-real-message",
    )

    record = captured_logs.records[-1]
    assert record.qf_module == "not-the-real-module"
    assert record.qf_message == "not-the-real-message"
    # And the real attributes survive.
    assert record.getMessage() == "collision"


def test_new_request_id_is_short_and_unique():
    ids = {new_request_id() for _ in range(500)}
    assert len(ids) == 500
    assert all(len(i) == 12 for i in ids)


def test_json_formatter_is_importable_standalone():
    # Named in the docs as something a host attaches to their own handler.
    assert issubclass(JsonFormatter, logging.Formatter)

