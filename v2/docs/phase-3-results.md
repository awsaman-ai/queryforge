# Phase 3 results — phrase and slot extraction

**Date:** 2026-09-21 · **Status:** built and self-verified; **awaiting the
user's manual gate**.

Environment as in [phase-2-results.md](phase-2-results.md). Phase 3 uses no
model and no Python: extraction is pure Go with zero dependencies.

## What Phase 3 is for

Phase 2 ended with six retrieval misses. **Two of them were the same failure**:
the whole sentence was embedded as one string, so whichever half had the
stronger corpus match won and the other was lost.

```text
client location  →  expected field:customer.country
  1. entity:customer        0.658   ("client")
  2. field:customer.name    0.621
  3. field:customer.country 0.548   ("location")
```

`recent customer` failed the same way and did not retrieve `created_at` at all.
Phase 3 is the layer those two were waiting for, and both are now in the gate as
regression checks.

## What was built

`internal/nlp`, ~1,100 lines, no dependencies:

| File | Role |
|---|---|
| `normalize.go` | Unicode folding, whitespace collapse, invisible-character removal |
| `tokenize.go` | tokens with exact byte offsets, lexical kinds, function-word marking |
| `numbers.go` | digits, grouping, decimals, `50k`/`1.2m`, currency, percent, spelled-out numbers |
| `dates.go` | rolling and calendar windows, absolute months and years, half-open resolution |
| `spans.go` | overlapping candidate spans, connectives, the `Analysis` result |

Plus `--analyze` on the CLI, which takes **no config and no model** — extraction
is config-independent by design, and that is enforced by a test.

```console
$ qf-v2 --analyze "customers who spent more than 50000 last year"
numbers:
  "50000"                  -> 50000
dates:
  "last year"              -> calendar year, offset -1, count 1
candidate spans (longest first):
  "spent more than 50000"          tokens[2:6]
  "more than 50000"                tokens[3:6]
  "more than"                      tokens[3:5]
  "last year"                      tokens[6:8]  [date]
  "customers"                      tokens[0:1]
  "spent"                          tokens[2:3]
  "50000"                          tokens[5:6]  [number]
  ...
```

## Exit criterion

> **PASS when phrase extraction is stable enough to feed Phase 4 without
> hardcoding complete example sentences.**

The tests assert that the span a later phase *needs* is present — never that the
span set equals some list, which would be the hardcoding the criterion rules
out. `TestParaphrasesYieldTheCarryingSpan` covers three concepts across twelve
phrasings:

| Concept | Sentence | Carrying span |
|---|---|---|
| spending field | `customers who spent more than 50000` | `spent` |
| | `customers spending over 50000` | `spending` |
| | `customers whose spending is above 50000` | `spending` |
| | `customers with purchase value over 50000` | `purchase value` |
| country field | `customers from India` | `from india` |
| | `customers living in India` | `living in india` |
| | `clients located in India` | `located in india` |
| operator `gt` | `spent more than 50000` | `more than` |
| | `spent over 50000` | `over` |
| | `spending above 50000` | `above` |

Paraphrases do not have to yield the *same* span — "spent" and "spending" are
different words — only a span the corpus can match. Bridging those two is the
embedding layer's job, and Phase 2 measured it doing so.

The brief's six manual-gate sentences (section 11) each have their own test, and
all six appear in the gate script.

## Numbers and dates

Spelled-out numbers are not a nicety: `customers spending over fifty thousand`
is one of the brief's own gate sentences, and a pipeline seeing only digits
would drop the value and emit a query missing its filter.

| Input | Parsed |
|---|---|
| `50,000` / `1.5` / `50k` / `1.2m` | 50000 / 1.5 / 50000 / 1200000 |
| `$50000` / `50000 USD` | 50000, currency USD |
| `20%` | 20, percent |
| `fifty thousand` | 50000, spelled |
| `two hundred and fifty` | 250, spelled |

Dates normalise to a descriptor, **not** to timestamps. `Resolve` does the
arithmetic and is fully tested, but nothing in the extraction path calls it: the
boundaries of "last month" depend on a clock and a timezone that belong to the
request, and resolving here would freeze both into the layer least able to
justify them.

| Input | Descriptor |
|---|---|
| `last year` | calendar year, offset −1 |
| `last 6 months` | **rolling** 6 months back from now |
| `this month` | calendar month, offset 0 |
| `past 30 days` | rolling 30 days |
| `January 2024` | absolute, 2024-01 |

**Rolling versus calendar is not pedantry.** "Last 6 months" is the six months up
to now; "last month" is the previous calendar month. Collapsing them shifts every
boundary by up to a month, and the error is invisible in the generated SQL.
Ranges are half-open (`>= start AND < end`) for the same reason decision 004
refused a `BETWEEN` operator.

## Span generation

Every contiguous 1–4 token span survives. No segmentation — see
[decision 010](decisions/010-span-generation.md). Three filters drop candidates
that could never be useful, and none of them chooses between competing readings:
spans of only function words, spans crossing punctuation, and text spans
duplicating a typed number or date span.

| Sentence | Tokens | Spans |
|---|---|---|
| `Show customers from India` | 4 | 8 |
| `customers who spent more than 50000` | 6 | 17 |
| `show the top 10 customers from India who spent more than 50000 last year` | 14 | 38 |

Roughly *n × N*, bounded by `maxPhrases`. Negligible against a 2.7 ms embedding
call; if it ever matters, Phase 9 will say so with a measurement.

## Coverage

40 Go files, ~9,700 lines. **161 test functions, 271 cases**, passing under
`-race`. The gate script is now **56 checks**, up from 46.

Robustness per section 21: `TestAnalyzeDoesNotPanic` covers empty input, 10,000
characters, 500 repeated words, control characters, CJK, emoji, SQL-injection
shapes, `1e999999`, and a 400-digit count in a date phrase. Every span's byte
offsets are validated against the normalised string for each.

## Failures and surprises

**Spelled numbers were summed greedily.** The first implementation added every
consecutive number word, so `one two three` parsed as **6** — a value appearing
nowhere in the sentence, which would have been bound as a filter and produced a
confidently wrong query. English number words combine only as tens-plus-units
("twenty five") or across a scale word ("fifty thousand"). Found by a test whose
*input* was badly chosen rather than by the assertion it was written for:
`TestMaxSpanTokensIsRespected` used "one two three four five six seven" as
filler, and the filler turned out to parse as a single number of 28.

**`from` was missing from the function-word list**, while `of`, `in`, `by` and
`with` were all present. Caught by a CLI test that expected *some* token in
"customers from India" to be marked. Deliberately still absent: `over`, `under`,
`above`, `below`, `than` — those are operator vocabulary the config is built
around.

**Writing `﻿` in source produced a literal BOM**, which Go rejects with
"illegal byte order mark" — an error naming a character that is invisible in
every editor. It happened twice, in `normalize.go` and again in the test file.
Invisible code points are now written as `rune(0xFEFF)` or as escapes, and the
reasoning is recorded where the constants live.

## Deliberate non-results

- **`--query` still compiles nothing.** Spans are not a query. Ranking (4),
  binding (5) and a compiler (6) all stand between.
- **No span is classified as field, operator or value.** That needs the config,
  which this layer deliberately does not have.
- **No cover selection.** Choosing a non-overlapping reading is a scoring
  problem and belongs with Phase 4, which has scores.
- **No lemmatisation, no POS, no NFKC.** Each is a bounded addition if a
  measured Phase 10 failure points at it; none is justified by evidence now.
- **English only.** The word lists are English, and nothing pretends otherwise.
- **Phase 2 retrieval is untouched**, as instructed. Phase 3 adds a layer in
  front of it and changes nothing behind it.

## Gate

```bash
cd v2 && ./scripts/run-manual-tests.sh
```

56 checks, all passing. Section 9 is Phase 3, including both Phase 2 misses as
regression checks. Phase 4 does not begin until the user confirms.
