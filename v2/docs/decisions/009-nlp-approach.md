# 009 — Hand-rolled deterministic NLP, no dependencies

**Status:** accepted · **Phase:** 3 · **Date:** 2026-09-21

## Decision

Write tokenisation, number parsing, date parsing and span generation by hand in
Go. No third-party library, no statistical tagger, no Python.

## Options considered

| Option | Verdict |
|---|---|
| Pure Go, hand-rolled | **Chosen** |
| A Go NLP library (prose, spago) for tokens and POS | Rejected |
| Extend the Python sidecar with spaCy | Rejected |

The brief allows "lightweight POS tagging if justified" and "dependency parsing
if needed". Neither is needed yet, and "if justified" is a bar this phase does
not clear: the spans that matter here — a field name, an operator phrase, a
value — are found by the lexical structure, not by knowing that "spent" is a
verb.

The decisive argument against a library is Phase 10. Every failure has to be
classified into one primary cause and quantified. A statistical tagger's
mistakes are not attributable to a rule anyone can read, so a failure caused by
one would land in the taxonomy as an unexplained `NLP_EXTRACTION` with no path
to a fix. Hand-rolled rules fail in ways that can be pointed at.

spaCy would also make Python mandatory for the whole pipeline rather than just
for embeddings, and add several hundred megabytes to setup, for features
nothing currently consumes.

## What this costs

Real work, and real gaps. The parts most likely to be revisited:

- **Unicode normalisation is pragmatic, not NFKC.** Full normalisation needs
  `golang.org/x/text`, which would be the module's first dependency. What is
  handled is what actually arrives from a keyboard or a paste: smart quotes,
  dashes, non-breaking and zero-width spaces, soft hyphens, control characters.
  Combining-accent sequences are **not** decomposed, so "café" written with a
  combining acute will not match one written precomposed.
- **English only.** The number words, unit words, month names and function words
  are English. Nothing in the design forbids another language; nothing supports
  one either.
- **No lemmatisation.** "spent" and "spending" are different strings. This costs
  less than it appears to, because the embedding layer is what bridges them —
  that is the division of labour the architecture is built on, and Phase 2
  measured it working.

## Where the line between phases falls

Extraction does not know what a span means, and must not learn. What a span
REFERS to needs the config and the schema graph, which Phases 4 and 5 have.
Extraction classifies only what is a lexical fact rather than a semantic
judgment: numbers, dates and connectives.

The practical consequence is that `--analyze` takes no `--config` and no model.
That is enforced by `TestExtractionIsConfigIndependent`, and it is what makes a
Phase 3 failure distinguishable from a config problem.

## Two things the tests caught

**Spelled numbers were summed greedily.** The first implementation added every
consecutive number word, so "one two three" parsed as 6 — a value appearing
nowhere in the sentence, which would have been bound as a filter. English number
words combine only as tens-plus-units ("twenty five") or across a scale word
("fifty thousand"). The group state in `parseSpelledNumber` enforces that.

**Writing `﻿` in source produced a literal BOM**, which the Go parser
rejects outright with an error naming a character nobody can see. Invisible code
points are now written as `rune(0xFEFF)` in tests and as escapes in
`normalize.go`. Source you cannot see is source you cannot review.

## Revisit if

- A measured Phase 10 failure traces to missing NFKC, lemmatisation or POS.
  Each is then a bounded addition with evidence behind it, rather than a guess.
- The experiment takes on a second language, where the hand-rolled word lists
  stop scaling.
