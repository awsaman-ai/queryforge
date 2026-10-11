# Phase 7 results: natural language to final SQL

**Date:** 2026-09-22 · **Status:** **CLOSED.** Phase 7 was kept open at its
first gate (finding V02, a silently narrower query). The six gate decisions
(decision 019) were applied. At the re-gate the user revised §5 ("show
customer date" must refuse), and every gate was then re-run green: the manual
gate is **146/146 checks, 0 failed, 0 skipped**, and `go test -race -count=1
./...` is clean in all 14 packages. There are still zero third-party Go
modules.

## Final gate (after the §5 revision)

| | First gate | First re-gate | **Final** |
|---|---|---|---|
| Query set | 63/74 | 68/74 | **70/77** (queries 49/55, specified refusals 21/22) |
| **Silently wrong accepted queries** | **1** (V02) + 1 arguable (X03) | 0 (X02 open) | **0** |
| Values accepted on similarity alone | 1 | 0 | **0** |
| Previously passing cases that regressed | — | none | **none** |
| Accepted queries valid PostgreSQL (libpg_query 8.4, parse-only) | 55/55 | 53/53 | **54/54** |
| Phase 4 probes | 33/34 · 17/18, 0 silently wrong | same | **33/34 · 17/18, 0 silently wrong**; both reports byte-identical |
| Calibration sweep | — | 0 silent wrongs at minMargin ≥ 0.10 | **0 silent wrongs in every cell** (was up to 2 + 2 below minMargin 0.10) |
| Manual gate | 136 checks | 141/141 | **146/146** |

### The §5 revision: a shared phrase needs distinguishing evidence

The user's principle: *if a generic semantic phrase is shared by multiple
candidates and the sentence does not provide explicit distinguishing
evidence, refuse.* Full record: [decision 019 §5](decisions/019-phase-7-gate-decisions.md).

| Sentence | Result (real model) |
|---|---|
| `show customer date` | **AMBIGUOUS_MATCH**, naming `customer.created_at` and `order.placed_at` |
| `show customer creation date` | `SELECT "t0"."created_at" FROM "customers" AS "t0";` |
| `orders by date` | `SELECT "t0"."placed_at" FROM "orders" AS "t0";` ("order date" is spelled out) |
| `show customer date` on `ambiguity.json`, which configures "customer date" | `SELECT "t0"."created_at" …` |
| `show customer value` (X03) | AMBIGUOUS_MATCH, as before, now with its real reason |
| `show customer order date` | refused, safely: **known limit**, below |

- **The similarity difference no longer decides.** The rule ignores
  similarity. Whichever of the two fields leads, and by however much, "show
  customer date" is refused. A test pins four orderings.
- **What counts as evidence.** A target's configured phrase that the
  sentence's own words spell out, and that belongs to that target only. It
  may be non-contiguous, as in "orders by date". Entity context does not
  count, a partial phrase does not count ("customer" without "creation"), and
  descriptions do not count. If the spelled-out phrase belongs to a target
  other than the leading one, the span is refused, never switched.
- **Scope held.** No retrieval, scoring, weight, threshold, cover or binding
  change. The existing post-decision check lost its similarity condition and
  gained the evidence exemption.
- **Bug found and fixed.** The first §5 refusal reused the margin message
  and told the caller "insufficient confidence separation … (margin 0.294 <
  0.100)". That is false, since 0.294 clears the bar. A shared-phrase refusal
  now says "… is part of the configured phrases of several targets, and nothing
  in the sentence distinguishes them: …". It is redaction-safe: token positions
  and structural targets only.
- **Tests.** Six new or extended ranking tests, one pipeline test, and five
  new gate checks. `TestSharedPhraseWithoutDistinguishingEvidenceIsRefused` is
  the exact case with MiniLM's scores. It asserts the code, both targets, the
  recorded evidence and reasons, a margin that *would* have accepted (so the
  refusal is provably the rule's), and the message. **Nine mutations of the
  rule, all caught.** One (counting descriptions) survived its first run and
  got `TestSharedPhraseIgnoresDescriptions`.

### Query-set changes

- **X02**, `show customer date`: the expectation changed to AMBIGUOUS_MATCH, by
  the user's decision. It is the only case whose outcome changed.
- **Added, with expectations written before the run:** X10 `show customer
  creation date` (passes), X11 `show customer order date` (*known* failure),
  X12 `orders by date` (passes).
- The other 73 cases are byte-identical except for X03's corrected refusal
  message.

### Known limit: `show customer order date` (X11)

For the phrase, the rule gives the user's answer: "order date" is placed_at's
configured synonym. The sentence as a whole is still refused, for two older
reasons, both safe:

1. **Cover tie.** "customer order" (the relationship `customer_orders`) and
   "order date" (placed_at) are both exact configured phrases. They overlap,
   and they tie on confidence and length. Cover selection breaks the tie by
   position and takes "customer order", which leaves "date" ambiguous by
   margin (0.015).
2. **Binding.** With the other cover, binding refuses listing order fields
   through customer (decision 015: "root the query at order").

Fixing it needs a cover-selection change and a binding change, which were
ruled out of scope for this re-gate. It is recorded, not worked around.

### The remaining 7 failures are all safe refusals; no answer was given

| Case | Why refused |
|---|---|
| S05 `list products` | "product" is a synonym of both `entity:product` and `relationship:order_product` (tie). A §4 warning in Phase 8 |
| D05 `orders placed in 2024` | "placed" is on both `relationship:customer_orders` and the field `placed_at`. A §4 warning |
| O06 `highest spending customers` | "highest" is on both `max` and `top_n`. The case expected the missing-N refusal; the collision refuses first. A §4 warning |
| J02 `customers with orders above 10000` | "orders": entity vs `order.amount`, margin 0.082 < 0.10 |
| M07 `… more than 1000 and less than 5000` | the second bound names no field, and customer has two number fields |
| X11 `show customer order date` | the known limit above |
| Z05 `customers named Robert'); DROP …` | a punctuation fragment is unexplained. The case accepted either outcome in words but encoded `sql` |

## First re-gate (decision 019 §1–§3 and the first §5), superseded above

- **V02 (§1).** Ranking now accepts a value only with literal evidence.
  "customers living in India" is refused, because "living" is no configured
  value.
- **X03 (§5, first version).** A shared word matched below the threshold was
  not settled by entity context. The revision above replaced the threshold
  condition with the evidence rule.
- **§2 and §3.** No code changed; the refusals are now specified and pinned by
  tests.
- **Expectations.** Four query-set expectations changed, only where a
  decision specified the outcome: V02, O05, A03 and A04.

---

*The sections below are the first Phase 7 gate's report, kept as the record of
what was found. Where they quote 63/74 or 55/55, the re-gate summary above
supersedes them.*

## Phase 6 closure (your four decisions, applied first)

The decisions are recorded in [decision 017](decisions/017-phase-6-gate-decisions.md).

1. Currency and units stay refused until per-field metadata exists. `₹`, `€`
   and `kg` are now pinned alongside `$`, `USD`, `%` and `dollars`.
2. The same-record ambiguity stays refused, with no global rule.
3. NULL semantics are **intentional**. `TestNegationKeepsStandardNullSemantics`
   fails, and points to 017, if negation ever gains `IS NULL`, `IS DISTINCT
   FROM` or `COALESCE`.
4. `TestMismatchedRetrievalIsAnInternalError` now asserts `err == nil ||`.
   The standing rule: every `CodeInternal` assertion also asserts that an error
   happened.

The gate was re-run after these changes: 125/125, race clean. Then Phase 7
began.

## What was built

| Piece | Role |
|---|---|
| `internal/pipeline` | The one orchestrator: extract → retrieve → rank → bind → SemanticPlan → compile. It stops at the first refusal and keeps the earlier stages for `--explain` |
| `--query` | NL → final SQL + parameters (the section 15 contract); `--now`/`--tz`; `--explain`; `--json` |
| telemetry | `pipeline.Stats` covers section 24's fields. It holds no user text or values, and prints under `--debug` |
| `--e2e` | The reviewed query set → a deterministic report, diffed by the gate against its golden |
| config `ranking` block | The carried-forward per-schema thresholds, validated at load |
| `maxInputLength` | Now enforced (INVALID_INPUT). `maxCandidates` is deliberately deferred to Phase 8 (decision 018) |
| contradiction refusal | `country = IN AND country = US` → AMBIGUOUS_MATCH (finding M06) |

`--rank`, `--bind` and `--plan` now rank through the same pipeline. Their
earlier gate checks pass unchanged. The Phase 0 `TestQueryIsNotImplemented` was
**replaced**, as TODO required, by `TestQueryIsDerivedOrRefused` (CLI) and
`TestBriefExamplesEndToEnd` (pipeline).

## The brief's three target examples (real model)

```console
$ qf-v2 --config testdata/configs/commerce.json --query "Show customers from India"
SELECT "t0"."name", "t0"."country", "t0"."status", "t0"."total_spent", "t0"."created_at"
FROM "customers" AS "t0"
WHERE "t0"."country" = $1;

params:
1 = "IN" (string)
```

| Example | Result |
|---|---|
| 1 `Show customers from India` | ✅ the brief's shape |
| 2 `Show customers who spent more than 50000` | ✅ `WHERE "t0"."total_spent" > $1`, `1 = 50000` |
| 3 `Show the top 10 customers from India who spent more than 50000 last year` | ❌ **refused**, UNSUPPORTED_QUERY at binding. It is safe, but it misses the brief's target. See judgment call 2 |
| 3, with the metric named: `Show the top 10 customers by spending from India` | ✅ `WHERE country = $1 … ORDER BY total_spent DESC NULLS LAST LIMIT 10` |

## The reviewed query set (74 cases, section 15's "at least 50")

[`testdata/queries/phase7-e2e.json`](../testdata/queries/phase7-e2e.json) holds
each case's input, expected semantic meaning and expected shape. **All were
written before the first run.** The report
([`phase7-e2e.golden`](../testdata/queries/phase7-e2e.golden)) adds the actual
query, pass or fail, and the failure category. **Please review both.**

**63/74 pass**: queries 48/57, specified refusals 15/17.

| Category | Cases | Pass |
|---|---|---|
| simple entity selection | 5 | 4 |
| values and synonyms | 9 | 8 |
| numeric operators (incl. spelled-out, `k`) | 10 | 10 |
| dates (calendar, rolling, absolute) | 5 | 4 |
| aggregation | 7 | 5 |
| ordering and limits | 7 | 5 |
| joins (EXISTS, COUNT, to-one JOIN) | 8 | 7 |
| multiple conditions, negation | 7 | 6 |
| ambiguity and value resolution (specified refusals) | 9 | 8 |
| adversarial | 7 | 6 |

The first run gave 62/74. The contradiction refusal (M06) was then added and
mutation-tested, which made it 63. No expectation was edited after the first
run, including the three I would now word differently (O06, X03, Z05, below).

### Every failure, reviewed

**Silently wrong: a query compiled that does not mean what was asked**

| Case | What happened |
|---|---|
| **V02** `customers living in India` | It added `status = 'active'`. The word "living" was accepted as the status alias **"live"** on embedding similarity alone (0.790, **lexical evidence 0**, confidence 1.00). The answer is narrower than asked, and nothing in it shows that. **This is the most important Phase 7 finding.** The report measures the class: across all 74 cases, **exactly one** value was accepted without lexical evidence, and it is this one. See judgment call 1 |
| X03 `show customer value` | It gave `SELECT total_spent`. The brief lists this as ambiguous, but here the entity evidence ("customer") picks `customer.total_spent` ("purchase value") over `order.amount`. The reading is defensible; I left the case as written. Your call (judgment call 5) |

**Fixed during Phase 7**

| Case | What happened |
|---|---|
| M06 `customers from India and United States` | The first run compiled `country = $1 AND country = $2`, which is unsatisfiable and returned nothing, silently. It is now AMBIGUOUS_MATCH ("say 'or'"). The rule is in the compiler (decision 018) and was mutation-tested |

**Safe: refused, no answer given**

| Case | Why |
|---|---|
| S05 `list products` | "products" ties `entity:product` and the relationship synonym "product" of `order_product` (margin 0.000). A config vocabulary collision |
| D05 `orders placed in 2024` | "placed" ties the relationship synonym "placed" and the field `placed_at` (margin 0.019). Another collision |
| O06 `highest spending customers` | "highest" is a configured phrase of both `max` and `top_n` (margin 0.000). I expected a missing-N refusal, but the collision refuses first, as A06 correctly predicted. Both outcomes are safe |
| J02 `customers with orders above 10000` | "orders" is too close between `entity:order` and `order.amount` (margin 0.082 < 0.10) |
| A03, A04 `what is the average customer spending` | "what" is unexplained (decision 016, no word list) |
| M07 `… more than 1000 and less than 5000` | The second bound names no field, and customer has two number fields |
| Z05 `customers named Robert'); DROP TABLE …` | A punctuation fragment is unexplained. The case accepted either outcome in words, but encoded `sql` |
| O05 | Brief example 3 (above). Marked *known* |

Failures by the brief's taxonomy: AMBIGUITY 5, UNSUPPORTED_FEATURE 4,
SCHEMA_LINKING 2.

## Exit criterion

> **The end-to-end engine reliably produces valid final SQL for the declared scope.**

| | Evidence |
|---|---|
| valid SQL | **55/55** outputs (50 from the query set and 5 compiler goldens) parse under **PostgreSQL's own grammar** (libpg_query, via pglast 8.4). Each is exactly one `SELECT` with contiguous `$n`. The check was parse-only, with no connection, from a throwaway environment; nothing was added to the project |
| deterministic | The golden report is byte-identical across runs, and `--query` is byte-identical across processes |
| no generation, no bypass | Every query comes from `compiler.Compile(plan.FromBinding(binding.Bind(…)))`. The model's role ends at retrieval |
| safe failure | Of the 26 outcomes that were refusals or failures (15 specified refusals and 11 failures), 24 produced no answer. The exceptions are one silently narrower answer (V02) and one arguable reading (X03) |

**My assessment:** the engine produces valid SQL for everything it accepts, and
it fails safe on almost everything it does not. It **does not yet reliably
cover** the declared scope: 48/57 intended queries compile, and the misses
cluster in four fixable places (below). V02 shows that "fails safe" has one
real hole.

## Judgment calls for the gate

1. **Values accepted on similarity alone (V02).** Proposed fix: ranking accepts
   a value-alias or enum candidate only with lexical evidence, meaning the span
   matches the stored surface after case folding. A value is data, not
   vocabulary, so being semantically near a value is not evidence of it. This
   changes **Phase 4** behaviour. I would re-run the Phase 4 probes (33/34,
   17/18) and the query set before quoting anything. **Not done without your
   approval.**
2. **Brief example 3 (O05).** Binding rule 12 requires a *free* field mention
   as the top-N metric. Option (a): when no free mention exists, the metric may
   be the one number field the sentence compares; with several, the result is
   AMBIGUOUS_MATCH. The catch is that "last year" would then bind to
   `customer.created_at` (rule 10's inferred date field), and "spent … last
   year" does not mean "created last year". Commerce has no dated spending.
   Option (b): keep the refusal, since the rephrasing works. This is a
   **Phase 5** rule change either way.
3. **"what is …" (A03, A04).** This is fixable **in config, not Go**: a pattern
   with intent `filter` and phrases like "what is the" makes the words
   explained, and the planner already treats `filter` as adding nothing. It
   changes the commerce corpus, so the Phase 2 and Phase 4 probes must be
   re-measured. Or keep them refused.
4. **Vocabulary collisions (S05, D05, A06/O06).** The same surface is
   configured for two targets of different kinds ("product", "placed",
   "highest"). They are refused safely today. I propose adding this check to
   Phase 8's config validation, next to decision 016's number-word check.
5. **X03 expectation.** Should `show customer value` → `total_spent` be
   accepted?
6. **The two Phase 6 files I changed in Phase 7:** the contradiction refusal
   (compiler), and parameters shown by default in `--query` (decision 018).
   Please confirm both.

## Deliberate non-results

- **Read-only rejection is not yet explicit.** "delete all customers" is
  refused as UNSUPPORTED_QUERY by the unexplained-word rule. Section 16.1's
  explicit rejection is Phase 8.
- **`maxCandidates`** is not enforced (decision 018). It is Phase 8's.
- **No execution, no DB.** The pglast check parsed text only.

## Gate

```bash
cd v2 && ./scripts/run-manual-tests.sh
```

There are 136 checks. Section 16 (no model) covers pipeline tests under
`-race`, the CLI contract, the removal of NOT_IMPLEMENTED, and the case count.
Section 17 (real model) covers the brief's three examples (example 3's refusal
pinned as known), a join, the M06 refusal, the 74-case report against its
golden, and cross-process identity. Phase 8 does not begin until you confirm.
