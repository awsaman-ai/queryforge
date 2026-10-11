# QueryForge V2 — Non-LLM Experiment: Status

**Snapshot date:** 2026-10-11 · **Branch:** `v2-non-llm-experiment` · **Module:** `queryforge.local/v2experiment`

## The question

Can natural language → SQL work with local embeddings plus deterministic reasoning, with
**no LLM in the loop**, while never producing a silently wrong query? The pipeline is:
extract → retrieve → rank → bind → SemanticPlan → compile (PostgreSQL SELECT only).

`TODO.md` is the live phase checklist, `docs/phase-N-results.md` holds each phase's
measurements, and `docs/decisions/001…023` records every locked design decision.

## Where it stands

| Phase | Scope | State | Gate |
|---|---|---|---|
| 0 | Scope / baseline, CLI contract | Closed | passed 2026-09-20 |
| 1 | Config model + validation (23 rules) | Closed | passed 2026-09-20 |
| 2 | Semantic corpus / vector index | Closed | passed 2026-09-21 (Top-1 82%, Top-3 91%) |
| 3 | NLP extraction (numbers, dates, operators) | Closed | passed 2026-09-21 |
| 4 | Candidate ranking | Closed | passed 2026-09-21 (81/81) |
| 5 | Binding / schema graph | Closed | passed 2026-09-21 |
| 6 | Deterministic compiler | Closed | passed 2026-09-21 |
| 7 | End-to-end | Closed | passed 2026-09-22 (146/146) — first gate kept OPEN |
| 8 | Security hardening | Closed | passed 2026-09-23 (170/170) — three gate rounds |
| **9** | **Performance benchmarking** | **Built, awaiting manual gate** | **not yet passed** |
| 10 | Evaluation (300–500 queries, ablations, Go/No-Go) | Not started | blocked on Phase 9 gate |
| 11 | Optional backends | Not started | blocked on Phase 10 approval |

**Next action (yours):** review `benchmarks/reports/performance.md` and
`docs/phase-9-results.md`, re-run `scripts/run-manual-tests.sh`, then pass or reopen the
Phase 9 gate. Nothing in Phase 10 starts before that.

## Headline numbers

- **Zero silently wrong accepted queries** in every corpus (ranking probes, Phase 7 query
  set, Phase 8 adversarial set).
- Ranking vs raw top-1 on the ambiguity probes: **33/34 vs 20/34** (ambiguity.json),
  **17/18 vs 13/18** (commerce).
- Phase 7 query set: 71/77 with the remaining misses being known, safe refusals.
- Phase 8 adversarial set: **93/93**; mutation testing **41/41** caught; fuzz **1,418,328**
  inputs clean; race detector clean across 19 packages; **80/80** outputs parse as a single
  valid PostgreSQL SELECT.
- Manual gate script: **170/170** checks, 0 skipped (before Phase 9 added section 23, 19 checks).

## Phase 9 measurements (awaiting gate)

| Schema size | Vectors | p50 | p95 | p99 | Go heap |
|---|---|---|---|---|---|
| 100 entries | 290 | 6.2 ms | 12.9 ms | 14.4 ms | 2.6 MB |
| 1k entries | 2,576 | 14.9 ms | 35.8 ms | 40.3 ms | 14.6 MB |
| 10k entries | 25,418 | 103.7 ms | 260.2 ms | 293.2 ms | 74.6 MB |

The embedding model process is ~0.4 GB at every scale. Cold start: 1.63 s model-load
floor; a cached index brings a 10k-entry schema to ready in 1.88 s (12.6 s uncached).
Retrieval quality holds at 100× schema size (0 of 25 sentences changed answer).

**Findings the gate must decide (none were optimised — Phase 9 was measurement only):**

- **P01** — the vector index is a full scan, so latency is linear in schema size (94% of a 10k request).
- **P02** — one embedding worker serialises every call; throughput saturates near 185 req/s from concurrency 10.
- **P03** — the worker's 2,048-text batch limit means the shipped CLI cannot index a ~1k-entry schema; chunking exists only in the harness.
- **P04** — ten index scans per sentence multiply the scan cost.
- **P05** — the deterministic stages add under 1 ms: they are free.
- **P06** — retrieval quality is stable across scale.

## Standing rules (from the gates)

- Unexplained words are **refused**; there is no harmless-word list.
- Values need literal evidence; similarity only discovers, never decides.
- Currency / percent / unit values are refused until per-field unit metadata exists.
- A new refusal must carry its own true reason in the user-facing message.
- Config values are redacted in errors by default and revealed only under `--debug`.
- No phrase-specific rules — domain-agnostic concepts only.

## Known limits (accepted, documented)

- **X11** "show customer order date" — safe refusal (cover tie + to-many listing rule); decision 020.
- **G36** "list price of products" — compiles; intentional, recorded in 022 §7.
- Refused as costs of earlier rules: `signup date`, `show product cost price`, `show customers list`,
  exponent notation (`1e5`), ordinals (`30th`), any `;` or `--` in a request.
- English only. PostgreSQL SELECT only. `LIKE` is case-sensitive.

## Deferred / open

- Worker pool, ANN index, chunked indexing in the shipped CLI (P01–P03; gate decision).
- Recalibration of ranking thresholds (0.70 / 0.10) on a real evaluation set — Phase 10.
- Per-field currency/unit metadata, "what is …" questions, temporal-aggregation patterns — unscheduled.
- Overlapping-phrase cover selection / relationship-aware resolution (X11's fix scope) — its own scoped change.

## What is NOT in this branch

Ignored by `v2/.gitignore`, reproducible, and deliberately not committed:

- `embedding_worker/.venv/` — recreate from `embedding_worker/requirements.txt`.
- `embedding_worker/models/` — model weights; pinned to MiniLM-L6-v2 @ `1110a243`.
- `*.index.json`, `/indexes/` — built vector indexes.
- `benchmarks/scratch/` — raw benchmark scratch (~70 MB); the committed reports live in `benchmarks/reports/`.

## Provenance

`v2/` was untracked on `main` until this branch. It is isolated by rule: nothing outside
`v2/` was edited for this track. The `/v1/noLLM/tosql` test app (`go_test_qf`, outside this
repo) links it through a `replace` directive.
