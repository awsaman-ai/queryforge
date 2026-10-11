# QueryForge V2 — Non-LLM Semantic Query Compiler (Experiment)

**This is a research experiment, not a product, not a roadmap, and not version 2
of QueryForge.** It may be deleted. Nothing here is supported, released, or
imported by the production library.

## The question

> Can a natural-language request be turned into a correct final database query
> **without a generative LLM**, by combining semantic embeddings, vector
> retrieval, deterministic NLP, typed schema metadata, value resolution,
> relationship reasoning, and query-grammar compilation?

The experiment has to answer that with measurements. A disproved hypothesis is
an acceptable outcome; an unmeasured claim is not.

Full brief: [`QUERYFORGE_V2_NON_LLM_SEMANTIC_QUERY_EXPERIMENT.md`](../../QUERYFORGE_V2_NON_LLM_SEMANTIC_QUERY_EXPERIMENT.md)
at the workspace root. Current state: [`TODO.md`](TODO.md).

## Status

**Phase 4: candidate ranking.** Each phrase's retrieval candidates are ranked
on similarity plus structural evidence, and each phrase is accepted, rejected,
or reported AMBIGUOUS from the top-1 vs runner-up margin, never from similarity
alone. On the ambiguity set, ranking gets 33/34 against raw vector top-1's
20/34, with zero silent wrong guesses. Results:
[Phase 0](docs/phase-0-results.md) · [Phase 1](docs/phase-1-results.md) ·
[Phase 2](docs/phase-2-results.md) · [Phase 3](docs/phase-3-results.md) ·
[Phase 4](docs/phase-4-results.md) · [Phase 5](docs/phase-5-results.md) ·
[Phase 6](docs/phase-6-results.md) · [Phase 7](docs/phase-7-results.md) ·
[Phase 8](docs/phase-8-results.md).

**Phase 5: binding.** Accepted readings bind into a root entity, join paths from
the schema graph, and conditions: `customers who bought laptops` →
`product.category = laptop` via customer → order → product. The five
rejection cases the brief requires refuse with stable codes.

**Phase 6: planner and compiler.** A binding becomes a typed `SemanticPlan`
(config names only, dates resolved against the request clock). The compiler
turns a plan into PostgreSQL-style SQL by a fixed grammar, with every value as a
`$n` parameter and every identifier looked up in the config. The same plan
always gives the same bytes. `--compile` takes a hand-written plan; `--plan`
shows the plan for a sentence.

**Phase 7: end to end.** `--query` runs the whole pipeline and prints the
final query and its parameters. It never connects to a database. On 77
reviewed queries, 70 pass. None of the accepted queries is silently wrong, and
every one parses as PostgreSQL. A value must appear literally in the sentence,
and a phrase the config shares between targets is refused unless the
sentence's own words distinguish them (decision 019). Phase 7 is closed.

**Phase 8: security and hardening** (**closed** 2026-09-23, after three gate
rounds: decisions 021 and 022 §1–§7).
- **Completeness.** A meaningful qualifier is never silently discarded: "show
  product discount price" is refused, not read as price.
- **Exact numbers.** Numbers are exact decimals from the sentence to the
  parameters; no float64 anywhere a user's literal travels.
- **One request, one query.** SQL statement and comment syntax (`;`, `--`,
  `/*`, `*/`) is refused at the input boundary, before anything reads it.
- **Limits.** Every section 16.7 limit is enforced, and decision 021 defines
  what each one counts.
- **Writes.** SQL write keywords are refused as writes unless a configured
  phrase explains them.
- **Output guard.** Every compiled query passes a check that admits only the
  grammar's lexicon.
- **Config warnings.** `--validate-config` reports phrases shared across
  kinds and phrases that can never match.
- **Offline worker.** The embedding worker makes no network connection.
- **One reading per target.** A span may not resolve by similarity to a
  target the sentence already names by the config's own words.
- **Adversarial set.** On 93 cases, every one ends in a safe query or an
  explicit refusal, and none of the accepted queries is silently wrong.
- **A known semantic interpretation.** `list price of products` compiles to
  `SELECT price`: `list` is the request verb when `list price` is not a
  configured target, and a config that defines `list price` resolves that
  target instead. Intentional and configuration-driven, decided at the final
  gate (022 §7) — not a bug.

**Phase 9: performance** (measured 2026-09-23; awaiting the gate:
[results](docs/phase-9-results.md), [decision 023](docs/decisions/023-phase-9-benchmark-method.md),
[report](benchmarks/reports/performance.md)).
- **Warm latency.** p50 6.2ms at 100 schema entries, 14.9ms at 1,000,
  103.7ms at 10,000. The first two meet every target budget in the brief.
- **The deterministic half is free.** Extraction, ranking, binding, planning
  and compilation together cost under 1ms per request at every scale.
- **The cost is the semantic layer, and it splits in two.** Embedding is flat
  at ~5.5ms; the vector scan is linear in schema size (1.2ms → 11.1ms →
  109ms) because the index is a full scan searched once per text span.
- **Cold start.** 1.6s of model load, plus 10.2s to build a 10,000-entry
  index — or 0.2s to load it from cache.
- **Concurrency.** One embedding worker serialises every request: throughput
  saturates at ~185 req/s and more clients only add queueing.
- **Scale does not change the answer.** All 25 benchmark sentences produce
  byte-identical SQL at 100, 1,000 and 10,000 entries.

```text
Phase 0  ✔ scope, baseline, contracts
Phase 1  ✔ config model, validator, registry
Phase 2  ✔ semantic corpus + vector index
Phase 3  ✔ NLP phrase/slot extraction
Phase 4  ✔ candidate retrieval + ranking
Phase 5  ✔ semantic binding + schema graph
Phase 6  ✔ SemanticPlan + deterministic compiler
Phase 7  ✔ end-to-end SQL generation
Phase 8  ✔ security + ambiguity hardening
Phase 9  ~ performance + concurrency + memory (measured; awaiting the gate)
Phase 10 · accuracy, ablation, go/no-go
Phase 11 · other backends, only if Phase 10 says go
```

## Isolation

The production library must remain untouched. Three mechanisms enforce that:

| Mechanism | What it prevents |
|---|---|
| Separate module `queryforge.local/v2experiment` | v2 code ending up in the published library |
| `v2/go.work` shadowing the root workspace | needing to edit the root `go.work` |
| `internal/isolation/isolation_test.go` | importing production packages, database drivers, or remote model SDKs |

The nested `go.mod` also means the parent module's `go test ./...` skips this
directory entirely, so v2 cannot break the library's build or test run.

Decision records live in [`docs/decisions/`](docs/decisions/): isolation
strategy, module identity, the CLI contract, the closed vocabularies, and the
validation strategy.

## Scope of the first experiment

PostgreSQL-style SQL only. Filters, comparisons, `IN`/`NOT IN`, `LIKE`,
`AND`/`OR`, date ranges and relative dates, `COUNT`/`SUM`/`AVG`/`MIN`/`MAX`,
`ORDER BY`, `LIMIT`, `DISTINCT`, and joins over configured relationships.

Out of scope: subqueries, window functions, CTEs, vendor-specific functions,
DML, DDL, and any free-form SQL fragment.

## Hard rules

1. No generative LLM anywhere in the pipeline, at build time or runtime.
2. No remote inference. A **local** embedding model is allowed, behind an
   `EmbeddingProvider` interface, loaded once per process and reused.
3. No database connection, no connection string, no query execution.
4. Nothing outside `v2/` changes — not the root `go.mod`, root `go.work`, the Go
   engine, the SDKs, or the config format.
5. Each phase stops at a manual gate. Phases are not combined.
6. Failures get recorded, not hidden.
7. **Config values never appear in default output.** Structure may; values may
   not, and are shown only under `--debug`. See below.

## The redaction contract

Established in Phase 1 so that later phases inherit it rather than negotiate it.
Full statement: [`docs/decisions/006-redaction-contract.md`](docs/decisions/006-redaction-contract.md)
and the package doc in `internal/qferr/sensitive.go`.

| | Default output | `--debug` |
|---|---|---|
| Structure — entity, field, table, column, operator, type, rule names, config positions | shown | shown |
| Values — enum values, value aliases, literals from a user sentence | `<redacted>` | shown |

```text
$ qf-v2 --config broken.json --validate-config
CONFIG_ERROR: config is invalid (1 problem):
  - fields[2] (customer.status): malformed_value_alias: value alias <redacted> points at <redacted>, which is not declared in enumValues
(1 problem(s) withheld a config value; re-run with --debug to see them)
```

The mechanism is a type, `qferr.Sensitive`, whose `String()` renders the
placeholder — so the safe rendering is the one you get by accident, and
revealing is always deliberate. `TestNoFixtureLeaksValuesByDefault` sweeps every
fixture's own values against the rendered output, which catches a value that
somebody forgot to wrap.

**Phase 2 onward:** the semantic corpus is built *from* config values. Holding
them as working material does not make them safe to put in an error.

## Usage

```bash
cd v2

# check a config and exit; since Phase 8 it also lists warnings (shared and unreachable phrases)
go run ./cmd/qf-v2 --config ./testdata/configs/customers.json --validate-config

# see how a sentence is split into candidate spans (no config, no model)
go run ./cmd/qf-v2 --analyze "customers who spent more than 50000 last year"

# retrieve the corpus entries nearest to a phrase
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --search "money spent"

# measure retrieval over a file of query/expected pairs, one warm model
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
  --probe ./testdata/queries/phase2-probes.tsv --strategy surface

# rank every reading of a sentence: accept / ambiguous / reject, with margins
go run ./cmd/qf-v2 --config ./testdata/configs/ambiguity.json --rank "payments with amount over 100"
#   --debug shows every candidate's evidence; --min-score / --min-margin override the policy

# bind a sentence: root, join paths with keys, conditions, aggregates
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --bind "customers who bought laptops"

# compile a hand-written SemanticPlan (no model needed); --debug shows string params
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --compile ./testdata/plans/brief-example.json

# show the plan the planner builds for a sentence (no SQL until Phase 7)
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --plan "orders last month" --now 2026-09-21T12:00:00Z

# ranking vs raw vector top-1 on the ambiguity set
go run ./cmd/qf-v2 --config ./testdata/configs/ambiguity.json \
  --rank-probe ./testdata/queries/phase4-ambiguity.tsv

# cache the index so a restart does not re-embed
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --build-index --index ./corpus.index.json

# natural language → final SQL + parameters (--explain shows the reasoning)
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --query "show customers from India"

# the reviewed Phase 7 query set, as a report
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --e2e ./testdata/queries/phase7-e2e.json

# the Phase 8 adversarial and hardening set
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --e2e ./testdata/queries/phase8-adversarial.json
```

The Phase 9 performance harness is its own binary, so the shipped CLI keeps
its audited flag surface:

```bash
# generate the 100 / 1k / 10k scale configs (exact corpus-entry counts)
go run ./cmd/qf-bench --suite generate

# warm latency, per-stage attribution, embedding vs vector-scan split
go run ./cmd/qf-bench --suite warm --reps 20

# cold start, layer comparison, concurrency 1/10/50/100
go run ./cmd/qf-bench --suite cold
go run ./cmd/qf-bench --suite depth
go run ./cmd/qf-bench --suite concurrency

# re-render benchmarks/reports/performance.md without re-measuring
go run ./cmd/qf-bench --suite report
```

`--search`, `--rank` and the probes need the local embedding model; see
[`embedding_worker/README.md`](embedding_worker/README.md). Without it, pass
`--embedder deterministic` for a non-semantic hashing stand-in that exercises
the same plumbing.

Flags: `--config`, `--query`, `--search`, `--rank`, `--rank-probe`, `--bind`,
`--plan`, `--compile`, `--e2e`, `--now`, `--tz`,
`--min-score`, `--min-margin`, `--analyze`, `--probe`,
`--build-index`, `--index`, `--strategy`, `--top-k`, `--embedder`, `--model`,
`--max-span-tokens`, `--backend`, `--validate-config`, `--explain`, `--debug`,
`--json`, `--version`.

An invalid config reports every problem in one run, each named by rule:

```text
CONFIG_ERROR: config is invalid (3 problems):
  - relationships[0] (bad_entity).to: unknown_relationship_target: endpoint points at unknown entity "invoice" (known: customer, order)
  - relationships[1] (bad_field).to: unknown_relationship_target: endpoint points at unknown field "buyer_id" on entity "order"
  - relationships[2] (bad_cardinality): invalid_cardinality: cardinality "many-to-many" is not supported ...
```

Exit codes: `0` success · `1` the engine refused, with a stable error code on
stderr · `2` the command line was wrong.

`--explain` and `--debug` are accepted now and start producing output in Phase 2,
once there is reasoning worth explaining. Debug output never prints secrets or
raw config values.

## Tests

```bash
cd v2 && go test ./...          # unit and golden tests
cd v2 && go test -race ./...    # the registry is shared across requests unlocked
cd v2 && ./scripts/run-manual-tests.sh
```

## Layout

Directories appear as the phase that needs them arrives, rather than as empty
placeholders.

```text
v2/
├── cmd/qf-v2/            CLI entry point
├── cmd/qf-bench/         Phase 9 performance harness and report renderer
├── internal/
│   ├── config/           typed model, loader, validator, schema registry
│   ├── corpus/           config → searchable semantic corpus
│   ├── nlp/              sentence → tokens, numbers, dates, candidate spans
│   ├── embedding/        Provider interface, local worker, hashing stand-in
│   ├── retrieval/        vector index, cosine search, persistence
│   ├── ranking/          evidence, margin, accept / ambiguous / reject, cover
│   ├── graph/            schema graph, join keys, deterministic path resolution
│   ├── binding/          readings → root, conditions, joins, aggregates
│   ├── plan/             SemanticPlan type, strict JSON, planner (binding → plan)
│   ├── compiler/         plan → checked → grammar nodes → SQL + $n parameters
│   ├── pipeline/         the one orchestration: NL → … → final query, telemetry
│   ├── bench/            latency/resource measurement, scale-config generator
│   ├── qferr/            the closed error-code taxonomy
│   └── isolation/        guard test for the hard rules
├── embedding_worker/     the local Python model sidecar
├── benchmarks/           the benchmark sentence set, the v1 baseline config
│   └── reports/          recorded measurements, incl. performance.md (Phase 9)
├── testdata/configs/     4 valid and 18 invalid config fixtures (ambiguity.json: Phase 4, multipath.json: Phase 5)
├── testdata/queries/     retrieval and ranking probe sets
├── testdata/plans/       hand-written SemanticPlans + reviewed .golden SQL (Phase 6)
├── docs/decisions/       one file per non-obvious technical choice
└── scripts/              manual-gate and benchmark drivers
```

Planned, not yet created: `internal/{planner,compiler,security,evaluation,benchmark}`.
