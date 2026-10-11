package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode"

	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/pipeline"
	"queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/ranking"
)

// This file implements --e2e: the Phase 7 manual gate's query set.
//
// Section 15: "Create at least 50 manually reviewed natural-language queries
// ... For every example record: input, expected semantic meaning, expected
// query shape, actual query, pass/fail, failure category."
//
// The case file holds the first three (written and reviewed by hand); this
// mode produces the other three against one warm pipeline, with the file's
// pinned clock so dates are reproducible. The report is deterministic —
// no timings — so the gate can diff it against its reviewed golden.
//
// Each case expects either a query ("sql", checked by required substrings
// and, optionally, parameter values) or a refusal code. A refusal the case
// asked for is a PASS: refusing is the specified behaviour for ambiguous,
// unsupported and adversarial input.

type e2eFile struct {
	// Title heads the report. Optional; the Phase 7 set predates it and keeps
	// its heading, so its golden is unchanged.
	Title  string    `json:"title,omitempty"`
	Config string    `json:"config"`
	Now    string    `json:"now"`
	TZ     string    `json:"tz"`
	Cases  []e2eCase `json:"cases"`
}

type e2eCase struct {
	ID       string `json:"id"`
	Category string `json:"category"`
	Input    string `json:"input"`
	// Meaning is the expected semantic meaning, in words, for the reviewer.
	Meaning string `json:"meaning"`
	// Expect is "sql", an error code, or "REFUSED" (Phase 8): any refusal
	// passes. REFUSED is for adversarial input where the brief's gate asks
	// only for "an explicit rejection" and which rule refuses first is not
	// what the case is about; a case that tests a rule names its code.
	Expect string `json:"expect"`
	// Shape lists substrings the SQL must contain; Absent, ones it must not.
	Shape  []string `json:"shape,omitempty"`
	Absent []string `json:"absent,omitempty"`
	// Params, when given, are the expected parameter values in order, as
	// rendered (numbers plain, strings unquoted, instants RFC 3339).
	Params []string `json:"params,omitempty"`
	// Known marks a case that is expected to FAIL today, with the reason. It
	// still counts as a failure; the mark only stops it from being
	// mistaken for a regression.
	Known string `json:"known,omitempty"`
}

type e2eRow struct {
	c        e2eCase
	pass     bool
	actual   string // SQL + params, or the refusal
	code     qferr.Code
	stage    pipeline.Stage
	category string
	why      string
	// semanticValues lists value readings accepted with NO lexical evidence:
	// the reading rests on embedding similarity alone ("living" → the alias
	// "live"). Measured, not judged — see phase-7-results.md, finding V02.
	semanticValues []string
}

func loadE2E(path string) (e2eFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return e2eFile{}, qferr.Wrap(qferr.CodeInvalidInput, err, "the case file could not be read")
	}
	var f e2eFile
	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&f); err != nil {
		return e2eFile{}, qferr.Wrap(qferr.CodeInvalidInput, err, "the case file is not valid")
	}
	seen := map[string]bool{}
	for _, c := range f.Cases {
		if c.ID == "" || seen[c.ID] || c.Input == "" || c.Expect == "" || c.Meaning == "" {
			return e2eFile{}, qferr.New(qferr.CodeInvalidInput, "case %q is missing a field or repeats an id", c.ID)
		}
		seen[c.ID] = true
	}
	return f, nil
}

func runE2E(ctx context.Context, opt options, stdout, stderr io.Writer) error {
	f, err := loadE2E(opt.e2e)
	if err != nil {
		return err
	}
	// The case file names its config; --config must agree, so a report is
	// never produced against a different schema than its expectations.
	if filepath.Base(opt.configPath) != f.Config {
		return qferr.New(qferr.CodeInvalidInput, "the case file is written for %s, but --config is %s", f.Config, filepath.Base(opt.configPath))
	}
	opt.now, opt.tz = f.Now, f.TZ
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

	var rows []e2eRow
	for _, c := range f.Cases {
		res, runErr := pl.Run(ctx, c.Input, clock)
		rows = append(rows, evaluateE2E(c, res, runErr))
	}
	emitE2E(eng, f, clock, rows, stdout)
	return nil
}

func evaluateE2E(c e2eCase, res pipeline.Result, runErr error) e2eRow {
	row := e2eRow{c: c, stage: res.Stage}
	for _, i := range res.Ranking.Cover {
		sr := res.Ranking.Spans[i]
		top, ok := sr.TopCandidate()
		if ok && sr.Decision == ranking.Accept && top.Entry.Kind == corpus.KindValueAlias && top.Evidence.Lexical == 0 {
			row.semanticValues = append(row.semanticValues, fmt.Sprintf("%q → %s", sr.Span.Text, top.DisplayTarget(true)))
		}
	}
	if runErr != nil {
		row.code = qferr.CodeOf(runErr)
		// The message is structure-only by construction (errors are redacted).
		msg := runErr.Error()
		if qe, ok := runErr.(*qferr.Error); ok {
			msg = qe.Message
		}
		row.actual = fmt.Sprintf("REFUSED %s at %s: %s", row.code, res.Stage, msg)
	} else {
		row.actual = res.Query.Render(true)
	}

	switch {
	case c.Expect == "sql" && runErr != nil:
		row.why = "expected a query, got a refusal"
		row.category = categorize(res.Stage, row.code)
	case c.Expect != "sql" && runErr == nil:
		row.why = "expected " + c.Expect + ", got a query"
		row.category = "SCHEMA_LINKING"
	case c.Expect != "sql":
		row.pass = string(row.code) == c.Expect || c.Expect == expectRefused
		if !row.pass {
			row.why = "expected " + c.Expect + ", got " + string(row.code)
			row.category = categorize(res.Stage, row.code)
		}
	default:
		var missing []string
		for _, s := range c.Shape {
			if !strings.Contains(res.Query.SQL, s) {
				missing = append(missing, s)
			}
		}
		for _, s := range c.Absent {
			if strings.Contains(res.Query.SQL, s) {
				missing = append(missing, "must not contain "+s)
			}
		}
		if len(c.Params) > 0 {
			var got []string
			for _, p := range res.Query.Params {
				got = append(got, paramText(p.Driver()))
			}
			if strings.Join(got, "|") != strings.Join(c.Params, "|") {
				missing = append(missing, fmt.Sprintf("params %v, got %v", c.Params, got))
			}
		}
		row.pass = len(missing) == 0
		if !row.pass {
			row.why = "shape: " + strings.Join(missing, "; ")
			row.category = "SCHEMA_LINKING"
		}
	}
	return row
}

// expectRefused is the expectation "any explicit refusal".
const expectRefused = "REFUSED"

func paramText(v any) string {
	switch x := v.(type) {
	case time.Time:
		return x.Format(time.RFC3339)
	default:
		return fmt.Sprint(x)
	}
}

// categorize maps where a request stopped to the brief's error taxonomy
// (section 18). It is the PRIMARY reason by stage and code; the report's
// reviewer may refine it, and the case file's "known" note records that.
func categorize(stage pipeline.Stage, code qferr.Code) string {
	switch code {
	case qferr.CodeSecurityRejected:
		return "SECURITY_REJECTION"
	case qferr.CodeAmbiguousMatch:
		return "AMBIGUITY"
	case qferr.CodeValueResolutionFailed:
		return "VALUE_RESOLUTION"
	case qferr.CodeRelationshipNotFound:
		return "RELATIONSHIP_REASONING"
	}
	switch stage {
	case pipeline.StageInput, pipeline.StageExtract:
		return "NLP_EXTRACTION"
	case pipeline.StageRetrieve, pipeline.StageRank:
		return "EMBEDDING_RETRIEVAL"
	case pipeline.StageBind:
		if code == qferr.CodeUnsupportedQuery {
			return "UNSUPPORTED_FEATURE"
		}
		return "SCHEMA_LINKING"
	case pipeline.StagePlan:
		return "QUERY_PLANNING"
	case pipeline.StageCompile:
		return "SQL_COMPILATION"
	}
	return "UNSUPPORTED_FEATURE"
}

func emitE2E(eng *engine, f e2eFile, clock plan.Clock, rows []e2eRow, w io.Writer) {
	pass, sqlPass, sqlTotal, refusePass, refuseTotal := 0, 0, 0, 0, 0
	semantic, semanticCompiled := 0, 0
	cats := map[string]int{}
	for _, r := range rows {
		if len(r.semanticValues) > 0 {
			semantic++
			if r.code == "" {
				semanticCompiled++
			}
		}
		if r.pass {
			pass++
		} else {
			cats[r.category]++
		}
		if r.c.Expect == "sql" {
			sqlTotal++
			if r.pass {
				sqlPass++
			}
		} else {
			refuseTotal++
			if r.pass {
				refusePass++
			}
		}
	}
	title := f.Title
	if title == "" {
		title = "Phase 7 end-to-end query set"
	}
	fmt.Fprintf(w, "# %s\n\n", title)
	fmt.Fprintf(w, "config `%s` · model `%s` · clock `%s` · %d cases\n\n", f.Config, eng.index.Identity(),
		clock.Now.In(clock.Location).Format(time.RFC3339), len(rows))
	fmt.Fprintf(w, "**%d/%d pass** — queries %d/%d, specified refusals %d/%d\n\n", pass, len(rows), sqlPass, sqlTotal, refusePass, refuseTotal)
	fmt.Fprintf(w, "Value readings accepted on similarity alone (no lexical evidence): %d case(s), %d of them compiled to a query\n\n", semantic, semanticCompiled)
	if len(cats) > 0 {
		fmt.Fprintf(w, "Failures by primary category (section 18 taxonomy):\n\n")
		var keys []string
		for k := range cats {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			fmt.Fprintf(w, "- %s: %d\n", k, cats[k])
		}
		fmt.Fprintln(w)
	}
	fmt.Fprintf(w, "| id | category | input | expected | result | failure category |\n|---|---|---|---|---|---|\n")
	for _, r := range rows {
		result := "PASS"
		if !r.pass {
			result = "**FAIL**"
			if r.c.Known != "" {
				result = "**FAIL** (known)"
			}
		}
		fmt.Fprintf(w, "| %s | %s | %s | %s | %s | %s |\n", r.c.ID, r.c.Category, mdCell(r.c.Input), r.c.Expect, result, dashIfEmpty(r.category))
	}
	fmt.Fprintf(w, "\n## Every case\n")
	for _, r := range rows {
		result := "PASS"
		if !r.pass {
			result = "FAIL — " + r.why
		}
		fmt.Fprintf(w, "\n### %s — %s\n\n", r.c.ID, result)
		fmt.Fprintf(w, "- input: `%s`\n- expected meaning: %s\n- expected: %s", visible(r.c.Input), r.c.Meaning, r.c.Expect)
		if len(r.c.Shape) > 0 {
			fmt.Fprintf(w, "; shape contains %s", backticks(r.c.Shape))
		}
		if len(r.c.Absent) > 0 {
			fmt.Fprintf(w, "; never %s", backticks(r.c.Absent))
		}
		fmt.Fprintln(w)
		if r.c.Known != "" {
			fmt.Fprintf(w, "- known: %s\n", r.c.Known)
		}
		if r.category != "" {
			fmt.Fprintf(w, "- failure category: %s\n", r.category)
		}
		for _, v := range r.semanticValues {
			fmt.Fprintf(w, "- value accepted on similarity alone: %s\n", v)
		}
		fmt.Fprintf(w, "\n```sql\n%s\n```\n", strings.TrimRight(r.actual, "\n"))
	}
}

func mdCell(s string) string { return "`" + strings.ReplaceAll(visible(s), "|", `\|`) + "`" }

// visible escapes control and invisible format characters (a NUL, a
// zero-width space, a right-to-left override) as \uXXXX, so a report shows
// what the input held instead of hiding it — or, for a NUL, turning the
// report into a binary file. Phase 8's adversarial set has such inputs; the
// Phase 7 set has none, so its report is unchanged.
func visible(s string) string {
	var b strings.Builder
	for _, r := range s {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			fmt.Fprintf(&b, `\u%04x`, r)
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func backticks(xs []string) string {
	out := make([]string, len(xs))
	for i, x := range xs {
		out[i] = "`" + x + "`"
	}
	return strings.Join(out, ", ")
}

func dashIfEmpty(s string) string {
	if s == "" {
		return "—"
	}
	return s
}
