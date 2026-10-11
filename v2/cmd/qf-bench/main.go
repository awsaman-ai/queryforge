// Command qf-bench is the Phase 9 performance harness: it measures the warm
// v2 architecture (cold start, warm latency, per-stage attribution, scale,
// concurrency, CPU and memory), records every sample in a JSON document, and
// renders the report the brief's manual gate asks for.
//
// It is a separate command from qf-v2 on purpose. qf-v2's flag surface is
// audited Phase 0-8 code, and a benchmark mode there would mean touching a
// closed record to add a measurement. This binary imports the same internal
// packages and assembles the engine the same way, but nothing in the shipped
// path depends on it.
//
// Usage:
//
//	qf-bench --suite generate                 # write the 100/1k/10k configs
//	qf-bench --suite cold                     # cold-start distribution
//	qf-bench --suite warm                     # warm latency at every scale
//	qf-bench --suite depth                    # retrieval / +ranking / full
//	qf-bench --suite concurrency              # 1/10/50/100 against one engine
//	qf-bench --suite v1                       # the LLM baseline, out of process
//	qf-bench --suite merge --merge-from f.json # fold another results file in
//	qf-bench --suite verify                   # recompute every summary from its samples
//	qf-bench --suite report                   # render markdown from the JSON
//	qf-bench --suite all                      # every measurement, then report
//
// Results accumulate in --results (default benchmarks/scratch/results.json):
// each suite merges into what is already there, so a long matrix can be run in
// pieces and reported once.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"queryforge.local/v2experiment/internal/bench"
)

type options struct {
	suite       string
	baseConfig  string
	dir         string
	queries     string
	scales      []int
	concurrency []int
	reps        int
	warmup      int
	total       int
	coldReps    int
	topK        int

	results string
	out     string

	workerScript string
	model        string

	v1Bin     string
	v1Config  string
	v1Reps    int
	v1Cases   int
	v1Timeout int

	note          string
	mergeFrom     string
	deterministic bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout, os.Stderr); err != nil {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

func run(args []string, stdout, stderr io.Writer) error {
	var opt options
	var scales, concurrency string

	fs := flag.NewFlagSet("qf-bench", flag.ContinueOnError)
	fs.SetOutput(stderr)
	fs.StringVar(&opt.suite, "suite", "warm", "generate | cold | warm | depth | concurrency | v1 | merge | verify | report | all")
	fs.StringVar(&opt.baseConfig, "base-config", "testdata/configs/commerce.json", "the reviewed config the scale configs are grown from")
	fs.StringVar(&opt.dir, "dir", "benchmarks/scratch", "where generated configs, indexes and raw results go")
	fs.StringVar(&opt.queries, "queries", "benchmarks/queries.json", "the benchmark sentence set")
	fs.StringVar(&scales, "scales", "100,1000,10000", "corpus-entry targets for the scale matrix")
	fs.StringVar(&concurrency, "concurrency", "1,10,50,100", "concurrency levels")
	fs.IntVar(&opt.reps, "reps", 20, "passes over the sentence set per warm scenario")
	fs.IntVar(&opt.warmup, "warmup", 1, "unrecorded passes before measuring")
	fs.IntVar(&opt.total, "total", 200, "total requests per concurrency level")
	fs.IntVar(&opt.coldReps, "cold-reps", 5, "cold starts to measure")
	fs.IntVar(&opt.topK, "top-k", 10, "retrieval candidates per span")
	fs.StringVar(&opt.results, "results", "", "raw results JSON (default <dir>/results.json)")
	fs.StringVar(&opt.out, "out", "benchmarks/reports/performance.md", "rendered report")
	fs.StringVar(&opt.workerScript, "worker-script", "", "path to worker.py (default: the module's own)")
	fs.StringVar(&opt.model, "model", "", "embedding model override")
	fs.BoolVar(&opt.deterministic, "deterministic", false, "use the hashing stand-in instead of the model: a MECHANISM check, never a research result")
	fs.StringVar(&opt.v1Bin, "v1-bin", "", "path to a prebuilt production QueryForge binary (cmd/queryforge) for the LLM baseline")
	fs.StringVar(&opt.v1Config, "v1-config", "benchmarks/v1-commerce.config.json", "the v1-format config describing the same schema")
	fs.IntVar(&opt.v1Reps, "v1-reps", 3, "repetitions per sentence for the v1 baseline")
	fs.IntVar(&opt.v1Cases, "v1-cases", 20, "how many sentences of the set to send to v1")
	fs.IntVar(&opt.v1Timeout, "v1-timeout", 120, "seconds allowed for one v1 request, model call included")
	fs.StringVar(&opt.note, "note", "", "a note recorded with the run (machine state, caveats)")
	fs.StringVar(&opt.mergeFrom, "merge-from", "", "with --suite merge: another results file to fold into --results")
	if err := fs.Parse(args); err != nil {
		return err
	}

	var err error
	if opt.scales, err = parseInts(scales); err != nil {
		return fmt.Errorf("--scales: %w", err)
	}
	if opt.concurrency, err = parseInts(concurrency); err != nil {
		return fmt.Errorf("--concurrency: %w", err)
	}
	if opt.results == "" {
		opt.results = filepath.Join(opt.dir, "results.json")
	}

	ctx := context.Background()
	switch opt.suite {
	case "generate":
		_, err := generateScales(opt, stdout)
		return err
	case "cold", "warm", "depth", "concurrency", "v1":
		return measure(ctx, opt, opt.suite, stdout, stderr)
	case "all":
		for _, suite := range []string{"cold", "warm", "depth", "concurrency"} {
			if err := measure(ctx, opt, suite, stdout, stderr); err != nil {
				return err
			}
		}
		if opt.v1Bin != "" {
			if err := measure(ctx, opt, "v1", stdout, stderr); err != nil {
				return err
			}
		}
		return renderTo(opt, stdout)
	case "merge":
		// A long suite (the v1 baseline is tens of minutes) can be run into
		// its own file and folded in afterwards, instead of holding a lock on
		// the main results document while it runs.
		if opt.mergeFrom == "" {
			return fmt.Errorf("--merge-from is required for --suite merge")
		}
		other, err := LoadRun(opt.mergeFrom)
		if err != nil {
			return err
		}
		if err := mergeInto(opt.results, other); err != nil {
			return err
		}
		fmt.Fprintf(stdout, "merged %d scenarios from %s into %s\n", len(other.Scenarios), opt.mergeFrom, opt.results)
		return nil
	case "verify":
		run, err := LoadRun(opt.results)
		if err != nil {
			return err
		}
		return Verify(run, stdout)
	case "report":
		return renderTo(opt, stdout)
	default:
		return fmt.Errorf("unknown suite %q", opt.suite)
	}
}

// scaleConfig is one generated schema plus where its cached index lives.
type scaleConfig struct {
	bench.Generated
	Label     string
	IndexPath string
}

// generateScales writes one config per requested size. Regeneration is
// deterministic, so an existing file is rewritten identically.
func generateScales(opt options, stdout io.Writer) ([]scaleConfig, error) {
	var out []scaleConfig
	for _, target := range opt.scales {
		label := scaleLabel(target)
		path := filepath.Join(opt.dir, "configs", fmt.Sprintf("scale-%s.json", label))
		g, err := bench.Generate(opt.baseConfig, path, target)
		if err != nil {
			return nil, err
		}
		sc := scaleConfig{
			Generated: g,
			Label:     label,
			IndexPath: filepath.Join(opt.dir, "indexes", fmt.Sprintf("scale-%s.index.json", label)),
		}
		out = append(out, sc)
		fmt.Fprintf(stdout, "%-6s %s  %d corpus entries, %d index vectors (%d entities, %d fields)\n",
			label, path, g.Entries, g.Surfaces, g.ConfigEntities, g.ConfigFields)
	}
	return out, nil
}

// measure runs one suite and merges its scenarios into the results file.
func measure(ctx context.Context, opt options, suite string, stdout, stderr io.Writer) error {
	cases, err := loadQuerySet(opt.queries)
	if err != nil {
		return err
	}
	scales, err := generateScales(opt, io.Discard)
	if err != nil {
		return err
	}

	run := &Run{Version: 1, StartedAt: time.Now(), Environment: LocalEnvironment()}
	if opt.note != "" {
		run.Notes = append(run.Notes, opt.note)
	}

	switch suite {
	case "v1":
		s, err := runV1Baseline(ctx, opt, cases, stdout)
		if err != nil {
			return err
		}
		run.Scenarios = append(run.Scenarios, s)
		run.FinishedAt = time.Now()
		return mergeInto(opt.results, run)
	case "cold":
		// Cold start is measured at the smallest and largest scale: the
		// difference between them IS the index-build cost curve.
		for _, sc := range []scaleConfig{scales[0], scales[len(scales)-1]} {
			fmt.Fprintf(stdout, "cold: %s (building the index each time)...\n", sc.Label)
			built, identity, strategy, err := runCold(ctx, engineOptions{
				configPath: sc.Path, topK: opt.topK, rebuildIndex: true,
				indexPath: sc.IndexPath, workerScript: opt.workerScript, model: opt.model,
				deterministic: opt.deterministic,
			}, sc.Label, opt.coldReps, false)
			if err != nil {
				return err
			}
			run.Scenarios = append(run.Scenarios, built)
			run.Environment = run.Environment.WithIdentity(identity, strategy)

			fmt.Fprintf(stdout, "cold: %s (loading the cached index)...\n", sc.Label)
			cached, _, _, err := runCold(ctx, engineOptions{
				configPath: sc.Path, topK: opt.topK,
				indexPath: sc.IndexPath, workerScript: opt.workerScript, model: opt.model,
				deterministic: opt.deterministic,
			}, sc.Label, opt.coldReps, true)
			if err != nil {
				return err
			}
			cached.Name = "cold-cached"
			run.Scenarios = append(run.Scenarios, cached)
		}
	default:
		for _, sc := range scales {
			eng, err := openEngine(ctx, engineOptions{
				configPath: sc.Path, indexPath: sc.IndexPath, topK: opt.topK,
				workerScript: opt.workerScript, model: opt.model, stderr: stderr,
				deterministic: opt.deterministic,
			})
			if err != nil {
				return err
			}
			run.Environment = run.Environment.WithIdentity(eng.index.Identity(), string(eng.index.Strategy()))
			scale := eng.scale(sc.Label, sc.Path)

			switch suite {
			case "warm":
				fmt.Fprintf(stdout, "warm: %s (%d entries)...\n", sc.Label, scale.CorpusEntries)
				run.Scenarios = append(run.Scenarios, runWarm(ctx, eng, cases, scale, opt.reps, opt.warmup))
			case "depth":
				for _, depth := range []string{"retrieval", "retrieval+ranking", "full"} {
					fmt.Fprintf(stdout, "depth: %s at %s...\n", depth, sc.Label)
					run.Scenarios = append(run.Scenarios, runDepth(ctx, eng, cases, scale, depth, opt.reps))
				}
			case "concurrency":
				for _, level := range opt.concurrency {
					fmt.Fprintf(stdout, "concurrency %d: %s...\n", level, sc.Label)
					run.Scenarios = append(run.Scenarios, runConcurrency(ctx, eng, cases, scale, level, opt.total))
				}
			}
			if err := eng.Close(); err != nil {
				return err
			}
		}
	}

	run.FinishedAt = time.Now()
	return mergeInto(opt.results, run)
}

// mergeInto folds a run into the results file, preserving earlier suites.
func mergeInto(path string, fresh *Run) error {
	merged := fresh
	if existing, err := LoadRun(path); err == nil {
		existing.Merge(fresh)
		merged = existing
	} else if !os.IsNotExist(err) && !strings.Contains(err.Error(), "no such file") {
		return err
	}
	return merged.Save(path)
}

func renderTo(opt options, stdout io.Writer) error {
	run, err := LoadRun(opt.results)
	if err != nil {
		return fmt.Errorf("no results to report (%w); run a measurement suite first", err)
	}
	markdown := Render(run)
	if err := os.MkdirAll(filepath.Dir(opt.out), 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(opt.out, []byte(markdown), 0o644); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "report written: %s (%d scenarios)\n", opt.out, len(run.Scenarios))
	return nil
}

func scaleLabel(entries int) string {
	switch {
	case entries >= 1000 && entries%1000 == 0:
		return fmt.Sprintf("%dk", entries/1000)
	default:
		return strconv.Itoa(entries)
	}
}

func parseInts(s string) ([]int, error) {
	var out []int
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		v, err := strconv.Atoi(part)
		if err != nil {
			return nil, err
		}
		if v <= 0 {
			return nil, fmt.Errorf("%d is not a positive count", v)
		}
		out = append(out, v)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no values")
	}
	return out, nil
}
