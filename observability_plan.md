# Observability plan — a narrow event hook for QueryForge

Status: **PROPOSAL, awaiting approval.** Nothing below is implemented.
Date: 2026-08-06.

---

## 1. Why, in one paragraph

The deterministic half of the library is provably not worth instrumenting: a
traced live request on 2026-08-06 measured config load 0.9ms, prompt build
0.1ms, and parse+validate+compile+explain **0.5ms**, against a model call of
1,781ms — **99.97% of wall time is one HTTP round trip**. So this is not a
"logging" feature in the broad sense. It is a narrow record of *the model
interaction*, which is simultaneously the only slow thing, the only expensive
thing, and the only non-deterministic thing in the system.

Every bug in `bugs.csv` that cost real time — BUG-005 (parse failures silently
not consuming the repair budget), BUG-008 (`jsonMode` producing unbalanced
JSON with `finish_reason: stop` and budget untouched, so *nothing flagged it*),
BUG-013 (`between` failing 10/10 and burning the full repair budget every
time) — would have been visible in a single line of structured output. None of
them were, because that line does not exist.

## 2. What is missing today (verified against the current tree)

**Library:** no logging of any kind. `grep -rn "log\.\|slog\|Logger"` over every
non-test `.go` file returns empty. That is correct for a library — it must not
write to someone else's stdout — but there is no *seam* either. The only
injectable point is `ModelProvider`, which is why the 2026-08-06 trace had to
wrap the provider and tee the HTTP transport to see anything.

**Service:** `slog` with one line per request from `withLogging`
(`server.go:92`) carrying method/path/status/duration/client/xff, plus startup
lines and three error lines. It deliberately never logs request bodies
(`server.go:90`) because a question can contain personal data — **that decision
is kept intact by this plan.**

The concrete gaps:

| Gap | Why it hurts |
|---|---|
| No model latency | The service logs total `duration`, but ~99.97% of it is the model. A slow model, a slow repair loop, and a slow handler are indistinguishable. |
| No token usage | `Complete` returns only a string. Hidden reasoning tokens were the single largest cost finding to date; a regression there is invisible in production. |
| `RepairAttempts` lost on failure | It lives on `TranslateResult`, which is `nil` when `Translate` errors — so the expensive case is exactly the case you cannot see. |
| Raw model output lost on failure | `raw` is captured in the loop (`queryforge.go:120`) but only escapes via `TranslateResult.Raw` on success. When the model emits garbage you learn *that* it was unparseable, never *what it said*. |
| No provider identity on the error path | `ProviderUsed` has the same success-only problem, so a fallback chain leaves no record of who failed before someone answered. |
| No correlation id | Nothing ties the request line to the error lines; under concurrency they cannot be grouped. |
| Warnings never logged | Non-indexed-field advisories go to the caller and vanish. |

## 3. Design

### 3.1 The shape: one optional observer, zero cost when unset

```go
// Observer receives one Event per notable step of a translation. It is the
// library's only observability seam: the library itself never writes a log
// line, never formats a message, and never decides a severity — it reports
// facts and lets the caller decide what to do with them.
//
// nil (the default) disables everything, at the cost of one nil check per
// event. An Observer MUST be safe for concurrent use: one Engine may serve
// many goroutines, and events from different translations will interleave.
// It MUST NOT panic and SHOULD return quickly — it is called on the hot path,
// synchronously, and a slow Observer directly slows the translation.
type Observer func(Event)
```

Added as one exported field on `Engine`:

```go
Observe Observer // optional; nil disables all event emission
```

**Why a func and not an interface:** there is exactly one method. A func type
composes trivially (a caller can fan out to two sinks with a three-line
closure) and makes the nil-means-off contract obvious.

**Why synchronous:** the library must not own a goroutine or a buffer. A caller
who wants async can make their Observer a channel send in one line; a caller
who wants the raw output on failure in the same log line needs it synchronous.

### 3.2 The Event

One flat struct, because it is going to be flattened into structured log
attributes anyway. Which fields are populated depends on `Kind`; everything
else is the zero value. Documented per field, not inferred.

```go
// EventKind names what happened. The three kinds form a nesting:
// one Translate contains 1..MaxRepairs+1 Attempts, each containing exactly
// one ModelCall (or zero, if the provider failed before sending).
type EventKind string

const (
    // EventModelCall — one HTTP round trip to a provider finished, success or
    // not. This is the only event carrying latency and token counts.
    EventModelCall EventKind = "model_call"

    // EventAttempt — one plan+parse+validate cycle resolved. Emitted once per
    // loop iteration in Translate, including the ones that fail and repair.
    EventAttempt EventKind = "attempt"

    // EventTranslate — the whole Translate call finished, success or failure.
    // Exactly one per Translate.
    EventTranslate EventKind = "translate"
)

type Event struct {
    Kind    EventKind // which of the three above; always set
    Attempt int       // 0-based repair attempt this event belongs to; always set
    Entity  string    // config entity, e.g. "Order"; always set
    Backend string    // requested backend id, e.g. "sql"; always set

    // --- EventModelCall only ---
    Provider     string        // provider id that answered, e.g. "gemini"
    Model        string        // model id, e.g. "gemini-3.1-flash-lite"
    Latency      time.Duration // wall time of the round trip
    PromptTokens int           // from the provider's usage block; 0 if absent
    CompletionTokens int
    HiddenTokens int // total - prompt - completion; the reasoning-token bill
    TotalTokens  int
    FinishReason string // "stop", "length", ...; the BUG-008 tell

    // --- EventAttempt and EventTranslate ---
    Outcome Outcome // see below; always set on these two kinds
    Err     error   // non-nil when Outcome is not OutcomeOK

    // Raw is the model's reply, populated ONLY on a failed EventAttempt.
    // It is the single most useful debugging field in this struct and the
    // only one that can contain caller data — see §5.
    Raw string

    // --- EventTranslate only ---
    Duration       time.Duration // wall time of the whole Translate
    RepairAttempts int           // final repair count (matches TranslateResult)
    Warnings       []string      // generator advisories
    ScopeKeys      []string      // scope field NAMES only, never values (§5)
}

// Outcome classifies how a step ended. It exists so a caller can branch on a
// value rather than string-matching an error message.
type Outcome string

const (
    OutcomeOK          Outcome = "ok"
    OutcomeParseError  Outcome = "parse_error"      // ErrModelOutput; repairable
    OutcomeValidation  Outcome = "validation_error" // broke a config rule; repairable
    OutcomeRefusal     Outcome = "refusal"          // *UnsupportedRequestError; final
    OutcomeTransport   Outcome = "transport_error"  // ErrModelTransport; final
    OutcomeBudgetSpent Outcome = "budget_exhausted" // all attempts used up
    OutcomeCallerError Outcome = "caller_error"     // bad scope / unknown backend
)
```

### 3.3 Where events are emitted

**`queryforge.go` / `Translate`** — the loop already has every fact:

- top of `Translate`: record `t0`.
- after each `e.planner.Plan(...)` returns: emit `EventAttempt` with the
  outcome derived from the error that is already being classified there
  (`ErrModelOutput` → parse, `*UnsupportedRequestError` → refusal,
  `ErrModelTransport` → transport), and `Raw: raw` when it failed.
- after each `Validate(...)` failure: emit `EventAttempt` with
  `OutcomeValidation`, `Err: verr`, `Raw: raw`.
- on the success return and on both budget-exhausted returns: emit
  `EventTranslate`.
- on the early `normalizeScope` / unknown-backend returns: emit
  `EventTranslate` with `OutcomeCallerError`, so *every* exit path emits
  exactly one — a caller counting events must never see a translation that
  started and never ended.

**`provider.go` / `OpenAIProvider.Complete`** — this is the only place that
sees latency, `usage`, and `finish_reason`. It times the round trip and emits
`EventModelCall`. The `finish_reason == "length"` branch that already exists
emits with `OutcomeTransport` before returning its error.

`provider_anthropic.go` gets the same treatment against its own response shape
(Anthropic reports `usage.input_tokens`/`output_tokens` and `stop_reason` —
different keys, same three facts).

### 3.4 The plumbing problem, and how it is solved

`Complete(ctx, system, user) (string, error)` returns a string. It cannot
return an event, and the interface is public — third parties implement it, so
**changing that signature is off the table.**

Two ways to get the observer from the `Engine` down into the provider:

**Option A — explicit field, wired at construction (RECOMMENDED).**
`OpenAIProvider` and `AnthropicProvider` each gain an `Observe Observer`
field. `New(c)` sets `Engine.Observe` on every provider it builds via
`ProvidersFrom`. For `NewWithProvider`, the engine checks for an optional
`interface{ SetObserver(Observer) }` and calls it if present.

- Plain, greppable, no hidden dependencies.
- Cost: a caller who builds a provider by hand *and* skips `SetObserver` gets
  attempt/translate events but no model-call events. Documented, and the
  degradation is graceful — you lose tokens and latency, not correctness.

**Option B — carry it on the context.** `Translate` stashes the observer in
`ctx`; providers read it back out. Flows through `ModelProvider` unchanged,
works with third-party providers and fallback chains for free, no construction
wiring at all.

- Strictly less code and no gap.
- But it is a hidden dependency passed invisibly through a public interface,
  which is exactly the kind of clever this codebase has deliberately avoided.

**Recommendation: Option A.** The wiring gap only affects hand-built providers,
which is the test/advanced path, and the code stays obvious. *This is the one
real decision in this document — flagging it rather than picking silently.*

### 3.5 Service wiring (`queryforge_service`)

- `main.go`: build the engine, then set `engine.Observe` to a closure over the
  existing `slog.Logger`.
- Add a **request id**: generated in `withLogging`, put on the request context,
  read by the observer closure, attached to every line as `rid`. This is what
  makes the model lines groupable with the request line.
- Level mapping, deliberately quiet by default:

| Event | Level | Fields |
|---|---|---|
| `EventModelCall` ok | `Info` | rid, provider, model, latency, prompt/completion/hidden/total tokens, finish_reason |
| `EventModelCall` failed | `Warn` | + outcome, err |
| `EventAttempt` ok | not logged | (the translate line covers it) |
| `EventAttempt` failed | `Warn` | rid, attempt, outcome, err |
| `EventAttempt` failed + raw | `Debug` | rid, attempt, raw (truncated, §5) |
| `EventTranslate` ok | `Info` | rid, backend, duration, repair_attempts, warnings, scope_keys |
| `EventTranslate` failed | `Error` for transport, `Warn` otherwise | + outcome, err |
| `EventTranslate` refusal | `Info` | a refusal is correct behaviour, not a fault |

- The three existing ad-hoc log calls in `writeEngineError` are **removed** —
  they become duplicates of `EventTranslate`.
- One new flag, `-log-level` (default `info`), so `debug` is what turns raw
  model output on. Off in production unless someone is actively debugging.

## 4. Files touched

| File | Change |
|---|---|
| `observe.go` **(new)** | `Observer`, `Event`, `EventKind`, `Outcome`, the nil-safe `emit` helper. |
| `observe_test.go` **(new)** | Table-driven + adversarial (§6). |
| `queryforge.go` | `Engine.Observe` field; emit calls on all exit paths of `Translate`; timing. |
| `provider.go` | `Observe` field + `SetObserver`; time the round trip; emit with usage + finish_reason. |
| `provider_anthropic.go` | Same, against Anthropic's usage/stop_reason shape. |
| `provider_fallback.go` | Propagate the observer to each chain member in `ProvidersFrom`. |
| `README.md` / `docs/` | An "Observability" section: the seam, the contract, the privacy rules. |
| svc `main.go` | Build the observer closure; `-log-level` flag. |
| svc `server.go` | Request id in `withLogging`; drop the three duplicate log calls. |
| svc `README.md` | Document the new log lines and what `debug` exposes. |

**Not touched:** `validate.go`, `gen_sql.go`, `gen_mongo.go`, `explain.go`,
`scope.go`, `ast.go`. The deterministic core stays free of observability
entirely — it is 0.5ms and it is correct by test, so there is nothing to watch.

## 5. Privacy rules (non-negotiable, and the reason this section exists)

The service's existing stance — *never log request bodies, because a question
can contain personal data* — is preserved. Therefore:

1. **The question text is never in an Event.** Not on any kind, not truncated,
   not hashed. If you want it, you already have it at the call site.
2. **Scope values are never in an Event.** `ScopeKeys` carries field *names*
   only. Scope values are tenant ids, user ids, enterprise ids — the most
   sensitive fields in the system.
3. **`Raw` is populated only on a failed attempt**, never on success. It can
   echo the question, so: the service logs it at `Debug` only, truncated to
   500 bytes via the existing `snippet` helper.
4. **The API key never appears in an Event.** `Provider`/`Model` are ids.

## 6. Test plan (per the standing QA rule: happy path *and* adversarial, before moving on)

Happy path, table-driven, all against `StubProvider` — **no test in this suite
makes a network call**:

- one event of each kind, in order, for a clean translation.
- `Attempt` numbering matches `TranslateResult.RepairAttempts`.
- a validation-repair run emits N+1 attempt events with the right outcomes.
- token arithmetic: `HiddenTokens == Total - Prompt - Completion`, including
  when the provider reports no usage block at all (all zeros, no negative).
- outcome classification: one case per `Outcome` constant, each asserting the
  right one is produced.

Adversarial — the ones that actually matter:

- **nil Observer** on every path (this is the default, so it is the most-run
  configuration in the world): no panic, no allocation.
- **panicking Observer**: decide and pin the contract. Proposal: **do not
  recover.** A panicking Observer is a caller bug and hiding it produces a
  system that silently stops reporting. Test asserts the panic propagates.
- **concurrent Translate on one Engine** under `-race`: events from two
  goroutines interleave without a data race (the existing suite already runs
  with `-race`).
- **Observer that blocks** — assert the call is synchronous, so the contract in
  the doc comment is real and not aspirational.
- **every exit path emits exactly one `EventTranslate`** — including unknown
  backend, bad scope, refusal, budget exhausted. A counting test over all six.
- **no leakage**: a translation whose question and scope contain a canary
  string produces no event, on any kind, containing that canary — except `Raw`
  on a failed attempt. This is the test that enforces §5 rather than trusting it.
- **`finish_reason: length`** still returns the existing helpful error *and*
  emits an event with the hidden-token count (the BUG-008 regression guard).

Service-side: an httptest run asserting one request id ties the request line to
the model line, and that `Raw` never appears at `info`.

Any defect found during this QA goes in `bugs.csv` with the usual columns.

## 7. Non-goals

- **No OpenTelemetry dependency.** The library is standard-library-only and
  stays that way. An OTel bridge is ~20 lines *in the caller*, on top of this
  seam — that is the point of the seam.
- **No metrics/counters.** Events are facts; aggregation is the caller's job.
- **No logging in the deterministic core.** See §4.
- **No sampling, no buffering, no async.** Caller's choice, one closure away.
- **Not a replacement for `TranslateResult`.** The result stays the API for
  callers who just want the answer; events are for the operator.

## 8. Open questions for you

1. **Option A vs Option B in §3.4** — explicit provider field (recommended) or
   context-carried observer?
2. **Panicking Observer: propagate (recommended) or recover?** Propagating is
   honest; recovering means a broken sink can never take down a translation.
3. **Should `EventTranslate` carry the compiled query string** (not the args)?
   It is enormously useful in a log and it contains no caller data *except* via
   literals the user typed — `WHERE name = 'Aman'`. My instinct is **no by
   default**, available at `Debug` alongside `Raw`.
4. **Scope: is `ScopeKeys` worth having at all**, or is even the set of scope
   *field names* something you would rather not emit?
