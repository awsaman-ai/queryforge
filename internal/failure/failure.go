// Package failure is the library's error taxonomy: the FailureCode vocabulary
// every surface reports, the model-call sentinels the planner and the providers
// both raise, and the finer ProviderErrorKind axis the retry loop acts on.
//
// It sits ABOVE internal/validate and internal/policy because Classify matches
// against their error types, and BELOW internal/provider and internal/planner
// because both of those raise the sentinels declared here. That ordering is what
// keeps the graph acyclic: the sentinels used to live in planner.go, where
// provider_error.go could not reach them without planner importing provider and
// provider importing planner.
package failure

import (
	"context"
	"errors"

	"github.com/awsaman-ai/queryforge/internal/policy"
	"github.com/awsaman-ai/queryforge/internal/scope"
	"github.com/awsaman-ai/queryforge/internal/validate"
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

	// FailurePolicy — the AST was structurally legal but broke a business rule
	// declared in Policy.Requires (e.g. passport expiry filtered without a
	// country). Distinct from FailureValidation for the same reason
	// FailureUnsupported is: the question COULD be expressed and was, but the
	// result would not make business sense on its own. See PolicyViolationError.
	FailurePolicy FailureCode = "POLICY_VIOLATION"

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
	if errors.Is(err, scope.ErrScope) {
		return FailureInvalidScope
	}

	var unsupported *UnsupportedRequestError
	if errors.As(err, &unsupported) {
		return FailureUnsupported
	}

	// A policy violation is checked before ordinary validation for the same
	// reason a refusal is: it is a deliberate, typed answer with its own
	// meaning, not a generic finding to lump in with FailureValidation.
	var violation *policy.PolicyViolationError
	if errors.As(err, &violation) {
		return FailurePolicy
	}

	// Validation findings survive the budget-exhausted wrapper, so this catches
	// both a caller's bad AST and a model that never produced a conforming one.
	var verrs validate.ValidationErrors
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

// Sentinel errors that classify a failed Plan call. The engine's repair loop
// keys off these: a reply we received but could not read is worth asking again
// (models occasionally emit truncated or double-brace JSON), whereas never
// reaching the model at all will not fix itself on a retry.
var (
	// ErrModelTransport means the model was not reached or refused the call —
	// network failure, auth rejection, retired model, quota block. Not repairable.
	ErrModelTransport = errors.New("model transport failure")

	// ErrModelOutput means the model answered but the reply could not be parsed
	// into an AST. Repairable: resend with a hint describing what was wrong.
	ErrModelOutput = errors.New("model output not parseable")
)

// UnsupportedRequestError reports that the request cannot be expressed with the
// vocabulary this config exposes — the user asked about something the config
// does not describe. It is a definitive answer, not a failure to retry: the
// model declined on purpose rather than substituting a field that does exist.
// Returning it is strictly safer than compiling a query against a guessed field,
// which would hand back confident, silently wrong rows.
type UnsupportedRequestError struct {
	Reason string // the model's short explanation, e.g. "no field for shipping warehouse"
}

// Error renders the refusal.
func (e *UnsupportedRequestError) Error() string {
	if e.Reason == "" {
		return "request is not supported by this config"
	}
	return "request is not supported by this config: " + e.Reason
}

// ProviderErrorKind classifies why a model call failed, at a granularity the
// retry loop can act on.
//
// The library already classifies failures for the CALLER via FailureCode
// (MODEL_TRANSPORT vs MODEL_OUTPUT vs …), and that taxonomy is a wire contract
// shared with the Java and Python SDKs — adding members to it would break older
// SDKs that map codes to exception classes. This kind is deliberately a
// SEPARATE, Go-only axis: it answers "should we try again?", not "what should
// the caller be told?". A ProviderError still reaches the planner as an ordinary
// error and is still wrapped into ErrModelTransport, so FailureCode is unchanged.
type ProviderErrorKind string

const (
	// KindAuth means the endpoint rejected the credential: missing, malformed,
	// revoked, or not entitled to this model. Retrying sends the same bad key.
	KindAuth ProviderErrorKind = "AUTH"

	// KindQuota means the account is out of credit or past a hard billing cap.
	// Distinct from KindRateLimit because waiting does not help — only a
	// different provider (or a different wallet) does. With a fallback chain
	// configured this should hand over immediately rather than sleep first.
	KindQuota ProviderErrorKind = "QUOTA"

	// KindRateLimit means too many requests in the window. This is the one
	// failure where backing off and trying again genuinely fixes the problem,
	// and it is the failure free-tier keys hit most.
	KindRateLimit ProviderErrorKind = "RATE_LIMIT"

	// KindModelNotFound means the endpoint does not serve the configured model
	// id — a typo, or a model that has been retired. Retrying cannot conjure it.
	KindModelNotFound ProviderErrorKind = "MODEL_NOT_FOUND"

	// KindInvalidRequest means the endpoint rejected the request body itself.
	// The body is deterministic for a given config, so a retry reproduces it
	// byte for byte.
	KindInvalidRequest ProviderErrorKind = "INVALID_REQUEST"

	// KindUnavailable means the provider failed on its own side (5xx) or is
	// overloaded. Transient by definition, so worth another attempt.
	KindUnavailable ProviderErrorKind = "UNAVAILABLE"

	// KindTimeout means the request exceeded its deadline. Retryable only when
	// the budget that expired was the PER-ATTEMPT one; see Retryable.
	KindTimeout ProviderErrorKind = "TIMEOUT"

	// KindTransport means the request never completed at the network level —
	// DNS failure, refused connection, TLS error, dropped socket.
	KindTransport ProviderErrorKind = "TRANSPORT"

	// KindBadResponse means the endpoint answered 2xx but the payload was not
	// usable: undecodable JSON, no choices, or a reply truncated by the token
	// budget. Not retried HERE — a truncated reply truncates again, and genuine
	// model-output problems belong to the engine's repair loop, which can
	// change the prompt. Retrying at this layer would silently multiply the
	// repair budget.
	KindBadResponse ProviderErrorKind = "BAD_RESPONSE"
)

// Retryable reports whether another identical attempt could plausibly succeed.
//
// The rule is deliberately narrow: retry only failures whose cause is TIME
// (the provider was busy, throttling, or briefly broken). Everything caused by
// the request itself — a bad key, an unknown model, a malformed body, an empty
// wallet — reproduces exactly on the next attempt, so retrying it only delays
// the error the caller needs to see, and on a rate-limited key it spends
// quota to learn nothing.
func (k ProviderErrorKind) Retryable() bool {
	switch k {
	case KindRateLimit, KindUnavailable, KindTimeout, KindTransport:
		return true
	default:
		return false
	}
}
