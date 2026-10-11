package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/embedding"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/retrieval"
)

// This file implements --search and --build-index: the Phase 2 surface.
//
// It is NOT the query pipeline. Search exposes the retrieval layer on its own
// so that the manual gate can ask what the corpus does with a phrase, and so
// that Phase 4's ranking has a measured baseline to improve on. Turning a
// sentence into a query needs phrases (Phase 3), ranking (Phase 4), binding
// (Phase 5) and a compiler (Phase 6); --query still refuses.

// embedderKind selects a provider.
type embedderKind string

const (
	// embedderWorker is the real local model. The default, because a research
	// result must never come from anything else.
	embedderWorker embedderKind = "worker"
	// embedderDeterministic is the hashing stand-in: no model, no Python. It
	// must be asked for explicitly and announces itself loudly.
	embedderDeterministic embedderKind = "deterministic"
)

// engine is config, corpus, provider and index assembled once.
//
// Assembling in this order is the cache strategy from section 10 —
// config → corpus → embeddings → index → ready — and it happens once per
// process, never per request.
type engine struct {
	registry *config.SchemaRegistry
	entries  []corpus.Entry
	provider embedding.Provider
	index    *retrieval.Index

	buildTime  time.Duration
	loadedFrom string
}

func (e *engine) Close() error {
	if e.provider != nil {
		return e.provider.Close()
	}
	return nil
}

// newEngine loads config, builds the corpus and produces a searchable index,
// reusing a cached index when one is given and still valid.
func newEngine(ctx context.Context, opt options, stderr io.Writer) (*engine, error) {
	cfg, err := config.Load(opt.configPath)
	if err != nil {
		return nil, err
	}
	registry := config.NewSchemaRegistry(cfg)
	entries := corpus.Build(registry)
	if len(entries) == 0 {
		return nil, qferr.New(qferr.CodeConfigError, "config produced an empty semantic corpus")
	}

	eng := &engine{registry: registry, entries: entries}

	// A cached index is tried first. Load validates it against the live corpus
	// and refuses a stale one, so this cannot silently answer under an old
	// schema.
	if opt.indexPath != "" && !opt.buildIndex {
		if _, statErr := os.Stat(opt.indexPath); statErr == nil {
			index, loadErr := retrieval.Load(opt.indexPath, entries)
			if loadErr != nil {
				return nil, loadErr
			}
			eng.index = index
			eng.loadedFrom = opt.indexPath
			return eng, nil
		}
	}

	provider, err := newProvider(ctx, opt, stderr)
	if err != nil {
		return nil, err
	}
	eng.provider = provider

	start := time.Now()
	index, err := retrieval.Build(ctx, provider, entries, opt.strategy)
	if err != nil {
		_ = provider.Close()
		return nil, err
	}
	eng.index = index
	eng.buildTime = time.Since(start)
	return eng, nil
}

func newProvider(ctx context.Context, opt options, stderr io.Writer) (embedding.Provider, error) {
	switch opt.embedder {
	case embedderDeterministic:
		// Loud on purpose. A number from this provider is a mechanism check,
		// never a finding, and the warning is what stops one being pasted into
		// a results table by mistake.
		fmt.Fprintln(stderr, "warning: using the deterministic hashing embedder — results are NOT semantic and must not be reported as research findings")
		return embedding.NewDeterministic(0), nil

	case embedderWorker, "":
		var workerErr io.Writer = io.Discard
		if opt.debug {
			// The worker's load progress and any traceback go to stderr; they
			// are its own diagnostics and name no config values.
			workerErr = stderr
		}
		return embedding.NewWorker(ctx, embedding.WorkerOptions{
			Script: opt.workerScript,
			Model:  opt.model,
			Stderr: workerErr,
		})

	default:
		return nil, qferr.New(qferr.CodeInvalidInput,
			"unknown embedder %q (known: %s, %s)", opt.embedder, embedderWorker, embedderDeterministic)
	}
}

// runBuildIndex embeds the corpus and writes the index to disk.
func runBuildIndex(ctx context.Context, opt options, stdout, stderr io.Writer) error {
	eng, err := newEngine(ctx, opt, stderr)
	if err != nil {
		return err
	}
	defer eng.Close()

	if err := eng.index.Save(opt.indexPath); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "index written: %s\n", opt.indexPath)
	fmt.Fprintf(stdout, "  model:    %s\n", eng.index.Identity())
	fmt.Fprintf(stdout, "  strategy: %s\n", eng.index.Strategy())
	fmt.Fprintf(stdout, "  corpus:   %d entries, %d vectors\n", eng.index.Len(), eng.index.VectorCount())
	fmt.Fprintf(stdout, "  built in: %s\n", eng.buildTime.Round(time.Millisecond))
	return nil
}

// runSearch embeds a phrase and reports the nearest corpus entries.
func runSearch(ctx context.Context, opt options, stdout, stderr io.Writer) error {
	if strings.TrimSpace(opt.search) == "" {
		return qferr.New(qferr.CodeInvalidInput, "--search needs a phrase")
	}

	eng, err := newEngine(ctx, opt, stderr)
	if err != nil {
		return err
	}
	defer eng.Close()

	// A loaded index carries its own vectors but no provider, so the query
	// still needs one. This is the single-query path the brief separates from
	// batch embedding.
	if eng.provider == nil {
		provider, perr := newProvider(ctx, opt, stderr)
		if perr != nil {
			return perr
		}
		eng.provider = provider
	}

	start := time.Now()
	query, err := eng.provider.EmbedOne(ctx, opt.search)
	if err != nil {
		return err
	}
	embedTime := time.Since(start)

	start = time.Now()
	matches, err := eng.index.Search(query, opt.topK)
	if err != nil {
		return err
	}
	searchTime := time.Since(start)

	if opt.asJSON {
		return emitSearchJSON(eng, opt, matches, embedTime, searchTime, stdout)
	}
	emitSearchText(eng, opt, matches, embedTime, searchTime, stdout)
	return nil
}

// renderMatched applies the redaction contract to a retrieved surface.
//
// A value alias is among the most likely things to be retrieved by a query
// about a value — "India" finding the country field is the corpus working as
// designed — so this path is common, not exceptional.
func renderMatched(m retrieval.Match, reveal bool) string {
	text, sensitive := m.MatchedText()
	if sensitive && !reveal {
		return qferr.Placeholder
	}
	return text
}

func emitSearchText(eng *engine, opt options, matches []retrieval.Match, embedTime, searchTime time.Duration, stdout io.Writer) {
	fmt.Fprintf(stdout, "query:    %q\n", opt.search)
	fmt.Fprintf(stdout, "model:    %s\n", eng.index.Identity())
	fmt.Fprintf(stdout, "strategy: %s   corpus: %d entries, %d vectors",
		eng.index.Strategy(), eng.index.Len(), eng.index.VectorCount())
	if eng.loadedFrom != "" {
		fmt.Fprintf(stdout, "   (index loaded from %s)", eng.loadedFrom)
	}
	fmt.Fprintln(stdout)

	if opt.debug {
		fmt.Fprintf(stdout, "timing:   embed %s, search %s",
			embedTime.Round(time.Microsecond), searchTime.Round(time.Microsecond))
		if eng.buildTime > 0 {
			fmt.Fprintf(stdout, ", index build %s", eng.buildTime.Round(time.Millisecond))
		}
		fmt.Fprintln(stdout)
	}
	fmt.Fprintln(stdout)

	if len(matches) == 0 {
		fmt.Fprintln(stdout, "no matches")
		return
	}

	fmt.Fprintf(stdout, "%-4s  %-6s  %-12s  %-34s  %s\n", "rank", "score", "kind", "id", "matched")
	var redacted int
	for i, m := range matches {
		text := renderMatched(m, opt.debug)
		if _, sensitive := m.MatchedText(); sensitive && !opt.debug {
			redacted++
		}
		fmt.Fprintf(stdout, "%-4d  %-6.3f  %-12s  %-34s  %s\n",
			i+1, m.Score, m.Entry.Kind, truncate(corpus.DisplayID(m.Entry.ID, opt.debug), 34), text)
	}
	if redacted > 0 {
		fmt.Fprintf(stdout, "\n(%d match(es) matched a config value; re-run with --debug to see them)\n", redacted)
	}
}

func emitSearchJSON(eng *engine, opt options, matches []retrieval.Match, embedTime, searchTime time.Duration, stdout io.Writer) error {
	type jsonMatch struct {
		Rank     int     `json:"rank"`
		Score    float64 `json:"score"`
		Kind     string  `json:"kind"`
		ID       string  `json:"id"`
		Entity   string  `json:"entity,omitempty"`
		Ref      string  `json:"ref,omitempty"`
		Matched  string  `json:"matched"`
		Redacted bool    `json:"redacted,omitempty"`
	}

	out := struct {
		Query    string             `json:"query"`
		Model    embedding.Identity `json:"model"`
		Strategy string             `json:"strategy"`
		Entries  int                `json:"corpusEntries"`
		Vectors  int                `json:"corpusVectors"`
		EmbedMS  float64            `json:"embedMs"`
		SearchMS float64            `json:"searchMs"`
		Matches  []jsonMatch        `json:"matches"`
	}{
		Query:    opt.search,
		Model:    eng.index.Identity(),
		Strategy: string(eng.index.Strategy()),
		Entries:  eng.index.Len(),
		Vectors:  eng.index.VectorCount(),
		EmbedMS:  float64(embedTime.Microseconds()) / 1000,
		SearchMS: float64(searchTime.Microseconds()) / 1000,
	}

	for i, m := range matches {
		_, sensitive := m.MatchedText()
		out.Matches = append(out.Matches, jsonMatch{
			Rank:     i + 1,
			Score:    m.Score,
			Kind:     string(m.Entry.Kind),
			ID:       corpus.DisplayID(m.Entry.ID, opt.debug),
			Entity:   m.Entry.Entity,
			Ref:      m.Entry.Ref,
			Matched:  renderMatched(m, opt.debug),
			Redacted: sensitive && !opt.debug,
		})
	}

	enc, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return qferr.Wrap(qferr.CodeInternal, err, "search output could not be encoded")
	}
	fmt.Fprintln(stdout, string(enc))
	return nil
}

func truncate(s string, width int) string {
	if len(s) <= width {
		return s
	}
	if width <= 1 {
		return s[:width]
	}
	return s[:width-1] + "…"
}
