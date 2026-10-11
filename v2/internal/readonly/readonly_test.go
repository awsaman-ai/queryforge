package readonly

import (
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/ranking"
)

// Section 16.1 (decision 021). The rule has three parts, each tested in both
// directions: which words are write keywords, what explains one, and what
// does not.

// reading builds a ranking result for a sentence in which the span `phrase`
// (a whole-token substring of the sentence) is accepted as `entry`. Nothing
// else in the sentence is accepted.
func reading(t *testing.T, sentence, phrase string, entry corpus.Entry) (nlp.Analysis, ranking.Result) {
	t.Helper()
	a := nlp.Analyze(sentence, nlp.DefaultOptions)
	for i, s := range a.Spans {
		if s.Kind == nlp.SpanText && s.Norm == phrase {
			return a, ranking.Result{
				Spans: []ranking.SpanRanking{{Index: i, Span: s, Candidates: []ranking.Candidate{{Entry: entry}},
					Decision: ranking.Accept, Top: 0, RunnerUp: -1}},
				Cover: []int{0},
			}
		}
	}
	t.Fatalf("%q is not a span of %q", phrase, sentence)
	return a, ranking.Result{}
}

func field(name string, surfaces ...string) corpus.Entry {
	e := corpus.Entry{ID: "field:item." + name, Kind: corpus.KindField, Entity: "item", Ref: name}
	for _, s := range surfaces {
		e.Surfaces = append(e.Surfaces, corpus.Surface{Kind: corpus.SurfaceSynonym, Text: s})
	}
	return e
}

var customers = corpus.Entry{ID: "entity:customer", Kind: corpus.KindEntity, Entity: "customer",
	Surfaces: []corpus.Surface{{Kind: corpus.SurfaceName, Text: "customer"}}}

func refused(t *testing.T, err error, keyword string) {
	t.Helper()
	if err == nil {
		t.Fatalf("not refused; want SECURITY_REJECTED for %s", keyword)
	}
	if qferr.CodeOf(err) != qferr.CodeSecurityRejected {
		t.Fatalf("code %s, want SECURITY_REJECTED: %v", qferr.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "is the write keyword "+keyword+"; the engine only reads") {
		t.Errorf("message does not give the reason: %v", err)
	}
}

// Every keyword, in its bare and plural forms and in any case, is refused
// when nothing configured explains it.
func TestEveryWriteKeywordIsRefused(t *testing.T) {
	if len(Keywords) != 12 {
		t.Fatalf("section 16.1's 11 statements plus MERGE, got %d", len(Keywords))
	}
	for folded, name := range Keywords {
		for _, form := range []string{folded, strings.ToUpper(folded), folded + "s"} {
			a, r := reading(t, form+" all customers", "customers", customers)
			refused(t, Check(a, r), name)
		}
	}
}

// Inflected forms are read vocabulary, not writes: "created", "deleted",
// "updating", "dropped" are not keywords. "copies" folds to "copie", not
// "copy". Other rules may still refuse these sentences; this one does not.
func TestInflectedFormsAreNotKeywords(t *testing.T) {
	for _, w := range []string{"created", "deleted", "updating", "dropped", "inserted", "copies", "calling", "merged"} {
		a, r := reading(t, "customers "+w, "customers", customers)
		if err := Check(a, r); err != nil {
			t.Errorf("%q: %v", w, err)
		}
	}
}

// Explained LITERALLY: the keyword is in an accepted span, the accepted
// target has a configured phrase containing it, and every word of that phrase
// is in the sentence.
func TestAConfiguredPhraseExplainsTheKeyword(t *testing.T) {
	a, r := reading(t, "items by update date", "update date", field("updated_at", "update date"))
	if err := Check(a, r); err != nil {
		t.Fatalf("a configured phrase spelled out in full was refused: %v", err)
	}
	// Non-contiguous, as decision 019 §5 allows: "update" is in the accepted
	// span, and "last" — the rest of the configured phrase — is elsewhere in
	// the sentence.
	a, r = reading(t, "last items by update", "update", field("updated_at", "last update"))
	if err := Check(a, r); err != nil {
		t.Fatalf("a configured phrase spelled out non-contiguously was refused: %v", err)
	}
}

// NOT explained by similarity: the span is accepted, but as a target none of
// whose configured phrases contains the keyword. This is the "show customers
// by update date" -> created_at case.
func TestSimilarityDoesNotExplainTheKeyword(t *testing.T) {
	a, r := reading(t, "items by update date", "update date", field("created_at", "joined date", "registration date"))
	refused(t, Check(a, r), "UPDATE")
}

// NOT explained by a PARTIAL configured phrase: the target's phrase contains
// the keyword, but the sentence does not spell the phrase out.
func TestAPartialPhraseDoesNotExplainTheKeyword(t *testing.T) {
	a, r := reading(t, "items by update date", "update date", field("updated_at", "last update date"))
	refused(t, Check(a, r), "UPDATE")
}

// NOT explained by a description: prose is not a configured phrase.
func TestADescriptionDoesNotExplainTheKeyword(t *testing.T) {
	e := field("updated_at")
	e.Surfaces = append(e.Surfaces, corpus.Surface{Kind: corpus.SurfaceDescription, Text: "update date"})
	a, r := reading(t, "items by update date", "update date", e)
	refused(t, Check(a, r), "UPDATE")
}

// NOT explained by an accepted span elsewhere: the keyword's own token must be
// covered. Here "update date" is configured and in the sentence, but the
// keyword the rule sees is the separate "delete".
func TestOnlyTheKeywordsOwnSpanCounts(t *testing.T) {
	a, r := reading(t, "delete items by update date", "update date", field("updated_at", "update date"))
	refused(t, Check(a, r), "DELETE")
	// The same keyword inside and outside the configured phrase: the phrase
	// explains its own "update", not the one at token 0. (A check on the span
	// alone, not the token, passed the case above and failed this one: found
	// by the Phase 8 mutation run.)
	a, r = reading(t, "update items by update date", "update date", field("updated_at", "update date"))
	err := Check(a, r)
	refused(t, err, "UPDATE")
	if !strings.Contains(err.Error(), "tokens[0:1]") {
		t.Errorf("refused, but not for the uncovered keyword: %v", err)
	}
}

// An ambiguous span explains nothing, even when its leading target's phrase
// contains the keyword: only an ACCEPTED reading is what the sentence means.
func TestAnAmbiguousSpanDoesNotExplain(t *testing.T) {
	a, r := reading(t, "items by update date", "update date", field("updated_at", "update date"))
	r.Spans[0].Decision = ranking.Ambiguous
	refused(t, Check(a, r), "UPDATE")
}

// The refusal names the keyword from the closed list and a token position. It
// never quotes the sentence (decision 013).
func TestTheRefusalDoesNotQuoteTheSentence(t *testing.T) {
	a, r := reading(t, "Zorblattia DROP customers", "customers", customers)
	err := Check(a, r)
	refused(t, err, "DROP")
	if strings.Contains(err.Error(), "Zorblattia") || !strings.Contains(err.Error(), "tokens[1:2]") {
		t.Errorf("message: %v", err)
	}
}
