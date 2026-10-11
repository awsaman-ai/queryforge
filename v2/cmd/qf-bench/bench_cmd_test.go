package main

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"queryforge.local/v2experiment/internal/bench"
	"queryforge.local/v2experiment/internal/embedding"
	"queryforge.local/v2experiment/internal/pipeline"
)

// These tests cover everything in the harness that does NOT need a model: the
// arithmetic, the result document, the report. A benchmark whose reporting is
// untested can publish a wrong number as easily as a right one, and the
// measured suites cannot run in a plain checkout (no venv, no weights).

func TestParseInts(t *testing.T) {
	got, err := parseInts("1, 10 ,50,100")
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 10, 50, 100}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v, want %v", got, want)
		}
	}
	for _, bad := range []string{"", "0", "-5", "ten", ","} {
		if _, err := parseInts(bad); err == nil {
			t.Errorf("parseInts(%q) should fail", bad)
		}
	}
}

func TestScaleLabel(t *testing.T) {
	for in, want := range map[int]string{100: "100", 1000: "1k", 10000: "10k", 1500: "1500"} {
		if got := scaleLabel(in); got != want {
			t.Errorf("scaleLabel(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanDuration(t *testing.T) {
	for in, want := range map[time.Duration]string{
		0:                       "-",
		500 * time.Nanosecond:   "500ns",
		1500 * time.Nanosecond:  "1.5us",
		2500 * time.Microsecond: "2.5ms",
		1500 * time.Millisecond: "1.50s",
		95 * time.Second:        "1m35s",
	} {
		if got := humanDuration(in); got != want {
			t.Errorf("humanDuration(%v) = %q, want %q", in, got, want)
		}
	}
}

func TestHumanBytes(t *testing.T) {
	for in, want := range map[int64]string{
		0: "-", 2048: "2 KB", 5 << 20: "5.0 MB", 3 << 30: "3.00 GB",
	} {
		if got := humanBytes(in); got != want {
			t.Errorf("humanBytes(%d) = %q, want %q", in, got, want)
		}
	}
}

// The benchmark set is a committed artefact; a duplicate id would silently
// overwrite a case in the cross-scale comparison, which is keyed by id.
func TestBenchmarkQuerySetIsWellFormed(t *testing.T) {
	cases, err := loadQuerySet(filepath.Join("..", "..", "benchmarks", "queries.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(cases) < 20 {
		t.Errorf("only %d cases; section 17 wants a representative mix", len(cases))
	}
	seenID := map[string]bool{}
	seenInput := map[string]bool{}
	v1, refusals := 0, 0
	for _, c := range cases {
		if seenID[c.ID] {
			t.Errorf("duplicate id %q", c.ID)
		}
		seenID[c.ID] = true
		if seenInput[c.Input] {
			t.Errorf("duplicate sentence %q", c.Input)
		}
		seenInput[c.Input] = true
		if strings.TrimSpace(c.Input) == "" || c.Category == "" || c.Expect == "" {
			t.Errorf("case %q is incomplete: %+v", c.ID, c)
		}
		if c.V1 {
			v1++
		}
		if c.Expect != "sql" {
			refusals++
		}
	}
	if v1 == 0 {
		t.Error("no case is marked comparable to v1; the baseline suite cannot run")
	}
	if refusals == 0 {
		t.Error("no refusals in the set; refusals are cheaper than compiles and belong in the mix")
	}
}

func TestSummarizeSplitsOutcomes(t *testing.T) {
	stats := func(total float64) *json.RawMessage {
		encoded, _ := json.Marshal(pipeline.Stats{
			ExtractMs: 0.1, RetrieveMs: total - 0.4, RankMs: 0.2, BindMs: 0.05,
			PlanMs: 0.02, CompileMs: 0.03, TotalMs: total,
		})
		raw := json.RawMessage(encoded)
		return &raw
	}
	outcomes := []Outcome{
		{ID: "A", Wall: 10 * time.Millisecond, SQLHash: "aaa", Stats: stats(10)},
		{ID: "B", Wall: 20 * time.Millisecond, SQLHash: "bbb", Stats: stats(20)},
		{ID: "C", Wall: 2 * time.Millisecond, Code: "AMBIGUOUS_MATCH"},
		{ID: "D", Wall: 1 * time.Millisecond, Code: "INTERNAL_ERROR"},
	}
	var s Scenario
	summarize(&s, outcomes, 33*time.Millisecond)

	if s.Requests != 4 || s.Succeeded != 2 || s.Refused != 1 || s.Failed != 1 {
		t.Errorf("counts wrong: %+v", s)
	}
	if s.Codes["AMBIGUOUS_MATCH"] != 1 || s.Codes["INTERNAL_ERROR"] != 1 {
		t.Errorf("codes wrong: %v", s.Codes)
	}
	if got := s.LatencyByClass["accepted"].N; got != 2 {
		t.Errorf("accepted n = %d, want 2", got)
	}
	// Stage attribution must come only from accepted requests: two samples,
	// not four.
	if got := s.Stages["retrieve"].N; got != 2 {
		t.Errorf("stage samples = %d, want 2 (accepted only)", got)
	}
	if s.StageShares["retrieve"] < 0.9 {
		t.Errorf("retrieve should dominate the share, got %v", s.StageShares)
	}
	if s.RPS <= 0 {
		t.Error("throughput should be positive")
	}
}

// ------------------------------------------------------------------ results

func sampleRun() *Run {
	mk := func(p50, p95, p99 time.Duration, n int) bench.Summary {
		return bench.Summary{N: n, Min: p50 / 2, Max: p99, Mean: p50, P50: p50, P95: p95, P99: p99}
	}
	scale := func(label string, entries int) ScaleInfo {
		return ScaleInfo{Label: label, CorpusEntries: entries, IndexVectors: entries * 3,
			ConfigEntities: entries / 10, ConfigFields: entries / 2}
	}
	warm100 := Scenario{
		Name: "warm", Scale: scale("100", 100), Concurrency: 1, Depth: "full",
		Requests: 100, Succeeded: 80, Refused: 20, WorkerPIDStable: true, WorkerPID: 42,
		Latency: mk(6*time.Millisecond, 12*time.Millisecond, 15*time.Millisecond, 100),
		LatencyByClass: map[string]bench.Summary{
			"accepted": mk(7*time.Millisecond, 13*time.Millisecond, 16*time.Millisecond, 80),
			"refused":  mk(2*time.Millisecond, 3*time.Millisecond, 4*time.Millisecond, 20),
		},
		Stages: map[string]bench.Summary{
			"retrieve": mk(6*time.Millisecond, 11*time.Millisecond, 14*time.Millisecond, 80),
			"rank":     mk(100*time.Microsecond, 200*time.Microsecond, 300*time.Microsecond, 80),
		},
		StageShares: map[string]float64{"retrieve": 0.96, "rank": 0.02},
		Outcomes: []Outcome{
			{ID: "S01", SQLHash: "aaa"}, {ID: "V01", SQLHash: "bbb"}, {ID: "Z01", Code: "SECURITY_REJECTED"},
		},
		After: bench.Resources{GoHeapBytes: 16 << 20, GoPeakRSSBytes: 40 << 20, WorkerRSSBytes: 440 << 20},
	}
	warm10k := warm100
	warm10k.Scale = scale("10k", 10000)
	warm10k.Latency = mk(60*time.Millisecond, 200*time.Millisecond, 400*time.Millisecond, 100)
	warm10k.Outcomes = []Outcome{
		{ID: "S01", SQLHash: "aaa"}, {ID: "V01", SQLHash: "CHANGED"}, {ID: "Z01", Code: "SECURITY_REJECTED"},
	}
	return &Run{
		Version: 1, StartedAt: time.Now().Add(-time.Hour), FinishedAt: time.Now(),
		Environment: LocalEnvironment().WithIdentity(identityStub(), "surface"),
		Scenarios:   []Scenario{warm100, warm10k},
	}
}

func TestRunRoundTripsThroughDisk(t *testing.T) {
	path := filepath.Join(t.TempDir(), "results.json")
	original := sampleRun()
	if err := original.Save(path); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadRun(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Scenarios) != len(original.Scenarios) {
		t.Fatalf("got %d scenarios, want %d", len(loaded.Scenarios), len(original.Scenarios))
	}
	if loaded.Scenarios[0].Latency.P50 != original.Scenarios[0].Latency.P50 {
		t.Error("latency did not survive the round trip")
	}
	if loaded.Environment.Model != original.Environment.Model {
		t.Error("model identity did not survive the round trip")
	}
}

func TestMergeReplacesMatchingScenarios(t *testing.T) {
	base := sampleRun()
	fresh := &Run{Version: 1, StartedAt: time.Now(), FinishedAt: time.Now()}
	replacement := base.Scenarios[0]
	replacement.Requests = 999
	fresh.Scenarios = []Scenario{replacement, {Name: "cold", Scale: ScaleInfo{Label: "100"}}}

	base.Merge(fresh)

	if len(base.Scenarios) != 3 {
		t.Fatalf("got %d scenarios, want 3 (one replaced, one added)", len(base.Scenarios))
	}
	for _, s := range base.Scenarios {
		if s.Name == "warm" && s.Scale.Label == "100" && s.Requests != 999 {
			t.Errorf("the matching scenario was not replaced: %+v", s)
		}
	}
}

// ------------------------------------------------------------------- report

func TestRenderCoversEverySection(t *testing.T) {
	out := Render(sampleRun())
	for _, want := range []string{
		"# Phase 9 performance report",
		"## Environment",
		"## Warm architecture (the prohibited pattern)",
		"## Warm latency, sequential",
		"## Where the time goes",
		"## CPU and memory",
		"## Does scale change the answer?",
		"## Method",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("report is missing %q", want)
		}
	}
}

func TestRenderMarksMissedBudgets(t *testing.T) {
	out := Render(sampleRun())
	// The 100-entry row meets every budget; the 10k row misses p95 and p99.
	if !strings.Contains(out, "met (6.0ms)") {
		t.Error("a met p50 budget should be shown as met")
	}
	if !strings.Contains(out, "**missed** (200.0ms)") || !strings.Contains(out, "**missed** (400.0ms)") {
		t.Error("a missed budget must be marked, not omitted")
	}
}

// A changed answer at a larger scale is the one result the report must never
// bury: it means a bigger index moved a reading.
func TestRenderReportsChangedAnswersAtScale(t *testing.T) {
	out := Render(sampleRun())
	if !strings.Contains(out, "1 (V01)") {
		t.Errorf("the changed case should be named in the invariance table:\n%s", out)
	}
}

func TestRenderWarnsAboutANonSemanticProvider(t *testing.T) {
	r := sampleRun()
	r.Environment.Semantic = false
	if !strings.Contains(Render(r), "Not a research result") {
		t.Error("a deterministic-provider run must be flagged in the report")
	}
}

// identityStub is the pinned Phase 2 model identity, used by the report tests
// so they need neither a worker nor weights.
func identityStub() embedding.Identity {
	return embedding.Identity{
		Model:      "sentence-transformers/all-MiniLM-L6-v2",
		Revision:   "1110a243fdf4706b3f48f1d95db1a4f5529b4d41",
		Dim:        384,
		Normalized: true,
		Semantic:   true,
	}
}

func TestMergeDeduplicatesNotes(t *testing.T) {
	base := &Run{Notes: []string{"idle laptop"}}
	base.Merge(&Run{Notes: []string{"idle laptop", "second caveat"}})
	if len(base.Notes) != 2 {
		t.Errorf("notes = %v, want the repeat folded away", base.Notes)
	}
}

// The concurrency suite drives ONE pipeline and one shared engine from many
// goroutines, and accumulates timings into atomics. Run it under -race with
// the hashing stand-in, so the harness's own concurrency is tested without
// needing the model or the weights.
func TestConcurrencySuiteIsRaceFree(t *testing.T) {
	if testing.Short() {
		t.Skip("builds an index")
	}
	ctx := context.Background()
	eng, err := openEngine(ctx, engineOptions{
		configPath:    filepath.Join("..", "..", "testdata", "configs", "commerce.json"),
		topK:          10,
		deterministic: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := eng.Close(); err != nil {
			t.Error(err)
		}
	}()

	cases := []queryCase{
		{ID: "S01", Category: "simple", Input: "show customers", Expect: "sql"},
		{ID: "V01", Category: "value", Input: "customers from India", Expect: "sql"},
		{ID: "Z01", Category: "adversarial", Input: "delete all customers", Expect: "SECURITY_REJECTED"},
	}
	scale := eng.scale("test", "commerce.json")
	s := runConcurrency(ctx, eng, cases, scale, 8, 48)

	if s.Requests != 48 {
		t.Errorf("requests = %d, want 48", s.Requests)
	}
	if s.Succeeded+s.Refused+s.Failed != 48 {
		t.Errorf("outcomes do not add up: %+v", s)
	}
	if s.Failed != 0 {
		t.Errorf("%d requests failed with INTERNAL_ERROR under concurrency", s.Failed)
	}
	if s.RPS <= 0 || s.Latency.N != 48 {
		t.Errorf("throughput/latency not recorded: rps=%v n=%d", s.RPS, s.Latency.N)
	}
	// The split accounting must survive concurrent accumulation.
	if s.EmbedPerRequest <= 0 || s.SearchPerRequest <= 0 || s.SearchesPerRequest <= 0 {
		t.Errorf("retrieval split missing under concurrency: %+v", s)
	}
}

// A v1 comparison must never appear as a bare multiplier: section 17's
// fairness rules require the categories to be named.
func TestRenderV1StatesTheComparisonCategories(t *testing.T) {
	r := sampleRun()
	r.Scenarios = append(r.Scenarios, Scenario{
		Name: "v1", Description: "production QueryForge (LLM)", Concurrency: 1,
		Scale: ScaleInfo{Label: "v1-single-entity"}, Requests: 10, Succeeded: 9, Failed: 1,
		Latency: bench.Summary{N: 10, P50: 30 * time.Second, P95: 90 * time.Second,
			Mean: 40 * time.Second, Min: 20 * time.Second, Max: 90 * time.Second},
		Note: "provider degraded during the window",
	})
	out := Render(r)
	for _, want := range []string{
		"## Against the LLM architecture (v1)",
		"### The comparison categories",
		"Cold vs cold", "Warm vs warm", "Local vs remote", "Single vs sustained",
		"provider degraded during the window",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("v1 section is missing %q", want)
		}
	}
	if strings.Contains(out, "x faster") {
		t.Error("the report must not print a bare 'x faster' claim")
	}
}
