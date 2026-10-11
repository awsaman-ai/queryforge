# 003 — The CLI contract, and why `--query` fails on purpose

**Status:** accepted · **Phase:** 0 · **Date:** 2026-09-20

## Decision

Freeze the CLI surface now, wire `--query` to a `NOT_IMPLEMENTED` refusal, and
define the closed error-code taxonomy before any semantic code exists.

## Context

Phase 0 is titled "scope, baseline, and **contracts**". The contracts that matter
this early are the ones later phases would otherwise each invent differently:
how a request is made, what comes back, and how failure is named.

The temptation in a Phase 0 skeleton is to have `--query` print a hand-written
`SELECT` so the thing "works". That is precisely the failure mode section 29 of
the brief lists — *"unsupported query features are silently approximated"* — and
it would make the Phase 7 gate meaningless, because the command would have looked
like it worked since day one.

## Chosen approach

**Surface.** `--config`, `--query`, `--backend`, `--explain`, `--debug`,
`--json`, `--validate-config`, `--version`.

`--validate-config` is not in the brief's flag list but the Phase 0 manual gate
requires proving that "a simple config loads" and "an invalid config is
rejected". With no pipeline, `--query` cannot demonstrate either. The flag also
keeps earning its place later as the fast config check.

`--explain` and `--debug` are accepted but silent until Phase 2. Accepting them
now fixes the contract; making them do something now would mean inventing
diagnostics for reasoning that does not exist.

**Backends.** Only `postgres` is accepted. `mysql`, `mongodb`, `elasticsearch`
and `opensearch` are rejected with `INVALID_INPUT` rather than ignored, so the
frozen scope is enforced by the binary and not only by the brief.

**Failure.** `internal/qferr` holds the closed code set from section 23, plus one
addition: `NOT_IMPLEMENTED`, for a pipeline stage that does not exist yet. The
brief describes the finished system and so has no code for this; the experiment
is built in phases and needs a way to say "not built" that cannot be confused
with "your query was bad". Every use of it should be gone by Phase 7.

**Exit codes.** `0` success, `1` the engine refused with a coded reason, `2` the
command line was wrong. The 1/2 split matters once the Phase 10 harness runs
thousands of invocations: a typo in the driver script must not be recorded as a
semantic failure.

**Redaction.** A coded error carries a redaction-safe `Message` plus a wrapped
cause. Only the message is printed by default; the cause needs `--debug`. A JSON
syntax error quotes file content, and config files carry table names and value
aliases (section 16.10).

## Trade-offs

- Flags that do nothing yet can read as unfinished. The README says which ones
  those are and when they light up.
- A closed error taxonomy declared before the code exists will need additions.
  Better that than each phase inventing its own vocabulary and Phase 10 having to
  reconcile them retroactively — the failure distribution is a headline result.

## Revisit if

- Phase 7 lands: `NOT_IMPLEMENTED` should disappear from the query path, and
  `TestQueryIsNotImplemented` must be **replaced** by real end-to-end assertions.
- Phase 9 needs a batch or server mode for warm-latency measurement. A
  process-per-query CLI cannot measure warm latency, and inventing that transport
  now would be premature.
