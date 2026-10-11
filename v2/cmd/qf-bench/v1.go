package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"time"

	"queryforge.local/v2experiment/internal/bench"
)

// The v1 baseline answers the question Phase 9 exists for: is the non-LLM
// architecture meaningfully faster than the LLM one? It can only be answered
// by measuring both, so this suite drives the PRODUCTION QueryForge binary.
//
// It does so strictly out of process. v2 may not import the production
// library -- internal/isolation/isolation_test.go fails the build if it ever
// does -- so the baseline speaks the SDK's stdio protocol to a prebuilt
// binary the operator passes in with --v1-bin. Nothing outside v2/ is
// modified, and the production module is not edited to be measured.
//
// Three honesty constraints, all from section 17's fairness rules:
//
//  1. v1 is a REMOTE generative call; v2 is LOCAL. The report never merges
//     the two categories into one number without that label.
//  2. v1's binary is process-per-request by design (it is not a daemon), so
//     its number includes process start. The floor is measured separately
//     with the model-free "version" op and reported, so a reader can see how
//     much of the gap is process start and how much is the model.
//  3. v1 is single-entity, so only the sentences a single-entity schema can
//     answer are sent. Comparing a join query against a system that cannot
//     express one would be measuring a refusal.
type v1Request struct {
	Op      string          `json:"op"`
	Backend string          `json:"backend,omitempty"`
	Config  json.RawMessage `json:"config,omitempty"`
	Query   string          `json:"query,omitempty"`
	Options *v1Options      `json:"options,omitempty"`
}

type v1Options struct {
	TimeoutMs int `json:"timeoutMs,omitempty"`
}

// v1Response is the part of the SDK protocol this harness reads. The failure
// code is a TOP-LEVEL field ("code"), not a nested error object: reading it
// from the wrong place turns every measured refusal into "UNKNOWN", which is
// how the first probe run reported two real TIMEOUTs.
type v1Response struct {
	Success bool   `json:"success"`
	Code    string `json:"code"`
	Message string `json:"message"`
}

// runV1Baseline measures the production engine and saves after every pass.
//
// The incremental save is not a nicety: one pass over fifteen sentences took
// half an hour against a degraded provider, and a suite that writes only at
// the end would have thrown all of it away on any interruption.
func runV1Baseline(ctx context.Context, opt options, cases []queryCase, stdout io.Writer) (Scenario, error) {
	s := Scenario{
		Name:        "v1",
		Description: "production QueryForge (LLM), one process per request, remote model",
		Concurrency: 1,
		Scale:       ScaleInfo{Label: "v1-single-entity"},
	}

	if opt.v1Bin == "" {
		return s, fmt.Errorf("--v1-bin is required for the v1 baseline")
	}
	if _, err := os.Stat(opt.v1Bin); err != nil {
		return s, fmt.Errorf("v1 binary: %w", err)
	}
	cfg, err := os.ReadFile(opt.v1Config)
	if err != nil {
		return s, fmt.Errorf("v1 config: %w", err)
	}
	if os.Getenv("QF_API_KEY") == "" {
		return s, fmt.Errorf("QF_API_KEY is not set; the v1 baseline makes a real model call")
	}

	// Only the sentences a single-entity schema can answer.
	var comparable []queryCase
	for _, c := range cases {
		if c.V1 && len(comparable) < opt.v1Cases {
			comparable = append(comparable, c)
		}
	}
	if len(comparable) == 0 {
		return s, fmt.Errorf("no benchmark cases are marked comparable to v1 (\"v1\": true)")
	}

	timeout := opt.v1Timeout
	if timeout <= 0 {
		timeout = 120
	}

	// The floor: the same binary, the same process start, no model call.
	var floor bench.Durations
	for i := 0; i < 5; i++ {
		start := time.Now()
		if _, err := callV1(ctx, opt.v1Bin, v1Request{Op: "version"}, timeout); err != nil {
			return s, fmt.Errorf("v1 version handshake: %w", err)
		}
		floor.Add(time.Since(start))
	}

	outcomes := make([]Outcome, 0, len(comparable)*opt.v1Reps)
	wallStart := time.Now()
	for rep := 0; rep < opt.v1Reps; rep++ {
		if rep > 0 {
			// Save what the previous pass measured before spending another
			// half hour on the next one.
			if err := saveV1Progress(opt, s, outcomes, floor, comparable, time.Since(wallStart), rep); err != nil {
				fmt.Fprintf(stdout, "warning: could not save partial v1 results: %v\n", err)
			}
		}
		for _, c := range comparable {
			req := v1Request{
				Op:      "translate",
				Backend: "sql",
				Config:  json.RawMessage(cfg),
				Query:   c.Input,
				Options: &v1Options{TimeoutMs: timeout * 1000},
			}
			start := time.Now()
			resp, err := callV1(ctx, opt.v1Bin, req, timeout)
			elapsed := time.Since(start)

			o := Outcome{ID: c.ID, Category: c.Category, Wall: elapsed}
			switch {
			case err != nil:
				o.Code = "TRANSPORT_ERROR"
			case !resp.Success:
				o.Code = "V1_" + errCode(resp)
			}
			outcomes = append(outcomes, o)
			fmt.Fprintf(stdout, "v1 %s rep %d: %s %s\n", c.ID, rep+1, humanDuration(elapsed), o.Code)
		}
	}
	wall := time.Since(wallStart)

	finishV1(&s, outcomes, floor, comparable, wall, opt.v1Reps, opt.v1Bin)
	return s, nil
}

// finishV1 folds the measured outcomes into the scenario.
func finishV1(s *Scenario, outcomes []Outcome, floor bench.Durations, comparable []queryCase,
	wall time.Duration, reps int, bin string) {
	summarize(s, outcomes, wall)
	s.Outcomes = outcomes
	s.Stages = map[string]bench.Summary{"process_floor_no_model": floor.Summarize()}
	s.Note = fmt.Sprintf("%d sentences x %d passes through %s; process floor (op=version, no model call) p50 %s. "+
		"Remote model, process per request -- a different category from v2's warm local process.",
		len(comparable), reps, bin, humanDuration(floor.Summarize().P50))
}

// saveV1Progress writes the passes completed so far into the results file.
func saveV1Progress(opt options, base Scenario, outcomes []Outcome, floor bench.Durations,
	comparable []queryCase, wall time.Duration, repsDone int) error {
	partial := base
	finishV1(&partial, append([]Outcome(nil), outcomes...), floor, comparable, wall, repsDone, opt.v1Bin)
	partial.Note = fmt.Sprintf("PARTIAL after %d of %d passes. %s", repsDone, opt.v1Reps, partial.Note)
	run := &Run{Version: 1, StartedAt: time.Now(), FinishedAt: time.Now(),
		Environment: LocalEnvironment(), Scenarios: []Scenario{partial}}
	return mergeInto(opt.results, run)
}

func callV1(ctx context.Context, bin string, req v1Request, timeoutSeconds int) (v1Response, error) {
	payload, err := json.Marshal(req)
	if err != nil {
		return v1Response{}, err
	}
	// A margin over the engine's own budget, so the harness never kills a
	// call the engine would have reported on.
	ctx, cancel := context.WithTimeout(ctx, time.Duration(timeoutSeconds+30)*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, bin)
	cmd.Stdin = bytes.NewReader(payload)
	var out, errOut bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errOut

	// Exit code 1 means a well-formed failure response, which is a measured
	// outcome rather than a transport error; only an empty stdout is fatal.
	runErr := cmd.Run()
	var resp v1Response
	if decodeErr := json.Unmarshal(out.Bytes(), &resp); decodeErr != nil {
		if runErr != nil {
			return v1Response{}, fmt.Errorf("%w: %s", runErr, truncate(errOut.String(), 200))
		}
		return v1Response{}, fmt.Errorf("undecodable response: %s", truncate(out.String(), 200))
	}
	return resp, nil
}

func errCode(r v1Response) string {
	if r.Code != "" {
		return r.Code
	}
	return "UNKNOWN"
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
