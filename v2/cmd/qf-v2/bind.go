package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"time"

	"queryforge.local/v2experiment/internal/binding"
	"queryforge.local/v2experiment/internal/graph"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/readonly"
)

// This file implements --bind: the Phase 5 surface.
//
// It runs extraction (3), retrieval (2), ranking (4) and binding (5) and shows
// the bound meaning: root entity, join paths with their keys, conditions, and
// what is aggregated, ranked or selected. It is still NOT the query pipeline:
// a binding is checked meaning, not SQL. The planner (6) and compiler stand
// between, and --query still refuses.
//
// DIAGNOSTIC OUTPUT (decision 013): the report may echo the user's sentence;
// stored values are redacted unless --debug. Errors locate phrases by token
// position and quote them only under --debug.

func runBind(ctx context.Context, opt options, stdout, stderr io.Writer) error {
	eng, err := warmEngine(ctx, opt, stderr)
	if err != nil {
		return err
	}
	defer eng.Close()
	p, err := policyFrom(opt, eng.registry)
	if err != nil {
		return err
	}

	start := time.Now()
	a, res, err := rankSentence(ctx, eng, opt.bind, opt, p)
	if err != nil {
		return err
	}
	// Section 16.1: a write request is refused before binding here too, so
	// --bind never binds a sentence --query refuses (decision 021).
	if err := readonly.Check(a, res); err != nil {
		return err
	}
	rankTime := time.Since(start)

	start = time.Now()
	b, bindErr := binding.Bind(binding.Input{
		Analysis: a, Ranking: res, Registry: eng.registry, Graph: graph.New(eng.registry),
	})
	bindTime := time.Since(start)

	if opt.asJSON {
		if err := emitBindJSON(opt, b, bindErr, stdout); err != nil {
			return err
		}
		return bindErr
	}

	fmt.Fprintf(stdout, "query:    %q\n", opt.bind)
	fmt.Fprintf(stdout, "model:    %s\n", eng.index.Identity())
	if opt.debug {
		fmt.Fprintf(stdout, "timing:   extract+retrieve+rank %s, bind %s\n",
			rankTime.Round(time.Microsecond), bindTime.Round(time.Microsecond))
	}
	fmt.Fprintln(stdout)

	if opt.debug || bindErr != nil {
		// The reading binding worked from — always shown on a refusal, so the
		// refusal can be argued with.
		fmt.Fprintln(stdout, "reading:")
		for _, item := range readingItems(a, res) {
			if item.typed != nil {
				fmt.Fprintf(stdout, "  %-24s value  %s\n", quoteTrunc(item.typed.Text, 24), typedDescription(*item.typed))
				continue
			}
			s := res.Spans[item.span]
			top, _ := s.TopCandidate()
			fmt.Fprintf(stdout, "  %-24s %-9s %s\n", quoteTrunc(s.Span.Text, 24), s.Decision, top.DisplayTarget(opt.debug))
		}
		fmt.Fprintln(stdout)
	}

	if bindErr != nil {
		fmt.Fprintf(stdout, "binding:  REFUSED (%s)\n", qferr.CodeOf(bindErr))
		return bindErr
	}
	fmt.Fprintln(stdout, "binding:")
	for _, line := range splitLines(b.Explain(opt.debug)) {
		fmt.Fprintf(stdout, "  %s\n", line)
	}
	return nil
}

func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}

func emitBindJSON(opt options, b binding.Binding, bindErr error, stdout io.Writer) error {
	type jsonPath struct {
		Path  string   `json:"path"`
		Steps []string `json:"steps"`
	}
	toPath := func(p graph.Path) *jsonPath {
		if p.Len() == 0 {
			return nil
		}
		jp := &jsonPath{Path: p.String()}
		for _, s := range p.Steps {
			jp.Steps = append(jp.Steps, fmt.Sprintf("%s.%s = %s.%s", s.From.Entity, s.From.Field, s.To.Entity, s.To.Field))
		}
		return jp
	}
	type jsonCond struct {
		Kind         string    `json:"kind"`
		Join         string    `json:"join,omitempty"`
		Field        string    `json:"field,omitempty"`
		Relationship string    `json:"relationship,omitempty"`
		Op           string    `json:"op,omitempty"`
		Value        string    `json:"value,omitempty"`
		Negated      bool      `json:"negated,omitempty"`
		Inferred     bool      `json:"inferred,omitempty"`
		Via          *jsonPath `json:"via,omitempty"`
	}
	out := struct {
		Query      string     `json:"query"`
		Bound      bool       `json:"bound"`
		Error      string     `json:"error,omitempty"`
		Root       string     `json:"root,omitempty"`
		Joins      []jsonPath `json:"joins,omitempty"`
		Conditions []jsonCond `json:"conditions,omitempty"`
		Aggregates []string   `json:"aggregates,omitempty"`
		Pattern    string     `json:"pattern,omitempty"`
		Selections []string   `json:"selections,omitempty"`
		Notes      []string   `json:"notes,omitempty"`
	}{Query: opt.bind, Bound: bindErr == nil}
	if bindErr != nil {
		out.Error = string(qferr.CodeOf(bindErr))
	} else {
		out.Root = b.Root
		for _, p := range b.Joins {
			out.Joins = append(out.Joins, *toPath(p))
		}
		for _, c := range b.Conditions {
			jc := jsonCond{Kind: string(c.Kind), Join: c.Join, Op: string(c.Op), Negated: c.Negated,
				Inferred: c.Inferred, Relationship: c.Relationship, Via: toPath(c.Path)}
			if c.Field.Field != "" {
				jc.Field = c.Field.String()
			}
			if c.Kind != binding.CondExists {
				jc.Value = valueText(c.Value, opt.debug)
			}
			out.Conditions = append(out.Conditions, jc)
		}
		for _, a := range b.Aggregates {
			target := a.Entity + ".*"
			if a.Field != nil {
				target = a.Field.String()
			}
			out.Aggregates = append(out.Aggregates, fmt.Sprintf("%s(%s)", a.Func, target))
		}
		if p := b.Pattern; p != nil {
			out.Pattern = fmt.Sprintf("%s N=%d", p.Name, p.N)
			if p.Metric != nil {
				out.Pattern += " metric=" + p.Metric.String()
			}
		}
		for _, s := range b.Selections {
			out.Selections = append(out.Selections, s.Field.String())
		}
		out.Notes = b.Notes
	}
	enc, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return qferr.Wrap(qferr.CodeInternal, err, "bind output could not be encoded")
	}
	fmt.Fprintln(stdout, string(enc))
	return nil
}

// valueText renders a condition value through the binding package's own
// redaction: the Explain rendering of a one-condition binding.
func valueText(v binding.Value, reveal bool) string {
	return v.Render(reveal)
}
