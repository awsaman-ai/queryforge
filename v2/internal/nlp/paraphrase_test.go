package nlp

import (
	"strings"
	"testing"
)

// The Phase 3 exit criterion (section 11): "PASS when phrase extraction is
// stable enough to feed Phase 4 without hardcoding complete example sentences."
//
// These tests use the brief's own manual-gate sentences. They assert that the
// spans a later phase NEEDS are present — not that the span set is exactly some
// list, which would be the hardcoding the criterion rules out.

// requireSpans fails unless every wanted surface is among the candidates.
func requireSpans(t *testing.T, sentence string, want ...string) Analysis {
	t.Helper()
	a := analyze(t, sentence)
	have := spanTexts(a)
	for _, w := range want {
		if !have[w] {
			t.Errorf("%q: expected a candidate span %q\ngot: %s", sentence, w, strings.Join(sortedKeys(have), " | "))
		}
	}
	return a
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j] < out[j-1]; j-- {
			out[j], out[j-1] = out[j-1], out[j]
		}
	}
	return out
}

// The six sentences from section 11's manual gate.
func TestBriefManualGateSentences(t *testing.T) {
	t.Run("Show customers from India", func(t *testing.T) {
		requireSpans(t, "Show customers from India", "customers", "india", "from india")
	})

	t.Run("customers living in India", func(t *testing.T) {
		requireSpans(t, "customers living in India", "customers", "living", "india", "living in india")
	})

	t.Run("customers who spent more than 50000", func(t *testing.T) {
		a := requireSpans(t, "customers who spent more than 50000",
			"customers", "spent", "more than", "spent more than")
		if len(a.Numbers) != 1 || a.Numbers[0].Value.String() != "50000" {
			t.Errorf("expected the value 50000, got %+v", a.Numbers)
		}
	})

	t.Run("customers spending over fifty thousand", func(t *testing.T) {
		a := requireSpans(t, "customers spending over fifty thousand",
			"customers", "spending", "over", "spending over")
		if len(a.Numbers) != 1 || a.Numbers[0].Value.String() != "50000" {
			t.Errorf("expected the value 50000, got %+v", a.Numbers)
		}
		if !a.Numbers[0].Spelled {
			t.Error("the value was written in words and should be marked spelled")
		}
	})

	t.Run("customers created last year", func(t *testing.T) {
		a := requireSpans(t, "customers created last year", "customers", "created")
		if len(a.Dates) != 1 {
			t.Fatalf("expected one date, got %+v", a.Dates)
		}
		if a.Dates[0].Unit != UnitYear || a.Dates[0].Anchor != AnchorCalendar || a.Dates[0].Offset != -1 {
			t.Errorf("got %+v, want the previous calendar year", a.Dates[0])
		}
	})

	t.Run("customers who registered in the last six months", func(t *testing.T) {
		a := requireSpans(t, "customers who registered in the last six months",
			"customers", "registered")
		if len(a.Dates) != 1 {
			t.Fatalf("expected one date, got %+v", a.Dates)
		}
		if a.Dates[0].Unit != UnitMonth || a.Dates[0].Anchor != AnchorRolling || a.Dates[0].Count != 6 {
			t.Errorf("got %+v, want a rolling six-month window", a.Dates[0])
		}
	})
}

// The stability requirement: paraphrases of one concept must each yield the
// span that carries it. They need not yield the SAME span — "spent" and
// "spending" are different words — only a span the corpus can match, which is
// what lets Phase 4 do its job without a rule per sentence.
func TestParaphrasesYieldTheCarryingSpan(t *testing.T) {
	groups := []struct {
		concept   string
		sentences map[string]string // sentence -> the span that must carry the concept
	}{
		{
			concept: "the spending field",
			sentences: map[string]string{
				"customers who spent more than 50000":      "spent",
				"customers spending over 50000":            "spending",
				"customers whose spending is above 50000":  "spending",
				"customers with purchase value over 50000": "purchase value",
			},
		},
		{
			concept: "the country field",
			sentences: map[string]string{
				"customers from India":      "from india",
				"customers living in India": "living in india",
				"clients located in India":  "located in india",
				"buyers in India":           "india",
			},
		},
		{
			concept: "the greater-than operator",
			sentences: map[string]string{
				"spent more than 50000":  "more than",
				"spent over 50000":       "over",
				"spending above 50000":   "above",
				"spent greater than 500": "greater than",
			},
		},
	}

	for _, g := range groups {
		t.Run(g.concept, func(t *testing.T) {
			for sentence, span := range g.sentences {
				if !spanTexts(analyze(t, sentence))[span] {
					t.Errorf("%q should produce the span %q", sentence, span)
				}
			}
		})
	}
}

// The two Phase 2 retrieval misses that Phase 3 exists to fix. Both failed
// because the whole sentence was embedded as one string; both must now produce
// the separate spans that let retrieval find each half.
func TestPhase2MissesNowSplitIntoSpans(t *testing.T) {
	t.Run("client location", func(t *testing.T) {
		// Phase 2 retrieved the customer ENTITY for this whole phrase, because
		// "client" is a strong entity synonym. Split, "location" can reach the
		// country field on its own.
		requireSpans(t, "client location", "client", "location", "client location")
	})

	t.Run("recent customer", func(t *testing.T) {
		// Phase 2 did not retrieve created_at at all: "recent" matched the
		// pattern and "customer" the entity, and the combined string matched
		// neither well.
		requireSpans(t, "recent customer", "recent", "customer", "recent customer")
	})
}

// Extraction must not depend on a config. What a span MEANS needs the schema;
// what a span IS does not, and a coupling here would make Phase 3 untestable in
// isolation and its failures indistinguishable from config problems.
func TestExtractionIsConfigIndependent(t *testing.T) {
	a := analyze(t, "customers from Atlantis who spent more than 50000 zorkmids")
	if len(a.Spans) == 0 {
		t.Fatal("unknown vocabulary should still produce spans")
	}
	have := spanTexts(a)
	if !have["atlantis"] || !have["zorkmids"] {
		t.Error("spans are produced for words no config declares")
	}
	if len(a.Numbers) != 1 {
		t.Errorf("the value should still be extracted, got %+v", a.Numbers)
	}
}
