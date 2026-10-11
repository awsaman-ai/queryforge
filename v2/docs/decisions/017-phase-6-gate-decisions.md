# 017 — Phase 6 gate decisions: units, same-record ambiguity, NULL semantics, test honesty

**Status:** accepted (user decision at the Phase 6 gate) · **Phase:** 6 → 7+ · **Date:** 2026-09-21

These are the four judgment calls Phase 6 left open. The user decided all four
when approving the Phase 6 gate.

## 1. Currency and units are refused until the config defines them

A value carrying a currency (`$`, `₹`, `€`, `USD`), a percent sign, or a unit
word (`kg`, `dollars`), when compared with a field, is **VALUE_RESOLUTION_FAILED**
unless the config explicitly defines what that unit means for the field.

- Nothing is interpreted from common knowledge. `$` is not assumed to match a
  money field, `%` is not assumed to mean ×0.01, and `kg` is not assumed to
  match a weight.
- **Later (not scheduled to a phase yet):** explicit per-field metadata, for
  example a numeric field declaring `"currency": "USD"` or `"unit": "kg"`. A
  value whose unit matches can then compile; one that differs stays refused
  (there is no conversion). Until that metadata exists, today's refusal stands.
- The one existing exception is unchanged: a unit after a **count** ("ordered 3
  times") is kept as a visible note (decision 014). It cannot change how many
  rows exist.
- Regression tests: `TestPlanRefusesWhatTheNumberSaidAboutItself` covers `$`,
  `₹`, `€`, `USD`, `%`, `dollars` and `kg`.

## 2. Several conditions through one to-many relationship stay ambiguous

"customers who bought laptops for more than 1000" means either that **one**
order is a laptop over 1000, or that the customer has **some** laptop order and
**some** order over 1000. The two readings return different rows.
AND-joined conditions through the same to-many step remain **AMBIGUOUS_MATCH**.

- **No global rule** is added that the same relationship means the same record
  (or different records). Such a rule would be a guess applied everywhere.
- OR-joined conditions still compile, because both readings coincide for OR.
- Regression tests: the `refuse-same-row-ambiguity` plan fixture, and the
  `same-row ambiguity` mutation in the Phase 6 mutation set.

## 3. Negation keeps standard SQL NULL semantics: an intentional rule

`neq` compiles to `col <> $n`, and `nin` to `col NOT IN (...)`. Rows whose value
is NULL (unknown) are **excluded**, as SQL's three-valued logic does.

**This is intentional, not a bug.** "Customers not from India" does not
include customers whose country is unknown. The query does not say they are
not from India; it does not know. Including them would require an explicit
include-unknown or NULL policy, expressed in the query language or the config.
Neither exists today.

- Do **not** rewrite negation to `col <> $n OR col IS NULL`, `IS DISTINCT FROM`,
  or `COALESCE`. `TestNegationKeepsStandardNullSemantics` fails if anyone does,
  and its message points here.
- The same rule covers `NOT (range)` on dates: an unknown date is not "outside
  last month".

## 4. A test must prove its failure path, not pass on a nil error

`qferr.CodeOf(nil)` returns `INTERNAL_ERROR`, because unclassified maps to
internal. A test that asserts only `CodeOf(err) == CodeInternal` therefore
**passes when there was no error**. Every such assertion now checks
`err == nil ||` first. That covers Phase 6's two tests and Phase 4's
`TestMismatchedRetrievalIsAnInternalError`, per the user's instruction.

Note on the Phase 4 test: with ranking's length check removed, `Rank` still
panics further on, so the test still fails. It fails by crash, not by
assertion. The guard now ensures that a silent nil return can never pass. This
is a test-only change; `internal/ranking` code is untouched.

Standing rule for later phases: any assertion on `CodeInternal` must also
assert that an error occurred.
