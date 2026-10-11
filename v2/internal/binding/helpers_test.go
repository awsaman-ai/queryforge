package binding

import (
	"path/filepath"
	"sort"
	"testing"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/graph"
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/ranking"
	"queryforge.local/v2experiment/internal/retrieval"
)

// Binding tests run the REAL ranking on SCRIPTED retrieval. Scripting
// retrieval keeps them model-free and exact; running real ranking means a
// binding test cannot pass on a reading ranking would never produce.
//
// The script helper mirrors internal/ranking's (it lives in a _test file
// there and cannot be imported). A shared test package was considered and
// declined: it would be a new exported surface on a layer the user asked not
// to refactor, for roughly sixty lines of test plumbing.

type hit struct {
	id      string
	score   float64
	surface string
}

func registry(t *testing.T, name string) *config.SchemaRegistry {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "configs", name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return config.NewSchemaRegistry(cfg)
}

// bind scripts retrieval for sentence, ranks it with the default policy, and
// binds the result.
func bind(t *testing.T, cfgName, sentence string, hits map[string][]hit) (Binding, error) {
	t.Helper()
	return bindReg(t, registry(t, cfgName), sentence, hits)
}

// bindReg is bind against a registry the test built — for limits a fixture
// does not set.
func bindReg(t *testing.T, reg *config.SchemaRegistry, sentence string, hits map[string][]hit) (Binding, error) {
	t.Helper()
	a, res := rankReg(t, reg, sentence, hits)
	return Bind(Input{Analysis: a, Ranking: res, Registry: reg, Graph: graph.New(reg)})
}

// rankReg is bindReg's first half: the scripted retrieval and the REAL
// ranking, returned so a test can show what ranking accepted before binding
// refused it.
func rankReg(t *testing.T, reg *config.SchemaRegistry, sentence string, hits map[string][]hit) (nlp.Analysis, ranking.Result) {
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
				t.Fatalf("script names %q, not in the corpus", h.id)
			}
			m := retrieval.Match{Entry: e, Score: h.score}
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
			if len(e.Surfaces) > 0 {
				sf := e.Surfaces[idx]
				m.Surface = &sf
			}
			ms = append(ms, m)
		}
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
	res, err := ranking.Rank(ranking.Input{Analysis: a, Retrieved: retrieved, Registry: reg}, ranking.DefaultPolicy())
	if err != nil {
		t.Fatalf("Rank: %v", err)
	}
	return a, res
}

func mustBind(t *testing.T, cfgName, sentence string, hits map[string][]hit) Binding {
	t.Helper()
	b, err := bind(t, cfgName, sentence, hits)
	if err != nil {
		t.Fatalf("%q: %v", sentence, err)
	}
	return b
}

// Common scripted readings on commerce.json, with MiniLM's real top scores.
var (
	hCustomers = []hit{{"entity:customer", 0.884, ""}, {"field:customer.name", 0.755, "customer name"}}
	hOrders    = []hit{{"entity:order", 0.881, ""}, {"relationship:customer_orders", 0.70, "ordered"}}
	hIndia     = []hit{{"value:customer.country:alias:India", 1.0, "India"}, {"field:customer.country", 0.711, "country"}}
	hLaptops   = []hit{{"value:product.category:alias:laptops", 1.0, "laptops"}, {"field:product.category", 0.6, "product category"}}
	hBought    = []hit{{"relationship:customer_orders", 1.0, "bought"}, {"field:order.placed_at", 0.5, "ordered on"}}
	hOrdered   = []hit{{"relationship:customer_orders", 1.0, "ordered"}, {"field:order.placed_at", 0.6, "ordered on"}}
	hMoreThan  = []hit{{"operator:gt", 1.0, "more than"}, {"operator:lte", 0.846, "no more than"}}
	hAbove     = []hit{{"operator:gt", 1.0, "above"}, {"operator:lt", 0.5, "below"}}
	hOver      = []hit{{"operator:gt", 1.0, "over"}, {"operator:lt", 0.5, "under"}}
	hSpent     = []hit{{"field:customer.total_spent", 0.835, "total spent"}, {"field:order.amount", 0.518, "amount"}}
	hOrderAmt  = []hit{{"field:order.amount", 1.0, "order amount"}, {"field:order.id", 0.725, "order number"}}
)
