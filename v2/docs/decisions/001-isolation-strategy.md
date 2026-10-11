# 001 — How v2 stays isolated from production QueryForge

**Status:** accepted · **Phase:** 0 · **Date:** 2026-09-20

## Decision

Put the experiment in `queryforge/v2/` as a **nested Go module** with its own
`go.mod` and its own `go.work`, and enforce the boundary with a test.

## Context

The brief's single most important constraint is that the existing QueryForge
architecture stays untouched (rules 1–6, and section 31's closing line). Three
concrete ways that could break:

1. v2 code compiling into the published library.
2. Adding v2 to the workspace at `/Users/aman/AI/nlp_to_query/go.work`, which
   rule 5 forbids outright.
3. A v2 file importing a production package for convenience, which quietly makes
   the library a dependency of the experiment and invites the reverse later.

There is a fourth, less obvious one: the root workspace makes `cd v2 && go test
./...` — the exact command the brief's manual gate specifies — fail. Go searches
upward for `go.work` from the working directory, finds the root one, sees that
`v2` is not in its `use` list, and reports *"directory v2 is contained in a
module that is not one of the workspace modules"*. So doing nothing is not an
option; the only question is which workaround.

## Options considered

| Option | Verdict |
|---|---|
| Add `./queryforge/v2` to the root `go.work` | **Rejected** — rule 5 forbids changing the root workspace, and it would put the experiment in every developer's tooling context |
| Require `GOWORK=off` on every v2 command | **Rejected** — invisible, easy to forget, and it breaks the brief's literal gate command. An experiment whose test command differs from its documented test command will drift |
| A separate sibling repository | **Rejected** — the brief says `queryforge/v2/`, and a separate repo makes "compare against v1" harder than it needs to be |
| A nested module plus a local `go.work` | **Chosen** |
| Same module, a `v2experiment` build tag | **Rejected** — leaves experimental code inside the published library, gated only by a tag someone can flip |

## Chosen approach

1. **`v2/go.mod`** makes v2 a separate module. Since Go 1.18, a directory with
   its own `go.mod` is pruned from the parent module's `./...` patterns, so the
   library's `go test ./...` never sees v2. Verified rather than assumed: see
   the Phase 0 results.
2. **`v2/go.work` containing `use .`** shadows the root workspace, because Go
   stops at the first `go.work` it finds walking upward. The root file is not
   edited. `cd v2 && go test ./...` works exactly as the brief specifies.
3. **`internal/isolation/isolation_test.go`** parses every `.go` file in the
   module and fails on an import of `github.com/awsaman-ai/queryforge`, of any
   database driver, or of any remote-model SDK.

## Trade-offs

- The guard test is a denylist, so an unlisted driver would slip through. An
  allowlist of the standard library was rejected because Phase 2 may legitimately
  need a third-party dependency; the denylist names exactly what the brief bans.
- `net/http` is deliberately **not** banned. Phase 2's local embedding worker may
  reasonably speak HTTP over loopback. The rule is "no remote model", not "no
  sockets" — a check that conflated the two would push the worker toward a worse
  transport just to satisfy a test.
- Two `go.work` files in one tree is unusual and will surprise someone. That is
  what this record is for, and the file itself carries the same explanation.
- Duplication is accepted. Where v2 needs something the library already does, it
  gets its own copy (brief, section 6): a shared package would mean editing
  production code to serve an experiment.

## Revisit if

- The experiment reaches a go decision in Phase 10 and graduates from a research
  track, at which point isolation stops being the point.
- A v2 dependency legitimately needs a database driver for **offline** evaluation
  — the guard would need a narrow, documented exception, and section 16.9 would
  need re-reading first.
