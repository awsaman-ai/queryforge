# 005 — Validation strategy: named rules, all violations at once

**Status:** accepted · **Phase:** 1 · **Date:** 2026-09-20

## Decision

Validation collects **every** violation rather than stopping at the first,
labels each with a name from a closed `Rule` set, and returns them sorted.

## Why named rules

The Phase 1 exit criterion is that every invalid fixture is rejected "for the
expected reason". That is only checkable if the reason has a name. Substring
matching on prose would pass for the wrong reason the moment a message is
reworded, and would let a fixture that trips an unrelated rule look like a pass.

So tests assert on `Rule` values, and `TestGoldenFixtures` asserts the **exact**
set of rules each fixture breaks, not merely that it contains the expected one. A
validator that grows a spurious extra violation is as broken as one that misses a
real one, and only an exact comparison catches the first case.

`TestEveryRuleHasAFixture` closes the other gap: a rule added later cannot sit
untested while the suite stays green.

## Why collect rather than fail fast

Config authoring is iterative. A validator that reveals one mistake per run turns
a config with ten problems into ten edit-run cycles. The cost is that the
validator must keep going after a failure, which means every check has to
tolerate partially-invalid input — a field on an unknown entity still gets its
type and operators checked, because that field usually has more than one problem
and reporting them together saves a round trip.

## Why sorted

Several checks walk Go maps — value aliases, operator phrases, aggregation
phrases — and Go randomises map iteration. Without an explicit sort, the same
invalid config would produce differently-ordered output on each run.

Section 25 requires the same inputs to produce the same result, and Phase 10 has
to defend a determinism claim. A validator whose error text shuffles between runs
would undercut that before the semantic layers even get a chance to.
`TestRejectionIsDeterministic` loads a multi-violation fixture 25 times and
compares the rendered message byte for byte.

## What messages may say

Violations name entities, fields, tables and operators. They do **not** quote
values — enum values, value-alias targets, or the content of the JSON.

The line is drawn there because section 16.10 treats values as potentially
sensitive while identifiers are structural, and because a message that will not
say which field is broken is useless to the person fixing it. The underlying JSON
parse error, which can quote arbitrary file content, stays wrapped and appears
only under `--debug`. `TestConfigErrorDoesNotEchoValues` pins both halves.

## Identifier policy

`^[A-Za-z_][A-Za-z0-9_]*$`, at most 63 characters.

Deliberately narrower than what PostgreSQL accepts when quoted. Section 16.2
requires that every emitted identifier originate in validated config, and the
cheapest way to keep that true is to allow only shapes that need no quoting and
cannot contain a quote, a semicolon or whitespace in the first place. User text
can never become an identifier — but a *config author* can write one, and the
compiler trusts config identifiers by construction.

The 63-character limit is not cosmetic: PostgreSQL silently truncates longer
identifiers, so two distinct configured columns can collide into one and return
wrong rows with no error anywhere.

A blank identifier is a violation, not a skip. `Normalize` defaults `table` and
`column` from the entity or field name, so blank at validation time means either
that `Normalize` was never called or that the default was overridden with an
empty string. Letting it through would hand the compiler an empty identifier to
splice into `FROM`. This was found by a test, not by design — the first version
skipped blanks on the assumption that a blank name had already been reported,
which is true only along the defaulted path.

## Zero means unset

A security limit left at zero gets the default rather than being treated as "no
budget". A config can then override one limit without restating the rest, and a
config with no `security` block at all is still bounded. Negative values are
rejected, so the escape hatch is explicit rather than a typo away.

## Revisit if

- Phase 10's failure taxonomy needs a finer split than the current rule set.
- A config legitimately needs an identifier this pattern refuses, such as a
  non-ASCII column name. That would need a quoting strategy in the compiler
  first, not a looser regex.
