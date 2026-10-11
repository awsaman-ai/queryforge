# Phase 1 results — typed config model, validator, schema registry

**Date:** 2026-09-20, amended 2026-09-21 with the redaction contract · **Status:**
built and self-verified; **awaiting the user's manual gate**.

Environment as recorded in [phase-0-results.md](phase-0-results.md): macOS
26.5.2 arm64, Apple M5, Go 1.26.4, library commit `51df283`. Still no embedding
model, no timings, no accuracy numbers — Phase 1 produces none and none should
be quoted from it.

## What was built

Phase 0's thin `RawConfig` reader is gone, replaced by the typed model every
later phase is grounded in.

| Concern | Where |
|---|---|
| Closed vocabularies and compatibility matrices | `internal/config/vocabulary.go` |
| Entity, Field, Relationship, QueryPattern, SecurityPolicy | `internal/config/model.go` |
| Loader, decoder, defaulting | `internal/config/load.go` |
| Validator — 23 named rules | `internal/config/validate.go` |
| Redaction contract — `Sensitive`, dual rendering | `internal/qferr/sensitive.go` |
| SchemaRegistry — indexed, immutable, concurrency-safe | `internal/config/registry.go` |

Section 9 names a `ConfigLoader` and a `ConfigValidator`. In Go those are
functions: neither holds state between calls, and `config.ConfigLoader` would
stutter at every call site. `Load` is the loader, `Validate` the validator,
`NewSchemaRegistry` the registry constructor.

Three design decisions were put to the user before implementation, since all
three would have been expensive to reverse once fixtures existed: the operator
vocabulary, the field-type set, and how far `QueryPattern` should commit. The
outcomes are recorded in [004](decisions/004-phase-1-vocabulary.md) and
[005](decisions/005-config-validation.md).

## Coverage

17 Go files, ~3,750 lines. 66 test functions, 110 cases including subtests. All
pass, including under `-race`.

Two valid fixtures and 18 invalid ones. `customers.json` remains byte-identical
to the brief; `commerce.json` adds the three-entity graph that join traversal,
enums and field flags need.

23 validation rules, each with a name from a closed set. Tests assert on rule
names rather than message text, because the Phase 1 exit criterion is that a
fixture is rejected *for the expected reason* — which is only checkable if the
reason has one.

Two tests guard the guards:

- **`TestGoldenFixtures`** asserts the **exact** rule set each fixture breaks.
  A validator that grows a spurious extra violation is as broken as one that
  misses a real one, and only exact comparison catches the first case.
- **`TestEveryRuleHasAFixture`** fails if a rule has neither a fixture nor a
  recorded alternative. Four rules are covered by in-memory configs instead of
  files, and the test makes that list explicit rather than incidental.

## Exit criteria

> **PASS when config behavior is deterministic and every invalid fixture is
> rejected for the expected reason.**

Both are checked mechanically, not asserted:

- **Deterministic.** `TestRejectionIsDeterministic` loads a four-violation
  fixture 25 times and compares the rendered message byte for byte; the gate
  script repeats it across six separate processes. This is not a formality —
  several rules walk Go maps, whose iteration order is randomised per run, and
  the first draft did shuffle until the sort in `Validate` was added.
- **Expected reason.** All 17 file-based fixtures are checked by rule name in
  section 4 of the gate script, and by exact rule set in the test suite.

## Manual gate

```bash
cd v2 && ./scripts/run-manual-tests.sh
```

Latest run: **36 checks, 36 passed.** The script covers both phases: Phase 0's
isolation and scope checks, plus Phase 1's per-fixture rule assertions,
determinism across processes, multi-problem reporting, and the redaction
contract in both directions — no value leaks by default, every withheld value
appears under `--debug`.

The brief's Phase 1 gate asks for at least a valid customer config and three
specific invalid ones. All four are present and named in the script output:
`invalid-duplicate-field → duplicate_field`, `invalid-relationship →
unknown_relationship_target`, `invalid-operator-type →
operator_type_mismatch`.

## The redaction contract

Added on 2026-09-21 at the user's direction, after the Phase 1 build. It is
recorded in full in [decision 006](decisions/006-redaction-contract.md); the
short version is that config **values** never reach default output, config
**structure** always may, and values appear only under `--debug`.

Phase 1 as first built satisfied this by writing vague messages — *"a value alias
points at a value not declared in enumValues"*, naming no alias. Redacted, but
close to useless in a field with thirty aliases, and a message people cannot act
on is one they route around.

What changed:

- `qferr.Sensitive` — a string type whose `String()` renders `<redacted>`. fmt
  consults `Stringer` for `%v`, `%s` and `%q`, so all three redact. The shape
  that matters is that **the safe rendering is the one you get by accident**;
  revealing requires `FormatRevealed` or `Reveal()`, which read as deliberate.
- `Violation` stores its message unrendered and renders twice: `Detail()`
  redacted, `DetailRevealed()` under debug. Messages can now be specific —
  *"value alias `<redacted>` points at `<redacted>`, which is not declared in
  enumValues"* by default, with both values shown under `--debug`.
- The default output says when something was withheld and how to see it. When
  nothing was withheld, that notice and the whole debug section are suppressed —
  an empty debug block that duplicates the default teaches people to skim past
  it.
- `--json --debug` gains `detail` and `cause` fields; plain `--json` has neither,
  and its `message` stays redacted even in debug JSON.

Why it was settled in Phase 1 rather than later: **Phase 2 builds the semantic
corpus out of config values.** Enum values and aliases become embedded text —
that is the point of the corpus. Once a subsystem holds values as its normal
working material, putting one in an error message looks harmless. The contract
had to exist before that subsystem did.

The guard that makes it stick is `TestNoFixtureLeaksValuesByDefault`: it reads
the values out of each fixture's own JSON and asserts none appear in the default
rendering, whatever the validator happened to say. A rule added in Phase 5 that
interpolates a value fails it without anyone writing a test for that rule.

## Judgment calls worth challenging at the gate

These are choices, not derivations. Each is reversible, and each is somewhere a
reasonable person could disagree.

1. **Ordering comparisons are refused on strings**, and `MIN`/`MAX` with them.
   `WHERE name > 'smith'` is valid SQL and lexicographic nonsense as a question.
   The deeper reason is that allowing it gives the Phase 4 ranker a place to bind
   a comparison phrase wrongly — "customers over 50000" could attach `over` to a
   string field and produce a query that runs and answers nothing.
2. **`LIKE` is refused on enums.** An enum's values are a closed declared set;
   substring-searching them defeats the point of declaring them.
3. **`many-to-many` is refused rather than approximated.** It needs a junction
   table the model does not express, and treating it as `one-to-many` would
   duplicate parent rows — a wrong answer with no error.
4. **No `between` operator.** Date ranges are half-open pairs (`>= start AND <
   end`), matching how the production library models periods. `BETWEEN` is
   inclusive at both ends, so "orders in March" either includes midnight on 1
   April or drops the last instant of 31 March depending on column precision.
5. **Value aliases only on string and enum fields.** On a number or date the
   useful mapping is a parse ("fifty thousand" → 50000), which is Phase 3's job,
   not a per-field lookup table.
6. **All violations reported at once, not fail-fast.** Config authoring is
   iterative; one mistake per run means one edit-run cycle per mistake. The cost
   is that every check must tolerate partially-invalid input.
7. **Unknown config keys are tolerated.** Fixtures are shared across phases, and
   a strict decoder would make a Phase 5 fixture unreadable by a Phase 2 binary.
8. **Identifiers are structure, not values**, so a table or column name is shown
   in default output. Without it an `invalid_identifier` message cannot be acted
   on. An environment that needs identifiers redacted too would need
   `Violation.Path` redacted as well — a stricter configured mode, not an edit
   to this one.

## Failures and surprises

**A blank identifier slipped through validation.** `TestInjectionShapedIdentifierIsRejected`
included an empty table name and failed: `checkIdentifier` skipped blanks on the
reasoning that a blank name had already been reported elsewhere. That is true
only along the path where `Normalize` defaults `table` from the entity name — but
`Validate` is exported and can be called on a hand-built config, and an
explicitly empty string overrides the default. The result would have been an
empty identifier spliced into `FROM`. Blank is now a violation. The test found
this, not the design.

**The error type would not compile as first written.** `ValidationError` embedded
`*qferr.Error`, which promotes a *field* named `Error` and shadows the `Error()`
method the `error` interface needs. It now holds the coded error unexported and
exposes it through `Unwrap`, so `qferr.CodeOf` and `errors.As` still find
`CONFIG_ERROR` through it exactly as through any other pipeline failure.

**The gate script's database check failed on its own guard**, again — the new
Phase 1 sections shifted the numbering but the cause was the earlier one already
recorded in Phase 0.

**The redaction sweep failed on an English article.** `invalid-enum.json` used
`["a","b"]` as placeholder enum values, and the whole-word check matched the "a"
in *"withheld a config value"*. The fixture was wrong, not the check: skipping
short values would have blinded the sweep to exactly the short codes (`IN`,
`US`) that real configs are full of. The fixture now uses distinctive values, and
the limitation is documented where the check lives so nobody loosens it later.

**`--debug` printed the same message twice.** With nothing withheld, the revealed
rendering is byte-identical to the default one, and the CLI printed both. Caught
by `TestNoDebugSectionWhenNothingWithheld`, which was written expecting it to
pass.

## Deliberate non-results

- **`--query` still compiles nothing.** No pipeline exists; it refuses with
  `NOT_IMPLEMENTED`.
- **`--explain` and `--debug` are still silent.** Phase 2.
- **`PatternIntent` is provisional.** The brief never specifies `QueryPattern`,
  and the five intents only acquire meaning in Phase 6's planner. Expect this to
  change; that is a revision, not a failure.
- **Confidence thresholds are not here.** A minimum score and top1/top2 margin
  are ranking calibration for Phase 4, not security limits.
- **Nothing reads the config semantically yet.** The registry indexes it; no
  corpus, no embeddings, no retrieval. That is Phase 2.

## Gate

Phase 2 does not begin until the user runs the script above and confirms.
