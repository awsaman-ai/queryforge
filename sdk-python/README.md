# QueryForge for Python

Turn a sentence into a parameterized database query, with a validated AST in between.

```bash
pip install queryforge-ai
```

The package installs as `queryforge-ai` and imports as `queryforge`.

```python
from queryforge import QueryForge

qf = QueryForge.mysql("orders.config.json")
sql = qf.query("delivered orders over $100 last month").to_sql()
```

No server to run, no Go toolchain to install, no runtime dependencies. The engine ships as a
native binary inside the wheel; this package spawns it as a local subprocess and turns its reply
into Python objects.

---

## How it fits together

QueryForge's engine is written in Go. Everything that decides what a query *means* — the parser,
the AST, validation, the dialects, SQL and Mongo generation — lives there and nowhere else. This
SDK is a wrapper: it validates your arguments, finds the right binary for your platform, sends one
JSON request, and maps the reply onto Python types.

That is what makes the guarantee worth having: this SDK, the Java one, and the engine's own Go API
produce **byte-identical output** for the same input, because there is only one implementation.

---

## The two halves of the API

### `query(text)` — the full pipeline

Costs one model call. Needs a `model` block in your config and the API key exported under the name
that block's `apiKeyEnv` gives — or passed in directly, see
[Supplying the key at runtime](#supplying-the-key-at-runtime).

```python
from queryforge import QueryForge

qf = QueryForge.postgres("orders.config.json")
pending = qf.query("cancelled orders from ACME this week")

sql = pending.to_sql()        # 'SELECT ... WHERE (status = $1 AND ...)'
args = pending.to_args()      # ('CANCELLED', ...)
prose = pending.explain()     # 'Return all fields from Order where ...'
```

Nothing runs until a terminal method is called, and the answer is cached — reading `to_sql()` and
then `explain()` off the same object costs **one** model call, not two.

### `generate(ast)` / `validate(ast)` — deterministic

No model call, no network, no API key. Use these to re-compile a stored AST for a second backend,
and in your tests.

```python
result = qf.generate(stored_ast)
mongo_doc = QueryForge.mongo(config).generate(stored_ast).doc
```

---

## Executing the query

Values are **never inlined** into the statement — that is the injection guarantee the library rests
on. Pass the args alongside:

```python
cursor.execute(pending.to_sql(), pending.to_args())
```

For MongoDB:

```python
doc = QueryForge.mongo(config).query("open orders").to_mongo()
cursor = db[doc["collection"]].find(doc["filter"], doc.get("projection"))
```

---

## Scope: filters your application imposes

Subscription, tenant, user, enterprise ids — values that come from the session, not from the
question. They are AND-ed onto the query *after* validation, so they can only narrow the result,
and the model is never told they exist.

```python
qf = QueryForge.postgres(config, scope={"tenantId": request.tenant_id})
sql = qf.query(user_question).to_sql()          # every query is scoped

# or per-query, merging with any scope already set
sql = qf.query(user_question).scope({"ownerId": user.id}).to_sql()
```

The applied filters come back on the result so an audit log can record exactly what was forced
onto the query:

```python
for f in pending.result().scope:
    print(f.field_name, f.operator, f.value, f.declared)
```

---

## Errors

Every failure maps to a distinct exception, so you can branch on the one that matters:

```python
from queryforge import (
    QueryForgeError,          # base class — catch this if you do not care why
    UnsupportedRequestError,  # the model declined; show the message to the user
    ValidationError,          # the AST broke a config rule; .details says which
    ModelTransportError,      # the model was unreachable; check the API key
    ModelOutputError,         # the model returned junk; retrying is reasonable
    InvalidScopeError,        # your scope map is wrong — an application bug
    TimeoutError,
)

try:
    sql = qf.query(question).to_sql()
except UnsupportedRequestError as e:
    return {"error": str(e)}          # written to be shown to the asker
except ValidationError as e:
    for d in e.details:
        print(d.code, d.field, d.suggestions)   # 'unknown_field' 'amont' ['amount']
except QueryForgeError as e:
    log.exception("query failed: %s", e.code)
```

| Exception | Code | What to do |
|---|---|---|
| `InvalidRequestError` | `INVALID_REQUEST`, `UNKNOWN_OP` | Fix the calling code |
| `InvalidConfigError` | `INVALID_CONFIG` | Fix the config file |
| `UnknownBackendError` | `UNKNOWN_BACKEND` | Use `sql`, `mysql` or `mongo` |
| `InvalidScopeError` | `INVALID_SCOPE` | Fix the scope map — an application bug |
| `ValidationError` | `VALIDATION_FAILED` | Register the field, or rephrase |
| `UnsupportedRequestError` | `UNSUPPORTED_REQUEST` | Show the message; the question needs rephrasing |
| `ModelOutputError` | `MODEL_OUTPUT` | Retry, or switch models |
| `ModelTransportError` | `MODEL_TRANSPORT` | Check the API key and the endpoint |
| `GenerateError` | `GENERATE_FAILED` | The AST is legal but not compilable for this backend |
| `TimeoutError` | `TIMEOUT` | Raise the timeout |
| `BinaryNotFoundError` | `BINARY_NOT_FOUND` | Reinstall, or set `QUERYFORGE_BINARY` |
| `ProtocolError` | `PROTOCOL_ERROR` | Broken install — the binary crashed or is the wrong version |

QueryForge **never fails silently**. There is no path that returns an empty query, a partial
result or `None` in place of a failure, and every wrapped error keeps the original as its
`__cause__`:

```python
except ProtocolError as e:
    e.__cause__          # the JSONDecodeError, OSError or TimeoutExpired underneath
```

---

## Logging

Diagnostics go through the standard `logging` module under the `queryforge` namespace. The SDK
attaches a `NullHandler` and nothing else — it never calls `basicConfig`, never touches the root
logger, and is silent until you say otherwise.

```python
import logging
logging.getLogger("queryforge").setLevel(logging.INFO)

# or, for JSON matching the engine's own field names:
import queryforge
queryforge.logging.configure("info")

# or, without a code change:
#   QUERYFORGE_LOG_LEVEL=debug python app.py
```

Records carry structured fields as attributes, and all of them together as `record.queryforge`:

```json
{"level":"ERROR","msg":"engine request failed","library":"queryforge","language":"python",
 "operation":"translate","request_id":"8f3a1c2d4e5f","backend":"mysql","entity":"Order",
 "duration_ms":1842,"outcome":"error","error_code":"MODEL_TRANSPORT",
 "error_type":"ModelTransportError"}
```

Levels mean the same thing here as in the engine and the Java SDK. `DEBUG` is detail while
tracing; `INFO` is an operation completing — **including a refusal**, which means the guard rails
worked; `WARNING` is a step that failed while the operation continues; `ERROR` is the caller
receiving an exception, and there is **exactly one per failed call**.

Once you configure the logger, the SDK asks the engine subprocess for logs at the same level and
passes a correlation id, so one `request_id` search returns both halves of a query. Supply your
own trace id with `.request_id(...)`:

```python
qf.query("delivered orders").request_id(trace_id).to_sql()
```

**The question text, the scope values and the config contents are never logged**, at any level.
Only shape is: the entity, the backend, the scope *keys*. Engine stderr quoted into an exception is
scrubbed of credentials and bounded first. See
[docs/OBSERVABILITY.md](https://github.com/awsaman-ai/queryforge/blob/main/docs/OBSERVABILITY.md)
for the full field schema.

---

## Configuration

Any of these works:

```python
QueryForge.mysql({"entity": "Order", ...})       # a dict
QueryForge.mysql("orders.config.json")           # a path
QueryForge.mysql(Path("configs/orders.json"))    # a Path
QueryForge.mysql('{"entity": "Order", ...}')     # JSON text (anything starting with '{')
```

Options, all optional:

```python
QueryForge.mysql(
    config,
    scope={"tenantId": "t1"},   # applied to every query
    timeout=30,                 # seconds, model call included
    max_repairs=2,              # validation-repair retries; 0 = one attempt
)

qf.query(text).timeout(10).max_repairs(0).include_raw().scope_in_ast()
```

---

## Backends

| Factory | Engine id | Produces |
|---|---|---|
| `QueryForge.postgres(cfg)` / `QueryForge.sql(cfg)` | `sql` | PostgreSQL, `$1` placeholders |
| `QueryForge.mysql(cfg)` | `mysql` | MySQL, `?` placeholders |
| `QueryForge.mongo(cfg)` | `mongo` | A query document |

---

## Environment

| Variable | Effect |
|---|---|
| `QUERYFORGE_BINARY` | Run this executable instead of the bundled one. Reported, never silently ignored, if it does not work. |
| `QUERYFORGE_LOG_LEVEL` | `off` \| `error` \| `warn` \| `info` \| `debug`. Turns on SDK and engine diagnostics without a code change. Unset means off. |
| whatever your config's `apiKeyEnv` names | The model API key. Never put the key in the config file. |

### Supplying the key at runtime

Exporting the variable before the interpreter starts is not always possible — the key may live in a
secret manager, a vault client, or a settings object, and may rotate while the process runs. Pass it
directly instead:

```python
qf = QueryForge.mysql(
    "orders.config.json",
    credentials={"QF_API_KEY": vault.get("openai/key")},
)
```

The dict key is a variable **name** — whatever your config's `model.apiKeyEnv` refers to — and the
value is the secret. What happens to it:

* It is placed in the engine subprocess's environment, and nowhere else. It never enters the request
  body (the one structure here that gets JSON-encoded, dumped on protocol errors, and pasted into
  bug reports), and it is never logged.
* `os.environ` is **not** modified, so two `QueryForge` instances holding different keys do not
  interfere — which is what makes this usable from a multi-tenant service.
* The subprocess still inherits the rest of your environment, so `PATH` and friends survive.
* Only `query()` receives it. `generate()` and `validate()` make no model call, so the engine they
  spawn never sees the key.

A name that is not a legal environment variable — most often because the **key** was pasted where the
**name** belongs — is rejected when you construct the instance, not on the first query.

Check an installation without needing a config or a key:

```python
import queryforge
print(queryforge.engine_version())   # {'success': True, 'protocol': '1.1', ...}
print(queryforge.binary_path())
print(queryforge.platform_tag())     # 'darwin-arm64'
```

---

## Platform support

Wheels are published per platform, each carrying only the binary that platform can run (~6 MB
rather than ~75 MB):

| Platform | Wheel tag |
|---|---|
| Linux x86-64 | `manylinux2014_x86_64` · `musllinux_1_1_x86_64` |
| Linux ARM64 | `manylinux2014_aarch64` · `musllinux_1_1_aarch64` |
| macOS Intel | `macosx_10_9_x86_64` |
| macOS Apple Silicon | `macosx_11_0_arm64` |
| Windows x86-64 | `win_amd64` |

The engine is statically linked with `CGO_ENABLED=0`, so it runs on Alpine as readily as on glibc
distributions — hence both tags on Linux.

---

## Development

```bash
go build -o sdk-python/queryforge/bin/queryforge ./cmd/queryforge   # from the repo root
cd sdk-python && python -m pytest tests/ -q
```

The suite splits into `test_integration.py`, which runs against the real engine binary using only
its offline ops, and `test_sdk.py`, which runs against a scripted fake to cover every error code,
crashes, corrupted output and protocol mismatches.

## License

Apache License 2.0 — see [LICENSE](../LICENSE).
