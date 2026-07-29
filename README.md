# QueryForge

[![CI](https://github.com/awsaman-ai/queryforge/actions/workflows/ci.yml/badge.svg)](https://github.com/awsaman-ai/queryforge/actions/workflows/ci.yml)
[![Go Reference](https://pkg.go.dev/badge/github.com/awsaman-ai/queryforge.svg)](https://pkg.go.dev/github.com/awsaman-ai/queryforge)
[![Go Report Card](https://goreportcard.com/badge/github.com/awsaman-ai/queryforge)](https://goreportcard.com/report/github.com/awsaman-ai/queryforge)
[![License](https://img.shields.io/badge/license-Apache%202.0-blue.svg)](LICENSE)
[![Go 1.25+](https://img.shields.io/badge/go-1.25%2B-00ADD8.svg)](go.mod)

**Turn plain English into SQL and MongoDB queries — without letting a language model write the query.**

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

## Install

```bash
go get github.com/awsaman-ai/queryforge
```

```go
cfg, _ := qf.LoadConfig("orders.config.json")
engine := qf.New(cfg)

res, err := engine.Translate(ctx, "cancelled orders over 200 dollars", "sql", nil)
fmt.Println(res.Query.SQL)   // SELECT … WHERE (status = $1 AND amount > $2) …
fmt.Println(res.Query.Args)  // [CANCELLED 200]
fmt.Println(res.Explain)     // plain-English readback of what it understood
```

No database connection, ever. QueryForge hands you the query; executing it stays yours.
Core has **no third-party dependencies** — standard library only.

📖 **[Full configuration reference →](https://awsaman-ai.github.io/queryforge/)**

### First: what's an AST?

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
| **No SQL injection** | The model emits structure, never a string. SQL is parameterized (`$1, $2, …`); Mongo values are typed map elements. |
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

A key the config doesn't declare is used **verbatim** as the physical column name. That's fine for one backend. When you target both SQL and Mongo, declare the column once with `queryable: false` — hidden from the model, still scopable, and mapped per backend:

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
"model":  { "provider": "gemini", "baseURL": "…/v1beta/openai", "model": "gemini-3.5-flash", "apiKeyEnv": "QF_API_KEY" },
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

Shipped examples in [`examples/`](examples/):
- `orders.config.json` — one Order config compiled to **both** SQL and Mongo.
- `sql_employees.config.json` — relational best practices (indexed/prioritized columns, RBAC, excluded `ssn`).
- `nosql_products.config.json` — document best practices (array fields, text search, rating bounds).

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
| `queryforge.go` | `Engine`: `New`, `Translate`, `GenerateFrom`, `Validate`, `Register` | — |

Stages 1–5 (planner) are the only place a model runs. Stages 6–10 (validate → generate → explain) are pure Go — the guarantees live there.

## Testing

The deterministic half is fully tested with **no API key and no network**:

```bash
go test ./...
```

Every component ships with happy-path and adversarial "try to break it" tests (injection strings, unknown fields, illegal operator/type pairs, out-of-domain enums, deep nesting, oversized limits). Bug tracking lives in [`bugs.csv`](bugs.csv).

## Read-only guarantee

QueryForge builds queries; it does **not** connect to or execute against your database. Every output is a read (`SELECT` / `find`). There is no operator or config option that can mutate data.

## Roadmap

- **Now (Phase 1):** Go library; JSON config; validator; SQL + Mongo generators; Gemini/Groq/Ollama via OpenAI-compatible HTTP; CLI; HTML config docs.
- **v0.0.2:** [scope filters](#scope-filters-your-own-filters-on-every-query) — caller-supplied predicates AND-ed onto every query, for multi-tenancy.
- **Next:** Elasticsearch/OpenSearch generator; YAML config; confidence scores; MCP server + REST facade.
- **Later:** aggregation AST node; more backends (DynamoDB, Cassandra, ClickHouse); SDKs for other languages over the same AST contract.

## Contributing

Issues and pull requests are welcome. CI runs `gofmt`, `go vet`, and the full
test suite with the race detector on every PR, including from forks — the suite
is entirely offline, so it needs no API key.

If you are changing behaviour, please add a test that fails without your change.
The project keeps a QA log in [`bugs.csv`](bugs.csv); if you are fixing a defect,
adding a row there is welcome but not required.

Security issues should go through [private disclosure](SECURITY.md), not a public
issue.

## License

Licensed under the [Apache License, Version 2.0](LICENSE).

You may use, modify, distribute, and embed this in commercial software. The
license also grants you an explicit patent licence from every contributor. In
return, keep the copyright and licence notices, and state any significant changes
you make to the files.
