package ranking

import (
	"testing"

	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/qferr"
)

// Decision 021 (Phase 8, section 16.8; finding "update date"): a span that
// qualifies a GENERIC word — a strict fragment of several targets' configured
// surfaces — with a content word none of the reading's configured surfaces
// contains names a target the config does not have. It is rejected as a
// reading, whatever the sentence spells out elsewhere, and the cover reads its
// words separately. Scores are real MiniLM numbers, measured before the rule.

const qualifierReason = "qualifies a word shared by several targets' configured surfaces with a word none of this target's configured surfaces contains"

// The finding itself. Before the rule, "update date" was accepted as
// created_at on entity context (0.575 + 0.15) with a 0.369 margin, and the
// sentence compiled to SELECT created_at.
func TestAnUnconfiguredQualifierOfASharedWordIsRejected(t *testing.T) {
	reg := loadRegistry(t, "commerce.json")
	res := mustRank(t, script(t, reg, "show customers by update date", map[string][]hit{
		"customers":   {{"entity:customer", 0.884, "customer"}, {"field:customer.name", 0.755, "customer name"}},
		"update date": {{"field:order.placed_at", 0.606, "order date"}, {"field:customer.created_at", 0.575, "joined date"}},
		"date":        {{"field:order.placed_at", 0.726, "order date"}, {"field:customer.created_at", 0.711, "joined date"}},
	}), DefaultPolicy())

	s := spanNamed(t, res, "update date")
	if s.Decision != Reject || s.Code != qferr.CodeNoSemanticMatch {
		t.Fatalf("decision = %s (%s), want reject", s.Decision, s.Code)
	}
	top, _ := s.TopCandidate()
	if top.Entry.ID != "field:customer.created_at" || !hasReason(top, qualifierReason) {
		t.Fatalf("leading reading %s, reasons %q", top.Entry.ID, top.Reasons)
	}
	// Premise: the score and the margin alone would have ACCEPTED. If this
	// fails, the test no longer exercises the rule.
	if top.Score < res.Policy.MinScore || s.Margin+scoreEpsilon < res.Policy.MinMargin {
		t.Fatalf("score %.3f, margin %.3f: the policy alone would refuse", top.Score, s.Margin)
	}
	// The cover reads the words separately: "date" meets §5 and is refused,
	// "update" is left unexplained for binding.
	if got := covered(res)["date"]; got.Decision != Ambiguous || !got.SharedPhrase {
		t.Errorf(`"date" in the cover: %s shared=%v`, got.Decision, got.SharedPhrase)
	}
	if !unexplainedAt(res, 3) {
		t.Errorf(`"update" (token 3) is not unexplained: %v`, res.Unexplained)
	}
	if err := res.Err(); err == nil || qferr.CodeOf(err) != qferr.CodeAmbiguousMatch {
		t.Errorf("verdict %v, want AMBIGUOUS_MATCH", err)
	}
}

// Spelled-out evidence elsewhere does not rescue the span. "orders" spells out
// placed_at's "order date" (decision 019 §5, X12), which settles "date" — and
// before the rule "shipping" was silently dropped: SELECT placed_at, at a
// similarity (0.729) ABOVE the floor, so no similarity threshold would have
// caught it. Now "date" alone is settled as placed_at and "shipping" is read
// on its own, where — with its real candidates — it is ambiguous, and the
// sentence is refused. (Had context carried "shipping" to one target on its
// own, that reading would stand: that is decision 011's context evidence, not
// this rule, and decision 021 records it as the residual risk.)
func TestSpelledOutEvidenceDoesNotRescueAnUnconfiguredQualifier(t *testing.T) {
	reg := loadRegistry(t, "commerce.json")
	res := mustRank(t, script(t, reg, "orders by shipping date", map[string][]hit{
		"orders":        {{"entity:order", 0.881, "order"}, {"relationship:customer_orders", 0.805, "ordered"}},
		"shipping date": {{"field:order.placed_at", 0.729, "order date"}, {"entity:order", 0.420, "order"}},
		"date":          {{"field:order.placed_at", 0.726, "order date"}, {"field:customer.created_at", 0.711, "joined date"}},
		"shipping": {
			{"field:product.price", 0.587, "price"}, {"entity:order", 0.568, "purchase"},
			{"relationship:customer_orders", 0.541, "ordered"}, {"entity:customer", 0.500, "buyer"},
			{"field:order.amount", 0.483, "order total"}, {"field:order.placed_at", 0.480, "ordered on"},
		},
	}), DefaultPolicy())

	s := spanNamed(t, res, "shipping date")
	top, _ := s.TopCandidate()
	if s.Decision != Reject || !hasReason(top, qualifierReason) {
		t.Fatalf("decision = %s, reasons %q", s.Decision, top.Reasons)
	}
	if top.Similarity < res.Policy.MinScore {
		t.Fatalf("premise: similarity %.3f should be above the floor", top.Similarity)
	}
	if got := covered(res)["date"]; got.Decision != Accept {
		t.Errorf(`"date": %s, want accepted (settled by "orders")`, got.Decision)
	} else if c, _ := got.TopCandidate(); c.Entry.ID != "field:order.placed_at" {
		t.Errorf(`"date" read as %s`, c.Entry.ID)
	}
	// "shipping" is not silently dropped: it is read, and refused, on its own.
	if got := covered(res)["shipping"]; got.Decision != Ambiguous {
		t.Errorf(`"shipping" in the cover: %s, want ambiguous`, got.Decision)
	}
	if err := res.Err(); err == nil || qferr.CodeOf(err) != qferr.CodeAmbiguousMatch {
		t.Errorf("verdict %v, want AMBIGUOUS_MATCH", err)
	}
}

// NARROW, direction 1: a SPECIFIC word — one home in the config — with an
// extra word is untouched. "spent" is only total_spent's; "money spent" is the
// brief's pure-paraphrase probe.
func TestAnExtraWordOnASpecificWordIsUntouched(t *testing.T) {
	reg := loadRegistry(t, "commerce.json")
	res := mustRank(t, script(t, reg, "money spent", map[string][]hit{
		"money spent": {{"field:customer.total_spent", 0.800, "spending"}, {"field:order.amount", 0.550, "order amount"}},
	}), DefaultPolicy())
	s := spanNamed(t, res, "money spent")
	if s.Decision != Accept {
		t.Fatalf("decision = %s (%s)", s.Decision, s.Code)
	}
	if top, _ := s.TopCandidate(); hasReason(top, qualifierReason) {
		t.Errorf("the rule fired on a specific word: %q", top.Reasons)
	}
}

// NARROW, direction 2: a span with NO configured word is semantic linking and
// untouched: "registered" is in none of created_at's surfaces ("registration
// date" folds differently), and is read by meaning.
func TestAPureParaphraseIsUntouched(t *testing.T) {
	reg := loadRegistry(t, "commerce.json")
	res := mustRank(t, script(t, reg, "customers registered", map[string][]hit{
		"customers":  {{"entity:customer", 0.884, "customer"}},
		"registered": {{"field:customer.created_at", 0.640, "registration date"}, {"field:order.placed_at", 0.450, "order date"}},
	}), DefaultPolicy())
	s := spanNamed(t, res, "registered")
	if s.Decision != Accept {
		t.Fatalf("decision = %s (%s)", s.Decision, s.Code)
	}
}

// A function word is not a qualifier: only CONTENT words the reading does not
// configure are set aside as evidence against it.
func TestFunctionWordsAreNotQualifiers(t *testing.T) {
	reg := loadRegistry(t, "commerce.json")
	in := script(t, reg, "date of the order", nil)
	r := newRanker(in, DefaultPolicy())
	var created corpus.Entry
	for _, e := range corpus.Build(reg) {
		if e.ID == "field:customer.created_at" {
			created = e
		}
	}
	span := nlp.Span{Start: 0, End: 3} // "date of the"
	known, setAside := r.configuredWords(span, created)
	if setAside {
		t.Errorf("function words were counted as qualifiers (known %v)", known)
	}
	span = nlp.Span{Start: 0, End: 4} // "date of the order": "order" is content
	if _, setAside = r.configuredWords(span, created); !setAside {
		t.Error(`"order" is a content word none of created_at's surfaces contains`)
	}
}

func unexplainedAt(res Result, token int) bool {
	for _, i := range res.Unexplained {
		if i == token {
			return true
		}
	}
	return false
}
