package ranking

import (
	"path/filepath"
	"sort"
	"testing"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/retrieval"
)

// Every test in this package runs without a model. Retrieval results are
// SCRIPTED: each test says what retrieval returned for each phrase, with exact
// scores, and asserts what ranking decided. That is what lets the threshold and
// margin tests be exact to the decimal, and it separates two questions the
// real-model probe mixes together — "did retrieval find it?" (Phase 2) and "did
// ranking decide well given what retrieval found?" (Phase 4).
//
// Where a script reproduces a real run, the scores are the ones MiniLM
// actually produced, recorded in docs/phase-2-results.md or
// docs/phase-4-results.md, so the regression is against the observed failure
// rather than an invented one.

func loadRegistry(t *testing.T, name string) *config.SchemaRegistry {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "configs", name))
	if err != nil {
		t.Fatalf("%s rejected: %v", name, err)
	}
	return config.NewSchemaRegistry(cfg)
}

// hit is one scripted retrieval match. surface names which of the entry's
// surfaces matched; "" means its first surface.
type hit struct {
	id      string
	score   float64
	surface string
}

// script builds a ranking Input for sentence, with retrieval returning hits
// for each span whose normalised text is a key of hits, and nothing for every
// other span.
func script(t *testing.T, reg *config.SchemaRegistry, sentence string, hits map[string][]hit) Input {
	t.Helper()
	entries := map[string]corpus.Entry{}
	for _, e := range corpus.Build(reg) {
		entries[e.ID] = e
	}

	a := nlp.Analyze(sentence, nlp.DefaultOptions)
	retrieved := make([][]retrieval.Match, len(a.Spans))
	used := map[string]bool{}
	for i, s := range a.Spans {
		if s.Kind != nlp.SpanText {
			continue
		}
		hs, ok := hits[s.Norm]
		if !ok {
			continue
		}
		used[s.Norm] = true
		var ms []retrieval.Match
		for _, h := range hs {
			e, ok := entries[h.id]
			if !ok {
				t.Fatalf("script names %q, which is not in the corpus", h.id)
			}
			m := retrieval.Match{Entry: e, Score: h.score}
			if len(e.Surfaces) > 0 {
				idx := 0
				if h.surface != "" {
					idx = -1
					for k, sf := range e.Surfaces {
						if sf.Text == h.surface {
							idx = k
						}
					}
					if idx < 0 {
						t.Fatalf("%s has no surface %q", h.id, h.surface)
					}
				}
				sf := e.Surfaces[idx]
				m.Surface = &sf
			}
			ms = append(ms, m)
		}
		// Retrieval's own order: score, then id.
		sort.SliceStable(ms, func(x, y int) bool {
			if ms[x].Score != ms[y].Score {
				return ms[x].Score > ms[y].Score
			}
			return ms[x].Entry.ID < ms[y].Entry.ID
		})
		retrieved[i] = ms
	}
	for k := range hits {
		if !used[k] {
			t.Fatalf("script has hits for %q, which is not a span of %q", k, sentence)
		}
	}
	return Input{Analysis: a, Retrieved: retrieved, Registry: reg}
}

func mustRank(t *testing.T, in Input, p Policy) Result {
	t.Helper()
	res, err := Rank(in, p)
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	return res
}

// spanNamed returns the ranking of the span with the given normalised text.
func spanNamed(t *testing.T, res Result, norm string) SpanRanking {
	t.Helper()
	for _, s := range res.Spans {
		if s.Span.Norm == norm {
			return s
		}
	}
	t.Fatalf("no span %q", norm)
	return SpanRanking{}
}

// covered returns the cover as norm → span ranking.
func covered(res Result) map[string]SpanRanking {
	out := map[string]SpanRanking{}
	for _, i := range res.Cover {
		out[res.Spans[i].Span.Norm] = res.Spans[i]
	}
	return out
}

func topID(s SpanRanking) string {
	c, ok := s.TopCandidate()
	if !ok {
		return ""
	}
	return c.Entry.ID
}

// withSharedValue returns ambiguity.json with the alias "Freedonia" ALSO
// configured on payment.status. Decision 019 allows only values the sentence
// contains literally to compete, so a real value-versus-value ambiguity needs
// one word configured as a value of two fields. The fixture has none; tests
// build it here rather than change a shared fixture.
func withSharedValue(t *testing.T) *config.SchemaRegistry {
	t.Helper()
	cfg := *loadRegistry(t, "ambiguity.json").Config()
	cfg.Fields = append([]config.Field(nil), cfg.Fields...)
	for i := range cfg.Fields {
		f := &cfg.Fields[i]
		if f.Entity == "payment" && f.Name == "status" {
			aliases := map[string]string{"Freedonia": "settled_qx"}
			for k, v := range f.ValueAliases {
				aliases[k] = v
			}
			f.ValueAliases = aliases
		}
	}
	return config.NewSchemaRegistry(&cfg)
}
