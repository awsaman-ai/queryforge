package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	qf "github.com/awsaman-ai/queryforge"
)

// defaultTimeout bounds an operation when the request names no deadline. It is
// generous because the cost of being too short is a failed translation the user
// paid a model call for, while the cost of being too long is a subprocess that
// lingers — and the SDK is waiting on it synchronously either way.
const defaultTimeout = 60 * time.Second

// newEngine is the seam tests use to inject a stub model provider. Production
// builds leave it nil, so handle() constructs the real engine from the config —
// meaning the offline test suite never needs an API key and never makes a
// network call, while the shipped binary has no test-only branch in it.
var newEngine func(*qf.Config) *qf.Engine

// engineFor builds the engine for a request, honouring the test seam.
func engineFor(cfg *qf.Config) *qf.Engine {
	if newEngine != nil {
		return newEngine(cfg)
	}
	return qf.New(cfg)
}

// handle runs one request and returns the response to write. It never returns an
// error: every failure is a Response with Success=false, because the protocol's
// whole promise is that stdout always carries one well-formed JSON object.
func handle(ctx context.Context, req *Request) *Response {
	// Version is answered before anything else — it takes no config, and an SDK
	// performing a handshake against a binary it cannot otherwise use must still
	// get an answer.
	if req.Op == OpVersion {
		return &Response{
			Success:  true,
			Protocol: ProtocolVersion,
			Op:       OpVersion,
			Version:  Version,
			Backends: qf.DefaultRegistry().Backends(),
		}
	}

	switch req.Op {
	case OpTranslate, OpGenerate, OpValidate:
		// Handled below; listed explicitly so a new op cannot silently fall
		// through to the config-loading path without being considered.
	case "":
		return errorResponse(req.Op, CodeInvalidRequest,
			`request is missing "op" (expected one of: translate, generate, validate, version)`)
	default:
		return errorResponse(req.Op, CodeUnknownOp,
			fmt.Sprintf("unknown op %q (expected one of: translate, generate, validate, version)", req.Op))
	}

	// Every remaining op needs a config: it is the vocabulary the AST is checked
	// against, so there is nothing meaningful to do without one.
	if len(req.Config) == 0 {
		return errorResponse(req.Op, CodeInvalidRequest,
			fmt.Sprintf("op %q requires a %q object", req.Op, "config"))
	}
	cfg, err := qf.ParseConfig(req.Config)
	if err != nil {
		return errorResponse(req.Op, CodeInvalidConfig, err.Error())
	}

	engine := engineFor(cfg)
	engine.ScopeInAST = req.Options.ScopeInAST
	if req.Options.MaxRepairs != nil {
		engine.MaxRepairs = *req.Options.MaxRepairs
	}

	if req.Op == OpValidate {
		return handleValidate(engine, req) // compiles nothing, so no backend to check
	}

	// Check the backend here rather than letting the registry lookup fail inside
	// the engine. Both Translate and GenerateFrom report an unknown id as a plain
	// error with no sentinel to match on, and classifying it by substring would
	// be one refactor away from silently becoming an INTERNAL error.
	backend := effectiveBackend(req.Backend)
	if _, ok := qf.DefaultRegistry().Get(backend); !ok {
		return errorResponse(req.Op, CodeUnknownBackend,
			fmt.Sprintf("unknown backend %q (registered: %s)", backend, strings.Join(engine.Backends(), ", ")))
	}

	if req.Op == OpGenerate {
		return handleGenerate(engine, backend, req)
	}
	return handleTranslate(ctx, engine, backend, req)
}

// handleTranslate runs the full NL -> query pipeline.
func handleTranslate(ctx context.Context, engine *qf.Engine, backend string, req *Request) *Response {
	if strings.TrimSpace(req.Query) == "" {
		return errorResponse(req.Op, CodeInvalidRequest,
			`op "translate" requires a non-empty "query" string`)
	}

	res, err := engine.Translate(ctx, req.Query, backend, req.Scope)
	if err != nil {
		return classify(req.Op, err)
	}

	out := &Response{
		Success:        true,
		Protocol:       ProtocolVersion,
		Op:             req.Op,
		AST:            res.AST,
		Explain:        res.Explain,
		Warnings:       res.Warnings,
		Scope:          res.Scope,
		ProviderUsed:   res.ProviderUsed,
		RepairAttempts: res.RepairAttempts,
	}
	if req.Options.IncludeRaw {
		out.Raw = res.Raw
	}
	fillQuery(out, res.Query)
	return out
}

// handleGenerate compiles a caller-supplied AST. Deterministic: no model call.
func handleGenerate(engine *qf.Engine, backend string, req *Request) *Response {
	if req.AST == nil {
		return errorResponse(req.Op, CodeInvalidRequest, `op "generate" requires an "ast" object`)
	}

	// Compute the effective AST separately so the response can report the scope
	// that was applied and an explanation of what will actually run. GenerateFrom
	// alone returns only the compiled query, which would leave an SDK unable to
	// show the caller which predicates were forced onto it.
	effective, filters, err := engine.ApplyScope(req.AST, req.Scope)
	if err != nil {
		return errorResponse(req.Op, CodeInvalidScope, err.Error())
	}

	res, err := engine.GenerateFrom(req.AST, backend, req.Scope)
	if err != nil {
		return classify(req.Op, err)
	}

	out := &Response{
		Success:  true,
		Protocol: ProtocolVersion,
		Op:       req.Op,
		AST:      req.AST,
		Explain:  qf.Explain(effective, engine.Config()),
		Warnings: res.Warnings,
		Scope:    filters,
	}
	if req.Options.ScopeInAST {
		out.AST = effective
	}
	fillQuery(out, res)
	return out
}

// handleValidate checks an AST against the config and reports the findings.
// A failing validation is a normal, expected answer here — but it is still
// reported as Success=false so an SDK's error mapping stays uniform: the caller
// asked "is this AST legal", and "no" is a QueryForgeError carrying the details.
func handleValidate(engine *qf.Engine, req *Request) *Response {
	if req.AST == nil {
		return errorResponse(req.Op, CodeInvalidRequest, `op "validate" requires an "ast" object`)
	}
	if err := engine.Validate(req.AST); err != nil {
		return classify(req.Op, err)
	}
	return &Response{
		Success:  true,
		Protocol: ProtocolVersion,
		Op:       req.Op,
		AST:      req.AST,
		Explain:  qf.Explain(req.AST, engine.Config()),
	}
}

// effectiveBackend applies the default. "sql" (Postgres dialect) matches the
// library's own default and the shipped example configs.
func effectiveBackend(b string) string {
	if strings.TrimSpace(b) == "" {
		return "sql"
	}
	return b
}

// fillQuery copies a compiled *qf.Result into the response's query fields. SQL
// backends populate SQL+Args; document backends populate Doc. Splitting the
// envelope this way — rather than nesting a "query" object — means a Java or
// Python caller reads `response.sql` directly instead of digging one level down
// for the common case.
func fillQuery(out *Response, res *qf.Result) {
	if res == nil {
		return
	}
	out.Backend = res.Backend
	out.SQL = res.SQL
	out.Args = res.Args
	out.Doc = res.Doc
	// Warnings can arrive from either the TranslateResult or the Result; they are
	// the same slice in practice, so only take them here if nothing set them.
	if len(out.Warnings) == 0 {
		out.Warnings = res.Warnings
	}
}

// classify maps an engine error onto a protocol Code. The order of the checks is
// load-bearing: the specific causes are tested before the general ones, because
// the engine wraps its budget-exhausted error around the last underlying failure
// and both would otherwise match.
func classify(op Op, err error) *Response {
	// A deadline is checked first. The engine surfaces a cancelled model call as
	// a transport failure — technically true, but "the model was unreachable"
	// sends the caller to check their API key when the real fix is a longer
	// timeout.
	if errors.Is(err, context.DeadlineExceeded) {
		return errorResponse(op, CodeTimeout, err.Error())
	}

	// Scope is checked before validation because a bad scope is the calling
	// application's bug, not the end user's question, and the two want different
	// handling: an INVALID_SCOPE should page whoever wired up the tenancy
	// filters, while a VALIDATION_FAILED is ordinary feedback about a question.
	if errors.Is(err, qf.ErrScope) {
		return errorResponse(op, CodeInvalidScope, err.Error())
	}

	// A refusal is a deliberate, well-formed answer from the model.
	var unsupported *qf.UnsupportedRequestError
	if errors.As(err, &unsupported) {
		return errorResponse(op, CodeUnsupportedRequest, err.Error())
	}

	// Validation findings survive the budget-exhausted wrapper, so this catches
	// both a caller's bad AST and a model that never produced a conforming one.
	var verrs qf.ValidationErrors
	if errors.As(err, &verrs) {
		return errorResponse(op, CodeValidationFailed, err.Error(), detailsFrom(verrs)...)
	}

	if errors.Is(err, qf.ErrModelOutput) {
		return errorResponse(op, CodeModelOutput, err.Error())
	}
	if errors.Is(err, qf.ErrModelTransport) {
		return errorResponse(op, CodeModelTransport, err.Error())
	}

	// Everything left on a compile path is a generator failure — the AST was
	// legal but could not be turned into this backend's dialect.
	if op == OpGenerate {
		return errorResponse(op, CodeGenerateFailed, err.Error())
	}
	return errorResponse(op, CodeInternal, err.Error())
}

// detailsFrom flattens the engine's structured findings into protocol Details.
func detailsFrom(verrs qf.ValidationErrors) []Detail {
	out := make([]Detail, 0, len(verrs))
	for _, v := range verrs {
		if v == nil {
			continue
		}
		out = append(out, Detail{
			Code:        string(v.Code),
			Path:        v.Path,
			Field:       v.Field,
			Message:     v.Error(),
			Suggestions: v.Suggestions,
		})
	}
	return out
}
