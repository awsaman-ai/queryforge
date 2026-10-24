"""``model.apiKey``: the key written into the config itself.

The engine owns the behaviour; these tests prove the SDK carries it faithfully
and never becomes a second place the secret can leak from. They run the REAL
engine binary against a local fake model endpoint, so the whole path is covered:
config dict -> SDK -> engine subprocess -> HTTP request.
"""

from __future__ import annotations

import json
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

from queryforge import InvalidConfigError, QueryForge

pytestmark = pytest.mark.usefixtures("real_binary")

KEY = "AIzaSyLEAKCANARY-0123456789abcdef"

AST = {
    "version": "1.0",
    "entity": "Order",
    "filter": {
        "type": "comparison",
        "field": "status",
        "operator": "equals",
        "value": {"kind": "enum", "v": "DELIVERED"},
    },
}


class _Endpoint:
    """A throwaway OpenAI-compatible server that records the Authorization header."""

    def __init__(self, status: int = 200):
        outer = self
        outer.auth: list[str | None] = []
        outer.status = status

        class H(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802
                self.rfile.read(int(self.headers.get("Content-Length", 0)))
                outer.auth.append(self.headers.get("Authorization"))
                if outer.status == 200:
                    body = json.dumps({"choices": [{"message": {"content": json.dumps(AST)}}]})
                else:
                    # A badly behaved gateway: echo the credential back in the error.
                    body = "invalid credential " + (self.headers.get("Authorization") or "")
                self.send_response(outer.status)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(body.encode())

            def log_message(self, *a):  # silence
                pass

        self.server = HTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()


@pytest.fixture
def endpoint():
    ep = _Endpoint()
    yield ep
    ep.close()


def _cfg(config: dict, url: str, **model) -> dict:
    cfg = json.loads(json.dumps(config))
    cfg["model"] = {"provider": "custom", "baseURL": url, "model": "m", **model}
    return cfg


def test_inline_key_reaches_the_model_endpoint(config, endpoint, monkeypatch):
    monkeypatch.delenv("QF_API_KEY", raising=False)
    sql = QueryForge.postgres(_cfg(config, endpoint.url, apiKey=KEY)).query("delivered orders").to_sql()
    assert "status" in sql
    assert endpoint.auth == [f"Bearer {KEY}"]


def test_pasted_whitespace_around_the_key_is_trimmed(config, endpoint):
    QueryForge.postgres(_cfg(config, endpoint.url, apiKey=f"  {KEY}\n")).query("delivered orders").to_sql()
    assert endpoint.auth == [f"Bearer {KEY}"]


def test_env_var_route_still_works(config, endpoint, monkeypatch):
    monkeypatch.setenv("QF_INLINE_TEST_KEY", "env-key-123")
    QueryForge.postgres(_cfg(config, endpoint.url, apiKeyEnv="QF_INLINE_TEST_KEY")).query("delivered orders").to_sql()
    assert endpoint.auth == ["Bearer env-key-123"]


@pytest.mark.parametrize(
    "model",
    [
        {"apiKey": KEY, "apiKeyEnv": "QF_API_KEY"},  # both
        {"apiKey": "AIza has a space"},
        {"apiKey": "AIzaabc\ndef"},
        {"apiKey": "   "},
        {"apiKey": "x" * 5000},
    ],
)
def test_bad_inline_keys_are_rejected_without_echoing_the_key(config, model):
    cfg = _cfg(config, "http://127.0.0.1:1", **model)
    with pytest.raises(InvalidConfigError) as exc:
        QueryForge.postgres(cfg).generate(AST)
    text = str(exc.value) + repr(exc.value)
    for fragment in (KEY, "AIza has", "AIzaabc", "LEAKCANARY"):
        assert fragment not in text


def test_a_rejected_key_is_not_leaked_by_the_error(config, caplog):
    ep = _Endpoint(status=401)
    try:
        with caplog.at_level("DEBUG"):
            with pytest.raises(Exception) as exc:
                QueryForge.postgres(_cfg(config, ep.url, apiKey=KEY)).query("delivered orders").to_sql()
    finally:
        ep.close()
    everything = str(exc.value) + repr(exc.value) + caplog.text
    assert KEY not in everything and "LEAKCANARY" not in everything


def test_inline_key_in_a_fallback_entry(config, endpoint):
    cfg = _cfg(config, "http://127.0.0.1:1", apiKeyEnv="NOT_SET_ANYWHERE")
    cfg["model"]["timeoutSeconds"] = 2
    cfg["model"]["maxRetries"] = 0
    cfg["models"] = [{"provider": "custom", "baseURL": endpoint.url, "model": "m", "apiKey": KEY}]
    QueryForge.postgres(cfg).query("delivered orders").to_sql()
    assert endpoint.auth == [f"Bearer {KEY}"]


def test_the_key_is_not_in_the_sdk_objects_repr(config, endpoint):
    forge = QueryForge.postgres(_cfg(config, endpoint.url, apiKey=KEY))
    assert KEY not in repr(forge) and KEY not in str(forge)
