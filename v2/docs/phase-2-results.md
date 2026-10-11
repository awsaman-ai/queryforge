# Phase 2 results — semantic corpus and vector index

**Date:** 2026-09-21 · **Status:** built and self-verified; **awaiting the
user's manual gate**.

## Environment

| | |
|---|---|
| OS / CPU | macOS 26.5.2, arm64, Apple M5, 10 cores, 16 GB |
| Go | 1.26.4 · Python | 3.13.13 |
| Embedding model | `sentence-transformers/all-MiniLM-L6-v2` |
| Model revision | `1110a243fdf4706b3f48f1d95db1a4f5529b4d41` |
| Dimensions | 384, L2-normalised |
| sentence-transformers / torch | 5.1.2 / 2.14.0 |
| Vector index | in-memory dense, cosine, full scan |
| Config under test | `testdata/configs/commerce.json` — 3 entities, 15 fields, 2 relationships |
| Corpus | 55 entries · 173 vectors (surface) / 55 vectors (passage) |
| Library commit | `51df283` |

Weights are not committed. The revision hash is what makes these numbers
traceable to exact weights rather than to a name upstream can re-upload.

## Headline

**Semantic retrieval works, and the margin over a lexical baseline is real but
smaller than the architecture's premise suggests.**

38 probe queries, 33 with a committed expected corpus entry:

| Configuration | Top-1 | Top-3 | Top-1 score min / median / max |
|---|---|---|---|
| **Surface vectors, MiniLM** | **82%** (27/33) | 91% | 0.586 / 0.861 / 1.000 |
| Passage vectors, MiniLM | 79% (26/33) | **94%** | 0.144 / 0.475 / 0.789 |
| Trigram hashing (lexical baseline) | 70% (23/33) | 85% | — |

Full tables, including every miss with its top three competitors:
`benchmarks/reports/phase2-probe-{surface,passage,lexical-baseline}.md`.

Reproduce:

```bash
cd v2 && go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json \
  --probe ./testdata/queries/phase2-probes.tsv --strategy surface --top-k 5
```

## The central bet, tested directly

The corpus exists to close vocabulary mismatch. These queries share **no token**
with the field they must retrieve:

| Query | Retrieved | Score |
|---|---|---|
| `money spent` | `field:customer.total_spent` | 0.814 |
| `how much a customer paid` | `field:customer.total_spent` | 0.694 |
| `when the account was opened` | `field:customer.created_at` | 0.780 |
| `things we sell` | `entity:product` | 0.608 |
| `what the customer is called` | `field:customer.name` | 0.775 |

That is the premise of the experiment behaving as advertised. It is also exactly
where the lexical baseline fails, which is why the +12 point gap is the number
that matters rather than the 82% on its own.

## Surface beats passage, and not on accuracy

Accuracy alone is near a coin flip — surface wins Top-1 by three points, passage
wins Top-3 by three. **The score distribution decides it.**

Passage scores are compressed into the bottom of the range. `at least` retrieves
`gte` at **0.079** under passage and **1.000** under surface. Phase 4's entire
admission policy is an absolute threshold plus a top1-vs-top2 margin (section
12), and neither can be calibrated on scores that cluster between 0.1 and 0.6
with the correct answer sometimes at the bottom of the band.

The mechanism is visible in the failures. A passage for an operator reads
`"gt. Also known as more than, greater than, above, over, exceeds."` — the
canonical name is an identifier, not language, and it sits at the front dragging
the vector toward noise. Worse, `purchase value` retrieves three country aliases
and misses `total_spent` entirely under passage, while scoring an exact 1.000
under surface. Short entries — operators, aggregations, values — are where
concatenation hurts most, and they are a third of this corpus.

Both strategies stay implemented. This is a property of *this* model and *this*
corpus; Phase 9's 1k and 10k schema runs may shift it, and Phase 10's ablation
needs the alternative to exist. See
[decision 008](decisions/008-embedding-strategy.md).

## Every miss, and what it points at

Six of 33. None are random; five point at layers that do not exist yet, which is
the most useful thing Phase 2 could have produced.

**1. Multi-span queries collapse onto one span — Phase 3.**

```
client location  →  expected customer.country
  1. entity:customer      0.658  "client"
  2. field:customer.name  0.621  "client name"
  3. field:customer.country 0.548 "location"
```

`recent customer` fails the same way and does not retrieve `created_at` at all;
`recent` matches the pattern entry and `customer` matches the entity. The whole
sentence is being embedded as one string. Phrase extraction is Phase 3, and
these two queries are its acceptance test.

**2. Relationships lose to entities — Phase 4.**

```
placed an order  →  expected relationship:customer_orders
  1. entity:order           0.783
  2. field:order.placed_at  0.781
  3. field:order.customer_id 0.741
```

`customers who bought something` misses the same way, at rank 5. A verb maps to
an edge, but entity and field entries carry more text and win on raw similarity.
This is precisely what section 12 means by "pure semantic similarity can produce
plausible but incorrect candidates": kind compatibility has to be a ranking
signal, not an afterthought.

**3. A genuine tie the retrieval layer cannot break — Phase 4.**

```
the highest ones  →  expected pattern:top_n
  1. aggregation:max  0.797  "highest"
  2. pattern:top_n    0.797  "highest"
```

Identical to three decimal places, because both entries configure the literal
phrase "highest". The tie-break by entry ID is deterministic and arbitrary — it
is doing its job, and its job is not to be right. This is the ambiguity case
Phase 4 must either resolve with structural evidence or refuse.

**4. One expected miss.** `cancelled accounts` → `suspended` at rank 3. No
config declares "cancelled", so the model is bridging the gap unaided and
getting close. Recorded as the honest limit of what the corpus can do without
the vocabulary being declared.

## False positives worth keeping in the report

Section 10 says not to claim semantic understanding from a handful of successes.
The exploratory queries — deliberately vague, no expected answer — show what the
layer does when the sentence underdetermines the target:

| Query | Top-1 | Score | Reading |
|---|---|---|---|
| `amount` | `field:order.amount` | 1.000 | Confident, and **wrong to be confident**: `customer.total_spent` and `product.price` are equally plausible. An exact-token match scores 1.000 and buries the ambiguity. |
| `date` | `field:order.placed_at` | 0.726 | Same shape, less confident. `customer.created_at` is as likely. |
| `name` | `field:customer.name` | 1.000 | `product.name` is equally valid. |
| `value` | `field:customer.total_spent` | 0.682 | Plausible but arbitrary. |

**A 1.000 similarity is not evidence of correctness.** It means a surface form
matched exactly, and an exact match on a word that several fields share is
precisely when confidence is least warranted. A threshold policy built on
absolute score alone would accept every one of these. That is the strongest
argument in this report for the top1-vs-top2 margin that section 12 requires.

## Latency

Not a Phase 9 benchmark — no percentiles, one machine, one corpus size. Recorded
because it is cheap and bounds expectations.

| | |
|---|---|
| Model load (cold, per process) | ~5.5 s |
| Index build, 55 entries / 173 vectors | ~180 ms |
| **Warm query embed, mean of 38** | **2.7 ms** |
| Vector search, 173 vectors | < 100 µs |

The warm path is dominated by embedding, and vector search is nowhere near the
bottleneck — which is the evidence for *not* reaching for an ANN index yet
(section 22, rule 7).

The architecture that makes 2.7 ms possible is the one the brief mandates: one
worker process, model loaded once, reused. `TestWorkerReusesOneProcess` asserts
it by pid, because a worker that respawned per request would still return
correct vectors and no correctness test would notice.

## What was built

| Package | Role |
|---|---|
| `internal/corpus` | config → corpus entries, deterministic, needs no model |
| `internal/embedding` | `Provider` interface, worker client, hashing stand-in |
| `internal/retrieval` | index, cosine search, persistence, staleness and corruption checks |
| `embedding_worker/` | the Python sidecar, `worker.py` |
| `cmd/qf-v2` | `--search`, `--build-index`, `--probe` |

31 Go files, ~7,480 lines. 125 test functions, 192 cases, passing under `-race`.

Corpus generation is deliberately separate from embedding: it is pure and fully
testable without weights, which means a change in retrieval results can be
attributed to the model rather than to the text it was handed.

## Redaction under retrieval

The contract from Phase 1 meets its first real test here. Value aliases and enum
values **are** embedded — that is how `India` finds the country field — so the
corpus now holds config values as ordinary working material. That is exactly the
situation decision 006 anticipated.

Every value surface carries `Sensitive: true` from the corpus builder, through
the index, through persistence, to the renderer:

```text
rank  score  kind         id                              matched
3     0.428  VALUE_ALIAS  value:customer.status:enum:…    <redacted>

(1 match(es) matched a config value; re-run with --debug to see them)
```

The entry ID is shown because it is structure — it names which config object
matched. The matched text is not.

## Deliberate non-results

- **`--query` still compiles nothing.** Retrieval is not translation. Phrases
  (3), ranking (4), binding (5) and a compiler (6) all stand between.
- **No ranking.** `--search` reports raw cosine similarity. The multi-signal
  scoring of section 12 is Phase 4, and the misses above are its brief.
- **No thresholds.** Nothing is accepted or rejected yet, so nothing can be
  wrongly accepted yet.
- **No ANN index.** A full scan of 173 vectors is under 100 µs. FAISS becomes a
  question when Phase 9 says so.
- **One corpus.** 55 entries, hand-written, by the same person who wrote the
  probes. The 1k and 10k runs are Phase 9; these numbers flatter everything.

## Failures and surprises

**The gate script scored itself 1/33.** Extracting the accuracy with
`grep -o 'Top-1: [0-9]*/[0-9]*' | grep -o '[0-9]*' | head -1` returns `1` — from
the label `Top-1`, not from the score. It had been reporting a catastrophic
regression that did not exist.

It now parses the numerator, the denominator and the percentage with one `sed`
capture, and checks Top-3 as well. Two further points came out of fixing it. The
denominator is read from the report rather than hardcoded, so adding a probe to
the TSV cannot silently turn the check into a comparison against a stale total.
And the percentage is read from the report rather than recomputed — shell
integer division truncates, so recomputing turned 27/33 into **81%** while the
report and this document both say **82%**. A gate that disagrees with the
document it guards is worse than no gate.

**The determinism check failed on its own timings.** It compared full JSON
output across processes, which includes `embedMs` and `searchMs`. Those
legitimately vary — the check was wrong, not the code.

The first fix filtered those two fields out by name. That is a denylist, and a
timing field added in Phase 4 would have silently defeated it. It now projects
the fields that must be stable — query, strategy, corpus shape, model identity,
and every match's rank, score, kind, id, matched text and redaction flag — and
compares those. An allowlist cannot be defeated that way.

Both checks were then verified to be capable of failing, not just passing: the
accuracy parse was run against real output and confirmed to yield 27/33/30, and
the determinism projection was confirmed to differ between two different
configs. A check that cannot fail is not a check.

**Passage vectors failed in a way accuracy hid.** 79% versus 82% looks like
noise. The score distribution is where the real difference is, and a comparison
that only recorded Top-1 would have chosen almost arbitrarily between two
strategies that behave very differently under a threshold policy.

## Gate

```bash
cd v2 && ./scripts/run-manual-tests.sh
```

46 checks, all passing. Sections 7 and 8 are Phase 2; section 8 skips cleanly
when the embedding venv is absent.

Latest run of section 8:

```text
PASS  'money spent' retrieves customer.total_spent with no shared token
PASS  the real model reports itself semantic
PASS  probe Top-1 27/33 = 82%, Top-3 30/33 = 91% (recorded: 27/33 = 82%, 30/33 = 91%)
```

Phase 3 does not begin until the user confirms.
