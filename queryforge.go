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

// now returns the engine's reference time.
func (e *Engine) now() time.Time {
	if e.Now != nil {
		return e.Now()
	}
	return time.Now().UTC()
}

// Translate runs the full pipeline: NL -> AST (one model call) -> validate ->
// (bounded repair) -> generate -> explain. It fails closed: if the model cannot
// produce a valid AST within the repair budget, it returns an error rather than
// a guessed query.
func (e *Engine) Translate(ctx context.Context, text, backend string) (*TranslateResult, error) {
	gen, ok := e.registry.Get(backend)
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (registered: %s)", backend, strings.Join(e.registry.Backends(), ", "))
	}

	var hint RepairHint // repair hint; zero value on the first attempt
	var lastErr error   // last repairable error, for the fail-closed message
	var raw string      // most recent raw model output

	for attempt := 0; attempt <= e.MaxRepairs; attempt++ {
		ast, r, err := e.planner.Plan(ctx, text, hint)
		raw = r
		if err != nil {
			// An unreadable reply is worth another ask — models occasionally
			// emit truncated or double-brace JSON, and one nudge usually fixes
			// it. Everything else (transport failure, or a deliberate refusal)
			// will not change on a retry, so surface it now.
			if errors.Is(err, ErrModelOutput) {
				lastErr = err
				hint = RepairHint{Kind: RepairParse, Message: err.Error()}
				continue
			}
			return nil, err
		}

		if verr := Validate(ast, e.config); verr != nil {
			lastErr = verr // remember why it failed
			// Feed the rule it broke back to the model.
			hint = RepairHint{Kind: RepairValidation, Message: verr.Error()}
			continue // try again within the budget
		}

		// Valid AST: compile deterministically and explain.
		res, gerr := gen.Generate(ast, e.config, GenOptions{Now: e.now()})
		if gerr != nil {
			return nil, gerr
		}
		out := &TranslateResult{
			AST:            ast,
			Query:          res,
			Explain:        Explain(ast, e.config),
			Raw:            raw,
			Warnings:       res.Warnings,
			RepairAttempts: attempt,
		}
		// When a fallback chain is configured, record which model answered.
		if namer, ok := e.provider.(providerNamer); ok {
			out.ProviderUsed = namer.LastUsed()
		}
		return out, nil
	}

	// Budget exhausted. Name the failure kind so the caller can tell a schema
	// mismatch (fix the config or the phrasing) from a model that kept emitting
	// unusable output (retry, or switch models).
	if errors.Is(lastErr, ErrModelOutput) {
		return nil, fmt.Errorf("model returned unparseable output on all %d attempt(s); last error: %w", e.MaxRepairs+1, lastErr)
	}
	return nil, fmt.Errorf("translation failed validation after %d attempt(s); last error: %w", e.MaxRepairs+1, lastErr)
}

// GenerateFrom is the deterministic half only: validate an existing AST and
// compile it to a backend. It makes NO model call and touches NO network — it is
// the entry point tests and multi-backend fan-out use.
func (e *Engine) GenerateFrom(ast *Query, backend string) (*Result, error) {
	gen, ok := e.registry.Get(backend)
	if !ok {
		return nil, fmt.Errorf("unknown backend %q (registered: %s)", backend, strings.Join(e.registry.Backends(), ", "))
	}
	if err := Validate(ast, e.config); err != nil {
		return nil, err // never compile an invalid AST
	}
	return gen.Generate(ast, e.config, GenOptions{Now: e.now()})
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
