// Package qferr defines the closed set of error codes the v2 experiment may
// return, plus the error type that carries one.
//
// This is a Phase 0 "contract" deliverable rather than plumbing. The research
// question is partly about whether a non-LLM pipeline can FAIL predictably —
// section 29 of the brief counts "ambiguity is hidden" and "values are blindly
// guessed" as experiment failures. A fixed taxonomy declared before any semantic
// code exists makes that measurable: every later phase must classify its refusals
// into these codes, and Phase 10's failure analysis counts them.
package qferr

import (
	"encoding/json"
	"errors"
	"fmt"
)

// Code is a stable, machine-readable failure category. The values come from
// section 23 of the experiment brief and are deliberately coarse: they describe
// what the caller should do, not which function returned.
type Code string

const (
	// CodeConfigError covers a config that cannot be loaded or violates a
	// validation rule. The fix is to the config file, not the query.
	CodeConfigError Code = "CONFIG_ERROR"

	// CodeInvalidInput covers a malformed request: empty query, unsupported
	// backend, input past a complexity limit.
	CodeInvalidInput Code = "INVALID_INPUT"

	// CodeNoSemanticMatch means retrieval returned nothing plausible for a span.
	CodeNoSemanticMatch Code = "NO_SEMANTIC_MATCH"

	// CodeLowConfidence means the best candidate fell under the absolute score
	// threshold. Distinct from CodeAmbiguousMatch: here nothing scored well.
	CodeLowConfidence Code = "LOW_CONFIDENCE"

	// CodeAmbiguousMatch means two or more candidates scored well and too close
	// together to separate. Distinct from CodeLowConfidence: here several
	// interpretations scored well, so guessing would be a coin flip.
	CodeAmbiguousMatch Code = "AMBIGUOUS_MATCH"

	// CodeValueResolutionFailed means a literal could not be parsed or mapped to
	// a configured alias safely.
	CodeValueResolutionFailed Code = "VALUE_RESOLUTION_FAILED"

	// CodeUnsupportedQuery means the request is understood but outside the
	// declared Phase 3 scope (window functions, CTEs, subqueries, DML).
	CodeUnsupportedQuery Code = "UNSUPPORTED_QUERY"

	// CodeRelationshipNotFound means the schema graph has no configured join path
	// connecting the bound entities, or more than one equally plausible path.
	CodeRelationshipNotFound Code = "RELATIONSHIP_NOT_FOUND"

	// CodeSecurityRejected means a hard safety rule refused the request — a write
	// verb, an identifier not in the allowlist, an injection-shaped input.
	CodeSecurityRejected Code = "SECURITY_REJECTED"

	// CodeCompilationError means a validated plan could not be emitted as SQL.
	// This is an engine bug, not a user error: the planner should not have
	// produced a plan the compiler cannot render.
	CodeCompilationError Code = "COMPILATION_ERROR"

	// CodeNotImplemented marks a pipeline stage that does not exist yet. It has
	// no counterpart in section 23 because the brief describes the finished
	// system; the experiment is built in phases and must never paper over a
	// missing stage with a plausible-looking query. Every use of this code should
	// disappear by Phase 7.
	CodeNotImplemented Code = "NOT_IMPLEMENTED"

	// CodeInternal is the catch-all for a broken invariant. Its message is
	// deliberately vague to the caller; detail belongs in debug output.
	CodeInternal Code = "INTERNAL_ERROR"
)

// Error is a coded failure. It is the only error type the CLI renders, so every
// exit path has a category that the evaluation harness can count.
type Error struct {
	Code Code
	// Message explains the failure to the caller. It must stay free of secrets
	// and of raw config values (brief, section 16.10) — it is printed by default,
	// not only under --debug.
	Message string
	// Wrapped carries the underlying cause for debug output. It is NOT included
	// in the default rendering: a JSON parse error, for instance, can quote
	// arbitrary file content.
	Wrapped error
}

func (e *Error) Error() string {
	if e.Wrapped != nil {
		return fmt.Sprintf("%s: %s: %v", e.Code, e.Message, e.Wrapped)
	}
	return fmt.Sprintf("%s: %s", e.Code, e.Message)
}

func (e *Error) Unwrap() error { return e.Wrapped }

// New builds a coded error.
func New(code Code, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...)}
}

// Wrap builds a coded error carrying a cause. The cause is shown only in debug
// output; see Error.Wrapped.
func Wrap(code Code, cause error, format string, args ...any) *Error {
	return &Error{Code: code, Message: fmt.Sprintf(format, args...), Wrapped: cause}
}

// CodeOf reports the code of the first *Error in err's chain. Anything
// unclassified is CodeInternal, so that an uncoded error surfacing from a later
// phase is loud rather than silently bucketed as a user mistake.
func CodeOf(err error) Code {
	var e *Error
	if errors.As(err, &e) {
		return e.Code
	}
	return CodeInternal
}

// JSONRevealed renders the error for --json --debug: the redaction-safe fields
// plus a detail string and the wrapped cause.
//
// DEBUG ONLY. It is a separate method rather than a flag on JSON so that the
// safe rendering is the one a caller reaches for by default, and revealing is
// always an explicit choice at the call site.
func (e *Error) JSONRevealed(detail string) ([]byte, error) {
	out := struct {
		Error struct {
			Code    Code   `json:"code"`
			Message string `json:"message"`
			Detail  string `json:"detail,omitempty"`
			Cause   string `json:"cause,omitempty"`
		} `json:"error"`
	}{}
	out.Error.Code = e.Code
	out.Error.Message = e.Message
	out.Error.Detail = detail
	if e.Wrapped != nil {
		out.Error.Cause = e.Wrapped.Error()
	}
	return json.MarshalIndent(out, "", "  ")
}

// JSON renders the error for --json output. Only the code and the redaction-safe
// message cross this boundary.
func (e *Error) JSON() ([]byte, error) {
	return json.MarshalIndent(struct {
		Error struct {
			Code    Code   `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}{Error: struct {
		Code    Code   `json:"code"`
		Message string `json:"message"`
	}{Code: e.Code, Message: e.Message}}, "", "  ")
}
