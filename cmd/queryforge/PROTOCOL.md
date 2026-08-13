# The QueryForge stdio protocol

This is the contract between the QueryForge engine and every language SDK. Read it if you are
writing a new SDK, debugging one, or driving the engine directly from a language that has none.

**Protocol version: 1.1**

> **1.1** added the two optional observability fields on `options`: `logLevel` and `requestId`.
> Note the asymmetry a MINOR bump has here. A *new* engine reading an *old* request is fine — the
> fields are optional. An *old* engine reading a *new* request is **not**: requests are decoded
> with unknown fields rejected, so an unknown `logLevel` is refused outright. That is the correct
> behaviour for a field-drop hazard, and it is why both SDKs send neither field unless the host has
> explicitly turned logging on — the default request stays byte-identical to a 1.0 one.

---

## The shape of it

The engine is **not a server**. There is no HTTP, no port, no daemon, no listening socket. An SDK
spawns `queryforge` as a subprocess, writes one JSON object to its stdin, reads one JSON object
from its stdout, and the process exits.

```
echo '{"op":"version"}' | queryforge
```

That keeps the security surface at "a local subprocess with the caller's own privileges", and
means a Java or Python program can depend on QueryForge without operating anything.

### The one invariant

**stdout carries exactly one JSON object and nothing else.** Every diagnostic, warning and panic
goes to stderr. An SDK parses stdout unconditionally, so anything else written there would corrupt
the stream for every language at once.

An SDK must **not** treat the exit code as the success signal — a failed request legitimately exits
non-zero with a perfectly good error object on stdout, and branching on the exit code throws that
structured error away.

| Exit code | Meaning |
|---|---|
| `0` | The response has `success: true` |
| `1` | The response has `success: false` — a well-formed error object is on stdout |
| `2` | No response could be produced at all; stdout is empty, see stderr |

---

## Request

```json
{
  "op": "translate",
  "backend": "mysql",
  "config": { "entity": "Order", "fields": [] },
  "query": "delivered orders over $100",
  "scope": { "tenantId": "T-42" },
  "options": { "timeoutMs": 30000, "maxRepairs": 2 }
}
```

| Field | Type | Notes |
|---|---|---|
| `op` | string | Required. `translate` \| `generate` \| `validate` \| `version` |
| `backend` | string | `sql` \| `mysql` \| `mongo`. Defaults to `sql`. Ignored by `validate` and `version` |
| `config` | object | The QueryForge config, inline. Required by every op but `version` |
| `query` | string | The natural-language question. Required by `translate` |
| `ast` | object | A pre-built AST. Required by `generate` and `validate` |
| `scope` | object | Filters AND-ed into the query after validation |
| `options` | object | See below; every field optional |

### Options

| Field | Type | Default | Notes |
|---|---|---|---|
| `timeoutMs` | int | 60000 | Bounds the whole operation, model call included |
| `maxRepairs` | int | 2 | Validation-repair retries. `0` means one attempt, no repairs |
| `scopeInAst` | bool | false | Report the effective AST rather than the model's own |
| `includeRaw` | bool | false | Include the model's verbatim reply on `raw` |
| `logLevel` | string | *(process default: off)* | `off` \| `error` \| `warn` \| `info` \| `debug`. Structured JSON diagnostics on **stderr**. An unrecognised value is rejected with `INVALID_REQUEST` rather than defaulted — see below |
| `requestId` | string | *(generated)* | Correlation id, stamped on every log record this invocation writes. Sanitized before use: anything outside `[A-Za-z0-9._:/-]` is dropped and the length is bounded, because it is caller-controlled text heading into a log sink |

#### Logging

Diagnostics are line-delimited JSON on **stderr**, never stdout — stdout carries exactly one JSON
object and that is the whole contract you are parsing against. Logging is **off by default**, so an
SDK that sets nothing sees the byte-for-byte behaviour it always did.

Three channels can set the level, in this precedence:

```
--log-level flag  >  QUERYFORGE_LOG_LEVEL  >  options.logLevel  >  off
```

The environment outranks the request option deliberately: it is the channel an operator can change
on a running container without shipping code, which is what makes "set `QUERYFORGE_LOG_LEVEL=debug`
and reproduce it" a workable instruction.

An unrecognised level is an error, not a default. Falling back to `off` would hide diagnostics from
someone who explicitly asked for them; falling back to `debug` would start writing model output a
typo did not authorise. The error is a normal `INVALID_REQUEST` response on stdout, so the protocol
promise still holds.

Field names, severity semantics and the privacy contract are documented in
[../../docs/OBSERVABILITY.md](../../docs/OBSERVABILITY.md).

### Unknown fields are rejected

A request carrying a field the engine does not recognise is refused with `INVALID_REQUEST`. This is
deliberate and worth keeping: silently dropping a misspelled `"scop"` would produce a perfectly
valid query built **without** the tenancy filter, and nothing in the output would look wrong.

---

## Operations

| Op | Model call? | Needs API key? | Returns |
|---|---|---|---|
| `translate` | Yes | Yes | AST + compiled query + explanation |
| `generate` | No | No | Compiled query + explanation, from a supplied AST |
| `validate` | No | No | Explanation, or the findings that make the AST illegal |
| `version` | No | No | Engine version, protocol version, registered backends |

`version` is the handshake and takes no config — it must answer even when the caller's config is
unusable, because an SDK performs it before it knows whether that config is any good.

---

## Response

Exactly one of the two halves is populated, discriminated by `success`.

### Success

```json
{
  "success": true,
  "protocol": "1.0",
  "op": "translate",
  "backend": "mysql",
  "sql": "SELECT status, amount FROM orders WHERE (tenant_id = ? AND amount > ?)",
  "args": ["T-42", 100],
  "ast": { "version": "1.0", "entity": "Order", "filter": {} },
  "explain": "Return all fields from Order where amount is greater than 100.",
  "warnings": ["filtering on non-indexed field \"amount\" may be slow"],
  "scope": [
    { "field": "tenantId", "operator": "equals", "value": { "kind": "string", "v": "T-42" }, "declared": true }
  ],
  "providerUsed": "groq/llama-3.3-70b",
  "repairAttempts": 0
}
```

SQL backends fill `sql` + `args`; document backends fill `doc` instead. Every field but `success`
and `protocol` is omitted when empty, so an SDK must not assume any of them are present.

### Failure

```json
{
  "success": false,
  "protocol": "1.0",
  "op": "generate",
  "code": "VALIDATION_FAILED",
  "message": "filter: unknown field \"amont\" (did you mean: amount?)",
  "details": [
    {
      "code": "unknown_field",
      "path": "filter",
      "field": "amont",
      "message": "filter: unknown field \"amont\"",
      "suggestions": ["amount"]
    }
  ]
}
```

### Error codes

`code` is the protocol-level class an SDK maps onto an exception type. `details[].code` is the
engine's own finer-grained vocabulary (`unknown_field`, `value_out_of_domain`, …). The two are
visually distinct — SCREAMING_SNAKE versus lower_snake — so they cannot be confused.

| Code | Meaning | Retryable |
|---|---|---|
| `INVALID_REQUEST` | Malformed request, or a missing required field | No |
| `UNKNOWN_OP` | Op not implemented — usually an SDK newer than the engine | No |
| `INVALID_CONFIG` | The config did not parse, or broke a structural rule | No |
| `UNKNOWN_BACKEND` | No generator registered for that backend id | No |
| `INVALID_SCOPE` | A caller-supplied scope filter was rejected — an application bug | No |
| `VALIDATION_FAILED` | An AST broke a config rule. `details` says which | No |
| `UNSUPPORTED_REQUEST` | The model declined; the question is not expressible here | No |
| `MODEL_OUTPUT` | The model answered but never with usable JSON | Yes |
| `MODEL_TRANSPORT` | The model was never reached — network, key, rate limit | Yes |
| `GENERATE_FAILED` | A legal AST could not be compiled for this backend | No |
| `TIMEOUT` | The deadline in `options.timeoutMs` expired | Yes |
| `INTERNAL` | A bug in the engine | No |

An SDK **must** map an unrecognised code to its base error class rather than failing, so an engine
that adds a code does not crash an older SDK.

---

## Versioning

`protocol` is on every response. An SDK checks the **major** component and refuses a mismatch.

- **MINOR bump** — additive: a new op, a new optional response field. Old SDKs keep working
  because they ignore what they do not recognise.
- **MAJOR bump** — an existing field changed meaning or disappeared. Continuing would produce
  quietly wrong output rather than an error, so the SDK must refuse.

A response with no `protocol` field at all comes from an engine too old to talk to, and is refused.

---

## Writing a new SDK

The full checklist:

1. **Validate locally what you can.** A blank question or a bogus backend should not cost a
   process spawn.
2. **Find the binary.** Detect `<os>-<arch>`; honour an override variable; report a broken
   override rather than falling back to a different engine.
3. **Spawn without a shell.** The config and the question are user data.
4. **Drain stdout and stderr concurrently with the write to stdin.** A subprocess whose stdout
   pipe fills will block; if you are still writing the request, both sides wait forever.
5. **Read stdout to EOF, ignore the exit code**, and parse one JSON object.
6. **Check the protocol major version.**
7. **Branch on `success`**, and map `code` onto a native exception with an unknown-code fallback.
8. **Ignore unknown response fields**, so a newer engine does not break you.
9. **Send `logLevel` and `requestId` only when the host has configured logging.** Always sending
   them breaks every caller pointing at a pre-1.1 engine, for a field they never asked for.

The Python (`sdk-python/`) and Java (`sdk-java/`) implementations are both small and are the
reference for all of the above.

---

## Debugging by hand

```bash
# Handshake
echo '{"op":"version"}' | queryforge --pretty

# Compile an AST with no model call and no API key
queryforge --pretty --request request.json

# Or from a heredoc
queryforge --pretty <<'EOF'
{"op":"validate","config":{"entity":"Order","fields":[{"name":"status","type":"string"}]},
 "ast":{"version":"1.0","entity":"Order"}}
EOF
```

`--pretty` indents the response. SDKs never set it; the output remains a single parseable object
either way.
