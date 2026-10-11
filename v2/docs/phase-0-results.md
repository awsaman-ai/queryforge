# Phase 0 results — scope, baseline, contracts

**Date:** 2026-09-20 · **Status:** built and self-verified; **awaiting the
user's manual gate**.

## Environment

Recorded per section 25 of the brief so later results stay comparable.

| | |
|---|---|
| OS | macOS 26.5.2 (Darwin 25.5.0), arm64 |
| CPU | Apple M5, 10 cores |
| RAM | 16 GB |
| Go | 1.26.4 (module targets `go 1.25`) |
| Python | 3.14.6 (not used in Phase 0) |
| Library commit at baseline | `51df283` |
| v2 module | `queryforge.local/v2experiment` |
| Third-party dependencies | none |

No embedding model, vector index, or timing measurement exists yet. Phase 0
produces no performance or accuracy numbers, and none should be quoted from it.

## Baseline — production QueryForge before any v2 work

Run from the library root **before** `v2/` was created, with `-count=1` to defeat
the test cache:

```text
ok  github.com/awsaman-ai/queryforge                     3.006s
ok  github.com/awsaman-ai/queryforge/cmd/queryforge      2.077s
ok  github.com/awsaman-ai/queryforge/internal/ast        0.555s
ok  github.com/awsaman-ai/queryforge/internal/config     3.407s
ok  github.com/awsaman-ai/queryforge/internal/datetime   1.066s
ok  github.com/awsaman-ai/queryforge/internal/explain    2.446s
ok  github.com/awsaman-ai/queryforge/internal/gen        4.340s
ok  github.com/awsaman-ai/queryforge/internal/planner    1.493s
ok  github.com/awsaman-ai/queryforge/internal/provider   6.512s
ok  github.com/awsaman-ai/queryforge/internal/validate   3.864s
```

All packages pass; the working tree was clean. The suite passes identically after
the v2 work, and the library's `go list ./...` does not include any v2 package —
the nested `go.mod` prunes the directory from the parent module, so the isolation
claim is verified rather than assumed.

**Nothing outside `v2/` was modified.** The root `go.mod`, root `go.work`, the Go
engine, the Python and Java SDKs, and the config format are untouched.

## What Phase 0 built

| Path | Purpose |
|---|---|
| `go.mod`, `go.work` | isolated module; local workspace shadowing the root one |
| `cmd/qf-v2/main.go` | the frozen CLI contract |
| `internal/qferr/` | the closed error-code taxonomy |
| `internal/config/` | thin structural config reader (Phase 1 replaces it with the typed model) |
| `internal/isolation/` | build-time guard for rules 6, 7 and 10 |
| `testdata/configs/` | one valid and four invalid config fixtures |
| `docs/decisions/001–003` | isolation strategy, module identity, CLI contract |
| `scripts/run-manual-tests.sh` | this gate, automated |

## Automated tests

`cd v2 && go test ./...` — 4 packages, all passing, no third-party dependencies.

The two worth calling out:

- **`internal/isolation`** parses every `.go` file in the module and fails on an
  import of the production library, of a database driver, or of a remote-model
  SDK. It was verified by deliberately adding `import _ "database/sql"` and
  confirming it fails; the import was then removed.
- **`cmd/qf-v2/TestQueryIsNotImplemented`** asserts that `--query` refuses and
  that nothing resembling SQL reaches stdout. This is the test that keeps the
  experiment honest until Phase 7, and it must be *replaced* there, not deleted.

## Manual gate

```bash
cd v2 && ./scripts/run-manual-tests.sh
```

Latest run: **15 checks, 15 passed.** It covers every exit criterion in section 8
of the brief:

| Criterion | Check |
|---|---|
| existing root tests unchanged and still pass | library `go test ./...`; library package list excludes v2 |
| v2 starts independently | v2 builds, tests, `gofmt`, `go vet`, `--version` |
| a simple config loads | `--validate-config` on `customers.json` |
| invalid config is rejected | four fixtures, each expecting `CONFIG_ERROR` |
| the CLI does not connect to a database | source scan for drivers, `sql.Open`, connection strings |

Two extra checks guard the frozen scope: `--query` must refuse with
`NOT_IMPLEMENTED`, and a non-Postgres backend must be rejected.

## Deliberate non-results

Stated explicitly so they are not mistaken for gaps:

- **`--query` compiles nothing.** There is no pipeline, so it refuses. Emitting a
  hand-written `SELECT` here would have made the Phase 7 gate meaningless.
- **`--explain` and `--debug` are silent.** The flags are part of the frozen
  contract; there is no reasoning to explain until Phase 2.
- **Config validation is minimal.** Four structural rules, not the full Phase 1
  list. The gap is itemised in `TODO.md` under "Carried forward".
- **`relationships` is read but not decoded.** Join reasoning is Phase 5.

## Failures and surprises

Two things did not work first time; both are recorded because the brief says not
to hide them, and both changed a design choice rather than just a line of code.

1. **The root workspace breaks the brief's own gate command.** `go.work` at
   `/Users/aman/AI/nlp_to_query/` lists three modules and, correctly, not this
   one. Go searches upward for `go.work`, so `cd v2 && go test ./...` — the exact
   command section 8 specifies — fails with *"directory v2 is contained in a
   module that is not one of the workspace modules"*. Adding v2 to the root file
   is forbidden by rule 5. Resolved with a local `v2/go.work` that shadows it;
   see decision 001.

2. **The database-access gate check failed on its own guard.** Section 5 of the
   script greps for `"database/sql"` and friends, and matched
   `internal/isolation/isolation_test.go` — whose denylist necessarily spells out
   the strings being searched for. Fixed by excluding that directory, which its
   own test already covers.

One trap was avoided rather than hit: the natural module path
`github.com/awsaman-ai/queryforge/v2` would have been read by Go as **major
version 2 of the production library**, since a final `v[0-9]+` element is a
version suffix. See decision 002.

## Gate

Phase 1 does not begin until the user runs the script above and confirms.
