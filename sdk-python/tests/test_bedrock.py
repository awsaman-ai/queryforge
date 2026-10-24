"""Amazon Bedrock through the real engine binary.

The SDK holds no Bedrock logic: it forwards the config's ``model.aws`` block as
opaque JSON and carries ``AWS_*`` variables to the engine through the
``credentials`` map. These tests pin that contract end to end against a fake
Bedrock endpoint, using a binary built from THIS checkout (the bundled one may
predate the feature).

They cover the two ways a credential can be supplied — by reference to an
environment variable, and written inline — and the guarantees that make the
second one tolerable: the value never reaches the request body, and the caller
is warned.
"""

from __future__ import annotations

import json
import shutil
import subprocess
import threading
from http.server import BaseHTTPRequestHandler, HTTPServer
from pathlib import Path

import pytest

from queryforge import QueryForge

REPO_ROOT = Path(__file__).resolve().parents[2]
INLINE_SECRET = "ABSKinline-bearer-secret-value-123"
ENV_SECRET = "bearer-from-the-credentials-map-456"

# The AST a model would return for "delivered orders".
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


@pytest.fixture(scope="module")
def fresh_binary(tmp_path_factory) -> Path:
    if shutil.which("go") is None:
        pytest.skip("needs a Go toolchain to build the engine from this checkout")
    out = tmp_path_factory.mktemp("bin") / "queryforge"
    subprocess.run(["go", "build", "-o", str(out), "./cmd/queryforge"], cwd=REPO_ROOT, check=True)
    return out


@pytest.fixture
def fake_bedrock():
    """A Bedrock Converse endpoint that records what it was sent."""
    seen: list[dict] = []

    class Handler(BaseHTTPRequestHandler):
        def do_POST(self):  # noqa: N802 - http.server's naming
            body = self.rfile.read(int(self.headers.get("Content-Length", 0)))
            seen.append({"path": self.path, "auth": self.headers.get("Authorization"), "body": body.decode()})
            reply = json.dumps(
                {
                    "output": {"message": {"content": [{"text": json.dumps(AST)}]}},
                    "stopReason": "end_turn",
                    "usage": {"inputTokens": 3, "outputTokens": 4},
                }
            ).encode()
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(reply)))
            self.end_headers()
            self.wfile.write(reply)

        def log_message(self, *args):  # silence
            pass

    server = HTTPServer(("127.0.0.1", 0), Handler)
    threading.Thread(target=server.serve_forever, daemon=True).start()
    yield f"http://127.0.0.1:{server.server_port}", seen
    server.shutdown()


def _config(base_url: str, aws: dict) -> dict:
    return {
        "entity": "Order",
        "model": {
            "provider": "bedrock",
            "model": "us.anthropic.claude-sonnet-4-6",
            "baseURL": base_url,
            "aws": {"region": "us-east-1", **aws},
        },
        "backends": {"sql": {"table": "orders"}},
        "fields": [
            {"name": "status", "type": "enum", "values": ["NEW", "DELIVERED"], "operators": ["equals"]},
        ],
    }


def test_credential_by_reference_arrives_through_the_credentials_map(fresh_binary, fake_bedrock, monkeypatch):
    base_url, seen = fake_bedrock
    monkeypatch.setenv("QUERYFORGE_BINARY", str(fresh_binary))
    cfg = _config(base_url, {"bearerTokenEnv": "AWS_BEARER_TOKEN_BEDROCK"})

    qf = QueryForge.sql(cfg, credentials={"AWS_BEARER_TOKEN_BEDROCK": ENV_SECRET})
    pending = qf.query("delivered orders")

    assert "status" in pending.to_sql()
    assert seen[0]["auth"] == f"Bearer {ENV_SECRET}"
    assert seen[0]["path"] == "/model/us.anthropic.claude-sonnet-4-6/converse"
    # By reference means the config itself holds no secret, hence no warning.
    assert not any("inline" in w for w in pending.warnings())
    assert ENV_SECRET not in json.dumps(cfg)


def test_the_standard_variable_works_with_no_aws_block_credentials(fresh_binary, fake_bedrock, monkeypatch):
    """The zero-config path: only the model and the standard AWS variable."""
    base_url, seen = fake_bedrock
    monkeypatch.setenv("QUERYFORGE_BINARY", str(fresh_binary))
    cfg = _config(base_url, {})

    QueryForge.sql(cfg, credentials={"AWS_BEARER_TOKEN_BEDROCK": ENV_SECRET}).query("delivered orders").to_sql()

    assert seen[0]["auth"] == f"Bearer {ENV_SECRET}"


def test_an_inline_credential_works_and_raises_a_warning(fresh_binary, fake_bedrock, monkeypatch):
    base_url, seen = fake_bedrock
    monkeypatch.setenv("QUERYFORGE_BINARY", str(fresh_binary))
    cfg = _config(base_url, {"bearerToken": INLINE_SECRET})

    pending = QueryForge.sql(cfg).query("delivered orders")
    pending.to_sql()

    assert seen[0]["auth"] == f"Bearer {INLINE_SECRET}"
    warnings = " ".join(pending.warnings())
    assert "aws.bearerToken" in warnings
    assert INLINE_SECRET not in warnings, "the warning must name the field, never the value"
    assert INLINE_SECRET not in seen[0]["body"], "the credential must not be sent in the request body"


def test_sigv4_keys_sign_the_request(fresh_binary, fake_bedrock, monkeypatch):
    base_url, seen = fake_bedrock
    monkeypatch.setenv("QUERYFORGE_BINARY", str(fresh_binary))
    cfg = _config(
        base_url,
        {"accessKeyIdEnv": "AWS_ACCESS_KEY_ID", "secretAccessKeyEnv": "AWS_SECRET_ACCESS_KEY", "sessionTokenEnv": "AWS_SESSION_TOKEN"},
    )

    QueryForge.sql(
        cfg,
        credentials={
            "AWS_ACCESS_KEY_ID": "AKIDEXAMPLE",
            "AWS_SECRET_ACCESS_KEY": "wJalrXUtnFEMI/K7MDENG+bPxRfiCYEXAMPLEKEY",
            "AWS_SESSION_TOKEN": "temporary-session-token",
        },
    ).query("delivered orders").to_sql()

    auth = seen[0]["auth"]
    assert auth.startswith("AWS4-HMAC-SHA256 Credential=AKIDEXAMPLE/")
    assert "/us-east-1/bedrock/aws4_request" in auth
    assert "x-amz-security-token" in auth
    assert "wJalrXUtnFEMI" not in auth


def test_a_config_naming_two_auth_modes_is_rejected_at_load(fresh_binary, fake_bedrock, monkeypatch):
    from queryforge import InvalidConfigError

    base_url, _ = fake_bedrock
    monkeypatch.setenv("QUERYFORGE_BINARY", str(fresh_binary))
    cfg = _config(base_url, {"bearerTokenEnv": "X", "accessKeyId": "AKIDEXAMPLE", "secretAccessKey": "s"})

    with pytest.raises(InvalidConfigError) as exc:
        QueryForge.sql(cfg).generate(AST)
    assert "one authentication mode" in str(exc.value)
