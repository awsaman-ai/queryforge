package main

import (
	"encoding/json"

	qf "github.com/awsaman-ai/queryforge"
)

// ProtocolVersion is the wire-format version this binary speaks. It is reported
// by the `version` op and carried on every response.
//
// The SDKs check it on the handshake and refuse a binary whose MAJOR differs
// from the one they were built against. That check is the whole reason the field
// exists: an SDK and a binary are shipped as one artifact, but a user who points
// QUERYFORGE_BINARY at a stale build would otherwise get silent field-drop
// nonsense (a response missing `sql`) instead of a sentence naming the mismatch.
//
// Bump MINOR for additive changes (a new op, a new optional response field) —
// old SDKs keep working because they ignore what they do not know. Bump MAJOR
// only when an existing field changes meaning or disappears.
const ProtocolVersion = "1.0"

// Op is the requested operation. Keeping this a closed set — rather than, say,
// deriving a method name from the request — means an unknown op is a clean
// UNKNOWN_OP error instead of a panic, and the supported list can be printed.
type Op string

const (
	// OpTranslate is the full pipeline: natural language -> AST -> compiled
	// query. It makes exactly one model call (more if a repair is needed), so it
	// needs a `model` block in the config and the API key in the environment.
	OpTranslate Op = "translate"

	// OpGenerate is the deterministic half: compile a caller-supplied AST. No
	// model call, no network, no API key. This is what SDK test suites use, and
	// what an application uses to re-compile a stored AST for a second backend.
	OpGenerate Op = "generate"

	// OpValidate checks an AST against the config and returns the structured
	// findings without compiling anything. Also fully offline.
	OpValidate Op = "validate"

	// OpVersion is the handshake: binary version, protocol version, registered
	// backends. It reads no config and is the one op that cannot fail.
	OpVersion Op = "version"
)

// Request is one unit of work read from stdin as a single JSON object.
//
// Config is held as json.RawMessage rather than a *qf.Config so the binary can
// distinguish "no config supplied" from "config supplied but empty", and so a
// config that fails to parse produces an INVALID_CONFIG error naming the JSON
// problem rather than a zero-value config that fails much later with a confusing
// "unknown field" complaint.
type Request struct {
	Op      Op              `json:"op"`                // which operation to run
	Backend string          `json:"backend,omitempty"` // target backend id: sql | mysql | mongo
	Config  json.RawMessage `json:"config,omitempty"`  // the QueryForge config, inline
	Query   string          `json:"query,omitempty"`   // the natural-language question (translate)
	AST     *qf.Query       `json:"ast,omitempty"`     // a pre-built AST (generate, validate)
	Scope   qf.Scope        `json:"scope,omitempty"`   // caller-imposed filters AND-ed into the query
	Options Options         `json:"options,omitempty"` // per-call knobs; every field optional
}

// Options are the per-call engine settings an SDK may expose. They are all
// optional and all have a defined zero-value behaviour, so an SDK that never
// sets them gets the engine's own defaults.
type Options struct {
	// TimeoutMS bounds the whole operation, model call included. Zero selects
	// defaultTimeout. It exists because the alternative — an SDK killing the
	// subprocess on its own deadline — loses the structured error: the caller
	// gets "process died" instead of "the model did not answer in time".
	TimeoutMS int `json:"timeoutMs,omitempty"`

	// MaxRepairs bounds the validation-repair retries. Nil selects the engine's
	// default (2). A pointer, not a plain int, because 0 is a meaningful value
	// ("one attempt, no repairs") that must be distinguishable from "unset".
	MaxRepairs *int `json:"maxRepairs,omitempty"`

	// ScopeInAST reports the effective AST (scope predicates included) instead of
	// the model's own. See qf.Engine.ScopeInAST.
	ScopeInAST bool `json:"scopeInAst,omitempty"`

	// IncludeRaw adds the model's verbatim reply to the response. Off by default:
	// the reply is derived from the user's question, and a response object tends
	// to end up in a log.
	IncludeRaw bool `json:"includeRaw,omitempty"`
}

// Response is the single JSON object written to stdout. Exactly one of the two
// halves is populated, discriminated by Success — an SDK branches on that one
// boolean and never has to guess.
type Response struct {
	Success  bool     `json:"success"`            // true => the query fields are populated
	Protocol string   `json:"protocol"`           // ProtocolVersion, on every response
	Op       Op       `json:"op,omitempty"`       // echo of the requested op
	Warnings []string `json:"warnings,omitempty"` // non-fatal advisories (e.g. non-indexed filter)

	// --- success half ---

	Backend string    `json:"backend,omitempty"` // which generator produced the query
	SQL     string    `json:"sql,omitempty"`     // parameterized statement (SQL backends)
	Args    []any     `json:"args,omitempty"`    // bound placeholder values, in order
	Doc     any       `json:"doc,omitempty"`     // query document (Mongo and friends)
	AST     *qf.Query `json:"ast,omitempty"`     // the validated intermediate representation
	Explain string    `json:"explain,omitempty"` // deterministic prose rendering of the AST

	Scope          []qf.ScopeFilter `json:"scope,omitempty"`          // filters forced onto the query
	ProviderUsed   string           `json:"providerUsed,omitempty"`   // which model answered (fallback chains)
	RepairAttempts int              `json:"repairAttempts,omitempty"` // repairs needed before a valid AST
	Raw            string           `json:"raw,omitempty"`            // model's verbatim reply; only when IncludeRaw

	// Version-op fields.
	Version  string   `json:"version,omitempty"`  // binary version, e.g. "1.1.0"
	Backends []string `json:"backends,omitempty"` // registered backend ids

	// --- error half ---

	Code    Code   `json:"code,omitempty"`    // stable classification; branch on this, never on Message
	Message string `json:"message,omitempty"` // human-readable explanation

	// Details carries the engine's per-error findings for a VALIDATION_FAILED.
	// It is what lets an SDK say "column 'agee' does not exist" and point at the
	// field, rather than reprinting one flattened sentence.
	Details []Detail `json:"details,omitempty"`
}

// Detail is one structured finding inside an error response. It mirrors
// qf.ValidationError, flattened to the three facts an SDK can act on.
type Detail struct {
	Code    string `json:"code"`              // the engine's ErrCode, e.g. "unknown_field"
	Path    string `json:"path,omitempty"`    // where in the AST, e.g. "filter.children[0]"
	Field   string `json:"field,omitempty"`   // the logical field at fault, when there is one
	Message string `json:"message,omitempty"` // the individual finding's text

	// Suggestions are nearest-match field names for an unknown_field finding.
	// They are carried separately as well as being appended to Message, because
	// an SDK that wants to offer "did you mean status?" as a UI affordance should
	// not have to parse it back out of a sentence.
	Suggestions []string `json:"suggestions,omitempty"`
}

// Code is a stable, machine-readable classification of a failed request. It is
// SCREAMING_SNAKE (not the engine's lowercase ErrCode) deliberately: these are
// protocol-level classes that an SDK maps onto exception types, and the visual
// difference keeps them from being confused with the per-finding codes in
// Details, which are the engine's own vocabulary.
type Code string

const (
	// CodeInvalidRequest means the request itself was unusable: not JSON, missing
	// a required field, an op that needs a config with none supplied. The caller's
	// bug, and no retry will help.
	CodeInvalidRequest Code = "INVALID_REQUEST"

	// CodeUnknownOp means the op is not one this binary implements — most often
	// an SDK newer than the binary it is talking to.
	CodeUnknownOp Code = "UNKNOWN_OP"

	// CodeInvalidConfig means the config did not parse or did not survive the
	// engine's own structural checks (an enum field with no values, a bad
	// apiKeyEnv, a duplicate field name).
	CodeInvalidConfig Code = "INVALID_CONFIG"

	// CodeUnknownBackend means the requested backend id has no registered
	// generator. The message lists the ones that do.
	CodeUnknownBackend Code = "UNKNOWN_BACKEND"

	// CodeInvalidScope means a caller-supplied scope filter was rejected — an
	// undeclared field where the policy requires declaration, or a value whose
	// type does not match the field. Always the calling application's bug, never
	// the end user's question.
	CodeInvalidScope Code = "INVALID_SCOPE"

	// CodeValidationFailed means an AST broke a rule this config declares. On
	// `generate`/`validate` that is the AST the caller passed; on `translate` it
	// means the model could not produce a conforming AST within the repair
	// budget. Details carries the individual findings.
	CodeValidationFailed Code = "VALIDATION_FAILED"

	// CodeUnsupportedRequest means the model deliberately refused: the question
	// cannot be expressed in the vocabulary this config registers. A well-formed
	// answer, not a failure of the pipeline — surface the message to the user.
	CodeUnsupportedRequest Code = "UNSUPPORTED_REQUEST"

	// CodeModelOutput means the model answered but the reply was not usable JSON,
	// on every attempt. Usually transient; a retry is reasonable.
	CodeModelOutput Code = "MODEL_OUTPUT"

	// CodeModelTransport means the model was never reached: network failure, a
	// missing or rejected API key, a rate limit. Check the key and the endpoint.
	CodeModelTransport Code = "MODEL_TRANSPORT"

	// CodeGenerateFailed means a valid AST could not be compiled to the target
	// backend — e.g. a relative date with an out-of-range amount.
	CodeGenerateFailed Code = "GENERATE_FAILED"

	// CodeTimeout means the deadline in Options.TimeoutMS expired.
	CodeTimeout Code = "TIMEOUT"

	// CodeInternal is the catch-all. Reaching it is a bug in this binary; the
	// message carries whatever detail there was.
	CodeInternal Code = "INTERNAL"
)

// errorResponse builds a failed Response. Every error exit in the binary routes
// through it, which is what guarantees an SDK always receives the same object
// shape and never has to parse a bare stderr string.
func errorResponse(op Op, code Code, message string, details ...Detail) *Response {
	return &Response{
		Success:  false,
		Protocol: ProtocolVersion,
		Op:       op,
		Code:     code,
		Message:  message,
		Details:  details,
	}
}
