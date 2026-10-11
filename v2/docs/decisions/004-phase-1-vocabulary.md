# 004 — The closed vocabularies, and why they differ from v1

**Status:** accepted · **Phase:** 1 · **Date:** 2026-09-20

## Decision

Field types `string, number, boolean, date, enum`. Operators `eq, neq, gt, gte,
lt, lte, in, nin, like`. Aggregations `count, sum, avg, min, max`. Cardinalities
`one-to-one, one-to-many, many-to-one`. Pattern intents `top_n, count, aggregate,
filter, time_window`.

All five are **closed sets**, validated at config load. Nothing outside them can
reach the compiler, which is what makes the allowlists of section 16 enforceable
rather than aspirational.

## Operators: short ids, diverging from the production library

The production library calls these `equals`, `notEquals`, `contains`
(`internal/ast/ast.go`). The brief's config in section 7 uses `eq`, `gt`, `lt`.
They cannot both be canonical.

| Option | Verdict |
|---|---|
| Brief's short ids | **Chosen** — `testdata/configs/customers.json` stays byte-identical to the specification, so any result measured against it traces back to the spec rather than to an edit I made |
| Production vocabulary | Rejected — one shared vocabulary would help a future v1/v2 comparison, but it means editing the brief's own fixture away from the brief |
| Short ids plus accepted v1 aliases | Rejected — friendlier to config authors, but a closed set with a normalisation table in front of it is no longer closed, and every later phase would have to know which spelling it was looking at |

The cost is real: a side-by-side v1/v2 comparison in Phase 10 will need a
translation table. That is a one-off mapping of nine names, written once, against
a fixture divergence that would otherwise contaminate every measurement.

v2 also carries fewer operators than v1 — no `between`, `startsWith`,
`endsWith`, `regex`, `isNull`, `containsAny`, `containsAll`. Those are outside
the declared Phase 3 scope. `between` is absent for a further reason, below.

## Field types: production's set minus `array`

`array` implies `containsAny`/`containsAll`, which the first experiment does not
support. A type the compiler cannot emit has no business being configurable —
it would validate cleanly and then fail much later, in the planner, as a
confusing internal error.

`enum` earns its place rather than collapsing into `string`. Its declared values
are themselves semantic content — "cancelled" should retrieve a status field —
and a closed set is what lets value resolution *refuse* an unrecognised value
instead of passing it through to a query that returns nothing.

## Two deliberate compatibility omissions

Both are cases where SQL would accept something that is a trap in natural
language. They are in `operatorsByType` and pinned by `TestCompatibilityMatrix`.

**No ordering comparison on strings.** `WHERE name > 'smith'` is valid SQL and
lexicographic nonsense as a question. Worse, allowing it gives the Phase 4 ranker
a place to bind a comparison phrase wrongly: "customers over 50000" could bind
`over` to a string field and produce a query that runs and answers nothing.

**No `LIKE` on enum.** An enum's values are a closed declared set; a substring
search across them defeats the point of declaring them.

The same reasoning refuses `MIN`/`MAX` on strings. `COUNT` is allowed on every
type because it counts rows, not magnitudes.

## No `between` operator

Date ranges are a pair of half-open comparisons — `>= start AND < end` — not a
`BETWEEN`. `BETWEEN` is inclusive at both ends, so "orders in March" written with
it either includes midnight on 1 April or excludes the last instant of 31 March,
depending on the column's precision. The production library models periods as
half-open for this reason, and the experiment matches it.

## many-to-many is refused, not approximated

It needs a junction table, which the `Relationship` model does not express.
Accepting it and treating it as one-to-many would silently duplicate parent rows
— a wrong answer with no error. The validator names it specifically, because it
is the cardinality a config author is most likely to reach for and
"unsupported" alone would read as an oversight rather than a scope decision.

## PatternIntent is provisional

The brief lists `QueryPattern` as a Phase 1 model but never specifies its shape,
and these intents only acquire meaning in Phase 6's planner. The set is closed so
config cannot smuggle in an intent nothing implements; Phase 2 consumes only the
phrases. Expect this enum to change once planning exists — that is a revision,
not a failure.

## Revisit if

- Phase 6's planner needs an intent this set cannot express.
- Phase 10 compares v1 and v2 directly, at which point the operator translation
  table has to be written down.
- A measured failure traces back to one of the refused pairings, which would mean
  the trap it protects against is rarer than the capability it costs.
