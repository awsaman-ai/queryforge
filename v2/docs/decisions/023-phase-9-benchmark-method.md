# 023 — Phase 9 benchmark method

**Status:** accepted · **Phase:** 9 · **Date:** 2026-09-23

Phase 9 asks whether the non-LLM architecture is meaningfully faster and more
predictable than the LLM one, and section 17 lists what to measure. How it is
measured decides whether the answer means anything, so the choices are recorded
here before the numbers are read.

Three of them are the user's, taken at the Phase 9 kickoff:

- **A small live v1 run**, not a deferred comparison and not a full one.
- **Measure the serialised embedding worker as built**; do not build a worker
  pool this phase. Brief rule 13: measure before optimising.
- **"Schema entries" means corpus/index entries**, with the configured
  entity and field counts reported alongside.

## §1 — A separate binary, not a mode of `qf-v2`

`cmd/qf-bench` imports the same internal packages and assembles the engine in
the same order as `cmd/qf-v2`, but it is its own command.

`qf-v2`'s flag surface is audited Phase 0–8 code: `--query`, `--rank`,
`--bind`, `--plan`, `--e2e` and the security behaviour around them are what the
gates checked. Adding a benchmark mode there would mean editing a closed record
to take a measurement. Nothing in the shipped path depends on `qf-bench`, and
the isolation guard still applies to it.

The cost is duplication: `benchRetriever` is a copy of the CLI's
`engineRetriever`. That is deliberate — exporting the CLI's copy would change a
Phase 7 signature to serve a benchmark.

## §2 — Scale configs are a reviewed config with synthetic padding

Nothing with 10,000 schema entries exists to hand, so `internal/bench.Generate`
produces one. Two properties matter more than realism:

1. **The same sentences run at every scale.** A generated config is
   `testdata/configs/commerce.json` — the reviewed Phase 7 schema — with
   synthetic entities appended. The core entities, fields, values and
   relationships are byte-identical at every scale, so a latency curve across
   sizes is a curve for one fixed set of questions.
2. **Padding cannot change the core's meaning.** Synthetic entities join only
   to each other, never to the core, so the graph the binder walks from a core
   root at 10,000 entries is the graph it walks at 100. A changed answer at
   scale is then retrieval noise from a bigger index — a finding — rather than
   the generator moving the goalposts. `TestGeneratePreservesTheBaseSchema`
   pins both properties.

Generation is deterministic (same target, same bytes) and the entry count is
verified with the real `corpus.Build`, not a formula. The first version walked
the padding down one entity at a time, ran out of iterations and silently
produced 9,998 entries for a 10,000 target; it now moves in proportional steps
and closes the remainder with single filler fields.

Targets are hit exactly: 100, 1,000 and 10,000 corpus entries, which are 290,
2,576 and 25,418 index vectors and 6/43, 75/529 and 768/5,371 configured
entities/fields.

## §3 — Percentiles are nearest-rank, and under-sampling is marked

The p-th percentile is the value at rank `ceil(p/100 × N)` of the sorted
samples. Every number printed is therefore a latency that actually happened,
which is what a tail budget is about; an interpolated p99 is a number no
request ever took.

The consequence is stated rather than hidden: p99 needs 100 samples and p95
needs 20 before they differ from "the slowest request". `bench.MinSamplesFor`
defines it, `Summary` carries `P95UnderSampled`/`P99UnderSampled`, and the
report marks such a cell with `*`.

## §4 — Telemetry comes from `pipeline.Stats`, never from stdout

The carried-forward obligation from decision 013 is discharged here: per-stage
latency is decoded from `pipeline.Stats` — the structured, redacted telemetry —
and the harness never parses `--rank` or `--query` output. The raw results
document holds ids, counts, timings and a hash of the SQL; it holds no
sentence text, no config values and no generated query, so it is publishable
under the same rules as the telemetry itself.

The SQL hash exists for one purpose: proving that the same sentence produced
the same query at a larger scale without reproducing the query.

## §5 — Embedding and vector search are timed separately

`Stats` reports one `RetrieveMs` covering both the model round trip and the
index scan, but section 17 asks for "embedding latency" and "vector retrieval
latency" as separate metrics, and its target — "vector lookup should be a small
fraction of total latency" — cannot be checked without the split. The harness's
own retriever times each half and counts the scans per request. It also records
how many index scans one sentence costs (one per text span), because that is
the multiplier on the scan cost.

This is measurement in the harness, not a change to the engine.

## §6 — Chunked embedding lives in the benchmark package

The worker refuses a batch over 2,048 texts (`MAX_BATCH` in `worker.py`) and
`retrieval.Build` embeds the whole corpus in one call by design. Every scale
past the smallest therefore failed to index at all: a 1,000-entry schema is
2,576 surfaces.

`internal/bench.Batched` wraps a provider and splits large calls. It is in the
benchmark package on purpose: this is a Phase 2 limitation found by Phase 9
measurement, and where the fix belongs — the worker, `retrieval.Build`, or an
explicit chunking provider in the shipped path — is a decision for the Phase 9
gate, not something to slip into closed code while benchmarking. Chunking
cannot change a vector: each text is embedded independently, and
`TestBatchedSplitsAndPreservesOrder` pins the seam.

## §7 — The v1 baseline is out of process, with its own config

v2 may not import the production library, so the baseline drives a prebuilt
`cmd/queryforge` binary over the SDK's stdio protocol, passed in with
`--v1-bin`. Nothing outside `v2/` is edited; the binary is built to a scratch
path.

Three consequences, all reported rather than smoothed over:

- **v1 is process-per-request.** It is not a daemon, so every request pays a
  process start. The floor is measured separately with the model-free
  `version` op and printed, so a reader can see how much of the gap is process
  start and how much is the model.
- **v1 is single-entity.** Only sentences a single-entity schema can answer are
  sent — the `"v1": true` cases in `benchmarks/queries.json`. Section 17:
  do not compare unsupported features against systems that support them.
- **The model had to be substituted.** The repo's example config names
  `gemini-3.1-flash-lite`, which this project's key cannot call (403, then 503
  from the provider). The benchmark config names `gemini-3.5-flash-lite`, the
  fastest model the key could actually reach, and the report names it.

The benchmark's v1 config (`benchmarks/v1-commerce.config.json`) describes the
same customer schema in v1's format. It is a v2 file; the production repo's
configs are untouched.

## §8 — What a run is allowed to claim

- Cold and warm are never averaged together, and the index-built and
  index-cached cold paths are separate rows.
- Refusals are summarised apart from accepted queries: a refusal stops early,
  so folding it in would flatter the median.
- A missed budget is printed as **missed** with the number beside it. Section
  17 says to diagnose before optimising, and a missing row cannot be diagnosed.
- A run from the deterministic (non-semantic) provider is stamped "Not a
  research result" in the report.
- The v1 comparison is labelled by category (warm local vs process-per-request
  remote) wherever a ratio appears.

## What would cause us to revisit it

- A worker pool (if the gate scopes one): the concurrency scenarios would need
  a pool-size axis, and `WorkerPIDStable` would become "the pool did not grow".
- An approximate index (HNSW/IVF): the scan half of §5's split stops being the
  whole story and recall becomes a reported metric beside latency.
- A v1 baseline taken on a healthy provider: today's numbers are a snapshot of
  one degraded window and the report says so.
