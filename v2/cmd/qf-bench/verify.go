package main

import (
	"fmt"
	"io"
	"math"
	"time"

	"queryforge.local/v2experiment/internal/bench"
)

// Verify recomputes every summary in a results document from the raw samples
// it stores, and reports any disagreement.
//
// A benchmark's credibility rests on arithmetic nobody watched happen. The
// scenarios that keep their per-request outcomes can be checked against them
// after the fact, by anyone, without re-running anything -- which is the
// difference between a number a reader can audit and a number they must
// trust.
//
// It is not a unit test of the percentile code (that exists separately). It
// is an end-to-end check that the document published to the report and the
// samples underneath it say the same thing.
func Verify(r *Run, out io.Writer) error {
	checked, mismatches := 0, 0

	for _, s := range r.Scenarios {
		if len(s.Outcomes) == 0 {
			continue
		}
		checked++
		label := fmt.Sprintf("%s/%s/c=%d", s.Name, s.Scale.Label, s.Concurrency)

		var all, accepted bench.Durations
		succeeded, refused, failed := 0, 0, 0
		for _, o := range s.Outcomes {
			all.Add(o.Wall)
			switch {
			case o.Code == "":
				accepted.Add(o.Wall)
				succeeded++
			case o.Code == "INTERNAL_ERROR":
				failed++
			default:
				refused++
			}
		}

		want := all.Summarize()
		mismatches += compare(out, label, "n", time.Duration(want.N), time.Duration(s.Latency.N))
		mismatches += compare(out, label, "p50", want.P50, s.Latency.P50)
		mismatches += compare(out, label, "p95", want.P95, s.Latency.P95)
		mismatches += compare(out, label, "p99", want.P99, s.Latency.P99)
		mismatches += compare(out, label, "min", want.Min, s.Latency.Min)
		mismatches += compare(out, label, "max", want.Max, s.Latency.Max)
		mismatches += compare(out, label, "mean", want.Mean, s.Latency.Mean)

		if got, ok := s.LatencyByClass["accepted"]; ok {
			mismatches += compare(out, label, "accepted n",
				time.Duration(accepted.Len()), time.Duration(got.N))
		}
		if succeeded != s.Succeeded || refused != s.Refused || failed != s.Failed {
			fmt.Fprintf(out, "  MISMATCH %s counts: recomputed %d/%d/%d, document says %d/%d/%d\n",
				label, succeeded, refused, failed, s.Succeeded, s.Refused, s.Failed)
			mismatches++
		}
		if s.Wall > 0 {
			wantRPS := bench.Throughput(len(s.Outcomes), s.Wall)
			if math.Abs(wantRPS-s.RPS) > 0.01 {
				fmt.Fprintf(out, "  MISMATCH %s throughput: recomputed %.3f, document says %.3f\n",
					label, wantRPS, s.RPS)
				mismatches++
			}
		}
	}

	fmt.Fprintf(out, "verified %d scenarios that kept their raw samples: %d mismatches\n", checked, mismatches)
	if mismatches > 0 {
		return fmt.Errorf("%d reported values do not match the samples they were computed from", mismatches)
	}
	if checked == 0 {
		return fmt.Errorf("no scenario in the document kept its raw samples; nothing could be verified")
	}
	return nil
}

func compare(out io.Writer, label, field string, want, got time.Duration) int {
	// One nanosecond of tolerance: the mean is stored as a rounded Duration.
	if diff := want - got; diff > time.Nanosecond || diff < -time.Nanosecond {
		fmt.Fprintf(out, "  MISMATCH %s %s: recomputed %v, document says %v\n", label, field, want, got)
		return 1
	}
	return 0
}
