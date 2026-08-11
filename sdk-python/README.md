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
that block's `apiKeyEnv` gives.

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
| whatever your config's `apiKeyEnv` names | The model API key. Never put the key in the config file. |

Check an installation without needing a config or a key:

```python
import queryforge
print(queryforge.engine_version())   # {'success': True, 'protocol': '1.0', ...}
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
