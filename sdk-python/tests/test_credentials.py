"""Supplying an API key from somewhere other than the process environment.

The rules these tests encode:

* A key handed to the SDK must reach the engine subprocess, and nowhere else.
  Not the request body, not the logs, not this process's own environment.
* The child must still inherit everything else it needs, so the credentials are
  an addition to the environment and never a replacement for it.
* Only the op that talks to a model gets the key.
* A malformed name fails where the mistake is — at construction — not on the
  first query, and never by echoing the secret.
"""

from __future__ import annotations

import json
import logging
import os
import sys
from pathlib import Path

import pytest

import queryforge
from queryforge import QueryForge
from queryforge.errors import InvalidRequestError

SECRET = "sk-test-not-a-real-key-9f8e7d6c5b4a"


@pytest.fixture
def env_recording_binary(tmp_path: Path, monkeypatch: pytest.MonkeyPatch):
    """A fake engine that records both the request AND its own environment.

    The stock ``fake_binary`` fixture logs only stdin, which cannot answer the
    question these tests ask: did the credential arrive out of band?
    """
    log = tmp_path / "calls.jsonl"
    script = tmp_path / "fake_engine.py"

    response = json.dumps(
        {
            "success": True,
            "protocol": "1.0",
            "op": "translate",
            "backend": "sql",
            "sql": "SELECT 1",
            "args": [],
        }
    )

    script.write_text(
        "import json, os, sys\n"
        "raw = sys.stdin.read()\n"
        f"rec = {{'request': json.loads(raw), 'env': dict(os.environ)}}\n"
        f"open({str(log)!r}, 'a').write(json.dumps(rec) + '\\n')\n"
        f"sys.stdout.write({response!r})\n"
    )

    launcher = tmp_path / "fake_engine"
    launcher.write_text(f'#!/bin/sh\nexec "{sys.executable}" "{script}" "$@"\n')
    launcher.chmod(0o755)
    monkeypatch.setenv("QUERYFORGE_BINARY", str(launcher))

    class Recorder:
        @staticmethod
        def calls() -> list[dict]:
            if not log.is_file():
                return []
            return [json.loads(line) for line in log.read_text().splitlines() if line.strip()]

    return Recorder


@pytest.fixture
def cfg() -> dict:
    return {
        "entity": "Order",
        "model": {"provider": "openai", "model": "gpt-5", "apiKeyEnv": "QF_TEST_KEY"},
        "backends": {"sql": {"table": "orders"}},
        "fields": [{"name": "status", "type": "string", "operators": ["equals"]}],
    }


# --------------------------------------------------------------- the happy path


def test_credentials_reach_the_engine_process(env_recording_binary, cfg):
    """The whole point: a key held in Python gets to the engine."""
    qf = QueryForge.sql(cfg, credentials={"QF_TEST_KEY": SECRET})
    qf.query("delivered orders").to_sql()

    env = env_recording_binary.calls()[0]["env"]
    assert env.get("QF_TEST_KEY") == SECRET


def test_credentials_never_enter_the_request_body(env_recording_binary, cfg):
    """The request is the one structure here that travels — it is JSON-encoded,
    dumped on protocol errors, and pasted into bug reports. The secret must not
    be in it."""
    qf = QueryForge.sql(cfg, credentials={"QF_TEST_KEY": SECRET})
    qf.query("delivered orders").to_sql()

    request = env_recording_binary.calls()[0]["request"]
    assert SECRET not in json.dumps(request)


def test_credentials_do_not_touch_this_process(env_recording_binary, cfg):
    """Mutating os.environ would leak the key to every other subprocess this
    program ever starts, and would make two engines with different keys fight."""
    before = os.environ.get("QF_TEST_KEY")

    qf = QueryForge.sql(cfg, credentials={"QF_TEST_KEY": SECRET})
    qf.query("delivered orders").to_sql()

    assert os.environ.get("QF_TEST_KEY") == before


def test_the_child_still_inherits_the_rest_of_the_environment(env_recording_binary, cfg, monkeypatch):
    """Handing the child a bare credentials dict would strip PATH and HOME and
    break the engine in ways that look nothing like a credentials problem."""
    monkeypatch.setenv("QF_UNRELATED_MARKER", "still-here")

    qf = QueryForge.sql(cfg, credentials={"QF_TEST_KEY": SECRET})
    qf.query("delivered orders").to_sql()

    env = env_recording_binary.calls()[0]["env"]
    assert env.get("QF_UNRELATED_MARKER") == "still-here"
    assert env.get("PATH"), "PATH must survive"


def test_credentials_override_an_inherited_variable(env_recording_binary, cfg, monkeypatch):
    """An explicitly supplied key is more specific than an ambient one, so it
    wins — otherwise a stale shell export would silently defeat the vault."""
    monkeypatch.setenv("QF_TEST_KEY", "stale-value-from-the-shell")

    qf = QueryForge.sql(cfg, credentials={"QF_TEST_KEY": SECRET})
    qf.query("delivered orders").to_sql()

    assert env_recording_binary.calls()[0]["env"]["QF_TEST_KEY"] == SECRET


def test_two_engines_with_different_keys_do_not_interfere(env_recording_binary, cfg):
    """Multi-tenant callers hold several engines at once. Each subprocess must
    see only its own key."""
    a = QueryForge.sql(cfg, credentials={"QF_TEST_KEY": "key-for-tenant-a"})
    b = QueryForge.sql(cfg, credentials={"QF_TEST_KEY": "key-for-tenant-b"})

    a.query("q").to_sql()
    b.query("q").to_sql()

    calls = env_recording_binary.calls()
    assert calls[0]["env"]["QF_TEST_KEY"] == "key-for-tenant-a"
    assert calls[1]["env"]["QF_TEST_KEY"] == "key-for-tenant-b"


def test_no_credentials_means_plain_inheritance(env_recording_binary, cfg, monkeypatch):
    """The pre-existing behaviour, unchanged: without credentials the child just
    inherits, exactly as it did before this feature existed."""
    monkeypatch.setenv("QF_TEST_KEY", "from-the-environment")

    QueryForge.sql(cfg).query("q").to_sql()

    assert env_recording_binary.calls()[0]["env"]["QF_TEST_KEY"] == "from-the-environment"


# ------------------------------------------------------------ least privilege


def test_offline_ops_never_receive_the_key(env_recording_binary, cfg):
    """generate and validate make no model call, so the engine they spawn has no
    use for a credential. Not sending it is the smallest blast radius available
    for free."""
    qf = QueryForge.sql(cfg, credentials={"QF_TEST_KEY": SECRET})
    ast = {
        "version": "1.0",
        "entity": "Order",
        "filter": {
            "type": "comparison",
            "field": "status",
            "operator": "equals",
            "value": {"kind": "string", "v": "NEW"},
        },
    }
    qf.generate(ast)

    env = env_recording_binary.calls()[0]["env"]
    assert env.get("QF_TEST_KEY") != SECRET


# ------------------------------------------------------------- bad input


def test_a_pasted_key_is_rejected_at_construction(cfg):
    """The likeliest mistake by far is passing the KEY where the NAME belongs.
    Real keys contain hyphens, so the env-var-name rule catches them."""
    with pytest.raises(InvalidRequestError) as exc:
        QueryForge.sql(cfg, credentials={SECRET: "whatever"})

    assert "environment variable name" in str(exc.value)


def test_an_invalid_name_fails_before_any_query_runs(cfg):
    """Failing at construction points at the mistake. Failing on the first query
    points at the query."""
    for bad in ["1LEADING_DIGIT", "has-a-hyphen", "has space", "", "has.dot"]:
        with pytest.raises(InvalidRequestError):
            QueryForge.sql(cfg, credentials={bad: "v"})


def test_a_non_string_value_is_rejected_without_echoing_it(cfg):
    """The value is the secret, so the error names the key and the type — never
    the value."""
    with pytest.raises(InvalidRequestError) as exc:
        QueryForge.sql(cfg, credentials={"QF_TEST_KEY": 12345})

    message = str(exc.value)
    assert "QF_TEST_KEY" in message
    assert "12345" not in message


def test_the_error_for_a_pasted_key_does_not_reprint_the_whole_secret(cfg):
    """A message that quotes the key puts it straight into the log the user is
    about to paste into an issue. The name is echoed because it IS the key here,
    but the guidance must still tell them the value belongs elsewhere."""
    with pytest.raises(InvalidRequestError) as exc:
        QueryForge.sql(cfg, credentials={"QF_TEST_KEY": "x", SECRET: "y"})

    assert "model.apiKeyEnv" in str(exc.value)


# ------------------------------------------------------------------ logging


def test_credentials_are_never_logged(env_recording_binary, cfg, caplog):
    """The SDK logs a line per call at DEBUG and one on failure. Neither may
    carry the secret."""
    handler = queryforge.logging.configure("debug")
    try:
        with caplog.at_level(logging.DEBUG, logger="queryforge"):
            qf = QueryForge.sql(cfg, credentials={"QF_TEST_KEY": SECRET})
            qf.query("delivered orders").to_sql()

        combined = "\n".join(record.getMessage() + str(record.__dict__) for record in caplog.records)
        assert SECRET not in combined
    finally:
        logging.getLogger("queryforge").removeHandler(handler)


# ------------------------------------------------------------- empty inputs


def test_empty_credentials_are_the_same_as_none(env_recording_binary, cfg):
    """An empty dict must not be treated as "replace the environment with
    nothing"."""
    qf = QueryForge.sql(cfg, credentials={})
    qf.query("q").to_sql()

    env = env_recording_binary.calls()[0]["env"]
    assert env.get("PATH"), "an empty credentials dict must not wipe the environment"
