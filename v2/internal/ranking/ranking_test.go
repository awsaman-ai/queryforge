package ranking

import (
	"errors"
	"fmt"
	"math/rand"
	"reflect"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/retrieval"
)

// ===========================================================================
// 1. High-score but ambiguous
// ===========================================================================

// A 1.000 similarity is not confidence. Two fields tied at the top are the
// clearest case: the absolute score is as high as it can be and the decision
// has no basis at all.
func TestHighScoreTieIsAmbiguous(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "amount", map[string][]hit{
		"amount": {
			{"field:order.amount", 1.000, "amount"},
			{"field:payment.amount", 1.000, "amount"},
			{"aggregation:sum", 0.676, "total"},
		},
	}), DefaultPolicy())

	s := spanNamed(t, res, "amount")
	if s.Decision != Ambiguous || s.Code != qferr.CodeAmbiguousMatch {
		t.Fatalf("decision = %s/%s, want ambiguous/AMBIGUOUS_MATCH", s.Decision, s.Code)
	}
	if s.Confidence != 0 {
		t.Errorf("confidence = %v; a tie must carry zero confidence however high the scores", s.Confidence)
	}
	top, _ := s.TopCandidate()
	if top.Similarity != 1.0 {
		t.Errorf("top similarity = %v; the test is only meaningful at 1.000", top.Similarity)
	}
	if len(s.Competitors) != 1 || s.Candidates[s.Competitors[0]].Entry.ID == top.Entry.ID {
		t.Errorf("competitors = %v, want exactly the other amount field", s.Competitors)
	}
	if res.Verdict != qferr.CodeAmbiguousMatch {
		t.Errorf("sentence verdict = %q, want AMBIGUOUS_MATCH", res.Verdict)
	}
}

// Not a tie, and both scores are well above the threshold — but 0.04 apart is
// not a decision.
func TestHighScoresWithNarrowMarginAreAmbiguous(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "status", map[string][]hit{
		"status": {
			{"field:customer.status", 0.97, "customer status"},
			{"field:order.status", 0.93, "order status"},
		},
	}), DefaultPolicy())

	s := spanNamed(t, res, "status")
	if s.Decision != Ambiguous {
		t.Fatalf("decision = %s (margin %.3f), want ambiguous", s.Decision, s.Margin)
	}
}

// Three-way: every rival within the margin is reported, not just the second.
// The caller cannot rephrase well if it is told about half the readings.
func TestAllCompetitorsWithinMarginAreReported(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "status", map[string][]hit{
		"status": {
			{"field:customer.status", 1.0, "status"},
			{"field:order.status", 1.0, "status"},
			{"field:payment.status", 1.0, "status"},
			{"entity:order", 0.4, ""},
		},
	}), DefaultPolicy())
	s := spanNamed(t, res, "status")
	if len(s.Competitors) != 2 {
		t.Fatalf("competitors = %d, want the two other status fields", len(s.Competitors))
	}
}

// ===========================================================================
// 2. Lower-score but clearly separated
// ===========================================================================

// 0.75 is far from 1.000, but nothing else is close. Confidence comes from the
// margin, so this is a confident accept.
func TestModestScoreWithWideMarginIsAccepted(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "what things cost", map[string][]hit{
		"cost": {
			{"field:product.price", 0.75, "unit price"},
			{"field:order.amount", 0.40, "order total"},
		},
	}), DefaultPolicy())

	s := spanNamed(t, res, "cost")
	if s.Decision != Accept || topID(s) != "field:product.price" {
		t.Fatalf("decision = %s %s, want accept product.price", s.Decision, topID(s))
	}
	if s.Confidence != 1 {
		t.Errorf("confidence = %v, want 1: the margin is 3.5x the minimum", s.Confidence)
	}
}

// The pair that states the design in one test: the LOWER top score gets the
// HIGHER confidence.
func TestConfidenceIsNotSimilarity(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	p := DefaultPolicy()

	high := spanNamed(t, mustRank(t, script(t, reg, "name", map[string][]hit{
		"name": {{"field:customer.name", 1.0, "name"}, {"field:product.name", 0.95, "name"}},
	}), p), "name")
	low := spanNamed(t, mustRank(t, script(t, reg, "cost", map[string][]hit{
		"cost": {{"field:product.price", 0.78, "cost"}, {"field:order.amount", 0.45, "amount"}},
	}), p), "cost")

	hs, _ := high.TopCandidate()
	ls, _ := low.TopCandidate()
	if !(hs.Similarity > ls.Similarity) {
		t.Fatalf("setup: want the first case to have the higher similarity")
	}
	if !(low.Confidence > high.Confidence) {
		t.Errorf("confidence: high-similarity %.2f vs low-similarity %.2f; the separated case must be the confident one",
			high.Confidence, low.Confidence)
	}
}

// ===========================================================================
// 3. Exact lexical matches that are still ambiguous
// ===========================================================================

// Both surfaces are exactly the span. Lexical evidence is real, but it is
// identical for both readings, so it cannot move the margin.
func TestExactLexicalMatchOnBothSidesStaysAmbiguous(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "name", map[string][]hit{
		"name": {
			{"field:customer.name", 1.0, "name"},
			{"field:product.name", 1.0, "name"},
		},
	}), DefaultPolicy())
	s := spanNamed(t, res, "name")
	for _, c := range s.Candidates {
		if c.Evidence.Lexical != 1 {
			t.Errorf("%s lexical = %v, want 1: the test needs both to be exact", c.Entry.ID, c.Evidence.Lexical)
		}
	}
	if s.Decision != Ambiguous {
		t.Errorf("decision = %s, want ambiguous", s.Decision)
	}
}

// "highest" is a configured phrase of BOTH aggregation:max and pattern:top_n.
// They are different readings (one scalar vs N rows), so they must not be
// treated as agreeing — unlike "how many", below.
func TestHighestIsAmbiguousBetweenMaxAndTopN(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "highest", map[string][]hit{
		"highest": {
			{"aggregation:max", 1.0, "highest"},
			{"pattern:top_n", 1.0, "highest"},
			{"aggregation:min", 0.656, "lowest"},
		},
	}), DefaultPolicy())
	if s := spanNamed(t, res, "highest"); s.Decision != Ambiguous {
		t.Errorf("decision = %s, want ambiguous", s.Decision)
	}
}

// ...and a count beside it is the structural evidence that decides it.
func TestCountBesideHighestSelectsTopN(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	for _, sentence := range []string{"the 5 highest orders", "highest 5 orders"} {
		t.Run(sentence, func(t *testing.T) {
			res := mustRank(t, script(t, reg, sentence, map[string][]hit{
				"highest": {
					{"aggregation:max", 1.0, "highest"},
					{"pattern:top_n", 1.0, "highest"},
				},
			}), DefaultPolicy())
			s := spanNamed(t, res, "highest")
			if s.Decision != Accept || topID(s) != "pattern:top_n" {
				t.Fatalf("decision = %s %s, want accept pattern:top_n", s.Decision, topID(s))
			}
		})
	}
}

// A currency amount is not a count: "$5 highest" does not ask for five rows.
func TestCurrencyIsNotACount(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "$5 highest", map[string][]hit{
		"highest": {{"aggregation:max", 1.0, "highest"}, {"pattern:top_n", 1.0, "highest"}},
	}), DefaultPolicy())
	if s := spanNamed(t, res, "highest"); s.Decision != Ambiguous {
		t.Errorf("decision = %s, want ambiguous: a currency value is not a row count", s.Decision)
	}
}

// "how many" configures both the count pattern and the count aggregation. They
// compile to the same COUNT, so they agree and the exact tie is NOT ambiguity.
func TestCountPatternAndCountAggregationAgree(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "how many", map[string][]hit{
		"how many": {
			{"aggregation:count", 1.0, "how many"},
			{"pattern:how_many", 1.0, "how many"},
			{"aggregation:sum", 0.5, "total"},
		},
	}), DefaultPolicy())
	s := spanNamed(t, res, "how many")
	if s.Decision != Accept {
		t.Fatalf("decision = %s, want accept: the two readings agree", s.Decision)
	}
	if got := s.Candidates[s.RunnerUp].Entry.ID; got != "aggregation:sum" {
		t.Errorf("runner-up = %s, want aggregation:sum (the agreeing reading is not a rival)", got)
	}
}

// A value and the field it belongs to are one reading stated twice.
func TestValueAndItsFieldAgree(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "freedonia", map[string][]hit{
		"freedonia": {
			{"value:customer.country:alias:Freedonia", 1.0, "Freedonia"},
			{"field:customer.country", 0.97, "customer country"},
			{"value:customer.country:alias:Sylvania", 0.55, "Sylvania"},
		},
	}), DefaultPolicy())
	s := spanNamed(t, res, "freedonia")
	if s.Decision != Accept {
		t.Fatalf("decision = %s, want accept", s.Decision)
	}
}

// Two values of the same field that resolve to DIFFERENT stored values are
// rivals, not an agreement.
func TestDifferentValuesOfOneFieldCompete(t *testing.T) {
	// Decision 019 changed this test's contract. Before it, a value the
	// sentence does not contain (Sylvania, retrieved for "freedonia") could
	// compete on similarity and make the span ambiguous. Now it is vetoed: a
	// value must be literally present to be a reading at all.
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "freedonia", map[string][]hit{
		"freedonia": {
			{"value:customer.country:alias:Freedonia", 0.9, "Freedonia"},
			{"value:customer.country:alias:Sylvania", 0.9, "Sylvania"},
		},
	}), DefaultPolicy())
	s := spanNamed(t, res, "freedonia")
	if s.Decision != Accept {
		t.Fatalf("decision = %s, want accept: the absent value may not compete", s.Decision)
	}
	for _, c := range s.Candidates {
		if c.Entry.ID == "value:customer.country:alias:Sylvania" && !c.Vetoed {
			t.Error("a value the sentence does not contain was not vetoed")
		}
	}

	// Values that ARE both literally present still compete: one word
	// configured on two fields is a genuine ambiguity, and stays refused.
	shared := withSharedValue(t)
	res = mustRank(t, script(t, shared, "freedonia", map[string][]hit{
		"freedonia": {
			{"value:customer.country:alias:Freedonia", 0.9, "Freedonia"},
			{"value:payment.status:alias:Freedonia", 0.9, "Freedonia"},
		},
	}), DefaultPolicy())
	if s := spanNamed(t, res, "freedonia"); s.Decision != Ambiguous {
		t.Errorf("two literal readings: decision = %s, want ambiguous", s.Decision)
	}
}

// ===========================================================================
// 4. Deterministic tie handling
// ===========================================================================

// The same input, 50 times, with retrieval's order of the tied pair reversed
// on alternate runs: the decision, the candidate order and every score must
// be identical. Phase 2 broke ties by id and that decided the Top-1; here the
// id may only decide which of two tied readings is LISTED first.
func TestTieHandlingIsIndependentOfInputOrder(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	base := script(t, reg, "amount", map[string][]hit{
		"amount": {
			{"field:order.amount", 1.0, "amount"},
			{"field:payment.amount", 1.0, "amount"},
			{"aggregation:sum", 0.676, "total"},
		},
	})
	want := mustRank(t, base, DefaultPolicy())

	rng := rand.New(rand.NewSource(1))
	for i := 0; i < 50; i++ {
		in := base
		in.Retrieved = make([][]retrieval.Match, len(base.Retrieved))
		for k, ms := range base.Retrieved {
			cp := append([]retrieval.Match(nil), ms...)
			rng.Shuffle(len(cp), func(a, b int) { cp[a], cp[b] = cp[b], cp[a] })
			in.Retrieved[k] = cp
		}
		got := mustRank(t, in, DefaultPolicy())
		if !reflect.DeepEqual(summary(got), summary(want)) {
			t.Fatalf("run %d differs:\n got %v\nwant %v", i, summary(got), summary(want))
		}
	}
	s := spanNamed(t, want, "amount")
	if topID(s) != "field:order.amount" {
		t.Errorf("listing order of a tie should be by id; got %s first", topID(s))
	}
}

// An exact tie is ambiguous under EVERY policy, including MinMargin 0 — the
// "threshold only" configuration the calibration sweep measures.
func TestTieIsAmbiguousEvenWithZeroMargin(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	p := DefaultPolicy()
	p.MinMargin = 0
	res := mustRank(t, script(t, reg, "name", map[string][]hit{
		"name": {{"field:customer.name", 1.0, "name"}, {"field:product.name", 1.0, "name"}},
	}), p)
	if s := spanNamed(t, res, "name"); s.Decision != Ambiguous {
		t.Errorf("decision = %s with MinMargin 0, want ambiguous: ties are never broken by sort order", s.Decision)
	}
}

// REGRESSION (Phase 4, found on the first real run): span quality compared a
// longer span against the shorter span's retrieval top-1, which retrieval had
// chosen from a 1.000 tie BY ID. "what is the highest" therefore penalised
// aggregation:max (carried by "highest") but not pattern:top_n (tied with it),
// and top_n was accepted by a 0.200 margin that existed only because
// "aggregation" sorts before "pattern".
func TestShorterSpanTieDoesNotFavourOneReading(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "what is the highest", map[string][]hit{
		"what is the highest": {
			{"pattern:top_n", 0.878, "highest"},
			{"aggregation:max", 0.878, "highest"},
		},
		"highest": {
			{"aggregation:max", 1.0, "highest"},
			{"pattern:top_n", 1.0, "highest"},
		},
	}), DefaultPolicy())

	long := spanNamed(t, res, "what is the highest")
	for _, c := range long.Candidates {
		if c.Evidence.Span != -1 {
			t.Errorf("%s span evidence = %v; both tied readings are carried by \"highest\"", c.Entry.ID, c.Evidence.Span)
		}
	}
	if long.Decision == Accept {
		t.Errorf("the long span was accepted as %s: a tie decided by id order", topID(long))
	}
	if res.Verdict != qferr.CodeAmbiguousMatch {
		t.Errorf("verdict = %q, want AMBIGUOUS_MATCH", res.Verdict)
	}
}

// ===========================================================================
// 5. Threshold and margin interactions
// ===========================================================================

func TestThresholdAndMarginTable(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	cases := []struct {
		name          string
		top, second   float64
		minScore, min float64
		want          Decision
		code          qferr.Code
	}{
		// The float trap: 0.9 - 0.8 is 0.0999... in binary. Exactly at the
		// boundary must accept, or the decision flips on the 14th decimal.
		{"margin exactly at boundary", 0.90, 0.80, 0.70, 0.10, Accept, ""},
		{"margin just under", 0.90, 0.81, 0.70, 0.10, Ambiguous, qferr.CodeAmbiguousMatch},
		{"score exactly at threshold", 0.70, 0.30, 0.70, 0.10, Accept, ""},
		{"score just under threshold, wide margin", 0.69, 0.10, 0.70, 0.10, Reject, qferr.CodeLowConfidence},
		// Score is tested BEFORE margin: two weak readings close together are
		// "nothing good enough", not a choice between two bad readings.
		{"weak and close", 0.65, 0.64, 0.70, 0.10, Reject, qferr.CodeLowConfidence},
		{"strong and close", 0.95, 0.94, 0.70, 0.10, Ambiguous, qferr.CodeAmbiguousMatch},
		{"strong and far", 0.95, 0.50, 0.70, 0.10, Accept, ""},
		{"stricter margin refuses", 0.95, 0.80, 0.70, 0.20, Ambiguous, qferr.CodeAmbiguousMatch},
		{"looser margin accepts", 0.95, 0.80, 0.70, 0.10, Accept, ""},
		{"stricter threshold refuses", 0.75, 0.30, 0.80, 0.10, Reject, qferr.CodeLowConfidence},
		{"zero margin policy, distinct scores", 0.95, 0.94, 0.70, 0, Accept, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			// Two readings with no lexical overlap with the span, so no
			// evidence moves the scores and the arithmetic is exact.
			res := mustRank(t, script(t, reg, "zzq", map[string][]hit{
				"zzq": {
					{"field:product.price", c.top, "unit price"},
					{"field:order.amount", c.second, "order total"},
				},
			}), Policy{MinScore: c.minScore, MinMargin: c.min, Weights: DefaultPolicy().Weights})
			s := spanNamed(t, res, "zzq")
			if s.Decision != c.want || s.Code != c.code {
				t.Errorf("decision = %s/%q (score %.3f, margin %.17f), want %s/%q",
					s.Decision, s.Code, c.top, s.Margin, c.want, c.code)
			}
		})
	}
}

func TestConfidenceFormula(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	cases := []struct {
		top, second, want float64
	}{
		{0.90, 0.80, 0.5},  // at the boundary: half-way
		{0.90, 0.75, 0.75}, // 1.5x the minimum margin
		{0.90, 0.60, 1.0},  // capped
		{0.90, 0.85, 0.0},  // not accepted → zero
	}
	for _, c := range cases {
		res := mustRank(t, script(t, reg, "zzq", map[string][]hit{
			"zzq": {{"field:product.price", c.top, "unit price"}, {"field:order.amount", c.second, "order total"}},
		}), DefaultPolicy())
		got := spanNamed(t, res, "zzq").Confidence
		if d := got - c.want; d > 1e-9 || d < -1e-9 {
			t.Errorf("%.2f vs %.2f: confidence %.6f, want %.2f", c.top, c.second, got, c.want)
		}
	}
}

func TestNoCandidatesIsNoSemanticMatch(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "zzq", nil), DefaultPolicy())
	s := spanNamed(t, res, "zzq")
	if s.Decision != Reject || s.Code != qferr.CodeNoSemanticMatch {
		t.Errorf("decision = %s/%s, want reject/NO_SEMANTIC_MATCH", s.Decision, s.Code)
	}
	if res.Verdict != qferr.CodeNoSemanticMatch {
		t.Errorf("verdict = %q, want NO_SEMANTIC_MATCH", res.Verdict)
	}
	if res.Err() == nil {
		t.Error("an unmatched sentence must produce an error, not a silent empty reading")
	}
}

// A lone candidate beats nothing: margin is its own score.
func TestSingleCandidateMargin(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "zzq", map[string][]hit{
		"zzq": {{"field:product.price", 0.8, "unit price"}},
	}), DefaultPolicy())
	s := spanNamed(t, res, "zzq")
	if s.Decision != Accept || s.RunnerUp != -1 || s.Margin != 0.8 {
		t.Errorf("got %s runner-up %d margin %v", s.Decision, s.RunnerUp, s.Margin)
	}
}

func TestPolicyValidation(t *testing.T) {
	good := DefaultPolicy()
	if err := good.Validate(); err != nil {
		t.Fatalf("default policy rejected: %v", err)
	}
	for name, mutate := range map[string]func(*Policy){
		"zero minScore":     func(p *Policy) { p.MinScore = 0 },
		"negative minScore": func(p *Policy) { p.MinScore = -1 },
		"minScore above 2":  func(p *Policy) { p.MinScore = 2.5 },
		"negative margin":   func(p *Policy) { p.MinMargin = -0.1 },
		"margin above 1":    func(p *Policy) { p.MinMargin = 1.5 },
		"negative weight":   func(p *Policy) { p.Weights.Entity = -0.1 },
		"weight above 1":    func(p *Policy) { p.Weights.Span = 2 },
	} {
		p := DefaultPolicy()
		mutate(&p)
		if err := p.Validate(); qferr.CodeOf(err) != qferr.CodeInvalidInput {
			t.Errorf("%s: got %v, want INVALID_INPUT", name, err)
		}
		if _, err := Rank(Input{Registry: loadRegistry(t, "ambiguity.json")}, p); err == nil {
			t.Errorf("%s: Rank accepted an invalid policy", name)
		}
	}
}

func TestMismatchedRetrievalIsAnInternalError(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	in := script(t, reg, "amount", nil)
	in.Retrieved = in.Retrieved[:0]
	// err == nil first: qferr.CodeOf(nil) is CodeInternal, so the code check
	// alone would pass if Rank returned no error at all (Phase 6 gate, 017).
	if _, err := Rank(in, DefaultPolicy()); err == nil || qferr.CodeOf(err) != qferr.CodeInternal {
		t.Errorf("got %v, want INTERNAL_ERROR", err)
	}
}

// ===========================================================================
// Structural evidence
// ===========================================================================

// The brief's section 12 example, decided by the entity the sentence names.
func TestEntityContextResolvesAmount(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	for _, c := range []struct{ sentence, entityWord, entity, want string }{
		{"orders with amount over 100", "orders", "entity:order", "field:order.amount"},
		{"payments with amount over 100", "payments", "entity:payment", "field:payment.amount"},
	} {
		t.Run(c.sentence, func(t *testing.T) {
			res := mustRank(t, script(t, reg, c.sentence, map[string][]hit{
				c.entityWord: {{c.entity, 0.93, ""}, {"relationship:customer_orders", 0.70, "ordered"}},
				"amount":     {{"field:order.amount", 1.0, "amount"}, {"field:payment.amount", 1.0, "amount"}},
				"over":       {{"operator:gt", 1.0, "over"}, {"operator:lt", 0.5, "under"}},
			}), DefaultPolicy())
			s := covered(res)["amount"]
			if s.Decision != Accept || topID(s) != c.want {
				t.Fatalf("amount → %s %s, want accept %s", s.Decision, topID(s), c.want)
			}
			top, _ := s.TopCandidate()
			if top.Evidence.Entity != 1 || top.Evidence.Type != 1 || top.Evidence.Operator != 1 {
				t.Errorf("evidence = %+v; want entity, type and operator support", top.Evidence)
			}
			if res.Verdict != "" {
				t.Errorf("verdict = %q, want OK", res.Verdict)
			}
		})
	}
}

// Context that names BOTH owning entities cannot decide.
func TestContextNamingBothEntitiesStaysAmbiguous(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "orders and payments with amount over 5", map[string][]hit{
		"orders":   {{"entity:order", 0.93, ""}},
		"payments": {{"entity:payment", 0.93, ""}},
		"amount":   {{"field:order.amount", 1.0, "amount"}, {"field:payment.amount", 1.0, "amount"}},
	}), DefaultPolicy())
	if s := covered(res)["amount"]; s.Decision != Ambiguous {
		t.Errorf("amount → %s %s, want ambiguous", s.Decision, topID(s))
	}
}

// Operator compatibility: a string field cannot take "more than".
func TestOperatorIncompatibleWithFieldType(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "name more than 5", map[string][]hit{
		"name":      {{"field:customer.name", 0.9, "customer name"}, {"field:product.price", 0.85, "unit price"}},
		"more than": {{"operator:gt", 1.0, "more than"}},
	}), DefaultPolicy())
	s := spanNamed(t, res, "name")
	for _, c := range s.Candidates {
		switch c.Entry.ID {
		case "field:customer.name":
			if c.Evidence.Operator != -1 || c.Evidence.Type != -1 {
				t.Errorf("customer.name evidence = %+v; want operator and type against", c.Evidence)
			}
		case "field:product.price":
			if c.Evidence.Operator != 1 || c.Evidence.Type != 1 {
				t.Errorf("product.price evidence = %+v; want operator and type support", c.Evidence)
			}
		}
	}
	if topID(s) != "field:product.price" {
		t.Errorf("top = %s; the schema says a string cannot be compared with a number", topID(s))
	}
}

// A connective stops attachment: in "amount and name over 5", the 5 belongs
// to name's clause, not to amount.
func TestConnectiveStopsAttachment(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "price and name over 5", map[string][]hit{
		"price": {{"field:product.price", 1.0, "price"}, {"field:order.amount", 0.5, "amount"}},
	}), DefaultPolicy())
	top, _ := spanNamed(t, res, "price").TopCandidate()
	if top.Evidence.Type != 0 || top.Evidence.Operator != 0 {
		t.Errorf("price picked up evidence across \"and\": %+v", top.Evidence)
	}
}

// REGRESSION (Phase 4): a join key — neither selectable nor filterable —
// competed with entity:customer for the word "customers" because its
// description says "The customer who placed the order".
func TestJoinKeyIsVetoed(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "customers", map[string][]hit{
		"customers": {
			{"entity:customer", 0.884, ""},
			{"field:order.customer_id", 0.95, "The customer who placed the order."},
		},
	}), DefaultPolicy())
	s := spanNamed(t, res, "customers")
	if topID(s) != "entity:customer" || s.Decision != Accept {
		t.Fatalf("customers → %s %s, want accept entity:customer", s.Decision, topID(s))
	}
	for _, c := range s.Candidates {
		if c.Entry.ID == "field:order.customer_id" && !c.Vetoed {
			t.Error("the join key was not vetoed")
		}
	}
}

// REGRESSION (Phase 2 miss 2): relationships lost to entities on raw
// similarity. "placed an order" retrieved entity:order 0.783 first; the verb
// maps to the edge. Scores are MiniLM's.
func TestRelationshipVerbBeatsEntityOnTheWholePhrase(t *testing.T) {
	reg := loadRegistry(t, "commerce.json")
	res := mustRank(t, script(t, reg, "placed an order", map[string][]hit{
		"placed an order": {
			{"entity:order", 0.783, ""},
			{"field:order.placed_at", 0.781, "Date the order was placed."},
			{"relationship:customer_orders", 0.731, "ordered"},
		},
		"placed": {
			{"relationship:customer_orders", 1.000, "placed"},
			{"field:order.placed_at", 0.781, "placed at"},
		},
		"order": {
			{"entity:order", 1.000, "order"},
			{"relationship:customer_orders", 0.805, "ordered"},
		},
	}), DefaultPolicy())
	cov := covered(res)
	if s, ok := cov["placed"]; !ok || s.Decision != Accept || topID(s) != "relationship:customer_orders" {
		t.Fatalf("placed → %+v, want accept relationship:customer_orders", s.Decision)
	}
	if s, ok := cov["order"]; !ok || topID(s) != "entity:order" {
		t.Errorf("order → %s, want entity:order", topID(s))
	}
	if _, ok := cov["placed an order"]; ok {
		t.Error("the whole phrase is in the cover; its target is carried by shorter spans")
	}
}

// REGRESSION (Phase 4): with a relationship boosted by naming just ONE end,
// "order" in "customers who placed an order" became ambiguous between the
// entity and the customer_orders edge.
func TestOneEndIsNotRelationshipSupport(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "customers who placed an order", map[string][]hit{
		"customers": {{"entity:customer", 0.884, ""}, {"field:customer.name", 0.6, "customer name"}},
		"placed":    {{"relationship:customer_orders", 1.0, "placed"}, {"field:order.created_at", 0.6, "ordered on"}},
		"order":     {{"entity:order", 1.0, "order"}, {"relationship:customer_orders", 0.915, "ordered"}},
	}), DefaultPolicy())
	s := covered(res)["order"]
	if s.Decision != Accept || topID(s) != "entity:order" {
		t.Errorf("order → %s %s, want accept entity:order", s.Decision, topID(s))
	}
	if p := covered(res)["placed"]; topID(p) != "relationship:customer_orders" {
		t.Errorf("placed → %s, want the relationship", topID(p))
	}
}

// REGRESSION (Phase 4): context from a field made its own entity's fields
// outrank the entity itself on the word that names it.
func TestContextIsSymmetricBetweenAnEntityAndItsFields(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "payments with payment amount", map[string][]hit{
		"payments":       {{"entity:payment", 0.932, ""}, {"field:payment.amount", 0.812, "payment amount"}},
		"payment amount": {{"field:payment.amount", 1.0, "payment amount"}},
	}), DefaultPolicy())
	if s := spanNamed(t, res, "payments"); topID(s) != "entity:payment" || s.Decision != Accept {
		t.Errorf("payments → %s %s, want accept entity:payment", s.Decision, topID(s))
	}
}

// REGRESSION (Phase 4, calibration sweep): a stricter margin produced a WRONG
// answer. "orders" was ambiguous and contributed no context, "payments" was
// accepted, so "amount" saw one entity and guessed. Ambiguous spans now
// contribute every entity among their live readings.
func TestAmbiguousSpansStillContributeContext(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	p := DefaultPolicy()
	p.MinMargin = 0.15
	res := mustRank(t, script(t, reg, "orders and payments with amount over 5", map[string][]hit{
		"orders":   {{"entity:order", 0.881, ""}, {"relationship:customer_orders", 0.805, "ordered"}},
		"payments": {{"entity:payment", 0.932, ""}, {"field:payment.status", 0.60, "payment status"}},
		"amount":   {{"field:order.amount", 1.0, "amount"}, {"field:payment.amount", 1.0, "amount"}},
	}), p)
	// "orders" is ambiguous in PASS 1 here (0.931 vs 0.805 against a 0.15
	// margin) — the condition that produced the bug — even though pass 2 then
	// resolves it. Pass 1 is internal, so the assertion is on the outcome.
	if s := covered(res)["amount"]; s.Decision == Accept {
		t.Errorf("amount accepted as %s: one-sided context from an ambiguous mention", topID(s))
	}
}

// Monotonicity, stated directly: raising MinMargin may turn accepts into
// refusals, never into different accepts.
func TestStricterMarginNeverChangesAnAcceptedTarget(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	in := script(t, reg, "orders and payments with amount over 5", map[string][]hit{
		"orders":   {{"entity:order", 0.881, ""}, {"relationship:customer_orders", 0.805, "ordered"}},
		"payments": {{"entity:payment", 0.932, ""}, {"field:payment.status", 0.60, "payment status"}},
		"amount":   {{"field:order.amount", 1.0, "amount"}, {"field:payment.amount", 1.0, "amount"}},
		"over":     {{"operator:gt", 1.0, "over"}},
	})
	loose := DefaultPolicy()
	loose.MinMargin = 0.01
	base := covered(mustRank(t, in, loose))
	for _, m := range []float64{0.05, 0.10, 0.15, 0.20, 0.30} {
		p := DefaultPolicy()
		p.MinMargin = m
		for norm, s := range covered(mustRank(t, in, p)) {
			if s.Decision != Accept {
				continue
			}
			if b, ok := base[norm]; ok && b.Decision == Accept && topID(b) != topID(s) {
				t.Errorf("minMargin %.2f: %q accepted as %s, but %s under a looser margin",
					m, norm, topID(s), topID(b))
			}
			if b, ok := base[norm]; ok && b.Decision != Accept {
				t.Errorf("minMargin %.2f: %q accepted, but refused under a LOOSER margin", m, norm)
			}
		}
	}
}

// ===========================================================================
// Span quality and cover
// ===========================================================================

// REGRESSION (Phase 4, first real run): "orders with" was accepted as
// payment.order_id at 0.649 and swallowed "orders".
func TestFunctionWordBoundaryIsPenalised(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "orders with amount", map[string][]hit{
		"orders with": {{"field:order.amount", 0.95, "order total"}, {"entity:order", 0.5, ""}},
		"orders":      {{"entity:order", 0.881, ""}, {"field:order.status", 0.5, "order status"}},
	}), DefaultPolicy())
	s := spanNamed(t, res, "orders with")
	top, _ := s.TopCandidate()
	if top.Evidence.Span != -1 {
		t.Errorf("\"orders with\" span evidence = %v, want -1", top.Evidence.Span)
	}
	if _, ok := covered(res)["orders with"]; ok {
		t.Error("the function-word span is in the cover")
	}
	if o := covered(res)["orders"]; topID(o) != "entity:order" {
		t.Errorf("orders → %s, want entity:order", topID(o))
	}
}

// The boundary rule on its own. Above, "orders" is also accepted, so the
// decomposition rule penalises "orders with" too and would hide a broken
// boundary rule (a mutation test showed exactly that). Here nothing shorter
// is scripted: only the boundary rule can fire.
func TestFunctionWordBoundaryAlone(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "orders with", map[string][]hit{
		"orders with": {{"field:order.amount", 0.95, "order total"}, {"entity:order", 0.5, ""}},
	}), DefaultPolicy())
	top, _ := spanNamed(t, res, "orders with").TopCandidate()
	if top.Evidence.Span != -1 {
		t.Fatalf("span evidence = %v, want -1 from the boundary rule alone", top.Evidence.Span)
	}
	found := false
	for _, r := range top.Reasons {
		found = found || strings.Contains(r, "function word")
	}
	if !found {
		t.Errorf("reasons %v do not name the function-word boundary", top.Reasons)
	}
}

// Configured phrases that begin with a function word are exempt, because the
// config author declared them.
func TestExactConfiguredPhraseIsExemptFromSpanRules(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "orders in the last", map[string][]hit{
		"in the last": {{"pattern:recent", 1.0, "in the last"}},
	}), DefaultPolicy())
	top, _ := spanNamed(t, res, "in the last").TopCandidate()
	if top.Evidence.Span != 0 {
		t.Errorf("\"in the last\" span evidence = %v; an exact configured phrase is exempt", top.Evidence.Span)
	}
}

// PHASE 3 → 4: "from" was missing from the function-word list in Phase 3.
// Its effect reaches ranking here: "from freedonia" is a boundary span, and the
// clean "freedonia" is the one read.
func TestFromIsAFunctionWordBoundary(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "customers from Freedonia", map[string][]hit{
		"from freedonia": {{"value:customer.country:alias:Freedonia", 0.919, "Freedonia"}},
		"freedonia":      {{"value:customer.country:alias:Freedonia", 1.0, "Freedonia"}},
	}), DefaultPolicy())
	if _, ok := covered(res)["from freedonia"]; ok {
		t.Error("\"from freedonia\" is in the cover; \"from\" must be a function word")
	}
	if _, ok := covered(res)["freedonia"]; !ok {
		t.Error("\"freedonia\" is not in the cover")
	}
}

// A text span that contains a number competes with the typed span for the
// value; it is the weaker reading.
func TestSpanSwallowingANumberIsPenalised(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "amount over 100", map[string][]hit{
		"amount over 100": {{"field:order.amount", 0.9, "order total"}},
	}), DefaultPolicy())
	top, _ := spanNamed(t, res, "amount over 100").TopCandidate()
	if top.Evidence.Span != -1 {
		t.Errorf("span evidence = %v, want -1", top.Evidence.Span)
	}
}

// REGRESSION (Phase 4): accepted spans came before ambiguous ones in the
// cover, so a DEFECTIVE accepted span resolved an ambiguity the sentence
// really has.
func TestDefectiveAcceptNeverOverrulesCleanAmbiguity(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "orders and payments with amount", map[string][]hit{
		"payments with amount": {{"field:payment.amount", 0.899, "payment amount"}, {"entity:order", 0.3, ""}},
		"amount":               {{"field:order.amount", 1.0, "amount"}, {"field:payment.amount", 1.0, "amount"}},
	}), DefaultPolicy())
	cov := covered(res)
	if _, ok := cov["payments with amount"]; ok {
		t.Error("the defective span won the cover")
	}
	if s := cov["amount"]; s.Decision != Ambiguous {
		t.Errorf("amount is %s, want ambiguous in the cover", s.Decision)
	}
}

// An exact configured multi-word synonym covers its parts: "order amount"
// accepted means the "amount" inside it is not ambiguous.
func TestAcceptedLongerSpanCoversAmbiguousPart(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "order amount", map[string][]hit{
		"order amount": {{"field:order.amount", 1.0, "order amount"}, {"field:payment.amount", 0.7, "payment amount"}},
		"amount":       {{"field:order.amount", 1.0, "amount"}, {"field:payment.amount", 1.0, "amount"}},
		"order":        {{"entity:order", 1.0, "order"}},
	}), DefaultPolicy())
	cov := covered(res)
	if s := cov["order amount"]; s.Decision != Accept {
		t.Fatalf("order amount → %s, want accept", s.Decision)
	}
	if res.Verdict != "" {
		t.Errorf("verdict = %q; the ambiguous part is covered by an accepted reading", res.Verdict)
	}
}

// REGRESSION (Phase 4): the partial lexical rule ran the wrong way. "value"
// inside "purchase value" earned support although the qualifier "purchase"
// was never said, and pushed commerce's "value" (raw margin 0.094) over the
// line to total_spent.
func TestLexicalPartialMatchDirection(t *testing.T) {
	cases := []struct {
		span, surface string
		want          float64
	}{
		{"value", "purchase value", 0},             // surface more specific: unconfirmed
		{"total spent amount", "total spent", 0.5}, // surface wholly present
		{"orders", "order", 1},                     // plural fold
		{"Total_Spent", "total spent", 1},          // identifier fold
		{"money spent", "total spent", 0},          // a paraphrase is never penalised, never supported
		{"", "x", 0},
	}
	for _, c := range cases {
		if got := lexicalScore(strings.ToLower(c.span), c.surface); got != c.want {
			t.Errorf("lexical(%q, %q) = %v, want %v", c.span, c.surface, got, c.want)
		}
	}

	reg := loadRegistry(t, "commerce.json")
	res := mustRank(t, script(t, reg, "value", map[string][]hit{
		"value": {
			{"field:customer.total_spent", 0.782, "purchase value"},
			{"field:order.amount", 0.688, "amount"},
		},
	}), DefaultPolicy())
	if s := spanNamed(t, res, "value"); s.Decision != Ambiguous {
		t.Errorf("value → %s (margin %.3f), want ambiguous: raw margin 0.094 with no real support", s.Decision, s.Margin)
	}
}

// Unexplained content words are reported, stop words and values are not.
func TestUnexplainedTokens(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	res := mustRank(t, script(t, reg, "zorp the orders over 5", map[string][]hit{
		"orders": {{"entity:order", 0.93, ""}},
		"over":   {{"operator:gt", 1.0, "over"}},
	}), DefaultPolicy())
	if !reflect.DeepEqual(res.Unexplained, []int{0}) {
		t.Errorf("unexplained = %v, want only token 0 (\"zorp\")", res.Unexplained)
	}
}

// ===========================================================================
// Concurrency: Rank holds no shared mutable state (section 22, rule 9).
// ===========================================================================

func TestRankIsSafeConcurrently(t *testing.T) {
	reg := loadRegistry(t, "ambiguity.json")
	in := script(t, reg, "orders with amount over 100", map[string][]hit{
		"orders": {{"entity:order", 0.93, ""}},
		"amount": {{"field:order.amount", 1.0, "amount"}, {"field:payment.amount", 1.0, "amount"}},
		"over":   {{"operator:gt", 1.0, "over"}},
	})
	want := summary(mustRank(t, in, DefaultPolicy()))
	done := make(chan []string, 32)
	for i := 0; i < 32; i++ {
		go func() {
			res, _ := Rank(in, DefaultPolicy())
			done <- summary(res)
		}()
	}
	for i := 0; i < 32; i++ {
		if got := <-done; !reflect.DeepEqual(got, want) {
			t.Fatalf("concurrent run differs: %v vs %v", got, want)
		}
	}
}

// summary is a comparable rendering of a result: every span's decision and
// ordered candidates with scores.
func summary(r Result) []string {
	var out []string
	for _, s := range r.Spans {
		line := fmt.Sprintf("%s %s %.12f %.12f", s.Span.Norm, s.Decision, s.Margin, s.Confidence)
		for _, c := range s.Candidates {
			line += fmt.Sprintf(" | %s %.12f", c.Entry.ID, c.Score)
		}
		out = append(out, line)
	}
	out = append(out, fmt.Sprint(r.Cover), string(r.Verdict))
	return out
}

// ===========================================================================
// Decision 012: "amount" is contextual, never globally ambiguous
// ===========================================================================

// The user's Phase 4 gate decision, pinned as one table. The SAME word is
// ambiguous alone and decided in context, and every decision comes from
// evidence the diagnostics can show. There is deliberately no rule that says
// "amount is ambiguous": such a rule would be one row fitted, and it would
// wrongly refuse commerce.json, where exactly one field is named amount.
//
// Scores are MiniLM's, recorded in docs/phase-4-results.md.
func TestAmountIsResolvedByContextNotByRule(t *testing.T) {
	bothAmounts := []hit{
		{"field:order.amount", 1.0, "amount"},
		{"field:payment.amount", 1.0, "amount"},
		{"aggregation:sum", 0.676, "total"},
	}
	cases := []struct {
		name, cfg, sentence, phrase string
		hits                        map[string][]hit
		want                        Decision
		target                      string
		// evidence names what must have decided it, so a future change that
		// reaches the right answer for the wrong reason still fails.
		evidence func(Evidence) bool
	}{
		{
			name: "alone, two fields named amount: ambiguous", cfg: "ambiguity.json",
			sentence: "amount", phrase: "amount",
			hits: map[string][]hit{"amount": bothAmounts},
			want: Ambiguous,
		},
		{
			name: "order amount: an exact configured synonym", cfg: "ambiguity.json",
			sentence: "order amount", phrase: "order amount",
			hits: map[string][]hit{
				"order amount": {{"field:order.amount", 1.0, "order amount"}, {"field:payment.amount", 0.457, "payment amount"}},
				"amount":       bothAmounts,
				"order":        {{"entity:order", 1.0, "order"}},
			},
			want: Accept, target: "field:order.amount",
			evidence: func(e Evidence) bool { return e.Lexical == 1 },
		},
		{
			name: "payment amount: an exact configured synonym", cfg: "ambiguity.json",
			sentence: "payment amount", phrase: "payment amount",
			hits: map[string][]hit{
				"payment amount": {{"field:payment.amount", 1.0, "payment amount"}, {"field:order.amount", 0.637, "order amount"}},
				"amount":         bothAmounts,
				"payment":        {{"entity:payment", 1.0, "payment"}},
			},
			want: Accept, target: "field:payment.amount",
			evidence: func(e Evidence) bool { return e.Lexical == 1 },
		},
		{
			name: "payments with amount over 100: entity + number + operator", cfg: "ambiguity.json",
			sentence: "payments with amount over 100", phrase: "amount",
			hits: map[string][]hit{
				"payments": {{"entity:payment", 0.932, ""}, {"field:payment.status", 0.60, "payment status"}},
				"amount":   bothAmounts,
				"over":     {{"operator:gt", 1.0, "over"}, {"operator:lt", 0.44, "under"}},
			},
			want: Accept, target: "field:payment.amount",
			evidence: func(e Evidence) bool { return e.Entity == 1 && e.Type == 1 && e.Operator == 1 },
		},
		{
			name: "orders with amount over 100: the mirror image", cfg: "ambiguity.json",
			sentence: "orders with amount over 100", phrase: "amount",
			hits: map[string][]hit{
				"orders": {{"entity:order", 0.881, ""}, {"field:order.status", 0.60, "order status"}},
				"amount": bothAmounts,
				"over":   {{"operator:gt", 1.0, "over"}, {"operator:lt", 0.44, "under"}},
			},
			want: Accept, target: "field:order.amount",
			evidence: func(e Evidence) bool { return e.Entity == 1 && e.Type == 1 && e.Operator == 1 },
		},
		{
			name: "commerce: one field named amount, accepted on margin", cfg: "commerce.json",
			sentence: "amount", phrase: "amount",
			hits: map[string][]hit{"amount": {
				{"field:order.amount", 1.0, "amount"},
				{"aggregation:sum", 0.676, "total"},
				{"field:customer.total_spent", 0.658, "amount spent"},
				{"field:product.price", 0.621, "price"},
			}},
			want: Accept, target: "field:order.amount",
			// Decided by the margin (0.374) with no structural evidence at
			// all: the config names one field "amount".
			evidence: func(e Evidence) bool { return e.Entity == 0 && e.Type == 0 && e.Operator == 0 },
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			res := mustRank(t, script(t, loadRegistry(t, c.cfg), c.sentence, c.hits), DefaultPolicy())
			s, ok := covered(res)[c.phrase]
			if !ok {
				t.Fatalf("%q is not in the cover", c.phrase)
			}
			if s.Decision != c.want {
				t.Fatalf("%q → %s (margin %.3f), want %s", c.phrase, s.Decision, s.Margin, c.want)
			}
			if c.want != Accept {
				return
			}
			top, _ := s.TopCandidate()
			if top.Entry.ID != c.target {
				t.Fatalf("%q → %s, want %s", c.phrase, top.Entry.ID, c.target)
			}
			if !c.evidence(top.Evidence) {
				t.Errorf("right answer, unexpected evidence %+v", top.Evidence)
			}
		})
	}
}

// ===========================================================================
// Decision 019: a value needs literal evidence (Phase 7 finding V02)
// ===========================================================================

// "customers living in India" once bound status = 'active': the model put
// "living" at 0.790 from the alias "live", and nothing else asked for the word
// to appear. A value must now be literally present; similarity only discovers.
func TestValueNeedsLiteralEvidence(t *testing.T) {
	reg := loadRegistry(t, "commerce.json")
	res := mustRank(t, script(t, reg, "customers living in India", map[string][]hit{
		"customers": {{"entity:customer", 0.884, ""}},
		"living": {
			{"value:customer.status:alias:live", 0.790, "live"},
			{"value:customer.status:enum:active", 0.435, "active"},
			{"field:customer.country", 0.403, "location"},
		},
		"india": {{"value:customer.country:alias:India", 1.0, "India"}},
	}), DefaultPolicy())
	living := spanNamed(t, res, "living")
	if living.Decision == Accept {
		top, _ := living.TopCandidate()
		t.Fatalf("\"living\" was accepted as %s", top.Entry.ID)
	}
	for _, c := range living.Candidates {
		if c.Entry.Kind == corpus.KindValueAlias && !c.Vetoed {
			t.Errorf("%s has no literal evidence and was not vetoed", c.Entry.ID)
		}
	}
	// The literal value in the same sentence is untouched.
	if s := spanNamed(t, res, "india"); s.Decision != Accept {
		t.Errorf("literal India: decision = %s", s.Decision)
	}
}

// What still counts as literal: the configured alias itself, a declared enum
// value, and the light plural folding every lexical test applies. Nothing
// morphological beyond that — no stemming, so "living" never reaches "live".
func TestLiteralValueEvidence(t *testing.T) {
	reg := loadRegistry(t, "commerce.json")
	for sentence, c := range map[string]struct {
		span, id string
		accept   bool
	}{
		"live customers":      {"live", "value:customer.status:alias:live", true},
		"active customers":    {"active", "value:customer.status:enum:active", true},
		"tablets":             {"tablets", "value:product.category:enum:tablet", true},
		"LAPTOPS":             {"laptops", "value:product.category:alias:laptops", true},
		"lively customers":    {"lively", "value:customer.status:alias:live", false},
		"activated customers": {"activated", "value:customer.status:enum:active", false},
	} {
		hits := map[string][]hit{c.span: {{c.id, 0.8, ""}}}
		if strings.Contains(sentence, "customers") {
			hits["customers"] = []hit{{"entity:customer", 0.884, ""}}
		}
		res := mustRank(t, script(t, reg, sentence, hits), DefaultPolicy())
		got := spanNamed(t, res, c.span).Decision == Accept
		if got != c.accept {
			t.Errorf("%q: accepted = %v, want %v", sentence, got, c.accept)
		}
	}
}

// Phase 7 finding X03: "show customer value" reached total_spent at
// similarity 0.682 — under the threshold — only because "customer" added
// entity evidence. "value" is part of total_spent's "purchase value" AND of
// order.amount's "order value": the config gives the word two homes, and
// structure may not choose between them. Scores are MiniLM's.
func TestSharedFragmentUnderThresholdIsAmbiguous(t *testing.T) {
	reg := loadRegistry(t, "commerce.json")
	res := mustRank(t, script(t, reg, "show customer value", map[string][]hit{
		"customer": {{"entity:customer", 1.0, "customer"}},
		"value":    {{"field:customer.total_spent", 0.682, "purchase value"}, {"field:order.amount", 0.588, "amount"}},
	}), DefaultPolicy())
	s := spanNamed(t, res, "value")
	if s.Decision != Ambiguous || s.Code != qferr.CodeAmbiguousMatch {
		t.Fatalf("decision = %s", s.Decision)
	}
	named := false
	for _, i := range s.Competitors {
		if s.Candidates[i].Entry.ID == "field:order.amount" {
			named = true
		}
	}
	if !named {
		t.Error("the refusal must name order.amount, the word's other home")
	}
	if !s.SharedPhrase {
		t.Error("refused, but not by the shared-phrase rule")
	}
	err := res.Err()
	if err == nil || qferr.CodeOf(err) != qferr.CodeAmbiguousMatch {
		t.Fatalf("sentence verdict = %v", err)
	}
	// The first version reported this refusal as "(margin 0.294 < 0.100)",
	// which is false: the margin cleared the bar. The message must give the
	// rule's own reason.
	if msg := err.Error(); !strings.Contains(msg, "nothing in the sentence distinguishes them") ||
		strings.Contains(msg, "margin") {
		t.Errorf("refusal gives the wrong reason: %s", msg)
	}
}

// What the rule leaves alone, each measured on the query set or the probes.
func TestSharedFragmentRuleIsNarrow(t *testing.T) {
	reg := loadRegistry(t, "commerce.json")
	for _, c := range []struct {
		name, sentence, span, want string
		hits                       map[string][]hit
	}{
		// "show customer date" used to be here, accepted because its reading
		// cleared the threshold. It is now refused (the re-gate decision on
		// 019 §5): see TestSharedPhraseWithoutDistinguishingEvidenceIsRefused.
		// Under the threshold but with ONE home in the config.
		{"single-home fragment", "customers created last year", "created", "field:customer.created_at", map[string][]hit{
			"customers": {{"entity:customer", 0.884, ""}},
			"created":   {{"field:customer.created_at", 0.683, "created at"}, {"field:customer.name", 0.3, "name"}},
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := mustRank(t, script(t, reg, c.sentence, c.hits), DefaultPolicy())
			s := spanNamed(t, res, c.span)
			top, _ := s.TopCandidate()
			if s.Decision != Accept || top.Entry.ID != c.want {
				t.Errorf("decision %s, top %s; want accept %s", s.Decision, top.Entry.ID, c.want)
			}
		})
	}
}

// Your "unless the config explicitly defines it": with "customer value" as a
// configured synonym of total_spent, the phrase is an exact surface and the
// sentence resolves.
func TestConfiguredPhraseResolvesTheSharedWord(t *testing.T) {
	cfg := *loadRegistry(t, "commerce.json").Config()
	cfg.Fields = append([]config.Field(nil), cfg.Fields...)
	for i := range cfg.Fields {
		if cfg.Fields[i].Entity == "customer" && cfg.Fields[i].Name == "total_spent" {
			cfg.Fields[i].Synonyms = append(append([]string(nil), cfg.Fields[i].Synonyms...), "customer value")
		}
	}
	reg := config.NewSchemaRegistry(&cfg)
	res := mustRank(t, script(t, reg, "show customer value", map[string][]hit{
		"customer value": {{"field:customer.total_spent", 1.0, "customer value"}},
		"customer":       {{"entity:customer", 1.0, "customer"}},
		"value":          {{"field:customer.total_spent", 0.682, "purchase value"}, {"field:order.amount", 0.588, "amount"}},
	}), DefaultPolicy())
	if err := res.Err(); err != nil {
		t.Fatalf("configured phrase still refused: %v", err)
	}
	s := spanNamed(t, res, "customer value")
	if top, _ := s.TopCandidate(); s.Decision != Accept || top.Entry.ID != "field:customer.total_spent" {
		t.Errorf("decision %s", s.Decision)
	}
}

// ===========================================================================
// Decision 019 §5 at the re-gate: a shared phrase needs distinguishing evidence
// ===========================================================================

// withSynonym returns commerce.json with one more synonym on one field — the
// "the config explicitly defines it" half of the rule, without editing a
// shared fixture.
func withSynonym(t *testing.T, entity, field, synonym string) *config.SchemaRegistry {
	t.Helper()
	cfg := *loadRegistry(t, "commerce.json").Config()
	cfg.Fields = append([]config.Field(nil), cfg.Fields...)
	for i := range cfg.Fields {
		if cfg.Fields[i].Entity == entity && cfg.Fields[i].Name == field {
			cfg.Fields[i].Synonyms = append(append([]string(nil), cfg.Fields[i].Synonyms...), synonym)
		}
	}
	return config.NewSchemaRegistry(&cfg)
}

// hasReason reports whether one of c's reasons contains want.
func hasReason(c Candidate, want string) bool {
	for _, r := range c.Reasons {
		if strings.Contains(r, want) {
			return true
		}
	}
	return false
}

// Phase 7 finding X02, the user's decision at the re-gate: "show customer
// date" is REFUSED. "date" is part of created_at's "joined date" and
// placed_at's "order date", and "customer" does not tell them apart: placed_at
// is one join from customer, and a customer's order date is as much a
// "customer date". Before, the reading cleared the threshold on its own
// (0.711) and entity context carried it past placed_at (0.726), so a 0.015
// difference in raw similarity decided the answer. Scores are MiniLM's.
//
// The test pins the REASON, not only the code: the refusal must come from the
// shared-phrase rule (the score margin, 0.185, would have accepted), name
// both fields, carry the evidence that was weighed, and say so in a message
// that neither quotes the sentence nor claims a margin shortfall.
func TestSharedPhraseWithoutDistinguishingEvidenceIsRefused(t *testing.T) {
	reg := loadRegistry(t, "commerce.json")
	res := mustRank(t, script(t, reg, "show customer date", map[string][]hit{
		"customer": {{"entity:customer", 1.0, "customer"}, {"field:customer.name", 0.827, "customer name"}},
		"date":     {{"field:order.placed_at", 0.726, "order date"}, {"field:customer.created_at", 0.711, "joined date"}},
	}), DefaultPolicy())

	s := spanNamed(t, res, "date")
	if s.Decision != Ambiguous || s.Code != qferr.CodeAmbiguousMatch {
		t.Fatalf("decision = %s (%s), want ambiguous", s.Decision, s.Code)
	}
	if !s.SharedPhrase {
		t.Fatal("refused, but not by the shared-phrase rule")
	}
	// The margin alone would have ACCEPTED: this is the rule refusing, not the
	// margin. If this ever fails, the test no longer exercises the rule.
	if s.Margin+scoreEpsilon < res.Policy.MinMargin {
		t.Fatalf("margin %.3f is under %.2f: the refusal would be the margin's", s.Margin, res.Policy.MinMargin)
	}
	if s.Confidence != 0 {
		t.Errorf("confidence = %.2f, want 0 for a refusal", s.Confidence)
	}

	// Exactly the two date fields, leading reading first.
	named := map[string]bool{topID(s): true}
	for _, i := range s.Competitors {
		named[s.Candidates[i].Entry.ID] = true
	}
	want := map[string]bool{"field:customer.created_at": true, "field:order.placed_at": true}
	if !reflect.DeepEqual(named, want) {
		t.Errorf("refusal names %v, want exactly %v", named, want)
	}

	// The evidence, as recorded. The leading reading was carried by entity
	// context, and the rule says that context is not what distinguishes them.
	top, _ := s.TopCandidate()
	if top.Entry.ID != "field:customer.created_at" || top.Evidence.Lexical != 0 || top.Evidence.Entity != 1 {
		t.Errorf("leading reading %s with evidence %+v; want created_at carried by entity context", top.Entry.ID, top.Evidence)
	}
	if !hasReason(top, "entity: the sentence names customer") {
		t.Errorf("the context that was weighed is not recorded: %q", top.Reasons)
	}
	if !hasReason(top, "spells out neither target's configured surface, so nothing distinguishes them") {
		t.Errorf("the refusal's reason is not recorded: %q", top.Reasons)
	}
	for _, i := range s.Competitors {
		if c := s.Candidates[i]; !hasReason(c, "the span's words are also part of this target's configured surface") {
			t.Errorf("%s: why it competes is not recorded: %q", c.Entry.ID, c.Reasons)
		}
	}

	// The sentence verdict and its message.
	if got := covered(res)["date"]; got.Decision != Ambiguous {
		t.Fatal("the refused span is not the cover's reading of \"date\"")
	}
	err := res.Err()
	if err == nil || qferr.CodeOf(err) != qferr.CodeAmbiguousMatch {
		t.Fatalf("sentence verdict = %v", err)
	}
	msg := err.Error()
	for _, w := range []string{"tokens[2:3]", "nothing in the sentence distinguishes them", "field:customer.created_at", "field:order.placed_at"} {
		if !strings.Contains(msg, w) {
			t.Errorf("message lacks %q: %s", w, msg)
		}
	}
	if strings.Contains(msg, "margin") || strings.Contains(msg, `"date"`) {
		t.Errorf("message claims a margin shortfall or quotes the sentence: %s", msg)
	}
	var v *VerdictError
	if !errors.As(err, &v) || !strings.Contains(v.ErrorRevealed(), `the phrase "date"`) {
		t.Errorf("the debug rendering must quote the phrase: %v", err)
	}
}

// "Do not let the small score difference determine the answer": whichever
// field leads on similarity, and by however much, "show customer date" is
// refused. Only the sentence's words may settle it.
func TestSharedPhraseRefusalIgnoresTheSimilarityOrder(t *testing.T) {
	reg := loadRegistry(t, "commerce.json")
	for _, c := range []struct {
		name              string
		created, placedAt float64
	}{
		{"as measured: placed_at ahead by 0.015", 0.711, 0.726},
		{"swapped: created_at ahead by 0.015", 0.726, 0.711},
		{"created_at far ahead", 0.950, 0.600},
		{"placed_at far ahead, but context still picks created_at", 0.700, 0.760},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := mustRank(t, script(t, reg, "show customer date", map[string][]hit{
				"customer": {{"entity:customer", 1.0, "customer"}},
				"date":     {{"field:customer.created_at", c.created, "joined date"}, {"field:order.placed_at", c.placedAt, "order date"}},
			}), DefaultPolicy())
			s := spanNamed(t, res, "date")
			if s.Decision == Accept {
				t.Fatalf("accepted as %s", topID(s))
			}
			if err := res.Err(); err == nil || qferr.CodeOf(err) != qferr.CodeAmbiguousMatch {
				t.Errorf("sentence verdict = %v", err)
			}
		})
	}
}

// The other half of the principle: explicit evidence resolves the shared
// word, so the rule is not a blanket "shared words are ambiguous". Each row is
// one of the user's examples, or the probe row the uniform variant broke.
func TestSharedPhraseWithDistinguishingEvidenceResolves(t *testing.T) {
	commerce := loadRegistry(t, "commerce.json")
	dateHits := []hit{{"field:order.placed_at", 0.726, "order date"}, {"field:customer.created_at", 0.711, "joined date"}}
	for _, c := range []struct {
		name, sentence, span, want string
		reg                        *config.SchemaRegistry
		hits                       map[string][]hit
		// viaRule: the shared-phrase rule saw the span and accepted it on the
		// sentence's words (recorded as a reason); otherwise the span matched
		// a configured surface itself and the rule never applied.
		viaRule bool
	}{
		{
			// The span IS created_at's configured synonym.
			name: "customer creation date → created_at", sentence: "show customer creation date",
			span: "customer creation date", want: "field:customer.created_at", reg: commerce,
			hits: map[string][]hit{
				"customer creation date": {{"field:customer.created_at", 1.0, "customer creation date"}, {"field:order.placed_at", 0.636, "purchase date"}},
				"customer":               {{"entity:customer", 1.0, "customer"}},
				"date":                   dateHits,
			},
		},
		{
			// The span IS placed_at's configured synonym. (The whole sentence
			// is refused end to end for other reasons; see decision 019 §5.)
			name: "customer order date → placed_at", sentence: "show customer order date",
			span: "order date", want: "field:order.placed_at", reg: commerce,
			hits: map[string][]hit{
				"order date": {{"field:order.placed_at", 1.0, "order date"}, {"field:customer.created_at", 0.6, "joined date"}},
				"customer":   {{"entity:customer", 1.0, "customer"}},
				"order":      {{"entity:order", 1.0, "order"}},
				"date":       dateHits,
			},
		},
		{
			// An explicit config alias: "customer date" configured on
			// created_at. The same sentence as the refusal, one synonym more.
			name: "configured customer date → created_at", sentence: "show customer date",
			span: "customer date", want: "field:customer.created_at",
			reg: withSynonym(t, "customer", "created_at", "customer date"),
			hits: map[string][]hit{
				"customer date": {{"field:customer.created_at", 1.0, "customer date"}},
				"customer":      {{"entity:customer", 1.0, "customer"}},
				"date":          dateHits,
			},
		},
		{
			// "order date" is spelled out, not contiguously: the rule reads
			// the sentence's words, not the span's neighbours. The uniform
			// variant refused this (decision 019 §5, probes 32/34).
			name: "orders by date → placed_at", sentence: "orders by date",
			span: "date", want: "field:order.placed_at", reg: commerce, viaRule: true,
			hits: map[string][]hit{
				"orders": {{"entity:order", 0.881, "order"}, {"relationship:customer_orders", 0.805, "ordered"}},
				"date":   dateHits,
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := mustRank(t, script(t, c.reg, c.sentence, c.hits), DefaultPolicy())
			s := spanNamed(t, res, c.span)
			top, _ := s.TopCandidate()
			if s.Decision != Accept || top.Entry.ID != c.want {
				t.Fatalf("%q: decision %s, top %s; want accept %s", c.span, s.Decision, top.Entry.ID, c.want)
			}
			if got, ok := covered(res)[c.span]; !ok || topID(got) != c.want {
				t.Errorf("the cover does not read %q as %s: %v", c.span, c.want, covered(res))
			}
			if c.viaRule != hasReason(top, "every word of this target's configured surface is in the sentence") {
				t.Errorf("evidence reason present = %v, want %v: %q", !c.viaRule, c.viaRule, top.Reasons)
			}
			// Only the cover is the reading. A span outside it may still be
			// refused by the rule: in "show customer order date", the lone
			// "date" beside an unscripted "order" would lead with created_at
			// while the sentence spells out "order date", and refusing that
			// is the rule working.
			for norm, sp := range covered(res) {
				if sp.SharedPhrase {
					t.Errorf("covered span %q was refused by the shared-phrase rule", norm)
				}
			}
			if c.span != "order date" { // that sentence's other spans are not scripted to resolve
				if err := res.Err(); err != nil {
					t.Errorf("sentence refused: %v", err)
				}
			}
		})
	}
}

// Evidence must single out the LEADING reading. When the sentence spells out
// another target's phrase, or several targets' phrases, the evidence disagrees
// with the ranker or does not decide, and the span is refused — never switched
// to the spelled-out target, which would be the rule choosing a reading.
func TestSharedPhraseEvidenceMustSingleOutTheReading(t *testing.T) {
	reg := loadRegistry(t, "commerce.json")
	for _, c := range []struct {
		name, sentence, reason string
		hits                   map[string][]hit
	}{
		{
			// Scores chosen so that created_at leads despite "orders": the
			// sentence spells out placed_at's "order date", not created_at's.
			name: "spelled-out phrase belongs to the runner-up", sentence: "orders by date",
			reason: "spells out a configured surface of another target, not this one",
			hits: map[string][]hit{
				"orders": {{"entity:order", 0.881, "order"}},
				"date":   {{"field:customer.created_at", 0.950, "joined date"}, {"field:order.placed_at", 0.600, "order date"}},
			},
		},
		{
			// "joined" and "order" both present: "joined date" AND "order date"
			// are spelled out, so the words do not decide.
			name: "phrases of both targets spelled out", sentence: "joined orders by date",
			reason: "spells out a configured surface of more than one of them",
			hits: map[string][]hit{
				"orders": {{"entity:order", 0.881, "order"}},
				"joined": {{"field:customer.created_at", 0.800, "joined date"}},
				// Far apart, so the margin alone would accept placed_at.
				"date": {{"field:order.placed_at", 0.900, "order date"}, {"field:customer.created_at", 0.600, "joined date"}},
			},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			res := mustRank(t, script(t, reg, c.sentence, c.hits), DefaultPolicy())
			s := spanNamed(t, res, "date")
			if s.Decision != Ambiguous || !s.SharedPhrase {
				t.Fatalf("decision %s (shared-phrase %v), want refused by the rule", s.Decision, s.SharedPhrase)
			}
			if top, _ := s.TopCandidate(); !hasReason(top, c.reason) {
				t.Errorf("reason not recorded: %q", top.Reasons)
			}
		})
	}
}

// Only a target's name, synonyms and phrases make a word shared, and only they
// can be spelled out: descriptions are prose, and their common words would
// make almost every word shared (decision 019 §5). Found as a surviving
// mutation: nothing pinned it.
func TestSharedPhraseIgnoresDescriptions(t *testing.T) {
	entries := map[string]corpus.Entry{}
	for _, e := range corpus.Build(loadRegistry(t, "commerce.json")) {
		entries[e.ID] = e
	}
	for _, c := range []struct {
		word, id string
		want     bool
	}{
		{"date", "field:customer.created_at", true},       // "joined date"
		{"date", "field:order.placed_at", true},           // "order date"
		{"account", "field:customer.created_at", false},   // only its description: "Date the customer account was created."
		{"located", "field:customer.country", false},      // only its description
		{"single", "entity:order", false},                 // only its description
		{"customer", "field:customer.created_at", true},   // "customer creation date"
		{"creation", "field:customer.created_at", true},   // "customer creation date"
		{"placed", "field:order.placed_at", true},         // its name, "placed at"
		{"placed", "relationship:customer_orders", false}, // "placed" is the whole synonym, not a fragment
	} {
		if got := fragmentOf(foldWords(c.word), entries[c.id]); got != c.want {
			t.Errorf("%q a fragment of %s = %v, want %v", c.word, c.id, got, c.want)
		}
	}
	// And a description never spells a target out: every word of
	// created_at's description is in this sentence, none of its phrases are.
	sentence := map[string]bool{}
	for _, w := range foldWords("date the customer account was created") {
		sentence[w] = true
	}
	if wholeSurfaceIn(foldWords("date"), entries["field:customer.created_at"], sentence) {
		t.Error("a description counted as a spelled-out configured phrase")
	}
}
