# 010 — Overlapping spans, not greedy segmentation

**Status:** accepted · **Phase:** 3 · **Date:** 2026-09-21

## Decision

Emit every contiguous 1..N-token span as a candidate. Do not segment the
sentence. Phase 4 chooses.

## The alternative, and why it loses

Greedy longest-match segmentation is cheaper and produces a clean, disjoint
result:

```text
"customers who spent more than 50000"
  [customers] [who] [spent] [more than] [50000]
```

Five spans instead of nineteen, and each token used once. The problem is that
the split is a **decision**, taken by the layer with the least information.
Phase 3 has no config, so it cannot know that this schema calls the field
"spending" and that "customers who" is meaningless here. A wrong early split
silently removes the correct interpretation before ranking ever sees it, and no
later phase can recover it — the evidence is gone.

Phase 2 produced the concrete evidence for this. Two of its six retrieval misses
were caused by exactly this failure at the sentence level:

```text
client location  →  expected customer.country
  1. entity:customer       0.658  ("client")
  2. field:customer.name   0.621
  3. field:customer.country 0.548 ("location")
```

The whole phrase was embedded as one string; "client" dominated because it is a
strong entity synonym. `recent customer` failed the same way and did not
retrieve `created_at` at all. Both now produce `client`, `location` and
`client location` as separate candidates, so retrieval can score each and Phase
4 can pick a cover with the config in hand.

## Cost

Candidate count grows as roughly *n × N*. For a 12-token sentence at N=4 that is
under 50 spans — negligible against a 2.7 ms embedding call, and section 16.7's
`maxPhrases` limit bounds it. If it ever matters, Phase 9 will say so with a
measurement; optimising it now would be rule 7 of section 22 in reverse.

The other cost is that Phase 4 now owns a problem Phase 3 could have hidden:
choosing a non-overlapping cover from overlapping candidates. That is the right
place for it. Selecting a cover is a scoring problem, and Phase 4 is the phase
with scores.

## What is filtered, and why that is not segmentation

Three rules drop candidates that could never be useful. None of them chooses
between competing readings:

- **Spans of only function words.** "show me the" cannot match a schema object.
  Function words are marked, never removed — "more than" and "from India" both
  depend on one — but a span made of nothing else is pure noise for the ranker.
- **Spans crossing punctuation.** "customers, orders" is two phrases.
- **Text spans duplicating a typed span.** A number or date already has a span
  carrying its parsed value; embedding the bare surface "50000" retrieves noise.
  Spans that merely *contain* a number survive, because the surrounding words
  are what carry the operator.

Ordering is longest-first, then left-to-right, then typed-before-text. A caller
taking the first acceptable match therefore sees "more than" before "more",
which is the specific-first preference, and the order is fully deterministic.

## Revisit if

- Phase 4's cover selection turns out to need spans Phase 3 filtered.
- Phase 9 shows candidate count is a real cost at 1k or 10k schema entries —
  though the cost there is corpus size, not sentence length.
