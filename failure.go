package queryforge

import (
	"context"
	"errors"
)

// ─────────────────────────────────────────────────────────────────────────────
// The failure-code vocabulary.
//
// QueryForge is reachable through four surfaces — the Go library, the engine
// binary, the Python SDK and the Java SDK — and each of them has to tell a
// caller WHY something failed. Before this file, three of the four agreed by
// coincidence: cmd/queryforge classified errors into SCREAMING_SNAKE protocol
// codes, and the two SDKs mapped those codes onto exception classes, but the Go
// library itself offered no way to ask "what class of failure is this?" without
// re-implementing the same chain of errors.Is/errors.As checks.
//
// Re-implementing it is exactly what happened, and it is the kind of duplication
// that rots silently: a new sentinel added to the library would be classified by
// the binary and not by anyone else, and nothing would fail — the caller would
// just get INTERNAL for something that had a perfectly good name.
//
// So the classification lives here, once, and every surface derives from it.
// ─────────────────────────────────────────────────────────────────────────────

// FailureCode is a stable, machine-readable classification of a failed
// operation. It answers "what kind of failure was this, and who should act on
// it?" — which is a different and coarser question than the one ErrCode answers
// (ErrCode names the individual rule an AST broke).
//
// The two are deliberately spelled differently. FailureCode is SCREAMING_SNAKE
// because it crosses process and language boundaries: it is what the engine
// binary puts on the wire, what QueryForgeError.code carries in Python, and what
// QueryForgeException.getCode() returns in Java. ErrCode is lowercase and stays
// inside a validation finding.
//
// Values are part of the public contract. A code may be added; an existing one
// will not change meaning without a major version.
type FailureCode string

const (
	// FailureNone is the zero value, reported for a nil error.
	FailureNone FailureCode = ""

	// FailureInvalidRequest — the request itself was unusable: a missing
	// argument, an empty question, a malformed call. The caller's bug; no retry
	// will help.
	FailureInvalidRequest FailureCode = "INVALID_REQUEST"

	// FailureInvalidConfig — the config did not parse, or broke one of the
	// engine's structural rules. Fails at load time, before any work is done.
	FailureInvalidConfig FailureCode = "INVALID_CONFIG"

	// FailureUnknownBackend — no generator is registered for the requested
	// backend id.
	FailureUnknownBackend FailureCode = "UNKNOWN_BACKEND"

	// FailureInvalidScope — a caller-supplied scope filter was rejected. Scope
	// comes from the application (tenant, subscription, user), never from the
	// end user's question, so this is always an application bug and should page
	// whoever wired up the tenancy filters.
	FailureInvalidScope FailureCode = "INVALID_SCOPE"

	// FailureValidation — an AST broke a rule this config declares. On the
	// deterministic path that is the AST the caller passed; on a translation it
	// means the model could not produce a conforming AST within the repair
	// budget, which is usually a config gap rather than a model problem.
	FailureValidation FailureCode = "VALIDATION_FAILED"

	// FailureUnsupported — the model deliberately refused: the question cannot
	// be expressed in the vocabulary this config registers. A well-formed answer
	// rather than a fault, and the one code whose message is written to be shown
	// to the person who asked.
	FailureUnsupported FailureCode = "UNSUPPORTED_REQUEST"

	// FailureModelOutput — the model answered, but never with usable JSON.
	// Usually transient; retrying, or switching models, is reasonable.
	FailureModelOutput FailureCode = "MODEL_OUTPUT"

	// FailureModelTransport — the model was never reached: network failure, a
	// missing or rejected API key, a rate limit.
	FailureModelTransport FailureCode = "MODEL_TRANSPORT"

	// FailureGenerate — a valid AST could not be compiled to the target backend.
	FailureGenerate FailureCode = "GENERATE_FAILED"

	// FailureTimeout — the operation exceeded its deadline.
	FailureTimeout FailureCode = "TIMEOUT"

	// FailureInternal — the catch-all. Reaching it means QueryForge produced an
	// error it does not have a name for, which is a bug worth reporting.
	FailureInternal FailureCode = "INTERNAL"
)

// Classify maps an error returned by this library onto its FailureCode.
//
// The ORDER OF THE CHECKS IS LOAD-BEARING and mirrors the reasoning a person
// would apply:
//
//  1. A deadline first. The engine surfaces a cancelled model call as a
//     transport failure — technically true, but "the model was unreachable"
//     sends someone to check their API key when the real fix is a longer
//     timeout.
//  2. Scope before validation. A bad scope is the calling application's bug,
//     not the end user's question, and the two want completely different
//     handling.
//  3. Refusal before validation. A refusal is a deliberate answer.
//  4. Validation before the model sentinels, because the budget-exhausted
//     wrapper carries the last underlying failure and both would otherwise
//     match.
//
// A nil error returns FailureNone. An error this library did not produce
// returns FailureInternal rather than a guess: inventing a friendlier code for
// something unrecognised is how a real bug gets filed as a user error.
func Classify(err error) FailureCode {
	if err == nil {
		return FailureNone
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return FailureTimeout
	}
	if errors.Is(err, ErrScope) {
		return FailureInvalidScope
	}

	var unsupported *UnsupportedRequestError
	if errors.As(err, &unsupported) {
		return FailureUnsupported
	}

	// Validation findings survive the budget-exhausted wrapper, so this catches
	// both a caller's bad AST and a model that never produced a conforming one.
	var verrs ValidationErrors
	if errors.As(err, &verrs) {
		return FailureValidation
	}

	if errors.Is(err, ErrModelOutput) {
		return FailureModelOutput
	}
	if errors.Is(err, ErrModelTransport) {
		return FailureModelTransport
	}

	return FailureInternal
}

// Retryable reports whether retrying the identical operation could plausibly
// succeed. It exists so a caller does not have to keep its own copy of "which
// of these codes are transient", which is the kind of table that drifts out of
// date the moment a code is added.
//
// It is deliberately conservative. Only two codes say yes: a model that emitted
// unusable JSON (models do this intermittently) and a model that could not be
// reached (networks recover, rate limits expire). Everything else — a bad
// config, a bad scope, a refusal, a validation failure — will fail identically
// on every attempt, and retrying it just spends money to reach the same answer.
func (c FailureCode) Retryable() bool {
	return c == FailureModelOutput || c == FailureModelTransport
}
