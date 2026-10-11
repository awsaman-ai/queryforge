// Package ranking turns retrieval's nearest neighbours into decisions.
//
// Phase 2 retrieval answers "which corpus entries are close to this phrase?"
// Phase 4 answers a different question: "is there ONE entry this phrase should
// be read as, and how sure is that?" The two are kept apart on purpose, and the
// separation shows up as three different numbers on every candidate:
//
//	Similarity   the retrieval cosine. Evidence, and nothing more. A 1.000 means
//	             a surface form matched exactly — which is precisely what happens
//	             when a word several fields share ("name", "amount") is typed on
//	             its own. It is NOT confidence.
//
//	Score        the ranking score: similarity plus weighted structural evidence
//	             (lexical, type, entity, operator, relationship, context, span
//	             quality). Comparable only between candidates for the SAME span.
//	             Not a probability, and not bounded to [0, 1].
//
//	Confidence   a property of the DECISION, not of any candidate. Derived from
//	             the top-1 vs runner-up margin: a candidate scoring 1.0 against a
//	             runner-up scoring 1.0 has zero confidence, however high both
//	             numbers are. See SpanRanking.Confidence.
//
// Everything here is deterministic and explainable: no training, no learned
// weights, no randomness, no map-order dependence. Every point of evidence is a
// named signal with a recorded reason, and every decision can be reproduced by
// hand from the diagnostics. See docs/decisions/011-ranking-formula.md.
//
// REDACTION: candidates can be config VALUES (value aliases, enum values). The
// ids of those embed the value, and their matched surface IS the value. Nothing
// in this package exposes either without an explicit reveal flag; see
// Candidate.DisplayTarget and Candidate.MatchedText.
package ranking

import (
	"fmt"
	"math"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/qferr"
)

// Decision is the outcome for one span.
type Decision string

const (
	// Accept means one target won by a margin the policy trusts.
	Accept Decision = "accept"
	// Ambiguous means at least two disagreeing targets are too close to call.
	// This is a refusal to guess, not a failure to find anything.
	Ambiguous Decision = "ambiguous"
	// Reject means nothing was good enough to be worth arguing about.
	Reject Decision = "reject"
)

// scoreEpsilon absorbs floating-point noise at the decision boundaries.
//
// Without it, a margin that is 0.1 on paper (0.9 - 0.8) evaluates to
// 0.09999999999999998 and a policy of "minMargin 0.1" would reject it — a
// decision flipping on the fourteenth decimal place, which nobody reading the
// diagnostics could reproduce. Also the tolerance within which two scores are a
// TIE. Far below any difference the scores can meaningfully express: cosine
// similarities come from float32 vectors.
const scoreEpsilon = 1e-9

// Weights scales each evidence signal before it is added to similarity.
//
// Every signal is in [-1, +1] with 0 meaning "no evidence either way", so a
// weight is the most that one signal can move a candidate in either direction.
// That bound is what makes the formula explainable: no signal can outweigh the
// retrieval layer by more than its own weight.
type Weights struct {
	Lexical      float64 `json:"lexical"`
	Type         float64 `json:"type"`
	Entity       float64 `json:"entity"`
	Operator     float64 `json:"operator"`
	Relationship float64 `json:"relationship"`
	Context      float64 `json:"context"`
	Span         float64 `json:"span"`
}

// Policy is the ranking configuration.
//
// Kept out of config.SecurityPolicy deliberately (TODO.md, carried forward):
// these are calibration numbers, and putting them beside the security limits
// would imply that loosening one is a security decision.
type Policy struct {
	// MinScore is the lowest ranking score a top-1 may have and still be
	// accepted. Below it the span is rejected as LOW_CONFIDENCE.
	MinScore float64 `json:"minScore"`
	// MinMargin is the lowest top-1 minus runner-up gap that counts as a
	// decision. Below it the span is AMBIGUOUS, however high top-1 scores.
	MinMargin float64 `json:"minMargin"`
	Weights   Weights `json:"weights"`
}

// DefaultPolicy returns the Phase 4 starting point.
//
// PROVISIONAL. The thresholds were chosen by a sweep over the Phase 4
// ambiguity set (docs/phase-4-results.md) and rounded to values a person can
// reason about; the weights are round numbers ordered by how directly each
// signal speaks to the question. The brief requires these to be calibrated on
// the evaluation dataset, and Phase 10 is where that dataset exists. Nothing
// here is defended as optimal — only as measured and reproducible.
func DefaultPolicy() Policy {
	return Policy{
		MinScore:  0.70,
		MinMargin: 0.10,
		Weights: Weights{
			// Small: an exact lexical match is already a 1.000 similarity, so
			// a large weight would count the same evidence twice.
			Lexical: 0.05,
			// Schema facts: a number next to a string field, an operator the
			// field does not allow.
			Type:     0.10,
			Operator: 0.10,
			// The sentence names the entity this field belongs to. The single
			// most useful disambiguator for "amount"-style words.
			Entity: 0.15,
			// Needing a join is weak evidence against; being unreachable is
			// stronger.
			Relationship: 0.10,
			// Count next to "highest" says rows (top N), not a scalar (max).
			Context: 0.15,
			// A longer span whose target is already carried by a shorter one
			// is the wrong reading of those extra words.
			Span: 0.20,
		},
	}
}

// Validate refuses a policy that cannot produce meaningful decisions.
func (p Policy) Validate() error {
	if math.IsNaN(p.MinScore) || math.IsNaN(p.MinMargin) {
		return qferr.New(qferr.CodeInvalidInput, "ranking thresholds must be numbers")
	}
	if p.MinScore <= 0 || p.MinScore > 2 {
		// Similarity tops out at 1 and positive evidence adds at most the sum
		// of the weights; a threshold above 2 could never be met, and one at
		// or below 0 would accept noise.
		return qferr.New(qferr.CodeInvalidInput, "minScore must be in (0, 2], got %g", p.MinScore)
	}
	if p.MinMargin < 0 || p.MinMargin > 1 {
		return qferr.New(qferr.CodeInvalidInput, "minMargin must be in [0, 1], got %g", p.MinMargin)
	}
	w := p.Weights
	for name, v := range map[string]float64{
		"lexical": w.Lexical, "type": w.Type, "entity": w.Entity, "operator": w.Operator,
		"relationship": w.Relationship, "context": w.Context, "span": w.Span,
	} {
		if math.IsNaN(v) || v < 0 || v > 1 {
			// Map iteration order only affects WHICH bad weight is named when
			// several are bad; the decision to refuse is the same.
			return qferr.New(qferr.CodeInvalidInput, "weight %s must be in [0, 1], got %g", name, v)
		}
	}
	return nil
}

// Evidence is one candidate's structural signals, each in [-1, +1].
//
// 0 means "this signal has nothing to say" — the common case. Positive
// supports the candidate, negative counts against it. The field names follow
// the ranking object in section 12 of the brief.
type Evidence struct {
	Lexical      float64 `json:"lexical"`
	Type         float64 `json:"type"`
	Entity       float64 `json:"entity"`
	Operator     float64 `json:"operator"`
	Relationship float64 `json:"relationship"`
	Context      float64 `json:"context"`
	Span         float64 `json:"span"`
}

// weighted is the total contribution of the evidence under w.
func (e Evidence) weighted(w Weights) float64 {
	return e.Lexical*w.Lexical + e.Type*w.Type + e.Entity*w.Entity +
		e.Operator*w.Operator + e.Relationship*w.Relationship +
		e.Context*w.Context + e.Span*w.Span
}

// Candidate is one interpretation of a span.
type Candidate struct {
	// Entry is the corpus entry this candidate stands for. Its ID may embed a
	// config value; render it through DisplayTarget.
	Entry corpus.Entry `json:"-"`

	// Similarity is the retrieval cosine, untouched.
	Similarity float64 `json:"similarity"`
	// Evidence is every structural signal, unweighted.
	Evidence Evidence `json:"evidence"`
	// Score is Similarity plus weighted Evidence.
	Score float64 `json:"score"`

	// Reasons explains every non-zero signal and every veto, in order. They
	// name STRUCTURE only — targets, kinds, operators, token positions — never
	// a config value or a literal from the sentence, so they are safe in
	// default output.
	Reasons []string `json:"reasons,omitempty"`
	// Vetoed marks a candidate the schema rules out outright (a field that is
	// not filterable, with an operator attached). A vetoed candidate cannot be
	// accepted and does not count as a competitor.
	Vetoed bool `json:"vetoed,omitempty"`

	matched          string
	matchedSensitive bool
}

// Target is the internal corpus ID. It may embed a config value: use it for
// comparison, never for output. DisplayTarget is the output form.
func (c Candidate) Target() string { return c.Entry.ID }

// DisplayTarget is the ID with any config value withheld unless reveal is set.
func (c Candidate) DisplayTarget(reveal bool) string {
	return corpus.DisplayID(c.Entry.ID, reveal)
}

// MatchedText is the surface that retrieval matched, redacted when it is a
// config value and reveal is false. There is deliberately no accessor that
// returns the raw text without a reveal decision.
func (c Candidate) MatchedText(reveal bool) string {
	if c.matchedSensitive && !reveal {
		return qferr.Placeholder
	}
	return c.matched
}

// String renders the candidate REDACTED. fmt consults it for %v, %+v, %s and
// %q, so formatting a candidate by accident — the obvious debugging mistake —
// cannot print the matched value or a value-bearing ID. Same principle as
// qferr.Sensitive: the safe rendering is the one you get without trying.
func (c Candidate) String() string {
	return fmt.Sprintf("%s(sim %.3f, score %.3f)", c.DisplayTarget(false), c.Similarity, c.Score)
}

// GoString covers %#v, which bypasses String.
func (c Candidate) GoString() string { return "ranking.Candidate{" + c.String() + "}" }

// MatchedSensitive reports whether the matched surface is a config value.
func (c Candidate) MatchedSensitive() bool { return c.matchedSensitive }

// SpanRanking is the ranked candidate list and the decision for one span.
type SpanRanking struct {
	// Index is the span's position in Analysis.Spans.
	Index int
	Span  nlp.Span

	// Candidates are sorted by Score, then Similarity, then Target, so that
	// identical inputs produce identical order regardless of how retrieval
	// happened to break its own ties.
	Candidates []Candidate

	Decision Decision
	// Code classifies a non-accept: NO_SEMANTIC_MATCH, LOW_CONFIDENCE or
	// AMBIGUOUS_MATCH. Empty on Accept.
	Code qferr.Code

	// Top indexes the best eligible candidate, or -1 when none is eligible.
	Top int
	// RunnerUp indexes the best eligible candidate that DISAGREES with Top,
	// or -1 when there is none. Candidates that agree with Top — the field a
	// value alias belongs to, a count pattern and the count aggregation — are
	// not competitors, so they cannot make a span ambiguous.
	RunnerUp int
	// Margin is Top.Score minus RunnerUp.Score, or Top.Score when there is no
	// runner-up. First-class: a high Top score with a small margin is
	// ambiguous, and a modest Top score with a wide margin is not.
	Margin float64
	// Confidence is how far the margin clears the bar, in [0, 1]:
	// margin / (2 × MinMargin), capped at 1, and 0 whenever the decision is
	// not Accept. At exactly the acceptance boundary it is 0.5.
	//
	// It is a statement about the DECISION. It deliberately ignores how high
	// the top score is once it clears MinScore: two fields tied at 1.000 give
	// confidence 0, and one field at 0.75 against a runner-up at 0.40 gives 1.
	Confidence float64
	// Competitors indexes the disagreeing eligible candidates within MinMargin
	// of Top, best first. Non-empty exactly when Decision is Ambiguous; these
	// are the readings the caller has to choose between.
	Competitors []int
	// SharedPhrase marks an Ambiguous decision taken by the shared-phrase rule
	// (decision 019 §5), not by the margin: the span's words are part of
	// several targets' configured surfaces and the sentence does not tell them
	// apart. Margin then still holds the score margin, which may well have
	// cleared MinMargin, so the refusal must not be reported as a margin
	// shortfall. It is the rule's reason, carried to the verdict message.
	SharedPhrase bool
}

// TopCandidate returns the accepted or leading candidate, if any.
func (s SpanRanking) TopCandidate() (Candidate, bool) {
	if s.Top < 0 || s.Top >= len(s.Candidates) {
		return Candidate{}, false
	}
	return s.Candidates[s.Top], true
}

// decide applies the threshold and margin policy to a sorted candidate list.
//
// The order of the tests is the policy:
//
//  1. no eligible candidate          → Reject, NO_SEMANTIC_MATCH
//  2. top score below MinScore       → Reject, LOW_CONFIDENCE
//  3. margin below MinMargin, or an
//     exact tie under any policy     → Ambiguous, AMBIGUOUS_MATCH
//  4. otherwise                      → Accept
//
// Score is tested before margin so that two weak candidates close together are
// reported as "nothing good enough" rather than as a choice between two bad
// readings — the second message would invite the caller to pick one.
func decide(s *SpanRanking, p Policy) {
	s.Top, s.RunnerUp = -1, -1
	s.Competitors = nil
	s.Margin, s.Confidence = 0, 0
	s.SharedPhrase = false

	for i, c := range s.Candidates {
		if !c.Vetoed {
			s.Top = i
			break
		}
	}
	if s.Top < 0 {
		s.Decision, s.Code = Reject, qferr.CodeNoSemanticMatch
		return
	}
	top := s.Candidates[s.Top]

	for i := s.Top + 1; i < len(s.Candidates); i++ {
		c := s.Candidates[i]
		if c.Vetoed || agrees(top.Entry, c.Entry) {
			continue
		}
		if s.RunnerUp < 0 {
			s.RunnerUp = i
		}
		if top.Score-c.Score+scoreEpsilon < p.MinMargin || top.Score-c.Score <= scoreEpsilon {
			s.Competitors = append(s.Competitors, i)
		}
	}

	if s.RunnerUp >= 0 {
		s.Margin = top.Score - s.Candidates[s.RunnerUp].Score
	} else {
		// Alone in the field: the margin is against nothing at all. Rare with
		// a top-k of ten from a full corpus, but it happens when every rival
		// is vetoed.
		s.Margin = top.Score
	}

	switch {
	case top.Score+scoreEpsilon < p.MinScore:
		s.Decision, s.Code = Reject, qferr.CodeLowConfidence
		s.Competitors = nil
	case s.Margin+scoreEpsilon < p.MinMargin, s.RunnerUp >= 0 && s.Margin <= scoreEpsilon:
		// The second clause is the tie invariant, and it holds under ANY
		// policy: even with MinMargin 0 — the "threshold only" configuration
		// the calibration sweep measures — two readings tied on score are
		// never resolved by which one sorts first.
		s.Decision, s.Code = Ambiguous, qferr.CodeAmbiguousMatch
	default:
		s.Decision, s.Code = Accept, ""
		s.Competitors = nil
		if p.MinMargin <= 0 {
			s.Confidence = 1
		} else {
			s.Confidence = math.Min(1, s.Margin/(2*p.MinMargin))
		}
	}
}

// sortCandidates orders by Score, then Similarity, then Target.
//
// The final tie-break is by ID and is arbitrary on purpose: it decides only
// the ORDER two tied candidates are listed in, never which is accepted — two
// candidates tied on score have a margin of zero, and a zero margin is never
// accepted. Phase 2's tie-break by ID decided the Top-1 outright; this one
// cannot.
func sortCandidates(cs []Candidate) {
	// Insertion sort: at most top-k (default 10) candidates per span, and it
	// is obviously stable.
	for i := 1; i < len(cs); i++ {
		for j := i; j > 0 && candidateLess(cs[j], cs[j-1]); j-- {
			cs[j], cs[j-1] = cs[j-1], cs[j]
		}
	}
}

func candidateLess(a, b Candidate) bool {
	if math.Abs(a.Score-b.Score) > scoreEpsilon {
		return a.Score > b.Score
	}
	if math.Abs(a.Similarity-b.Similarity) > scoreEpsilon {
		return a.Similarity > b.Similarity
	}
	return a.Entry.ID < b.Entry.ID
}

// agrees reports whether two entries are the same reading, so that one must
// not count as the other's competitor.
//
// Deliberately narrow — every rule here is a place where ambiguity is
// suppressed, so each needs a reason:
//
//   - the same entry;
//   - a value and the field it belongs to: "India" retrieving both the India
//     alias and customer.country is one reading, stated twice;
//   - two values of the same field resolving to the same stored value: the
//     alias "disabled" and the enum value "suspended" both mean suspended;
//   - a count-intent pattern and the count aggregation: "how many" configures
//     both, and they compile to the same COUNT.
//
// Not included: top_n versus max. "highest" configures both, and they mean
// different things (N rows versus one scalar). That pair is the canonical
// ambiguity, not an agreement.
func agrees(a, b corpus.Entry) bool {
	if a.ID == b.ID {
		return true
	}
	if a.Kind == corpus.KindValueAlias && b.Kind == corpus.KindField {
		return a.Entity == b.Entity && a.Ref == b.Ref
	}
	if b.Kind == corpus.KindValueAlias && a.Kind == corpus.KindField {
		return a.Entity == b.Entity && a.Ref == b.Ref
	}
	if a.Kind == corpus.KindValueAlias && b.Kind == corpus.KindValueAlias {
		return a.Entity == b.Entity && a.Ref == b.Ref && a.Meta.ValueTarget == b.Meta.ValueTarget
	}
	if isCountReading(a) && isCountReading(b) {
		return true
	}
	return false
}

func isCountReading(e corpus.Entry) bool {
	switch e.Kind {
	case corpus.KindAggregation:
		return config.Aggregation(e.Ref) == config.AggCount
	case corpus.KindQueryPattern:
		return e.Meta.Intent == config.IntentCount
	}
	return false
}

// reasonf formats a reason. Only structural arguments may be passed; see
// Candidate.Reasons.
func reasonf(format string, args ...any) string { return fmt.Sprintf(format, args...) }
