# 006 — The redaction contract

**Status:** accepted · **Phase:** 1 · **Date:** 2026-09-21

## Decision

Config **values** never appear in default output. Config **structure** always
may. Values are available only through an explicit debug mode, chosen per
invocation.

The rule is carried by a type, `qferr.Sensitive`, not by a convention each call
site has to remember.

## Context

Phase 1 originally satisfied this by writing vague messages — *"a value alias
points at a value not declared in enumValues"*, with no indication of which
alias. That is redacted, but it is also close to useless in a field with thirty
aliases, and a message people cannot act on is a message they route around. The
usual way people route around it is to turn redaction off.

The deeper reason to settle this in Phase 1 rather than later: **Phase 2 builds
the semantic corpus out of config values.** Enum values and value aliases become
embedded text — that is the entire point of the corpus. Once a subsystem holds
values as its normal working material, it is a short step to putting them in an
error, on the reasoning that the corpus already has them. The contract has to
exist before that subsystem does.

## Where the line is drawn

**Redacted by default — the data domain:**
enum values, value-alias keys and targets, literals extracted from a user
sentence, and anything else that describes what is *stored* rather than how the
schema is *shaped*. A config's aliases can map real customer names, account
numbers or internal product codes.

**Retained by default — structure:**
entity, field, table and column names; operator, aggregation, field-type,
cardinality and intent names; rule names; positions in the config
(`fields[2] (customer.country)`).

This split is a judgment call and worth challenging. The argument for it: a
message that will not say which field is broken cannot be acted on at all, and
schema shape is already visible to anyone who can read the config file, whereas
values may describe third parties who cannot. The identifier in an
`invalid_identifier` violation is deliberately **not** redacted for the same
reason — it is the one thing the author needs in order to find the line.

If an environment needs identifiers treated as sensitive too, that is a
different and stricter contract; it would need the `Path` field redacted as well,
and it should be a configured mode rather than a quiet change here.

## Mechanism, and why it is a type

`qferr.Sensitive` is a string type with a `String()` method that renders
`<redacted>`. fmt consults `Stringer` for `%v`, `%s` and `%q`, so all three
redact — including `%q`, which a call site is likely to reach for precisely when
quoting a value.

The shape that matters: **the safe rendering is the one you get by accident.**
Revealing requires calling `FormatRevealed` or `Reveal()`, both of which read as
deliberate at the call site. Had it been the other way round — a `Redact()` you
must remember to call — the failure mode of forgetting would be a leak.

`Violation` therefore stores its message unrendered, as a format string plus
arguments, and renders twice: `Detail()` redacted, `DetailRevealed()` for debug.

## What stops an unwrapped value

The type only protects values somebody wrapped. `TestNoFixtureLeaksValuesByDefault`
closes that gap generically: it reads the values out of each fixture's own JSON
and asserts none appear in the default rendering, whatever the validator
happened to say. A rule added in Phase 5 that interpolates a value into a message
fails that test without anyone having written a test for that rule.

Its one limitation is deliberate. A config value that is an ordinary English word
will false-positive, because nothing distinguishes it from prose. The answer is
for fixtures to use distinctive values, **not** for the check to skip short
strings — skipping would blind it to exactly the short codes (`IN`, `US`) that
configs are full of. `invalid-enum.json` originally used `["a","b"]` and tripped
on the article in "withheld a config value"; the fixture changed, not the check.

## The redaction has to announce itself

A redacted message that gives no hint is indistinguishable from a vague one. When
anything is withheld, the default output ends with:

```text
(3 problem(s) withheld a config value; re-run with --debug to see them)
```

And when nothing was withheld, that notice — and the whole debug reveal section —
is suppressed. An empty debug section that duplicates the default output teaches
people to skim past it, which costs more than it gives.

## Binding on later phases

1. Any user-facing string built from config or user input goes through
   `qferr.Format` with values wrapped, or is structural only.
2. `FormatRevealed`, `Reveal()` and `JSONRevealed` are debug-only. Their output
   must not reach default stdout/stderr, a metric label, or a log line that is on
   by default.
3. Phase 2's corpus builder holds values as working material. That does not make
   them safe to put in an error.
4. Phase 8's observability fields (`selected_entity`, `selected_fields`) are
   structural and may be logged. Extracted literals and resolved values are not.

## Revisit if

- An environment needs identifiers redacted too — that is a stricter mode, not an
  edit to this one.
- Phase 9 shows the double rendering costs measurable time on the hot path. It
  should not: violations render lazily and only on a failure path.

## Later decisions that build on this

- [012](012-contextual-ambiguity.md) is not about redaction, but its regression
  cases run through the same rendering.
- [013](013-rank-output-is-diagnostic.md) draws the line for **user text** by
  audience. `--rank` diagnostics may echo it, while error and log paths locate
  it by position. Config values stay redacted on every surface, as here.
- Phase 4 found that value-entry **IDs** embed the value. They are rendered
  through `corpus.DisplayID` everywhere they are printed.
