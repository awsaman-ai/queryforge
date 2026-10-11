package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"queryforge.local/v2experiment/internal/bench"
	"queryforge.local/v2experiment/internal/embedding"
	"queryforge.local/v2experiment/internal/pipeline"
	"queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
)

// A benchmark sentence. The set is fixed and versioned beside the harness so
// that two runs, two scales and two machines are answering the same questions.
type queryCase struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Input    string `json:"input"`
	// Expect is "sql" for a request that should compile, or a refusal code.
	// It is not an assertion the harness enforces -- accuracy is Phase 10's
	// subject -- but an unexpected outcome is recorded and reported, because
	// a latency measured on an accidental refusal is not a latency.
	Expect string `json:"expect"`
	// V1 marks a sentence a SINGLE-ENTITY schema can answer, and so one that
	// may fairly be sent to the production LLM engine, which has no joins.
	// Section 17: do not compare unsupported features against systems that
	// support them.
	V1 bool `json:"v1,omitempty"`
}

type querySet struct {
	Description string      `json:"description"`
	Cases       []queryCase `json:"cases"`
}

func loadQuerySet(path string) ([]queryCase, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var qs querySet
	if err := json.Unmarshal(raw, &qs); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if len(qs.Cases) == 0 {
		return nil, fmt.Errorf("%s: no cases", path)
	}
	return qs.Cases, nil
}

// runOne issues a single request and records everything the report may keep.
//
// The clock is fixed per request (time.Now), exactly as the CLI does it, so
// date windows resolve the same way they would in production.
func runOne(ctx context.Context, eng *benchEngine, c queryCase, keepStats bool) Outcome {
	start := time.Now()
	res, err := eng.pl.Run(ctx, c.Input, plan.Clock{Now: time.Now(), Location: time.UTC})
	wall := time.Since(start)

	out := Outcome{ID: c.ID, Category: c.Category, Wall: wall}
	if err != nil {
		out.Code = string(qferr.CodeOf(err))
	} else {
		sum := sha256.Sum256([]byte(res.Query.SQL))
		out.SQLHash = hex.EncodeToString(sum[:6])
	}
	if keepStats {
		if encoded, marshalErr := json.Marshal(res.Stats); marshalErr == nil {
			raw := json.RawMessage(encoded)
			out.Stats = &raw
		}
	}
	return out
}

// summarize folds a set of outcomes into a scenario's aggregate fields.
func summarize(s *Scenario, outcomes []Outcome, wall time.Duration) {
	var all, accepted, refused bench.Durations
	codes := map[string]int{}

	for _, o := range outcomes {
		all.Add(o.Wall)
		if o.Code == "" {
			accepted.Add(o.Wall)
			s.Succeeded++
			continue
		}
		refused.Add(o.Wall)
		codes[o.Code]++
		// INTERNAL_ERROR is a harness or engine failure, not a refusal: a
		// benchmark that quietly counts crashes as fast requests is worthless.
		if o.Code == string(qferr.CodeInternal) {
			s.Failed++
		} else {
			s.Refused++
		}
	}

	s.Requests = len(outcomes)
	s.Wall = wall
	s.RPS = bench.Throughput(len(outcomes), wall)
	s.Latency = all.Summarize()
	s.LatencyByClass = map[string]bench.Summary{
		"accepted": accepted.Summarize(),
		"refused":  refused.Summarize(),
	}
	if len(codes) > 0 {
		s.Codes = codes
	}

	// Stage attribution is taken from ACCEPTED requests only. A refusal stops
	// early by design, so folding one into the stage table would make binding
	// and compilation look free.
	var stages bench.StageTotals
	for _, o := range outcomes {
		if o.Code != "" || o.Stats == nil {
			continue
		}
		// Stats decodes back into its own type: the harness reads the
		// pipeline's telemetry, never a reconstruction of it.
		var st pipeline.Stats
		if err := json.Unmarshal(*o.Stats, &st); err != nil {
			continue
		}
		stages.Add(st)
	}
	if stages.Len() > 0 {
		s.Stages = map[string]bench.Summary{}
		for _, named := range stages.Named() {
			s.Stages[named.Stage] = named.Data.Summarize()
		}
		s.Stages["total_measured_in_pipeline"] = stages.Total.Summarize()
		s.StageShares = stages.ShareOfTotal()
	}
}

// runWarm is the core scenario: a warm process answering the query set
// sequentially, after a warmup that is measured and discarded.
func runWarm(ctx context.Context, eng *benchEngine, cases []queryCase, scale ScaleInfo, reps, warmup int) Scenario {
	s := Scenario{
		Name:        "warm",
		Description: "warm process, sequential requests, full pipeline",
		Scale:       scale,
		Concurrency: 1,
		Depth:       "full",
	}

	for i := 0; i < warmup; i++ {
		for _, c := range cases {
			_ = runOne(ctx, eng, c, false)
		}
	}

	s.WorkerPID = eng.workerPID()
	s.Before = bench.Sample(ctx, s.WorkerPID)
	split := eng.splitSnapshot()

	outcomes := make([]Outcome, 0, reps*len(cases))
	start := time.Now()
	for i := 0; i < reps; i++ {
		for _, c := range cases {
			outcomes = append(outcomes, runOne(ctx, eng, c, true))
		}
	}
	wall := time.Since(start)

	s.After = bench.Sample(ctx, s.WorkerPID)
	s.WorkerPIDStable = eng.workerPID() == s.WorkerPID && s.WorkerPID > 0
	summarize(&s, outcomes, wall)
	applySplit(&s, eng.splitSince(split))
	s.Outcomes = outcomes
	return s
}

// applySplit records the mean embedding and vector-search time per request.
// Requests refused before retrieval (the input boundary, an over-long input)
// never reach the retriever, so the mean is taken over the requests that did.
func applySplit(s *Scenario, split splitTimings) {
	if split.EmbedCalls > 0 {
		s.EmbedPerRequest = split.Embed / time.Duration(split.EmbedCalls)
	}
	if split.SearchCalls > 0 {
		s.SearchPerRequest = split.Search / time.Duration(split.SearchCalls)
		s.SearchesPerRequest = float64(split.SearchOps) / float64(split.SearchCalls)
	}
}

// runDepth measures the three comparison points of section 17: retrieval
// alone, retrieval plus ranking, and the full compiler. The differences are
// where the architecture's cost actually sits.
func runDepth(ctx context.Context, eng *benchEngine, cases []queryCase, scale ScaleInfo, depth string, reps int) Scenario {
	s := Scenario{
		Name:        "depth",
		Description: "one layer set, sequential requests",
		Scale:       scale,
		Concurrency: 1,
		Depth:       depth,
	}
	s.WorkerPID = eng.workerPID()
	s.Before = bench.Sample(ctx, s.WorkerPID)
	split := eng.splitSnapshot()

	outcomes := make([]Outcome, 0, reps*len(cases))
	start := time.Now()
	for i := 0; i < reps; i++ {
		for _, c := range cases {
			switch depth {
			case "retrieval":
				t := time.Now()
				err := eng.retrieveOnly(ctx, c.Input)
				o := Outcome{ID: c.ID, Category: c.Category, Wall: time.Since(t)}
				if err != nil {
					o.Code = string(qferr.CodeOf(err))
				}
				outcomes = append(outcomes, o)
			case "retrieval+ranking":
				t := time.Now()
				_, _, err := eng.pl.Rank(ctx, c.Input)
				o := Outcome{ID: c.ID, Category: c.Category, Wall: time.Since(t)}
				if err != nil {
					o.Code = string(qferr.CodeOf(err))
				}
				outcomes = append(outcomes, o)
			default:
				outcomes = append(outcomes, runOne(ctx, eng, c, true))
			}
		}
	}
	wall := time.Since(start)

	s.After = bench.Sample(ctx, s.WorkerPID)
	s.WorkerPIDStable = eng.workerPID() == s.WorkerPID && s.WorkerPID > 0
	summarize(&s, outcomes, wall)
	applySplit(&s, eng.splitSince(split))
	// Keep the raw samples: every published number must be recomputable from
	// them by `--suite verify`, including this scenario's.
	s.Outcomes = outcomes
	return s
}

// runConcurrency drives N goroutines against ONE warm engine.
//
// Every goroutine shares the single embedding worker, whose protocol is
// serialised by a mutex (internal/embedding.Worker). That is the architecture
// as built, and measuring it is the point: the queueing this produces is a
// finding, not a flaw in the harness.
func runConcurrency(ctx context.Context, eng *benchEngine, cases []queryCase, scale ScaleInfo, level, total int) Scenario {
	s := Scenario{
		Name:        "concurrency",
		Description: "warm process, concurrent requests against one shared engine",
		Scale:       scale,
		Concurrency: level,
		Depth:       "full",
	}
	s.WorkerPID = eng.workerPID()
	s.Before = bench.Sample(ctx, s.WorkerPID)
	split := eng.splitSnapshot()

	var next atomic.Int64
	results := make([]Outcome, total)
	var wg sync.WaitGroup
	start := time.Now()
	for g := 0; g < level; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				i := int(next.Add(1)) - 1
				if i >= total {
					return
				}
				results[i] = runOne(ctx, eng, cases[i%len(cases)], false)
			}
		}()
	}
	wg.Wait()
	wall := time.Since(start)

	s.After = bench.Sample(ctx, s.WorkerPID)
	s.WorkerPIDStable = eng.workerPID() == s.WorkerPID && s.WorkerPID > 0
	summarize(&s, results, wall)
	applySplit(&s, eng.splitSince(split))
	s.Outcomes = results
	return s
}

// runCold measures the cold path itself: a fresh process-equivalent assembly,
// including a fresh model load, repeated so the number has a distribution
// rather than an anecdote.
//
// Each repetition starts a NEW embedding worker and closes it, which is the
// expensive part and exactly what the warm architecture exists to avoid
// paying per request.
func runCold(ctx context.Context, opt engineOptions, scaleLabel string, reps int, fromCache bool) (Scenario, embedding.Identity, string, error) {
	s := Scenario{
		Name:        "cold",
		Description: "assembly from nothing to READY, repeated",
		Concurrency: 1,
	}
	if fromCache {
		s.Description = "assembly from nothing to READY, index loaded from cache"
		s.Note = "index loaded from disk; the model is still loaded in full"
	}

	var ready, modelLoad, indexStep bench.Durations
	var last ColdTimings
	var identity embedding.Identity
	var strategy string
	for i := 0; i < reps; i++ {
		eng, err := openEngine(ctx, opt)
		if err != nil {
			return s, identity, strategy, err
		}
		last = eng.cold
		ready.Add(eng.cold.Ready)
		modelLoad.Add(eng.cold.ModelLoad)
		if eng.cold.FromCache {
			indexStep.Add(eng.cold.IndexLoad)
		} else {
			indexStep.Add(eng.cold.IndexBuild)
		}
		if i == reps-1 {
			s.Scale = eng.scale(scaleLabel, opt.configPath)
			identity = eng.index.Identity()
			strategy = string(eng.index.Strategy())
			s.After = bench.Sample(ctx, eng.workerPID())
			s.WorkerPID = eng.workerPID()
			s.WorkerPIDStable = true // one worker for this engine's lifetime
		}
		if err := eng.Close(); err != nil {
			return s, identity, strategy, err
		}
	}

	s.Requests = reps
	s.Succeeded = reps
	s.Latency = ready.Summarize()
	s.Cold = &last
	s.Stages = map[string]bench.Summary{
		"model_load": modelLoad.Summarize(),
		"index":      indexStep.Summarize(),
		"ready":      ready.Summarize(),
	}
	return s, identity, strategy, nil
}
