# 015 — The SemanticPlan, the planner, and the query grammar

**Status:** accepted — confirmed at the Phase 6 gate (see [017](017-phase-6-gate-decisions.md)) · **Phase:** 6 · **Date:** 2026-09-21

## Decision

A typed **SemanticPlan** (`internal/plan`) states a query's meaning in the
config's own names. A **planner** (`plan.FromBinding`) builds it from a Phase 5
binding. A **compiler** (`internal/compiler`) checks, lowers and renders it as
PostgreSQL-style SQL by a fixed grammar, with every value as a `$n` parameter.

```text
binding.Binding ──FromBinding(clock)──▶ plan.Plan ──check──▶ checked ──lower──▶ grammar nodes ──render──▶ SQL + params
```

The plan says **what** (e.g. "customers with some order over 10000"). The
compiler decides **how** (e.g. `EXISTS`), from configured cardinality.

## The plan

| Element | Holds |
|---|---|
| `root` | an entity name |
| `select`, `aggregates`, `orderBy` | `entity.field` plus `via`: relationship names from the root |
| `where` | `compare` (op + values), `range` (half-open, resolved instants), `exists`, `count`, joined by one connective |
| `having` | aggregate, op, number |
| `limit`, `distinct`, `notes` | `notes` are never compiled; they record what the sentence said that the plan keeps only as a note |

It holds no table name, no column name and no SQL fragment. Dates are resolved
**before** a plan exists, so a plan is a pure value and always compiles to the
same bytes. Hand-written plans are decoded **strictly**: an unknown key such as
`"filters"` for `"where"` is refused, because otherwise it would silently drop
the conditions.

## Shape rules (how the compiler turns paths into SQL)

| Case | SQL | Why |
|---|---|---|
| a path that stays to-one | `LEFT JOIN` chain from the root | Each root row meets at most one row, so the root's rows are neither lost nor repeated. A condition on the joined column still filters, because a NULL never satisfies it |
| a condition through a to-many step | its own correlated `EXISTS (…)` over the whole path | It restricts root rows without repeating them, and it combines with OR as freely as a comparison |
| `count` through a relationship | correlated `(SELECT COUNT(*) …) op $n` | Same reasons. `GROUP BY … HAVING COUNT` could not be ORed with a plain condition, and would need a key the config does not declare |
| aggregates over related rows ("the domain") | that one to-many path joined `INNER` | The aggregate ranges over related rows that exist, so `COUNT(*)` never counts a NULL-extended row |
| selections alongside aggregates | `GROUP BY` those selections | Showing a column beside an aggregate has no other valid SQL |
| `ORDER BY` | always `NULLS LAST` | PostgreSQL sorts NULLs **first** under `DESC`, so "top 10 by spending" would open with unknown spenders |
| `like` | `col LIKE $n ESCAPE '\'`, param `%` + escaped text + `%` | The compiler owns the wildcards. The user's `%`, `_` and `\` are escaped |
| identifiers | from the registry only, re-checked against `[A-Za-z_][A-Za-z0-9_]*` (at most 63 characters), always double-quoted | Defence in depth: a registry can be built from an unvalidated config. Quoting keeps a reserved-word column legal and takes config names literally, case included |
| aliases | `"t0"`, `"t1"`, … numbered in plan order, subqueries continuing the count | Deterministic, never user-derived, and unique even when one table is joined twice (the multipath fixture) |

## Refusals the compiler adds

Each rule below was mutation-tested (27 rules: 27 caught).

| Refused | Code | Reason |
|---|---|---|
| a name the config lacks: entity, field, relationship, operator or function | SECURITY_REJECTED | The allowlists of sections 16.2 and 16.4 to 16.6. The name is redacted in the message |
| a non-selectable field in `select`, `orderBy` or an aggregate; a non-filterable one in `where` | SECURITY_REJECTED | Sorting by or aggregating a hidden column discloses it |
| more conditions than `maxFilters`; SQL longer than `maxQueryLength` | SECURITY_REJECTED | Section 16.7 |
| listing fields through a to-many path | UNSUPPORTED_QUERY | Each root row would repeat once per related row |
| two different to-many paths in the output | UNSUPPORTED_QUERY | They cross-multiply |
| an aggregate that does not range over the domain (e.g. `SUM(customer.total_spent)` beside orders) | UNSUPPORTED_QUERY | Each value would be counted once per related row |
| `count` through a to-one path | UNSUPPORTED_QUERY | The count is always 0 or 1 |
| two AND-joined conditions through the same to-many step | AMBIGUOUS_MATCH | The same related row, or any related rows? ("bought laptops over 1000") |
| a condition through the domain's own to-many step | AMBIGUOUS_MATCH | Does it restrict the aggregated rows or the root rows? |
| enum value outside the declared set, NaN or Inf, NUL in text, an empty range | VALUE_RESOLUTION_FAILED | |
| `LIMIT` without `ORDER BY`; ordering by an ungrouped field; `DISTINCT` with aggregates; wrong value count | COMPILATION_ERROR | Invalid or arbitrary SQL |

## Planner rules

The planner **accounts** for every element of a binding: each one is planned,
refused, or carried as a note. `TestPlannerAccountsForEveryBindingField`
enumerates every field of every binding type by reflection. It fails when a
later phase adds one, until someone decides what the new field means.

| Rule | Code |
|---|---|
| A number carrying a **currency**, a **percent sign**, or a **unit word**, compared with a field, is refused. The config declares no unit for any field | VALUE_RESOLUTION_FAILED |
| A unit after a **count** ("3 times") is kept as a note (decision 014's exception). It cannot change how many rows exist | — |
| A **negated** condition through a to-many path is refused. "Not above 10000" could mean no order above it, or some order not above it | AMBIGUOUS_MATCH |
| A period compared with a date maps to half-open bounds: after → `>= end`, before → `< start`, in → range, not in → `NOT` range | — |
| **top_n** direction comes from the config's new optional `order` key (`asc` or `desc`; top_n only; validated by `invalid_pattern_order`). If it is missing, the pattern is refused, not guessed | CONFIG_ERROR |
| An **aggregate**-intent pattern names no function or field, so it is refused. `filter` and `time_window` add nothing beyond their conditions | UNSUPPORTED_QUERY |
| A listing with no selection shows the root's **selectable** fields in config order. This is the "configured query policy" section 15 leaves open, and the config's own flags decide it | — |
| Every join the binding recorded must be used by a plan element | INTERNAL_ERROR |

## Options considered

- **JOIN + DISTINCT for to-many conditions.** Rejected. DISTINCT over the
  projected columns merges distinct root rows that share values (two customers
  both named "Smith"). Deduplicating by a key needs a primary key, which the
  config does not model.
- **JOIN + GROUP BY + HAVING for counts.** Rejected: it cannot be ORed with a
  WHERE condition, and it needs a key to group by.
- **One EXISTS per condition, always.** This is correct and uniform, but it
  would turn "orders for laptops" (to-one) into a subquery for no benefit. To-one
  paths use joins.
- **Picking the "same related row" reading for AND conditions through one
  to-many step.** Rejected. It is a guess, and refusal costs one rephrase.
- **Unquoted identifiers.** They would read better, but break on a reserved word
  and fold case silently. Always quoting takes the config literally.
- **Adding a `bottom_n` intent instead of an `order` key.** Rejected. It changes
  a closed enum that four configs already use, and a new intent would still need
  a direction for anything else that ranks.
- **Parameterising LIMIT.** The brief's own examples write `LIMIT 10`, and the
  value is an `int` formatted by `strconv`, never text.

## Trade-offs and limits

- **Currency and units** stay refused until per-field unit metadata exists,
  and the same-record ambiguity stays refused with no global rule (017 §1, §2).
- **Refusals cost reach.** These are now refused: "customers who spent more than
  $50000", "customers who bought laptops over 1000" (same-row ambiguity), and a
  negated condition through orders. Each has a config-driven or phrasing-driven
  way out, noted in TODO.
- **Negation follows SQL's three-valued logic, INTENTIONALLY.** `country <>
  'IN'` excludes customers whose country is NULL. This is a semantic rule,
  not an accident: see [017](017-phase-6-gate-decisions.md) §3 before changing it.
- **LIKE is case-sensitive.** `ILIKE` is PostgreSQL-only, and the scope is
  "PostgreSQL-style". Revisit if Phase 10 shows case misses.
- **No tie-breaker in ORDER BY.** The SQL is deterministic; the database's row
  order among ties is not. A tie-breaker needs a declared key.
- **`maxJoins` is per path, not per query.** The same holds for the graph.
  Phase 8 owns query-wide complexity limits.
- **HAVING is reachable only from hand-written plans.** Binding cannot express a
  condition on an aggregate yet. The grammar supports it and is tested.

## Evidence

- 5 golden plans, byte-identical across 50 in-process and 3 cross-process
  compilations.
- A fuzz test ran 12.6M executions in 45 s (20 s on every gate run). Every
  output held only configured identifiers, and every parameter was accounted
  for.
- 27 of 27 rule mutations are caught. Two survived at first: the two-domain
  refusal had no test, and the lost-join test passed for the wrong reason
  (`qferr.CodeOf(nil)` is `INTERNAL_ERROR`). Both are fixed.

## What would cause us to revisit it

A Phase 10 failure pattern concentrated in any refusal above; a config that
needs a primary key (tie-breakers, DISTINCT by key); a backend other than
PostgreSQL (Phase 11).
