# 014 — Binding rules: positional, schema-checked, and strict

**Status:** accepted — confirmed at the Phase 5 gate (see [016](016-phase-5-gate-decisions.md)) · **Phase:** 5 · **Date:** 2026-09-21

## Decision

The binder turns ranking's accepted reading into a checked meaning using
**only** the order of the readings in the sentence and the configured schema.
It never re-ranks, never re-embeds, and never consults similarity. When a rule
cannot place something, the sentence is **refused**; nothing is dropped.

## The rules

| # | Rule | Why |
|---|---|---|
| 1 | **Root** = the first entity named. Otherwise the entity of the first field or value. Otherwise the `from` side of the first relationship | English puts the subject first in every shape handled here, and a person can check "first" by eye |
| 2 | A **subject** (field, relationship, or a value of an unclaimed field) takes the items after it, up to the next subject or an and/or connective | The brief's example is exactly this shape: `spent` · `more than` · `50000` |
| 3 | Within that tail, the **last operator** before the operand is the comparison. No operator means `eq`, or a date **window** for a date | "is above" is one comparison, and "is" never reaches the binder (Phase 3 drops function-word spans) |
| 4 | A value alias no field claimed is **its own condition** on its field | "customers from India" |
| 5 | A relationship followed by a number is a **count** of related rows | "ordered more than 3 times". Refused unless the root is one end and the other end is reached to-many: a to-one count is always 0 or 1 |
| 6 | A relationship or entity mentioned with nothing attached is an **EXISTS**, if the path from the root fans out. Otherwise it is a note | "customers who ordered" restricts, but "orders placed by customers" cannot (every order has one). The **configured cardinality** decides, not the wording |
| 7 | Paths come from the schema graph: the one using the most **mentioned relationships**, then the shortest. A tie is AMBIGUOUS_MATCH | Section 13: "multiple join paths equally plausible and not disambiguated" must be rejected. "shipped to" vs "billed to" chooses |
| 8 | Paths are resolved **before** mentions become EXISTS | Otherwise "customers who bought laptops" also gains a redundant `exists(customer_orders)` (found on the first real run) |
| 9 | An **orphan comparison** (operator and value, no field) binds to the root's one field that fits. Several fit → AMBIGUOUS_MATCH | "orders over 100" could mean `id` or `amount`, so it is refused. It is never guessed |
| 10 | An **orphan date** binds to a date field named in its clause, else the root's only date field | "orders last month" → `placed_at`, marked *inferred* |
| 11 | **Aggregates** take the nearest unclaimed field mention to their right, else their left. `count` with no field counts rows | "average order amount", "how many customers from India" |
| 12 | **top_n** needs a count beside it and a metric field. **time_window** needs a date beside it | Neither has a configured default. "recent customers" is VALUE_RESOLUTION_FAILED, not a guessed 30 days |
| 13 | **Values**: numbers only for number fields, dates only for date fields. Enum fields accept only a declared value or alias. String fields take free text as a literal | "customers with status cancelled" is refused: guessing *suspended* would be silent substitution |
| 14 | **Negation** complements the operator (`gt` → `lte`). An operator with no configured opposite (`like`) is refused | "not" must never be silently dropped |
| 15 | **and/or**: conditions join by what sits between them, `and` by default. A sentence mixing both is refused | "A and B or C" has two readings, and precedence is a guess the user did not make |
| 16 | **Unexplained words are refused**, with one exception: exactly one word directly after a number is that number's *unit* ("3 times", "500 dollars"), kept and not interpreted | See below |
| 17 | More conditions than `security.maxFilters` → SECURITY_REJECTED | Section 16.7. The limit already existed in the config |

## The strictness rule, and its cost

Phase 4 reported unexplained words and left the judgement to Phase 5. The
measurement that decided it: retrieval cannot tell a harmless word from a
dropped filter. `Atlantis` scores 0.384 toward `customer.country`, while
`times` scores 0.401 toward a relationship.

Ignoring unexplained words would bind `customers from Atlantis` as **all
customers**: a broader answer than asked, with nothing in the output to show it.
That is the failure v1's BUG-057 was about (a repair that "succeeds" by
dropping a predicate). So binding refuses.

**The cost is real.** `customers who bought something` is refused because
"something" is unexplained, although ignoring it would be right. The unit rule
is the only exception, and it is positional and bounded: one word, directly
adjacent to a number, never interpreted. A word list ("something", "anything",
"times") was considered and declined. It would be English hardcoded into the
engine, which the brief rules out, and it would grow without end.

## Options considered

- **Ignore unexplained words.** Rejected above: silently broader answers.
- **Judge them by retrieval score.** Rejected on measurement: the scores do not
  separate.
- **Choose the root by frequency or by the most conditions.** Rejected. It is
  harder to predict, and it changes when a condition is added.
- **Default AND/OR precedence.** Rejected, because it guesses. Refusal costs one
  rephrase.
- **Shortest path only, ignoring mentions.** Rejected: the multipath fixture has
  two equal-length paths, and only the verb separates them.

## Trade-offs and limits

- **Direction of top_n is not bound.** `PatternIntent` has no direction.
  `top_n` and `bottom_n` bind identically, apart from their names. *Resolved
  in Phase 6:* the planner reads the direction from the pattern's new `order`
  config key (decision 015).
- **Counts only through relationships that touch the root.** "products
  ordered more than 3 times" is refused: counting through `customer_orders`
  from a product root would need a choice of what to count.
- **Only the first refusal is reported.** It is deterministic, because items are
  processed left to right.
- **Self-referencing relationships are never traversed** (simple paths only).
