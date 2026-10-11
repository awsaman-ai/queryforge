# 013 — `--rank` output is a diagnostic surface, not telemetry

**Status:** accepted (user decision at the Phase 4 gate) · **Phase:** 4 · **Date:** 2026-09-21

## Decision

`--rank` and `--rank-probe` output may contain the **user's own text**: the
sentence, its spans, and unexplained words. They are developer and research
diagnostics, like `--analyze`. That output **must not** automatically be treated
as production telemetry or logs. Nothing may forward it to a log pipeline, a
metrics label or an event stream by default.

Normal error and logging paths do **not** carry user text by default. The
sentence-level refusal (`ranking.VerdictError`) locates a phrase by token
position (`the phrase at tokens[0:1] ...`) and quotes it only under `--debug`.

The redaction contract for **config values** (decision 006) is unchanged
everywhere, `--rank` included. Value-entry IDs render as
`value:<entity>.<field>:<kind>:<redacted>` and matched value surfaces as
`<redacted>`, unless `--debug` is set.

## The three surfaces

| Surface | User text | Config values | Audience |
|---|---|---|---|
| `--rank` / `--rank-probe` report (stdout) | shown | redacted unless `--debug` | a developer at a terminal |
| error message (stderr, `qferr.Error.Message`, `--json` error) | withheld, located by position | redacted | anything, including a log aggregator |
| `--debug` renderings | shown | revealed | a developer who asked for them |

## Why

The report is useless without the words. A ranking decision cannot be argued
about unless you can see which phrase it was about. But an error message is the
output most likely to be shipped off the machine, and decision 006 puts literals
from a user sentence in the data domain. The line is drawn by **audience**, not
by content.

## Obligations for later phases

- Phase 7's pipeline and Phase 9's telemetry must build their events from the
  structured, redacted forms (codes, targets through `corpus.DisplayID`, token
  positions), **never** by capturing `--rank` stdout.
- Any new error type with a phrase in it follows `VerdictError`: redacted
  `Error()`, plus `ErrorRevealed()` / `RedactedValueCount()` for `--debug`.
  `report()` finds it through the `revealer` interface.
- If an environment needs user text withheld from the diagnostic report as
  well, that is a configured mode, not a quiet change here. It is the same
  reasoning decision 006 applies to a stricter identifier mode.
