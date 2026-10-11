# Phase 8 results: security, ambiguity and hardening

**Date:** 2026-09-22, extended 2026-09-23 · **Status:** **CLOSED** — the phase
passed its final gate on 2026-09-23, with **zero silently wrong accepted
queries in every corpus**. It took three rounds: the first gate kept Phase 8
open for four items (round 2); at the re-gate the user approved the rule for
G33/G34 and asked for `list price of products` to be investigated (round 3);
the final gate accepted round 3 and decided G36. Round 1's choices are in
[decision 021](decisions/021-phase-8-security-hardening.md), the gate rounds'
in [decision 022](decisions/022-phase-8-gate-decisions.md) §1–§7.

## Final gate — Phase 8 closed (2026-09-23)

The acceptance criterion the phase was measured against — **zero silently
wrong accepted queries** — is met. The evidence the gate passed on:

| Check | Result |
|---|---|
| Manual gate, phases 0–8 (`scripts/run-manual-tests.sh`) | **170/170**, 0 failed, **0 skipped** |
| Phase 0–7 regressions | all pass |
| Phase 7 query set | **byte-identical** — no outcome moved |
| Generated SQL | **80/80** parse as PostgreSQL |
| Mutation testing | **41/41** caught |
| `go test -race -count=1 ./...` | clean |
| `FuzzPipeline` | **1,418,328** inputs, no failure |
| Silently wrong accepted queries | **0** across every reviewed corpus |

**G36 `list price of products` is intentional behaviour, not a bug.** The user
decided at this gate to keep it compiling and to add no further refusal rule:
`list` is the request verb when `list price` is not a configured target, and a
config that defines `list price` resolves that configured target instead —
both configuration-driven, both domain-agnostic, and §6 is what guarantees the
second half. It is recorded as a known semantic interpretation in
[decision 022 §7](decisions/022-phase-8-gate-decisions.md) and stays an
expected `sql` case in the Phase 8 corpus. It is counted as *correct* in the
table above, not excused from it.

**Phases 0–8 are now closed records.** Nothing in them is edited again unless
a later phase produces a clearly scoped regression.

## Gate round 3 (decision 022 §5 and §6)

| | Round 2 | Now |
|---|---|---|
| Phase 8 set | 89/91, 2 known silent wrongs (G33, G34) | **93/93**, 0 known failures (2 new control cases: G35, G36) |
| Silently wrong accepted queries | Phase 7 set 0, probes 0, Phase 8 set 2 | **0 in every corpus**: Phase 7 (77), Phase 8 (93), the probes, the 70-sentence exploration, an 18-sentence paraphrase stress list |
| Phase 7 query set | 71/77 | **71/77, byte-identical** — no outcome changed |
| Phase 4 probes and sweep | byte-identical / 45 cells identical | **unchanged again** (both rules are binding rules) |
| Valid PostgreSQL | 80/80 | **80/80** (26 accepted Phase 8 queries: G33/G34 now refuse, G35/G36 added) |
| Mutation | 31/31 on §1–§4 | **41/41**: the 31, plus 10 on §5 and §6 (first run 7/10; the 3 survivors were test gaps, now closed) |
| `FuzzPipeline`, 60 s | 767,112 inputs | **1,418,328 inputs**, invariants unchanged |
| `go test -race -count=1 ./...` | 19 packages clean | **19 packages clean** |
| Manual gate, phases 0–8 | 167/167 | **170/170, 0 failed, 0 skipped** (3 checks added for §5 and §6) |

### Changed outcomes, relative to round 2

Only two, both the findings the user scoped:

| Case | Round 2 | Now |
|---|---|---|
| G33 `products with price and discount above 10` | compiled to `price > 10` (known failure) | UNSUPPORTED_QUERY (§5), expectation REFUSED → UNSUPPORTED_QUERY |
| G34 `price of discount` | compiled to `SELECT price` (known failure) | UNSUPPORTED_QUERY (§5), same expectation change |

Nothing else moved anywhere: the Phase 7 report is byte-identical, the other
91 Phase 8 cases are unchanged, and so are the probes, the sweep, the
exploration and the paraphrase list.

### `list price of products`: the investigation

**It is a separate issue, not semantic completeness.** In §1 and §5 a word is
discarded — the reading has no role for it. Here the opening request verb has
the role English gives it: it IS the request. "list price of products" reads
as "list the price of products", exactly as "list customers from India" reads
as a command.

**What the config decides.** With a config that has a `list_price` field
(synonym "list price"), the configured field won in all 18 phrasings tried,
sentence-initial and elsewhere; none silently fell back to `price`. With
commerce, which has no such concept, the words name no other configured
target, so the command reading is the only one the config supports.

**The rule added (§6)** turns that into a guarantee: a configured surface of
more than one word that runs from the verb takes the verb out of the request
frame, and the reading must then name that target literally or be refused.
Contiguity is required — the looser "spelled out anywhere" test refused
`list products by price`, an ordinary command. Measured cost: no outcome
changed in any corpus.

**Residual risk, and why it is not a silent wrong.** A user who means a term
the config lacks gets the command reading. No configured target is discarded
and no word is ignored that the config could explain, so it is not a silently
wrong accepted query by this project's definition; the remedy is to configure
the term, as for 021 §6's "signup date". `show list price of products` stays
refused, because there the request slot is already filled and no configured
phrase explains "list".

### The acceptance criterion

**Zero silently wrong accepted queries.** Every accepted query was re-read
against its sentence:

| Corpus | Accepted | Silently wrong |
|---|---|---|
| Phase 7 query set (77 cases) | 49 | 0 |
| Phase 8 set (93 cases) | 26 | 0 |
| Phase 4 probes (52 scored rows) | — | 0 |
| Exploration of §1's shape (70 sentences) | 46 | 0 |
| Paraphrase stress list (18 sentences) | 15 | 0 |

The one accepted query a reviewer should look at deliberately is **G36**
`list price of products` → `SELECT "t0"."price"`. It is accepted by design
after the §6 investigation: commerce has no list-price concept, so nothing
configured is discarded, and a config that has one reads it instead
(guaranteed by §6). If you would rather it were refused, that is a decision
to take, not a defect to fix — and §6 is where it would go. **Decided at the
final gate:** kept as it is, recorded as intentional behaviour
([decision 022 §7](decisions/022-phase-8-gate-decisions.md)).

### Regression cover added this round

- §5: the two findings with their reasons; paraphrase with configured evidence
  (two mentions of total_spent); Phase 7's V03 shape, which the wider variant
  refused; a paraphrase of a target named nowhere else; the message naming the
  first explicit mention; descriptions not counting as naming.
- §6: commerce's command reading; the configured field winning; **the
  guarantee** (config has the term, the reading does not use it → refused);
  `list products by price` and `list customers from India` untouched; a
  one-word surface not claiming the verb.
- Corpus: G35 (a §5 control that must still compile) and G36
  (`list price of products`).



## Gate round 2 (decision 022 §1–§4)

The user's four items, and what each produced:

| Item | Rule (the user's) | Built | Result |
|---|---|---|---|
| 1. Semantic completeness | a meaningful qualifier in a candidate phrase cannot be silently discarded: accept only a configured complete phrase, or a qualifier with another valid role | a binding-stage rule over phrases (touching spans that agree), including request verbs in noun position (`internal/binding/complete.go`) | P10, P11 and 4 more silent wrongs of the shape refused; controls unchanged |
| 2. Numeric precision | no float64 for user literals; exact from extraction to parameters | `internal/numeric.Decimal`, used at every stage; malformed literals reported and refused | V02 exact (99999999999999999999999 reaches the parameter); 10 further kinds of silent value change found and fixed |
| 3. Multi-statement input | one request is one query; refuse `;` and SQL comment delimiters by syntax, not keyword | an input-boundary guard before extraction (`internal/inputguard`) | I07 refused; a refused request produces no query and reaches no stage (tested down to the live HTTP endpoint) |
| 4. maxPhrases = 256 | approved; keep and document it; retain the resource-bound tests | documented at the default; a missing both-directions test for input length added | 7 resource-bound tests, run by the gate |

### Summary

| | Previous Phase 8 baseline (round 1) | Now |
|---|---|---|
| Phase 8 set | 53/57 (4 known findings) | **89/91**: the 57, with 6 expectations changed by decision, plus 34 new (G01–G34). The 2 failures are **new known findings** (G33, G34; below) |
| Phase 7 query set | 70/77, 0 silently wrong | **71/77, 0 silently wrong**: only Z04 and Z05 changed; all 49 accepted queries byte-identical |
| Phase 4 probes | 33/34 · 17/18, 0 silently wrong | **byte-identical** |
| Calibration sweep | identical in all 45 cells | **identical in all 45 cells** again |
| Accepted queries that parse as exactly one PostgreSQL SELECT with contiguous `$n` (pglast 8.4) | 69/69 | **80/80**: 49 (Phase 7) + 26 (Phase 8) + 5 compiler goldens |
| Silently wrong accepted queries | Phase 7 set 0; Phase 8 set 4 known (P10, P11, V02, I07) | Phase 7 set **0**; Phase 4 probes **0**; Phase 8 set **2 known** (G33, G34), outside the four items and awaiting scope |
| Mutation run on the new rules | 38/38 (round 1's rules) | **31/31** (first run 21/31: 7 mutants did not build and were rewritten; 3 survivors each exposed a test gap, now closed; the rerun exposed a real flaw, fixed) |
| `FuzzPipeline`, 60 s | 412,551 inputs | **767,112 inputs**, with 2 new invariants: statement syntax never passes the input stage, and every number parameter equals a number the sentence holds, digit for digit |
| `go test -race -count=1 ./...` | clean, 16 packages | **clean, 19 packages** (new: `numeric`, `inputguard`, and `api`'s first test) |
| Manual gate, phases 0–8 | 159/159 | **167/167, 0 failed, 0 skipped**: the 159 checks, all still green (one reworded: the stacked-DROP check no longer uses ";"), plus 8 in the new section 22 |

### Every changed result relative to the previous Phase 8 baseline

**Phase 8 set (the 57 original cases).**

| Case | Before (round 1) | Now | Why |
|---|---|---|---|
| W16, I01, I04 | SECURITY_REJECTED at rank (write keyword) | SECURITY_REJECTED at **input** (statement separator) | 022 §3 runs first. Same code, and the expectation is unchanged |
| I02 `' OR 1=1 --` | NO_SEMANTIC_MATCH (expect REFUSED: pass) | SECURITY_REJECTED at input (pass) | 022 §3 |
| I07 `…; select * from orders` | **compiled** (known failure) | SECURITY_REJECTED at input | 022 §3. Expectation REFUSED → SECURITY_REJECTED |
| I08 `customers/**/from/**/India` | compiled (expected: sql) | SECURITY_REJECTED at input | **expectation changed by decision** (022 §3) |
| I09 `customers named Smith; --` | compiled (expected: sql) | SECURITY_REJECTED at input | **expectation changed by decision** (022 §3) |
| V01 `… more than 1e309` | UNSUPPORTED_QUERY: the token was silently dropped, so "gt has no value" | VALUE_RESOLUTION_FAILED: malformed number | 022 §2 (a truer reason; still REFUSED) |
| V02 `… more than 99999999999999999999999` | **compiled with 1e23** (known failure) | compiles with **99999999999999999999999** | **expectation changed by decision** (022 §2): VALUE_RESOLUTION_FAILED → sql with the exact parameter |
| P10 `show product discount price` | **compiled to product.price** (known failure) | UNSUPPORTED_QUERY (the qualifier rule) | 022 §1. Expectation REFUSED → UNSUPPORTED_QUERY |
| P11 `show product list price` | **compiled to product.price** (known failure) | UNSUPPORTED_QUERY (a request verb in noun position) | 022 §1, as P10 |

Every other original case has the same outcome, code and stage as before.

**Phase 7 set.** Only two cases changed; decision 022 §3 changed both
expectations:

- **Z04** `' OR 1=1 --`: NO_SEMANTIC_MATCH → SECURITY_REJECTED at input.
- **Z05** `customers named Robert'); DROP TABLE customers;--`: was expected
  `sql` and refused as DROP (a known failure); it now expects and gets
  SECURITY_REJECTED at input. 70/77 → 71/77.

`docs/phase-7-results.md` is unchanged.

**New cases (G01–G34), expectations written before their first run.**

- **§1 qualifier refusals (G01–G08).** Discount/list/cost price, a request verb
  in noun position (`return date`, `display name`, `customers who return
  orders`), and the head-first `price discount`.
- **Controls, all still compile (G09–G15).** Unit price, the configured
  synonym, `customer amount spent`, `money spent`, `list customers …`, `give
  me a list of …`, and `average price`.
- **§2 exact values (G16–G19, G23, G24).** `-5`, `.5`, 19 decimal places,
  `4.35k`, LIMIT 2^53+1, and a 23-digit count.
- **§2 refusals (G20–G22, G25, G26).** `1,5`, `12x`, `one billion billion`,
  window counts 2.5 and 0.
- **§3 boundary (G27–G32).** A trailing `;`, `--`, `*/`, a fullwidth `;`, and a
  zero-width space inside `--`; plus the control `top-5 …`, which compiles
  (a hyphen is not a sign or a comment).
- **G33, G34.** Two known failures (next section).

The first run matched every G01–G32 expectation (then 89/89). G22's refusal
later moved from planning (the leftover "billion" read as a unit word) to
binding (a malformed number), after mutation testing exposed the spelled-scale
flaw that decision 022 §2 records. It was REFUSED both times.

### New findings (scoped at the re-gate; fixed in round 3 as decision 022 §5)

| Case | Sentence | Compiles to | Shape |
|---|---|---|---|
| G33 | `products with price and discount above 10` | `price > $1` (10) | "discount" is accepted as product.price on similarity alone, in a phrase separate from "price" (a connective between), so 022 §1's phrase rule does not see it |
| G34 | `price of discount` | `SELECT price` | as G33, across a function word |

These failed before and after round 2. **The user approved the measured fix at
the re-gate, and round 3 built it (§5).** It was **measured here first**: refuse a phrase read by similarity alone when another phrase
names the same target by a configured word. It refuses both, and changes
nothing in the Phase 7 set, the rest of the Phase 8 set, the 70-sentence
exploration or the probes. A wider form, which treats a field and its own value
as the same target, cost Phase 7's V03 (`clients located in India`).
Details in decision 022 §1.

### Other measurements

- **Exploration (70 sentences of the §1 shape, run before and after).** Seven
  outcomes changed, all refusals of queries that compiled before:
  - the six silent wrongs: P10's shape three times, `show list price of
    products`, and the two condition forms;
  - `show product cost price` (the rule's cost).
  - No control changed: `money spent`, `customer amount spent`, `product unit
    price`, `product item name`, `customer account status`, `order total`, `total
    amount` (SUM), and every other accepted sentence.
- **Limits measured with the real model.**
  - `customers from India select * from orders` (I07 without `;`) compiled
    before and is now refused by the request-verb rule.
  - `show customers list` and `show price list` are now refused: the cost of a
    trailing request verb.
  - `list price of products` still compiles to price: the opening verb is read
    as the command. This is a known limit.
  - `1e5` is now refused; it used to be 100000.
- **The live endpoint**: `POST /v1/noLLM/tosql` in `go_test_qf`, unmodified,
  real model.
  - Statement syntax gets HTTP 400 with SECURITY_REJECTED and no `query`
    field; `go_test_qf`'s own tests pass.
  - `99999999999999999999999` and `0.1234567890123456789` arrive as those
    exact strings in `params`; `-5` and `50000` arrive as JSON numbers.
  - P10/P11 get HTTP 400 with UNSUPPORTED_QUERY.

---

### How to check it

```bash
cd v2
scripts/run-manual-tests.sh      # phases 0-8; section 22 is the gate rounds, section 21 runs the 93-case set
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --query "show product discount price"
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --query "customers who spent more than 99999999999999999999999"
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --query "customers from India; select * from orders"
go run ./cmd/qf-v2 --analyze "customers who spent more than -5"
go test -run XXX -fuzz FuzzPipeline -fuzztime 60s ./internal/pipeline
```

### For the re-gate

1. ~~**G33 and G34**: scope the measured rule, or leave them recorded.~~
   **Decided at the re-gate:** the rule was approved and built (§5, round 3);
   both are now refused, and no corpus holds a silently wrong accepted query.
2. **The costs of §1:**
   - `show product cost price` is refused;
   - `show customers list` and `show price list` are refused;
   - `list price of products` still reads "list" as the command — investigated
     at the re-gate (round 3): a separate issue, and the config decides it (§6).
3. **The costs of §2:** `1e5` (exponent notation) and `30th` (ordinals) are
   refused.
4. **The parameter contract** (§2): a number that is not an int64 reaches
   `api.ToSQL` and the HTTP response as its exact decimal text, a JSON string.
5. **The costs of §3:** any `;` is refused, even inside a value, and so is `--`
   typed as a dash. `#` is not a delimiter.
6. **The eight decision-driven expectation changes:** Phase 7 Z04 and Z05;
   Phase 8 I07, I08, I09, V02, P10 and P11 (P10/P11 and I07 were known
   failures whose expected code is now named).

## Round 1 (before the gate)

The record below is round 1 as submitted to the first gate. Its numbers are the
"previous Phase 8 baseline" above.

### Summary

| | Result |
|---|---|
| Brief's gate: every adversarial case ends in a safe query or an explicit rejection | **57/57.** Every compiled query passed the output guard, and every other case was refused with a code |
| Phase 8 adversarial set | **53/57 pass.** All 4 failures are recorded findings (§ Findings), each a safe query answering a different question |
| Accepted queries that parse as exactly one PostgreSQL SELECT with contiguous `$n` (libpg_query via pglast 8.4, parse-only, throwaway environment) | **69/69**: 49 from the Phase 7 set, 15 from the Phase 8 set, 5 compiler goldens |
| Phase 7 query set | **70/77, 0 silently wrong.** Only Z01–Z03's expected code and Z05's category changed (§ Earlier results) |
| Phase 4 probes | **33/34 · 17/18, 0 silently wrong**; both reports byte-identical |
| Calibration sweep, re-run after the ranking change | **identical in all 45 cells**, 0 silently wrong in every cell (`benchmarks/reports/phase4-calibration-sweep.md`) |
| Mutation run on every Phase 8 rule | **38/38 caught.** First run 29/38: 5 mutations did not compile and were rewritten, and 4 survived and each exposed a real test gap, now closed |
| `FuzzPipeline` (whole pipeline, deterministic retriever) | 412,551 inputs in 60 s, no invariant broken; the gate runs it for 30 s more |
| Embedding worker network connections in normal use | **0**, down from a DNS lookup and a TLS connection to huggingface.co on every start |
| Manual gate, phases 0–8 | **159/159, 0 failed, 0 skipped**: the 146 checks of phases 0–7, all still green, and 13 new |
| `go test -race -count=1 ./...` | clean in all 16 packages (2 new: `readonly`, `configlint`) |

### What was built, by section of the brief

| § | What | Where |
|---|---|---|
| 16.1 read-only | SQL write keywords (the brief's 11 plus MERGE) refused as SECURITY_REJECTED unless a configured phrase in the sentence explains the word literally. Runs before ranking's verdict, in every sentence mode | `internal/readonly`, `pipeline.Admit` |
| 16.2–16.5 allowlists | An **output guard**: a lexer over the finished SQL that admits only the grammar's keywords, configured identifiers, `$n` in order, and the one `'\'` literal; one statement only | `internal/compiler/guard.go` |
| 16.6 joins | graph.Walk (unchanged), plus a query-wide join count | `internal/compiler/limits.go` |
| 16.7 limits | All eight enforced, each defined (021 §1). `maxPhrases` 64→256, `maxCandidates` per span, new `maxNestingDepth`, `maxPlanningMillis` over the deterministic stages | `internal/pipeline`, `internal/compiler/limits.go`, `internal/config` |
| 16.8 ambiguity | A generic word qualified by an unconfigured word is rejected as a reading (021 §6) | `internal/ranking/rank.go` |
| 16.9 no database | Unchanged: import denylist and a source grep | `internal/isolation` |
| 16.10 logging | Audit: every new message names only structure (keywords from a fixed list, token positions, counts, limits). configlint redacts values. Nothing new leaks | — |
| 16.11 local model | The worker runs offline by default. An audit-hook check requires zero connections and is self-tested | `embedding_worker/worker.py`, `netcheck.py` |
| carried from 016, 019 §4 | Config warning channel: phrases shared across kinds, and phrases that never form | `internal/configlint` |

### The adversarial set (57 cases, expectations written before the run)

`testdata/queries/phase8-adversarial.json`, golden
`phase8-adversarial.golden`, byte-identical across processes.

| Group | Cases | Result |
|---|---|---|
| W — write requests (every keyword, a plural, capitals, a stacked write) | 16 | 16/16: 14 SECURITY_REJECTED; "remove" (not an SQL keyword) refused as an unexplained word; "customers created in 2024" compiles |
| I — SQL-injection-like (the brief's list and variants) | 10 | 9/10. I07 is a finding |
| N — identifiers the config does not have | 6 | 6/6 refused |
| L — limits: input length, span count, the brief's "very deep conditions" | 3 | 3/3 |
| U — Unicode: homoglyphs in a word and in a value, zero-width space, RTL override, fullwidth, NUL, emoji | 7 | 7/7. Homoglyphs and fullwidth are refused; invisible characters are ignored; a NUL does not hide DROP |
| V — values: 1e309, a 23-digit number, an impossible date, LIKE metacharacters | 4 | 3/4. V02 is a finding |
| P — the 16.8 shape, with its controls | 11 | 9/11. P10 and P11 are a finding |

**Written before the run, corrected after it (one case).** I10 `customers
named O'Brien` was written as REFUSED, on a guess that Phase 3 splits the
apostrophe and leaves a fragment. The engine produced the designed outcome
instead: `"t0"."name" = $1` with the parameter `O'Brien`, and no quote in the
SQL text. The case's own meaning line asked for exactly that. The expectation
was corrected, and the case says so.

**Not blind.** About 15 inputs had been run while exploring before the set
was written: the brief's list and the "update date" family. Their
expectations follow the rules, but they were not written blind.

### Findings

#### Fixed in Phase 8

1. **"show customers by update date" → `SELECT created_at`** (user-scoped as
   16.8). Also "modification date", "orders by shipping date", "show order
   shipping date", "orders by order shipping date". All are now refused.
   Decision 021 §6 has the rule, the four alternatives measured, the cost
   ("customers by signup date" is now refused) and the residual risk.
2. **The embedding worker contacted huggingface.co on every start** (16.11).
   It is now offline by default, and the revision is still `1110a243…`, read
   from the local cache. The worker README's claim to the contrary was
   corrected.
3. **`--rank` lost its reading for write sentences.** Caused by the first
   placement of the read-only rule, and found by running it. The rule moved
   from the shared ranking step into `Admit`, so `--rank` shows the reading
   and then the refusal.
4. **A config value could leak through a warning** when a synonym shared its
   text. Found while writing the test, and caught by mutation.
5. **A NUL byte in an input made the report a binary file.** Report cells now
   show control and format characters as `\uXXXX`.

#### Recorded, not fixed (each awaits scope)

Each is a safe query (one SELECT, the guard passed, values as parameters)
answering a different question. Decision 021 §8 explains why each is not fixed
here.

| Case | Sentence | Compiles to | Shape |
|---|---|---|---|
| P10, P11 | `show product discount price`, `show product list price` | `product.price` | an unconfigured word inside a wholly present configured phrase ("product … price") |
| V02 | `customers who spent more than 99999999999999999999999` | `total_spent > $1` with 1e23 | numbers are float64 end to end, so a 23-digit literal is rounded |
| I07 | `customers from India; select * from orders` | customers from India who have orders | "select" is a function word and `;` punctuation, so two requests merge |

### Earlier results

- **Phase 7 query set (approved in advance, 021 §9).**
  - Z01 `delete all customers`, Z02 `drop the customers table` and Z03 `run an
    update on every customer` now expect and get SECURITY_REJECTED. Before,
    they were refused as unexplained words; their notes said Phase 8 would add
    the explicit rejection. Z03 was not named in the question, but it is the
    same class.
  - Z05, a known failure, is now SECURITY_REJECTION (DROP) instead of
    UNSUPPORTED_FEATURE.
  - Nothing else changed, and no accepted query changed.
- **Phase 7's closure.** Decision 019, `phase-7-results.md` and X11 (decision
  020) are untouched.
- **Phase 6 goldens.** Unchanged. All are within the new whole-query limits.
  The per-read join count, measured as an alternative, would have refused
  `exists-and-count`.
- **Phase 4.** Both probe reports are byte-identical, and the sweep was
  re-run (summary table).
- **Other code.** `api/` (added by another session during this phase)
  compiles unchanged: `pipeline.New` and `Run` kept their signatures.

### How to check it

```bash
cd v2
scripts/run-manual-tests.sh                        # phases 0-8; sections 20-21 are Phase 8

go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --validate-config
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --query "Delete all customers"
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --query "show customers by modification date"
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --query "customers by joined date"
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --top-k 300 --query "customers from India"
go run ./cmd/qf-v2 --config ./testdata/configs/commerce.json --e2e ./testdata/queries/phase8-adversarial.json
go test -run XXX -fuzz FuzzPipeline -fuzztime 60s ./internal/pipeline
```

### For the gate

1. **The four recorded findings** (P10/P11, V02, I07): scope any of them, or
   leave them recorded.
2. **The cost of 021 §6:** "customers by signup date" is now refused. Accept
   it, or ask for a different rule.
3. **`maxPhrases` default 64 → 256** (021 §1): a changed default, justified by
   measurement.
4. **The Phase 7 query-set change** (Z01–Z03, Z05), including Z03, which the
   question did not name.
5. **I10's corrected expectation.**
