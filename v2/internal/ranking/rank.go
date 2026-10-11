package ranking

import (
	"sort"
	"strings"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/retrieval"
)

// Input is everything ranking needs for one sentence.
//
// Retrieval results come IN rather than being fetched here, so the whole
// package runs without a model: tests construct matches with chosen scores and
// assert on decisions, which is what makes the threshold and margin behaviour
// testable to the decimal.
type Input struct {
	Analysis nlp.Analysis
	// Retrieved[i] holds retrieval's matches for Analysis.Spans[i], best
	// first, as retrieval.Index.Search returns them. Typed spans (numbers and
	// dates) are not embedded and their entry is ignored.
	Retrieved [][]retrieval.Match
	Registry  *config.SchemaRegistry
}

// Result is the ranking of every text span plus the chosen reading.
type Result struct {
	Policy Policy
	// Spans holds one ranking per TEXT span, in Analysis.Spans order.
	Spans []SpanRanking
	// Cover indexes Spans: the non-overlapping reading, left to right. Only
	// accepted and ambiguous spans take part; see chooseCover.
	Cover []int
	// Verdict is the sentence-level code: "" when every covered span was
	// accepted, AMBIGUOUS_MATCH when any covered span is ambiguous, and
	// NO_SEMANTIC_MATCH when nothing was accepted at all.
	Verdict qferr.Code
	// Unexplained are the positions of content tokens that no covered span
	// and no number or date accounts for. Reported, not judged: whether an
	// unexplained word matters is a binding question (Phase 5).
	Unexplained []int
}

// Rank scores every candidate of every text span and decides each span.
//
// TWO PASSES, NEVER MORE. Context evidence ("this sentence mentions orders")
// has to come from somewhere, and taking it from the final decisions would be
// circular. Pass 1 decides every span on LOCAL evidence only — similarity,
// lexical match, span quality. The spans pass 1 accepts become the context for
// pass 2, which adds type, entity, operator, relationship and context
// evidence. The number of passes is fixed rather than iterated to a fixpoint,
// because a fixpoint's result can depend on evaluation order, and because
// "context is what the sentence says on its own" is a rule a person can check.
func Rank(in Input, p Policy) (Result, error) {
	if err := p.Validate(); err != nil {
		return Result{}, err
	}
	if in.Registry == nil {
		return Result{}, qferr.New(qferr.CodeInternal, "ranking needs a schema registry")
	}
	if len(in.Retrieved) != len(in.Analysis.Spans) {
		return Result{}, qferr.New(qferr.CodeInternal,
			"ranking got retrieval results for %d spans, the analysis has %d",
			len(in.Retrieved), len(in.Analysis.Spans))
	}

	r := newRanker(in, p)

	// Pass 1: local evidence only.
	for i := range r.spans {
		r.scoreLocal(&r.spans[i])
		sortCandidates(r.spans[i].Candidates)
		decide(&r.spans[i], p)
	}

	// Context from what pass 1 accepted.
	r.collectContext()

	// Pass 2: add structural evidence and decide again.
	for i := range r.spans {
		r.scoreStructural(&r.spans[i])
		sortCandidates(r.spans[i].Candidates)
		decide(&r.spans[i], p)
		r.fragmentAmbiguity(&r.spans[i])
	}

	res := Result{Policy: p, Spans: r.spans}
	res.Cover = chooseCover(res.Spans)
	res.Verdict = verdict(res.Spans, res.Cover)
	res.Unexplained = unexplained(in.Analysis, res.Spans, res.Cover)
	return res, nil
}

// ranker holds per-sentence working state. Built and discarded per call: the
// package has no shared mutable state, so Rank is safe to call concurrently
// against one registry (section 22, rule 9).
type ranker struct {
	in    Input
	p     Policy
	spans []SpanRanking

	// rawTop is, per (start,end) text span, every entry TIED at retrieval's
	// top similarity, with that similarity. Span quality compares against what
	// a shorter span retrieves on its own.
	//
	// All tied entries, not retrieval's first one: retrieval breaks ties by
	// entry id, and an id-order accident must not decide anything here. The
	// first version kept only matches[0]; on "what is the highest" it marked
	// aggregation:max as carried by "highest" but not pattern:top_n, which is
	// tied with it at 1.000 — and top_n was accepted by a 0.200 margin that
	// existed only because "aggregation" sorts before "pattern".
	rawTop map[tokenRange]rawTopSet

	// Pass-1 context.
	entities  []anchor // entities the sentence establishes
	operators []anchor // operator phrases the sentence establishes
	accepted  []tokenRange

	// graph is entity adjacency via configured relationships, for join-distance.
	graph map[string][]string

	// entries is the whole corpus, built on first use by fragmentAmbiguity:
	// a word shared with a target retrieval did not return is still shared.
	entries []corpus.Entry
}

type tokenRange struct{ start, end int }

type rawTopSet struct {
	score float64
	ids   map[string]bool
}

func (a tokenRange) overlaps(b tokenRange) bool { return a.start < b.end && b.start < a.end }

// anchor is something pass 1 accepted, with where it was said.
type anchor struct {
	at     tokenRange
	entity string
	op     config.Operator
}

func newRanker(in Input, p Policy) *ranker {
	r := &ranker{in: in, p: p, rawTop: map[tokenRange]rawTopSet{}}

	for i, s := range in.Analysis.Spans {
		if s.Kind != nlp.SpanText {
			continue
		}
		matches := in.Retrieved[i]
		if len(matches) > 0 {
			best := matches[0].Score
			for _, m := range matches[1:] {
				// Retrieval sorts, but ranking does not rely on it.
				if m.Score > best {
					best = m.Score
				}
			}
			set := rawTopSet{score: best, ids: map[string]bool{}}
			for _, m := range matches {
				if best-m.Score <= scoreEpsilon {
					set.ids[m.Entry.ID] = true
				}
			}
			r.rawTop[tokenRange{s.Start, s.End}] = set
		}

		sr := SpanRanking{Index: i, Span: s, Top: -1, RunnerUp: -1}
		for _, m := range matches {
			text, sensitive := m.MatchedText()
			sr.Candidates = append(sr.Candidates, Candidate{
				Entry:            m.Entry,
				Similarity:       m.Score,
				Score:            m.Score,
				matched:          text,
				matchedSensitive: sensitive,
			})
		}
		r.spans = append(r.spans, sr)
	}

	r.graph = map[string][]string{}
	for _, e := range in.Registry.Entities() {
		for _, rel := range in.Registry.RelationshipsTouching(e.Name) {
			other := rel.To.Entity
			if other == e.Name {
				other = rel.From.Entity
			}
			r.graph[e.Name] = append(r.graph[e.Name], other)
		}
		// Sorted so BFS order — and therefore nothing at all — depends on the
		// registry's internal ordering.
		sort.Strings(r.graph[e.Name])
	}
	return r
}

// ---------------------------------------------------------------------------
// Pass 1: local evidence
// ---------------------------------------------------------------------------

func (r *ranker) scoreLocal(s *SpanRanking) {
	for i := range s.Candidates {
		c := &s.Candidates[i]
		c.Evidence = Evidence{}
		c.Reasons = nil
		c.Vetoed = false

		// A field that can be neither selected nor filtered is a join key: it
		// exists so relationships can be declared, and no phrase in a query can
		// mean it. Found when order.customer_id ("The customer who placed the
		// order") competed with entity:customer for the word "customers".
		if c.Entry.Kind == corpus.KindField && !c.Entry.Meta.Selectable && !c.Entry.Meta.Filterable {
			c.Vetoed = true
			c.Reasons = append(c.Reasons, "veto: the field is neither selectable nor filterable, so no phrase can refer to it")
		}

		c.Evidence.Lexical = lexicalScore(s.Span.Norm, c.matched)
		switch c.Evidence.Lexical {
		case 1:
			c.Reasons = append(c.Reasons, "lexical: span is exactly the matched surface")
		case 0.5:
			c.Reasons = append(c.Reasons, "lexical: every word of the matched surface appears in the span")
		}

		// A VALUE needs literal evidence (Phase 7 gate, decision 019). A value
		// entry's one surface is a configured alias or declared enum value, so
		// the span must contain it, after the same light folding as every
		// lexical test (case, underscores, one plural s). Similarity alone
		// never authorises a value: "living" is near the alias "live" to the
		// model, and binding it silently added status = 'active' to
		// "customers living in India" (finding V02). Retrieval still DISCOVERS
		// value candidates; it cannot choose one. A vetoed value can be neither
		// the reading nor a competitor — a value the sentence does not contain
		// cannot be what it meant.
		if c.Entry.Kind == corpus.KindValueAlias && c.Evidence.Lexical == 0 {
			c.Vetoed = true
			c.Reasons = append(c.Reasons, "veto: a value is accepted only when the sentence contains it literally (a configured value or alias); similarity alone is not evidence of a value")
		}

		// Span quality. Every rule here exempts a span that is EXACTLY a
		// configured surface: "in the last" starts with a function word and
		// "order amount" is two words, but the config author declared both,
		// and that outranks any shape heuristic.
		if c.Evidence.Lexical < 1 {
			if sub, ok := r.carriedByShorterSpan(s.Span, *c); ok {
				c.Evidence.Span = -1
				c.Reasons = append(c.Reasons, reasonf(
					"span: target is already carried by the shorter span tokens[%d:%d]", sub.start, sub.end))
			}
			if r.functionWordBoundary(s.Span) {
				c.Evidence.Span = -1
				c.Reasons = append(c.Reasons, "span: begins or ends on a function word, which adds nothing but noise")
			}
			if r.swallowsTypedSpan(s.Span) {
				c.Evidence.Span = -1
				c.Reasons = append(c.Reasons, "span: contains a number or date, which is a value and has its own span")
			}
		}

		c.Score = c.Similarity + c.Evidence.weighted(r.p.Weights)
	}
}

// carriedByShorterSpan reports a strict sub-span whose own top retrieval —
// including every entry tied at the top — is this candidate's target, at least
// as strongly.
//
// This is the cover-selection signal Phase 3 deferred. "placed an order"
// retrieves entity:order at 0.783, but "order" on its own retrieves the same
// entity more strongly — so the extra words "placed an" contribute nothing to
// that reading, and the longer span is the wrong place to read it. Without this,
// long spans win covers by swallowing the words that carry other targets.
//
// Exempt: a span that is EXACTLY a configured surface. "order amount" is a
// declared synonym of order.amount; the author has said the two words belong
// together, and that outranks the arithmetic.
func (r *ranker) carriedByShorterSpan(span nlp.Span, c Candidate) (tokenRange, bool) {
	if span.End-span.Start < 2 {
		return tokenRange{}, false
	}
	// Deterministic scan: shortest sub-spans first, then left to right.
	for length := 1; length < span.End-span.Start; length++ {
		for start := span.Start; start+length <= span.End; start++ {
			sub := tokenRange{start, start + length}
			top, ok := r.rawTop[sub]
			if !ok {
				continue
			}
			if top.ids[c.Entry.ID] && top.score+scoreEpsilon >= c.Similarity {
				return sub, true
			}
		}
	}
	return tokenRange{}, false
}

// functionWordBoundary reports a multi-token span whose first or last token is
// a function word. "orders with" and "customers who placed an" are Phase 3
// n-grams, kept because segmentation is not Phase 3's job; they are never a
// better reading than the same span without the function word, and long spans
// match long descriptions ("The customer who placed the order") well enough to
// be accepted on similarity alone. Found on the first real run.
func (r *ranker) functionWordBoundary(span nlp.Span) bool {
	toks := r.in.Analysis.Tokens
	if span.End-span.Start < 2 || span.End > len(toks) {
		return false
	}
	return toks[span.Start].Stop || toks[span.End-1].Stop
}

// swallowsTypedSpan reports a text span that contains a number or date. The
// value already has a typed span; a text span around it embeds the literal
// ("with amount over 100") and competes with the phrase that carries the
// field. Phase 3 kept these deliberately so the surrounding words stay
// visible; Phase 4 is where they are marked as the weaker reading.
func (r *ranker) swallowsTypedSpan(span nlp.Span) bool {
	self := tokenRange{span.Start, span.End}
	for _, s := range r.in.Analysis.Spans {
		if s.Kind != nlp.SpanNumber && s.Kind != nlp.SpanDate {
			continue
		}
		if (tokenRange{s.Start, s.End}).overlaps(self) {
			return true
		}
	}
	return false
}

// lexicalScore compares a span with the surface retrieval matched.
//
//	1    the same words, after light folding (case, underscores, a plural s)
//	0.5  every SURFACE word appears in the span: the config's phrase is fully
//	     present, with extra words around it ("total spent amount")
//	0    otherwise
//
// The direction of the partial case matters. The first version gave 0.5 when
// every SPAN word appeared in the surface — "value" inside "purchase value" —
// and that pushed commerce's "value" over the margin to total_spent (raw
// margin 0.094, ranked 0.119). But a lone word found inside a longer synonym
// has left the synonym's qualifier ("purchase") unconfirmed: the surface is
// MORE specific than what was said. Only a surface that is wholly present is
// evidence.
//
// Never negative: a paraphrase sharing no word with the surface ("money spent"
// for total_spent) is exactly what the embedding layer exists to bridge, and
// penalising it would undo Phase 2.
func lexicalScore(spanNorm, surface string) float64 {
	a, b := foldWords(spanNorm), foldWords(surface)
	if len(a) == 0 || len(b) == 0 {
		return 0
	}
	if strings.Join(a, " ") == strings.Join(b, " ") {
		return 1
	}
	have := make(map[string]bool, len(a))
	for _, w := range a {
		have[w] = true
	}
	for _, w := range b {
		if !have[w] {
			return 0
		}
	}
	return 0.5
}

// foldWords lower-cases, splits identifiers, and strips one plural s.
//
// Deliberately crude: it is applied identically to both sides, so "status"
// folding to "statu" is harmless — it folds that way everywhere. Real
// lemmatisation is deferred until a measured failure asks for it (decision 009).
func foldWords(s string) []string {
	s = strings.ToLower(s)
	s = strings.NewReplacer("_", " ", "-", " ").Replace(s)
	words := strings.Fields(s)
	for i, w := range words {
		if len(w) > 3 && strings.HasSuffix(w, "s") && !strings.HasSuffix(w, "ss") {
			words[i] = w[:len(w)-1]
		}
	}
	return words
}

// ---------------------------------------------------------------------------
// Context between the passes
// ---------------------------------------------------------------------------

// collectContext records what pass 1 established.
//
// Entities come from entity, field and value readings — a field or a value
// establishes its entity as surely as naming it does ("customers from India":
// the India alias puts customer in play). Operators come from accepted
// operator spans.
//
// Two rules keep context CONSERVATIVE, both found by the calibration sweep,
// where raising minMargin from 0.10 to 0.15 introduced a silent wrong answer
// on "orders and payments with amount over 5" — a stricter policy guessing
// more, which should be impossible:
//
//  1. Only CLEAN spans contribute. A span pass 1 accepted despite a
//     span-quality defect ("payments with amount") is a reading the ranker
//     already distrusts; it must not vouch for anything else.
//  2. An ambiguous span contributes every entity among its live readings, not
//     none. At minMargin 0.15, "orders" was ambiguous (entity vs relationship)
//     and contributed nothing, while "payments" was accepted — so "amount"
//     saw evidence for payment and none for order, and picked payment.amount.
//     Context has to be symmetric: whatever might have been named, counts.
//
// Rejected spans contribute nothing: below MinScore is not a mention.
func (r *ranker) collectContext() {
	r.entities, r.operators, r.accepted = nil, nil, nil
	for _, s := range r.spans {
		if s.Decision == Reject || spanDefect(s) {
			continue
		}
		at := tokenRange{s.Span.Start, s.Span.End}
		live := []int{s.Top}
		if s.Decision == Accept {
			r.accepted = append(r.accepted, at)
		} else {
			live = append(live, s.Competitors...)
		}
		for _, k := range live {
			e := s.Candidates[k].Entry
			switch e.Kind {
			case corpus.KindEntity, corpus.KindField, corpus.KindValueAlias:
				r.entities = append(r.entities, anchor{at: at, entity: e.Entity})
			case corpus.KindOperator:
				// An operator is attached only when it was accepted: an
				// ambiguous "over"/"under" must not type-check anything.
				if s.Decision == Accept {
					r.operators = append(r.operators, anchor{at: at, op: config.Operator(e.Ref)})
				}
			}
		}
	}
}

// contextEntities returns the entities established by spans that do NOT
// overlap at. A span cannot be its own context: "order amount" must not count
// its own "order" as evidence for order.amount a second time — the similarity
// already did.
func (r *ranker) contextEntities(at tokenRange) map[string]bool {
	out := map[string]bool{}
	for _, a := range r.entities {
		if !a.at.overlaps(at) {
			out[a.entity] = true
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Pass 2: structural evidence
// ---------------------------------------------------------------------------

// attachment is what follows a span within reach: the operator and the value
// a filter phrase would bind to.
type attachment struct {
	op        config.Operator
	hasOp     bool
	number    *nlp.Number
	date      *nlp.DateExpr
	opAt      tokenRange
	valueAt   tokenRange
	hasValue  bool
	countNear bool // an integer count adjacent to the span (either side)
	dateNear  bool // a date expression adjacent to or inside the span
}

// attachWindow is how far to the right an operator or value may sit and still
// attach to a span. Four tokens covers "amount of more than 100" and similar;
// beyond that, attachment guesses. Not a tuned number: it bounds a heuristic,
// and a Phase 5 binder replaces the heuristic with structure.
const attachWindow = 4

func (r *ranker) attachmentFor(span nlp.Span) attachment {
	var a attachment
	toks := r.in.Analysis.Tokens
	self := tokenRange{span.Start, span.End}

	typedAt := map[int]nlp.Span{}
	for _, s := range r.in.Analysis.Spans {
		if s.Kind == nlp.SpanNumber || s.Kind == nlp.SpanDate {
			typedAt[s.Start] = s
		}
	}
	opAt := map[int]anchor{}
	for _, o := range r.operators {
		if !o.at.overlaps(self) {
			opAt[o.at.start] = o
		}
	}
	subjectAt := map[int]bool{}
	for _, e := range r.entities {
		if !e.at.overlaps(self) {
			subjectAt[e.at.start] = true
		}
	}

	// Scan right. Stops at punctuation, a connective, or another subject —
	// "amount and price over 5" must not hand the 5 to amount.
	for j := span.End; j < len(toks) && j < span.End+attachWindow; {
		t := toks[j]
		if t.Kind == nlp.KindPunct || isConnective(r.in.Analysis, j) {
			break
		}
		if ts, ok := typedAt[j]; ok {
			a.hasValue = true
			a.valueAt = tokenRange{ts.Start, ts.End}
			a.number, a.date = ts.Number, ts.Date
			break
		}
		if o, ok := opAt[j]; ok && !a.hasOp {
			a.hasOp, a.op, a.opAt = true, o.op, o.at
			j = o.at.end
			continue
		}
		if subjectAt[j] {
			break
		}
		j++
	}

	// Adjacency, either side, skipping function words: "top 5", "5 highest",
	// "the 5 highest".
	a.countNear = isCount(r.neighbour(span, -1, typedAt)) || isCount(r.neighbour(span, +1, typedAt))
	for _, d := range r.in.Analysis.Spans {
		if d.Kind != nlp.SpanDate {
			continue
		}
		dr := tokenRange{d.Start, d.End}
		if dr.overlaps(self) || d.End == span.Start || d.Start == span.End {
			a.dateNear = true
		}
	}
	return a
}

// neighbour returns the typed span directly beside span in direction dir,
// skipping up to two function words, or nil.
func (r *ranker) neighbour(span nlp.Span, dir int, typedAt map[int]nlp.Span) *nlp.Span {
	toks := r.in.Analysis.Tokens
	// Looking left, a typed span is found by where it ENDS; looking right, by
	// where it starts. Both are direct lookups, so no map is ever iterated.
	typedEnd := map[int]nlp.Span{}
	for _, ts := range typedAt {
		typedEnd[ts.End-1] = ts
	}
	j := span.End
	if dir < 0 {
		j = span.Start - 1
	}
	for skipped := 0; j >= 0 && j < len(toks); skipped++ {
		lookup := typedAt
		if dir < 0 {
			lookup = typedEnd
		}
		if ts, ok := lookup[j]; ok {
			return &ts
		}
		if !toks[j].Stop || skipped >= 2 {
			return nil
		}
		j += dir
	}
	return nil
}

// isCount reports a number that reads as "how many rows": a positive whole
// number with no currency or percent, within int64 (a row count or LIMIT is
// never larger). "top 5" is a count; "$5" and "5%" are not. Exact since the
// Phase 8 gate (decision 022 §2); for every integer up to int64's range this
// is the decision the float64 test made.
func isCount(s *nlp.Span) bool {
	if s == nil || s.Kind != nlp.SpanNumber || s.Number == nil {
		return false
	}
	n := s.Number
	_, fits := n.Value.Int64()
	return !n.Malformed && fits && n.Value.Sign() > 0 && n.Currency == "" && !n.Percent
}

func isConnective(a nlp.Analysis, tok int) bool {
	for _, c := range a.Connectives {
		if c.Start <= tok && tok < c.End {
			return true
		}
	}
	return false
}

func (r *ranker) scoreStructural(s *SpanRanking) {
	at := tokenRange{s.Span.Start, s.Span.End}
	ctx := r.contextEntities(at)
	att := r.attachmentFor(s.Span)

	decomposed, byRange := r.acceptedSubSpan(at)

	for i := range s.Candidates {
		c := &s.Candidates[i]
		e := c.Entry

		// The pass-2 half of span quality: the words of this span are already
		// read, one piece at a time, by shorter spans pass 1 accepted. A long
		// non-exact reading then competes with a composition of confident
		// short ones, and it is the long one that is guessing.
		if decomposed && c.Evidence.Lexical < 1 && c.Evidence.Span > -1 {
			c.Evidence.Span = -1
			c.Reasons = append(c.Reasons, reasonf(
				"span: the shorter span tokens[%d:%d] was already accepted on its own", byRange.start, byRange.end))
		}

		switch e.Kind {
		case corpus.KindEntity:
			// Symmetric with fields and values. Context must never shift the
			// balance between an entity and its own fields: on the span
			// "payments", naming payment elsewhere is evidence for
			// payment.amount exactly as much as for payment itself.
			r.entityEvidence(c, e.Entity, ctx)
		case corpus.KindField:
			r.fieldEvidence(c, att, ctx)
		case corpus.KindValueAlias:
			r.entityEvidence(c, e.Entity, ctx)
		case corpus.KindOperator:
			operatorEvidence(c, att)
		case corpus.KindAggregation:
			if att.countNear {
				c.Evidence.Context = -1
				c.Reasons = append(c.Reasons,
					"context: a count sits next to this phrase, which asks for N rows, not one aggregate")
			}
		case corpus.KindQueryPattern:
			patternEvidence(c, att)
		case corpus.KindRelationship:
			r.relationshipEvidence(c, ctx)
		}

		c.Score = c.Similarity + c.Evidence.weighted(r.p.Weights)
	}
}

// acceptedSubSpan reports a strict sub-span of at that pass 1 accepted.
func (r *ranker) acceptedSubSpan(at tokenRange) (bool, tokenRange) {
	if at.end-at.start < 2 {
		return false, tokenRange{}
	}
	for _, a := range r.accepted {
		if a.start >= at.start && a.end <= at.end && a != at {
			return true, a
		}
	}
	return false, tokenRange{}
}

func (r *ranker) fieldEvidence(c *Candidate, att attachment, ctx map[string]bool) {
	m := c.Entry.Meta

	// Type: does the attached value fit the field?
	if att.hasValue {
		switch {
		case att.number != nil && m.FieldType == config.TypeNumber,
			att.date != nil && m.FieldType == config.TypeDate:
			c.Evidence.Type = 1
			c.Reasons = append(c.Reasons, reasonf("type: attached %s value fits a %s field", valueKind(att), m.FieldType))
		default:
			c.Evidence.Type = -1
			c.Reasons = append(c.Reasons, reasonf("type: attached %s value does not fit a %s field", valueKind(att), m.FieldType))
		}
	}

	// Operator: is the attached operator one this field allows?
	if att.hasOp {
		if !m.Filterable {
			c.Vetoed = true
			c.Reasons = append(c.Reasons, reasonf("veto: an operator (%s) is attached but the field is not filterable", att.op))
		} else if containsOp(m.Operators, att.op) {
			c.Evidence.Operator = 1
			c.Reasons = append(c.Reasons, reasonf("operator: %s is allowed on this field", att.op))
		} else {
			c.Evidence.Operator = -1
			c.Reasons = append(c.Reasons, reasonf("operator: %s is not allowed on this field", att.op))
		}
	}

	r.entityEvidence(c, c.Entry.Entity, ctx)
}

// entityEvidence scores a candidate against the entities the rest of the
// sentence establishes.
//
//	entity        +1  its own entity is named elsewhere in the sentence
//	relationship  -0.5 only reachable from the named entities through a join
//	relationship  -1  not reachable at all within the join limit
//
// Nothing when the sentence names no entity: absence of context is not
// evidence against anything.
func (r *ranker) entityEvidence(c *Candidate, own string, ctx map[string]bool) {
	if len(ctx) == 0 || own == "" {
		return
	}
	if ctx[own] {
		c.Evidence.Entity = 1
		c.Reasons = append(c.Reasons, reasonf("entity: the sentence names %s", own))
		return
	}
	if c.Entry.Kind == corpus.KindEntity {
		// Naming a second entity is how a sentence ASKS for a join, so an
		// entity mention is never evidence against itself. Only fields and
		// values — attributes that must hang off something already named —
		// pay for distance.
		return
	}
	hops := r.joinDistance(own, ctx)
	if hops < 0 {
		c.Evidence.Relationship = -1
		c.Reasons = append(c.Reasons, reasonf("relationship: %s is unreachable from the entities the sentence names", own))
		return
	}
	c.Evidence.Relationship = -0.5
	c.Reasons = append(c.Reasons, reasonf("relationship: %s is %d join(s) from the entities the sentence names", own, hops))
}

// joinDistance is the fewest relationship hops from any context entity to
// target, or -1 if more than the configured join limit.
func (r *ranker) joinDistance(target string, ctx map[string]bool) int {
	limit := r.in.Registry.Security().MaxJoins
	var frontier []string
	seen := map[string]bool{}
	for e := range ctx {
		frontier = append(frontier, e)
		seen[e] = true
	}
	sort.Strings(frontier)
	for hops := 1; hops <= limit && len(frontier) > 0; hops++ {
		var next []string
		for _, e := range frontier {
			for _, n := range r.graph[e] {
				if n == target {
					return hops
				}
				if !seen[n] {
					seen[n] = true
					next = append(next, n)
				}
			}
		}
		frontier = next
	}
	return -1
}

func operatorEvidence(c *Candidate, att attachment) {
	if !att.hasValue {
		return
	}
	var t config.FieldType
	switch {
	case att.number != nil:
		t = config.TypeNumber
	case att.date != nil:
		t = config.TypeDate
	default:
		return
	}
	op := config.Operator(c.Entry.Ref)
	if config.OperatorAllowed(t, op) {
		c.Evidence.Operator = 1
		c.Reasons = append(c.Reasons, reasonf("operator: %s applies to the attached %s value", op, t))
	} else {
		c.Evidence.Operator = -1
		c.Reasons = append(c.Reasons, reasonf("operator: %s cannot apply to the attached %s value", op, t))
	}
}

func patternEvidence(c *Candidate, att attachment) {
	switch c.Entry.Meta.Intent {
	case config.IntentTopN:
		if att.countNear {
			c.Evidence.Context = 1
			c.Reasons = append(c.Reasons, "context: a count sits next to this phrase, as a top-N pattern expects")
		}
	case config.IntentTimeWindow:
		if att.dateNear {
			c.Evidence.Context = 1
			c.Reasons = append(c.Reasons, "context: a date expression sits next to this phrase")
		}
	}
}

// relationshipEvidence: a traversal only makes sense between entities the
// sentence is about. Phase 2's finding was that relationships lose to entities
// on raw similarity; this is the signal that was missing.
//
//	+1  the sentence names BOTH ends elsewhere ("customers who placed an order")
//	 0  it names one end — too weak: any sentence about customers would then
//	    boost every customer relationship, including on the very word that
//	    names the other end ("order" vs customer_orders; found on the first run)
//	-1  it names entities, but neither end
func (r *ranker) relationshipEvidence(c *Candidate, ctx map[string]bool) {
	if len(ctx) == 0 {
		return
	}
	rel, ok := r.in.Registry.Relationship(c.Entry.Ref)
	if !ok {
		return
	}
	from, to := ctx[rel.From.Entity], ctx[rel.To.Entity]
	switch {
	case from && to:
		c.Evidence.Relationship = 1
		c.Reasons = append(c.Reasons, "relationship: the sentence names both ends of this traversal")
	case !from && !to:
		c.Evidence.Relationship = -1
		c.Reasons = append(c.Reasons, "relationship: the sentence names neither end of this traversal")
	}
}

func containsOp(ops []config.Operator, op config.Operator) bool {
	for _, o := range ops {
		if o == op {
			return true
		}
	}
	return false
}

func valueKind(a attachment) string {
	if a.date != nil {
		return "date"
	}
	return "number"
}

// ---------------------------------------------------------------------------
// Cover and verdict
// ---------------------------------------------------------------------------

// chooseCover picks a non-overlapping reading of the sentence.
//
// Candidates for the cover are taken in this order:
//
//  1. clean accepted spans     (by confidence, then length, then position)
//  2. clean ambiguous spans    (strongest leading reading, then shortest)
//  3. defective accepted spans (a span-quality penalty on the leading reading)
//  4. defective ambiguous spans
//
// Span quality is the PRIMARY key. A penalised span can still clear its own
// margin, because its rivals carry the same penalty. When accepted spans came
// first regardless, "orders and payments with amount over 5" was read as
// payment.amount through the defective span "payments with amount", which
// swallowed the entity word and resolved an ambiguity the sentence really
// has. A defective reading may fill tokens nothing clean explains; it may never
// overrule a clean one, and that includes a clean "ambiguous".
//
// "order amount" accepted means the "amount" inside it is not ambiguous,
// however the lone word would have ranked: overlap is by tokens. Rejected
// spans never enter the cover; their tokens stay unexplained.
func chooseCover(spans []SpanRanking) []int {
	order := make([]int, 0, len(spans))
	for i, s := range spans {
		if s.Decision != Reject {
			order = append(order, i)
		}
	}
	sort.SliceStable(order, func(x, y int) bool {
		a, b := spans[order[x]], spans[order[y]]
		if da, db := spanDefect(a), spanDefect(b); da != db {
			return !da
		}
		if (a.Decision == Accept) != (b.Decision == Accept) {
			return a.Decision == Accept
		}
		if a.Decision == Accept {
			if a.Confidence != b.Confidence {
				return a.Confidence > b.Confidence
			}
			if la, lb := a.Span.End-a.Span.Start, b.Span.End-b.Span.Start; la != lb {
				return la > lb
			}
		} else {
			// Among ambiguous spans, report the one whose leading readings are
			// strongest, and the shortest such span: the minimal phrase that
			// shows the ambiguity is the one a user can act on.
			ta, tb := a.Candidates[a.Top].Score, b.Candidates[b.Top].Score
			if ta != tb {
				return ta > tb
			}
			if la, lb := a.Span.End-a.Span.Start, b.Span.End-b.Span.Start; la != lb {
				return la < lb
			}
		}
		return a.Span.Start < b.Span.Start
	})

	var taken []tokenRange
	var cover []int
	for _, i := range order {
		at := tokenRange{spans[i].Span.Start, spans[i].Span.End}
		clash := false
		for _, t := range taken {
			if t.overlaps(at) {
				clash = true
				break
			}
		}
		if clash {
			continue
		}
		taken = append(taken, at)
		cover = append(cover, i)
	}
	sort.Slice(cover, func(x, y int) bool { return spans[cover[x]].Span.Start < spans[cover[y]].Span.Start })
	return cover
}

// spanDefect reports whether the leading reading carries a span-quality
// penalty.
func spanDefect(s SpanRanking) bool {
	c, ok := s.TopCandidate()
	return ok && c.Evidence.Span < 0
}

func verdict(spans []SpanRanking, cover []int) qferr.Code {
	accepted := false
	for _, i := range cover {
		switch spans[i].Decision {
		case Ambiguous:
			return qferr.CodeAmbiguousMatch
		case Accept:
			accepted = true
		}
	}
	if !accepted {
		return qferr.CodeNoSemanticMatch
	}
	return ""
}

func unexplained(a nlp.Analysis, spans []SpanRanking, cover []int) []int {
	covered := make([]bool, len(a.Tokens))
	for _, i := range cover {
		for t := spans[i].Span.Start; t < spans[i].Span.End && t < len(covered); t++ {
			covered[t] = true
		}
	}
	for _, s := range a.Spans {
		if s.Kind == nlp.SpanNumber || s.Kind == nlp.SpanDate {
			for t := s.Start; t < s.End && t < len(covered); t++ {
				covered[t] = true
			}
		}
	}
	for _, c := range a.Connectives {
		for t := c.Start; t < c.End && t < len(covered); t++ {
			covered[t] = true
		}
	}
	var out []int
	for i, t := range a.Tokens {
		if !covered[i] && t.Kind == nlp.KindWord && !t.Stop {
			out = append(out, i)
		}
	}
	return out
}

// fragmentAmbiguity is decision 019 §5, the shared-phrase rule (Phase 7
// findings X03 and X02).
//
// THE PRINCIPLE: if a generic phrase is shared by several targets and the
// sentence gives no explicit evidence that tells them apart, refuse.
//
//   - GENERIC and SHARED: the accepted reading has no lexical evidence, and the
//     span's words are only a strict FRAGMENT of its configured name, synonym
//     or phrase, and also a fragment of another non-agreeing target's.
//     "date" is part of created_at's "joined date" AND of placed_at's "order
//     date"; "value" is part of total_spent's "purchase value" AND of
//     order.amount's "order value". The config itself gives the word two homes.
//   - EXPLICIT DISTINGUISHING EVIDENCE: every word of one of a target's
//     configured surfaces that holds the fragment appears in the sentence. In
//     "orders by date", "order date" is spelled out, and it belongs to
//     placed_at only. It must single out ONE target, and that target must be
//     the leading reading; anything else is refused.
//   - NOT EVIDENCE: entity context. In "show customer date", "customer" names
//     the entity that owns created_at, but placed_at is one join away from it,
//     and a customer's order date is as much a "customer date". Context scores
//     the candidates (0.711 + 0.15 against 0.726 - 0.075); it cannot settle
//     what the words never said. Nor is a PARTIAL surface: "customer" is in
//     "customer creation date", but "creation" is not in the sentence, and
//     the qualifier that makes the synonym specific is exactly what is missing
//     (lexicalScore's rule, applied across the sentence).
//
// Similarity does not matter. The first version refused only readings under
// MinScore (X03's 0.682), and so let "show customer date" through at 0.711
// with a 0.726 rival: a 0.015 difference in raw similarity decided the answer.
// The user's decision at the re-gate: the difference is not evidence.
//
// Deliberately narrow, so ordinary semantic linking is untouched:
//   - an exact or wholly-present surface in the span (lexical evidence) is
//     never affected ("amount" is order.amount's own name: decision 012
//     stands; "customer creation date" is created_at's own synonym);
//   - a paraphrase that shares no word with any surface ("money spent") is
//     never affected: bridging it is what the embedding layer is for;
//   - a fragment of ONE target only ("created" in created_at, "joined" in
//     "joined date") is not shared: the config gives the word one home;
//   - descriptions do not count: their prose shares common words everywhere.
//
// No global "shared words are ambiguous" list: whether a word is shared is
// read from the config, and whether it is settled is read from the sentence.
//
// The whole corpus is checked, not only what retrieval returned: a competitor
// the model ranked outside top-k shares the word just the same. A competitor
// retrieval did not return is appended as a zero-similarity candidate, so the
// refusal can name it. Nothing is appended or annotated unless the span is
// refused, so an accepted span's candidate list is what ranking decided on.
//
// UNCONFIGURED QUALIFIERS (Phase 8, decision 021, finding "update date"). The
// rule above looks only at spans ALL of whose words are in the reading's
// configured surfaces. A span that adds a word none of them contains slipped
// past it, and past binding's unexplained-word rule too, because the word was
// inside an accepted span: "show customers by update date" compiled to
// created_at on entity context alone, "show customers by modification date"
// the same, and "orders by shipping date" to placed_at — "orders" spells out
// "order date", which settled "date", and "shipping" was silently dropped.
//
// The config says which words are GENERIC: a word that is a strict fragment
// of several targets' configured phrases ("date" in "joined date" and "order
// date") needs a qualifier to name one of them. When the span qualifies it
// with a content word that none of the reading's configured phrases contains,
// the span names a target the config does not have — an update date, a
// shipping date — and no evidence elsewhere in the sentence can make it mean
// a configured one. The span is REJECTED as a reading. The cover then reads
// its words separately: the generic word meets §5 on its own (refused, or
// settled by spelled-out evidence), and the qualifier meets binding's rule
// that an unexplained word refuses the sentence (decision 016). Each refusal
// then states its own, true reason.
//
// Deliberately narrow:
//   - only a word the config shows to be generic (shared, as §5 defines it)
//     triggers it. "money spent" is untouched: "spent" has one home
//     (total_spent), so it names its target without a qualifier, and "money"
//     is paraphrase — the brief's own probe;
//   - a span with NO configured word ("registered" for created_at) is
//     untouched: semantic linking, which the similarity floor measured and
//     rejected in decision 019 would have refused;
//   - a function word ("of", "on") set aside does not trigger it.
func (r *ranker) fragmentAmbiguity(s *SpanRanking) {
	if s.Decision != Accept || s.Top < 0 {
		return
	}
	top := s.Candidates[s.Top]
	if top.Evidence.Lexical != 0 {
		return // the span itself matches a configured surface
	}
	words := foldWords(s.Span.Norm)
	if len(words) == 0 {
		return
	}
	qualified := false
	if !fragmentOf(words, top.Entry) {
		known, setAside := r.configuredWords(s.Span, top.Entry)
		if len(known) == 0 || !setAside || !fragmentOf(known, top.Entry) {
			return // not a fragment of the reading: a paraphrase, or no words
		}
		words, qualified = known, true
	}
	if r.entries == nil {
		r.entries = corpus.Build(r.in.Registry)
	}

	// Every other target the same words are a fragment of, found without
	// touching the span: whether it is refused is decided below.
	var sharers []corpus.Entry
	for _, e := range r.entries {
		if e.ID == top.Entry.ID || agrees(top.Entry, e) || e.Kind == corpus.KindValueAlias {
			continue
		}
		if e.Kind == corpus.KindField && !e.Meta.Selectable && !e.Meta.Filterable {
			continue // a join key: no phrase can mean it (the scoreLocal veto)
		}
		if !fragmentOf(words, e) {
			continue
		}
		if idx := candidateIndex(s.Candidates, e.ID); idx >= 0 && s.Candidates[idx].Vetoed {
			continue // ruled out by the schema: not a reading, so not a rival
		}
		sharers = append(sharers, e)
	}
	if len(sharers) == 0 {
		return // the config gives the words one home
	}

	if qualified {
		// Decision 021: a generic word qualified by a word no configured
		// phrase of the reading contains. Rejected whatever the sentence
		// spells out elsewhere; the cover re-reads the words separately.
		s.Candidates[s.Top].Reasons = append(s.Candidates[s.Top].Reasons,
			"reject: the span qualifies a word shared by several targets' configured surfaces with a word none of this target's configured surfaces contains, so it names a target the config does not have")
		s.Decision, s.Code = Reject, qferr.CodeNoSemanticMatch
		s.Confidence = 0
		return
	}

	// Explicit distinguishing evidence: which of the sharing targets has a
	// configured surface spelled out, whole, by the sentence's own words.
	sentence := r.sentenceWords()
	topNamed := wholeSurfaceIn(words, top.Entry, sentence)
	othersNamed := 0
	for _, e := range sharers {
		if wholeSurfaceIn(words, e, sentence) {
			othersNamed++
		}
	}
	if topNamed && othersNamed == 0 {
		s.Candidates[s.Top].Reasons = append(s.Candidates[s.Top].Reasons,
			"lexical: the span's words are shared with another target's configured surface, but every word of this target's configured surface is in the sentence, and no other target's is")
		return
	}

	// Refused. The reason says which way the evidence failed; all three are
	// structural (no sentence text, no config value), as Reasons must be.
	reason := "ambiguous: the span is only part of this target's configured surface and part of another target's too, and the sentence spells out neither target's configured surface, so nothing distinguishes them"
	switch {
	case topNamed:
		reason = "ambiguous: the span is only part of this target's configured surface and part of another target's too, and the sentence spells out a configured surface of more than one of them"
	case othersNamed > 0:
		reason = "ambiguous: the span is only part of this target's configured surface and part of another target's too, and the sentence spells out a configured surface of another target, not this one"
	}
	var competitors []int
	for _, e := range sharers {
		idx := candidateIndex(s.Candidates, e.ID)
		if idx < 0 {
			s.Candidates = append(s.Candidates, Candidate{Entry: e})
			idx = len(s.Candidates) - 1
		}
		s.Candidates[idx].Reasons = append(s.Candidates[idx].Reasons,
			"lexical: the span's words are also part of this target's configured surface")
		competitors = append(competitors, idx)
	}
	s.Candidates[s.Top].Reasons = append(s.Candidates[s.Top].Reasons, reason)
	s.Decision, s.Code = Ambiguous, qferr.CodeAmbiguousMatch
	s.Competitors = competitors
	s.Confidence = 0
	s.SharedPhrase = true
}

// configuredWords splits a span's words by the reading: those that appear in
// one of the entry's name, synonym or phrase surfaces (known, folded, in span
// order), and whether any CONTENT word — not a function word — appears in
// none of them.
func (r *ranker) configuredWords(span nlp.Span, e corpus.Entry) (known []string, contentSetAside bool) {
	have := map[string]bool{}
	for _, sf := range e.Surfaces {
		if sf.Kind != corpus.SurfaceName && sf.Kind != corpus.SurfaceSynonym && sf.Kind != corpus.SurfacePhrase {
			continue
		}
		for _, w := range foldWords(sf.Text) {
			have[w] = true
		}
	}
	toks := r.in.Analysis.Tokens
	for i := span.Start; i < span.End && i < len(toks); i++ {
		for _, w := range foldWords(toks[i].Norm) {
			switch {
			case have[w]:
				known = append(known, w)
			case !toks[i].Stop:
				contentSetAside = true
			}
		}
	}
	return known, contentSetAside
}

// candidateIndex returns the position of the candidate for id, or -1.
func candidateIndex(cs []Candidate, id string) int {
	for i := range cs {
		if cs[i].Entry.ID == id {
			return i
		}
	}
	return -1
}

// sentenceWords is the set of the sentence's words, folded as every lexical
// test folds them. Function words are kept: a configured surface may hold one
// ("ordered on"), and it is the config author's phrase that must be spelled
// out, not a filtered version of it.
func (r *ranker) sentenceWords() map[string]bool {
	out := map[string]bool{}
	for _, t := range r.in.Analysis.Tokens {
		if t.Kind != nlp.KindWord {
			continue
		}
		for _, w := range foldWords(t.Norm) {
			out[w] = true
		}
	}
	return out
}

// wholeSurfaceIn reports whether one of e's configured surfaces that holds the
// span as a strict fragment has EVERY word in the sentence: the sentence
// spells the target's own phrase out, perhaps not contiguously ("orders by
// date" spells "order date"). Contiguity is not required because the words
// around a phrase are the sentence's structure, not the phrase's; requiring it
// refused "orders by date" (the uniform variant measured in decision 019).
func wholeSurfaceIn(words []string, e corpus.Entry, sentence map[string]bool) bool {
	for _, sw := range fragmentSurfaces(words, e) {
		all := true
		for _, w := range sw {
			if !sentence[w] {
				all = false
				break
			}
		}
		if all {
			return true
		}
	}
	return false
}

// fragmentOf reports whether every folded word of the span appears in one of
// the entry's name, synonym or phrase surfaces, and that surface has more
// words than the span: the span is a strict fragment of it.
func fragmentOf(words []string, e corpus.Entry) bool {
	return len(fragmentSurfaces(words, e)) > 0
}

// fragmentSurfaces returns, folded, each of the entry's name, synonym or
// phrase surfaces that holds every word of the span and has more words than
// the span.
func fragmentSurfaces(words []string, e corpus.Entry) [][]string {
	var out [][]string
	for _, sf := range e.Surfaces {
		if sf.Kind != corpus.SurfaceName && sf.Kind != corpus.SurfaceSynonym && sf.Kind != corpus.SurfacePhrase {
			continue
		}
		sw := foldWords(sf.Text)
		if len(sw) <= len(words) {
			continue
		}
		have := make(map[string]bool, len(sw))
		for _, w := range sw {
			have[w] = true
		}
		all := true
		for _, w := range words {
			if !have[w] {
				all = false
				break
			}
		}
		if all {
			out = append(out, sw)
		}
	}
	return out
}
