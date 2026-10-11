# 011 — The ranking formula: evidence, margin, and a confidence that is not similarity

**Status:** accepted (provisional numbers) · **Phase:** 4 · **Date:** 2026-09-21

## Decision

Rank each span's retrieval candidates by an additive, bounded, named formula,
and decide from the **margin** between the top candidate and the best
*disagreeing* runner-up, not from any absolute score.

```text
score(c)    = similarity(c) + Σ weight_i × evidence_i(c)     evidence_i ∈ [-1, +1]

decide(span):
  no eligible candidate          → reject     NO_SEMANTIC_MATCH
  top.score < minScore           → reject     LOW_CONFIDENCE
  margin < minMargin, or a tie   → ambiguous  AMBIGUOUS_MATCH
  otherwise                      → accept

confidence  = min(1, margin / (2 × minMargin))  when accepted, else 0
```

Three numbers are kept separate on every candidate, as the user required:

| | What it is | What it is not |
|---|---|---|
| **similarity** | the retrieval cosine, untouched | confidence. A 1.000 is an exact surface match, which is what a shared word like "name" produces |
| **score** | similarity plus weighted evidence | comparable across spans, or a probability |
| **confidence** | a property of the *decision*, from the margin | a property of any one candidate |

## The signals

| Signal | Weight | +1 | −1 |
|---|---|---|---|
| lexical | 0.05 | span is exactly the matched surface (folded) | never negative. 0.5 when every *surface* word appears in the span |
| type | 0.10 | the attached value fits the field type | the attached value does not fit |
| operator | 0.10 | the attached operator is allowed on the field, or the operator fits the attached value | not allowed |
| entity | 0.15 | the sentence names this candidate's entity elsewhere | — |
| relationship | 0.10 | a relationship whose two ends are both named | fields: unreachable (−1), or one join away (−0.5). relationships: neither end named |
| context | 0.15 | top_n with a count beside it. time_window with a date beside it | an aggregation with a count beside it |
| span | 0.20 | — | carried by a shorter span, starts or ends on a function word, swallows a number/date, or decomposes into shorter accepted spans |

Plus two **vetoes**: a field that is neither selectable nor filterable (a join
key), and a non-filterable field with an operator attached.

Plus four **agreements**. These readings are not rivals, so they cannot make a
span ambiguous: an entry with itself, a value with its field, two values of one
field that share a stored value, and the count pattern with the count
aggregation. `top_n` vs `max` is deliberately *not* an agreement.

## Why margin, not threshold

Measured on the two sets (48 scored rows, real model, `minScore 0.70`), in three
layers:

| What decides | Silently wrong |
|---|---|
| raw vector top-1 | 14 + 5 |
| top-1 gated by an absolute threshold only | 13 + 4 |
| ranking evidence + the tie rule, `minMargin 0` | 1 + 1 |
| the same, with any `minMargin` from 0.03 to 0.30 | **0 + 0** |

The threshold does almost nothing on its own, because the dangerous cases score
1.000. Most of the remaining work is done by the **tie rule** (an exact tie is
ambiguous under every policy) and by the structural evidence. The margin closes
the rest: the near-ties that evidence narrows but does not separate.

An earlier sweep, run before the tie rule existed, showed 9 + 3 silent wrongs
at `minMargin 0`, and a draft of this record quoted that figure. It no longer
describes the code. The current grid is in
`benchmarks/reports/phase4-calibration-sweep.md`.

## Why two passes

Context ("the sentence mentions orders") has to come from somewhere, and taking
it from the final decisions would be circular. Pass 1 decides every span on
local evidence: similarity, lexical, span quality. What pass 1 establishes
becomes the context for pass 2. The number of passes is fixed rather than run to
a fixpoint, because a fixpoint's result can depend on evaluation order.

Context is **conservative**, by two rules found through measurement (below):
only clean spans contribute, and an ambiguous span contributes *every* entity
among its live readings.

## Why a cover

Phase 3 kept overlapping spans on purpose (decision 010) and left the choice to
Phase 4. The cover is greedy and deterministic. Candidates are ordered
**clean-accepted → clean-ambiguous → defective-accepted → defective-ambiguous**,
and within each group by confidence (or strength), then length, then position.
Span quality is the primary key: a defective reading may fill tokens nothing
clean explains, but it may never overrule a clean reading, including a clean
"ambiguous".

## Options considered

- **Absolute threshold only.** Rejected on measurement: 13 + 4 silent wrongs.
- **Learned weights / a trained reranker.** Out of scope by instruction: no ML
  training, and nothing to train on until Phase 10's dataset exists.
- **Multiplicative scoring** (similarity × compatibility). Harder to explain: a
  zero anywhere erases everything, and "how much did the entity signal move this"
  has no one-line answer. Additive with bounded signals does.
- **Iterate context to a fixpoint.** Rejected: its answer can depend on order,
  and "two passes" is a rule a person can check by hand.
- **Resolve ties by a secondary key.** Rejected: that is exactly Phase 2's
  arbitrary id tie-break wearing a different hat. A tie is ambiguous under every
  policy, including `minMargin = 0`.

## Trade-offs

- **The numbers are provisional.** They were calibrated on 48 hand-written rows
  by the person who wrote the ranker. Phase 10 recalibrates on a real
  evaluation set. The sweep is in `benchmarks/reports/phase4-calibration-sweep.md`.
- **Attachment is a heuristic.** "The value to the right, within 4 tokens, not
  across a connective or another subject". A binder with real structure is
  Phase 5, and it replaces this.
- **Ranking reorders only retrieval's top-k.** Evidence can lift a candidate by
  at most the sum of the positive weights (0.65). A reading ranked below k by
  similarity is never seen. k = 10 by default.
- **One exact configured phrase beats every shape rule.** "in the last" starts
  with a function word, and "order amount" is two words, but the config author
  declared both. That puts trust in the config, which is where the experiment
  already puts it.

## Benchmark evidence

`docs/phase-4-results.md` (final): ambiguity set **33/34 vs raw top-1 20/34**,
commerce regression set **17/18 vs 13/18**, **zero silently wrong** on both.
How the formula treats a word like `amount` in context is decision 012.
