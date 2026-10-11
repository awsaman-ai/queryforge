package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"queryforge.local/v2experiment/internal/compiler"
	"queryforge.local/v2experiment/internal/pipeline"
	"queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
)

// This file implements --query: the Phase 7 end-to-end surface.
//
//	natural language → extract → retrieve → rank → bind → SemanticPlan → compile → final query
//
// OUTPUT CONTRACT (section 15, decision 018): the final query, then its
// parameters, in order. Parameter VALUES are printed by default. They are the
// deliverable, not a diagnostic: the application executing the query needs
// them, and the SQL text itself never holds a value. This differs from
// --compile, --bind and --plan, which redact stored values unless --debug,
// because those are inspection surfaces.
//
// What stays redacted by default, as before:
//   - errors: a refusal names structure and token positions, never the
//     user's words or a config value (decisions 006, 013);
//   - --explain's trace of the reading, binding and plan: stored values show
//     only under --debug;
//   - telemetry (--debug, on stderr): built only from pipeline.Stats, which
//     holds counts, timings, schema names and codes, and nothing else.

func runQuery(ctx context.Context, opt options, stdout, stderr io.Writer) error {
	clock, err := clockFrom(opt)
	if err != nil {
		return err
	}
	eng, err := warmEngine(ctx, opt, stderr)
	if err != nil {
		return err
	}
	defer eng.Close()
	pol, err := policyFrom(opt, eng.registry)
	if err != nil {
		return err
	}
	pl, err := newPipeline(eng, opt, pol)
	if err != nil {
		return err
	}

	res, runErr := pl.Run(ctx, opt.query, clock)

	if opt.debug {
		// Telemetry: safe to log by construction (see pipeline.Stats).
		if enc, err := json.Marshal(res.Stats); err == nil {
			fmt.Fprintf(stderr, "telemetry: %s\n", enc)
		}
	}
	if opt.asJSON {
		if err := emitQueryJSON(res, runErr, opt, stdout); err != nil {
			return err
		}
		return runErr
	}
	if runErr == nil {
		fmt.Fprintln(stdout, res.Query.Render(true))
	}
	if opt.explain {
		explainRun(res, runErr, opt, stdout)
	}
	return runErr
}

// explainRun prints the reasoning behind the query, stage by stage, or up to
// the stage that refused. Stored values follow --debug; the user's own words
// may appear (decision 013: stdout diagnostics may echo the sentence).
func explainRun(res pipeline.Result, runErr error, opt options, stdout io.Writer) {
	fmt.Fprintln(stdout, "explain:")
	fmt.Fprintf(stdout, "  request: %q\n", opt.query)
	if runErr != nil {
		fmt.Fprintf(stdout, "  refused at: %s (%s)\n", res.Stage, qferr.CodeOf(runErr))
	}
	reached := func(s pipeline.Stage) bool {
		order := []pipeline.Stage{pipeline.StageInput, pipeline.StageExtract, pipeline.StageRetrieve,
			pipeline.StageRank, pipeline.StageBind, pipeline.StagePlan, pipeline.StageCompile, pipeline.StageDone}
		pos := map[pipeline.Stage]int{}
		for i, st := range order {
			pos[st] = i
		}
		return pos[res.Stage] > pos[s]
	}
	if reached(pipeline.StageRetrieve) {
		fmt.Fprintln(stdout, "  reading:")
		for _, item := range readingItems(res.Analysis, res.Ranking) {
			if item.typed != nil {
				fmt.Fprintf(stdout, "    %-24s value  %s\n", quoteTrunc(item.typed.Text, 24), typedDescription(*item.typed))
				continue
			}
			s := res.Ranking.Spans[item.span]
			top, _ := s.TopCandidate()
			fmt.Fprintf(stdout, "    %-24s %-9s %s\n", quoteTrunc(s.Span.Text, 24), s.Decision, top.DisplayTarget(opt.debug))
		}
	}
	if reached(pipeline.StageBind) {
		fmt.Fprintln(stdout, "  binding:")
		for _, line := range splitLines(res.Binding.Explain(opt.debug)) {
			fmt.Fprintf(stdout, "    %s\n", line)
		}
	}
	if reached(pipeline.StagePlan) {
		fmt.Fprintln(stdout, "  plan:")
		for _, line := range splitLines(res.Plan.Explain(opt.debug)) {
			fmt.Fprintf(stdout, "    %s\n", line)
		}
	}
}

type jsonParam struct {
	Index int    `json:"index"`
	Type  string `json:"type"`
	Value any    `json:"value"`
}

func paramsJSON(q compiler.Query, reveal bool) []jsonParam {
	out := []jsonParam{}
	for _, pr := range q.Params {
		v := pr.Driver()
		if pr.Value.Kind == plan.StringValue && !reveal {
			v = qferr.Placeholder
		}
		out = append(out, jsonParam{Index: pr.Index, Type: string(pr.Value.Kind), Value: v})
	}
	return out
}

func emitQueryJSON(res pipeline.Result, runErr error, opt options, stdout io.Writer) error {
	out := struct {
		OK        bool            `json:"ok"`
		SQL       string          `json:"sql,omitempty"`
		Params    []jsonParam     `json:"params,omitempty"`
		Error     string          `json:"error,omitempty"`
		Stage     string          `json:"stage"`
		Telemetry *pipeline.Stats `json:"telemetry,omitempty"`
	}{OK: runErr == nil, Stage: string(res.Stage)}
	if runErr != nil {
		out.Error = string(qferr.CodeOf(runErr))
	} else {
		out.SQL = res.Query.SQL
		out.Params = paramsJSON(res.Query, true)
	}
	if opt.debug {
		out.Telemetry = &res.Stats
	}
	return writeJSON(stdout, out)
}

// writeJSON encodes without HTML escaping, so the SQL's < and > survive.
func writeJSON(w io.Writer, v any) error {
	var buf strings.Builder
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return qferr.Wrap(qferr.CodeInternal, err, "output could not be encoded")
	}
	fmt.Fprint(w, buf.String())
	return nil
}
