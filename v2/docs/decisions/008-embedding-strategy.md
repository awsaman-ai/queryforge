# 008 — Surface vectors over passage vectors, on evidence

**Status:** accepted · **Phase:** 2 · **Date:** 2026-09-21

## Decision

Default to `StrategySurface`: embed every surface form of a corpus entry
separately — the name, the description, each synonym, each phrase — and score an
entry as the best of its own vectors.

Both strategies are implemented and both stay implemented, because the choice
was made on measurements and the measurement should be repeatable.

## The two options

**Passage** concatenates an entry into one block — name, description, synonyms,
type, entity — and embeds it once. One vector per entry, simplest index.

**Surface** embeds each piece separately. More vectors, a max-pool at query
time, and no dilution of a rare synonym by the rest of the entry.

## Evidence

38 probe queries against `commerce.json` (55 entries), MiniLM-L6-v2. Full
tables in `benchmarks/reports/`.

| | Top-1 | Top-3 | Top-1 score: min / median / max |
|---|---|---|---|
| Surface | **82%** (27/33) | 91% | 0.586 / 0.861 / 1.000 |
| Passage | 79% (26/33) | **94%** | 0.144 / 0.475 / 0.789 |
| Lexical baseline (trigram hashing) | 70% (23/33) | 85% | — |

Accuracy alone would be close to a coin flip — surface wins Top-1 by three
points, passage wins Top-3 by three. **The score distribution decides it.**

Passage scores are compressed into the bottom half of the range. `at least`
retrieves `gte` at **0.079**; under surface the same query scores **1.000**. A
confidence threshold and a top1-vs-top2 margin are the entire admission policy
of Phase 4 (section 12), and they cannot be calibrated on scores that cluster
between 0.1 and 0.6 with the correct answer sometimes at the bottom of that
band.

The mechanism is visible in the failures. Passage renders an operator entry as
`"gt. Also known as more than, greater than, above, over, exceeds."` — the
canonical name is an identifier, not language, and it sits at the front of the
passage dragging the vector toward noise. Value entries fare worse still:
`purchase value` retrieves three country aliases and misses `total_spent`
entirely under passage, while scoring an exact 1.000 under surface. Short
entries — operators, aggregations, values — are exactly where concatenation
hurts most, and they are a third of the corpus.

## Why keep passage at all

Three reasons. The comparison should be re-runnable when the model or the corpus
changes, because this result is a property of *this* model and *this* corpus and
may not survive either. Phase 10's ablation study needs the alternative to still
exist. And a larger schema may shift the balance: passage's flat Top-3 advantage
suggests it is retrieving a broader neighbourhood, which could matter when
there are a thousand fields rather than fifteen.

## What this does not show

The lexical baseline scores 70% Top-1. Embeddings buy **+12 points**, which is
real and smaller than the architecture's premise might lead one to assume. That
number belongs in the Phase 10 ablation as a finding, not as a footnote — and
the corpus here is small and hand-written, which flatters both.

## Revisit if

- Phase 4's ranking changes what the retrieval layer is being asked for.
- Phase 9's benchmark matrix (1k and 10k schema entries) shifts the balance;
  surface produces roughly 3× the vectors, and that ratio grows with how richly
  a config is described.
- The model changes. These are MiniLM's numbers, not embedding's numbers.
