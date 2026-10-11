package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"time"

	"queryforge.local/v2experiment/internal/binding"
	"queryforge.local/v2experiment/internal/compiler"
	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/graph"
	"queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/readonly"
)

// This file implements the two Phase 6 surfaces.
//
// --compile <plan.json> is the brief's manual gate: "Given manually
// constructed SemanticPlan objects, verify deterministic SQL output." It needs
// the config and nothing else — no model, no sentence.
//
// --plan <sentence> runs extraction through binding and shows the plan the
// planner builds. It deliberately stops there. Emitting SQL from a sentence is
// Phase 7's pipeline, and --query still refuses until then.
//
// REDACTION: string parameters and plan string values are data (config values
// or the user's literals) and print as <redacted> unless --debug (decision
// 006). Numbers, booleans and instants always print. Errors never carry a
// value.

func runCompile(opt options, cfg *config.Config, stdout io.Writer) error {
	data, err := os.ReadFile(opt.compile)
	if err != nil {
		return qferr.Wrap(qferr.CodeInvalidInput, err, "the plan file could not be read")
	}
	p, err := plan.Decode(data)
	if err != nil {
		return err
	}
	reg := config.NewSchemaRegistry(cfg)
	q, err := compiler.Compile(p, reg, graph.New(reg))
	if err != nil {
		return err
	}
	if opt.asJSON {
		out := struct {
			SQL    string      `json:"sql"`
			Params []jsonParam `json:"params"`
		}{SQL: q.SQL, Params: paramsJSON(q, opt.debug)}
		return writeJSON(stdout, out)
	}
	fmt.Fprintln(stdout, q.Render(opt.debug))
	return nil
}

// clockFrom builds the request clock from --now and --tz. --now defaults to
// the wall clock; a gate or a test pins it so dates resolve reproducibly.
func clockFrom(opt options) (plan.Clock, error) {
	loc := time.UTC
	if opt.tz != "" {
		l, err := time.LoadLocation(opt.tz)
		if err != nil {
			return plan.Clock{}, qferr.New(qferr.CodeInvalidInput, "--tz is not a known IANA timezone (%s)", qferr.Sensitive(opt.tz))
		}
		loc = l
	}
	now := time.Now()
	if opt.now != "" {
		t, err := time.Parse(time.RFC3339, opt.now)
		if err != nil {
			return plan.Clock{}, qferr.New(qferr.CodeInvalidInput, "--now must be an RFC 3339 instant such as 2026-09-21T12:00:00Z")
		}
		now = t
	}
	return plan.Clock{Now: now, Location: loc}, nil
}

func runPlan(ctx context.Context, opt options, stdout, stderr io.Writer) error {
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

	a, res, err := rankSentence(ctx, eng, opt.plan, opt, pol)
	if err != nil {
		return err
	}
	// Section 16.1, as in --bind: no plan for a sentence --query refuses.
	if err := readonly.Check(a, res); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "query:    %q\n", opt.plan)
	fmt.Fprintf(stdout, "clock:    %s\n\n", clock.Now.In(clock.Location).Format(time.RFC3339))

	b, err := binding.Bind(binding.Input{Analysis: a, Ranking: res, Registry: eng.registry, Graph: graph.New(eng.registry)})
	if err != nil {
		// Say which stage refused: a binding refusal is not a planning one.
		fmt.Fprintf(stdout, "binding:  REFUSED (%s)\n", qferr.CodeOf(err))
		return err
	}
	p, err := plan.FromBinding(b, eng.registry, clock)
	if opt.debug || err != nil {
		fmt.Fprintln(stdout, "binding:")
		for _, line := range splitLines(b.Explain(opt.debug)) {
			fmt.Fprintf(stdout, "  %s\n", line)
		}
		fmt.Fprintln(stdout)
	}
	if err != nil {
		fmt.Fprintf(stdout, "plan:     REFUSED (%s)\n", qferr.CodeOf(err))
		return err
	}
	fmt.Fprintln(stdout, "plan:")
	for _, line := range splitLines(p.Explain(opt.debug)) {
		fmt.Fprintf(stdout, "  %s\n", line)
	}
	return nil
}
