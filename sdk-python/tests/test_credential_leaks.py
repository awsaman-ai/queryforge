"""Nothing the SDK logs, raises or reprs may carry a credential.

Each scenario plants a canary as the key (inline, via the environment, or via
``credentials=``), makes a misbehaving model endpoint reflect it back in every
way a real gateway might, and then searches everything a caller could end up
writing to a log: the JSON log stream at DEBUG, every exception's str, repr and
full chained traceback, and the repr of the objects holding the config.
"""

from __future__ import annotations

import io
import json
import logging
import threading
import traceback
from http.server import BaseHTTPRequestHandler, HTTPServer

import pytest

import queryforge
from queryforge import QueryForge
from queryforge import logging as qflog

pytestmark = pytest.mark.usefixtures("real_binary")

KEY = "AIzaSyLEAKCANARY-0123456789abcdef"
ENV_KEY = "ENVCANARY-9876543210-abcdefghij"
CANARIES = ("LEAKCANARY", "ENVCANARY", KEY, ENV_KEY)

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


class _Reflector:
    """A model endpoint that reflects the credential it was sent, in the mode asked for."""

    def __init__(self, mode: str):
        outer = self
        outer.mode = mode

        class H(BaseHTTPRequestHandler):
            def do_POST(self):  # noqa: N802
                self.rfile.read(int(self.headers.get("Content-Length", 0)))
                echo = f"{self.headers.get('Authorization')} {self.headers.get('x-api-key')}"
                m = outer.mode
                if m == "401":
                    code, body = 401, "unauthorized: " + echo
                elif m == "500":
                    code, body = 500, "<html>" + echo + "</html>"
                elif m == "400-json":
                    code, body = 400, json.dumps({"error": {"message": "bad " + echo}})
                elif m == "reply":
                    code, body = 200, json.dumps({"choices": [{"message": {"content": "no. " + echo}}]})
                else:
                    code, body = 200, "garbage " + echo
                self.send_response(code)
                self.send_header("Content-Type", "application/json")
                self.end_headers()
                self.wfile.write(body.encode())

            def log_message(self, *a):
                pass

        self.server = HTTPServer(("127.0.0.1", 0), H)
        self.url = f"http://127.0.0.1:{self.server.server_port}"
        threading.Thread(target=self.server.serve_forever, daemon=True).start()

    def close(self):
        self.server.shutdown()


def _flatten(exc: BaseException) -> str:
    """str, repr and the whole chained traceback of an exception."""
    return f"{exc!s} {exc!r} " + "".join(traceback.format_exception(type(exc), exc, exc.__traceback__))


@pytest.mark.parametrize("mode", ["401", "500", "400-json", "reply", "garbage"])
@pytest.mark.parametrize("route", ["inline", "env", "credentials"])
def test_no_credential_reaches_logs_exceptions_or_reprs(config, monkeypatch, mode, route):
    ep = _Reflector(mode)
    stream = io.StringIO()
    handler = qflog.configure("debug", stream=stream)
    try:
        model = {"provider": "custom", "baseURL": ep.url, "model": "m", "maxRetries": 0}
        kwargs = {}
        if route == "inline":
            model["apiKey"] = KEY
        elif route == "env":
            model["apiKeyEnv"] = "QF_LEAKTEST_KEY"
            monkeypatch.setenv("QF_LEAKTEST_KEY", ENV_KEY)
        else:
            model["apiKeyEnv"] = "QF_LEAKTEST_KEY"
            kwargs["credentials"] = {"QF_LEAKTEST_KEY": ENV_KEY}
        cfg = json.loads(json.dumps(config))
        cfg["model"] = model

        forge = QueryForge.postgres(cfg, **kwargs)
        seen = [repr(forge), str(forge)]
        pending = forge.query("delivered orders")
        seen.append(repr(pending))
        try:
            result = pending.result()
            seen += [repr(result), str(result)]
        except queryforge.QueryForgeError as exc:
            seen.append(_flatten(exc))
    finally:
        logging.getLogger("queryforge").removeHandler(handler)
        ep.close()

    blob = "\n".join(seen) + "\n" + stream.getvalue()
    for canary in CANARIES:
        assert canary not in blob, f"{route}/{mode}: {canary!r} leaked:\n{blob[:1500]}"
    assert stream.getvalue(), "expected DEBUG log lines to have been captured"


@pytest.mark.parametrize(
    "bad_model",
    [
        {"apiKey": KEY, "apiKeyEnv": "QF_API_KEY"},
        {"apiKey": KEY + " two words"},
        {"apiKeyEnv": KEY},
        {"apiKey": {"k": KEY}},
    ],
)
def test_rejected_configs_never_echo_the_key(config, bad_model):
    cfg = json.loads(json.dumps(config))
    cfg["model"] = {"provider": "custom", "baseURL": "http://127.0.0.1:1", **bad_model}
    stream = io.StringIO()
    handler = qflog.configure("debug", stream=stream)
    try:
        with pytest.raises(queryforge.QueryForgeError) as exc:
            QueryForge.postgres(cfg).generate(AST)
    finally:
        logging.getLogger("queryforge").removeHandler(handler)
    blob = _flatten(exc.value) + stream.getvalue()
    assert KEY not in blob and "LEAKCANARY" not in blob, blob[:1200]
