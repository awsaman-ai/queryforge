# 002 — Module path and Go version for the experiment

**Status:** accepted · **Phase:** 0 · **Date:** 2026-09-20

## Decision

Module path `queryforge.local/v2experiment`, with a `go 1.25` directive.

## Context

The obvious name for a module living in `queryforge/v2/` is
`github.com/awsaman-ai/queryforge/v2`. That name is a trap.

Go treats a final path element matching `v[0-9]+` as a **major version suffix**.
`github.com/awsaman-ai/queryforge/v2` does not mean "a subdirectory called v2";
it means "QueryForge version 2.x.x", and the tooling would demand `v2.x.x` tags
and treat it as the library's next major release. The experiment is the opposite
of that: a research track that may be deleted.

## Options considered

| Path | Verdict |
|---|---|
| `github.com/awsaman-ai/queryforge/v2` | **Rejected** — claims to be major version 2 of the production library |
| `github.com/awsaman-ai/queryforge-v2` | **Rejected** — no such repository; a resolvable-looking path invites a `go get` that 404s, and implies a publication intent that does not exist |
| `queryforge.local/v2experiment` | **Chosen** |

## Chosen approach

`queryforge.local/v2experiment`.

- `.local` is not a real TLD, so the path is unresolvable by design: `go get` can
  never fetch it and it cannot be published by accident.
- `v2experiment` does not match `v[0-9]+`, so no major-version semantics.
- The name reads as what it is at every import site.

The `go 1.25` directive matches the parent library's floor rather than the
installed toolchain (1.26.4). It costs nothing — the experiment uses no newer
language features — and keeps v2 buildable on the same Go versions the library's
CI already covers, should it ever be built there.

## Trade-offs

- The experiment can never be `go get`-able. That is the intent, and it would be
  a rename away if Phase 10 ever said go.
- Someone may still expect `queryforge/v2/` to be "QueryForge v2". The README's
  opening line and this record are the mitigation.

## Revisit if

Phase 10 returns a go decision and the work graduates into a supported artifact.
At that point the module gets a real published path, and the major-version
question becomes a real question rather than an accident.
