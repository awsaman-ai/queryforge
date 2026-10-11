// Package bench holds the measurement primitives Phase 9 needs: latency
// summaries, per-stage attribution built from pipeline.Stats, process
// resource sampling, and a deterministic schema generator for the scale
// matrix.
//
// It deliberately contains no orchestration and no output formatting. Those
// live in cmd/qf-bench, so that the arithmetic a performance claim rests on
// can be unit-tested without starting a model. Section 25 of the brief asks
// for numbers a reader can check; a percentile computed by an untested helper
// is exactly the kind of number that cannot be checked.
//
// Nothing here may read the user's sentence or a config value. The benchmark
// harness consumes pipeline.Stats -- the structured, redacted telemetry
// decision 013 requires -- and never the CLI's diagnostic stdout.
package bench

import (
	"math"
	"sort"
	"time"

	"queryforge.local/v2experiment/internal/pipeline"
)

// Durations is a set of measured latencies, one per request.
//
// The zero value is an empty set; Summary reports N=0 for it rather than
// panicking, because a scenario that produced no successful request is a
// result worth printing, not a crash.
type Durations []time.Duration

// Add appends one measurement.
func (d *Durations) Add(v time.Duration) { *d = append(*d, v) }

// Len reports how many measurements are held.
func (d Durations) Len() int { return len(d) }

// Percentile returns the p-th percentile by the NEAREST-RANK method: the
// smallest value at or above rank ceil(p/100 * N) in the sorted set, 1-indexed.
//
// Nearest-rank is chosen over interpolation because every reported number is
// then a latency that actually occurred, which is what a tail budget is about.
// It also means p99 of fewer than 100 samples is simply the maximum -- see
// MinSamplesFor, and the report states the sample count beside every
// percentile so a reader can judge it.
//
// p is clamped to (0, 100]. The receiver is copied before sorting, so calling
// Percentile does not reorder the caller's slice.
func (d Durations) Percentile(p float64) time.Duration {
	if len(d) == 0 {
		return 0
	}
	if p <= 0 {
		p = math.SmallestNonzeroFloat64
	}
	if p > 100 {
		p = 100
	}
	sorted := append(Durations(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// MinSamplesFor reports how many samples a percentile needs before it is
// distinguishable from the maximum: p99 needs 100, p95 needs 20, p50 needs 2.
//
// The harness uses it to mark a percentile as under-sampled in the report
// rather than printing a tail number that is really just the worst request.
func MinSamplesFor(p float64) int {
	if p <= 0 || p >= 100 {
		return 1
	}
	// The epsilon absorbs binary floating point: 100-99.9 is 0.09999999999999432,
	// which would otherwise round 1000 up to 1001 and mislabel a correctly sized
	// run as under-sampled.
	return int(math.Ceil(100/(100-p) - 1e-9))
}

// Summary is the reported shape of one latency distribution.
type Summary struct {
	N      int           `json:"n"`
	Min    time.Duration `json:"min"`
	Max    time.Duration `json:"max"`
	Mean   time.Duration `json:"mean"`
	StdDev time.Duration `json:"stddev"`
	P50    time.Duration `json:"p50"`
	P95    time.Duration `json:"p95"`
	P99    time.Duration `json:"p99"`
	// P95UnderSampled and P99UnderSampled are true when N is too small for
	// that percentile to mean anything beyond "the slowest request".
	P95UnderSampled bool `json:"p95_under_sampled"`
	P99UnderSampled bool `json:"p99_under_sampled"`
}

// Summarize computes the full summary in one pass over a sorted copy.
func (d Durations) Summarize() Summary {
	s := Summary{N: len(d)}
	if len(d) == 0 {
		return s
	}
	sorted := append(Durations(nil), d...)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })

	s.Min, s.Max = sorted[0], sorted[len(sorted)-1]

	var total float64
	for _, v := range sorted {
		total += float64(v)
	}
	mean := total / float64(len(sorted))
	s.Mean = time.Duration(mean)

	// Population standard deviation: the samples ARE the population of this
	// run, not a sample drawn from a larger one.
	var sq float64
	for _, v := range sorted {
		diff := float64(v) - mean
		sq += diff * diff
	}
	s.StdDev = time.Duration(math.Sqrt(sq / float64(len(sorted))))

	s.P50 = sorted.Percentile(50)
	s.P95 = sorted.Percentile(95)
	s.P99 = sorted.Percentile(99)
	s.P95UnderSampled = len(sorted) < MinSamplesFor(95)
	s.P99UnderSampled = len(sorted) < MinSamplesFor(99)
	return s
}

// Throughput reports completed requests per second over a wall-clock window.
//
// It takes the WALL time of the whole run, not the sum of the latencies:
// under concurrency those differ by the parallelism factor, and the sum would
// overstate throughput by exactly that factor.
func Throughput(completed int, wall time.Duration) float64 {
	if wall <= 0 || completed <= 0 {
		return 0
	}
	return float64(completed) / wall.Seconds()
}

// StageTotals accumulates per-stage latencies across requests.
//
// Every value comes from pipeline.Stats, which carries no user text and no
// config values (decision 013). The harness never parses CLI output.
type StageTotals struct {
	Extract  Durations `json:"extract"`
	Retrieve Durations `json:"retrieve"`
	Rank     Durations `json:"rank"`
	Bind     Durations `json:"bind"`
	Plan     Durations `json:"plan"`
	Compile  Durations `json:"compile"`
	Total    Durations `json:"total"`
}

// Add records one request's stage timings.
//
// Stats reports milliseconds as float64; the conversion is exact enough for
// microsecond reporting (Stats itself is built from Duration.Microseconds).
// Stages a refused request never reached arrive as zero and are recorded as
// zero, so the stage tables stay aligned with the request count. Callers that
// only want successful requests filter before calling.
func (s *StageTotals) Add(st pipeline.Stats) {
	s.Extract.Add(fromMillis(st.ExtractMs))
	s.Retrieve.Add(fromMillis(st.RetrieveMs))
	s.Rank.Add(fromMillis(st.RankMs))
	s.Bind.Add(fromMillis(st.BindMs))
	s.Plan.Add(fromMillis(st.PlanMs))
	s.Compile.Add(fromMillis(st.CompileMs))
	s.Total.Add(fromMillis(st.TotalMs))
}

// Len reports how many requests have been accumulated.
func (s *StageTotals) Len() int { return s.Total.Len() }

// Named returns the stages in the pipeline's own order, for table rendering.
// The names match pipeline.Stage so a reader can line the table up with the
// architecture diagram.
func (s *StageTotals) Named() []struct {
	Stage string
	Data  Durations
} {
	return []struct {
		Stage string
		Data  Durations
	}{
		{string(pipeline.StageExtract), s.Extract},
		{string(pipeline.StageRetrieve), s.Retrieve},
		{string(pipeline.StageRank), s.Rank},
		{string(pipeline.StageBind), s.Bind},
		{string(pipeline.StagePlan), s.Plan},
		{string(pipeline.StageCompile), s.Compile},
	}
}

// ShareOfTotal reports each stage's mean as a fraction of the mean total.
//
// The shares need not sum to 1: the total is measured around the whole run and
// includes the orchestration between stages, so a residue is expected and is
// itself informative (a large one means time is going somewhere unmeasured).
func (s *StageTotals) ShareOfTotal() map[string]float64 {
	out := map[string]float64{}
	total := float64(s.Total.Summarize().Mean)
	if total <= 0 {
		return out
	}
	for _, st := range s.Named() {
		out[st.Stage] = float64(st.Data.Summarize().Mean) / total
	}
	return out
}

func fromMillis(ms float64) time.Duration {
	if ms <= 0 {
		return 0
	}
	return time.Duration(ms * float64(time.Millisecond))
}
