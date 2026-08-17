package queryforge

import (
	"context"
	"fmt"
	"time"
)

// ─────────────────────────────────────────────────────────────────────────────
// Observability seam.
//
// QueryForge itself never writes a log line, never formats a message, and never
// picks a severity — a library that logs is a library that fights its host. It
// reports facts through one optional callback and lets the caller decide.
//
// The scope of what is reported is deliberately narrow. A traced live request
// (2026-08-06) measured the entire deterministic half — parse, validate,
// compile, explain — at 0.5ms against a 1,781ms model call: 99.97% of wall time
// is one HTTP round trip. So this seam watches the model interaction, which is
// simultaneously the only slow thing, the only expensive thing, and the only
// non-deterministic thing in the system. The deterministic core is left alone.
// ─────────────────────────────────────────────────────────────────────────────

// Observer receives one Event per notable step of a translation, along with the
// context the work is running under.
//
// The context is what makes correlation possible: a caller that stamps a
// request id (or a trace span) onto the context it passes to Translate can read
// it back here and tie every event to the request that caused it. The library
// itself puts nothing in the context and reads nothing from it — it is passed
// through untouched, purely so the Observer can.
//
// nil (the default) disables everything, at the cost of one nil check per event.
//
// Contract for implementers:
//
//   - It MUST be safe for concurrent use. One Engine may serve many goroutines,
//     and events from different translations interleave freely.
//   - It MUST NOT panic. A panicking Observer is a bug in the calling code and
//     is deliberately NOT recovered — swallowing it would produce a system that
//     silently stops reporting, which is worse than a loud crash.
//   - It SHOULD return quickly. It is called synchronously on the hot path, so
//     a slow Observer directly slows the translation. Callers who want async
//     delivery can make the Observer a one-line channel send.
type Observer func(ctx context.Context, e Event)

// EventKind names what happened. The three kinds nest: one EventTranslate
// covers 1..MaxRepairs+1 EventAttempts, and each attempt covers exactly one
// EventModelCall (or zero, when the provider failed before sending anything).
type EventKind string

const (
	// EventModelCall reports that one round trip to a provider finished, success
	// or not. It is the only kind carrying latency and token counts, because the
	// provider is the only component that sees them.
	EventModelCall EventKind = "model_call"

	// EventAttempt reports that one plan+parse+validate cycle resolved. It is
	// emitted once per iteration of the repair loop, including the failures —
	// which is the point, since a failed attempt costs a full model call.
	EventAttempt EventKind = "attempt"

	// EventTranslate reports that the whole Translate call finished. Exactly one
	// is emitted per Translate, on every exit path, so a caller counting events
	// never sees a translation that started and never ended.
	EventTranslate EventKind = "translate"
)

// Outcome classifies how a step ended. It exists so callers can branch on a
// value instead of string-matching an error message.
type Outcome string

const (
	OutcomeOK          Outcome = "ok"               // succeeded
	OutcomeParseError  Outcome = "parse_error"      // ErrModelOutput; repairable
	OutcomeValidation  Outcome = "validation_error" // broke a config rule; repairable
	OutcomeRefusal     Outcome = "refusal"          // *UnsupportedRequestError; a real answer, not a fault
	OutcomeTransport   Outcome = "transport_error"  // ErrModelTransport; not repairable
	OutcomeBudgetSpent Outcome = "budget_exhausted" // every attempt used, still no valid AST
	OutcomeCallerError Outcome = "caller_error"     // bad scope or unknown backend; decided before any model call
	OutcomeGenerate    Outcome = "generate_error"   // valid AST, but the backend generator failed
)

// Event is one observation. It is a single flat struct because it is going to be
// flattened into structured log attributes anyway; which fields are populated
// depends on Kind, and everything else is left at its zero value.
//
// PRIVACY — this struct is designed so that routing it straight to a log is
// safe by default. The question text is NEVER present, on any kind. Scope
// VALUES are never present (they are tenant/user/enterprise ids — the most
// sensitive fields in the system); only ScopeKeys, the field names. The API key
// is never present. The one field that can contain caller data is Raw, which is
// populated only on a FAILED attempt — see its own comment.
type Event struct {
	Kind    EventKind // which of the three kinds; always set
	Attempt int       // 0-based repair attempt this event belongs to; always set
	Entity  string    // the config's entity, e.g. "Order"; always set
	Backend string    // the requested backend id, e.g. "sql"; always set

	// ── EventModelCall only ──────────────────────────────────────────────────

	Provider         string        // provider id that was called, e.g. "gemini"
	Model            string        // model id, e.g. "gemini-3.1-flash-lite"
	Latency          time.Duration // wall time of the round trip
	PromptTokens     int           // from the provider's usage block; 0 when absent
	CompletionTokens int           // visible output tokens
	HiddenTokens     int           // total - prompt - completion: the reasoning-token bill
	TotalTokens      int           // as reported by the provider
	FinishReason     string        // "stop", "length", ... — the tell that caught BUG-008

	// Retry is which transport attempt this round trip was: 0 for the first,
	// 1+ for retries after a retryable failure. One EventModelCall is emitted
	// per round trip, so a throttled call that eventually succeeds appears as
	// several events with rising Retry — which is the only way to see that a
	// translation was slow because the provider was rate-limiting rather than
	// because the model was thinking.
	Retry int

	// ErrorKind classifies a failed model call (AUTH, RATE_LIMIT, QUOTA, …).
	// Empty when the call succeeded, and empty for a third-party ModelProvider
	// whose errors this library cannot classify.
	//
	// Separate from the FailureCode reported to callers: that one is a wire
	// contract shared with the SDKs, this one is a diagnostic axis free to gain
	// members without breaking anything.
	ErrorKind ProviderErrorKind

	// ── EventAttempt and EventTranslate ──────────────────────────────────────

	Outcome Outcome // how the step ended; always set on these two kinds
	Err     error   // non-nil whenever Outcome is not OutcomeOK

	// Raw is the model's reply verbatim, populated ONLY on a failed EventAttempt
	// (never on success, never on the other kinds). It is the single most useful
	// debugging field here — without it a parse failure tells you THAT the reply
	// was unusable but never WHAT it said — and it is also the only field that
	// can echo caller data. Treat it as sensitive: log it at debug level.
	//
	// It is truncated before emission to Engine.MaxRawLength (4 KB by default),
	// with an "… (N bytes truncated)" marker when anything was cut. That bound is
	// the library's, not the caller's: the advice above used to be the only
	// protection, and advice in a doc comment is not a control. An Observer that
	// ships straight to a log aggregator is the expected configuration, and a
	// model that answers a parse failure with 2 MB of repeated text should not be
	// able to put 2 MB of caller-derived text into that log through the default
	// path. Callers who want the untruncated reply still get it, on the success
	// path, as TranslateResult.Raw.
	Raw string

	// ── EventTranslate only ──────────────────────────────────────────────────

	Duration       time.Duration // wall time of the whole Translate call
	RepairAttempts int           // final repair count; matches TranslateResult.RepairAttempts
	Warnings       []string      // generator advisories (e.g. non-indexed filter)
	ScopeKeys      []string      // scope field NAMES only — never their values
}

// emit delivers an event when an Observer is set. Every emission site goes
// through this, so the nil-Observer path — the default, and therefore the
// most-executed configuration — is one comparison and nothing else.
func (o Observer) emit(ctx context.Context, e Event) {
	if o == nil {
		return
	}
	o(ctx, e)
}

// defaultMaxRawLength bounds Event.Raw when Engine.MaxRawLength is left at zero.
//
// 4 KB is comfortably more than any well-formed AST for a realistic config, so a
// reply that is merely wrong arrives whole and stays debuggable; only a reply
// that is pathological gets cut, which is exactly the one no log needs in full.
const defaultMaxRawLength = 4096

// truncateRaw bounds a raw model reply for emission on an Event.
//
// A negative max disables the bound (the caller has taken responsibility);
// zero selects the default. The marker states the byte count that was dropped,
// so a reader of the log can tell "the model said this" from "the model said
// this and 900 KB more" — a silent truncation would make a 4 KB prefix look
// like the complete reply and send someone hunting for a parse bug in text that
// was never the whole story.
//
// Cutting is by byte, and the cut can land inside a multi-byte rune. That is
// accepted: this is a debugging string, not a value, and the alternative — rune
// alignment — would still leave a truncated JSON document, which is what the
// marker is there to announce.
func truncateRaw(raw string, max int) string {
	if max == 0 {
		max = defaultMaxRawLength
	}
	if max < 0 || len(raw) <= max {
		return raw
	}
	return fmt.Sprintf("%s… (%d bytes truncated)", raw[:max], len(raw)-max)
}

// hiddenTokens derives the reasoning-token count a provider does not report
// directly. Reasoning models bill hidden thinking tokens inside total but list
// neither in prompt nor completion, so the difference is the only way to see
// them — and on some measurements they have been ~5x the visible answer.
//
// It clamps at zero: a provider that reports a total smaller than its parts (or
// omits usage entirely) must never produce a negative count in a log.
func hiddenTokens(total, prompt, completion int) int {
	h := total - prompt - completion
	if h < 0 {
		return 0
	}
	return h
}

// scopeKeys extracts the field names from normalized scope filters, for the
// audit trail. Values are deliberately dropped — see the privacy note on Event.
func scopeKeys(filters []ScopeFilter) []string {
	if len(filters) == 0 {
		return nil
	}
	keys := make([]string, 0, len(filters))
	for _, f := range filters {
		keys = append(keys, f.Field)
	}
	return keys
}

// observerSetter is implemented by providers that can report model-call events.
// The engine uses it to push its Observer down into a caller-supplied provider,
// since ModelProvider itself is a public one-method interface that third
// parties implement and must not grow a second method.
type observerSetter interface {
	SetObserver(Observer)
}
