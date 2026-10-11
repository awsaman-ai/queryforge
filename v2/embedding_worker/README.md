# Embedding worker

A long-lived local process that maps text to vectors. It is the only part of the
experiment that runs a model, and it is deliberately the only part that could.

## What it is not

It is **not** a generative model, and it never calls a remote service. It maps
text to a fixed-size vector, on this machine. Rule 11 of the experiment brief
allows a local embedding model precisely because the research question is about
semantic vector retrieval; rules 9 and 10 forbid anything generative or remote.

It runs OFFLINE (Phase 8, section 16.11). The only network access in its life
is the one-time download of the model weights, which needs an explicit opt-in
(below); after that, every start loads from the local cache and resolves the
weights' revision from it. `netcheck.py` runs the worker under a Python audit
hook and counts socket connections and DNS lookups; the Go test
`TestWorkerMakesNoNetworkConnections` requires zero.

Before Phase 8 this README said the same, and it was not true: measured with
that audit hook, every start contacted huggingface.co (a revision check at
load, and `model_info()` for the revision), cached weights or not. No query
text was sent. Decision 021 records the fix.

## Setup

```bash
cd v2
uv venv --python 3.13 embedding_worker/.venv
VIRTUAL_ENV=embedding_worker/.venv uv pip install -r embedding_worker/requirements.txt
```

Download the weights once — the one step that uses the network:

```bash
printf '%s\n' '{"op":"identity"}' '{"op":"shutdown"}' \
  | QF_V2_ALLOW_MODEL_DOWNLOAD=1 embedding_worker/.venv/bin/python embedding_worker/worker.py
```

Without it, a worker whose weights are not cached fails at start with a
message naming this variable, rather than reaching out on its own.

The Go side finds `.venv/bin/python` on its own. It prefers that interpreter
over the system `python3` deliberately: a system Python may be a different
version with a different — or missing — sentence-transformers, and silently
falling back to it would produce vectors from a stack nobody recorded.

Without the venv, `go test ./...` still passes. The worker tests skip, and
everything else uses the deterministic hashing provider.

## Model

Pinned: `sentence-transformers/all-MiniLM-L6-v2`, 384 dimensions, ~80 MB.
Measured revision `1110a243fdf4706b3f48f1d95db1a4f5529b4d41`.

Weights are **not** committed. Section 10 of the brief is explicit about this:
they are large, they are reproducible from the pinned model id, and a repository
is the wrong place to distribute them. The revision hash is what makes a result
traceable to exact weights rather than to a name upstream may have re-uploaded.

Override with `QF_V2_EMBEDDING_MODEL`, or `--model` on the CLI. Cache location:
`QF_V2_MODEL_CACHE`.

## Protocol

Newline-delimited JSON on stdin and stdout. One object per line, one response
per request, in order.

```text
-> {"op":"identity"}
<- {"ok":true,"model":"...","revision":"...","dim":384,"normalized":true,"semantic":true}

-> {"op":"embed","texts":["customer spending","money spent"]}
<- {"ok":true,"vectors":[[...384 floats...],[...]]}

-> {"op":"shutdown"}
<- {"ok":true}

   on any failure
<- {"ok":false,"error":"..."}
```

**stdout carries protocol only.** Diagnostics go to stderr. A stray `print()`
would land in the middle of a JSON response and produce a parse error in Go that
looks nothing like its cause — which is why the ML stack's logging and progress
bars are silenced before those libraries are imported.

Vectors are L2-normalised in the worker, so cosine similarity is a plain dot
product downstream and the invariant lives in one place.

`semantic: true` is how the Go side tells a real model from the deterministic
hashing stand-in used in tests. Research results may only be recorded from a
provider that reports true.

## One process, many requests

The model is loaded **once**, at startup, and reused for every request until the
process exits.

This is not an optimisation, it is the architecture under test. Section 17 of
the brief names the alternative — spawn an interpreter per request, load the
model, embed, exit — as a prohibited pattern, because it would add seconds of
cost unrelated to anything being measured and would invalidate every latency
number the experiment produces.

`TestWorkerReusesOneProcess` asserts it by pid: correctness tests cannot catch a
worker that respawns, because a respawning worker still returns correct vectors.

Concurrency is serialised by a mutex on the Go side. The protocol is a single
ordered stream, so two concurrent callers would interleave their writes and each
read the other's reply. A pool of workers is how to add parallelism, and whether
it is needed is a Phase 9 question to answer with measurements.

## Running it by hand

```bash
echo '{"op":"identity"}' | ./embedding_worker/.venv/bin/python embedding_worker/worker.py
```
