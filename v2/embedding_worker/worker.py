#!/usr/bin/env python3
"""Local embedding worker for the QueryForge V2 experiment.

Reads newline-delimited JSON requests on stdin, writes newline-delimited JSON
responses on stdout. The model is loaded ONCE, at startup, and reused for every
request afterwards.

That is the whole point of this file. Section 17 of the brief names the
alternative -- spawn a process per request, load the model, embed, exit -- as a
prohibited pattern, because it would make the experiment slow for reasons that
have nothing to do with the architecture being tested and would invalidate every
latency measurement taken.

This is NOT a generative model and never calls a remote service. It maps text to
a fixed-size vector, locally. The only network access is the one-time download of
the model weights on first run, which is cached outside the repository.

Protocol (one JSON object per line, in both directions):

    -> {"op": "identity"}
    <- {"ok": true, "model": "...", "revision": "...", "dim": 384,
        "normalized": true, "semantic": true}

    -> {"op": "embed", "texts": ["customer spending", "money spent"]}
    <- {"ok": true, "vectors": [[...384 floats...], [...]]}

    -> {"op": "shutdown"}
    <- {"ok": true}

    any failure
    <- {"ok": false, "error": "human-readable reason"}

stdout carries protocol only. Anything diagnostic goes to stderr, because a
stray print() would corrupt the stream and produce a parse error in Go that
looks nothing like its cause.
"""

import json
import os
import sys

# Every log line and progress bar from the ML stack must be kept off stdout, or
# it lands in the middle of a JSON response. Set before importing torch and
# sentence_transformers, which read these at import time.
os.environ.setdefault("TRANSFORMERS_VERBOSITY", "error")
os.environ.setdefault("HF_HUB_DISABLE_PROGRESS_BARS", "1")
os.environ.setdefault("TOKENIZERS_PARALLELISM", "false")

# OFFLINE BY DEFAULT (brief section 16.11; Phase 8, decision 021). Measured in
# Phase 8: loading the model through sentence-transformers contacted
# huggingface.co on EVERY start — a revision check at load, and model_info()
# in resolve_revision — although the weights were already cached. No query
# text was sent, but the worker is supposed to run on this machine alone.
# Both switches are forced, not defaulted: a stray HF_HUB_OFFLINE=0 in the
# caller's environment must not quietly reopen the network. The one
# legitimate network use, the first download of the weights, needs an
# explicit opt-in: QF_V2_ALLOW_MODEL_DOWNLOAD=1.
ALLOW_DOWNLOAD = os.environ.get("QF_V2_ALLOW_MODEL_DOWNLOAD") == "1"
if not ALLOW_DOWNLOAD:
    os.environ["HF_HUB_OFFLINE"] = "1"
    os.environ["TRANSFORMERS_OFFLINE"] = "1"

DEFAULT_MODEL = "sentence-transformers/all-MiniLM-L6-v2"

# Hard cap on a single batch. A caller asking to embed an unbounded list would
# otherwise decide this process's peak memory for it.
MAX_BATCH = 2048
MAX_TEXT_LENGTH = 8192


def log(message: str) -> None:
    """Diagnostics go to stderr; stdout is protocol only."""
    print(message, file=sys.stderr, flush=True)


def respond(payload: dict) -> None:
    # separators avoids incidental whitespace, and the explicit flush matters:
    # the Go side blocks on a newline, so a buffered response is a deadlock.
    sys.stdout.write(json.dumps(payload, separators=(",", ":")) + "\n")
    sys.stdout.flush()


def fail(message: str) -> None:
    respond({"ok": False, "error": message})


def main() -> int:
    model_id = os.environ.get("QF_V2_EMBEDDING_MODEL", DEFAULT_MODEL)
    cache_dir = os.environ.get("QF_V2_MODEL_CACHE") or None

    try:
        from sentence_transformers import SentenceTransformer
    except ImportError as exc:
        # Reported over the protocol rather than as a traceback, so the Go side
        # can turn it into a coded error with an actionable message.
        fail(f"sentence-transformers is not installed in this interpreter: {exc}")
        return 1

    log(f"loading model {model_id} (this is a one-time cost per process)")
    try:
        model = SentenceTransformer(model_id, cache_folder=cache_dir, device="cpu")
    except Exception as exc:  # noqa: BLE001 - any load failure is fatal and must be reported
        if not ALLOW_DOWNLOAD:
            fail(f"could not load model {model_id} from the local cache: {exc}. "
                 "The worker runs offline; download the weights once with "
                 "QF_V2_ALLOW_MODEL_DOWNLOAD=1")
        else:
            fail(f"could not load model {model_id}: {exc}")
        return 1

    dim = int(model.get_sentence_embedding_dimension())
    revision = resolve_revision(model, model_id, cache_dir)
    log(f"ready: dim={dim} revision={revision}")

    for line in sys.stdin:
        line = line.strip()
        if not line:
            continue
        try:
            request = json.loads(line)
        except json.JSONDecodeError as exc:
            fail(f"request is not valid JSON: {exc}")
            continue

        op = request.get("op")
        if op == "identity":
            respond({
                "ok": True,
                "model": model_id,
                "revision": revision,
                "dim": dim,
                # Vectors are L2-normalised below, so cosine similarity is a
                # plain dot product on the Go side.
                "normalized": True,
                # Distinguishes a real semantic model from the deterministic
                # stand-in used in tests. The Go side refuses to record research
                # results from a non-semantic provider.
                "semantic": True,
            })
        elif op == "embed":
            handle_embed(model, request)
        elif op == "shutdown":
            respond({"ok": True})
            return 0
        else:
            fail(f"unknown op {op!r}")

    return 0


def handle_embed(model, request: dict) -> None:
    texts = request.get("texts")
    if not isinstance(texts, list) or not texts:
        fail("embed requires a non-empty 'texts' list")
        return
    if len(texts) > MAX_BATCH:
        fail(f"batch of {len(texts)} exceeds the {MAX_BATCH} limit")
        return
    for text in texts:
        if not isinstance(text, str):
            fail("every entry in 'texts' must be a string")
            return
        if len(text) > MAX_TEXT_LENGTH:
            fail(f"a text of {len(text)} characters exceeds the {MAX_TEXT_LENGTH} limit")
            return

    try:
        vectors = model.encode(
            texts,
            batch_size=32,
            convert_to_numpy=True,
            # Normalising here rather than in Go keeps the invariant in one
            # place and makes cosine similarity a dot product downstream.
            normalize_embeddings=True,
            show_progress_bar=False,
        )
    except Exception as exc:  # noqa: BLE001 - surfaced over the protocol
        fail(f"embedding failed: {exc}")
        return

    respond({"ok": True, "vectors": [[float(x) for x in row] for row in vectors]})


def resolve_revision(model, model_id: str, cache_dir) -> str:
    """Best-effort identity of the exact weights in use.

    Recorded so a retrieval number can be traced to specific weights, not just
    to a model name that upstream may have since re-uploaded (brief, section 25).
    It is also part of every cached index's identity, so it must come out the
    same offline as it did online, or every cache would look stale.
    Never fatal: an unknown revision is worth reporting, not worth refusing to
    start over.

    In order: the model's own metadata; the LOCAL cache's snapshot directory,
    whose name is the commit the weights were downloaded at (no network); and
    only when downloads are explicitly allowed, the hub's model_info(). Before
    Phase 8 the hub call was the one that answered, on every start.
    """
    for attr in ("model_card_data", "config"):
        holder = getattr(model, attr, None)
        for key in ("base_model_revision", "revision", "_commit_hash"):
            value = getattr(holder, key, None)
            if isinstance(value, str) and value:
                return value
    try:
        from huggingface_hub import snapshot_download

        path = snapshot_download(model_id, cache_dir=cache_dir, local_files_only=True)
        name = os.path.basename(os.path.normpath(path))
        if len(name) == 40 and all(c in "0123456789abcdef" for c in name):
            return name
    except Exception:  # noqa: BLE001 - not cached this way; fall through
        pass
    if ALLOW_DOWNLOAD:
        try:
            from huggingface_hub import model_info

            info = model_info(model_id)
            if info.sha:
                return info.sha
        except Exception:  # noqa: BLE001 - offline or unreachable is fine
            pass
    return "unknown"


if __name__ == "__main__":
    sys.exit(main())
