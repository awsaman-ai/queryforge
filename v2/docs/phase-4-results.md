# Phase 4 results: candidate ranking

**Date:** 2026-09-21 · **Status:** **gate passed and approved by the user**
(2026-09-21). Final gate: **81/81 checks, no skips**. `go test -race -count=1 ./...`
is clean.

Environment as in [phase-2-results.md](phase-2-results.md). Model:
MiniLM-L6-v2 @ 1110a243, surface strategy, top-k 10.

## What Phase 4 is for

Phase 2 retrieval answers *"which corpus entries are close to this phrase?"*
Phase 4 answers *"is there **one** entry this phrase should be read as, and how
sure is that?"* The two questions come apart exactly when it matters:

```text
"name"     field:customer.name  1.000
           field:product.name   1.000     ← raw top-1 picks customer.name, by id order
"highest"  aggregation:max      1.000
           pattern:top_n        1.000     ← same
```

A 1.000 similarity is an exact surface match. On a word several fields share,
that is the moment confidence is *least* warranted. Phase 4 therefore keeps
three numbers apart on every candidate:

| | |
|---|---|
| **similarity** | the retrieval cosine, untouched. Evidence, not confidence |
| **score** | similarity + weighted structural evidence. Comparable only within one span |
| **confidence** | a property of the **decision**, from the top-1 vs runner-up margin |

The formula, signals, weights and the reasons for each are in
[decision 011](decisions/011-ranking-formula.md).

## What was built

`internal/ranking`, ~1,500 lines, no dependencies, no model:

| File | Role |
|---|---|
| `ranking.go` | `Policy`, `Evidence`, `Candidate`, `SpanRanking`, the decision rule, agreements |
| `rank.go` | the two passes, every signal, attachment, the cover, unexplained tokens |
| `verdict.go` | `VerdictError`: the sentence-level refusal, redacted and revealed renderings |

Retrieval results come **in** to `Rank` rather than being fetched there, so the
whole package is tested with scripted scores, exact to the decimal.

CLI: `--rank <sentence>` (reading and verdict, full ranking object under
`--debug`), `--rank-probe <file.tsv>` (ranking vs raw top-1 vs threshold-only),
and `--min-score` / `--min-margin` overrides, which the output announces as
OVERRIDDEN.

```console
$ qf-v2 --config testdata/configs/ambiguity.json --rank "amount"
  "amount"   ambiguous  field:order.amount     1.000  1.050  0.000  0.00
               vs       field:payment.amount   1.000  1.050
verdict:  AMBIGUOUS_MATCH
AMBIGUOUS_MATCH: the phrase at tokens[0:1] matches multiple configured targets with
insufficient confidence separation: field:order.amount, field:payment.amount (margin 0.000 < 0.100)

$ qf-v2 --config testdata/configs/ambiguity.json --rank "payments with amount over 100"
  "payments"  accept  entity:payment         0.932  1.132  0.170  0.85
  "amount"    accept  field:payment.amount   1.000  1.400  0.200  1.00
  "over"      accept  operator:gt            1.000  1.150  0.557  1.00
  "100"       value   number 100
verdict:  OK
```

In the second example, `amount` gets similarity 1.000 against *both* amount
fields. It is decided by entity (+0.15, payments named), type (+0.10, a number
follows) and operator (+0.10, `gt` is allowed): 1.400 against 1.200.

## Exit criterion

> **PASS when ranking materially outperforms raw vector top-1 on the ambiguity
> test set.**

The brief's gate asks for intentional ambiguity. `testdata/configs/ambiguity.json`
provides it: order, payment and product each have fields called `amount`,
`status`, a date, and `name`, with distinctive config values (`Freedonia`,
`zorbified`, `*_qx`) so the leak sweep cannot confuse them with prose.

| Set | Raw vector top-1 | Threshold only | **Phase 4 ranking** |
|---|---|---|---|
| Ambiguity (34 scored) | 20 correct · **14 silently wrong** | 20 · 13 wrong · 1 refused | **33 · 0 wrong** · 1 refused |
| Commerce regression (18) | 13 · **5 silently wrong** | 13 · 4 wrong · 1 refused | **17 · 0 wrong** · 1 refused |

These are the **final** figures. They include the rows added for decision 012
(`order amount`, `payment amount`, and commerce `amount` now scored). The
figures reported when the gate was first run were 31/32 vs 18/32 and 15/16 vs
11/16. Nothing already there changed. Decision 012 added three rows
(`order amount` and `payment amount` on ambiguity, `order amount` on commerce)
and turned commerce `amount` from exploratory into scored. All four are correct
under ranking.

*Silently wrong* is the failure that matters: a confident pick of the wrong
target. On an AMBIGUOUS row, any pick at all counts. Full tables:
`benchmarks/reports/phase4-rank-{ambiguity,commerce}.md`.

The baselines are **generous**: they are handed the phrase under test, while
ranking has to find it among every overlapping span of the sentence.

The one refusal on each set is the same row: `value` alone. Its top-1 scores
0.616 / 0.682, below `minScore`, so it is rejected as LOW_CONFIDENCE rather than
reported as AMBIGUOUS. It is counted as *refused*, not correct: "I found
nothing good enough" is a different claim from "I found two".

### The brief's five words

| Word (alone, ambiguity.json) | Raw top-1 | Phase 4 |
|---|---|---|
| amount | order.amount, 1.000 | AMBIGUOUS (order / payment, margin 0.000) |
| value | payment.amount, 0.616 | NO_SEMANTIC_MATCH (below minScore) |
| date | order.created_at, 0.726 | AMBIGUOUS (margin 0.005) |
| status | customer.status, 1.000 | AMBIGUOUS (three fields tied) |
| name | customer.name, 1.000 | AMBIGUOUS (tied) |

And each resolved by sentence evidence: `orders with amount over 100` →
order.amount, `payments with amount over 100` → payment.amount, `status of the
payment` → payment.status, `orders by date` → order.created_at, `names of
customers` → customer.name. `customers and products by name` stays AMBIGUOUS,
because context naming *both* owners cannot decide, and it must not pretend to.

### Phase 2 findings, asked of the ranker

| Phase 2 finding | Phase 4 |
|---|---|
| `client location` collapsed onto `client` | `location` → customer.country ✔ |
| `recent customer` found no date field | `recent` → pattern:recent ✔ (binding it to created_at is Phase 5) |
| `placed an order`: entity beat the relationship | `placed` → relationship:customer_orders, `order` → entity:order ✔ |
| `customers who bought something` at rank 5 | `bought` → relationship:customer_orders ✔ |
| `the highest ones`: max/top_n tie broken by id | AMBIGUOUS ✔. With a count, `the 10 highest customers` → top_n ✔ |
| `name` / `date` at 1.000 / 0.726 hid a rival | AMBIGUOUS ✔ |
| `status` | accepted: commerce has one status field, margin 0.3+. Correct *to* accept |

## Calibration

`minScore 0.70`, `minMargin 0.10`, chosen from a 5 × 9 grid over both sets
(`benchmarks/reports/phase4-calibration-sweep.md`):

- At `minScore ≥ 0.70`, **every** `minMargin` from 0.03 to 0.30 gives zero
  silently-wrong answers on both sets. Raising the margin only turns correct
  answers into refusals, never into different answers.
- 0.70 / 0.10 is inside that safe region with the most correct answers. Below
  0.70, commerce picks up a wrong answer at small margins. Above 0.10,
  refusals start to climb.
- The sweep ran on the 48 rows that existed before decision 012. The four rows
  it added pass at the chosen policy and do not move the choice.
- **Provisional.** 48 hand-written rows, written by the person who wrote the
  ranker, with several sentences seen during development. Phase 10 recalibrates
  on a real evaluation set.

## Failures and surprises

Found on real runs and in the sweep, not anticipated in the design. Every one
has a regression test, and every regression test was checked by reverting its
fix: 11 mutations, 11 caught.

**1. Phase 2 leaked config values through entry IDs.** Value entries embed
the value in their ID, `value:customer.country:alias:India`, and `--search`
printed the ID verbatim in text and JSON while the column beside it said
`<redacted>`. The Phase 2 gate check looked only for `<redacted>`, so it
passed. Fixed at the source with `corpus.DisplayID`, used by `--search`,
`--probe` and all ranking output. Internal IDs, the cache fingerprint and the
probe files are unchanged. **This changes Phase 2 output**, as a contract fix.

**2. Junk n-grams won readings.** `orders with` was accepted as
`payment.order_id` at similarity 0.649, and `with amount over 100` as
`payment.amount`. Long spans match long descriptions well enough to clear a
threshold. The fix is span-quality evidence: a function-word boundary, a
swallowed number or date, a target already carried by a shorter span, or a span
that decomposes into shorter accepted ones. Exact configured phrases are exempt.

**3. The cover preferred confidence over span quality.** A penalised span can
still clear its *own* margin, because its rivals carry the same penalty. So
`orders and payments with amount over 5` was read as payment.amount through
the defective span `payments with amount`, which swallowed the entity word and
resolved an ambiguity the sentence genuinely has. Span quality is now the
primary cover key, above accept-vs-ambiguous.

**4. A tie decided by id order, one level down.** Span quality compared against
a shorter span's retrieval top-1. For `highest`, retrieval had picked
aggregation:max out of a 1.000 tie *by id*. So `what is the highest` penalised
max but not top_n, and top_n was accepted by a 0.200 margin that existed only
because "aggregation" sorts before "pattern". Every entry tied at the top now
counts.

**5. A stricter policy guessed more.** Raising `minMargin` from 0.10 to 0.15
*introduced* a silent wrong answer. The stricter margin made `orders` ambiguous
in pass 1, so it contributed no context, while a defective span contributed
`payment`. `amount` then saw one-sided evidence. Context is now conservative:
only clean spans contribute, and an ambiguous span contributes every entity
among its live readings. The sweep is now monotone. A test asserts that
directly.

**6. The partial-lexical rule ran the wrong way.** It rewarded a span whose
words all appear *in* the surface, so `value` got credit from `purchase value`,
although the qualifier "purchase" was never said. That pushed commerce's
`value` (raw margin 0.094) over the line to total_spent. The rule now rewards
only a surface that is wholly present in the span.

**7. Smaller ones.** Naming *one* end of a relationship boosted it even on the
word that names the other end (`order` vs customer_orders), so support now
needs both ends. Context boosted an entity's fields but not the entity itself
on the word that names it, so it is now symmetric. Entity candidates took a
join-distance penalty, which is wrong because naming a second entity is how a
sentence *asks* for a join. A join key (order.customer_id, neither selectable nor
filterable) competed for `customers` via its description; it is now vetoed.
`--min-score 0` silently meant "default", and was replaced by `flag.Visit`.

**8. The redaction sweep first proved nothing.** With `DisplayID` disabled
entirely, the first CLI sweep still passed: every sentence *typed* its value,
and a typed value may be echoed. Misspellings (`Freedon`, `payments that
bounce`) retrieve values the user did not type, which gives the sweep teeth.
Found by mutation-testing the redaction.

## Redaction

- Value IDs render as `value:customer.country:alias:<redacted>`, and matched
  surfaces as `<redacted>`, unless `--debug` is set.
- `Candidate` implements `String`/`GoString`, so formatting one by accident
  (`%v`, `%+v`, `%#v`) redacts. It is the same principle as `qferr.Sensitive`.
- `VerdictError` names competing targets (structure) and locates the phrase by
  token position. The phrase itself is a user literal, which decision 006 puts
  in the data domain, so only `--debug` quotes it. It implements the same
  `ErrorRevealed` / `RedactedValueCount` pair as `ValidationError`. `report()`
  now checks an interface instead of the concrete type, so a later phase's
  error cannot be forgotten.
- Reasons name structure only, and a sweep against every fixture value asserts
  that.

**User text (decision 013, the user's gate decision):** the `--rank` *report*
echoes the user's own sentence and spans, as `--analyze` does. It is a developer
and research diagnostic, and must never be captured as telemetry or logs. The
*error message*, which is what reaches logs, locates phrases by token position
and quotes them only under `--debug`. Config values stay redacted on both
surfaces.

## Coverage

48 Go files, ~13,700 lines. **221 test functions** (was 161): 47 in
`internal/ranking`, 11 new CLI tests, 2 in corpus. All pass under `-race`. The
gate script is **81 checks** (was 56).

The six test categories you asked for:

| Category | Tests (internal/ranking) |
|---|---|
| High-score but ambiguous | `TestHighScoreTieIsAmbiguous`, `…NarrowMarginAreAmbiguous`, `…AllCompetitorsWithinMarginAreReported` |
| Lower-score, clearly separated | `TestModestScoreWithWideMarginIsAccepted`, `TestConfidenceIsNotSimilarity` |
| Exact lexical, still ambiguous | `TestExactLexicalMatchOnBothSidesStaysAmbiguous`, `TestHighestIsAmbiguousBetweenMaxAndTopN`, `TestContextNamingBothEntitiesStaysAmbiguous` |
| Deterministic ties | `TestTieHandlingIsIndependentOfInputOrder` (50 shuffles), `TestTieIsAmbiguousEvenWithZeroMargin`, `TestShorterSpanTieDoesNotFavourOneReading`, `TestRankIsSafeConcurrently` |
| Threshold × margin | `TestThresholdAndMarginTable` (11 cases incl. the 0.9−0.8 float trap), `TestConfidenceFormula`, `TestStricterMarginNeverChangesAnAcceptedTarget`, `TestPolicyValidation` |
| Phase 2/3 regressions | relationship vs entity, the max/top_n tie, `from` as a boundary, the ID leak (corpus + CLI), and every Phase 4 finding above |
| Contextual `amount` (decision 012) | `TestAmountIsResolvedByContextNotByRule`: six cases, each asserting the outcome AND the evidence that decided it |

## Deliberate non-results

- **`--query` still compiles nothing.** An accepted reading is a set of targets,
  not a query. Binding (5) and the compiler (6) stand between.
- **Attachment is a window heuristic**: the value to the right, within 4 tokens,
  not across a connective or another subject. Phase 5's binder replaces it with
  structure.
- **Unexplained words are reported, not judged.** `zorp the orders` reads
  `orders` and lists `zorp`. Whether an unexplained word must fail the query is
  a binding decision.
- **No ML, no FAISS, no learned weights**, as instructed. Every weight is a round
  number with a stated reason.
- **Retrieval is untouched**, apart from the display fix above. Phase 2 and
  Phase 3 behaviour is unchanged, and their gate sections still pass.

## Gate decisions (user, 2026-09-21)

**1. `amount` is contextual, and there is no global ambiguity rule.** Current
behaviour is kept. `amount` alone is ambiguous where two fields carry the name.
`order amount` and `payment amount` resolve through their exact synonyms.
`payments with amount over 100` resolves through entity + number + operator
evidence. On commerce.json, `amount` alone → order.amount, because exactly one
field is named amount. Recorded in [decision 012](decisions/012-contextual-ambiguity.md),
pinned by `TestAmountIsResolvedByContextNotByRule`, and checked in section 11
of the gate.

**2. `--rank` keeps user text; error and log paths do not.** Recorded in
[decision 013](decisions/013-rank-output-is-diagnostic.md). It places
obligations on Phase 7 and Phase 9: telemetry is built from the structured,
redacted forms, never from `--rank` stdout.

## Gate

```bash
cd v2 && ./scripts/run-manual-tests.sh
```

**Final: 81 checks, all passing, none skipped** (2026-09-21). Section 10 is
Phase 4 mechanics and redaction (model-free). Section 11 runs against the real
model: the brief's five words, their resolution by evidence, the decision-012
`amount` cases, the Phase 2 regressions, and the exit criterion on both sets.
Approved; Phase 5 may begin.
