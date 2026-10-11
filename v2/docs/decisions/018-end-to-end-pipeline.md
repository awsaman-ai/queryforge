# 018 — The end-to-end pipeline, its output contract, and telemetry

**Status:** accepted — output contract and contradiction refusal approved at the Phase 7 gate (019 §6) · **Phase:** 7 · **Date:** 2026-09-22

## Decision

One orchestrator, `internal/pipeline`, runs every request through the same
sequence of stages:

```text
natural language → extract → retrieve → rank → bind → SemanticPlan → compile → final query
```

`--query` uses it, and so do the diagnostic modes (`--rank`, `--bind`, `--plan`)
through `Pipeline.Rank`. A diagnostic therefore never ranks differently from a
real request. The pipeline adds **no semantic rule**. It sequences and times
the stages, stops at the first refusal, and keeps the results of the earlier
stages so that `--explain` can show them.

**No bypass.** The compiler's only input is a `plan.Plan`, and the only path from
a sentence to a plan is `plan.FromBinding`. Nothing in the pipeline builds SQL
or generates anything. The embedding model's role ends at retrieval.

## Output contract (section 15)

```text
SELECT "t0"."name", ...
FROM "customers" AS "t0"
WHERE "t0"."country" = $1;

params:
1 = "IN" (string)
```

- **Parameter values are printed by default.** They are the deliverable: the
  application that executes the query needs them, and the SQL text never holds a
  value. This deliberately differs from `--compile`, `--bind` and `--plan`,
  which are inspection surfaces and redact stored values unless `--debug` is
  set.
- The format differs slightly from the brief's `1 = IN`: strings are quoted and
  each parameter carries its type, so `" IN"` and `IN`, or `5` and `"5"`, cannot
  be confused.
- `--json` gives `{ok, sql, params[{index, type, value}], error, stage}`, where
  `value` is the driver value: a number, a string, a boolean, or an RFC 3339
  instant.
- **Refusals** go to stderr as `CODE: message`, redacted as before (decisions
  006 and 013), with exit status 1 and no SQL on stdout.
- **`--explain`** prints the reading, binding and plan, up to the stage that
  refused. Stored values in it follow `--debug`.
- **The clock** comes from `--now` and `--tz` (default: the wall clock, UTC).
  Every relative date resolves against it at planning time.

## Telemetry (section 24, decision 013's obligation)

`pipeline.Stats` records:

- the stage reached and the error code;
- input length, phrase count and candidate count;
- per-stage and total latency in milliseconds;
- the lowest confidence among the accepted phrases;
- the selected entity, fields and operators;
- the join count, parameter count and query length.

It is built only from structured forms. It holds **no user text and no value**,
so it is safe to log. It is printed as one JSON line on stderr under `--debug`,
and included in `--json --debug`. `TestTelemetryCarriesNoUserTextOrValues`
marshals it after a request full of sensitive words and checks that none
appear.

## Also decided here

| Item | Decision |
|---|---|
| Ranking policy in config (carried from Phase 4) | Optional `ranking: {minScore, minMargin}`, validated at load (`invalid_ranking_thresholds`) with the same ranges as `ranking.Policy.Validate`. The precedence is: calibrated default, then config, then explicit flags (the flags are announced as "overridden") |
| `security.maxInputLength` | Now **enforced**: an empty or over-long request is INVALID_INPUT, never truncated |
| `security.maxCandidates` | **Not enforced yet.** As a request total, the default of 256 would refuse ordinary sentences (a 21-span sentence already retrieves 350 candidates). Whether the limit applies per span or per request is Phase 8's decision |
| Contradictory AND (finding M06) | The compiler refuses two AND-joined `eq` conditions on the same field, reached by the same to-one path, with different values. No row can satisfy both, and the query would silently return nothing. The code is AMBIGUOUS_MATCH, and the message says "or" must be stated. Nothing correct is lost. A Phase 6 file changed in Phase 7; **approved** (019 §6) |
| `--e2e` | Runs a reviewed case file through one warm pipeline. For each case the report gives the input, the expected meaning, the expected shape, the actual query, pass or fail, and a failure category from section 18's taxonomy. It also counts value readings accepted on similarity alone. It includes no timings, so the gate can diff it against a golden |

## Options considered

- **A separate code path for `--query`, with the diagnostic modes keeping
  their own.** Rejected. Two orchestrations would drift, and a diagnostic that
  ranks differently from the real path would lie.
- **Redacting parameters by default, as `--compile` does.** Rejected. It would
  make the default output useless to the caller the experiment exists to serve.
- **Rewriting a contradictory AND to OR.** Rejected: that is a guess. Refusal
  asks the user to say which they meant.

## Evidence

*Superseded where it quotes the query set: after decision 019 (with §5 as
revised at the re-gate) the set is 70/77 with 0 silently wrong accepted
queries, and 54/54 accepted queries are valid PostgreSQL. The figures below
are the first Phase 7 gate's.*

- 74 reviewed cases: **63/74 pass** (queries 48/57, specified refusals 15/17).
  One compiled answer is **silently wrong**; see phase-7-results.md.
- **55/55** SQL outputs (50 from the query set and 5 compiler goldens) parse
  under PostgreSQL's own grammar (libpg_query via pglast 8.4). Each is exactly
  one `SELECT` with contiguous `$n`. This was a parse-only check, with no
  connection, from a throwaway environment outside the project.
- The gate passes 136/136. The race detector is clean, including 32 concurrent
  requests on one pipeline.

## What would cause us to revisit it

A consumer that needs parameters redacted by default. Phase 9's performance
work splitting embedding from retrieval latency. A second backend (Phase 11),
which would add a compiler per dialect behind the same plan.
