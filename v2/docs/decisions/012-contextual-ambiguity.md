# 012 — Ambiguity is contextual: no word is globally ambiguous

**Status:** accepted (user decision at the Phase 4 gate) · **Phase:** 4 · **Date:** 2026-09-21

## Decision

No rule marks a word as ambiguous in itself. A phrase is ambiguous only when
the configured schema offers more than one reading of it and the sentence does
not separate them. For `amount`:

| Sentence | Config | Outcome | Why |
|---|---|---|---|
| `amount` | ambiguity.json | **AMBIGUOUS** | order.amount and payment.amount are both named amount: a 1.000 tie |
| `order amount` | ambiguity.json | order.amount | an exact configured synonym |
| `payment amount` | ambiguity.json | payment.amount | an exact configured synonym |
| `payments with amount over 100` | ambiguity.json | payment.amount | entity (payments named) + type (a number follows) + operator (`gt` allowed): 1.400 vs 1.200 |
| `orders with amount over 100` | ambiguity.json | order.amount | the mirror image |
| `amount` | commerce.json | **order.amount** | only one field is *named* amount. total_spent says "amount spent". Margin 0.374 |

## Context

Phase 2's report called commerce `amount` → order.amount "wrong to be
confident", because customer.total_spent and product.price are plausible
readings. At the Phase 4 gate the question went to the user, and the answer
was to keep the current behaviour.

The alternative was a signal like "an unqualified word appearing in several
fields' descriptions is ambiguous". That would be one row of evidence fitted
into a rule, and it would contradict the config: the author of commerce.json
named exactly one field `amount`. The experiment's premise is that the config is
the source of truth about vocabulary. A rule that overrides an explicit name
because *other* fields' descriptions mention the word would put an English-usage
guess above the schema.

## Consequences

- Whether `amount` is ambiguous is a property of **the config**, which is where
  the author can change it: add a second field named amount, or a synonym, and
  the behaviour follows.
- Commerce `amount` is now a **scored** row (field:order.amount) in
  `phase4-commerce.tsv`, not an exploratory one.
- Pinned by `TestAmountIsResolvedByContextNotByRule`. Each case also asserts
  *which evidence* decided it, so a future change that reaches the right answer
  for the wrong reason still fails. The same six outcomes are gate checks in
  section 11 of `run-manual-tests.sh`.

## What would reopen this

A Phase 10 failure where the accepted single-name reading is measurably wrong
for real users. Then the fix belongs in config guidance or a measured signal,
not a word list.
