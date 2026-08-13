# Logging and Error Handling

QueryForge reaches you through four surfaces — the Go library, the engine binary, the Python SDK
and the Java SDK. This document is the contract all four keep: the same field names, the same
severity meanings, the same error codes.

The invariant everything below serves:

> **If QueryForge cannot correctly complete an operation, it does not pretend that it did.**

There is no path through this library that returns an empty query, a partial result, or a `null`
in place of a failure. A failure is always an error or an exception, always carries a stable code,
and always keeps the thing that actually went wrong reachable as its cause.

---

## 1. The one design decision worth knowing

**The Go library does not log.** It never has, and this change did not alter that.

A library that logs on its own initiative fights its host: it picks a destination the host did not
choose, a format their aggregator may not parse, and a severity they do not agree with. So the
library reports facts through one optional callback — `Observer` — and the host decides everything
else. What is new is `qf.SlogObserver`, the adapter that turns those facts into structured
`log/slog` records with the field names below, so that every caller does not write their own
slightly different version.

The same principle holds in the SDKs, expressed idiomatically:

| Surface | Mechanism | Default |
|---|---|---|
| Go library | `Observer` callback; `qf.SlogObserver(logger)` adapts it to `log/slog` | silent (nil Observer) |
| Engine binary | JSON on **stderr**; `--log-level`, `QUERYFORGE_LOG_LEVEL`, or `options.logLevel` | **off** |
| Python SDK | `logging.getLogger("queryforge")`, `NullHandler` attached | silent until you set a level |
| Java SDK | `java.util.logging` under `io.queryforge`, no handler installed | floor of `WARNING` |

None of them calls `basicConfig`, installs a root handler, or reads a logging config file.

### Why `java.util.logging` and not SLF4J

The Java SDK has **no runtime dependencies**, deliberately: a thin wrapper that drags in a logging
facade forces that facade's version on every application using it. JUL is in the JDK, so it costs
nothing — the same choice the JDK's own `HttpClient` makes. Hosts on Logback or Log4j bridge it in
one line:

```java
// SLF4J / Logback
SLF4JBridgeHandler.removeHandlersForRootLogger();
SLF4JBridgeHandler.install();          // needs org.slf4j:jul-to-slf4j

// Log4j 2
-Djava.util.logging.manager=org.apache.logging.log4j.jul.LogManager
```

The structured fields survive the bridge: they are attached to each `LogRecord` as its parameters,
so a handler reads a `Map` rather than parsing a sentence.

---

## 2. Turning it on

```go
// Go — the library
logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
engine := qf.New(cfg)
engine.SetObserver(qf.SlogObserver(logger))   // SetObserver, not Observe — see below
```

`SetObserver` rather than assigning `Engine.Observe`: only the former pushes the observer down into
the provider, where latency and token counts live. Assigning the field reaches the engine's own
events and silently drops the expensive half.

```bash
# The engine binary
queryforge --log-level=debug < request.json
QUERYFORGE_LOG_LEVEL=info queryforge < request.json
```

```python
# Python
import logging
logging.getLogger("queryforge").setLevel(logging.INFO)

# or, for JSON matching the engine's own fields:
import queryforge
queryforge.logging.configure("info")
```

```java
// Java
Logger.getLogger(QueryForgeLogging.LOGGER_NAME).setLevel(Level.FINE);

// or, for JSON matching the engine's own fields:
QueryForgeLogging.configure(Level.INFO);
```

`QUERYFORGE_LOG_LEVEL` is honoured by all four surfaces, so one variable lights up a whole call.

### Engine logs from an SDK

When you configure an SDK's logger, the SDK asks the engine subprocess for logs at the same level
and passes a correlation id, so one `request_id=…` search returns both halves of a query.

It does that **only once you have configured logging**. An unconfigured SDK sends a request that is
byte-identical to a protocol-1.0 one, because the engine rejects unknown request fields — an SDK
that always sent `logLevel` would turn every call into `INVALID_REQUEST` for anyone pointing
`QUERYFORGE_BINARY` at an older engine. If you enable logging against a pre-1.1 engine you will get
that error, naming the field; upgrade the engine or leave logging off.

Correlate with your own trace id:

```python
qf.query("delivered orders").request_id(trace_id).to_sql()
```
```java
forge.query("delivered orders").requestId(traceId).toSql();
```

---

## 3. Severity

Identical meanings in all four surfaces. Java's JUL names differ; the mapping is in the table.

| Level | JUL | Means |
|---|---|---|
| `DEBUG` | `FINE` | A step that succeeded, or detail for tracing: binary resolution, a request being sent, a model call that answered, an attempt that produced a valid AST. |
| `INFO` | `INFO` | A lifecycle fact: an operation completed, or was **refused**. |
| `WARN` | `WARNING` | A step failed but the operation continues: one model call in a fallback chain, one attempt inside the repair budget. |
| `ERROR` | `SEVERE` | The caller is receiving an error. **Exactly one per failed operation.** |

Two of these are deliberate and worth stating:

**A refusal is `INFO`, not `ERROR`.** When the model declines because the question cannot be
expressed in the vocabulary the config registers, that is the guard rail working — no query was
invented. Reporting it at `ERROR` is how an error stream becomes something people filter out.

**A failed operation produces exactly one `ERROR`.** A translation that burns three repair attempts
and then fails emits three `WARN` lines and one `ERROR`, not four stack traces of the same
underlying problem. The stack trace appears once, at the boundary, which is the only layer that
knows the whole story.

---

## 4. Fields

Every record carries `library`, `language` and (outside the Go library) `version`. The rest appear
when they are known; a field whose value is unknown is **omitted**, never logged as `null`, so a
record's key set says what was actually established.

| Field | Meaning |
|---|---|
| `library` | Always `queryforge`. |
| `language` | `go` \| `python` \| `java`. |
| `version` | The SDK or engine version that produced the line. |
| `component` | `engine` \| `provider` \| `transport` \| `binary` \| `cli`. |
| `operation` | `translate` \| `generate` \| `validate` \| `version` \| `attempt` \| `model_call` \| `resolve_binary`. |
| `outcome` | `ok` \| `refusal` \| `error`, or the finer `Outcome` vocabulary on library events. |
| `request_id` | Correlation id, shared between an SDK and the engine it spawned. |
| `backend` | `sql` \| `mysql` \| `mongo`. |
| `entity` | The config's entity, e.g. `Order`. |
| `duration_ms` | Wall time of the step. |
| `attempt` | 0-based repair attempt. |
| `repair_attempts` | Final repair count for a translation. |
| `error_code` | The stable failure code — §5. Same string the SDKs put on the exception. |
| `error_type` | The concrete error or exception class. |
| `scope_keys` | The **names** of the caller-imposed filters. Never their values. |
| `warnings` | Generator advisories, e.g. a filter on a non-indexed field. |
| `provider`, `model` | Which model answered (model-call records). |
| `prompt_tokens`, `completion_tokens`, `hidden_tokens`, `total_tokens` | Token usage. `hidden_tokens` is the reasoning-token bill, invisible in the visible answer and measured at up to ~5× the completion count. |
| `finish_reason` | `stop`, `length`, … — the tell that caught a real bug once. |
| `config_fields` | How many fields the config registers. Shape, not content. |
| `raw` | The model's verbatim reply, on a **failed** attempt at **DEBUG only**. See §6. |

Example (engine binary, `--log-level=debug`):

```json
{"time":"2026-08-13T01:30:00.412Z","level":"ERROR","msg":"request failed",
 "library":"queryforge","language":"go","version":"1.1.2","component":"cli",
 "operation":"translate","request_id":"8f3a1c2d4e5f","backend":"mysql","entity":"Order",
 "config_fields":5,"duration_ms":1842,"outcome":"error","error_code":"MODEL_TRANSPORT",
 "error":"planner: model transport failure: dial tcp: connection refused"}
```

---

## 5. Error codes

One vocabulary, shared by every surface. In Go it is `qf.FailureCode`, produced by `qf.Classify`;
on the wire it is the `code` field; in Python it is `QueryForgeError.code`; in Java it is
`QueryForgeException.getCode()`. Each maps to a distinct exception class in the SDKs.

| Code | Means | Whose problem | Retry? |
|---|---|---|---|
| `INVALID_REQUEST` | Malformed call, missing argument, empty question. | Caller's code | No |
| `INVALID_CONFIG` | The config did not parse or broke a structural rule. | Config author | No |
| `UNKNOWN_BACKEND` | No generator for that backend id. | Caller's code | No |
| `INVALID_SCOPE` | A caller-imposed filter was rejected. Scope comes from the session, never the question — this is always an application bug. | Application | No |
| `VALIDATION_FAILED` | An AST broke a rule the config declares. On a translation: the model could not produce a conforming AST within the repair budget, usually a config gap. | Config author | No |
| `UNSUPPORTED_REQUEST` | The model refused: the question cannot be expressed in this vocabulary. A well-formed answer — the message is written to be shown to the person who asked. | End user | No |
| `MODEL_OUTPUT` | The model answered, but never with usable JSON. | Transient | **Yes** |
| `MODEL_TRANSPORT` | The model was never reached: network, missing or rejected key, rate limit. | Transient / config | **Yes** |
| `GENERATE_FAILED` | A valid AST could not be compiled to the target backend. | QueryForge | No |
| `TIMEOUT` | The deadline expired. | Caller's timeout | Maybe |
| `INTERNAL` | QueryForge produced an error it has no name for. A bug worth reporting. | QueryForge | No |
| `BINARY_NOT_FOUND` | *SDK only.* No engine executable for this platform. | Installation | No |
| `PROTOCOL_ERROR` | *SDK only.* The engine crashed, wrote non-JSON, or speaks an incompatible protocol. | Installation | No |

`qf.FailureCode.Retryable()` answers the last column, so you do not have to keep your own copy of a
table that goes stale.

A code this SDK version has never heard of degrades to the base exception class with the code
intact, rather than crashing — an SDK talking to a newer engine must degrade, not fail.

### Classification order is load-bearing

`Classify` checks in a specific order because several failures match more than one branch:

1. **Deadline first.** A cancelled model call surfaces as a transport failure — technically true,
   but "the model was unreachable" sends someone to check their API key when the fix is a longer
   timeout.
2. **Scope before validation.** A bad scope should page whoever wired up the tenancy filters; a
   validation failure is ordinary feedback about a question.
3. **Refusal before validation.** A refusal is a deliberate answer, not a config gap.
4. **Validation before the model sentinels.** The budget-exhausted error wraps the last underlying
   failure, and both would otherwise match.

---

## 6. What is never logged

Not by default, not at `DEBUG`, not on the failure path. Each of these is pinned by a canary test
in every language: a distinctive string is planted where a naive implementation would echo it, and
the test asserts it never appears in any record.

- **The natural-language question.** User data. It may hold a name, an account number, a medical
  detail.
- **Scope values.** Tenant, user, subscription and enterprise ids — the most sensitive fields in
  the system. Only `scope_keys`, the names, are logged, because an audit trail has to be able to
  say which filters were forced onto a query.
- **The config.** Physical table and column names, which plenty of organisations treat as
  confidential. Its *shape* is logged instead: `entity` and `config_fields`.
- **The API key.** Read from the environment, passed in an HTTP header, never placed on an event.
- **The compiled query and its bound arguments.**

Two controlled exceptions:

**`raw`** — the model's verbatim reply, on a failed attempt only. It is the single most useful
field for diagnosing a parse failure (without it you know *that* the reply was unusable but never
*what* it said) and the only one that can echo caller data. It is bounded to
`Engine.MaxRawLength` (4 KB default) inside the library and attached at `DEBUG` only.

**Engine stderr** — quoted into a `ProtocolError` / `ProtocolException` when the subprocess dies
before answering, because it is the only evidence there is. It is scrubbed first: bearer tokens,
`api_key=`/`password=`/`token=` forms, credentials embedded in URLs, and the recognisable `sk-…`
and `AIza…` shapes are replaced with `[REDACTED]`, and the excerpt is bounded to 500 bytes with an
explicit `(N bytes truncated)` marker. Silent truncation would make a prefix look like the whole
reply.

A caller-supplied `request_id` is sanitized before it is logged: it is caller-controlled text going
straight into a log sink, so anything outside `[A-Za-z0-9._:/-]` is dropped and the length is
bounded. A newline in a correlation id would otherwise let a caller forge a second log record.

---

## 7. Failure behaviour, before and after

```
Java — an unreadable config file

  before:  IOException caught → InvalidConfigException("could not read …")
           → cause DROPPED → no way to tell a wrong path from wrong permissions
  after:   IOException caught → ERROR log → InvalidConfigException(…, cause)
           → caller gets the code, the message and getCause()

Java — a truncated read from the engine's stdout

  before:  join() times out or is interrupted → the PARTIAL buffer is returned
           → parsed as JSON → "produced output that is not JSON"
           → sends someone to debug the engine's encoder
  after:   the incomplete drain is detected → ProtocolException naming it,
           partial output discarded rather than parsed

Java — the request could not be written to the subprocess

  before:  IOException swallowed, process killed, symptom reported alone
  after:   the write failure is kept and attached as the cause of the
           "produced no response" error

Every surface — any operation that cannot complete

  before/after (unchanged, and now pinned by test):
           failure → structured ERROR at the boundary → typed error carrying a
           stable code and the original cause → propagated to the caller.
           Never an empty query, never a partial result, never a fake success.
```

---

## 8. Testing your own handling

Assert on **fields**, never on a rendered line and never on a timestamp:

```python
assert record.queryforge["error_code"] == "MODEL_TRANSPORT"
assert record.levelno == logging.ERROR
```
```java
Map<String, Object> fields = QueryForgeLogging.fieldsOf(record);
assertEquals("MODEL_TRANSPORT", fields.get("error_code"));
```
```go
// Feed a slog.JSONHandler into a buffer and decode the lines.
if rec["error_code"] != string(qf.FailureModelTransport) { … }
```

A test that string-matches a log line fails the moment someone reorders two attributes, which is
how a team learns to distrust logging tests and delete them.
