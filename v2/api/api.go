// Package api is the smallest public surface over the v2 experiment's
// pipeline (internal/pipeline and what it depends on), for external Go
// applications that need natural language -> SQL without reaching into
// internal packages directly, which Go's internal/ rule forbids anyway.
//
// It adds no semantic behavior of its own: Open assembles the same
// config -> corpus -> embeddings -> index -> pipeline chain cmd/qf-v2 builds
// for --query, once at startup, and ToSQL is cmd/qf-v2's runQuery minus the
// CLI's flag parsing and text rendering. See examples/no-llm-tosql-api for
// the intended caller.
package api

import (
	"context"
	"errors"
	"fmt"
	"time"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/embedding"
	"queryforge.local/v2experiment/internal/pipeline"
	"queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/retrieval"
)

// Embedder selects the embedding provider Open uses.
type Embedder string

const (
	// EmbedderWorker is the real local model, out of process
	// (embedding_worker/worker.py). The default.
	EmbedderWorker Embedder = "worker"
	// EmbedderDeterministic is a hashing stand-in with no external process
	// and NO semantic meaning; see internal/embedding.NewDeterministic.
	EmbedderDeterministic Embedder = "deterministic"
)

// Options configure Open.
type Options struct {
	// ConfigPath is the semantic config JSON (required).
	ConfigPath string
	// Embedder picks the provider. Zero value is EmbedderWorker.
	Embedder Embedder
	// WorkerScript and Model are passed through to EmbedderWorker; both have
	// the same defaults as the qf-v2 CLI when empty.
	WorkerScript string
	Model        string
	// TopK is how many corpus matches retrieval considers per span. Zero
	// value defaults to 10, matching the CLI.
	TopK int
}

// Engine is a warm QueryForge V2 pipeline: config, corpus, embeddings and
// index assembled once, then reused for every request (see the v2 brief,
// section 10, "assemble once per process, never per request").
type Engine struct {
	provider embedding.Provider
	pl       *pipeline.Pipeline
}

// Open builds a warm Engine. Call it once at process startup and reuse it;
// Engine.ToSQL is safe for concurrent use, per pipeline.Pipeline's own
// contract.
func Open(ctx context.Context, opt Options) (*Engine, error) {
	if opt.ConfigPath == "" {
		return nil, qferr.New(qferr.CodeConfigError, "api.Open needs a ConfigPath")
	}

	cfg, err := config.Load(opt.ConfigPath)
	if err != nil {
		return nil, err
	}
	registry := config.NewSchemaRegistry(cfg)
	entries := corpus.Build(registry)
	if len(entries) == 0 {
		return nil, qferr.New(qferr.CodeConfigError, "config produced an empty semantic corpus")
	}

	provider, err := newProvider(ctx, opt)
	if err != nil {
		return nil, err
	}

	index, err := retrieval.Build(ctx, provider, entries, retrieval.StrategySurface)
	if err != nil {
		_ = provider.Close()
		return nil, err
	}

	topK := opt.TopK
	if topK <= 0 {
		topK = 10
	}
	pl, err := pipeline.New(registry, engineRetriever{provider: provider, index: index, topK: topK}, pipeline.Options{})
	if err != nil {
		_ = provider.Close()
		return nil, err
	}

	return &Engine{provider: provider, pl: pl}, nil
}

func newProvider(ctx context.Context, opt Options) (embedding.Provider, error) {
	switch opt.Embedder {
	case EmbedderDeterministic:
		return embedding.NewDeterministic(0), nil
	case EmbedderWorker, "":
		return embedding.NewWorker(ctx, embedding.WorkerOptions{Script: opt.WorkerScript, Model: opt.Model})
	default:
		return nil, qferr.New(qferr.CodeInvalidInput, "unknown embedder %q (known: %s, %s)", opt.Embedder, EmbedderWorker, EmbedderDeterministic)
	}
}

// Close releases the embedding provider (e.g. the worker subprocess).
func (e *Engine) Close() error {
	if e.provider != nil {
		return e.provider.Close()
	}
	return nil
}

// engineRetriever adapts a provider+index pair to pipeline.Retriever, exactly
// as cmd/qf-v2's own engineRetriever does.
type engineRetriever struct {
	provider embedding.Provider
	index    *retrieval.Index
	topK     int
}

func (r engineRetriever) Retrieve(ctx context.Context, texts []string) ([][]retrieval.Match, error) {
	vectors, err := r.provider.Embed(ctx, texts)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(texts) {
		return nil, qferr.New(qferr.CodeInternal, "embedding provider returned %d vectors for %d spans", len(vectors), len(texts))
	}
	out := make([][]retrieval.Match, len(texts))
	for k, v := range vectors {
		m, err := r.index.Search(v, r.topK)
		if err != nil {
			return nil, err
		}
		out[k] = m
	}
	return out, nil
}

// Param is one bound parameter of a compiled query, in positional order. The
// compiler never inlines a value into SQL text; a caller executing the query
// must bind these separately.
//
// Value has the shape a PostgreSQL driver takes (compiler.Param.Driver):
// int64 for a whole number in int64's range; for any other number, its EXACT
// decimal text as a string ("99999999999999999999999", "0.125"), which a
// driver sends as text and the server parses into the column's numeric type
// digit for digit; string, bool or time.Time otherwise. Numbers were float64
// until the Phase 8 gate (decision 022 §2), which rounded long literals.
type Param struct {
	Index int
	Value any
}

// Result is a successful compilation.
type Result struct {
	SQL    string
	Params []Param
}

// ToSQL runs one natural-language request through the full v2 pipeline --
// extraction, retrieval, ranking, binding, planning, the deterministic
// compiler -- and returns its SQL and parameters. A refusal (ambiguous,
// unsupported, invalid, unresolved, or otherwise unsafe) comes back as a
// non-nil *Error carrying the pipeline's own code; ToSQL never turns a
// refusal into a successful Result.
func (e *Engine) ToSQL(ctx context.Context, text string) (Result, error) {
	clock := plan.Clock{Now: time.Now(), Location: time.UTC}
	res, err := e.pl.Run(ctx, text, clock)
	if err != nil {
		return Result{}, asError(err)
	}
	params := make([]Param, 0, len(res.Query.Params))
	for _, p := range res.Query.Params {
		params = append(params, Param{Index: p.Index, Value: p.Driver()})
	}
	return Result{SQL: res.Query.SQL, Params: params}, nil
}

// Error is a coded refusal or failure, safe to hand to a caller: Message is
// qferr's own redacted rendering, which already excludes wrapped causes,
// stored config values and (outside --debug/detail surfaces) the request
// text. See internal/qferr.
type Error struct {
	Code    string
	Message string
}

func (e *Error) Error() string { return fmt.Sprintf("%s: %s", e.Code, e.Message) }

func asError(err error) error {
	var qe *qferr.Error
	if errors.As(err, &qe) {
		return &Error{Code: string(qe.Code), Message: qe.Message}
	}
	return &Error{Code: string(qferr.CodeInternal), Message: "internal error"}
}
