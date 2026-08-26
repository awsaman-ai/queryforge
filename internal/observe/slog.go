package observe

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/awsaman-ai/queryforge/internal/failure"
)

// ─────────────────────────────────────────────────────────────────────────────
// The structured-logging adapter.
//
// Read observe.go first: the library itself still never writes a log line, never
// formats a message and never picks a severity. Nothing in this file changes
// that. What it adds is the one adapter almost every caller was going to write
// by hand — Event in, slog record out — so that the FIELD NAMES, the LEVELS and
// the ERROR CODES are identical whether you are reading Go logs, Python logs or
// Java logs, instead of being three people's independent guesses.
//
// It is opt-in and inert until called:
//
//	engine.SetObserver(qf.SlogObserver(slog.Default()))
//
// Use SetObserver rather than assigning Engine.Observe, or the model-call
// events — latency, tokens, finish reason, i.e. the only expensive facts in the
// system — never reach the logger. See Engine.SetObserver.
//
// WHY THIS IS NOT A LOGGER INSIDE THE LIBRARY. A library that logs on its own
// initiative fights its host: it picks a destination the host did not choose, a
// format the host's aggregator may not parse, and a severity the host does not
// agree with. Handing back an Observer inverts all three — the host supplies the
// *slog.Logger, so the handler, the level threshold, the output and any
// pre-attached attributes are entirely theirs. The default remains total
// silence.
// ─────────────────────────────────────────────────────────────────────────────

// Log attribute keys. They are constants rather than string literals at the
// emission sites because they are a cross-language contract, not an
// implementation detail: the Python and Java SDKs emit the same names, and a
// dashboard filtering on error_code has to work regardless of which SDK
// produced the line. Changing one of these is a breaking change to anyone's
// saved queries, so they are spelled out here where that is obvious.
const (
	LogKeyLibrary   = "library"   // always "queryforge"; lets a shared index separate our lines
	LogKeyLanguage  = "language"  // "go" here; "python"/"java" in the SDKs
	LogKeyComponent = "component" // which part of the system spoke
	LogKeyOperation = "operation" // the unit of work: translate, generate, model_call, ...
	LogKeyOutcome   = "outcome"   // how it ended; the Outcome vocabulary from observe.go
	LogKeyBackend   = "backend"   // target backend id: sql | mysql | mongo
	LogKeyEntity    = "entity"    // the config's entity, e.g. "Order"
	LogKeyAttempt   = "attempt"   // 0-based repair attempt
	LogKeyDuration  = "duration_ms"
	LogKeyErrorCode = "error_code" // FailureCode; the same string the SDKs put on an exception
	LogKeyErrorType = "error_type" // Go type of the error, for narrowing a search
	LogKeyError     = "error"      // the error's message

	LogKeyProvider     = "provider"
	LogKeyModel        = "model"
	LogKeyPromptTokens = "prompt_tokens"
	LogKeyOutputTokens = "completion_tokens"
	LogKeyHiddenTokens = "hidden_tokens"
	LogKeyTotalTokens  = "total_tokens"
	LogKeyFinishReason = "finish_reason"

	LogKeyRepairAttempts = "repair_attempts"
	LogKeyWarnings       = "warnings"
	LogKeyScopeKeys      = "scope_keys"
	LogKeyRaw            = "raw"
)

// LogLibraryName and LogLanguageName are stamped on every record so lines from
// the Go library, the Python SDK and the Java SDK land in one searchable stream
// and can still be told apart.
const (
	LogLibraryName  = "queryforge"
	LogLanguageName = "go"
)

// Component names. Kept to the smallest set that is actually useful to filter
// on: "engine" is the deterministic pipeline plus the repair loop, "provider" is
// the model round trip. A finer breakdown would be noise — observe.go explains
// why the deterministic half is not instrumented at all (it is 0.03% of wall
// time).
const (
	LogComponentEngine   = "engine"
	LogComponentProvider = "provider"
)

// SlogObserver returns an Observer that writes each Event to l as one structured
// log record, using the cross-language field names documented in
// docs/OBSERVABILITY.md.
//
// LEVELS. The mapping is the point of this function, and it is not the obvious
// "error means ERROR":
//
//   - DEBUG — a step that succeeded. A model call that answered, an attempt that
//     produced a valid AST. Useful when tracing, noise otherwise.
//   - INFO  — the lifecycle facts a running system wants: a translation finished,
//     and a translation was REFUSED. A refusal is logged at INFO deliberately.
//     It means the guard rails worked: the question could not be expressed in
//     the registered vocabulary and no query was invented. Logging that at ERROR
//     trains people to ignore the error stream.
//   - WARN  — a step failed but the operation continues. One model call in a
//     fallback chain, one attempt inside the repair budget. These cost money and
//     latency and are worth watching, but they are not yet failures.
//   - ERROR — the caller is receiving an error. Exactly one of these per failed
//     Translate, at the boundary, which is the only place the whole story is
//     known.
//
// That last rule is what keeps the stream readable: a translation that burns
// three attempts and fails produces three WARN lines and ONE ERROR, not four
// stack traces of the same underlying problem.
//
// PRIVACY. Everything Event guarantees, this inherits — the question text is
// never present, scope VALUES are never present (only the field names), the API
// key is never present. The one field that can echo caller data is Event.Raw,
// already bounded to Engine.MaxRawLength by the library, and it is attached only
// at DEBUG so it cannot reach a production log without someone turning debug on.
//
// A nil logger returns a nil Observer, which the emit path treats as "nobody is
// listening" — so SlogObserver(nil) disables logging rather than panicking on
// the first event.
func SlogObserver(l *slog.Logger) Observer {
	if l == nil {
		return nil
	}
	return func(ctx context.Context, e Event) {
		level, msg := LogLevelFor(e)

		attrs := []slog.Attr{
			slog.String(LogKeyLibrary, LogLibraryName),
			slog.String(LogKeyLanguage, LogLanguageName),
			slog.String(LogKeyComponent, logComponentFor(e.Kind)),
			slog.String(LogKeyOperation, string(e.Kind)),
			slog.Int(LogKeyAttempt, e.Attempt),
		}
		if e.Entity != "" {
			attrs = append(attrs, slog.String(LogKeyEntity, e.Entity))
		}
		if e.Backend != "" {
			attrs = append(attrs, slog.String(LogKeyBackend, e.Backend))
		}
		if e.Outcome != "" {
			attrs = append(attrs, slog.String(LogKeyOutcome, string(e.Outcome)))
		}

		switch e.Kind {
		case EventModelCall:
			attrs = append(attrs, modelCallAttrs(e)...)
		case EventAttempt:
			// Raw is the model's verbatim reply and the single most useful field
			// for diagnosing a parse failure — and the only one that can echo
			// caller data. DEBUG only, and only when the handler is actually
			// going to keep it.
			if e.Raw != "" && l.Enabled(ctx, slog.LevelDebug) {
				attrs = append(attrs, slog.String(LogKeyRaw, e.Raw))
			}
		case EventTranslate:
			attrs = append(attrs, translateAttrs(e)...)
		}

		if e.Err != nil {
			attrs = append(attrs,
				slog.String(LogKeyErrorCode, string(failure.Classify(e.Err))),
				slog.String(LogKeyErrorType, errorTypeName(e.Err)),
				slog.String(LogKeyError, e.Err.Error()),
			)
		}

		l.LogAttrs(ctx, level, msg, attrs...)
	}
}

// modelCallAttrs adds the facts only a provider sees. Token counts are omitted
// when zero rather than logged as 0: a provider that reports no usage block and
// a provider that genuinely used no tokens are different situations, and a
// column of zeroes hides the first one.
func modelCallAttrs(e Event) []slog.Attr {
	attrs := []slog.Attr{slog.Int64(LogKeyDuration, e.Latency.Milliseconds())}
	if e.Provider != "" {
		attrs = append(attrs, slog.String(LogKeyProvider, e.Provider))
	}
	if e.Model != "" {
		attrs = append(attrs, slog.String(LogKeyModel, e.Model))
	}
	if e.PromptTokens > 0 {
		attrs = append(attrs, slog.Int(LogKeyPromptTokens, e.PromptTokens))
	}
	if e.CompletionTokens > 0 {
		attrs = append(attrs, slog.Int(LogKeyOutputTokens, e.CompletionTokens))
	}
	if e.HiddenTokens > 0 {
		// The reasoning-token bill. Worth its own field because it is invisible
		// in the visible answer and has measured ~5x the completion count.
		attrs = append(attrs, slog.Int(LogKeyHiddenTokens, e.HiddenTokens))
	}
	if e.TotalTokens > 0 {
		attrs = append(attrs, slog.Int(LogKeyTotalTokens, e.TotalTokens))
	}
	if e.FinishReason != "" {
		attrs = append(attrs, slog.String(LogKeyFinishReason, e.FinishReason))
	}
	return attrs
}

// translateAttrs adds the whole-call summary.
func translateAttrs(e Event) []slog.Attr {
	attrs := []slog.Attr{
		slog.Int64(LogKeyDuration, e.Duration.Milliseconds()),
		slog.Int(LogKeyRepairAttempts, e.RepairAttempts),
	}
	if len(e.Warnings) > 0 {
		attrs = append(attrs, slog.Any(LogKeyWarnings, e.Warnings))
	}
	if len(e.ScopeKeys) > 0 {
		// NAMES only. See the privacy note on Event: the values are tenant, user
		// and enterprise ids, and this is the field most likely to be exported
		// to a shared log index.
		attrs = append(attrs, slog.Any(LogKeyScopeKeys, e.ScopeKeys))
	}
	return attrs
}

// logComponentFor names the part of the system an event came from.
func logComponentFor(k EventKind) string {
	if k == EventModelCall {
		return LogComponentProvider
	}
	return LogComponentEngine
}

// LogLevelFor chooses the severity and the message for an event.
//
// The messages are fixed strings with no interpolation, which is what makes them
// groupable: an aggregator can count "attempt rejected: validation failed"
// without the count fragmenting across every distinct field name that appeared
// in a message. Everything variable is an attribute.
func LogLevelFor(e Event) (slog.Level, string) {
	switch e.Kind {
	case EventModelCall:
		if e.Outcome == OutcomeOK {
			return slog.LevelDebug, "model call completed"
		}
		// WARN, not ERROR: a single failed call may still be recovered by the
		// next provider in a fallback chain or the next repair attempt. If it
		// cannot be, the translate event says so at ERROR.
		return slog.LevelWarn, "model call failed"

	case EventAttempt:
		switch e.Outcome {
		case OutcomeOK:
			return slog.LevelDebug, "attempt produced a valid AST"
		case OutcomeRefusal:
			// The model declined to invent a query for something the config
			// cannot express. That is the system working.
			return slog.LevelInfo, "model refused the request"
		case OutcomePolicy:
			// The AST was legal but broke a declared business rule. Also the
			// system working, not a fault — same INFO treatment as a refusal.
			return slog.LevelInfo, "attempt rejected: policy violation"
		case OutcomeParseError:
			return slog.LevelWarn, "attempt rejected: model output not parseable"
		case OutcomeValidation:
			return slog.LevelWarn, "attempt rejected: validation failed"
		default:
			return slog.LevelWarn, "attempt failed"
		}

	case EventTranslate:
		switch e.Outcome {
		case OutcomeOK:
			return slog.LevelInfo, "query generation completed"
		case OutcomeRefusal:
			return slog.LevelInfo, "query generation refused"
		case OutcomePolicy:
			return slog.LevelInfo, "query generation rejected by policy"
		default:
			return slog.LevelError, "query generation failed"
		}
	}

	// An event kind added later, before this switch learns about it. Report it
	// rather than dropping it: a silently discarded event is precisely the
	// failure this whole file exists to prevent.
	return slog.LevelInfo, "queryforge event"
}

// errorTypeName renders an error's concrete Go type for the error_type field.
//
// It reports the OUTERMOST type — the wrapper, not the cause — because that is
// what the caller actually received and what a `case *X:` in their code will
// match. The cause is still reachable: error_code is produced by Classify,
// which unwraps all the way down.
func errorTypeName(err error) string {
	if err == nil {
		return ""
	}
	return fmt.Sprintf("%T", err)
}
