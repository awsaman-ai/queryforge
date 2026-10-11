# Phase 6 results: SemanticPlan, planner, deterministic compiler

**Date:** 2026-09-21 · **Status:** built and self-verified; **awaiting the
user's manual gate**. Gate: **125/125 checks, no skips** (was 99).
`go test -race -count=1 ./...` is clean. There are still zero third-party Go
modules.

## What Phase 6 is for

Section 14 of the brief:

> Turn the semantic meaning into a valid query plan without generating SQL
> free-form. ... Given manually constructed `SemanticPlan` objects, verify
> deterministic SQL output. **PASS when compiler output is deterministic and
> cannot emit identifiers outside the configured schema.**

Retrieval and ranking are **unchanged**, and the Phase 5 binding contract is
**unchanged**: the planner consumes `binding.Binding` as it is. The rules are in
[decision 015](decisions/015-query-grammar-and-compiler.md). Your two Phase 5
gate decisions are in [decision 016](decisions/016-phase-5-gate-decisions.md).

## What was built

| Piece | Role |
|---|---|
| `internal/plan` | The `SemanticPlan` type: config names only, no SQL. Strict JSON decoding for hand-written plans, and redaction by default. `FromBinding` is the planner |
| `internal/compiler` | `check.go` resolves every name against the registry and graph, and applies the fan-out and allowlist refusals. `lower.go` builds section 14's grammar nodes. `render.go` writes fixed-layout text with `$n` parameters |
| `graph.Walk` (additive) | Turns a plan's relationship names back into keyed, directed steps, and refuses any it would not have produced itself |
| config `queryPatterns[].order` (optional, additive) | The top_n direction. It is validated by the new rule `invalid_pattern_order`, and commerce and ambiguity now declare it |
| `--compile <plan.json>` | The brief's gate surface. It needs the config and no model |
| `--plan <sentence>` | Sentence → binding → plan, with `--now` and `--tz`. It **stops at the plan**, because sentence → SQL is Phase 7 |

## The gate: hand-written plans (`testdata/plans/`)

```console
$ qf-v2 --config testdata/configs/commerce.json --compile testdata/plans/brief-example.json --debug
SELECT "t0"."name"
FROM "customers" AS "t0"
WHERE "t0"."country" = $1
  AND "t0"."total_spent" > $2
ORDER BY "t0"."total_spent" DESC NULLS LAST
LIMIT 10;

params:
1 = "IN" (string)
2 = 50000 (number)
```

Without `--debug`, `1 = <redacted> (string)`. Five accepted plans cover every
grammar node:

| Plan | Exercises |
|---|---|
| `brief-example` | the brief's own section 14 plan: WHERE, ORDER BY, LIMIT |
| `joins-and-subqueries` | two to-one LEFT JOINs, `IN`, a half-open date range, `LIKE` with the user's `%` and `_` escaped |
| `exists-and-count` | EXISTS over a two-hop to-many path, a correlated COUNT, NOT EXISTS, a negated range, all joined by OR |
| `group-having` | an INNER join to the aggregated relationship, GROUP BY, HAVING, and ORDER BY an aggregate |
| `multipath-two-joins` (multipath.json) | the same table joined twice by two relationships, each with its own alias |

Each has a reviewed `.golden` next to it. **Please read the five goldens.**
They are the contract this gate approves.

Five more plans must be refused: an identifier injection
(SECURITY_REJECTED, not echoed), an unconfigured join (SECURITY_REJECTED), the
same-row ambiguity (AMBIGUOUS_MATCH), a fan-out sum (UNSUPPORTED_QUERY), and a
misspelt key `"filters"` (INVALID_INPUT, not silently ignored).

## Exit criterion

| Requirement | Evidence |
|---|---|
| deterministic | byte-identical over 50 in-process and 3 cross-process compilations of every golden plan |
| no identifier outside the schema | every name is looked up and only registry values are emitted. The compiler re-checks each identifier even when the registry skipped validation (`TestUnvalidatedRegistryCannotInjectIdentifiers`). A fuzz run of 12.6M executions (20 s on each gate run) found no identifier outside the schema and no unaccounted parameter |
| values are parameters | hostile strings (`'; DROP …`, `$1`, a newline) never reach the SQL text. The only literals in any output are the LIMIT integer and `ESCAPE '\'` |

## Your Phase 6 instructions, and where each landed

| Instruction | Status |
|---|---|
| no retrieval or ranking interface changes | none. `internal/ranking` and `internal/retrieval` are byte-identical |
| preserve the Phase 5 binding contract | `internal/binding` is untouched; the planner reads it as it is |
| domain-agnostic | no entity, field or English word appears in Go logic. The direction comes from config, and the default columns come from `selectable` flags |
| refusal over guessing | Seven new refusal classes (decision 015). The two same-row or negation ambiguities are refused, never resolved by a default |
| regression tests: nothing silently discarded | the planner refuses a currency, percent or unit it cannot carry. The accounting test checks that the binding's conditions and aggregates match the plan's, and that each plan condition matches one SQL predicate. A reflection tripwire fails if any binding type gains a field the planner does not handle. Strict plan decoding refuses unknown keys. Every join must be used |
| Phase 1–5 gates pass | all 99 earlier checks still pass inside the 125 |
| TODO + decisions | TODO updated; decisions 015 and 016 added; 014 marked confirmed |

## Failures and surprises

**1. `qferr.CodeOf(nil)` is `INTERNAL_ERROR`.** Mutation testing found that my
lost-join test passed with the check disabled: a *nil* error "had" the expected
code. Both of my affected tests now assert `err != nil`. A Phase 4 test
(`ranking_test.go`, `TestMismatchedRetrievalIsAnInternalError`) has the same
pattern. I checked it by temporarily disabling ranking's check and then
restoring the file byte-for-byte: it still fails, but only because ranking
**panics** further on. I left ranking untouched as you asked. A one-line
`err == nil ||` would make that test honest. Do you want it?

**2. Two mutations survived at first, and are now caught (27/27).** One was the
lost-join test above. The other was the "two different to-many paths" refusal,
which had no test; the multipath fixture now provides one.

**3. The real model reads "500 dollars" differently from the scripted test.**
Ranking takes "dollars" as a mention of `customer.total_spent`, so the word is
**explained**, as a column to show (`SELECT total_spent WHERE total_spent >
500`), not dropped. That is a safe reading. The planner's unit refusal is
exercised by the scripted test, where "dollars" stays unexplained.

**4. The negated fan-out refusal is not reachable on the real model today.**
"whose order amount is **not** above 10000" is refused earlier, at binding:
"is not" is the `neq` phrase and has no value after it. The planner rule is
covered by the scripted test and stands ready for phrasings that do bind.

**5. A test of mine was wrong, not the code.** The predicate-accounting test
first subtracted subquery lines it had never counted. I fixed the counting.

## Judgment calls for the gate

1. **Currency and percent are now refused on compared values.**
   `customers who spent more than $50000` → VALUE_RESOLUTION_FAILED. This
   follows your Decision 1 literally: the config cannot say what "$" means for
   `total_spent`. The way out would be a per-field `unit`/`currency` config key.
   I did not add one, because it is a config-vocabulary change nobody asked for.
   Say if you want it, or want currency accepted when the config is silent.
2. **Two AND conditions through the same to-many relationship are refused.**
   "customers who bought laptops for more than 1000" is AMBIGUOUS_MATCH: the
   same order, or any orders? The alternative is to choose one reading, most
   likely *same related row*, and document it. OR-joined conditions compile,
   because both readings coincide.
3. **NULL semantics.** `neq` and `nin` follow SQL: `country <> 'IN'` excludes
   customers whose country is NULL. `IS DISTINCT FROM` would include them.
4. **Default columns.** A listing that names no field shows every
   `selectable` field of the root, in config order.

## Deliberate non-results

- **No sentence → SQL surface.** `--query` still refuses with
  NOT_IMPLEMENTED, and `--plan` prints no SQL. Wiring them together is Phase 7.
- **HAVING is reachable only from hand-written plans.** Binding cannot express a
  condition on an aggregate.
- **The "one of" collision is unchanged.** It is scheduled for Phase 8 config
  validation (decision 016).

## Gate

```bash
cd v2 && ./scripts/run-manual-tests.sh
```

There are 125 checks. Section 14 (no model) covers the goldens,
cross-process byte-identity, the identifier tests plus a 20 s fuzz, the five
refusals, and redaction. Section 15 (real model) runs `--plan` on the gate
sentences with a pinned clock. Phase 7 does not begin until you confirm.
