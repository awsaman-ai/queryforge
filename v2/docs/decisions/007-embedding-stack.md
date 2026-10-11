# 007 — The embedding stack: local model, sidecar, transport

**Status:** accepted · **Phase:** 2 · **Date:** 2026-09-21

## Decision

`sentence-transformers/all-MiniLM-L6-v2` (384 dimensions), run by a Python
sidecar process that speaks newline-delimited JSON over stdin and stdout, loaded
once per process and reused.

## Transport: stdio, not HTTP, not pure Go

| Option | Verdict |
|---|---|
| Python sidecar over stdio | **Chosen** |
| Python sidecar over localhost HTTP | Rejected |
| Pure Go: ONNX Runtime via cgo, Go tokenizer | Rejected for Phase 2 |

Stdio makes "no remote inference" **structurally** true rather than merely
intended: there is no port to misconfigure and nothing for another process to
connect to. HTTP would be easier to poke at with curl, at the cost of port
allocation, readiness handling and a listening socket that the rule exists to
avoid — a weaker story even though the traffic never leaves the machine.

There is also house precedent: QueryForge's Python and Java SDKs already speak a
JSON stdio protocol to the Go engine, so this is a shape to copy rather than
invent.

Pure Go would remove the Python dependency and the process boundary entirely,
but it means reimplementing WordPiece tokenisation and shipping a native
library — a phase of work on its own, before any semantics get measured. It
stays open for Phase 9 if the process boundary turns out to cost real latency.

## Model: MiniLM as the pinned baseline

384 dimensions, ~80 MB, the standard lightweight Sentence Transformers model and
the most likely reading of the brief's "lightweight ST-compatible model". Fast
on CPU, needs no query prefix, and widely enough used that published numbers
exist to sanity-check ours against.

`bge-small-en-v1.5` scores better on retrieval benchmarks but requires an
asymmetric query prefix, which would mean the `EmbeddingProvider` interface has
to model the difference between embedding a query and embedding a corpus entry.
That is a real design cost to take on before there is any evidence it is needed;
Phase 9 or 10 can benchmark it behind the interface that now exists.

The revision hash — `1110a243fdf4706b3f48f1d95db1a4f5529b4d41` — is recorded with
every index and every report. A model name alone is not reproducible: upstream
can re-upload weights under the same name.

## Two providers, and the guard between them

`Worker` runs the real model. `Deterministic` is a character-trigram hashing
embedder: no model, no Python, no network, same text always the same vector.

The hashing provider exists for two reasons. Tests of corpus generation, index
mechanics and persistence need vectors but not *meaning*, and making them depend
on an 80 MB download would make them slow, offline-fragile and unrunnable in a
fresh checkout — and a test that is hard to run stops being run. Second, it is
an honest **lexical baseline**: trigram hashing captures surface overlap and
nothing else, which is ablation A from section 18, the thing embeddings have to
beat before anyone can claim vectors earn their place.

The risk is that a number from the stand-in gets reported as a finding.
`Identity.Semantic` is the guard: the hashing provider reports false, the CLI
prints a loud warning when it is selected, and a `--probe` report produced with
it carries a `NON-SEMANTIC PROVIDER` banner in the report body — not just on
stderr, because the report body is what gets copied into a document.

## Trade-offs

- A Python dependency for anyone who wants real retrieval. Mitigated by tests
  skipping rather than failing when the venv is absent.
- One worker serialises requests. Concurrency is a Phase 9 question; adding a
  pool now would be optimising before measuring (section 22, rule 7).
- Weights are not vendored, so first run needs network. That is the brief's
  explicit instruction, and the revision pin is what preserves reproducibility.

## Revisit if

- Phase 9 shows the process boundary is a material share of warm latency —
  then pure-Go ONNX becomes worth its cost.
- Phase 10's ablation shows MiniLM is the limiting factor rather than the
  ranking, in which case a stronger model is benchmarked behind the same
  interface.
