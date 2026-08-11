# QueryForge

[![CI](https://github.com/awsaman-ai/queryforge/actions/workflows/ci.yml/badge.svg)](https://github.com/awsaman-ai/queryforge/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/awsaman-ai/queryforge.svg)](https://pkg.go.dev/github.com/awsaman-ai/queryforge)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](go.mod)

**Replace the whole filter panel with one sentence.**

Your users stop translating what they want into dropdowns, checkboxes and date pickers — they just say it, in their own words. QueryForge turns that into a query you can trust.

The model never writes the query. It fills in a typed **Query AST** your config constrains, and deterministic Go compiles that to whatever database you run. Postgres (`"sql"`), MySQL (`"mysql"`) and MongoDB (`"mongo"`) ship today; a new backend is a generator, not a rewrite.

🌐 Website: **[queryforge-service.vercel.app](https://queryforge-service.vercel.app)**
▶️ **[Live demo](https://queryforge-demo.amtry.in)** — flip one toggle and watch a thirteen-filter product page become a single search box
📖 [Docs](https://awsaman-ai.github.io/queryforge/) · 🛠 [Build a config in your browser](https://awsaman-ai.github.io/queryforge/config-builder.html) — no install, nothing leaves the page
🔌 [**queryforge_mcp**](https://github.com/awsaman-ai/queryforge_mcp) — use it from Claude Desktop or Cursor, over the Model Context Protocol

Your users ask a question. QueryForge gives you a parameterized query you can actually trust.

```
"orders over 500 dollars in the last 30 days that were not cancelled, newest first, top 20"
```

```sql
SELECT status, created_at, amount, customer_name FROM orders
WHERE (status <> $1 AND created_at >= $2 AND amount > $3)
ORDER BY created_at DESC LIMIT 20
-- args: ["CANCELLED", "2026-06-29T08:49:06Z", 500]
```

The same question, against MongoDB, from the identical intermediate representation:

```javascript
{ amount: {$gt: 500}, createdAt: {$gte: "2026-06-29T08:49:06Z"}, status: {$ne: "CANCELLED"} }
```

## Why not just ask an LLM for SQL?

Because you cannot check what comes back. Ask a model for SQL directly and it can invent a column, invent a table, quietly widen a filter, or hand you a statement that is only *probably* right.

QueryForge never lets the model near a query string. The model fills in a **typed form** — the Query AST — constrained to a vocabulary your config registers. Ordinary deterministic Go does everything after that:

```
Natural language  →  [ AI planner ]  →  Query AST  →  [ generators ]  →  SQL / Mongo / …
   (unbounded)        model + config     (typed)       pure code, no AI, no network
```

Everything that must be *guaranteed* lives on the right-hand side, where it can be tested offline.

| | |
|---|---|
| **Invented a field?** | Rejected by the validator before anything compiles — with "did you mean" suggestions. |
| **SQL injection?** | The model emits structure, never a string. Values are bound (`$1`, `$2`); only config-supplied identifiers reach the statement. |
| **Asked to delete something?** | Not expressible. The AST has no mutation node, so every output is a `SELECT` or a `find`. |
| **Asked about a field you hid?** | `returnable: false` is enforced on the default projection too, not just explicit `select`. |
| **Asked something impossible?** | You get a typed refusal, not a plausible query built on a lookalike field. |
| **Asked for another tenant's rows?** | [Scope filters](#scope-filters-your-own-filters-on-every-query) are AND-ed on after the model has answered — it never learns the field exists. |

## Get started

Pick how you want to call it. All four need the same two things and nothing else: a **config file**
describing your data ([build one in your browser](https://awsaman-ai.github.io/queryforge/config-builder.html))
and your **model API key** in the environment. No server, no Docker, no database connection.

### 🔌 MCP — Claude Desktop, Cursor

```bash
go install github.com/awsaman-ai/queryforge_mcp@latest
```

Add it to `claude_desktop_config.json`:

```json
{
  "mcpServers": {
    "queryforge": {
      "command": "queryforge-mcp",
      "args": ["--config", "orders.config.json"]
    }
  }
}
```

Restart Claude and ask *"cancelled orders over $200"* — it comes back as a validated query.
[Docs →](https://github.com/awsaman-ai/queryforge_mcp)

### 🐹 Go — the library

```bash
go get github.com/awsaman-ai/queryforge
```

```go
cfg, _ := qf.LoadConfig("orders.config.json")
engine := qf.New(cfg)

res, _ := engine.Translate(ctx, "cancelled orders over 200 dollars", "sql", nil)

fmt.Println(res.Query.SQL)   // SELECT … WHERE (status = $1 AND amount > $2) …
fmt.Println(res.Query.Args)  // [CANCELLED 200]
fmt.Println(res.Explain)     // plain-English readback of what it understood
```

No third-party dependencies — standard library only.
[API docs →](https://pkg.go.dev/github.com/awsaman-ai/queryforge)

### ☕ Java — Maven, Java 11+

Two dependencies: the classes, and the engine binary for the platform you run on.

```xml
<dependency>
    <groupId>io.github.awsaman-ai</groupId>
    <artifactId>queryforge</artifactId>
    <version>1.1.2</version>
</dependency>
<dependency>
    <groupId>io.github.awsaman-ai</groupId>
    <artifactId>queryforge</artifactId>
    <version>1.1.2</version>
    <classifier>linux-amd64</classifier>
</dependency>
```

```java
QueryForge forge = QueryForge.postgres(Paths.get("orders.config.json"));

String sql = forge.query("cancelled orders over $200").toSql();
List<Object> args = forge.query("cancelled orders over $200").toArgs();
```

Zero runtime dependencies — not even a JSON library. [Docs →](sdk-java/README.md)

### 🐍 Python — pip

```bash
pip install queryforge-ai   # installs as queryforge-ai, imports as queryforge
```

```python
from queryforge import QueryForge

qf = QueryForge.postgres("orders.config.json")
pending = qf.query("cancelled orders over $200")

print(pending.to_sql())    # SELECT … WHERE (status = $1 AND amount > $2) …
print(pending.to_args())   # ('CANCELLED', 200)
```

The engine ships inside the wheel — no Go toolchain needed. [Docs →](sdk-python/README.md)

---

Whichever you pick, the answer is the same query: the model fills in an AST, your config validates
it, and deterministic code compiles it. Swap `postgres` for `mysql` or `mongo` and the same sentence
compiles for that backend instead.

No database connection, ever. QueryForge hands you the query; executing it stays yours.

📖 **[Full configuration reference →](https://awsaman-ai.github.io/queryforge/)**

## What's an AST?

**AST = Abstract Syntax Tree.** It's a plain JSON object describing *what the user asked for*, sitting between the English sentence and the finished query. The name is literal: a **tree** because filters nest (`A AND (B OR C)` branches), **abstract** because it knows nothing about SQL or Mongo — it says `"operator": "gt"`, never `>` and never `$gt`.

Ask *"orders over 500 dollars in the last 30 days that were not cancelled, newest first, top 20"* and the model produces exactly this — nothing else:

```json
{
  "entity": "Order",
  "filter": {
    "type": "logical", "op": "AND",
    "children": [
      { "type": "comparison", "field": "amount",    "operator": "gt",        "value": {"kind":"number","v":500} },
      { "type": "comparison", "field": "createdAt", "operator": "after",     "value": {"kind":"relative_date","unit":"day","amount":-30} },
      { "type": "comparison", "field": "status",    "operator": "notEquals", "value": {"kind":"enum","v":"CANCELLED"} }
    ]
  },
  "sort":  [{ "field": "createdAt", "dir": "DESC" }],
  "limit": 20
}
```

That one object compiles into **both** of these, with no second prompt:

```sql
SELECT … FROM orders WHERE (status <> $1 AND created_at >= $2 AND amount > $3)
ORDER BY created_at DESC LIMIT 20
```
```javascript
{ amount: {$gt: 500}, createdAt: {$gte: "…"}, status: {$ne: "CANCELLED"} }
```

**Why not just let the model write SQL?** Because a model asked for SQL can invent a column, invent a table, or emit something you can't check before running it. With an AST in the middle the model only fills in a form; validation and generation are ordinary testable Go. And the AST has **no node type that can write** — so "read-only" doesn't depend on a check someone might forget, it depends on there being no way to express a write.

Full reference: [`docs/config.html`](docs/config.html#ast).

| Guarantee | How |
|---|---|
| **No hallucinated fields** | The AST is validated against the config before generation. Unknown field → hard rejection (with "did you mean" suggestions). |
| **No SQL injection** | The model emits structure, never a string. SQL is parameterized (`$1, $2, …` on Postgres, `?` on MySQL); Mongo values are typed map elements. Identifiers, which no dialect lets you bind, are checked against a strict identifier rule — at config load, and again on the way into the statement — and quoted where the dialect needs it. |
| **Read-only / GET only** | The operator catalogue contains no mutation verbs; every generator emits only `SELECT` / `find`. Enforced by test. |
| **N backends, one brain** | Add a database = implement one generator. Zero prompt or model changes. |
| **Config-driven, no training** | You register metadata. The config *is* the prompt, the grammar, and the mapping. |

---

## Quickstart (library)

```go
cfg, _ := queryforge.LoadConfig("examples/orders.config.json")
engine := queryforge.New(cfg)

// Full pipeline: natural language -> AST -> validate -> SQL
res, err := engine.Translate(ctx, "delivered orders over $100 in the last 30 days", "sql", nil)
fmt.Println(res.Query.SQL)   // SELECT * FROM orders WHERE ... ($1, $2, …)
fmt.Println(res.Query.Args)  // bound arguments
fmt.Println(res.Explain)     // prose explanation

// Deterministic half only — no model call, no network. Fan one AST out to many backends.
sql, _   := engine.GenerateFrom(ast, "sql", nil)
mongo, _ := engine.GenerateFrom(ast, "mongo", nil)
```

The last argument is the **scope** — extra filters your application imposes on every query. `nil` means none; see below.

## Scope filters: your own filters on every query

Some predicates are not the user's to choose. You already know the caller's subscription, user, and enterprise before any question is asked, and every query must be confined to them **whatever the question says**. Those values are not query vocabulary — nobody should be able to phrase, widen, or omit them — so they never go in your config's `fields`, and the model never sees them.

Pass them as a map. Every entry is AND-ed onto the query:

```go
res, err := engine.Translate(ctx, "delivered orders over 500 dollars", "sql", qf.Scope{
    "subscriptionId": session.SubscriptionID,  // "SUB-42"
    "userId":         session.UserID,          // 9
    "enterpriseId":   session.EnterpriseIDs,   // []string{"E-1", "E-2"}
})
```

```sql
SELECT … FROM orders
WHERE (enterpriseId IN ($1, $2) AND subscriptionId = $3 AND userId = $4
       AND status = $5 AND amount > $6)
ORDER BY created_at DESC LIMIT 50
-- args: ["E-1", "E-2", "SUB-42", 9, "DELIVERED", 500]
```

The explanation keeps the two apart, so a readback never presents a forced predicate as something the user asked for:

```
Return all fields from Order where (status equals "DELIVERED" AND amount is greater than 500),
sorted by createdAt (descending). Always scoped to enterpriseId is one of [E-1, E-2],
subscriptionId equals "SUB-42", userId equals 9.
```

### Why it holds

| | |
|---|---|
| **The model never learns the field exists** | Scope is applied *after* the model answers; the prompt never names it. Asked *"show me orders for subscription SUB-99 instead"*, the model declines — there is no such field in its vocabulary. Verified live. |
| **A scope can only narrow** | Predicates are AND-ed at the **root**. A model filter of `A OR B` becomes `scope AND (A OR B)`, never `scope OR …`. No AST the model can emit escapes it. |
| **Values are still parameterized** | A scope value is data: bound as `$1, $2, …` in SQL, a typed map element in Mongo. An injection payload stays an argument. |
| **The deterministic path is scoped too** | `GenerateFrom` takes the same map — otherwise a caller could sidestep tenancy by building the AST themselves. |
| **Bad scope fails before the model call** | A `nil` value, an empty list, or a value that breaks a declared field's type is your bug, not the question's. It costs no API quota and is tagged `qf.ErrScope` so a facade can answer `400`. |

### What a value may be

| You pass | You get |
|---|---|
| a scalar — `string`, `bool`, any int/uint/float, `time.Time`, or a pointer to one | `equals` |
| a slice or array of those | `in` |
| a scalar, on a field declared `type: "array"` | `contains` |
| a slice, on a field declared `type: "array"` | `containsAny` |

Keys apply in alphabetical order, so the query and its argument order are identical run to run.

### Naming the column

A key the config doesn't declare is used **verbatim** as the physical column name — after being checked against the identifier rule (`[A-Za-z_][A-Za-z0-9_]*`, dots allowed for Mongo paths). A key that is not an identifier is rejected as an `ErrScope`, so a key derived from a header, a config file, or a per-customer mapping table can never become query syntax. That's fine for one backend. When you target both SQL and Mongo, declare the column once with `queryable: false` — hidden from the model, still scopable, and mapped per backend:

```jsonc
{ "name": "tenantId", "type": "string",
  "queryable": false,                                    // invisible to the model
  "mapping": { "sql": "tenant_id", "mongo": "tenantId" } }
```

```go
engine.GenerateFrom(ast, "sql",   qf.Scope{"tenantId": "T-1"})  // … WHERE tenant_id = $1
engine.GenerateFrom(ast, "mongo", qf.Scope{"tenantId": "T-1"})  // { tenantId: "T-1", … }
```

Declaring it also gets the value checked against the field's type, enum domain, and `validators`.

### What comes back

`res.Scope` always lists the normalized filters that were applied — the record an audit log wants. `engine.ScopeInAST` chooses what `res.AST` holds; the query and explanation include the scope either way:

- **`false` (default)** — the AST is exactly what the model produced. It **round-trips**: pass it back to `GenerateFrom` with the same scope and get the identical query.
- **`true`** — the AST is the effective one, scope included, so a single object proves what ran. It then names fields your config need not declare, so it won't pass `GenerateFrom` validation unless they're registered.

Try it from the CLI:

```bash
go run ./examples -config examples/orders.config.json -backend sql \
  -text "delivered orders over 500 dollars" \
  -scope subscriptionId=SUB-42 -scope userId=9 -scope 'enterpriseId=["E-1","E-2"]'
```

📖 [Full scope reference →](docs/config.html#scope)

## Try the CLI

The **offline path** needs no API key — it compiles a hand-written AST:

```bash
go run ./examples -config examples/orders.config.json -backend sql   -ast sample_ast.json
go run ./examples -config examples/orders.config.json -backend mongo -ast sample_ast.json
```

The **natural-language path** calls a model (see below):

```bash
export QF_API_KEY=<your-key>
go run ./examples -config examples/orders.config.json -backend sql \
  -text "delivered but not refunded, created in the last 30 days, tagged premium and express"
```

## Choosing a model — it's a config change, not a code change

Two providers ship, both standard-library only. The default speaks any **OpenAI-compatible `/chat/completions`** endpoint; a second speaks **Anthropic's native Messages API**. Which one is used is chosen by the `model` block (`provider: "anthropic"` or an `api.anthropic.com` base URL selects the native one) — a config change, never a code change.

| Provider | `baseURL` | Notes |
|---|---|---|
| **Google Gemini** (free tier) | `https://generativelanguage.googleapis.com/v1beta/openai` | Default in the examples. |
| **Groq** (free tier) | `https://api.groq.com/openai/v1` | Fast Llama/Qwen class. |
| **OpenAI** | `https://api.openai.com/v1` | e.g. `gpt-4.1-mini`. |
| **Anthropic** (native) | `https://api.anthropic.com` | Set `provider: "anthropic"`; uses the Messages API (`x-api-key`), e.g. `claude-opus-4-8`. |
| **Ollama** (local, no key) | `http://localhost:11434/v1` | Fully offline. Leave `apiKeyEnv` empty. |

The API **key value never lives in the config** — `apiKeyEnv` names the environment variable that holds it. Adding another provider (a new dialect) means implementing the one-method `ModelProvider` interface and selecting it — see `provider.go` / `provider_anthropic.go`.

### Fallback across models (resilience)

List several models and QueryForge tries them in order, using the first that answers — so a rate-limit, quota, or billing block on one provider transparently falls through to the next. It's the same `ModelProvider` seam composed (`FallbackProvider`), so nothing else changes.

```jsonc
"model":  { "provider": "gemini", "baseURL": "…/v1beta/openai", "model": "gemini-3.1-flash-lite", "apiKeyEnv": "QF_API_KEY" },
"models": [
  { "provider": "groq",      "baseURL": "https://api.groq.com/openai/v1", "model": "llama-3.3-70b-versatile", "apiKeyEnv": "GROQ_API_KEY" },
  { "provider": "anthropic", "baseURL": "https://api.anthropic.com",      "model": "claude-opus-4-8",          "apiKeyEnv": "ANTHROPIC_API_KEY" },
  { "provider": "ollama",    "baseURL": "http://localhost:11434/v1",      "model": "qwen2.5" }
]
```

`model` is the primary; `models` are the ordered fallbacks (a chain can mix providers freely). The CLI prints which model answered. A ready-to-run example is [`examples/orders.fallback.config.json`](examples/orders.fallback.config.json):

```bash
go run ./examples -config examples/orders.fallback.config.json -backend sql -text "delivered orders in the last 30 days"
```

## Configuration

One JSON file is the prompt context, the validation rulebook, the field→backend mapping, and the model selector. **Full reference:** [`docs/config.html`](docs/config.html).

**Don't hand-write it — build it:** [`docs/config-builder.html`](docs/config-builder.html) is a single self-contained page (open it straight from disk, no server, no network). Each field card starts with the four settings that matter — name, type, values, synonyms — and folds the rest behind **Advanced**; every setting carries an ⓘ that explains what it does and shows a worked example. It validates as you type with the same rules the loader enforces, downloads the finished JSON, and imports an existing config for editing.

Shipped examples in [`examples/`](examples/):
- `orders.config.json` — one Order config compiled to **both** SQL and Mongo.
- `sql_employees.config.json` — relational best practices (indexed/prioritized columns, RBAC, excluded `ssn`).
- `nosql_products.config.json` — document best practices (array fields, text search, rating bounds).
- `mongo_nested.config.json` — nested documents: an embedded address, plus arrays of sub-documents.

### Nested Mongo documents

Nesting lives entirely in the mapping layer, so the vocabulary the model sees stays flat. An embedded document just needs a dot path:

```json
{ "name": "city", "type": "string", "mapping": { "mongo": "shippingAddress.city" } }
```

An **array of sub-documents** needs one more key, because dot paths alone are silently wrong there — Mongo satisfies each one independently, so `{"items.sku":"ABC","items.price":{"$gt":100}}` matches an order whose sku is on one item and whose price is on another. Declaring the array fixes it:

```json
{ "name": "itemSku",   "type": "string", "mapping": { "mongo": "items.sku" },   "elemMatch": "items" },
{ "name": "itemPrice", "type": "number", "mapping": { "mongo": "items.price" }, "elemMatch": "items" }
```

```
"orders containing item ABC costing over 100"
→ {"items": {"$elemMatch": {"sku": "ABC", "price": {"$gt": 100}}}}
```

One element must now satisfy every condition. `elemMatch` must be a leading segment of the field's Mongo path — a mismatch is rejected at load, not discovered in production. Grouping applies to `AND` only, is per-array, and other backends ignore the key entirely (give the field a flat `mapping.sql` column). Full rules: [`docs/config.html#nested`](docs/config.html).

### Value case

When a column stores `SHIPPED` but people say "shipped", say so once on the field rather than teaching the model to shout:

```json
{ "name": "status", "type": "enum", "values": ["shipped", "cancelled"], "valueCase": "upper" }
```

```
"orders that shipped"
→ SQL    status = $1   args: ["SHIPPED"]
→ Mongo  {"status": "SHIPPED"}
```

The fold happens only when the query is built. The model never sees the key and still emits the `values` as written, validation still checks that exact spelling, and the AST stays round-trippable — so one AST compiles correctly for a second backend that cases things differently. It covers every string a predicate carries, including scope filters and fields inside an `elemMatch` array; numbers, booleans and dates are untouched, and a raw `regex` value is deliberately exempt (upper-casing `\d` would give `\D`). Only `string`, `enum` and arrays of them may set it — anywhere else the key could never fire, so the config is rejected at load. Full rules: [`docs/config.html#valuecase`](docs/config.html).

## Observability

QueryForge never writes a log line. It reports facts through one optional
callback and lets you decide the destination, the format, and the severity:

```go
engine := qf.New(cfg)
engine.SetObserver(func(ctx context.Context, e qf.Event) {
    switch e.Kind {
    case qf.EventModelCall: // one round trip: latency, tokens, finish reason
        log.Info("model", "model", e.Model, "latency", e.Latency,
            "prompt", e.PromptTokens, "hidden", e.HiddenTokens, "total", e.TotalTokens)
    case qf.EventAttempt: // one plan+validate cycle; e.Raw on failure
        if e.Outcome != qf.OutcomeOK {
            log.Warn("attempt failed", "n", e.Attempt, "outcome", e.Outcome, "err", e.Err)
        }
    case qf.EventTranslate: // the whole call, on every exit path
        log.Info("translate", "outcome", e.Outcome, "duration", e.Duration,
            "repairs", e.RepairAttempts)
    }
})
```

**What it watches, and why only that.** A traced live request measured the entire
deterministic half — parse, validate, compile, explain — at **0.5ms** against a
**1,781ms** model call. Roughly **99.97%** of a translation is one HTTP round
trip, so the seam reports the model interaction and leaves the deterministic
core alone. There is nothing to watch there.

Use `SetObserver` rather than assigning `engine.Observe`: only the former pushes
the observer down into the provider, where latency and token counts live. A
fallback chain reports one `EventModelCall` per attempted model, so a chain that
is silently always falling back no longer looks identical to a healthy one.

**Privacy is a design property, not a convention.** The question text is never
in an `Event`. Scope **values** are never in an `Event` — only `ScopeKeys`, the
field names — because those values are tenant, user, and enterprise ids. The API
key is never in an `Event`. The one field that can echo caller data is `Raw`, the
model's verbatim reply, populated **only on a failed attempt**; it is the only
way to learn *what* an unusable reply actually said, and it should be logged at
debug level and truncated. A test asserts the rest with a canary rather than
trusting the convention.

The `ctx` is passed through untouched so you can pull a request id or trace span
off it and correlate every event with the request that caused it. The Observer
is called **synchronously**, must be safe for concurrent use, and must not panic
— a panicking Observer is not recovered, because a seam that silently stops
reporting is worse than a loud failure.

## Architecture

| File | Role | AI? |
|---|---|---|
| `ast.go` | Query AST types + JSON (de)serialization | — |
| `config.go` | Config types + loader + capability flags | — |
| `validate.go` | **The guarantee**: field/operator/type/enum/depth + capability checks, with suggestions | No |
| `generate.go` + `gen_sql.go` + `gen_mongo.go` | Registry + backend generators (parameterized, read-only) | No |
| `scope.go` | Caller-supplied filters → typed predicates AND-ed onto the query root | No |
| `explain.go` | AST → prose (dry-run, no execution) | No |
| `provider.go` | `ModelProvider` interface + OpenAI-compatible default + test stub | Yes |
| `planner.go` | Build prompt from config, parse model output → AST | Yes |
| `observe.go` | `Observer` seam: `Event`, `EventKind`, `Outcome` — facts out, no logging | — |
| `queryforge.go` | `Engine`: `New`, `Translate`, `GenerateFrom`, `Validate`, `Register` | — |

Stages 1–5 (planner) are the only place a model runs. Stages 6–10 (validate → generate → explain) are pure Go — the guarantees live there.

## Testing

The deterministic half is fully tested with **no API key and no network**:

```bash
go test ./...
```

Every component ships with happy-path and adversarial "try to break it" tests (injection strings, unknown fields, illegal operator/type pairs, out-of-domain enums, deep nesting, oversized limits).

CI also runs [golangci-lint](https://golangci-lint.run) against [`.golangci.yml`](.golangci.yml). To get the same report before you push:

```bash
go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@v2.12.2
golangci-lint run ./...
```

### Testing the half that tests cannot reach — [qfeval](https://github.com/awsaman-ai/qfeval)

The suite above covers everything after the model: AST → validate → generate is
pure Go, so it is pinned by ordinary assertions. What it cannot cover is
**comprehension** — whether the model reads a real sentence the way your user
meant it. That is a statistical property of a remote service, so it needs a
corpus and a pass rate, not an assertion.

**[qfeval](https://github.com/awsaman-ai/qfeval)** is a separate tool for exactly
that. You give it a CSV of sentences and the queries they should compile to; it
runs them through the real engine and grades each one — matched exactly, matched
in a different spelling, correctly refused, or wrong — with a canonicalizer that
forgives differences of spelling and refuses to forgive differences of meaning.

On the 25-case example corpus it ships, `gemini-3.1-flash-lite` scores
**24 / 25**, and the one failure turns out to be an ambiguous sentence rather
than a misread one. It also ships a 573-case dataset that exercises every
capability flag in this config format.

```bash
git clone https://github.com/awsaman-ai/qfeval.git && cd qfeval
export QF_API_KEY=<your-key>
go run . -in golden.example.csv -config ../queryforge/examples/orders.config.json
```

Worth running against **your** config and **your** sentences before you decide
which model to pay for.

## Other languages

QueryForge is a Go library first, but you do not need to write Go to use it. The same engine ships
as a native library for Python and Java.

```bash
pip install queryforge-ai
```
```python
from queryforge import QueryForge

sql = QueryForge.mysql(schema).query("users older than 18").to_sql()
```

```xml
<dependency>
    <groupId>io.github.awsaman-ai</groupId>
    <artifactId>queryforge</artifactId>
    <version>1.1.2</version>
</dependency>
```
```java
String sql = QueryForge.mysql(schema).query("users older than 18").toSql();
```

No server, no Docker, no Go toolchain, no background service. Install the package and call it.

### How it works

```
                  ┌─────────────────────────┐
                  │  QueryForge core (Go)   │
                  │  parser · AST · validate│
                  │  dialects · generators  │
                  └────────────▲────────────┘
                               │  one JSON object in, one out
                        stdin / stdout
                               │
          ┌────────────────────┼────────────────────┐
          │                    │                    │
   ┌──────┴──────┐      ┌──────┴──────┐      ┌──────┴──────┐
   │ Python SDK  │      │  Java SDK   │      │  (Node,     │
   │   PyPI      │      │Maven Central│      │   .NET…)    │
   └─────────────┘      └─────────────┘      └─────────────┘
```

Each SDK spawns the bundled engine binary as a **local subprocess**, writes one JSON request to
its stdin, and reads one JSON response from its stdout. No HTTP, no REST, no gRPC, no socket, no
daemon — the security surface stays at "a subprocess with the caller's own privileges".

The SDKs contain **no query logic whatsoever**. Every parser, validator, dialect and generator
lives in the Go core and nowhere else, so all three languages produce byte-identical output for
the same input. An SDK only validates arguments, finds the right binary, serializes a request, and
maps the reply onto native types and exceptions.

| | Python | Java |
|---|---|---|
| Install | `pip install queryforge-ai` | one Maven `<dependency>` |
| Runtime dependencies | none | none — not even a JSON library |
| Wrapper size | ~30 KB of code | ~35 KB jar |
| Engine binary | in the platform wheel (~6 MB) | in an auto-selected classifier jar (~2.5 MB) |
| Docs | [sdk-python/README.md](sdk-python/README.md) | [sdk-java/README.md](sdk-java/README.md) |

Both offer the full pipeline (`query`, one model call) and the deterministic half (`generate` /
`validate`, no model call and no API key — which is what makes them testable offline).

### The engine binary

`cmd/queryforge` is the executable the SDKs drive. It is also usable directly from any language,
or from a shell:

```bash
echo '{"op":"version"}' | queryforge --pretty
```

The wire format is specified in **[cmd/queryforge/PROTOCOL.md](cmd/queryforge/PROTOCOL.md)** —
read that if you are writing an SDK for another language. It documents the request and response
shapes, the stable error codes, the versioning rules, and a checklist of the eight things a
correct SDK has to do.

Build every platform's binary with:

```bash
./scripts/build-binaries.sh 1.1.2 dist
```

## Read-only guarantee

QueryForge builds queries; it does **not** connect to or execute against your database. Every output is a read (`SELECT` / `find`). There is no operator or config option that can mutate data.

## Roadmap

- **Now (Phase 1):** Go library; JSON config; validator; SQL + Mongo generators; Gemini/Groq/Ollama via OpenAI-compatible HTTP; CLI; HTML config docs.
- **v0.0.2:** [scope filters](#scope-filters-your-own-filters-on-every-query) — caller-supplied predicates AND-ed onto every query, for multi-tenancy.
- **Shipped alongside:** [queryforge_mcp](https://github.com/awsaman-ai/queryforge_mcp) — the compiler over MCP, as a separate module. The library stays transport-agnostic: no JSON-RPC, no MCP SDK, no transport code here.
- **Shipped alongside:** [Python and Java SDKs](#other-languages) — the same engine as a native library in each, driven over a JSON stdio protocol. No server, no duplicated query logic.
- **Next:** Elasticsearch/OpenSearch generator; YAML config; confidence scores; REST facade.
- **Later:** Node and .NET SDKs over the same [stdio protocol](cmd/queryforge/PROTOCOL.md); a persistent engine process reusing one binary across requests (an internal optimisation, invisible to SDK users); aggregation AST node; more backends (DynamoDB, Cassandra, ClickHouse).

## Contributing

Issues and pull requests are welcome. CI runs `gofmt`, `go vet`, and the full
test suite with the race detector on every PR, including from forks — the suite
is entirely offline, so it needs no API key.

If you are changing behaviour, please add a test that fails without your change.

Security issues should go through [private disclosure](SECURITY.md), not a public
issue.

## Related projects

| Project | What it is |
|---|---|
| **[queryforge_mcp](https://github.com/awsaman-ai/queryforge_mcp)** | QueryForge over the **Model Context Protocol**, so Claude Desktop, Cursor or any MCP client can query your data. Point it at the same config and it generates the tool schema from it — your entity, the operators each field permits, your enum domains — so the model picks from your vocabulary instead of guessing. It writes an AST, never a query string, and this library validates it before anything compiles. Hidden fields and physical column names never leave the process. |
| **[qfeval](https://github.com/awsaman-ai/qfeval)** | Scores a QueryForge config against a golden file of natural-language sentences and the queries they should compile to. Measures how well a given model actually understands your users' requests — and lets you compare two models on the same corpus before you pay for one. Ships a 25-case example (24/25 on `gemini-3.1-flash-lite`) and a 573-case dataset covering every capability flag. |
| [Documentation](https://awsaman-ai.github.io/queryforge/) | Guide, full configuration reference, and the Query AST explained. |
| [Config builder](https://awsaman-ai.github.io/queryforge/config-builder.html) | Build a config in a form and download it, with live validation. Runs entirely in the browser. |
| [Live demo](https://queryforge-demo.amtry.in) | A product page with a sentence box, translating for real. |

## License

Licensed under the [Apache License, Version 2.0](LICENSE).

You may use, modify, distribute, and embed this in commercial software. The
license also grants you an explicit patent licence from every contributor. In
return, keep the copyright and licence notices, and state any significant changes
you make to the files.
