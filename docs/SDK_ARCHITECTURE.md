# How the Python and Java SDKs run the Go engine

This document explains, end to end, how `queryforge-ai` (PyPI) and `io.github.awsaman-ai:queryforge`
(Maven Central) let a Python or Java program call a natural-language-to-query engine that is
entirely written in Go — with no server, no Go toolchain, and (from the caller's point of view) no
visible subprocess at all.

It is written for an engineer who has never touched this repo. If you already know the protocol
shape, skip to whichever section names your question:

- [The one-sentence version](#the-one-sentence-version)
- [Why a subprocess and not a server, a C binding, or a rewrite](#why-a-subprocess-and-not-a-server-a-c-binding-or-a-rewrite)
- [The wire protocol](#the-wire-protocol-stdin-in-stdout-out) — what actually goes over stdin/stdout
- [How the binary gets inside the package](#how-the-binary-gets-inside-the-package) — build & packaging
- [How the Python SDK finds and runs it](#how-the-python-sdk-finds-and-runs-it)
- [How the Java SDK finds and runs it](#how-the-java-sdk-finds-and-runs-it)
- [The public API shape (both languages)](#the-public-api-shape-both-languages)
- [Error handling: wire code → native exception](#error-handling-wire-code--native-exception)
- [Credentials: how an API key reaches the engine](#credentials-how-an-api-key-reaches-the-engine)
- [Logging: how SDK and engine logs correlate](#logging-how-sdk-and-engine-logs-correlate)
- [Versioning and compatibility rules](#versioning-and-compatibility-rules)
- [The release pipeline: from `git tag` to `pip install`](#the-release-pipeline-from-git-tag-to-pip-install)
- [Reading the actual source, file by file](#reading-the-actual-source-file-by-file)

---

## The one-sentence version

The Go engine is compiled once per OS/CPU combination into a completely standalone executable
called `queryforge`; that executable is stuffed inside the Python wheel / Java jar as ordinary
binary data; and at call time, the SDK spawns it as a child process, writes one JSON object to its
stdin, reads one JSON object back from its stdout, and turns that into a native `Result` object or
a native exception. There is no IPC beyond "write a pipe, read a pipe." No socket, no port, no
daemon, nothing stays running between calls.

```
 Python or Java caller                    child process (per call)
┌─────────────────────────┐              ┌───────────────────────────┐
│ QueryForge.mysql(cfg)    │   spawn      │  ./queryforge (Go binary) │
│   .query("delivered      │ ───────────► │                           │
│    orders over $100")    │  stdin: 1    │  parses config            │
│   .toSql()                │  JSON object │  calls the LLM            │
│                           │ ◄─────────── │  validates the AST        │
│  <- native Result object  │  stdout: 1   │  compiles SQL/Mongo       │
│     or a thrown exception │  JSON object │  writes JSON, exits       │
└─────────────────────────┘              └───────────────────────────┘
```

Everything that decides what a question *means* — parsing intent, building the AST, validating it
against your config, compiling it to a SQL string or a Mongo document — happens inside that one Go
binary. The SDKs contain zero query logic. Their entire job is: locate the binary, spawn it
correctly, and translate JSON to and from native types. That is *why* Python, Java, and (per the
project's docs) eventually Node and .NET can all promise **byte-identical output for the same
input** — they are not five reimplementations of a compiler, they are five thin drivers for the
same one.

---

## Why a subprocess and not a server, a C binding, or a rewrite

Three choices were available and rejected for concrete reasons (see `cmd/queryforge/PROTOCOL.md`
and the top-of-file comment in `cmd/queryforge/main.go`):

1. **Not a long-running HTTP/gRPC server.** That would mean every consumer has to *operate*
   something — a port to open, a process to keep alive, a health check to wire up. A one-shot
   subprocess needs none of that; it has "the caller's own privileges" and nothing more, which is
   also a smaller security surface than a listening socket.
2. **Not a CGO/JNI binding.** That would tie every SDK's build to a C compiler and a matching
   native-toolchain-per-platform-per-language matrix. A cross-compiled static Go binary sidesteps
   all of it: `CGO_ENABLED=0 GOOS=... GOARCH=... go build` produces a binary with **no libc
   dependency at all**, built entirely from one Linux CI runner, for five platforms, with no C
   toolchain involved anywhere in the chain.
3. **Not a per-language reimplementation.** Parsing, AST validation, and SQL/Mongo generation are
   the entire value of this project and are subtle (injection safety, dialect quoting, nested-field
   handling). Reimplementing that in Java and Python would mean three copies that can silently
   drift apart. A subprocess means there is exactly one implementation, ever.

The trade-off accepted in exchange: a process-spawn cost per call (tens of milliseconds), and a
binary that has to be shipped and located correctly per platform. Both of those problems are what
the rest of this document is about.

---

## The wire protocol: stdin in, stdout out

Full spec: `cmd/queryforge/PROTOCOL.md`. The essential contract, currently at **protocol version
1.2**:

```
echo '{"op":"version"}' | queryforge
```

- **stdout carries exactly one JSON object and nothing else.** All diagnostics (warnings, panics,
  structured logs) go to **stderr**. An SDK parses stdout unconditionally; anything else landing
  there would corrupt the stream.
- **The exit code is not the success signal.** A well-formed failure (bad config, model refusal,
  validation error) exits `1` *with a complete JSON error object on stdout*. Only `2` means "no
  response could be produced at all — read stderr." Both SDKs explicitly ignore the exit code and
  decode stdout instead; branching on it would throw away a perfectly good structured error.

| Exit code | Meaning |
|---|---|
| `0` | `success: true` |
| `1` | `success: false` — a structured error object is on stdout |
| `2` | Nothing usable on stdout at all; see stderr |

### Request shape

```json
{
  "op": "translate",
  "backend": "mysql",
  "config": { "entity": "Order", "fields": [] },
  "query": "delivered orders over $100",
  "scope": { "tenantId": "T-42" },
  "options": { "timeoutMs": 30000, "maxRepairs": 2, "logLevel": "info", "requestId": "abc-123" }
}
```

| Op | Calls the model? | Needs an API key? | What it returns |
|---|---|---|---|
| `translate` | Yes | Yes | AST + compiled query + explanation |
| `generate` | No | No | Compiled query, from a caller-supplied AST |
| `validate` | No | No | Explanation, or the findings that make the AST illegal |
| `version` | No | No | Engine version, protocol version, registered backends |

**Unknown fields in the request are rejected outright** (`json.Decoder.DisallowUnknownFields()` in
Go). This is deliberate, not an oversight: a caller who misspells `"scop"` as `"scope"` gone wrong
must not get back a *plausible-looking, silently-wrong* query with the tenancy filter missing.
Rejecting is the only failure mode that cannot be mistaken for success.

### Response shape

Exactly one of two halves is populated, selected by `success`:

```json
// success
{
  "success": true, "protocol": "1.2", "op": "translate", "backend": "mysql",
  "sql": "SELECT status, amount FROM orders WHERE (tenant_id = ? AND amount > ?)",
  "args": ["T-42", 100],
  "ast": { "...": "..." }, "explain": "...", "warnings": [ "..." ],
  "providerUsed": "groq/llama-3.3-70b", "repairAttempts": 0
}
```

```json
// failure
{
  "success": false, "protocol": "1.2", "op": "generate",
  "code": "VALIDATION_FAILED",
  "message": "filter: unknown field \"amont\" (did you mean: amount?)",
  "details": [ { "code": "unknown_field", "path": "filter", "field": "amont",
                 "suggestions": ["amount"] } ]
}
```

`code` is the coarse, protocol-level classification an SDK maps to an exception **class**.
`details[].code` is a finer engine-owned vocabulary (`unknown_field`, `value_out_of_domain`, …)
used for message text and "did you mean" suggestions, never for control flow. The full code table:

| Code | Meaning | Retryable |
|---|---|---|
| `INVALID_REQUEST` | Malformed request / missing required field | No |
| `UNKNOWN_OP` | Op the engine doesn't implement (SDK newer than engine) | No |
| `INVALID_CONFIG` | Config failed to parse or broke a structural rule | No |
| `UNKNOWN_BACKEND` | No generator registered for that backend id | No |
| `INVALID_SCOPE` | A caller-supplied scope filter was rejected (app bug) | No |
| `VALIDATION_FAILED` | AST broke a config rule; `details` says which | No |
| `UNSUPPORTED_REQUEST` | Model declined — not expressible in this config | No |
| `POLICY_VIOLATION` | AST legal but broke a `policy.requires` business rule | No |
| `MODEL_OUTPUT` | Model answered but never with usable JSON | Yes |
| `MODEL_TRANSPORT` | Model never reached — network, key, rate limit | Yes |
| `GENERATE_FAILED` | Legal AST couldn't be compiled for this backend | No |
| `TIMEOUT` | `options.timeoutMs` deadline expired | Yes |
| `INTERNAL` | A bug in the engine | No |

**A code this SDK version has never heard of must degrade to the base exception class, never
crash.** That is what lets an old SDK keep working against a newer engine that has learned a new
failure mode.

---

## How the binary gets inside the package

Neither SDK compiles Go at install time. The binary is built once, in CI, and copied verbatim into
the artifact.

### Step 1 — cross-compile five targets on one runner

`scripts/build-binaries.sh` runs, per platform:

```bash
CGO_ENABLED=0 GOOS="$goos" GOARCH="$goarch" \
  go build -trimpath -ldflags "-s -w -X main.Version=${VERSION}" -o "${target_dir}/${binary}" ./cmd/queryforge
```

for `linux-amd64`, `linux-arm64`, `darwin-amd64`, `darwin-arm64`, `windows-amd64`, producing:

```
dist/
  linux-amd64/queryforge
  linux-arm64/queryforge
  darwin-amd64/queryforge
  darwin-arm64/queryforge
  windows-amd64/queryforge.exe
  checksums.txt
```

Three things about this step matter for everything downstream:

- **`CGO_ENABLED=0`** produces a fully static binary with no libc dependency — the single most
  common way a bundled native binary breaks in the wild (glibc version mismatch) simply cannot
  happen here. It's also *why* the Linux wheels can legally claim both `manylinux` and `musllinux`
  tags (see below) — the same binary genuinely runs on glibc and on Alpine.
- **`-ldflags "-X main.Version=..."`** stamps the release version into the binary at build time, so
  `queryforge --version` and the `version` op always report the release they actually came from,
  never "dev" (that string is reserved for a local `go build` with no ldflags, so a locally-built
  binary can never be confused for a shipped one).
- **The directory names (`linux-amd64`, …) are load-bearing.** Both `BinaryResolver.java` and
  `_binary.py` independently *compute* this exact same `<os>-<arch>` tag string at runtime and use
  it to look up the matching binary. A rename here has to happen in three places in lockstep, and
  is called out explicitly in the script's own header comment.

### Step 2a — Python: one wheel per platform

`scripts/build-wheels.py` builds a **separate wheel per platform**, not one universal bundle. The
reasoning: bundling all five ~15 MB binaries into every install would triple or quadruple every
user's download for four executables they will never run. Instead:

1. Copy exactly one platform's binary into `sdk-python/queryforge/bin/`.
2. Run `python -m build --wheel`, which — because there's no C extension — produces a generic
   `py3-none-any` wheel.
3. **Retag it** after the fact with `wheel tags --remove --platform-tag <tags>`, e.g.
   `manylinux2014_x86_64.musllinux_1_1_x86_64` for `linux-amd64`. This is what makes `pip install
   queryforge-ai` on a given machine resolve to *only* the wheel containing a binary that machine
   can execute — pip's own platform-tag matching does the platform selection; the SDK code never
   has to guess "did I get the right wheel," only "did I get the right binary inside it" (a much
   narrower, still-checked, invariant).
4. Repeat for all five platforms, upload all five wheels to the same PyPI release.

`pyproject.toml` backs this up: `include-package-data = true` plus
`[tool.setuptools.package-data] queryforge = ["bin/*", "bin/*/*", "py.typed"]` — needed because the
bundled executable has no file extension on Unix, so setuptools' default extension-based file
matching would otherwise silently drop it from the wheel.

### Step 2b — Java: classifier jars, no auto-selection

Java takes a different shape for the same underlying problem. `sdk-java/pom.xml`:

- The **main jar** (`queryforge-1.2.0.jar`) carries only `.class` files — zero binaries, zero
  runtime dependencies (deliberately: "a thin wrapper that drags in Jackson forces its version on
  every application that uses it").
- **Five classifier jars** (`queryforge-1.2.0-linux-amd64.jar`, `...-darwin-arm64.jar`, …) each
  carry exactly one binary, placed at the resource path `/io/queryforge/bin/<tag>/queryforge` —
  which is precisely where `BinaryResolver.extractFromJar()` looks for it. Each is produced by its
  own `maven-jar-plugin` execution in the pom, each skipped cleanly (`skipIfEmpty`) when its native
  directory doesn't exist, so a plain `mvn test` on a laptop with no cross-compiled binaries still
  works.
- **A consumer names the classifier they want explicitly**, e.g.:
  ```xml
  <dependency>
    <groupId>io.github.awsaman-ai</groupId>
    <artifactId>queryforge</artifactId>
    <version>1.2.0</version>
    <classifier>linux-amd64</classifier>
  </dependency>
  ```
  There is **no automatic OS-detection profile** doing this for you, and the pom's own top comment
  explains why one existed once and was removed: a Maven profile that adds a classifier of *your
  own artifact* as a dependency is evaluated when *this* artifact is built, so `mvn test` on Linux
  tried to resolve `io.queryforge:queryforge:jar:linux-amd64` from Central — i.e., itself, which
  doesn't exist yet — and failed on all five platforms simultaneously. Fixing this properly requires
  moving the natives into a **separate artifactId** in their own module (the shape
  `netty-tcnative` uses), which hasn't been done. So today, Java consumers pick their own
  classifier by hand.

---

## How the Python SDK finds and runs it

Two files: `sdk-python/queryforge/_binary.py` (find it) and `sdk-python/queryforge/_transport.py`
(run it).

### Finding the binary (`_binary.py`)

```python
def platform_tag() -> str:
    # sys.platform -> "linux" | "darwin" | "windows"
    # platform.machine() -> normalized to "amd64" | "arm64"
    return f"{os_name}-{arch}"
```

`find_binary()` searches, in order:

1. **`QUERYFORGE_BINARY` environment variable**, if set. If it points at a path that doesn't exist
   or isn't executable, this is a **hard failure with a specific message** ("chmod +x ..."), never
   a silent fallback to the bundled binary — falling back would mean running a *different* engine
   than the one the caller explicitly named, defeating the entire point of the override.
2. `<package_dir>/bin/queryforge` (or `.exe`) — the normal case for a correctly installed
   per-platform wheel.
3. `<package_dir>/bin/<platform_tag>/queryforge` — a fallback layout for a hand-built "universal"
   package or a dev checkout with multiple platforms staged.

If nothing executable is found, the error message distinguishes **"present but not executable"**
(one-line fix: `chmod +x`, usually caused by an archive tool stripping mode bits) from **"absent
entirely"** (usually the wrong wheel got installed — points the user at
`pip install --force-reinstall queryforge-ai`).

`resolve_binary()` **caches the result for the life of the process** (module-level `_cached`
variable), because the default call pattern spawns one subprocess per query and re-probing the
filesystem on every single call would be wasted work. The cache is deliberately bypassed whenever
`QUERYFORGE_BINARY` is set, so a test suite or notebook can repoint it between calls.

### Running it (`_transport.py`)

`run_request(request, timeout_seconds, request_id, credentials)` is the entire communication layer:

```python
completed = subprocess.run(
    [str(binary)],
    input=payload,              # the JSON request, as a string
    capture_output=True,
    text=True,
    timeout=kill_after,         # request timeout + 15s grace
    shell=False,                # NEVER a shell — config/query are untrusted user data
    env=build_env(credentials), # None = inherit; a dict = process env + credentials merged in
)
```

Key details worth knowing if you're debugging or extending this:

- **`shell=False` is load-bearing security, not a style choice.** The config and the natural-
  language question are both effectively user-controlled data; passing them through a shell would
  make them potentially executable. `subprocess.run` with an argv list (here, just `[binary]`, with
  the JSON going over stdin — not argv) never invokes a shell to interpret anything.
- **`timeout=kill_after` is a safety net, not the real deadline.** The engine enforces its own
  `options.timeoutMs` internally and reports a structured `TIMEOUT` error on stdout when it fires.
  This outer `subprocess.run` timeout (`timeout_seconds + 15.0` grace seconds) exists purely to
  catch a pathological case — an engine bug that hangs past its own deadline — and if it fires, the
  SDK explicitly says so in the raised error: *"This is a bug in the engine — its own deadline
  should have produced a TIMEOUT error first."*
- **The exit code is never consulted for success/failure**, only stdout content, per the protocol
  contract above.
- `_decode()` walks through: empty stdout → `ProtocolError` (with redacted stderr attached as
  context) → not-valid-JSON → not-an-object → protocol major-version mismatch → `success: false` →
  raise the mapped exception; only a fully successful, protocol-compatible object is returned.

---

## How the Java SDK finds and runs it

Two files: `BinaryResolver.java` (find/extract it) and `Transport.java` (run it). Java's problem is
harder than Python's in one specific way, and everything in `BinaryResolver` exists because of it:

> **A jar entry is not a file on disk — it cannot be executed where it sits.** It has to be
> extracted to a real filesystem path first.

### Extraction and caching (`BinaryResolver.java`)

`resolve()`:

1. Checks for an override — `-Dqueryforge.binary=...` (system property, wins) or
   `QUERYFORGE_BINARY` (env var). Two mechanisms exist because they serve different operators: the
   env var is what a container image or shell profile sets and is the *same name every QueryForge
   SDK honours*; the system property exists because **Java has no supported way to set an
   environment variable for its own already-running process**, so without a property, a pure-JVM
   caller (a test, a launch script using `-D`) would have no way to redirect the binary at all.
2. Otherwise, checks an in-memory cache (`static volatile Path cached`).
3. On a cache miss, calls `extractFromJar()`, which:
   - Reads the resource at `/io/queryforge/bin/<platformTag>/queryforge[.exe]` from the classpath
     (this is exactly the path the classifier jar's `maven-jar-plugin` execution placed it at).
   - Computes a **content-addressed cache location**:
     `${java.io.tmpdir}/queryforge-bin/queryforge-<tag>-<first-8-bytes-of-sha256-hex>[.exe]`
     (or `$QUERYFORGE_CACHE_DIR` / `-Dqueryforge.cacheDir` if set).
   - If a file already exists there with the same size, it's reused as-is (no full re-read/re-hash
     needed — the name already encodes the content hash, so a name+length match is enough).
   - Otherwise, writes to a **unique temp file in the same directory, then atomically moves it into
     place** (`ATOMIC_MOVE`, with a plain `REPLACE_EXISTING` fallback for filesystems — some network
     mounts — that reject atomic moves). This specifically defends against two JVMs starting
     concurrently: without the temp-file-then-move dance, one process could execute a half-written
     file the other was still copying.

Why content-addressing matters: **two applications on one machine using two different QueryForge
versions get two different cache entries automatically.** An upgrade can never be silently defeated
by a stale file that happens to have the expected name — the name itself changes when the bytes do.

`platformTag()` mirrors `_binary.py`'s logic exactly (same `<os>-<arch>` string), reading
`os.name`/`os.arch` system properties instead of Python's `sys.platform`/`platform.machine()`.

### Running it (`Transport.java`)

This is the one place where Java's implementation is meaningfully more involved than Python's, for
a specific, well-documented reason: **`subprocess.run()` in Python drains stdout/stderr for you;
`ProcessBuilder` in Java does not.**

```java
Process process = builder.start();

StreamReader stdout = new StreamReader(process.getInputStream(), "stdout");
StreamReader stderr = new StreamReader(process.getErrorStream(), "stderr");
stdout.start();
stderr.start();

try (OutputStream in = process.getOutputStream()) {
    in.write(payload);
}
```

The comment in the source states the hazard directly: *"A subprocess whose stdout pipe fills will
block; if you are still writing the request, both sides wait forever."* OS pipe buffers are
finite (typically 64 KB); a large config plus a large model response can exceed that. If the SDK
wrote the whole request to stdin synchronously *before* starting to read stdout, and the child
filled its stdout buffer before finishing reading stdin, you'd get a genuine deadlock — both sides
blocked on a full pipe forever. `StreamReader` is a small daemon-thread class that drains one
stream into a `ByteArrayOutputStream` continuously and independently of the main thread's write to
stdin, which is exactly what Python's `subprocess.run` does for you internally with its own thread
pool.

`StreamReader.await()` — waiting for the drain to finish — has one deliberate, non-obvious
behavior: **an incomplete drain throws rather than returning a partial buffer.** The doc comment
explains the bug this fixes: a truncated JSON document is not obviously truncated — the SDK would
have surfaced it as a generic "produced output that is not JSON" `ProtocolError`, sending whoever
is debugging it toward the *engine's* JSON encoder, when the real fault was on the Java side of the
pipe (the reader thread got interrupted, or didn't finish inside `killGraceMillis`). This was closed
as **BUG-045 (High)** during the enterprise-logging pass — see `[[queryforge-logging-error-handling]]`
in project memory.

Everything after that — protocol version check, `success` branch, exception construction — mirrors
`_transport.py`'s `_decode()` structurally, just in Java: `Json.readObject(raw)` (a small hand-rolled
JSON parser — see below), `checkProtocol()`, `toException()`.

### `Json.java` — why there's no Jackson/Gson

The pom's own top comment states the constraint plainly: **zero runtime dependencies.** Pulling in
Jackson would force its version onto every application embedding this SDK, and a version conflict
inside someone else's Spring app is judged a worse experience than maintaining a small hand-rolled
JSON reader/writer. `Json.java` implements just enough JSON to round-trip the protocol's request/
response shapes — objects, arrays, strings, numbers, booleans, null — with no schema binding, no
annotations, no reflection.

---

## The public API shape (both languages)

Both SDKs converge on the identical two-path design the protocol itself exposes:

| | Model call? | Needs API key? | Use for |
|---|---|---|---|
| `.query(text)` → terminal (`toSql()`/`toMongo()`/`explain()`/…) | Yes | Yes | The actual NL→query pipeline |
| `.generate(ast)` / `.validate(ast)` | No | No | Re-compiling a stored AST for a second backend; tests |

**Python** wraps `.query(text)` in a `PendingQuery` — an immutable builder. Nothing runs until a
terminal method (`.to_sql()`, `.to_mongo()`, `.explain()`, `.result()`, …) is called, and the result
is cached on first call, so reading `.to_sql()` then `.explain()` off the same `PendingQuery` costs
**one** model call, not two. Every builder method (`.scope(...)`, `.timeout(...)`,
`.max_repairs(...)`, …) returns a **new** `PendingQuery` rather than mutating the receiver — which
is what makes a partially-built query safe to reuse as a template:

```python
base = qf.query("open orders").scope({"tenantId": tenant})
sql = base.to_sql()
prose = base.explain()   # same cached result, no second model call
```

**Java**'s `PendingQuery` follows the identical pattern (same file list: `PendingQuery.java`,
`QueryForgeResult.java`). `QueryForge` itself is immutable and thread-safe in both languages —
`.withScope(...)`, `.withTimeout(...)`, `.withCredentials(...)` in Java, and constructor kwargs plus
`PendingQuery` builders in Python, all return new instances.

Both languages offer named backend factories that map onto the same three engine backend ids:

```python
QueryForge.mysql(config)      # -> backend "mysql" ($? placeholders, backtick quoting)
QueryForge.postgres(config)   # -> backend "sql"   ($1 placeholders)
QueryForge.sql(config)        # alias for postgres — "sql" is the engine's own dialect id
QueryForge.mongo(config)      # -> backend "mongo", produces a query document, not SQL
```

```java
QueryForge.mysql(config);
QueryForge.postgres(config);
QueryForge.sql(config);       // alias for postgres, same reasoning
QueryForge.mongo(config);
```

`config` may be a parsed object, a file path, or (Python only, detected by a leading `{`) a raw JSON
string. Java offers explicit `Map` vs. `Path` overloads plus a `QueryForge.parseConfig(String)`
static helper — deliberately **not** an ambiguous `String` overload alongside `Path`, since at a
call site `QueryForge.mysql(s)` would be unreadable as to whether `s` is a path or the config body.

---

## Error handling: wire code → native exception

Both SDKs maintain a table mapping the wire `code` string onto a specific exception class, with an
explicit "unknown code" fallback to the shallow common base:

**Python** (`errors.py`, `_CODE_TO_ERROR` dict) — `QueryForgeError` is the root; leaves include
`InvalidRequestError`, `InvalidConfigError`, `UnknownBackendError`, `InvalidScopeError`,
`ValidationError`, `UnsupportedRequestError`, `PolicyViolationError`, `ModelOutputError`,
`ModelTransportError`, `GenerateError`, `TimeoutError` (shadows the builtin *inside this module
only*; `builtins.TimeoutError` remains reachable), `BinaryNotFoundError`, `ProtocolError`. A code
absent from the table (i.e., from a newer engine) falls back to the bare `QueryForgeError` base —
this is what lets an old SDK talk to a new engine without crashing on an error class it predates.

**Java** mirrors this with one exception class per file (`ValidationException.java`,
`UnsupportedRequestException.java`, `PolicyViolationException.java`, `TimeoutException.java`,
`BinaryNotFoundException.java`, `ProtocolException.java`, …), each extending
`QueryForgeException`, with `QueryForgeException.fromCode(code, message, details, policyError)`
doing the same table lookup.

Two error classes are semantically distinct from "something broke," and both SDKs document this:

- **`UnsupportedRequestError`/`UnsupportedRequestException`** — the model *declined*; the question
  legitimately cannot be expressed against this config. This is a well-formed answer, not a
  pipeline failure — its message is written to be shown directly to the end user who asked the
  question.
- **`PolicyViolationError`/`PolicyViolationException`** — the AST was fully legal (every field,
  operator, and value passed ordinary validation) but broke a cross-field `policy.requires`
  business rule declared in the config (e.g., "passport expiry needs a country"). Carries a
  `policy`/`PolicyErrorDetail` object separate from `details`, because `details` is per-field
  validation findings on an AST that couldn't be expressed *at all*, while a policy violation is a
  legal AST rejected for a business reason — conflating the two would blur "your input was invalid"
  with "your input was fine but against policy," which callers need to present differently.

`ValidationError`/`ValidationException` carries a `details: [Detail]` list — one entry per finding,
each with `code` (engine's own lower_snake vocabulary — deliberately visually distinct from the
protocol's SCREAMING_SNAKE `code`, so the two can never be confused when read side by side),
`path`, `field`, `message`, and `suggestions` (nearest-match field names for a likely typo).

---

## Credentials: how an API key reaches the engine

`translate` is the **only** op that calls the model, and therefore the only op that needs an API
key. Both SDKs enforce this at the code-path level, not just by documentation: `generate()` and
`validate()` never pass `credentials` into the subprocess call at all — spawning an engine process
that literally never receives the key, for the smallest blast radius available "for free" (direct
quote from both `queryforge.py` and `QueryForge.java`'s internal comments).

Credentials are **environment variables passed to the child process, never fields in the JSON
request body.** The reasoning, stated identically in both SDKs' source comments: the request body
is the one structure in the SDK that *travels* — it gets JSON-encoded, it's what shows up when the
protocol breaks, and it's the natural thing someone pastes into a bug report. The environment goes
to exactly one subprocess and is never serialized by the SDK at all.

```python
QueryForge.mysql(
    "orders.config.json",
    credentials={"QF_API_KEY": vault.get("openai/key")},
)
```
```java
QueryForge qf = QueryForge.mysql(configPath)
    .withCredentials(Collections.singletonMap("QF_API_KEY", vault.read("openai/key")));
```

The **key** (`"QF_API_KEY"`) must be a syntactically valid POSIX environment variable name — both
SDKs validate this with the identical regex-equivalent rule (`^[A-Za-z_][A-Za-z0-9_]*$`), matching
what the engine itself enforces on `model.apiKeyEnv` in the config. This also happens to catch the
single most likely user mistake — passing the *secret value* where the *variable name* belongs —
since real API keys contain hyphens or a lowercase provider prefix and fail the name-shape check
immediately, before anything is spawned.

Both SDKs build the child's environment as a **copy of the current process environment with
credentials merged in**, never a bare replacement — handing the child process a raw credentials-only
map would strip `PATH`, `HOME`, `TMPDIR`, etc. and break the engine in ways that look nothing like a
credentials problem.

---

## Logging: how SDK and engine logs correlate

Both SDKs are silent by default. Python hooks the standard `logging` module under the `queryforge`
namespace; Java uses `java.util.logging` (again: zero dependencies, no SLF4J) via
`QueryForgeLogging`.

When the *host application* configures the SDK's logger to a level, the SDK — and only then — adds
`options.logLevel` and `options.requestId` to the outgoing request, so the engine's own stderr logs
land at a matching verbosity and share a correlation id with the SDK's own log lines. This is
deliberately conditional, not automatic, because of a real protocol asymmetry documented in
`PROTOCOL.md`: a new engine reading an old (pre-1.1) request is fine (`logLevel` is optional), but
an *old* engine reading a request with `logLevel` set is not — that engine rejects unknown fields
outright (`INVALID_REQUEST`). Always sending it would silently break every caller pointing
`QUERYFORGE_BINARY` at an older build. So the default request stays byte-identical to a
protocol-1.0 request unless the host explicitly opted into logging.

Both SDKs log exactly one line per call at the boundary: one `INFO` on success (with duration,
`repairAttempts`, and `providerUsed` if present — a translation that needed three model calls to
pass validation is almost always a config gap worth knowing about), one `ERROR`/`SEVERE` on failure
(with the exception type, code, and duration). Neither SDK ever logs the question text, scope
*values*, or config *contents* — only shape (`entity`, `backend`, scope *keys*, not values). This
mirrors a privacy contract enforced by canary tests on the Go side too (see
`docs/OBSERVABILITY.md`).

---

## Versioning and compatibility rules

`protocol` (currently `"1.2"`) travels on every response. Both SDKs check **only the major
component**:

- **MINOR bump** (e.g. 1.1 → 1.2) is additive-only: a new op, a new optional response field. An old
  SDK talking to a newer engine simply never reads the new field and behaves exactly as before.
- **MAJOR bump** means an existing field changed meaning or disappeared. Continuing would produce
  *quietly wrong* output rather than a clean error, so both SDKs refuse outright with a
  `ProtocolError`/`ProtocolException` naming both versions.
- A response missing `protocol` entirely is treated as coming from an engine too old to speak to at
  all, and is refused the same way.

This is why the SDK's own `PROTOCOL_VERSION` constant (`"1.2"` in both `_transport.py` and
`Transport.java`) must be bumped in lockstep with a protocol change on the Go side, and why every
new *optional* field the engine adds is safe for an already-shipped SDK to receive without a
release of its own.

---

## The release pipeline: from `git tag` to `pip install`

One GitHub Actions job (`.github/workflows/release.yml`, `build`) does the entire cross-language
build on a **single Ubuntu runner** — deliberately, because Go cross-compiles without a
per-target toolchain and neither SDK has a compiled extension, so there is nothing a build matrix
would buy beyond "five runners that could disagree about what they built":

1. `go vet` + `go test -race` against the engine (offline, stub model provider — no API key needed
   or consumed).
2. `scripts/build-binaries.sh` cross-compiles all five platform binaries; a verification step
   confirms every expected artifact actually exists (catching, e.g., a missing target before it
   ships as "works on four platforms, silently 'no executable bundled' on the fifth").
3. **Python:** run the Python test suite *against the freshly built Linux binary*
   (`QUERYFORGE_BINARY=.../dist/linux-amd64/queryforge`) — so CI is testing the engine that is
   about to ship, not whatever happened to already be on the runner. Then rewrite the version
   string into `pyproject.toml` and `queryforge/__init__.py`, build all five wheels via
   `build-wheels.py`, run `twine check`, and — critically — **install the actual Linux wheel into a
   clean venv and run a real query through it**, asserting the resulting SQL string. This is the
   check that would catch a wheel whose binary lost its executable bit, or that got built with the
   wrong platform's binary accidentally staged.
4. **Java:** stage each platform's binary into `target/native/<tag>/io/queryforge/bin/<tag>/`,
   rewrite the version in both `pom.xml` (`mvn versions:set`) *and* the
   `QueryForgeLogging.SDK_VERSION` constant (`scripts/set-java-sdk-version.sh` — these two are held
   in sync by a test, because rewriting only the pom leaves every log line lying about its own
   build), `mvn package`, then unzip each resulting classifier jar and grep for its binary's
   resource path to confirm it's actually inside.
5. All three artifact sets (binaries, wheels, jars) are uploaded as CI artifacts.
6. **Publishing is a separate, manual, `workflow_dispatch`-only step**, gated on
   `inputs.publish == 'pypi'` or `'maven'` — never both in one run, and never automatically on tag
   push. Neither PyPI nor Maven Central permits deleting or reusing a version number once uploaded,
   so nothing reaches either registry without a human explicitly choosing to, one registry at a
   time.

The `queryforge-cross-language-sdk` and `queryforge-release-pipeline` project-memory entries have
further operational detail (a Maven Central quirk around `autoPublish=false` requiring a manual
click, PyPI naming history, etc.) that is out of scope for *how the SDKs work* but relevant if
you're the one running a release.

---

## Reading the actual source, file by file

If you want to go straight to the code instead of this document, read in this order:

1. **`cmd/queryforge/PROTOCOL.md`** — the actual protocol spec; the ground truth this document
   summarizes.
2. **`cmd/queryforge/main.go`** — the Go side of the contract: reads stdin, decodes with
   `DisallowUnknownFields`, dispatches, encodes exactly one response object, picks the exit code.
3. **Python:**
   - `sdk-python/queryforge/_binary.py` — locate the executable.
   - `sdk-python/queryforge/_transport.py` — spawn it, decode its reply, raise on failure.
   - `sdk-python/queryforge/__init__.py` — the public `QueryForge` / `PendingQuery` API.
   - `sdk-python/queryforge/errors.py` — the exception hierarchy and code table.
4. **Java:**
   - `sdk-java/src/main/java/io/queryforge/BinaryResolver.java` — extract-from-jar + cache.
   - `sdk-java/src/main/java/io/queryforge/Transport.java` — spawn it, concurrent stream draining,
     decode, exception mapping.
   - `sdk-java/src/main/java/io/queryforge/QueryForge.java` — the public API.
   - `sdk-java/src/main/java/io/queryforge/Json.java` — the zero-dependency JSON codec.
   - `sdk-java/src/main/java/io/queryforge/QueryForgeException.java` and its subclasses.
5. **Packaging:**
   - `scripts/build-binaries.sh` — cross-compilation.
   - `scripts/build-wheels.py` — Python per-platform wheel build + retagging.
   - `sdk-java/pom.xml` — the classifier-jar executions and the "why not auto-selection" comment.
   - `.github/workflows/release.yml` — the full CI pipeline tying all of the above together.
