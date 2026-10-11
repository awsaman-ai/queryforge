package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/retrieval"
)

// --probe runs a whole file of queries against ONE loaded model and reports
// Top-1 and Top-3 accuracy against expected corpus entries.
//
// It exists because the Phase 2 manual gate asks for a recorded table — query,
// expected target, Top-1, Top-3, score, model, notes — and producing that by
// running the CLI once per query would reload the model each time. That would
// be slow, and it would measure the prohibited pattern from section 17 rather
// than the architecture under test.
//
// It is a measurement tool, not part of the pipeline. Nothing in a request path
// calls it.

// probeCase is one row of the probe file.
type probeCase struct {
	Query string
	// Expected is the corpus entry id that should be retrieved, or "" for an
	// exploratory query with no committed expectation.
	Expected string
	Note     string
	Line     int
}

// probeResult is what one case produced.
type probeResult struct {
	Case probeCase
	// Top holds the ranked matches, trimmed to what the report needs.
	Top []retrieval.Match
	// Top1 and Top3 report whether Expected was retrieved at that depth.
	Top1, Top3 bool
	// Rank is where Expected appeared, or 0 if it did not appear at all.
	Rank int
}

// loadProbeFile reads a TSV of query<TAB>expected-id<TAB>note.
func loadProbeFile(path string) ([]probeCase, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, qferr.Wrap(qferr.CodeInvalidInput, err, "probe file could not be read: %s", path)
	}
	defer f.Close()

	var cases []probeCase
	scanner := bufio.NewScanner(f)
	line := 0
	for scanner.Scan() {
		line++
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}
		parts := strings.Split(text, "\t")
		c := probeCase{Query: strings.TrimSpace(parts[0]), Line: line}
		if len(parts) > 1 {
			c.Expected = strings.TrimSpace(parts[1])
		}
		if len(parts) > 2 {
			c.Note = strings.TrimSpace(parts[2])
		}
		if c.Query == "" {
			return nil, qferr.New(qferr.CodeInvalidInput, "probe file line %d has no query", line)
		}
		cases = append(cases, c)
	}
	if err := scanner.Err(); err != nil {
		return nil, qferr.Wrap(qferr.CodeInvalidInput, err, "probe file could not be read")
	}
	if len(cases) == 0 {
		return nil, qferr.New(qferr.CodeInvalidInput, "probe file contains no queries")
	}
	return cases, nil
}

// runProbe embeds every query against one warm model and reports the results.
func runProbe(ctx context.Context, opt options, stdout, stderr io.Writer) error {
	cases, err := loadProbeFile(opt.probe)
	if err != nil {
		return err
	}

	eng, err := newEngine(ctx, opt, stderr)
	if err != nil {
		return err
	}
	defer eng.Close()

	if eng.provider == nil {
		provider, perr := newProvider(ctx, opt, stderr)
		if perr != nil {
			return perr
		}
		eng.provider = provider
	}

	// Whether these numbers may be reported at all. A table produced by the
	// hashing embedder is a mechanism check; presenting it as a retrieval
	// result would be exactly the false claim section 29 warns about.
	identity := eng.index.Identity()

	results := make([]probeResult, 0, len(cases))
	var totalEmbed time.Duration

	for _, c := range cases {
		start := time.Now()
		q, err := eng.provider.EmbedOne(ctx, c.Query)
		if err != nil {
			return err
		}
		totalEmbed += time.Since(start)

		matches, err := eng.index.Search(q, maxInt(opt.topK, 3))
		if err != nil {
			return err
		}

		r := probeResult{Case: c, Top: matches}
		if c.Expected != "" {
			for i, m := range matches {
				if m.Entry.ID == c.Expected {
					r.Rank = i + 1
					r.Top1 = i == 0
					r.Top3 = i < 3
					break
				}
			}
		}
		results = append(results, r)
	}

	emitProbeReport(eng, opt, results, totalEmbed, identity.Semantic, stdout)
	return nil
}

func emitProbeReport(eng *engine, opt options, results []probeResult, totalEmbed time.Duration, semantic bool, stdout io.Writer) {
	fmt.Fprintf(stdout, "model:    %s\n", eng.index.Identity())
	fmt.Fprintf(stdout, "strategy: %s   corpus: %d entries, %d vectors\n",
		eng.index.Strategy(), eng.index.Len(), eng.index.VectorCount())
	fmt.Fprintf(stdout, "queries:  %d, mean embed %s\n\n",
		len(results), (totalEmbed / time.Duration(len(results))).Round(time.Microsecond))

	if !semantic {
		fmt.Fprintln(stdout, "!! NON-SEMANTIC PROVIDER — these numbers are a mechanism check, not a result !!")
		fmt.Fprintln(stdout)
	}

	fmt.Fprintln(stdout, "| query | expected | top-1 | top-3 | rank | top-1 hit | score | note |")
	fmt.Fprintln(stdout, "|---|---|---|---|---|---|---|---|")

	var scored, top1, top3 int
	for _, r := range results {
		best := "—"
		score := "—"
		if len(r.Top) > 0 {
			best = shortID(corpus.DisplayID(r.Top[0].Entry.ID, opt.debug))
			score = fmt.Sprintf("%.3f", r.Top[0].Score)
		}

		expected := r.Case.Expected
		t1, t3, rank := "—", "—", "—"
		if expected == "" {
			expected = "—"
		} else {
			scored++
			t1, t3 = "no", "no"
			if r.Top1 {
				t1 = "**yes**"
				top1++
			}
			if r.Top3 {
				t3 = "yes"
				top3++
			}
			if r.Rank > 0 {
				rank = fmt.Sprintf("%d", r.Rank)
			} else {
				rank = "miss"
			}
		}

		fmt.Fprintf(stdout, "| %s | %s | %s | %s | %s | %s | %s | %s |\n",
			r.Case.Query, shortID(corpus.DisplayID(expected, opt.debug)), t1, t3, rank, best, score, r.Case.Note)
	}

	if scored > 0 {
		fmt.Fprintf(stdout, "\n**Top-1: %d/%d (%.0f%%)  ·  Top-3: %d/%d (%.0f%%)**\n",
			top1, scored, 100*float64(top1)/float64(scored),
			top3, scored, 100*float64(top3)/float64(scored))
	}

	// Misses in full, because section 10 says not to claim semantic
	// understanding from a handful of successes and to keep false positives in
	// the report. A summary percentage hides exactly what is worth reading.
	var misses []probeResult
	for _, r := range results {
		if r.Case.Expected != "" && !r.Top1 {
			misses = append(misses, r)
		}
	}
	if len(misses) > 0 {
		fmt.Fprintf(stdout, "\n### Misses (%d)\n\n", len(misses))
		sort.Slice(misses, func(i, j int) bool { return misses[i].Case.Query < misses[j].Case.Query })
		for _, r := range misses {
			fmt.Fprintf(stdout, "- `%s` → expected `%s`", r.Case.Query, shortID(corpus.DisplayID(r.Case.Expected, opt.debug)))
			if r.Rank > 0 {
				fmt.Fprintf(stdout, ", found at rank %d", r.Rank)
			} else {
				fmt.Fprintf(stdout, ", not retrieved")
			}
			fmt.Fprintln(stdout)
			for i, m := range r.Top {
				if i >= 3 {
					break
				}
				fmt.Fprintf(stdout, "    %d. %s (%.3f) — %s\n",
					i+1, shortID(corpus.DisplayID(m.Entry.ID, opt.debug)), m.Score, renderMatched(m, opt.debug))
			}
		}
	}
}

// shortID trims the corpus prefix so a table stays readable. The prefix is
// recoverable from the kind column.
func shortID(id string) string {
	if id == "—" || id == "" {
		return id
	}
	if i := strings.Index(id, ":"); i >= 0 && i < len(id)-1 {
		return id[i+1:]
	}
	return id
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
