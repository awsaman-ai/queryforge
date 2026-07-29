# QueryForge

**A config-driven Go library that compiles natural language into database queries across many backends — through a validated intermediate representation (the Query AST).**

QueryForge is a *compiler, not a chatbot*. The model never writes a query string. It emits a **validated AST** constrained to a config-registered vocabulary of fields and operators; deterministic Go then compiles that AST to Mongo, SQL, and more.

```
Natural language  →  [ AI planner ]  →  Query AST (IR)  →  [ deterministic generators ]  →  SQL / Mongo / …
   (unbounded)         model + config     (bounded, typed)      pure code, no AI, no network
```

The point of the split: everything that must be *guaranteed* lives in deterministic, offline-testable code.

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

## Install

```bash
go get github.com/awsaman-ai/queryforge
```

Core has **no third-party dependencies** — standard library only (`net/http`, `encoding/json`).

## Quickstart (library)

```go
cfg, _ := queryforge.LoadConfig("examples/orders.config.json")
engine := queryforge.New(cfg)

// Full pipeline: natural language -> AST -> validate -> SQL
res, err := engine.Translate(ctx, "delivered orders over $100 in the last 30 days", "sql")
fmt.Println(res.Query.SQL)   // SELECT * FROM orders WHERE ... ($1, $2, …)
fmt.Println(res.Query.Args)  // bound arguments
fmt.Println(res.Explain)     // prose explanation

// Deterministic half only — no model call, no network. Fan one AST out to many backends.
sql, _   := engine.GenerateFrom(ast, "sql")
mongo, _ := engine.GenerateFrom(ast, "mongo")
```

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
- **Next:** Elasticsearch/OpenSearch generator; YAML config; multi-tenancy predicate injection; confidence scores; MCP server + REST facade.
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
