package main

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/numeric"
	"queryforge.local/v2experiment/internal/qferr"
)

// --analyze shows what Phase 3 makes of a sentence: tokens, numbers, dates,
// connectives and the candidate spans.
//
// It exists for the Phase 3 manual gate, which asks that the same semantic
// concept produce stable spans across paraphrases (section 11). That is not
// checkable without being able to see the spans.
//
// It needs no config and no model: extraction is config-independent by design.
// What a span MEANS depends on the schema; what a span IS does not.

func runAnalyze(opt options, stdout io.Writer) error {
	if strings.TrimSpace(opt.analyze) == "" {
		return qferr.New(qferr.CodeInvalidInput, "--analyze needs a sentence")
	}

	limits := nlp.DefaultOptions
	if opt.maxSpanTokens > 0 {
		limits.MaxSpanTokens = opt.maxSpanTokens
	}

	a := nlp.Analyze(opt.analyze, limits)

	// The complexity limits of section 16.7 are refusals, not truncations:
	// quietly dropping spans would answer a different question than the one
	// asked. The config's own limit applies once a config is loaded; this is
	// the extraction-layer default.
	if len(a.Spans) > limits.MaxSpans {
		return qferr.New(qferr.CodeInvalidInput,
			"input produced %d candidate spans, over the limit of %d", len(a.Spans), limits.MaxSpans)
	}

	if opt.asJSON {
		return emitAnalyzeJSON(a, stdout)
	}
	emitAnalyzeText(a, opt, stdout)
	return nil
}

func emitAnalyzeText(a nlp.Analysis, opt options, stdout io.Writer) {
	fmt.Fprintf(stdout, "input:      %q\n", a.Input)
	if a.Normalized != a.Input {
		fmt.Fprintf(stdout, "normalized: %q\n", a.Normalized)
	}
	fmt.Fprintf(stdout, "tokens:     %d   spans: %d\n\n", len(a.Tokens), len(a.Spans))

	if opt.debug {
		fmt.Fprintln(stdout, "tokens:")
		for _, t := range a.Tokens {
			stop := ""
			if t.Stop {
				stop = "  (stop)"
			}
			fmt.Fprintf(stdout, "  %2d  %-8s %-16q [%d:%d]%s\n", t.Index, t.Kind, t.Text, t.Start, t.End, stop)
		}
		fmt.Fprintln(stdout)
	}

	if len(a.Numbers) > 0 {
		fmt.Fprintln(stdout, "numbers:")
		for _, n := range a.Numbers {
			extra := ""
			if n.Currency != "" {
				extra += "  currency=" + n.Currency
			}
			if n.Percent {
				extra += "  percent"
			}
			if n.Spelled {
				extra += "  spelled"
			}
			// Exact digits (decision 022 §2), never %g's exponent form; a
			// number the parser cannot read exactly says so.
			value := n.Value.String()
			if n.Malformed {
				value = "malformed (not read)"
			}
			fmt.Fprintf(stdout, "  %-24q -> %s%s\n", n.Text, value, extra)
		}
		fmt.Fprintln(stdout)
	}

	if len(a.Dates) > 0 {
		fmt.Fprintln(stdout, "dates:")
		for _, d := range a.Dates {
			switch d.Anchor {
			case nlp.AnchorAbsolute:
				if d.Month != 0 {
					fmt.Fprintf(stdout, "  %-24q -> absolute %s %d\n", d.Text, d.Month, d.Year)
				} else {
					fmt.Fprintf(stdout, "  %-24q -> absolute year %d\n", d.Text, d.Year)
				}
			case nlp.AnchorRolling:
				fmt.Fprintf(stdout, "  %-24q -> rolling %d %s back from now\n", d.Text, d.Count, d.Unit)
			default:
				fmt.Fprintf(stdout, "  %-24q -> calendar %s, offset %d, count %d\n", d.Text, d.Unit, d.Offset, d.Count)
			}
		}
		fmt.Fprintln(stdout)
	}

	if len(a.Connectives) > 0 {
		fmt.Fprint(stdout, "connectives: ")
		for i, c := range a.Connectives {
			if i > 0 {
				fmt.Fprint(stdout, ", ")
			}
			fmt.Fprintf(stdout, "%s(%q)", c.Op, c.Text)
		}
		fmt.Fprint(stdout, "\n\n")
	}

	fmt.Fprintln(stdout, "candidate spans (longest first):")
	for _, s := range a.Spans {
		tag := ""
		switch s.Kind {
		case nlp.SpanNumber:
			tag = "  [number]"
		case nlp.SpanDate:
			tag = "  [date]"
		}
		fmt.Fprintf(stdout, "  %-32q tokens[%d:%d]%s\n", s.Text, s.Start, s.End, tag)
	}
}

func emitAnalyzeJSON(a nlp.Analysis, stdout io.Writer) error {
	type jsonSpan struct {
		Kind  string `json:"kind"`
		Text  string `json:"text"`
		Start int    `json:"start"`
		End   int    `json:"end"`
	}
	type jsonNumber struct {
		Text string `json:"text"`
		// Value is the exact literal; absent for a malformed number.
		Value     *numeric.Decimal `json:"value,omitempty"`
		Malformed bool             `json:"malformed,omitempty"`
		Currency  string           `json:"currency,omitempty"`
		Percent   bool             `json:"percent,omitempty"`
		Spelled   bool             `json:"spelled,omitempty"`
	}
	type jsonDate struct {
		Text   string `json:"text"`
		Unit   string `json:"unit"`
		Anchor string `json:"anchor"`
		Count  int    `json:"count"`
		Offset int    `json:"offset,omitempty"`
		Year   int    `json:"year,omitempty"`
		Month  int    `json:"month,omitempty"`
	}

	out := struct {
		Input       string       `json:"input"`
		Normalized  string       `json:"normalized"`
		TokenCount  int          `json:"tokenCount"`
		Numbers     []jsonNumber `json:"numbers,omitempty"`
		Dates       []jsonDate   `json:"dates,omitempty"`
		Connectives []string     `json:"connectives,omitempty"`
		Spans       []jsonSpan   `json:"spans"`
	}{
		Input:      a.Input,
		Normalized: a.Normalized,
		TokenCount: len(a.Tokens),
	}
	for _, n := range a.Numbers {
		jn := jsonNumber{
			Text: n.Text, Malformed: n.Malformed, Currency: n.Currency,
			Percent: n.Percent, Spelled: n.Spelled,
		}
		if !n.Malformed {
			v := n.Value
			jn.Value = &v
		}
		out.Numbers = append(out.Numbers, jn)
	}
	for _, d := range a.Dates {
		out.Dates = append(out.Dates, jsonDate{
			Text: d.Text, Unit: string(d.Unit), Anchor: string(d.Anchor),
			Count: d.Count, Offset: d.Offset, Year: d.Year, Month: int(d.Month),
		})
	}
	for _, c := range a.Connectives {
		out.Connectives = append(out.Connectives, c.Op)
	}
	for _, s := range a.Spans {
		out.Spans = append(out.Spans, jsonSpan{
			Kind: string(s.Kind), Text: s.Text, Start: s.Start, End: s.End,
		})
	}

	enc, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		return qferr.Wrap(qferr.CodeInternal, err, "analysis output could not be encoded")
	}
	fmt.Fprintln(stdout, string(enc))
	return nil
}
