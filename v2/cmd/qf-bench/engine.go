package main

import (
	"context"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"queryforge.local/v2experiment/internal/bench"
	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/embedding"
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/pipeline"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/retrieval"
)

// benchEngine is the warm process the brief's section 17 diagram describes:
// config, corpus, model and index assembled ONCE, then reused by every
// request. It mirrors cmd/qf-v2's own engine step for step -- deliberately,
// because a benchmark of a different assembly order would measure a system
// nobody runs.
//
// The one addition is timing: each step of the cold path is recorded, so the
// report can say where a cold start goes instead of quoting one number.
type benchEngine struct {
	registry *config.SchemaRegistry
	entries  []corpus.Entry
	provider embedding.Provider
	// worker is the same process as provider, kept typed so the harness can
	// read its pid: provider is wrapped for batching and a type assertion
	// through the wrapper would silently report "no worker".
	worker *embedding.Worker
	index  *retrieval.Index
	pl     *pipeline.Pipeline
	limits nlp.Options
	topK   int

	cold ColdTimings

	// Section 17 asks for "embedding latency" and "vector retrieval latency"
	// as separate metrics, but pipeline.Stats reports one RetrieveMs covering
	// both -- and the answer to "is the vector lookup a small fraction of
	// total latency?" depends entirely on splitting them. The retriever adds
	// to these; scenarios read the deltas. Atomics because the concurrency
	// suite drives one engine from many goroutines.
	embedNanos  atomic.Int64
	searchNanos atomic.Int64
	embedCalls  atomic.Int64
	searchCalls atomic.Int64
	// searchOps counts INDIVIDUAL index searches (one per text span), which is
	// what multiplies the scan cost: a request with eight spans scans the
	// whole index eight times.
	searchOps atomic.Int64
}

// splitTimings is one scenario's embedding-vs-search accounting.
type splitTimings struct {
	Embed       time.Duration
	Search      time.Duration
	EmbedCalls  int64
	SearchCalls int64
	SearchOps   int64
}

// splitSnapshot reads the running totals; splitSince returns what accumulated
// after one.
func (e *benchEngine) splitSnapshot() splitTimings {
	return splitTimings{
		Embed:       time.Duration(e.embedNanos.Load()),
		Search:      time.Duration(e.searchNanos.Load()),
		EmbedCalls:  e.embedCalls.Load(),
		SearchCalls: e.searchCalls.Load(),
		SearchOps:   e.searchOps.Load(),
	}
}

func (e *benchEngine) splitSince(before splitTimings) splitTimings {
	now := e.splitSnapshot()
	return splitTimings{
		Embed:       now.Embed - before.Embed,
		Search:      now.Search - before.Search,
		EmbedCalls:  now.EmbedCalls - before.EmbedCalls,
		SearchCalls: now.SearchCalls - before.SearchCalls,
		SearchOps:   now.SearchOps - before.SearchOps,
	}
}

type engineOptions struct {
	configPath   string
	indexPath    string // optional cache; "" always embeds
	rebuildIndex bool
	topK         int
	workerScript string
	model        string
	stderr       io.Writer
	// deterministic swaps the model for the hashing stand-in. It exists for
	// MECHANISM checks only -- the harness's own concurrency tests, and a
	// smoke run without weights. Any number it produces is stamped "not a
	// research result" in the report, because the vectors carry no meaning.
	deterministic bool
}

// openEngine performs the cold path and returns a warm engine.
func openEngine(ctx context.Context, opt engineOptions) (*benchEngine, error) {
	if opt.topK <= 0 {
		opt.topK = 10
	}
	if opt.stderr == nil {
		opt.stderr = io.Discard
	}

	var cold ColdTimings
	wall := time.Now()

	t := time.Now()
	cfg, err := config.Load(opt.configPath)
	if err != nil {
		return nil, err
	}
	cold.ConfigLoad = time.Since(t)

	t = time.Now()
	registry := config.NewSchemaRegistry(cfg)
	entries := corpus.Build(registry)
	cold.CorpusBuild = time.Since(t)
	if len(entries) == 0 {
		return nil, qferr.New(qferr.CodeConfigError, "config produced an empty semantic corpus")
	}

	eng := &benchEngine{registry: registry, entries: entries, topK: opt.topK}

	// The model is loaded even when a cached index is used: warm requests
	// embed their spans, so a process without a model cannot serve one. This
	// is also why "cold start" and "index build" are separate numbers.
	t = time.Now()
	var provider *embedding.Worker
	var raw embedding.Provider
	if opt.deterministic {
		raw = embedding.NewDeterministic(0)
	} else {
		provider, err = embedding.NewWorker(ctx, embedding.WorkerOptions{
			Script: opt.workerScript,
			Model:  opt.model,
			Stderr: opt.stderr,
		})
		if err != nil {
			return nil, err
		}
		raw = provider
	}
	cold.ModelLoad = time.Since(t)
	// The worker refuses a batch over 2,048 texts, and retrieval.Build embeds
	// the whole corpus in one call: past the smallest scale, the index cannot
	// be built without chunking. See internal/bench.Batched -- the vectors are
	// identical either way, since each text is embedded independently.
	batched := bench.NewBatched(raw)
	eng.provider = batched
	eng.worker = provider

	cached := opt.indexPath != "" && !opt.rebuildIndex
	if cached {
		if _, statErr := os.Stat(opt.indexPath); statErr == nil {
			t = time.Now()
			index, loadErr := retrieval.Load(opt.indexPath, entries)
			if loadErr != nil {
				_ = batched.Close()
				return nil, loadErr
			}
			cold.IndexLoad = time.Since(t)
			cold.FromCache = true
			eng.index = index
		}
	}
	if eng.index == nil {
		t = time.Now()
		index, buildErr := retrieval.Build(ctx, batched, entries, retrieval.StrategySurface)
		if buildErr != nil {
			_ = batched.Close()
			return nil, buildErr
		}
		cold.IndexBuild = time.Since(t)
		eng.index = index
		if opt.indexPath != "" {
			if err := index.Save(opt.indexPath); err != nil {
				_ = batched.Close()
				return nil, err
			}
		}
	}

	t = time.Now()
	policy := pipeline.PolicyFor(registry)
	eng.limits = nlp.DefaultOptions
	pl, err := pipeline.New(registry, benchRetriever{eng: eng}, pipeline.Options{
		Policy: &policy, Limits: eng.limits, CandidateDepth: opt.topK,
	})
	if err != nil {
		_ = batched.Close()
		return nil, err
	}
	cold.PipelineNew = time.Since(t)
	cold.Ready = time.Since(wall)

	eng.pl = pl
	eng.cold = cold
	return eng, nil
}

func (e *benchEngine) Close() error {
	if e.provider != nil {
		return e.provider.Close()
	}
	return nil
}

// workerPID is the embedding worker's process id, or 0 when the provider is
// not an out-of-process worker. The harness records it before and after every
// scenario: an unchanged pid is the evidence that no model was respawned per
// request.
func (e *benchEngine) workerPID() int {
	if e.worker == nil {
		return 0
	}
	return e.worker.PID()
}

func (e *benchEngine) scale(label, path string) ScaleInfo {
	return ScaleInfo{
		Label:          label,
		ConfigPath:     path,
		CorpusEntries:  e.index.Len(),
		IndexVectors:   e.index.VectorCount(),
		ConfigEntities: len(e.registry.Config().Entities),
		ConfigFields:   len(e.registry.Config().Fields),
	}
}

// benchRetriever is cmd/qf-v2's engineRetriever, duplicated rather than
// exported: the CLI's copy is closed Phase 7 code, and a benchmark is not a
// reason to change a shipped signature.
type benchRetriever struct{ eng *benchEngine }

func (r benchRetriever) Retrieve(ctx context.Context, texts []string) ([][]retrieval.Match, error) {
	start := time.Now()
	vectors, err := r.eng.provider.Embed(ctx, texts)
	r.eng.embedNanos.Add(int64(time.Since(start)))
	r.eng.embedCalls.Add(1)
	if err != nil {
		return nil, err
	}
	if len(vectors) != len(texts) {
		return nil, qferr.New(qferr.CodeInternal,
			"embedding provider returned %d vectors for %d spans", len(vectors), len(texts))
	}
	out := make([][]retrieval.Match, len(texts))
	start = time.Now()
	for k, v := range vectors {
		m, err := r.eng.index.Search(v, r.eng.topK)
		if err != nil {
			return nil, err
		}
		out[k] = m
	}
	r.eng.searchNanos.Add(int64(time.Since(start)))
	r.eng.searchCalls.Add(1)
	r.eng.searchOps.Add(int64(len(vectors)))
	return out, nil
}

// retrieveOnly runs extraction plus embedding plus vector search and stops --
// the first of the three comparison depths in section 17. It reproduces the
// pipeline's own span selection so the two depths are measuring the same work.
func (e *benchEngine) retrieveOnly(ctx context.Context, sentence string) error {
	a := nlp.Analyze(sentence, e.limits)
	var texts []string
	for _, s := range a.Spans {
		if s.Kind == nlp.SpanText {
			texts = append(texts, s.Norm)
		}
	}
	if len(texts) == 0 {
		return nil
	}
	_, err := benchRetriever{eng: e}.Retrieve(ctx, texts)
	return err
}

// cpuModel and memoryGB fill the hardware half of the report header. Both are
// best-effort: an empty field is better than a wrong one, and the report says
// "unknown" rather than inventing a machine.
func cpuModel() string {
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "machdep.cpu.brand_string").Output(); err == nil {
			return strings.TrimSpace(string(out))
		}
	case "linux":
		if raw, err := os.ReadFile("/proc/cpuinfo"); err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				if name, value, ok := strings.Cut(line, ":"); ok && strings.TrimSpace(name) == "model name" {
					return strings.TrimSpace(value)
				}
			}
		}
	}
	return ""
}

func memoryGB() int {
	switch runtime.GOOS {
	case "darwin":
		if out, err := exec.Command("sysctl", "-n", "hw.memsize").Output(); err == nil {
			if bytes, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64); err == nil {
				return int(bytes / (1 << 30))
			}
		}
	case "linux":
		if raw, err := os.ReadFile("/proc/meminfo"); err == nil {
			for _, line := range strings.Split(string(raw), "\n") {
				if strings.HasPrefix(line, "MemTotal:") {
					fields := strings.Fields(line)
					if len(fields) >= 2 {
						if kb, err := strconv.ParseInt(fields[1], 10, 64); err == nil {
							return int(kb / (1 << 20))
						}
					}
				}
			}
		}
	}
	return 0
}
