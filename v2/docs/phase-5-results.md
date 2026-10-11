# Phase 5 results: semantic binding and the schema graph

**Date:** 2026-09-21 · **Status:** built and self-verified; **awaiting the
user's manual gate**. Gate: **99/99 checks, no skips**. `go test -race
-count=1 ./...` is clean.

## What Phase 5 is for

Phase 4 decides what each phrase *is*. Phase 5 decides what the sentence
*means*: which entity it is about, which conditions apply, through which joins,
and what is counted, aggregated or ranked. Section 13 of the brief:

> Combine independent candidates into a coherent query meaning. ... The graph,
> not an LLM, determines how those entities can connect.

Retrieval and ranking are **unchanged**. The binder consumes `ranking.Result` as
it is, and no Phase 2–4 interface changed.

## What was built

| Package | Role |
|---|---|
| `internal/graph` | Schema graph. Every relationship is traversable both ways, with join keys and to-one/to-many **per step**. Simple paths within `maxJoins`. `Resolve` prefers the paths the sentence's verbs mention, then the shortest, and refuses ties |
| `internal/binding` | `Bind`: root entity, conditions (compare / window / count / exists), aggregates, top-N and time-window patterns, selections, and AND/OR joining. `binding.Error` redacts the phrase, like `VerdictError` |
| `cmd/qf-v2 --bind` | The whole path: extraction, retrieval, ranking, binding. It shows the reading on a refusal, so the refusal can be argued with |

The rules, and why each exists, are in
[decision 014](decisions/014-binding-rules.md).

## The brief's gate sentences (real model, commerce.json)

```console
$ qf-v2 --config testdata/configs/commerce.json --bind "customers from India" --debug
  root: customer
  conditions:
         customer.country eq "IN" (alias)

$ qf-v2 ... --bind "customers who bought laptops" --debug
  root: customer
  joins:
    customer -[customer_orders]-> order -[order_product]-> product
        customer.id = order.customer_id  (customer_orders, to-many)
        order.product_id = product.id  (order_product, to-one)
  conditions:
         product.category eq "laptop" (alias)  via customer -[customer_orders]-> order -[order_product]-> product

$ qf-v2 ... --bind "customers who ordered more than 3 times"
  conditions:
         count(customer_orders) gt 3  via customer -[customer_orders]-> order
  note: a unit word after the count through customer_orders is kept but not interpreted

$ qf-v2 ... --bind "customers whose order amount is above 10000"
  root: customer
  conditions:
         order.amount gt 10000  via customer -[customer_orders]-> order
```

Without `--debug`, the stored values print as `<redacted> (alias)`.

## Exit criterion

> **PASS when multi-field and multi-entity semantics can be bound without model
> generation.**

| Shape | Sentence | Bound |
|---|---|---|
| multi-field, one entity | customers from India who spent more than 50000 | `country = IN` AND `total_spent > 50000` |
| multi-entity, two-hop join | customers who bought laptops | `product.category = laptop` via customer → order → product |
| condition on a joined entity | customers whose order amount is above 10000 | `order.amount > 10000` via customer → order |
| count through a relationship | customers who ordered more than 3 times | `count(customer_orders) > 3` |
| existence | customers who ordered | `exists(customer_orders)` |
| path chosen by the verb | orders shipped to customers in Ruritania | via `order_shipping_customer` (billing also exists) |
| OR | customers from India or United Kingdom | `country = IN` OR `country = UK` |
| negation | customers not from India | `country neq IN` |
| aggregate | average order amount / how many customers from India | `avg(order.amount)` / `count(customer.*)` + filter |
| top-N | top 5 customers by spending | N=5, metric `total_spent` |
| date window, inferred field | orders last month | `order.placed_at` within previous calendar month (*inferred*) |

No model generates any part of this. The embedding model's only role ends at
retrieval (Phase 2). Everything after it is deterministic code over the config.

## Required rejection cases (section 13), all on the real model

| Case | Sentence | Code |
|---|---|---|
| candidates cannot be connected | customers with warehouse capacity over 100 *(multipath)* | RELATIONSHIP_NOT_FOUND |
| equally plausible join paths | orders from customers in Ruritania *(multipath)* | AMBIGUOUS_MATCH, listing both paths |
| field incompatible with the operator | customer spending contains 5 · customer name over 5 | UNSUPPORTED_QUERY |
| aggregation invalid for the field type | average customer name | UNSUPPORTED_QUERY |
| value cannot be resolved safely | customers with status cancelled · recent customers | VALUE_RESOLUTION_FAILED |

Plus binding's own: an unexplained word (`customers from Atlantis`), a mix of
and/or, and more conditions than `maxFilters`.

## Failures and surprises

**1. A redundant EXISTS.** The first run bound `customers who bought laptops`
with an extra `exists(customer_orders)` beside the category condition that
already joins through it. The cause was that mentions were turned into EXISTS
before paths were resolved. Paths now come first (rule 8).

**2. "one of" collides with the number one.** The in-operator phrase `one of`
contains a word Phase 3 parses as the spelled number 1. The span swallows a
number, takes the span penalty, and the operator never forms. The sentence is
**refused**, not mis-bound. That is safe, but the operator is unreachable. It
was found because the test meant to prove the operator allowlist passed *for the
wrong reason*: a mutation disabling the allowlist still passed. It is pinned by
`TestOneOfCollidesWithTheNumberOne`. The fix is a config-vocabulary question,
not a binder one (see *Judgment calls*).

**3. Tests that proved nothing, found by mutation.** 15 binder and graph
rules were each reverted to confirm a test fails. Two initially did not: unit
adjacency, and the operator allowlist (finding 2). Both tests were tightened,
and all 15 are now caught. Separately, disabling `corpus.DisplayID` did not fail
the `--bind` redaction sweep, because a successful binding prints no corpus id
at all. The sweep now includes a *refused* misspelling, whose reading does
print ids, and it catches the mutation.

**4. A test expectation that was wrong.** I expected `customer name India` to be
refused. It binds as *select name where country = India*, which is the better
reading, and the test now says so (`TestValueOfAnotherFieldIsItsOwnCondition`).

**5. A gate check that contradicted decision 013.** It grepped stdout and
stderr together for the user's own word, and failed on the report's legitimate
echo. It now checks the error stream only, which is where 013 draws the line.

## Coverage

57 Go files, ~16,900 lines. **274 test functions** (was 221): 10 in graph, 39 in
binding, 4 CLI. All pass under `-race`. The gate script is **99 checks** (was
81).

## Judgment calls for the gate

1. **Strictness.** Unexplained words are refused (decision 014). It costs
   `customers who bought something`, which is refused although ignoring
   "something" would be right. The alternative, a word list, is English
   hardcoded into the engine. I chose refusal. Say if you want otherwise.
2. **"one of" / "none of".** Choose between: (a) config validation warns when an
   operator phrase contains a number word; (b) Phase 3 skips number words inside
   configured phrases, which would give extraction a config dependency it
   deliberately does not have; or (c) leave it, with the phrase unreachable but
   safe. I lean (a), in Phase 8. Nothing is changed until you decide.

## Deliberate non-results

- **No SemanticPlan and no SQL.** A binding is checked meaning. The planner and
  compiler are Phase 6. `--query` still refuses.
- **top-N has no direction.** `PatternIntent` carries none, and revisiting it is
  scheduled for Phase 6.
- **Dates stay descriptors.** Resolution to half-open ranges happens at planning
  time, with the request's clock and timezone (Phase 3's decision).
- **No new dependencies.** Still zero third-party Go modules.

## Gate

```bash
cd v2 && ./scripts/run-manual-tests.sh
```

99 checks, all passing. Section 12 covers binding mechanics, redaction and
determinism (model-free). Section 13 covers the brief's four sentences,
multi-condition and multi-path binding, and every required rejection on the real
model. Phase 6 does not begin until you confirm.
