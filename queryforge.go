package queryforge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"
)

// Engine is the public entry point of the library. It composes the four pieces
// — planner (AI), validator (deterministic guarantee), generators (deterministic
// compilation), and explainer — behind a small API. Construct it with New (which
// wires the provider from config) or NewWithProvider (inject your own provider,
// e.g. a stub for tests).
type Engine struct {
	config     *Config          // the single source of truth
	provider   ModelProvider    // how the model is reached
	planner    *Planner         // NL -> candidate AST
	registry   *Registry        // backend id -> generator
	MaxRepairs int              // bounded validation-repair retries (default 2)
	Now        func() time.Time // injectable clock for relative dates (tests)

	// ScopeInAST chooses what TranslateResult.AST reports when a Scope is
	// supplied. The compiled query and the explanation always include the scope;
	// only this one field changes.
	//
	//	false (default) — the AST is exactly what the model produced, in the
	//	  config's vocabulary. The scope is reported separately on
	//	  TranslateResult.Scope. This AST round-trips: hand it back to
	//	  GenerateFrom with the same Scope and you get the identical query.
	//
	//	true — the AST is the effective one, scope predicates included, so it
	//	  alone is a complete record of what was compiled. Note that it then
	//	  names fields the config need not declare, so feeding it back through
	//	  GenerateFrom fails validation unless those fields are registered.
	//
	// Pick false for pipelines that re-compile ASTs; true for audit logs that
	// store one object and must be able to prove what ran.
	ScopeInAST bool

	// Observe receives one Event per notable step of a translation — every model
	// round trip, every repair attempt, and the final outcome. nil (the default)
	// disables emission entirely.
	//
	// Set it directly for an engine built by New, which wires the same Observer
	// into the providers it constructs so model-call events (latency, tokens,
	// finish reason) are reported too. For NewWithProvider, call SetObserver on
	// the engine instead of assigning this field, so a caller-supplied provider
	// gets wired up the same way. See Observer for the implementer's contract.
	Observe Observer
}

// TranslateResult is the full output of a translation: the AST, the compiled
// backend query, a prose explanation, the raw model text (for logging), any
// non-fatal warnings, and how many repair attempts were needed.
type TranslateResult struct {
	AST            *Query   // the validated intermediate representation
	Query          *Result  // the compiled backend query (SQL/Doc)
	Explain        string   // deterministic prose rendering of the AST
	Raw            string   // raw model output, for audit/telemetry
	Warnings       []string // advisories (e.g. non-indexed filter)
	RepairAttempts int      // number of validation repairs performed (0 = first try)
	ProviderUsed   string   // which model answered, when a fallback chain is configured

	// Repairs records why each earlier attempt was rejected, oldest first. It is
	// empty on a first-try success, and has exactly RepairAttempts entries
	// otherwise.
	//
	// It exists because RepairAttempts alone says a retry happened but not what
	// provoked it, and that "what" is almost always a config problem rather than
	// a model problem: a field typed `string` that the model insists is an enum,
	// an operator the whitelist forbids, a value outside a declared domain. The
	// engine computes each reason to build the retry prompt and, before this
	// field existed, discarded it — leaving the only copy in an Observer event,
	// which a caller reading an API response cannot see.
	//
	// Treat a non-empty Repairs on an otherwise successful translation as a
	// signal worth surfacing: the answer was correct, but it cost an extra model
	// round trip that a config change would remove.
	Repairs []RepairRecord

	// Scope lists the caller-supplied filters that were AND-ed into the query,
	// normalized and in the order they were applied. It is always populated when
	// a Scope was passed, whatever ScopeInAST is set to — an audit log should
	// never have to infer which predicates were forced.
	Scope []ScopeFilter
}

// RepairRecord is one rejected attempt in the bounded repair loop.
//
// Kind separates the two failure modes, because they call for different fixes:
// a RepairParse means the model's reply was not usable JSON (a model or prompt
// problem, usually transient), while a RepairValidation means the reply parsed
// cleanly but broke a rule this config declares (almost always a config problem
// that will recur on every similar question until the config changes).
//
// Errors is populated only for RepairValidation, and carries the structured
// ValidationError list — Code, Path, Field — so a caller can classify the
// failure without matching the prose in Message. Message is always set and is
// the full rendered error, which is also exactly what was fed back to the model.
type RepairRecord struct {
	Attempt int              `json:"attempt"`          // 0-based attempt that was rejected
	Kind    RepairKind       `json:"kind"`             // "validation" or "parse"
	Message string           `json:"message"`          // the full error, as fed back to the model
	Errors  ValidationErrors `json:"errors,omitempty"` // structured detail; validation failures only
}

// New builds an engine from a config, selecting the model provider(s) from the
// config's model block(s): a single provider when only `model` is set, or an
// ordered fallback chain when `models` lists several. This is the standard
// constructor.
func New(c *Config) *Engine {
	return NewWithProvider(c, ProvidersFrom(c))
}

// NewWithProvider builds an engine with a caller-supplied model provider. Use it
// for a custom dialect or a deterministic stub in tests.
func NewWithProvider(c *Config, p ModelProvider) *Engine {
	return &Engine{
		config:     c,
		provider:   p,
		planner:    NewPlanner(c, p),
		registry:   DefaultRegistry(),
		MaxRepairs: 2, // §5.2: bounded to N retries, then fail closed
		Now:        func() time.Time { return time.Now().UTC() },
	}
}

// SetObserver installs an Observer on the engine AND pushes it down into the
// provider, so model-call events (latency, token counts, finish reason) are
// reported alongside the engine's own attempt/translate events.
//
// It is the preferred way to turn observability on, because assigning the
// Observe field directly reaches only the engine: a provider built by the
// caller and handed to NewWithProvider would then report nothing, and the
// tokens and latency — the two facts worth watching — would be missing.
// Providers that cannot report (a third-party ModelProvider implementation, or
// a test stub) are skipped silently; the engine's own events still flow.
func (e *Engine) SetObserver(o Observer) {
	e.Observe = o
	if s, ok := e.provider.(observerSetter); ok {
		s.SetObserver(o)
	}
}

// now returns the engine's reference time.
func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

// Translate runs the full pipeline: NL -> AST (one model call) -> validate ->
// (bounded repair) -> scope injection -> generate -> explain. It fails closed:
// if the model cannot produce a valid AST within the repair budget, it returns
// an error rather than a guessed query.
//
// scope carries filters the calling application imposes regardless of what was
// asked — subscription, tenant, user, enterprise ids. They are AND-ed onto the
// root of the filter tree after validation, so they can only narrow the result
// and the model never learns they exist. Pass nil for none. See Scope.
func (e *Engine) Translate(ctx context.Context, text, backend string, scope Scope) (*TranslateResult, error) {
	start := time.Now() // wall clock for the EventTranslate duration

	// base returns a fresh event pre-filled with the fields every kind carries.
	// Building it here keeps each emission site to the facts it actually adds.
	base := func(kind EventKind, attempt int) Event {
		ev := Event{Kind: kind, Attempt: attempt, Backend: backend}
		if e.config != nil {
			ev.Entity = e.config.Entity
		}
		return ev
	}

	// done emits the single EventTranslate for this call. Every exit path below
	// routes through it, so a caller counting events never sees a translation
	// that started and never ended.
	done := func(attempt int, outcome Outcome, err error, res *TranslateResult, filters []ScopeFilter) {
		if e.Observe == nil {
			return // skip the work entirely when nobody is listening
		}
		ev := base(EventTranslate, attempt)
		ev.Outcome = outcome
		ev.Err = err
		ev.Duration = time.Since(start)
		ev.RepairAttempts = attempt
		ev.ScopeKeys = scopeKeys(filters)
		if res != nil {
			ev.Warnings = res.Warnings
		}
		e.Observe.emit(ctx, ev)
	}

	gen, ok := e.registry.Get(backend)
	if !ok {
		err := fmt.Errorf("unknown backend %q (registered: %s)", backend, strings.Join(e.registry.Backends(), ", "))
		done(0, OutcomeCallerError, err, nil, nil)
		return nil, err
	}

	// Normalize the scope up front. A bad scope is a bug in the calling code and
	// cannot be repaired by asking the model again, so it should not cost an API
	// call or leave the caller wondering whether the question was at fault.
	//
	// Named scopeErr rather than err deliberately: the repair loop below declares
	// its own err per iteration, and a function-scoped err here would be shadowed
	// by it — which reads as though the loop were assigning the outer variable.
	filters, scopeErr := normalizeScope(scope, e.config)
	if scopeErr != nil {
		done(0, OutcomeCallerError, scopeErr, nil, nil)
		return nil, scopeErr
	}

	var hint RepairHint        // repair hint; zero value on the first attempt
	var lastErr error          // last repairable error, for the fail-closed message
	var raw string             // most recent raw model output
	var repairs []RepairRecord // why each rejected attempt was rejected, oldest first

	for attempt := 0; attempt <= e.MaxRepairs; attempt++ {
		ast, r, err := e.planner.Plan(ctx, text, hint)
		raw = r
		if err != nil {
			// Report the failed attempt before deciding what to do with it. Raw
			// travels with the event because it is the only record of WHAT the
			// model said — the error alone says only that the reply was unusable.
			outcome := classifyPlanError(err)
			if e.Observe != nil {
				ev := base(EventAttempt, attempt)
				ev.Outcome = outcome
				ev.Err = err
				ev.Raw = raw
				e.Observe.emit(ctx, ev)
			}

			// An unreadable reply is worth another ask — models occasionally
			// emit truncated or double-brace JSON, and one nudge usually fixes
			// it. Everything else (transport failure, or a deliberate refusal)
			// will not change on a retry, so surface it now.
			if errors.Is(err, ErrModelOutput) {
				lastErr = err
				hint = RepairHint{Kind: RepairParse, Message: err.Error()}
				// No Errors slice: nothing parsed, so there is no AST to have
				// structured findings about.
				repairs = append(repairs, RepairRecord{
					Attempt: attempt, Kind: RepairParse, Message: err.Error(),
				})
				continue
			}
			done(attempt, outcome, err, nil, filters)
			return nil, err
		}

		if verr := Validate(ast, e.config); verr != nil {
			lastErr = verr // remember why it failed
			if e.Observe != nil {
				ev := base(EventAttempt, attempt)
				ev.Outcome = OutcomeValidation
				ev.Err = verr
				ev.Raw = raw
				e.Observe.emit(ctx, ev)
			}
			// Feed the rule it broke back to the model.
			hint = RepairHint{Kind: RepairValidation, Message: verr.Error()}

			// Keep the structured findings for the caller. Validate returns a
			// ValidationErrors, but it is typed as error, so match rather than
			// assume: a future validator returning some other error type must
			// degrade to just the message, never panic. errors.As rather than a
			// type assertion so the findings survive a validator that wraps.
			rec := RepairRecord{Attempt: attempt, Kind: RepairValidation, Message: verr.Error()}
			var ves ValidationErrors
			if errors.As(verr, &ves) {
				rec.Errors = ves
			}
			repairs = append(repairs, rec)

			continue // try again within the budget
		}

		// The model's half succeeded: a parseable AST that satisfies every config
		// rule. Report that before compiling, so the attempt event describes the
		// model interaction only and a generator failure cannot be misread as the
		// model's fault.
		if e.Observe != nil {
			ev := base(EventAttempt, attempt)
			ev.Outcome = OutcomeOK
			e.Observe.emit(ctx, ev)
		}

		// Valid AST: splice in the caller's scope, then compile deterministically
		// and explain. Both the query and the explanation describe the effective
		// AST, so what the caller is shown is always what will run.
		effective := applyScope(ast, filters)
		res, gerr := gen.Generate(effective, e.config, GenOptions{Now: e.now()})
		if gerr != nil {
			done(attempt, OutcomeGenerate, gerr, nil, filters)
			return nil, gerr
		}
		out := &TranslateResult{
			AST:            ast,
			Query:          res,
			Explain:        Explain(effective, e.config),
			Raw:            raw,
			Warnings:       res.Warnings,
			RepairAttempts: attempt,
			Repairs:        repairs,
			Scope:          filters,
		}
		if e.ScopeInAST {
			out.AST = effective // report the compiled AST instead of the model's
		}
		// When a fallback chain is configured, record which model answered.
		if namer, ok := e.provider.(providerNamer); ok {
			out.ProviderUsed = namer.LastUsed()
		}
		done(attempt, OutcomeOK, nil, out, filters)
		return out, nil
	}

	// Budget exhausted. Name the failure kind so the caller can tell a schema
	// mismatch (fix the config or the phrasing) from a model that kept emitting
	// unusable output (retry, or switch models).
	if errors.Is(lastErr, ErrModelOutput) {
		spent := fmt.Errorf("model returned unparseable output on all %d attempt(s); last error: %w", e.MaxRepairs+1, lastErr)
		done(e.MaxRepairs, OutcomeBudgetSpent, spent, nil, filters)
		return nil, spent
	}
	spent := fmt.Errorf("translation failed validation after %d attempt(s); last error: %w", e.MaxRepairs+1, lastErr)
	done(e.MaxRepairs, OutcomeBudgetSpent, spent, nil, filters)
	return nil, spent
}

// classifyPlanError maps a Plan failure onto an Outcome. It mirrors, exactly,
// the branching the repair loop already performs on the same error — a refusal
// is a deliberate answer, a transport failure never reaches the model, and an
// unreadable reply is the repairable case.
func classifyPlanError(err error) Outcome {
	var unsupported *UnsupportedRequestError
	switch {
	case errors.As(err, &unsupported):
		return OutcomeRefusal
	case errors.Is(err, ErrModelOutput):
		return OutcomeParseError
	case errors.Is(err, ErrModelTransport):
		return OutcomeTransport
	default:
		// Plan tags everything it returns, so this is unreachable today. Report
		// it as a transport failure rather than inventing a kind: an unclassified
		// error means the model did not produce a usable answer.
		return OutcomeTransport
	}
}

// GenerateFrom is the deterministic half only: validate an existing AST, splice
// in the caller's scope, and compile it to a backend. It makes NO model call and
// touches NO network — it is the entry point tests and multi-backend fan-out use.
//
// scope behaves exactly as in Translate, and applies here too: the deterministic
// path must be as confined as the AI one, or a caller could sidestep tenancy by
// building the AST itself. Pass nil for none.
//
// ast is never modified, so fanning one AST out to several backends with the
// same scope compiles the same predicates each time rather than accumulating
// them.
func (e *Engine) GenerateFrom(ast *Query, backend string, scope Scope) (*Result, error) {
	gen, ok := e.registry.Get(backend)
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (registered: %s)", backend, strings.Join(e.registry.Backends(), ", "))
	}
	if err := Validate(ast, e.config); err != nil {
		return nil, err // never compile an invalid AST
	}
	filters, err := normalizeScope(scope, e.config)
	if err != nil {
		return nil, err
	}
	return gen.Generate(applyScope(ast, filters), e.config, GenOptions{Now: e.now()})
}

// ApplyScope performs, on its own, the scope-injection step that Translate and
// GenerateFrom do internally: it returns the effective AST that would be
// compiled, plus the normalized filters that were spliced in.
//
// It exists because GenerateFrom returns only the compiled query, leaving a
// caller who wants to Explain or audit that call with no way to see the scope
// that was applied. It is deterministic — no model call, no network — and never
// modifies ast.
func (e *Engine) ApplyScope(ast *Query, scope Scope) (*Query, []ScopeFilter, error) {
	filters, err := normalizeScope(scope, e.config)
	if err != nil {
		return nil, nil, err
	}
	return applyScope(ast, filters), filters, nil
}

// Validate exposes standalone validation with the engine's config.
func (e *Engine) Validate(ast *Query) error {
	return Validate(ast, e.config)
}

// Register adds (or replaces) a backend generator — the plugin point.
func (e *Engine) Register(g Generator) {
	e.registry.Register(g)
}

// Config returns the engine's config (read-only use).
func (e *Engine) Config() *Config { return e.config }

// Backends lists the registered backend ids.
func (e *Engine) Backends() []string { return e.registry.Backends() }
