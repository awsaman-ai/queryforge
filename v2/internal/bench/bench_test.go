package bench

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/embedding"
	"queryforge.local/v2experiment/internal/pipeline"
)

// ---------------------------------------------------------------- percentiles

func TestPercentileIsNearestRank(t *testing.T) {
	// 1..100 ms, so the nearest-rank answer for pN is exactly N ms.
	var d Durations
	for i := 1; i <= 100; i++ {
		d.Add(time.Duration(i) * time.Millisecond)
	}
	for _, tc := range []struct {
		p    float64
		want time.Duration
	}{
		{50, 50 * time.Millisecond},
		{95, 95 * time.Millisecond},
		{99, 99 * time.Millisecond},
		{100, 100 * time.Millisecond},
	} {
		if got := d.Percentile(tc.p); got != tc.want {
			t.Errorf("p%v = %v, want %v", tc.p, got, tc.want)
		}
	}
}

func TestPercentileRoundsRankUp(t *testing.T) {
	// Three samples: p50 is rank ceil(1.5) = 2, the middle value. An
	// interpolating definition would answer 20ms here too, so use a set where
	// the two differ: rank 2 of {10,20,30} is 20; linear interpolation at
	// p95 would give 29ms, nearest-rank gives 30ms (rank 3).
	d := Durations{10 * time.Millisecond, 20 * time.Millisecond, 30 * time.Millisecond}
	if got := d.Percentile(50); got != 20*time.Millisecond {
		t.Errorf("p50 = %v, want 20ms", got)
	}
	if got := d.Percentile(95); got != 30*time.Millisecond {
		t.Errorf("p95 = %v, want 30ms (nearest rank, never interpolated)", got)
	}
}

func TestPercentileDoesNotReorderCaller(t *testing.T) {
	d := Durations{30 * time.Millisecond, 10 * time.Millisecond, 20 * time.Millisecond}
	_ = d.Percentile(50)
	if d[0] != 30*time.Millisecond {
		t.Fatalf("Percentile sorted the caller's slice: %v", d)
	}
}

func TestPercentileEmptyAndClamped(t *testing.T) {
	var empty Durations
	if got := empty.Percentile(99); got != 0 {
		t.Errorf("empty p99 = %v, want 0", got)
	}
	d := Durations{5 * time.Millisecond, 9 * time.Millisecond}
	if got := d.Percentile(0); got != 5*time.Millisecond {
		t.Errorf("p0 = %v, want the minimum", got)
	}
	if got := d.Percentile(1000); got != 9*time.Millisecond {
		t.Errorf("p1000 = %v, want the maximum", got)
	}
}

func TestMinSamplesFor(t *testing.T) {
	for _, tc := range []struct {
		p    float64
		want int
	}{{50, 2}, {95, 20}, {99, 100}, {99.9, 1000}} {
		if got := MinSamplesFor(tc.p); got != tc.want {
			t.Errorf("MinSamplesFor(%v) = %d, want %d", tc.p, got, tc.want)
		}
	}
}

func TestSummarizeReportsUnderSampledTails(t *testing.T) {
	var d Durations
	for i := 0; i < 10; i++ {
		d.Add(time.Millisecond)
	}
	s := d.Summarize()
	if !s.P95UnderSampled || !s.P99UnderSampled {
		t.Errorf("10 samples should mark p95 and p99 under-sampled, got %+v", s)
	}

	var many Durations
	for i := 0; i < 100; i++ {
		many.Add(time.Millisecond)
	}
	s = many.Summarize()
	if s.P95UnderSampled || s.P99UnderSampled {
		t.Errorf("100 samples should not be under-sampled, got %+v", s)
	}
}

func TestSummarizeArithmetic(t *testing.T) {
	d := Durations{
		10 * time.Millisecond, 20 * time.Millisecond,
		30 * time.Millisecond, 40 * time.Millisecond,
	}
	s := d.Summarize()
	if s.N != 4 {
		t.Errorf("N = %d, want 4", s.N)
	}
	if s.Min != 10*time.Millisecond || s.Max != 40*time.Millisecond {
		t.Errorf("min/max = %v/%v, want 10ms/40ms", s.Min, s.Max)
	}
	if s.Mean != 25*time.Millisecond {
		t.Errorf("mean = %v, want 25ms", s.Mean)
	}
	// Population stddev of {10,20,30,40} is sqrt(125) = 11.1803...ms.
	if got := s.StdDev.Round(time.Microsecond); got != 11180*time.Microsecond+339*time.Nanosecond {
		if diff := s.StdDev - 11180339*time.Nanosecond; diff > time.Microsecond || diff < -time.Microsecond {
			t.Errorf("stddev = %v, want ~11.18ms", s.StdDev)
		}
	}
}

func TestSummarizeEmpty(t *testing.T) {
	var empty Durations
	if s := empty.Summarize(); s.N != 0 || s.Mean != 0 {
		t.Errorf("empty summary = %+v, want zeroes", s)
	}
}

func TestThroughputUsesWallClock(t *testing.T) {
	if got := Throughput(100, 2*time.Second); got != 50 {
		t.Errorf("throughput = %v, want 50/s", got)
	}
	if got := Throughput(0, time.Second); got != 0 {
		t.Errorf("no completions should be 0/s, got %v", got)
	}
	if got := Throughput(10, 0); got != 0 {
		t.Errorf("zero wall time should be 0/s, not infinity, got %v", got)
	}
}

// ------------------------------------------------------------------- stages

func TestStageTotalsReadPipelineStats(t *testing.T) {
	var st StageTotals
	st.Add(pipeline.Stats{
		ExtractMs: 1, RetrieveMs: 20, RankMs: 4, BindMs: 2, PlanMs: 0.5,
		CompileMs: 0.5, TotalMs: 28,
	})
	st.Add(pipeline.Stats{
		ExtractMs: 3, RetrieveMs: 40, RankMs: 8, BindMs: 4, PlanMs: 1.5,
		CompileMs: 1.5, TotalMs: 58,
	})

	if st.Len() != 2 {
		t.Fatalf("Len = %d, want 2", st.Len())
	}
	if got := st.Retrieve.Summarize().Mean; got != 30*time.Millisecond {
		t.Errorf("mean retrieve = %v, want 30ms", got)
	}
	shares := st.ShareOfTotal()
	// Retrieval is 30 of 43ms mean total.
	if shares["retrieve"] < 0.69 || shares["retrieve"] > 0.70 {
		t.Errorf("retrieve share = %v, want ~0.698", shares["retrieve"])
	}
	if len(st.Named()) != 6 {
		t.Errorf("Named() = %d stages, want 6", len(st.Named()))
	}
}

func TestStageTotalsEmptyShares(t *testing.T) {
	var st StageTotals
	if got := st.ShareOfTotal(); len(got) != 0 {
		t.Errorf("no samples should give no shares, got %v", got)
	}
}

// ----------------------------------------------------------------- resources

func TestParsePSTime(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want time.Duration
	}{
		{"0:00.00", 0},
		{"0:01.50", 1500 * time.Millisecond},
		{"1:02.00", time.Minute + 2*time.Second},
		{"01:02:03", time.Hour + 2*time.Minute + 3*time.Second},
		{"2-01:02:03", 48*time.Hour + time.Hour + 2*time.Minute + 3*time.Second},
		{" 0:07.25 ", 7250 * time.Millisecond},
	} {
		got, err := ParsePSTime(tc.in)
		if err != nil {
			t.Errorf("ParsePSTime(%q): %v", tc.in, err)
			continue
		}
		if got != tc.want {
			t.Errorf("ParsePSTime(%q) = %v, want %v", tc.in, got, tc.want)
		}
	}
}

func TestParsePSTimeRejectsJunk(t *testing.T) {
	for _, in := range []string{"", "abc", "1:2:3:4", "x:00"} {
		if _, err := ParsePSTime(in); err == nil {
			t.Errorf("ParsePSTime(%q) should fail", in)
		}
	}
}

func TestSampleReadsThisProcess(t *testing.T) {
	r := Sample(context.Background(), os.Getpid())
	if r.GoHeapBytes == 0 {
		t.Error("heap should not be zero")
	}
	if r.GoPeakRSSBytes <= 0 {
		t.Error("peak rss should be positive")
	}
	// The test process is its own "worker" here, so the ps path is exercised.
	if r.WorkerErr != "" {
		t.Errorf("ps read failed: %s", r.WorkerErr)
	}
	if r.WorkerRSSBytes <= 0 {
		t.Errorf("ps rss = %d, want positive", r.WorkerRSSBytes)
	}
	if r.TotalRSSBytes() <= 0 || r.TotalCPU() < 0 {
		t.Errorf("totals look wrong: %+v", r)
	}
}

func TestSampleWithoutWorker(t *testing.T) {
	r := Sample(context.Background(), 0)
	if r.WorkerPID != 0 || r.WorkerRSSBytes != 0 {
		t.Errorf("no worker should read as zero, got %+v", r)
	}
	if r.WorkerErr != "" {
		t.Errorf("no worker is not an error, got %q", r.WorkerErr)
	}
}

// ------------------------------------------------------------------ schemagen

func basePath(t *testing.T) string {
	t.Helper()
	p, err := filepath.Abs("../../testdata/configs/commerce.json")
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestGenerateHitsTargetExactly(t *testing.T) {
	for _, target := range []int{100, 250, 1000} {
		out := filepath.Join(t.TempDir(), "scale.json")
		g, err := Generate(basePath(t), out, target)
		if err != nil {
			t.Fatalf("target %d: %v", target, err)
		}
		if g.Entries != target {
			t.Errorf("target %d: produced %d corpus entries", target, g.Entries)
		}
		if g.Surfaces <= g.Entries {
			t.Errorf("target %d: %d vectors for %d entries; every entry has at least one surface",
				target, g.Surfaces, g.Entries)
		}
	}
}

func TestGenerateIsDeterministic(t *testing.T) {
	dir := t.TempDir()
	first := filepath.Join(dir, "a.json")
	second := filepath.Join(dir, "b.json")
	if _, err := Generate(basePath(t), first, 400); err != nil {
		t.Fatal(err)
	}
	if _, err := Generate(basePath(t), second, 400); err != nil {
		t.Fatal(err)
	}
	a, _ := os.ReadFile(first)
	b, _ := os.ReadFile(second)
	if string(a) != string(b) {
		t.Error("two runs of the generator produced different bytes")
	}
}

func TestGeneratedConfigLoadsAndValidates(t *testing.T) {
	out := filepath.Join(t.TempDir(), "scale.json")
	if _, err := Generate(basePath(t), out, 600); err != nil {
		t.Fatal(err)
	}
	cfg, err := config.Load(out)
	if err != nil {
		t.Fatalf("generated config rejected by the real loader: %v", err)
	}
	if err := config.Validate(cfg); err != nil {
		t.Fatalf("generated config is invalid: %v", err)
	}
}

// The core is what the benchmark's sentences talk to. If padding altered it,
// every cross-scale comparison would be measuring two different questions.
func TestGeneratePreservesTheBaseSchema(t *testing.T) {
	out := filepath.Join(t.TempDir(), "scale.json")
	if _, err := Generate(basePath(t), out, 800); err != nil {
		t.Fatal(err)
	}

	base, err := readJSONObject(basePath(t))
	if err != nil {
		t.Fatal(err)
	}
	gen, err := readJSONObject(out)
	if err != nil {
		t.Fatal(err)
	}

	for _, key := range []string{"entities", "fields", "relationships"} {
		baseItems := asSlice(base[key])
		genItems := asSlice(gen[key])
		if len(genItems) < len(baseItems) {
			t.Fatalf("%s shrank: %d < %d", key, len(genItems), len(baseItems))
		}
		for i := range baseItems {
			want, _ := json.Marshal(baseItems[i])
			got, _ := json.Marshal(genItems[i])
			if string(want) != string(got) {
				t.Errorf("%s[%d] changed:\n base: %s\n gen:  %s", key, i, want, got)
			}
		}
	}

	// Padding must not join to the core, or the binder's graph would differ.
	coreEntities := map[string]bool{}
	for _, e := range asSlice(base["entities"]) {
		coreEntities[e.(map[string]any)["name"].(string)] = true
	}
	for i, r := range asSlice(gen["relationships"]) {
		if i < len(asSlice(base["relationships"])) {
			continue
		}
		rel := r.(map[string]any)
		from := rel["from"].(map[string]any)["entity"].(string)
		to := rel["to"].(map[string]any)["entity"].(string)
		if coreEntities[from] || coreEntities[to] {
			t.Errorf("synthetic relationship %v touches the core schema", rel["name"])
		}
	}
}

func TestGenerateRefusesToShrinkTheBase(t *testing.T) {
	out := filepath.Join(t.TempDir(), "scale.json")
	if _, err := Generate(basePath(t), out, 5); err == nil {
		t.Error("a target below the base config should be an error, not a trimmed base")
	}
}

// ------------------------------------------------------------------- batched

// fakeProvider records the batch sizes it is asked for, so chunking can be
// checked without a model.
type fakeProvider struct {
	sizes []int
	dim   int
}

func (f *fakeProvider) Identity() embedding.Identity {
	return embedding.Identity{Model: "fake", Dim: f.dim, Semantic: false}
}

func (f *fakeProvider) Embed(_ context.Context, texts []string) ([][]float32, error) {
	f.sizes = append(f.sizes, len(texts))
	out := make([][]float32, len(texts))
	for i, t := range texts {
		// A vector that encodes its own text, so a reordered stitch is visible.
		out[i] = []float32{float32(len(t)), float32(t[0])}
	}
	return out, nil
}

func (f *fakeProvider) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	v, err := f.Embed(ctx, []string{text})
	if err != nil {
		return nil, err
	}
	return v[0], nil
}

func (f *fakeProvider) Close() error { return nil }

func TestBatchedSplitsAndPreservesOrder(t *testing.T) {
	texts := make([]string, 0, 2600)
	for i := 0; i < 2600; i++ {
		texts = append(texts, string(rune('a'+i%26))+fmt.Sprint(i))
	}
	fake := &fakeProvider{dim: 2}
	b := NewBatched(fake)

	got, err := b.Embed(context.Background(), texts)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != len(texts) {
		t.Fatalf("got %d vectors for %d texts", len(got), len(texts))
	}
	if len(fake.sizes) != 2 || fake.sizes[0] != BatchLimit || fake.sizes[1] != 2600-BatchLimit {
		t.Errorf("batches = %v, want [%d %d]", fake.sizes, BatchLimit, 2600-BatchLimit)
	}
	// Order is what a stitched result gets wrong; check the seam explicitly.
	for _, i := range []int{0, BatchLimit - 1, BatchLimit, len(texts) - 1} {
		want := []float32{float32(len(texts[i])), float32(texts[i][0])}
		if got[i][0] != want[0] || got[i][1] != want[1] {
			t.Errorf("vector %d = %v, want %v (text %q)", i, got[i], want, texts[i])
		}
	}
	if b.Calls() != 2 {
		t.Errorf("Calls = %d, want 2", b.Calls())
	}
}

func TestBatchedPassesSmallCallsThrough(t *testing.T) {
	fake := &fakeProvider{dim: 2}
	b := NewBatched(fake)
	if _, err := b.Embed(context.Background(), []string{"one", "two"}); err != nil {
		t.Fatal(err)
	}
	if len(fake.sizes) != 1 || fake.sizes[0] != 2 {
		t.Errorf("batches = %v, want [2]", fake.sizes)
	}
	if b.Identity().Model != "fake" {
		t.Error("identity should pass through unchanged")
	}
}

// The wrapper is shared by every goroutine of the concurrency scenarios. The
// race detector caught a plain int counter here; this keeps it caught.
func TestBatchedCountsCallsConcurrently(t *testing.T) {
	fake := &fakeProvider{dim: 2}
	b := NewBatched(fake)
	b.Provider = &lockedProvider{inner: fake}

	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := b.Embed(context.Background(), []string{"one", "two"}); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if got := b.Calls(); got != 16 {
		t.Errorf("Calls = %d, want 16", got)
	}
}

// lockedProvider serialises the fake, which is not itself concurrency-safe;
// the point of the test is the wrapper's own counter, not the fake's slice.
type lockedProvider struct {
	mu    sync.Mutex
	inner embedding.Provider
}

func (l *lockedProvider) Identity() embedding.Identity { return l.inner.Identity() }
func (l *lockedProvider) Close() error                 { return l.inner.Close() }

func (l *lockedProvider) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.Embed(ctx, texts)
}

func (l *lockedProvider) EmbedOne(ctx context.Context, text string) ([]float32, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.inner.EmbedOne(ctx, text)
}
