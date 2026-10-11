# 016 — Phase 5 gate decisions: strict unexplained words, and the "one of" collision

**Status:** accepted (user decision at the Phase 5 gate) · **Phase:** 5 → 6, 8 · **Date:** 2026-09-21

These are the two judgment calls Phase 5 left open. The user decided both when
approving the Phase 5 gate. They are recorded here as given.

## Decision 1: unexplained words stay refused

> Keep the current strict behavior. If a meaningful part of the user's request
> cannot be explained by the semantic plan, refuse the request rather than
> silently dropping the word. This is intentional safety behavior. Do not
> introduce a hardcoded list of "harmless" English words just to make examples
> pass.

Decision 014's rule 16 stands unchanged. `customers who bought something` stays
refused, and no stop-word or "harmless word" list will be added to the engine.

**How Phase 6 applies it.** The rule now reaches past binding. The planner
refuses the attributes of a number that binding kept but nothing consumed:
currency, percent, and a unit word on a compared value. All three are
VALUE_RESOLUTION_FAILED (decision 015). The one exception is decision 014's
unit after a count, which is kept as a visible note. Regression tests:
`TestPlanRefusesWhatTheNumberSaidAboutItself`,
`TestUnexplainedWordsStillRefusedBeforePlanning`,
`TestEveryBoundElementReachesTheSQL`, and the reflection tripwire
`TestPlannerAccountsForEveryBindingField`.

## Decision 2: "one of" / "none of" is caught by config validation, in Phase 8

> Choose the config-validation warning approach. Do not make Phase 3
> config-aware. Keep phrase extraction independent of QueryForge configuration.
> In Phase 8, add config validation that detects operator phrases conflicting
> with number-word parsing and emits a clear warning/error as appropriate.

**Nothing changes before Phase 8.** Phase 3 stays config-free, and
`TestOneOfCollidesWithTheNumberOne` keeps pinning today's behaviour: the
sentence is refused, and nothing is mis-bound.

**Phase 8 obligation.** Config validation detects operator, aggregation and
pattern phrases that contain a word Phase 3 parses as a number ("one of"
today). It reports each with the
phrase's path. Whether that is a warning or an error is Phase 8's call. A
phrase that can never form is at least a warning, and the validator currently
has no warning channel, so Phase 8 must add one. The number-word list it checks
against must be **Phase 3's own**, exported from `internal/nlp`, not a copy.
Otherwise the two lists drift apart.
