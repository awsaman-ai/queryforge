package pipeline

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/ranking"
	"queryforge.local/v2experiment/internal/retrieval"
)

// Pipeline tests run EVERY stage for real — extraction, ranking, binding,
// planning, compiling — on SCRIPTED retrieval: the scores come from the real
// model (the same scripts binding's tests use), so a test cannot pass on a
// reading the pipeline would never produce, and no model is needed.
//
// These replace cmd/qf-v2's Phase 0 TestQueryIsNotImplemented, which promised
// that --query would refuse until a real pipeline existed. The pipeline now
// exists; these tests assert what it derives.

type hit struct {
	id    string
	score float64
}

type scripted struct {
	entries map[string]corpus.Entry
	hits    map[string][]hit
	mu      sync.Mutex
	used    map[string]bool
}

func (s *scripted) Retrieve(_ context.Context, texts []string) ([][]retrieval.Match, error) {
	out := make([][]retrieval.Match, len(texts))
	for i, text := range texts {
		for _, h := range s.hits[text] {
			e, ok := s.entries[h.id]
			if !ok {
				return nil, fmt.Errorf("script names %q, not in the corpus", h.id)
			}
			m := retrieval.Match{Entry: e, Score: h.score}
			if len(e.Surfaces) > 0 {
				sf := e.Surfaces[0]
				m.Surface = &sf
			}
			out[i] = append(out[i], m)
		}
		sort.SliceStable(out[i], func(a, b int) bool { return out[i][a].Score > out[i][b].Score })
		s.mu.Lock()
		if s.hits[text] != nil {
			s.used[text] = true
		}
		s.mu.Unlock()
	}
	return out, nil
}

func registry(t *testing.T, name string) *config.SchemaRegistry {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "configs", name))
	if err != nil {
		t.Fatal(err)
	}
	return config.NewSchemaRegistry(cfg)
}

func newScripted(reg *config.SchemaRegistry, hits map[string][]hit) *scripted {
	s := &scripted{entries: map[string]corpus.Entry{}, hits: hits, used: map[string]bool{}}
	for _, e := range corpus.Build(reg) {
		s.entries[e.ID] = e
	}
	return s
}

var clock = plan.Clock{Now: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC), Location: time.UTC}

// Real MiniLM top scores, as in internal/binding's scripts.
var (
	hCustomers = []hit{{"entity:customer", 0.884}, {"field:customer.name", 0.755}}
	hIndia     = []hit{{"value:customer.country:alias:India", 1.0}, {"field:customer.country", 0.711}}
	hSpent     = []hit{{"field:customer.total_spent", 0.835}, {"field:order.amount", 0.518}}
	hMoreThan  = []hit{{"operator:gt", 1.0}, {"operator:lte", 0.846}}
	hBought    = []hit{{"relationship:customer_orders", 1.0}, {"field:order.placed_at", 0.5}}
	hLaptops   = []hit{{"value:product.category:alias:laptops", 1.0}, {"field:product.category", 0.6}}
	hTop       = []hit{{"pattern:top_n", 0.825}, {"pattern:bottom_n", 0.4}}
	hSpending  = []hit{{"field:customer.total_spent", 1.0}}
)

func run(t *testing.T, sentence string, hits map[string][]hit) (Result, error) {
	t.Helper()
	reg := registry(t, "commerce.json")
	s := newScripted(reg, hits)
	p, err := New(reg, s, Options{})
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Run(context.Background(), sentence, clock)
	for k := range hits {
		if !s.used[k] {
			t.Fatalf("script has hits for %q, which is not a span of %q", k, sentence)
		}
	}
	return res, err
}

// The brief's section 15 examples 1 and 2, exactly.
func TestBriefExamplesEndToEnd(t *testing.T) {
	res, err := run(t, "Show customers from India", map[string][]hit{"customers": hCustomers, "india": hIndia})
	if err != nil {
		t.Fatal(err)
	}
	want := `SELECT "t0"."name", "t0"."country", "t0"."status", "t0"."total_spent", "t0"."created_at"
FROM "customers" AS "t0"
WHERE "t0"."country" = $1;`
	if res.Query.SQL != want || res.Query.Params[0].Driver() != "IN" || res.Stage != StageDone {
		t.Errorf("example 1:\n%s\n%v", res.Query.SQL, res.Query.Params)
	}

	res, err = run(t, "Show customers who spent more than 50000", map[string][]hit{
		"customers": hCustomers, "spent": hSpent, "more than": hMoreThan})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasSuffix(res.Query.SQL, `WHERE "t0"."total_spent" > $1;`) || res.Query.Params[0].Driver() != int64(50000) {
		t.Errorf("example 2:\n%s\n%v", res.Query.SQL, res.Query.Params)
	}
}

// Example 3 as the brief words it is refused at binding (rule 12: the metric
// "spent" is claimed by the comparison). Pinned so a change to that is a
// decision, not an accident — see phase-7-results.md.
func TestBriefExample3IsRefusedAtBinding(t *testing.T) {
	res, err := run(t, "Show the top 10 customers from India who spent more than 50000 last year", map[string][]hit{
		"top": hTop, "customers": hCustomers, "india": hIndia, "spent": hSpent, "more than": hMoreThan})
	if err == nil || res.Stage != StageBind || qferr.CodeOf(err) != qferr.CodeUnsupportedQuery {
		t.Fatalf("stage %s, err %v", res.Stage, err)
	}
	// Rephrased with the metric named, it compiles to the brief's shape.
	res, err = run(t, "Show the top 10 customers by spending from India", map[string][]hit{
		"top": hTop, "customers": hCustomers, "spending": hSpending, "india": hIndia})
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`WHERE "t0"."country" = $1`, `ORDER BY "t0"."total_spent" DESC NULLS LAST`, "LIMIT 10;"} {
		if !strings.Contains(res.Query.SQL, want) {
			t.Errorf("missing %q:\n%s", want, res.Query.SQL)
		}
	}
}

// Every stage runs, in order, and a refusal stops at its stage with the
// earlier stages' results kept for --explain.
func TestStagesAndRefusalsStopWhereTheyHappen(t *testing.T) {
	res, err := run(t, "customers who bought laptops", map[string][]hit{
		"customers": hCustomers, "bought": hBought, "laptops": hLaptops})
	if err != nil || res.Stage != StageDone {
		t.Fatalf("stage %s err %v", res.Stage, err)
	}
	if res.Binding.Root != "customer" || len(res.Plan.Where) != 1 || !strings.Contains(res.Query.SQL, "EXISTS (") {
		t.Errorf("stages did not all run:\n%s", res.Query.SQL)
	}

	for _, c := range []struct {
		sentence string
		hits     map[string][]hit
		stage    Stage
		code     qferr.Code
	}{
		{"", nil, StageInput, qferr.CodeInvalidInput},
		{strings.Repeat("customers ", 60), nil, StageInput, qferr.CodeInvalidInput},
		{"customers from Atlantis", map[string][]hit{"customers": hCustomers}, StageBind, qferr.CodeUnsupportedQuery},
		{"customers who spent more than $500", map[string][]hit{"customers": hCustomers, "spent": hSpent, "more than": hMoreThan}, StagePlan, qferr.CodeValueResolutionFailed},
		{"zzz", nil, StageRank, qferr.CodeNoSemanticMatch},
	} {
		res, err := run(t, c.sentence, c.hits)
		if err == nil || res.Stage != c.stage || qferr.CodeOf(err) != c.code {
			t.Errorf("%q: stage %s err %v, want %s %s", c.sentence, res.Stage, err, c.stage, c.code)
		}
		if res.Query.SQL != "" {
			t.Errorf("%q: a refusal carried SQL", c.sentence)
		}
		if res.Stats.ErrorCode != c.code || res.Stats.Stage != c.stage {
			t.Errorf("%q: telemetry says %s at %s", c.sentence, res.Stats.ErrorCode, res.Stats.Stage)
		}
	}
}

// Decision 013's obligation: telemetry is built from structured, redacted
// forms. A request full of the user's words and the config's values must
// leave none of them in the marshalled telemetry.
func TestTelemetryCarriesNoUserTextOrValues(t *testing.T) {
	for _, c := range []struct {
		sentence string
		hits     map[string][]hit
	}{
		{"customers from India who bought laptops", map[string][]hit{"customers": hCustomers, "india": hIndia, "bought": hBought, "laptops": hLaptops}},
		{"customers from Zorblatt", map[string][]hit{"customers": hCustomers}},
	} {
		res, _ := run(t, c.sentence, c.hits)
		enc, err := json.Marshal(res.Stats)
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range []string{"India", "\"IN\"", "laptop", "Zorblatt", "bought", "from"} {
			if strings.Contains(string(enc), secret) {
				t.Errorf("telemetry holds %q: %s", secret, enc)
			}
		}
	}
	res, _ := run(t, "customers from India who bought laptops", map[string][]hit{"customers": hCustomers, "india": hIndia, "bought": hBought, "laptops": hLaptops})
	s := res.Stats
	if s.SelectedEntity != "customer" || s.JoinCount != 2 || s.ParamCount != 2 || s.Confidence <= 0 ||
		strings.Join(s.SelectedOperators, ",") != "eq" || s.PhraseCount == 0 || s.QueryLength != len(res.Query.SQL) {
		t.Errorf("telemetry = %+v", s)
	}
}

// Section 22 rule 9: one pipeline serves concurrent requests. Run under -race.
func TestConcurrentRequestsAreIndependentAndDeterministic(t *testing.T) {
	reg := registry(t, "commerce.json")
	hits := map[string][]hit{"customers": hCustomers, "india": hIndia, "spent": hSpent, "more than": hMoreThan}
	p, err := New(reg, newScripted(reg, hits), Options{})
	if err != nil {
		t.Fatal(err)
	}
	first, err := p.Run(context.Background(), "customers from India who spent more than 50000", clock)
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	errs := make(chan string, 64)
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			res, err := p.Run(context.Background(), "customers from India who spent more than 50000", clock)
			if err != nil || res.Query.Render(true) != first.Query.Render(true) {
				errs <- fmt.Sprintf("%v / %s", err, res.Query.SQL)
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Error(e)
	}
}

// The ranking policy comes from the config when it sets one, and an explicit
// override beats both.
func TestPolicyPrecedence(t *testing.T) {
	reg := registry(t, "commerce.json")
	if got := PolicyFor(reg); got.MinScore != ranking.DefaultPolicy().MinScore {
		t.Errorf("commerce sets no thresholds, got %v", got.MinScore)
	}
	cfg := *reg.Config()
	score := 0.8
	cfg.Ranking.MinScore = &score
	tuned := config.NewSchemaRegistry(&cfg)
	p, err := New(tuned, newScripted(tuned, nil), Options{})
	if err != nil || p.Policy().MinScore != 0.8 || p.Policy().MinMargin != ranking.DefaultPolicy().MinMargin {
		t.Errorf("config thresholds: %+v %v", p.Policy(), err)
	}
	override := ranking.DefaultPolicy()
	override.MinScore = 0.9
	p, _ = New(tuned, newScripted(tuned, nil), Options{Policy: &override})
	if p.Policy().MinScore != 0.9 {
		t.Errorf("override lost: %v", p.Policy().MinScore)
	}
	bad := ranking.DefaultPolicy()
	bad.MinScore = 0
	if _, err := New(tuned, newScripted(tuned, nil), Options{Policy: &bad}); err == nil {
		t.Error("an invalid override was accepted")
	}
}

// Rank (the diagnostic path) and Run agree on the reading: a diagnostic can
// never rank differently from a real request.
func TestRankAndRunShareOneReading(t *testing.T) {
	reg := registry(t, "commerce.json")
	hits := map[string][]hit{"customers": hCustomers, "india": hIndia}
	p, _ := New(reg, newScripted(reg, hits), Options{})
	_, ranked, err := p.Rank(context.Background(), "customers from India")
	if err != nil {
		t.Fatal(err)
	}
	res, err := p.Run(context.Background(), "customers from India", clock)
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprint(ranked.Cover) != fmt.Sprint(res.Ranking.Cover) || ranked.Verdict != res.Ranking.Verdict {
		t.Errorf("Rank cover %v, Run cover %v", ranked.Cover, res.Ranking.Cover)
	}
}

// Phase 7 finding V02, end to end: "customers living in India" once compiled
// to country = IN AND status = 'active' — a narrower answer than asked, with
// nothing to show it. With decision 019 the value "live" cannot be authorised
// by similarity, "living" is left unexplained, and the request is REFUSED.
// It must never again compile to a status filter.
func TestNoValueIsBoundOnSimilarityAlone(t *testing.T) {
	res, err := run(t, "customers living in India", map[string][]hit{
		"customers": hCustomers,
		"living":    {{"value:customer.status:alias:live", 0.790}, {"value:customer.status:enum:active", 0.435}, {"field:customer.country", 0.403}},
		"india":     hIndia,
	})
	if strings.Contains(res.Query.SQL, "status") {
		t.Fatalf("a value was bound on similarity alone:\n%s", res.Query.SQL)
	}
	if err == nil || qferr.CodeOf(err) != qferr.CodeUnsupportedQuery || res.Stage != StageBind {
		t.Errorf("stage %s, err %v; want the unexplained word refused at binding", res.Stage, err)
	}
	// The literal alias still works.
	res, err = run(t, "live customers from India", map[string][]hit{
		"live": {{"value:customer.status:alias:live", 1.0}}, "customers": hCustomers, "india": hIndia,
	})
	if err != nil || !strings.Contains(res.Query.SQL, `"t0"."status" = $1`) || res.Query.Params[0].Driver() != "active" {
		t.Errorf("literal alias: %v\n%s", err, res.Query.SQL)
	}
}

// Phase 7 gate decision 2: a top-N query must NAME its ranking metric. The
// metric is never inferred from a number field compared elsewhere in the
// sentence, and "spent ... last year" is not "top-N by spending last year"
// (that needs an explicit temporal aggregation pattern, which does not exist).
func TestTopNMetricIsNeverInferredFromAComparison(t *testing.T) {
	res, err := run(t, "top 10 customers who spent more than 50000", map[string][]hit{
		"top": hTop, "customers": hCustomers, "spent": hSpent, "more than": hMoreThan})
	if err == nil || qferr.CodeOf(err) != qferr.CodeUnsupportedQuery || strings.Contains(res.Query.SQL, "ORDER BY") {
		t.Fatalf("stage %s err %v\n%s", res.Stage, err, res.Query.SQL)
	}
}

// Phase 7 finding X03, end to end: refused, never total_spent by context.
func TestSharedWordIsNotSettledByContextAlone(t *testing.T) {
	res, err := run(t, "show customer value", map[string][]hit{
		"customer": {{"entity:customer", 1.0}},
		"value":    {{"field:customer.total_spent", 0.682}, {"field:order.amount", 0.588}},
	})
	if err == nil || qferr.CodeOf(err) != qferr.CodeAmbiguousMatch || res.Query.SQL != "" {
		t.Fatalf("stage %s err %v\n%s", res.Stage, err, res.Query.SQL)
	}
}

// Phase 7 finding X02, end to end (decision 019 §5 at the re-gate): "show
// customer date" is REFUSED at ranking, naming both date fields, with no SQL.
// "date" is part of created_at's and placed_at's configured phrases and
// "customer" does not distinguish them. It once compiled to SELECT created_at
// because entity context outweighed a 0.015 similarity difference. A sibling
// that spells out a configured phrase still compiles.
func TestSharedDateIsNotSettledByCustomer(t *testing.T) {
	hCustomer := []hit{{"entity:customer", 1.0}, {"field:customer.name", 0.827}}
	hDate := []hit{{"field:order.placed_at", 0.726}, {"field:customer.created_at", 0.711}}

	res, err := run(t, "show customer date", map[string][]hit{"customer": hCustomer, "date": hDate})
	if err == nil || qferr.CodeOf(err) != qferr.CodeAmbiguousMatch || res.Stage != StageRank || res.Query.SQL != "" {
		t.Fatalf("stage %s err %v\n%s", res.Stage, err, res.Query.SQL)
	}
	for _, w := range []string{"nothing in the sentence distinguishes them", "field:customer.created_at", "field:order.placed_at"} {
		if !strings.Contains(err.Error(), w) {
			t.Errorf("refusal lacks %q: %v", w, err)
		}
	}
	if strings.Contains(err.Error(), "margin") {
		t.Errorf("refusal reports a margin shortfall that did not happen: %v", err)
	}

	// "show customer creation date" is not scripted here: this file's
	// retrieval always matches an entry's FIRST surface, so it cannot say the
	// span matched the synonym "customer creation date". It is pinned with
	// that surface in ranking's TestSharedPhraseWithDistinguishingEvidenceResolves,
	// and end to end on the real model by the gate and the query set (X10).
	res, err = run(t, "orders by date", map[string][]hit{
		"orders": {{"entity:order", 0.881}, {"relationship:customer_orders", 0.805}},
		"date":   hDate,
	})
	if err != nil || !strings.Contains(res.Query.SQL, `SELECT "t0"."placed_at"`) || !strings.Contains(res.Query.SQL, `FROM "orders"`) {
		t.Errorf("orders by date: %v\n%s", err, res.Query.SQL)
	}
}
