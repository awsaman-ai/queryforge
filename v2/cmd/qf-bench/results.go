package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"time"

	"queryforge.local/v2experiment/internal/bench"
	"queryforge.local/v2experiment/internal/embedding"
)

// Everything the harness measures lands in one JSON document before any
// markdown is written. Two reasons, both from section 25: a report should be
// re-renderable without re-running a 40-minute benchmark, and a reader should
// be able to check the arithmetic against the raw samples rather than trust
// the prose.
//
// The document carries no user text, no config values and no generated SQL --
// only identifiers, counts, timings and a hash. Benchmark sentences are the
// project's own, but keeping the artefact free of query text means it stays
// publishable under the same rules as pipeline.Stats (decision 013).

// Run is one invocation of the harness: an environment header plus scenarios.
type Run struct {
	Version     int         `json:"version"`
	StartedAt   time.Time   `json:"started_at"`
	FinishedAt  time.Time   `json:"finished_at"`
	Environment Environment `json:"environment"`
	Scenarios   []Scenario  `json:"scenarios"`
	// Notes record anything that qualifies a number: a skipped level, a
	// machine that was not idle, a suite run separately.
	Notes []string `json:"notes,omitempty"`
}

// Environment is the "hardware, OS, model, model version, schema size, test
// count, concurrency" header the brief's manual gate requires. Schema size,
// test count and concurrency vary per scenario and live there.
type Environment struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	CPUs      int    `json:"cpus"`
	GoVersion string `json:"go_version"`
	CPUModel  string `json:"cpu_model,omitempty"`
	MemoryGB  int    `json:"memory_gb,omitempty"`

	Model         string `json:"embedding_model"`
	ModelRevision string `json:"embedding_revision"`
	Dim           int    `json:"embedding_dim"`
	Semantic      bool   `json:"embedding_semantic"`
	Strategy      string `json:"index_strategy"`
}

// WithIdentity fills the model half of the header from the provider itself,
// so a report can never name a model that did not produce its vectors.
func (e Environment) WithIdentity(id embedding.Identity, strategy string) Environment {
	e.Model = id.Model
	e.ModelRevision = id.Revision
	e.Dim = id.Dim
	e.Semantic = id.Semantic
	e.Strategy = strategy
	return e
}

// LocalEnvironment reads what the process can know about its own machine.
func LocalEnvironment() Environment {
	return Environment{
		OS:        runtime.GOOS,
		Arch:      runtime.GOARCH,
		CPUs:      runtime.NumCPU(),
		GoVersion: runtime.Version(),
		CPUModel:  cpuModel(),
		MemoryGB:  memoryGB(),
	}
}

// ScaleInfo pins which schema a scenario ran against. Both counts are carried
// because "schema entries" reads two ways: index rows and configured elements.
type ScaleInfo struct {
	Label          string `json:"label"`
	ConfigPath     string `json:"config_path"`
	CorpusEntries  int    `json:"corpus_entries"`
	IndexVectors   int    `json:"index_vectors"`
	ConfigEntities int    `json:"config_entities"`
	ConfigFields   int    `json:"config_fields"`
}

// ColdTimings breaks a cold start into the steps of the brief's warm
// architecture diagram: load config, build embeddings, load model, build
// index, READY.
type ColdTimings struct {
	ConfigLoad  time.Duration `json:"config_load"`
	CorpusBuild time.Duration `json:"corpus_build"`
	ModelLoad   time.Duration `json:"model_load"`
	IndexBuild  time.Duration `json:"index_build"`
	IndexLoad   time.Duration `json:"index_load"`
	PipelineNew time.Duration `json:"pipeline_new"`
	Ready       time.Duration `json:"ready_total"`
	// FromCache is true when the index was loaded from disk instead of
	// embedded. The two are different products and are never averaged.
	FromCache bool `json:"index_from_cache"`
}

// Outcome is one request's result, stripped to what a benchmark may keep.
type Outcome struct {
	ID       string        `json:"id"`
	Category string        `json:"category"`
	Wall     time.Duration `json:"wall"`
	// Code is empty on success, else the pipeline's refusal code. A refusal is
	// a legitimate, measured outcome -- refusals are faster than compiles, so
	// mixing them into one latency number would flatter the result.
	Code string `json:"code,omitempty"`
	// SQLHash identifies the produced query without reproducing it, so a
	// later scale can be proved to have answered identically.
	SQLHash string `json:"sql_hash,omitempty"`
	// Stats is the pipeline's own redacted telemetry for this request.
	Stats *json.RawMessage `json:"stats,omitempty"`
}

// Scenario is one measured cell of the matrix.
type Scenario struct {
	Name        string    `json:"name"`
	Description string    `json:"description"`
	Scale       ScaleInfo `json:"scale"`
	Concurrency int       `json:"concurrency"`
	// Depth names which layers ran: "retrieval", "retrieval+ranking" or
	// "full" (section 17's three comparison points).
	Depth string `json:"depth,omitempty"`

	Requests  int           `json:"requests"`
	Succeeded int           `json:"succeeded"`
	Refused   int           `json:"refused"`
	Failed    int           `json:"failed"`
	Wall      time.Duration `json:"wall"`
	RPS       float64       `json:"requests_per_second"`

	// EmbedPerRequest and SearchPerRequest split the retrieval stage into the
	// model round trip and the vector scan -- section 17 asks for both, and
	// pipeline.Stats reports only their sum. They are means over the
	// scenario, not distributions: the split is accumulated by the retriever.
	EmbedPerRequest  time.Duration `json:"embed_per_request"`
	SearchPerRequest time.Duration `json:"vector_search_per_request"`
	// SearchesPerRequest is how many times the index is scanned for one
	// sentence: once per text span. It is the multiplier on the scan cost.
	SearchesPerRequest float64 `json:"searches_per_request"`

	Latency        bench.Summary            `json:"latency"`
	LatencyByClass map[string]bench.Summary `json:"latency_by_class,omitempty"`
	Stages         map[string]bench.Summary `json:"stages,omitempty"`
	StageShares    map[string]float64       `json:"stage_shares,omitempty"`

	Before bench.Resources `json:"resources_before"`
	After  bench.Resources `json:"resources_after"`

	Cold *ColdTimings `json:"cold,omitempty"`

	// WorkerPIDStable proves the warm-architecture requirement: one worker
	// process served every request, rather than one being spawned per
	// request (the brief's prohibited pattern).
	WorkerPIDStable bool `json:"worker_pid_stable"`
	WorkerPID       int  `json:"worker_pid"`

	Codes    map[string]int `json:"refusal_codes,omitempty"`
	Outcomes []Outcome      `json:"outcomes,omitempty"`
	Note     string         `json:"note,omitempty"`
}

// Save writes the run as indented JSON, creating the directory if needed.
func (r *Run) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	encoded, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(encoded, '\n'), 0o644)
}

// LoadRun reads a run back, so the report can be re-rendered without
// re-measuring.
func LoadRun(path string) (*Run, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var r Run
	if err := json.Unmarshal(raw, &r); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &r, nil
}

// Merge folds another run's scenarios into this one, so suites can be run
// separately (the 10k build alone takes minutes) and reported together.
// Scenarios with the same identity are replaced, newest wins.
func (r *Run) Merge(other *Run) {
	if r.Environment.Model == "" {
		r.Environment = other.Environment
	}
	if r.StartedAt.IsZero() || (!other.StartedAt.IsZero() && other.StartedAt.Before(r.StartedAt)) {
		r.StartedAt = other.StartedAt
	}
	if other.FinishedAt.After(r.FinishedAt) {
		r.FinishedAt = other.FinishedAt
	}
	for _, s := range other.Scenarios {
		r.replace(s)
	}
	// Notes are deduplicated: every suite run carries the same --note, and a
	// report repeating one caveat four times reads like four caveats.
	for _, n := range other.Notes {
		seen := false
		for _, existing := range r.Notes {
			if existing == n {
				seen = true
				break
			}
		}
		if !seen {
			r.Notes = append(r.Notes, n)
		}
	}
	sort.SliceStable(r.Scenarios, func(i, j int) bool {
		if r.Scenarios[i].Name != r.Scenarios[j].Name {
			return r.Scenarios[i].Name < r.Scenarios[j].Name
		}
		if r.Scenarios[i].Scale.CorpusEntries != r.Scenarios[j].Scale.CorpusEntries {
			return r.Scenarios[i].Scale.CorpusEntries < r.Scenarios[j].Scale.CorpusEntries
		}
		return r.Scenarios[i].Concurrency < r.Scenarios[j].Concurrency
	})
}

func (r *Run) replace(s Scenario) {
	for i, existing := range r.Scenarios {
		if existing.Name == s.Name && existing.Depth == s.Depth &&
			existing.Scale.Label == s.Scale.Label && existing.Concurrency == s.Concurrency {
			r.Scenarios[i] = s
			return
		}
	}
	r.Scenarios = append(r.Scenarios, s)
}

// Select returns the scenarios of one name, in matrix order.
func (r *Run) Select(name string) []Scenario {
	var out []Scenario
	for _, s := range r.Scenarios {
		if s.Name == name {
			out = append(out, s)
		}
	}
	return out
}
