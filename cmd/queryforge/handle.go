package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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
//
// It is also the ONE PLACE THIS BINARY LOGS AN OUTCOME. Every exit path funnels
// through the deferred summary below, which is what delivers two properties the
// design brief calls for and that scattered log calls cannot: no failure exits
// unlogged, and no failure is logged twice at successive layers.
func handle(ctx context.Context, logger *slog.Logger, req *Request) *Response {
	// Version is answered before anything else — it takes no config, and an SDK
	// performing a handshake against a binary it cannot otherwise use must still
	// get an answer. It is also the one op that cannot fail, so it needs no
	// outcome logging beyond a debug line.
	if req.Op == OpVersion {
		logger.Debug("handshake answered",
			logKeyComponent, logComponentCLI,
			logKeyOperation, string(OpVersion))
		return &Response{
			Success:  true,
			Protocol: ProtocolVersion,
			Op:       OpVersion,
			Version:  Version,
			Backends: qf.DefaultRegistry().Backends(),
		}
	}

	start := time.Now()
	log := logger.With(logKeyComponent, logComponentCLI, logKeyOperation, string(req.Op))

	// The single outcome line for this request, emitted on every return path.
	//
	// Naming the response through a pointer-to-pointer is the only way a defer
	// can see what a `return errorResponse(...)` produced, and it is worth the
	// small awkwardness: the alternative is a log call before each of the eleven
	// returns below, which is how a new early return ships unlogged.
	var resp *Response
	defer func() { logOutcome(log, resp, time.Since(start)) }()

	switch req.Op {
	case OpTranslate, OpGenerate, OpValidate:
		// Handled below; listed explicitly so a new op cannot silently fall
		// through to the config-loading path without being considered.
	case "":
		resp = errorResponse(req.Op, CodeInvalidRequest,
			`request is missing "op" (expected one of: translate, generate, validate, version)`)
		return resp
	default:
		resp = errorResponse(req.Op, CodeUnknownOp,
			fmt.Sprintf("unknown op %q (expected one of: translate, generate, validate, version)", req.Op))
		return resp
	}

	// Every remaining op needs a config: it is the vocabulary the AST is checked
	// against, so there is nothing meaningful to do without one.
	if len(req.Config) == 0 {
		resp = errorResponse(req.Op, CodeInvalidRequest,
			fmt.Sprintf("op %q requires a %q object", req.Op, "config"))
		return resp
	}
	cfg, err := qf.ParseConfig(req.Config)
	if err != nil {
		// A configuration failure fails fast, before any engine is built and
		// before a model call could be paid for. The message names the JSON or
		// structural problem; the config itself is never logged, since it can
		// carry table and column names a customer treats as confidential.
		resp = errorResponse(req.Op, CodeInvalidConfig, err.Error())
		return resp
	}

	// Shape, not content. The entity and field count are enough to tell two
	// configs apart in a log without reproducing either of them.
	log = log.With(logKeyEntity, cfg.Entity, logKeyFieldsN, len(cfg.Fields))
	if keys := sortedKeys(req.Scope); len(keys) > 0 {
		// Scope KEYS. Never the values — those are tenant and user ids.
		log = log.With(logKeyScopeKeys, keys)
	}

	engine := engineFor(cfg)
	engine.ScopeInAST = req.Options.ScopeInAST
	if req.Options.MaxRepairs != nil {
		engine.MaxRepairs = *req.Options.MaxRepairs
	}
	// Push the logger into the library through its Observer seam. SetObserver
	// rather than assigning Observe, so the provider reports model latency and
	// token counts too — the only expensive facts in a translation. When logging
	// is off this installs an Observer that writes to a discarding handler; the
	// cost is one nil check plus a level comparison per event, which against a
	// ~1.8s model call is not measurable.
	engine.SetObserver(qf.SlogObserver(log))

	if req.Op == OpValidate {
		resp = handleValidate(engine, req) // compiles nothing, so no backend to check
		return resp
	}

	// Check the backend here rather than letting the registry lookup fail inside
	// the engine. Both Translate and GenerateFrom report an unknown id as a plain
	// error with no sentinel to match on, and classifying it by substring would
	// be one refactor away from silently becoming an INTERNAL error.
	backend := effectiveBackend(req.Backend)
	if _, ok := qf.DefaultRegistry().Get(backend); !ok {
		resp = errorResponse(req.Op, CodeUnknownBackend,
			fmt.Sprintf("unknown backend %q (registered: %s)", backend, strings.Join(engine.Backends(), ", ")))
		return resp
	}
	log = log.With(logKeyBackend, backend)

	if req.Op == OpGenerate {
		resp = handleGenerate(engine, backend, req)
		return resp
	}
	resp = handleTranslate(ctx, engine, backend, req)
	return resp
}

// logOutcome writes the one summary line for a finished request.
//
// Success is INFO — a lifecycle event a running system wants a record of.
// Failure is ERROR, except for a refusal, which is INFO: a refusal means the
// model declined to invent a query for something the config cannot express,
// which is the guard rail working. Reporting that at ERROR is how an error
// stream becomes something people filter out.
//
// A nil response would mean handle panicked, which cannot currently happen but
// would otherwise produce a log line claiming success. Say so instead.
func logOutcome(log *slog.Logger, resp *Response, elapsed time.Duration) {
	ms := elapsed.Milliseconds()
	switch {
	case resp == nil:
		log.Error("request produced no response", logKeyDuration, ms,
			logKeyErrorCode, string(CodeInternal))
	case resp.Success:
		log.Info("request completed", logKeyDuration, ms, logKeyOutcome, "ok")
	case resp.Code == CodeUnsupportedRequest:
		log.Info("request refused", logKeyDuration, ms,
			logKeyOutcome, "refusal", logKeyErrorCode, string(resp.Code))
	default:
		// The message is the engine's, and is safe: every errorResponse message
		// in this binary describes a rule or a transport fact, never the
		// question text. `error` carries it so a reader does not have to
		// correlate with the response they may not have kept.
		log.Error("request failed", logKeyDuration, ms,
			logKeyOutcome, "error", logKeyErrorCode, string(resp.Code),
			"error", resp.Message)
	}
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

// classify maps an engine error onto a protocol Code.
//
// The chain of errors.Is/errors.As checks that used to live here now lives in
// the library, as qf.Classify, and this function is its protocol-level wrapper.
// Moving it was not tidying: four surfaces have to agree on what code an error
// carries — this binary, the Go library's own logs, and the two SDKs that map
// the code onto an exception class — and two independent copies of that chain
// would drift the first time a sentinel was added, with nothing failing to say
// so. A caller would simply start getting INTERNAL for something that had a
// perfectly good name.
//
// Two things stay here, because they are protocol knowledge the library does
// not have:
//
//   - the structured Details for a validation failure, which are this
//     protocol's flattening of qf.ValidationError, and
//   - the GENERATE_FAILED fallback, which depends on which op was requested:
//     an unrecognised error on a compile path means the AST was legal but the
//     generator could not render it, whereas the same error anywhere else is a
//     genuine internal fault.
func classify(op Op, err error) *Response {
	code := qf.Classify(err)

	if code == qf.FailureValidation {
		// Findings survive the budget-exhausted wrapper, so this catches both a
		// caller's bad AST and a model that never produced a conforming one.
		var verrs qf.ValidationErrors
		if errors.As(err, &verrs) {
			return errorResponse(op, CodeValidationFailed, err.Error(), detailsFrom(verrs)...)
		}
		return errorResponse(op, CodeValidationFailed, err.Error())
	}

	if code == qf.FailureInternal && op == OpGenerate {
		return errorResponse(op, CodeGenerateFailed, err.Error())
	}

	return errorResponse(op, Code(code), err.Error())
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
