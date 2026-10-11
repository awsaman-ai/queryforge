# Phase 9 results: performance, concurrency and memory

**Date:** 2026-09-23 · **Status:** measured, **awaiting the manual gate** ·
Method and its justifications: [decision 023](decisions/023-phase-9-benchmark-method.md).
Generated report with every table: [`../benchmarks/reports/performance.md`](../benchmarks/reports/performance.md),
rendered from `benchmarks/scratch/results.json` by `cmd/qf-bench --suite report`.

## The short answer

The deterministic half of the architecture is effectively free. Everything the
experiment was built to test — extraction, ranking, binding, planning,
compilation — costs **under 1 ms per request at every scale measured**. All of
the latency is in the semantic layer, and within it the split is now known:

| | 100 entries | 1,000 entries | 10,000 entries |
|---|---|---|---|
| Warm p50, full pipeline | **6.2 ms** | **14.9 ms** | **103.7 ms** |
| Embedding (model round trip) | 5.5 ms | 5.5 ms | 5.9 ms |
| Vector search (index scan) | 1.2 ms | 11.1 ms | 109.0 ms |
| Everything deterministic | ~0.2 ms | ~0.9 ms | ~0.8 ms |

Embedding is flat in schema size. The vector scan is linear in it, and by
10,000 entries it is 94% of the request. That single fact explains every
budget miss and is the whole of Phase 9's diagnosis.

## Against section 17's targets

| Scale | p50 ≤ 50 ms | p95 ≤ 150 ms | p99 ≤ 300 ms |
|---|---|---|---|
| 100 | met (6.2 ms) | met (12.9 ms) | met (14.4 ms) |
| 1k | met (14.9 ms) | met (35.8 ms) | met (40.3 ms) |
| 10k | **missed** (103.7 ms) | **missed** (260.2 ms) | met (293.2 ms) |

> "Vector lookup should be a small fraction of total latency." Met at 100
> entries (18%), **missed at 1,000** (66%) and **badly missed at 10,000**
> (94%).

> "Compiler should be effectively negligible relative to embedding." Met
> everywhere: 9.6 µs against 5.5 ms at 100 entries, 148 µs against 5.9 ms at
> 10,000.

> "No network on the normal request path." Met, structurally: the worker runs
> offline by default since Phase 8 (16.11) and the isolation guard forbids a
> remote model SDK.

### Why the 10k row misses, in one paragraph

`retrieval.Index.Search` is a full scan: every query vector is compared against
every vector in the index. One sentence produces **10 text spans** (measured,
and constant across the set), so one request scans the whole index ten times:
10 × 25,418 vectors × 384 dimensions ≈ **97.6 million multiply-adds**, which at
the measured 109 ms works out to 1.12 ns each — about what scalar Go on one
core should do. Nothing is pathological; the algorithm is simply O(spans ×
entries × dimensions) and the constant is already reasonable. Making 10,000
entries fast needs a different index (approximate nearest neighbour, or a
coarse filter before the exact pass), not tuning.

## Cold start

| Index | Scale | Ready p50 | Model load | Index step |
|---|---|---|---|---|
| built | 100 | 1.77 s | 1.63 s | 137 ms |
| built | 10k | 12.63 s | 2.12 s | 10.25 s |
| cached | 100 | 1.63 s | 1.63 s | 3.3 ms |
| cached | 10k | **1.88 s** | 1.64 s | 220 ms |

Loading the model is a ~1.6 s floor that no schema size changes. Building a
10,000-entry index costs 10.2 s once (25,418 vectors at ~2,500 vectors/s);
caching it to disk brings the cold start back to 1.9 s, at 62 MB of JSON on
disk. Config load and corpus build are noise (12 ms and 6 ms at 10k).

## Concurrency

One warm engine, one embedding worker, shared by every goroutine.

| Scale | c=1 | c=10 | c=50 | c=100 |
|---|---|---|---|---|
| 100: throughput | 147/s | 183/s | 184/s | 185/s |
| 100: p50 | 6.3 ms | 58.6 ms | 279.1 ms | 519.0 ms |
| 1k: throughput | 59/s | 156/s | 155/s | 155/s |
| 1k: p50 | 15.0 ms | 65.3 ms | 326.9 ms | 606.5 ms |
| 10k: throughput | 8.4/s | 41.0/s | 38.4/s | 36.8/s |
| 10k: p50 | 104.8 ms | 199.2 ms | 1.24 s | 2.44 s |

**Throughput saturates at c=10 and never improves; latency then grows linearly
with concurrency.** That is queueing, and the queue is the embedding worker:
`internal/embedding.Worker` serialises request/response pairs behind a mutex,
because the stdio protocol is one ordered stream. The measured embedding time
per request at 100 entries goes 5.6 ms → 54.1 ms → 244.0 ms → 413.8 ms as
concurrency goes 1 → 10 → 50 → 100, while the vector scan beside it stays at
1.2–1.3 ms and the work per request is unchanged: the extra time is waiting.

Concurrency does buy something between c=1 and c=10 (147 → 183 req/s, 59 → 156
at 1k, 8.4 → 41.0 at 10k), because the Go-side vector scan of one request
overlaps the Python-side embedding of another. Past that, both are saturated.

The worker's own comment calls a pool "a Phase 9 question to answer with
measurements". The measurement says: **one worker caps this architecture at
~185 requests/second on this machine, and no amount of client concurrency
moves that number.** Building a pool was deliberately out of scope this phase
(decision 023); the gate can now scope it against real numbers.

These cells were re-measured twice: once after a data race was found and fixed
in the harness's own chunking wrapper (an unsynchronised call counter, caught
by `-race`; it could not affect a latency, but a number measured by racy code
is not worth defending), and again once every scenario began keeping its raw
per-request samples so `--suite verify` can recompute it. Values moved by
1–5% between runs; no conclusion changed.

## Memory

| Scale | Go heap | Go peak RSS | Worker RSS | Index on disk |
|---|---|---|---|---|
| 100 | 2.6 MB | 41 MB | ~450 MB | 0.7 MB |
| 1k | 14.6 MB | 50 MB | ~450 MB | 6.2 MB |
| 10k | 74.6 MB | 341 MB | ~250–450 MB | 62 MB |

The model process dominates resident memory at every schema size: ~0.4 GB for
MiniLM-L6-v2 under PyTorch, against 2.6 MB of Go heap for a 100-entry index.
Only at 10,000 entries does the Go side become comparable.

Two caveats, both measurement artefacts rather than findings:

- **Worker RSS is unstable on this platform.** Readings for the same process
  ranged 150–456 MB across scenarios; macOS compresses and reclaims pages from
  an idle Python process. Treat it as "a few hundred megabytes", not a precise
  number.
- **Go peak RSS is a process peak, not a scenario peak.** Scenarios sharing a
  process inherit the high-water mark of whatever ran before them, which is why
  the 10k concurrency rows report ~980 MB: that includes the index build.

## Does scale change the answer?

**No.** Every one of the 25 benchmark sentences produced a byte-identical query
(compared by SQL hash) and the identical refusal code at 1,000 and 10,000
entries as at 100 — 0 changed of 25, at both scales. Adding 762 synthetic
entities and 25,000 vectors of competing vocabulary around the reviewed schema
moved no reading.

That is a retrieval-quality result, not a performance one, and it is the
strongest single piece of evidence this phase produced for the architecture:
the ranking rules built in Phases 4–8 hold when the index grows by two orders
of magnitude.

## Against the LLM architecture (v1)

See the report's own section for the table. The honest summary:

- v1 is a **remote** generative call from a **process started per request**;
  v2 is a **warm local process**. The report labels every comparison with both
  categories and never prints a bare multiplier.
- The production example config names `gemini-3.1-flash-lite`, which this
  project's key cannot call (HTTP 403, then 503 from the provider). The
  baseline uses `gemini-3.5-flash-lite`, the fastest model the key could reach.
- **The provider was degraded during the measurement window.** A direct,
  minimal call to the endpoint took 58.7 s; sibling models returned 503. The
  v1 numbers are therefore a snapshot of one bad window, not a fair
  characterisation of the LLM architecture at its best, and are labelled as
  such. They should be re-run on a healthy provider before being quoted; the
  harness does it in one command.

## Findings

| # | Finding | Evidence | Status |
|---|---|---|---|
| P01 | The vector index is a full scan, so request latency is linear in schema size and dominates past ~1,000 entries | 1.2 / 11.1 / 109 ms of scan at 290 / 2,576 / 25,418 vectors, 10 scans per request | **Open for the gate.** Diagnosed, not optimised (brief rule 13) |
| P02 | One embedding worker serialises every request, capping throughput at ~185 req/s regardless of client concurrency | throughput flat from c=10 to c=100 at all three scales; embed time per request grows 5.6 → 413.8 ms while the scan beside it stays at 1.3 ms | **Open for the gate.** A pool was explicitly out of scope |
| P03 | The worker refuses a batch over 2,048 texts and `retrieval.Build` embeds in one call, so no schema past ~800 entries could be indexed at all | `batch of 2576 exceeds the 2048 limit` on the first 1k run | **Worked around in the harness** (`bench.Batched`); where the real fix belongs is a gate decision |
| P04 | Ten index scans per sentence — one per text span — multiply the scan cost | measured constant 10.0 scans/request across scales | **Open for the gate.** Deduplicating identical spans or batching the scans would divide the cost |
| P05 | Deterministic stages cost under 1 ms and do not grow with schema size | extract 4 µs, rank 0.34 ms, bind 20 µs, plan 37 µs, compile 145 µs at 10k | No action; this is the architecture working |
| P06 | Retrieval quality is unchanged at 100× the schema size | 0 of 25 answers changed at 1k and 10k | No action |

## For the gate

1. **P01, the full scan.** Accept 10,000-entry latency as measured, or scope an
   index change (approximate nearest neighbour, coarse pre-filter, or
   dimensionality reduction) as its own phase of work. Phase 9's brief says
   diagnose before optimising, so nothing was changed.
2. **P02, the worker pool.** The measurement the code comment asked for exists
   now. Scope a pool, or record the ~185 req/s ceiling as the architecture's
   throughput and move on.
3. **P03, the batch limit.** Decide where chunking belongs: the worker's
   protocol, `retrieval.Build`, or a provider wrapper in the shipped path. It
   currently exists only in the benchmark package, which means the CLI still
   cannot build an index for a 1,000-entry schema.
4. **P04, ten scans per sentence.** Worth its own look before any index change:
   it is a 10× multiplier available without changing the index structure.
5. **The v1 baseline.** Re-run on a healthy provider before the comparison is
   quoted anywhere, or accept it as labelled.
6. **The budgets themselves.** 100 and 1,000 entries meet all three targets.
   Confirm whether 10,000 entries is a size this experiment needs to serve at
   all, or whether the realistic ceiling is 1,000.

## How to check it

```bash
cd v2
go test ./internal/bench/ ./cmd/qf-bench/        # the harness's own arithmetic
go run ./cmd/qf-bench --suite generate           # 100 / 1k / 10k configs, exact counts
go run ./cmd/qf-bench --suite warm --reps 20     # warm latency at every scale
go run ./cmd/qf-bench --suite depth --reps 10    # retrieval / +ranking / full
go run ./cmd/qf-bench --suite concurrency        # 1 / 10 / 50 / 100
go run ./cmd/qf-bench --suite cold --cold-reps 5 # cold start, built and cached
go run ./cmd/qf-bench --suite verify             # recompute every published number from its samples
go run ./cmd/qf-bench --suite report             # re-render without re-measuring
scripts/run-manual-tests.sh                      # phases 0-9
```

The v1 baseline needs a prebuilt production binary and a key in the
environment:

```bash
(cd ../.. && go build -o /tmp/queryforge-v1 ./cmd/queryforge)
QF_API_KEY=... go run ./cmd/qf-bench --suite v1 --v1-bin /tmp/queryforge-v1 \
  --v1-cases 15 --v1-reps 3 --results /tmp/v1.json
go run ./cmd/qf-bench --suite merge --merge-from /tmp/v1.json
go run ./cmd/qf-bench --suite report
```
