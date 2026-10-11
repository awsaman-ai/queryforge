package binding

import (
	"fmt"
	"sort"
	"strings"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/corpus"
	"queryforge.local/v2experiment/internal/graph"
	"queryforge.local/v2experiment/internal/nlp"
	"queryforge.local/v2experiment/internal/qferr"
	"queryforge.local/v2experiment/internal/ranking"
)

// Input is everything binding needs for one sentence. Ranking's result is
// consumed as it is: binding does not re-rank (the Phase 4 interface is
// unchanged).
type Input struct {
	Analysis nlp.Analysis
	Ranking  ranking.Result
	Registry *config.SchemaRegistry
	Graph    *graph.Graph
}

// Bind turns an accepted reading into a checked meaning, or refuses.
//
// Refusal is the safe failure. Every rejection case section 13 requires maps
// to a stable code:
//
//	candidates cannot be connected           RELATIONSHIP_NOT_FOUND
//	several join paths equally plausible     AMBIGUOUS_MATCH
//	field incompatible with an operator      UNSUPPORTED_QUERY
//	aggregation invalid for a field type     UNSUPPORTED_QUERY
//	a value that cannot be resolved safely   VALUE_RESOLUTION_FAILED
//
// plus two of binding's own: a word nothing explains (UNSUPPORTED_QUERY, see
// bindOrphans) and more conditions than the security limit
// (SECURITY_REJECTED). Binding stops at the first refusal; the first is
// reported deterministically because items are processed left to right.
func Bind(in Input) (Binding, error) {
	if in.Registry == nil || in.Graph == nil {
		return Binding{}, qferr.New(qferr.CodeInternal, "binding needs a registry and a schema graph")
	}
	// Binding only ever sees a reading ranking was willing to stand behind.
	// An ambiguous phrase is not something binding may resolve by structure
	// after the fact — that would be guessing with extra steps.
	if err := in.Ranking.Err(); err != nil {
		return Binding{}, err
	}
	// Then the Phase 8 completeness rules (decision 022): a number that
	// cannot be read exactly, or a meaningful word the reading would discard,
	// refuses the sentence before any structure is built on it.
	if err := checkComplete(in); err != nil {
		return Binding{}, err
	}
	b := &binder{in: in, reg: in.Registry, g: in.Graph, paths: map[string]graph.Path{},
		countRels: map[string]bool{}, trivialRels: map[string]bool{}}
	b.items = buildItems(in.Analysis, in.Ranking)
	if err := b.run(); err != nil {
		return b.out, err
	}
	return b.out, nil
}

// ---------------------------------------------------------------------------
// Items: the accepted reading, left to right
// ---------------------------------------------------------------------------

type itemKind int

const (
	kEntity itemKind = iota
	kField
	kValue
	kOperator
	kAggregation
	kPattern
	kRelationship
	kNumber
	kDate
	kConnective
	kWord
)

type item struct {
	kind       itemKind
	start, end int
	entry      corpus.Entry // for ranked readings
	span       nlp.Span     // for numbers and dates
	conn       string       // for connectives: and / or / not
	text       string       // the user's words, for errors
	used       bool
	// role records what claimed an item that is used but is not a condition
	// operand: a pattern's count or date, or a field mention an aggregate,
	// metric or window took.
	role itemRole
}

type itemRole int

const (
	roleNone itemRole = iota
	rolePatternCount
	rolePatternDate
	roleClaimed
)

func buildItems(a nlp.Analysis, res ranking.Result) []item {
	var items []item
	covered := make([]bool, len(a.Tokens))
	for _, i := range res.Cover {
		s := res.Spans[i]
		top, ok := s.TopCandidate()
		if !ok {
			continue
		}
		it := item{start: s.Span.Start, end: s.Span.End, entry: top.Entry, text: s.Span.Text}
		switch top.Entry.Kind {
		case corpus.KindEntity:
			it.kind = kEntity
		case corpus.KindField:
			it.kind = kField
		case corpus.KindValueAlias:
			it.kind = kValue
		case corpus.KindOperator:
			it.kind = kOperator
		case corpus.KindAggregation:
			it.kind = kAggregation
		case corpus.KindQueryPattern:
			it.kind = kPattern
		case corpus.KindRelationship:
			it.kind = kRelationship
		default:
			continue
		}
		for t := s.Span.Start; t < s.Span.End && t < len(covered); t++ {
			covered[t] = true
		}
		items = append(items, it)
	}
	for _, s := range a.Spans {
		switch {
		case s.Kind == nlp.SpanNumber && s.Number != nil:
			items = append(items, item{kind: kNumber, start: s.Start, end: s.End, span: s, text: s.Text})
		case s.Kind == nlp.SpanDate && s.Date != nil:
			items = append(items, item{kind: kDate, start: s.Start, end: s.End, span: s, text: s.Text})
		}
	}
	for _, c := range a.Connectives {
		if c.Start < len(covered) && covered[c.Start] {
			continue // inside an accepted phrase; not a joiner
		}
		items = append(items, item{kind: kConnective, start: c.Start, end: c.End, conn: c.Op, text: c.Text})
	}
	for _, t := range res.Unexplained {
		items = append(items, item{kind: kWord, start: t, end: t + 1, text: a.Tokens[t].Text})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].start != items[j].start {
			return items[i].start < items[j].start
		}
		return items[i].kind < items[j].kind
	})
	return items
}

// ---------------------------------------------------------------------------
// The binder
// ---------------------------------------------------------------------------

type binder struct {
	in    Input
	reg   *config.SchemaRegistry
	g     *graph.Graph
	items []item
	out   Binding

	// mentions are field items named without an operand: candidates for
	// aggregation, ranking metric, or selection, claimed in that order.
	mentions []int
	// relMentions are relationship names the sentence used, in order. They
	// steer path choice (graph.Resolve) and may become exists conditions.
	relMentions []string
	relItems    []int
	// countRels are relationships consumed by a count condition.
	countRels map[string]bool
	// trivialRels are relationships whose existence is always satisfied.
	trivialRels map[string]bool
	// entityMentions are non-root entity items.
	entityMentions []int
	// paths caches the resolved path per target entity.
	paths map[string]graph.Path
}

func (b *binder) run() error {
	if err := b.chooseRoot(); err != nil {
		return err
	}
	if err := b.claimPatternOperands(); err != nil {
		return err
	}
	if err := b.bindConditions(); err != nil {
		return err
	}
	if err := b.bindAggregates(); err != nil {
		return err
	}
	if err := b.bindPattern(); err != nil {
		return err
	}
	if err := b.bindOrphans(); err != nil {
		return err
	}
	b.bindSelections()
	// Paths first, THEN mentions: whether "bought" still needs an exists
	// condition depends on whether some other path already joins through it.
	// The first version did these the other way round and gave "customers who
	// bought laptops" a redundant exists(customer_orders) alongside the
	// product.category condition that already traverses it.
	if err := b.resolvePaths(); err != nil {
		return err
	}
	if err := b.bindRelationshipsAndEntities(); err != nil {
		return err
	}
	if err := b.finishJoins(); err != nil {
		return err
	}
	if err := b.joinConditions(); err != nil {
		return err
	}
	if max := b.reg.Security().MaxFilters; max > 0 && len(b.out.Conditions) > max {
		return qferr.New(qferr.CodeSecurityRejected,
			"the sentence binds %d conditions, over the configured limit of %d", len(b.out.Conditions), max)
	}
	return nil
}

// chooseRoot picks the entity the sentence is about.
//
//  1. the first entity the sentence names ("customers whose order amount...":
//     customer, although order is also mentioned);
//  2. otherwise the entity of the first field or value ("order amount over
//     100": order);
//  3. otherwise the From side of the first relationship.
//
// Leftmost, not most-frequent: English puts the subject first in every
// sentence shape this experiment handles, and "first" is a rule a person can
// check by eye.
func (b *binder) chooseRoot() error {
	for _, want := range []itemKind{kEntity, kField, kValue} {
		for _, it := range b.items {
			if it.kind == want {
				b.out.Root = it.entry.Entity
				return nil
			}
		}
	}
	for _, it := range b.items {
		if it.kind == kRelationship {
			if rel, ok := b.reg.Relationship(it.entry.Ref); ok {
				b.out.Root = rel.From.Entity
				return nil
			}
		}
	}
	return qferr.New(qferr.CodeNoSemanticMatch,
		"the sentence names no entity, field or value, so there is nothing to bind it to")
}

// claimPatternOperands takes the count beside a top-N pattern and the date
// beside a time-window pattern BEFORE conditions are read, so "top 5
// customers" does not leave 5 looking like an orphan comparison value.
func (b *binder) claimPatternOperands() error {
	for i := range b.items {
		it := &b.items[i]
		if it.kind != kPattern {
			continue
		}
		switch it.entry.Meta.Intent {
		case config.IntentTopN:
			for _, j := range []int{i + 1, i - 1} {
				if j >= 0 && j < len(b.items) && b.items[j].kind == kNumber && !b.items[j].used && isCount(b.items[j].span) {
					b.items[j].used = true
					b.items[j].role = rolePatternCount
					break
				}
			}
		case config.IntentTimeWindow:
			for _, j := range []int{i + 1, i - 1} {
				if j >= 0 && j < len(b.items) && b.items[j].kind == kDate && !b.items[j].used {
					b.items[j].used = true
					b.items[j].role = rolePatternDate
					break
				}
			}
		}
	}
	return nil
}

// tail returns the items after i that belong to the phrase begun at i: up to
// the next subject (entity, field, relationship, aggregation, pattern, or a
// value of a DIFFERENT field), or an and/or connective.
func (b *binder) tail(i int, field *FieldRef) []int {
	var out []int
	for j := i + 1; j < len(b.items); j++ {
		it := b.items[j]
		switch it.kind {
		case kConnective:
			if it.conn == "and" || it.conn == "or" {
				return out
			}
		case kEntity, kField, kRelationship, kAggregation, kPattern:
			return out
		case kValue:
			if field == nil || it.entry.Entity != field.Entity || it.entry.Ref != field.Field {
				return out
			}
		}
		out = append(out, j)
	}
	return out
}

// operandIn finds, within a tail, the operators before the operand, the
// operand itself, and a "not" if one precedes it.
type operand struct {
	op      config.Operator
	hasOp   bool
	opItem  int
	at      int   // index of the operand item, or -1
	words   []int // for a literal: the contiguous run of words
	negated bool
	unit    int // a unit word directly after a numeric operand, or -1
}

func (b *binder) operandIn(tail []int) operand {
	o := operand{at: -1, unit: -1, opItem: -1}
	for k, j := range tail {
		it := b.items[j]
		if it.used {
			continue
		}
		switch it.kind {
		case kConnective:
			if it.conn == "not" {
				o.negated = true
			}
		case kOperator:
			// The LAST operator before the operand is the comparison: "is
			// above" reads as one comparison, and "is" never reaches here
			// anyway (function-word spans are dropped in Phase 3).
			o.op, o.hasOp, o.opItem = config.Operator(it.entry.Ref), true, j
		case kNumber, kDate, kValue:
			o.at = j
			if it.kind == kNumber && k+1 < len(tail) {
				next := b.items[tail[k+1]]
				// A unit: exactly one unexplained word directly after the
				// number ("3 times", "500 dollars"). Positional, bounded, and
				// never interpreted — kept only so it is not an unexplained
				// word. See docs/phase-5-results.md.
				if next.kind == kWord && next.start == it.end {
					o.unit = tail[k+1]
				}
			}
			return o
		case kWord:
			o.at = j
			o.words = []int{j}
			for _, n := range tail[k+1:] {
				if b.items[n].kind == kWord && b.items[n].start == b.items[o.words[len(o.words)-1]].end {
					o.words = append(o.words, n)
					continue
				}
				break
			}
			return o
		}
	}
	return o
}

// bindConditions reads every subject — field, relationship, standalone value —
// with the operator and operand that follow it.
func (b *binder) bindConditions() error {
	pendingNot := false
	for i := range b.items {
		it := b.items[i]
		if it.used {
			continue
		}
		switch it.kind {
		case kConnective:
			switch it.conn {
			case "not":
				pendingNot = true
				b.items[i].used = true
			case "and", "or":
				pendingNot = false
			}
		case kEntity:
			b.items[i].used = true
			if it.entry.Entity != b.out.Root {
				b.entityMentions = append(b.entityMentions, i)
			}
		case kField:
			f, err := b.fieldRef(it)
			if err != nil {
				return err
			}
			o := b.operandIn(b.tail(i, &f))
			if o.at < 0 {
				if o.hasOp {
					return b.issue(qferr.CodeUnsupportedQuery, i,
						"%s compares %s with nothing: the operator %s has no value after it", "{phrase}", f, o.op)
				}
				b.items[i].used = true
				b.mentions = append(b.mentions, i)
				continue
			}
			cond, err := b.compare(f, o, i, pendingNot || o.negated, false)
			if err != nil {
				return err
			}
			pendingNot = false
			b.out.Conditions = append(b.out.Conditions, cond)
		case kRelationship:
			b.items[i].used = true
			o := b.operandIn(b.tail(i, nil))
			if o.at >= 0 && b.items[o.at].kind == kNumber {
				cond, err := b.count(it, o, i, pendingNot || o.negated)
				if err != nil {
					return err
				}
				pendingNot = false
				b.out.Conditions = append(b.out.Conditions, cond)
				continue
			}
			b.relMentions = append(b.relMentions, it.entry.Ref)
			b.relItems = append(b.relItems, i)
		case kValue:
			// A value no field claimed: "customers from India". Its own field
			// is the subject; an operator directly before it is the
			// comparison, eq otherwise.
			f, err := b.fieldRef(it)
			if err != nil {
				return err
			}
			o := operand{at: i, unit: -1, opItem: -1, op: config.OpEq}
			if i > 0 && b.items[i-1].kind == kOperator && !b.items[i-1].used {
				o.op, o.hasOp, o.opItem = config.Operator(b.items[i-1].entry.Ref), true, i-1
			}
			cond, err := b.compare(f, o, i, pendingNot, false)
			if err != nil {
				return err
			}
			pendingNot = false
			b.out.Conditions = append(b.out.Conditions, cond)
		}
	}
	return nil
}

// compare builds and checks field <op> operand.
func (b *binder) compare(f FieldRef, o operand, subject int, negated, inferred bool) (Condition, error) {
	field, _ := b.reg.Field(f.Entity, f.Field)
	start, end := b.items[subject].start, b.items[subject].end
	opnd := b.items[o.at]
	if opnd.end > end {
		end = opnd.end
	}
	for _, w := range o.words {
		if b.items[w].end > end {
			end = b.items[w].end
		}
	}

	cond := Condition{Kind: CondCompare, Field: f, Inferred: inferred, Start: start, End: end}
	op := o.op
	if !o.hasOp {
		op = config.OpEq
	}

	switch opnd.kind {
	case kNumber:
		if f.Type != config.TypeNumber {
			return cond, b.issue(qferr.CodeUnsupportedQuery, o.at,
				"%s is a number, but %s is %s field", "{phrase}", f, article(f.Type))
		}
		n := opnd.span.Number
		cond.Value = Value{Kind: ValueNumber, Number: n.Value, Currency: n.Currency, Percent: n.Percent}
		if o.unit >= 0 {
			cond.Value.Unit = qferr.Sensitive(b.items[o.unit].text)
			b.items[o.unit].used = true
			b.note("a unit word after the value of %s is kept but not interpreted", f)
		}
	case kDate:
		if f.Type != config.TypeDate {
			return cond, b.issue(qferr.CodeUnsupportedQuery, o.at,
				"%s is a date, but %s is %s field", "{phrase}", f, article(f.Type))
		}
		d := *opnd.span.Date
		cond.Value = Value{Kind: ValueDate, Date: &d}
		if !o.hasOp {
			cond.Kind = CondWindow
			op = ""
		}
	case kValue:
		if opnd.entry.Entity != f.Entity || opnd.entry.Ref != f.Field {
			return cond, b.issue(qferr.CodeUnsupportedQuery, o.at,
				"%s is a value of %s.%s, not of %s", "{phrase}", opnd.entry.Entity, opnd.entry.Ref, f)
		}
		kind := ValueAlias
		if strings.Contains(opnd.entry.ID, ":enum:") {
			kind = ValueEnum
		}
		cond.Value = Value{Kind: kind, Stored: qferr.Sensitive(opnd.entry.Meta.ValueTarget)}
	case kWord:
		v, err := b.literal(f, field, o)
		if err != nil {
			return cond, err
		}
		cond.Value = v
		for _, w := range o.words {
			b.items[w].used = true
		}
	}

	if negated {
		comp, ok := complement(op, cond.Kind)
		if !ok {
			return cond, b.issue(qferr.CodeUnsupportedQuery, subject,
				"%s is negated, but the operator %s has no configured opposite", "{phrase}", op)
		}
		op = comp
		cond.Negated = true
	}

	if cond.Kind == CondCompare {
		if !field.IsFilterable() {
			return cond, b.issue(qferr.CodeUnsupportedQuery, subject,
				"%s is %s, which is not filterable", "{phrase}", f)
		}
		if !containsOp(field.AllowedOperators(), op) {
			return cond, b.issue(qferr.CodeUnsupportedQuery, subject,
				"%s compares %s with %s, which that field does not allow", "{phrase}", f, op)
		}
	} else if !field.IsFilterable() {
		return cond, b.issue(qferr.CodeUnsupportedQuery, subject,
			"%s is %s, which is not filterable", "{phrase}", f)
	}
	cond.Op = op

	b.items[subject].used = true
	b.items[o.at].used = true
	if o.opItem >= 0 {
		b.items[o.opItem].used = true
	}
	return cond, nil
}

// literal resolves free text against a field.
//
// String fields take the text as written — "customers named Smith". Enum
// fields accept only a declared value or alias, matched case-insensitively:
// anything else is a value that cannot be resolved safely (section 13), and
// guessing the nearest enum value would be exactly the silent substitution
// the experiment exists to avoid. Numbers, dates and booleans never take free
// text.
func (b *binder) literal(f FieldRef, field *config.Field, o operand) (Value, error) {
	var words []string
	for _, w := range o.words {
		words = append(words, b.items[w].text)
	}
	text := strings.Join(words, " ")
	switch f.Type {
	case config.TypeString:
		if target, ok := b.reg.ResolveValueAlias(f.Entity, f.Field, text); ok {
			return Value{Kind: ValueAlias, Stored: qferr.Sensitive(target)}, nil
		}
		return Value{Kind: ValueLiteral, Stored: qferr.Sensitive(text)}, nil
	case config.TypeEnum:
		if target, ok := b.reg.ResolveValueAlias(f.Entity, f.Field, text); ok {
			return Value{Kind: ValueAlias, Stored: qferr.Sensitive(target)}, nil
		}
		var hits []string
		for _, v := range field.EnumValues {
			if strings.EqualFold(v, text) {
				hits = append(hits, v)
			}
		}
		if len(hits) == 1 {
			return Value{Kind: ValueEnum, Stored: qferr.Sensitive(hits[0])}, nil
		}
		return Value{}, b.issue(qferr.CodeValueResolutionFailed, o.words[0],
			"%s is not a declared value or alias of %s", "{phrase}", f)
	default:
		return Value{}, b.issue(qferr.CodeValueResolutionFailed, o.words[0],
			"%s cannot be read as a value of the %s field %s", "{phrase}", f.Type, f)
	}
}

// count builds "count of related rows <op> N" from a relationship with a
// number after it: "customers who ordered more than 3 times".
//
// Supported only when the root is one end of the relationship and the other
// end is reached to-many. Counting through a relationship that does not touch
// the root would need a choice of WHAT to count, and a to-one count is always
// 0 or 1 — both are refused rather than given a meaning.
func (b *binder) count(it item, o operand, subject int, negated bool) (Condition, error) {
	rel, ok := b.reg.Relationship(it.entry.Ref)
	if !ok {
		return Condition{}, qferr.New(qferr.CodeInternal, "relationship %s vanished from the registry", it.entry.Ref)
	}
	var far string
	switch b.out.Root {
	case rel.From.Entity:
		far = rel.To.Entity
	case rel.To.Entity:
		far = rel.From.Entity
	default:
		return Condition{}, b.issue(qferr.CodeUnsupportedQuery, subject,
			"%s counts through %s, which does not touch %s, so what to count is not determined",
			"{phrase}", rel.Name, b.out.Root)
	}
	path, err := b.g.Resolve(b.out.Root, far, []string{rel.Name})
	if err != nil {
		return Condition{}, err
	}
	if !path.Uses(rel.Name) || !path.ToMany() {
		return Condition{}, b.issue(qferr.CodeUnsupportedQuery, subject,
			"%s counts %s through %s, but each %s has at most one: a count would always be 0 or 1",
			"{phrase}", far, rel.Name, b.out.Root)
	}

	n := b.items[o.at].span.Number
	if n == nil || n.Malformed || n.Value.Sign() < 0 || !n.Value.IsInteger() || n.Currency != "" || n.Percent {
		return Condition{}, b.issue(qferr.CodeValueResolutionFailed, o.at,
			"%s is not a whole number of rows to count", "{phrase}")
	}
	op := o.op
	if !o.hasOp {
		op = config.OpEq
	}
	if !containsOp(config.OperatorsFor(config.TypeNumber), op) {
		return Condition{}, b.issue(qferr.CodeUnsupportedQuery, subject,
			"%s compares a count with %s, which applies only to numbers' own operators", "{phrase}", op)
	}
	cond := Condition{Kind: CondCount, Relationship: rel.Name, Path: path,
		Value: Value{Kind: ValueNumber, Number: n.Value}, Start: it.start, End: b.items[o.at].end}
	if negated {
		comp, ok := complement(op, CondCompare)
		if !ok {
			return cond, b.issue(qferr.CodeUnsupportedQuery, subject, "%s cannot be negated", "{phrase}")
		}
		op, cond.Negated = comp, true
	}
	cond.Op = op
	b.countRels[rel.Name] = true
	b.items[o.at].used = true
	if o.opItem >= 0 {
		b.items[o.opItem].used = true
	}
	if o.unit >= 0 {
		cond.Value.Unit = qferr.Sensitive(b.items[o.unit].text)
		b.items[o.unit].used = true
		cond.End = b.items[o.unit].end
		b.note("a unit word after the count through %s is kept but not interpreted", rel.Name)
	}
	return cond, nil
}

// bindAggregates attaches each aggregation to the field it applies to: the
// nearest unclaimed field mention to its right, else to its left. count with
// no field counts rows of the entity named after it, or of the root.
func (b *binder) bindAggregates() error {
	for i := range b.items {
		it := b.items[i]
		isCountPattern := it.kind == kPattern && it.entry.Meta.Intent == config.IntentCount
		if it.kind != kAggregation && !isCountPattern {
			continue
		}
		fn := config.AggCount
		if it.kind == kAggregation {
			fn = config.Aggregation(it.entry.Ref)
		}
		b.items[i].used = true

		field, entity := b.aggregateTarget(i)
		switch {
		case field >= 0:
			f, err := b.fieldRef(b.items[field])
			if err != nil {
				return err
			}
			cfgField, _ := b.reg.Field(f.Entity, f.Field)
			if !containsAgg(cfgField.AllowedAggregations(), fn) {
				return b.issue(qferr.CodeUnsupportedQuery, i,
					"%s applies %s to %s, which is not allowed for %s field", "{phrase}", fn, f, article(f.Type))
			}
			b.claimMention(field)
			ff := f
			b.out.Aggregates = append(b.out.Aggregates, Aggregate{Func: fn, Field: &ff})
		case fn == config.AggCount:
			target := b.out.Root
			if entity >= 0 {
				target = b.items[entity].entry.Entity
			}
			b.out.Aggregates = append(b.out.Aggregates, Aggregate{Func: fn, Entity: target})
		default:
			return b.issue(qferr.CodeUnsupportedQuery, i,
				"%s asks for %s but names no field to apply it to", "{phrase}", fn)
		}
	}
	return nil
}

// aggregateTarget finds the field mention (or entity, for count) an
// aggregation at i applies to, within its clause.
func (b *binder) aggregateTarget(i int) (field, entity int) {
	field, entity = -1, -1
	for j := i + 1; j < len(b.items); j++ {
		it := b.items[j]
		if it.kind == kConnective && (it.conn == "and" || it.conn == "or") {
			break
		}
		if it.kind == kField && b.isMention(j) {
			return j, entity
		}
		if it.kind == kEntity && entity < 0 {
			entity = j
		}
		if it.kind == kAggregation || it.kind == kPattern {
			break
		}
	}
	for j := i - 1; j >= 0; j-- {
		it := b.items[j]
		if it.kind == kConnective && (it.conn == "and" || it.conn == "or") {
			break
		}
		if it.kind == kField && b.isMention(j) {
			return j, entity
		}
	}
	return -1, entity
}

// bindPattern binds the single query pattern, if any.
func (b *binder) bindPattern() error {
	var found []int
	for i, it := range b.items {
		if it.kind == kPattern && it.entry.Meta.Intent != config.IntentCount {
			found = append(found, i)
		}
	}
	if len(found) == 0 {
		return nil
	}
	if len(found) > 1 {
		return b.issue(qferr.CodeUnsupportedQuery, found[1],
			"%s is a second query pattern; one sentence may invoke only one", "{phrase}")
	}
	i := found[0]
	it := b.items[i]
	b.items[i].used = true
	p := &Pattern{Name: it.entry.Ref, Intent: it.entry.Meta.Intent}

	switch p.Intent {
	case config.IntentTopN:
		for j := range b.items {
			if b.items[j].role == rolePatternCount {
				// isCount already required a whole number in int64's range;
				// the conversion is exact on every 64-bit platform, and a
				// value an int cannot hold is refused, never wrapped.
				n, _ := b.items[j].span.Number.Value.Int64()
				if int64(int(n)) != n {
					return b.issue(qferr.CodeValueResolutionFailed, j,
						"%s is more rows than a limit can hold", "{phrase}")
				}
				p.N = int(n)
			}
		}
		if p.N == 0 {
			return b.issue(qferr.CodeValueResolutionFailed, i,
				"%s asks for the top rows but gives no count, and the config sets no default", "{phrase}")
		}
		metric := -1
		for j := i + 1; j < len(b.items) && metric < 0; j++ {
			if b.items[j].kind == kField && b.isMention(j) {
				metric = j
			}
		}
		for j := i - 1; j >= 0 && metric < 0; j-- {
			if b.items[j].kind == kField && b.isMention(j) {
				metric = j
			}
		}
		if metric < 0 {
			return b.issue(qferr.CodeUnsupportedQuery, i,
				"%s ranks rows but names no field to rank them by", "{phrase}")
		}
		f, err := b.fieldRef(b.items[metric])
		if err != nil {
			return err
		}
		b.claimMention(metric)
		p.Metric = &f

	case config.IntentTimeWindow:
		date := -1
		for j := range b.items {
			if b.items[j].role == rolePatternDate {
				date = j
			}
		}
		if date < 0 {
			return b.issue(qferr.CodeValueResolutionFailed, i,
				"%s restricts to a time window, but no period is given and the config defines none", "{phrase}")
		}
		// An explicitly named date field wins; otherwise the root's one date
		// field.
		f, inferred, err := b.dateFieldFor(date)
		if err != nil {
			return err
		}
		d := *b.items[date].span.Date
		b.out.Conditions = append(b.out.Conditions, Condition{
			Kind: CondWindow, Field: f, Value: Value{Kind: ValueDate, Date: &d}, Inferred: inferred,
			Start: min(it.start, b.items[date].start), End: max(it.end, b.items[date].end),
		})
	}
	b.out.Pattern = p
	return nil
}

// bindOrphans handles what no subject claimed: an operator with a value but no
// field, a date with no field, and anything left over — which is refused.
func (b *binder) bindOrphans() error {
	for i := range b.items {
		it := b.items[i]
		if it.used {
			continue
		}
		switch it.kind {
		case kOperator:
			o := b.operandIn(b.tail(i, nil))
			// Re-read with the operator itself as the comparison.
			o.op, o.hasOp, o.opItem = config.Operator(it.entry.Ref), true, i
			if o.at < 0 {
				return b.issue(qferr.CodeUnsupportedQuery, i,
					"%s is a comparison with no value after it", "{phrase}")
			}
			f, err := b.inferField(i, b.items[o.at].kind, o.op)
			if err != nil {
				return err
			}
			cond, err := b.compare(f, o, i, o.negated, true)
			if err != nil {
				return err
			}
			b.out.Conditions = append(b.out.Conditions, cond)
		case kDate:
			f, inferred, err := b.dateFieldFor(i)
			if err != nil {
				return err
			}
			d := *it.span.Date
			b.items[i].used = true
			b.out.Conditions = append(b.out.Conditions, Condition{
				Kind: CondWindow, Field: f, Value: Value{Kind: ValueDate, Date: &d},
				Inferred: inferred, Start: it.start, End: it.end,
			})
		}
	}
	// Whatever is left has no role in the reading. Refuse, never drop:
	// dropping "Atlantis" from "customers from Atlantis" answers a BROADER
	// question than was asked, and nothing in the output would show it.
	for i, it := range b.items {
		if it.used {
			continue
		}
		switch it.kind {
		case kNumber:
			return b.issue(qferr.CodeUnsupportedQuery, i, "%s is a number with no role in the reading", "{phrase}")
		case kWord:
			return b.issue(qferr.CodeUnsupportedQuery, i,
				"%s is a word nothing in the config explains; binding refuses rather than ignore it", "{phrase}")
		case kOperator:
			return b.issue(qferr.CodeUnsupportedQuery, i, "%s is a comparison that applies to nothing", "{phrase}")
		}
	}
	return nil
}

// inferField finds the one root field an orphan comparison can mean.
func (b *binder) inferField(at int, operandKind itemKind, op config.Operator) (FieldRef, error) {
	var want config.FieldType
	switch operandKind {
	case kNumber:
		want = config.TypeNumber
	case kDate:
		want = config.TypeDate
	default:
		return FieldRef{}, b.issue(qferr.CodeUnsupportedQuery, at,
			"%s compares with a value but names no field", "{phrase}")
	}
	var hits []FieldRef
	for _, f := range b.reg.FieldsOf(b.out.Root) {
		if f.Type == want && f.IsFilterable() && containsOp(f.AllowedOperators(), op) {
			hits = append(hits, FieldRef{Entity: f.Entity, Field: f.Name, Type: f.Type})
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], nil
	case 0:
		return FieldRef{}, b.issue(qferr.CodeUnsupportedQuery, at,
			"%s compares with a %s, but %s has no filterable %s field that allows %s",
			"{phrase}", want, b.out.Root, want, op)
	default:
		var names []string
		for _, h := range hits {
			names = append(names, h.String())
		}
		return FieldRef{}, b.issue(qferr.CodeAmbiguousMatch, at,
			"%s names no field, and %s has several it could mean: %s",
			"{phrase}", b.out.Root, strings.Join(names, ", "))
	}
}

// dateFieldFor picks the date field a date expression restricts: a date field
// named in the same clause, else the root's only filterable date field.
func (b *binder) dateFieldFor(date int) (FieldRef, bool, error) {
	for _, m := range b.mentions {
		it := b.items[m]
		if it.entry.Meta.FieldType == config.TypeDate && b.sameClause(m, date) && b.isMention(m) {
			f, err := b.fieldRef(it)
			if err != nil {
				return f, false, err
			}
			b.claimMention(m)
			return f, false, nil
		}
	}
	var hits []FieldRef
	for _, f := range b.reg.FieldsOf(b.out.Root) {
		if f.Type == config.TypeDate && f.IsFilterable() {
			hits = append(hits, FieldRef{Entity: f.Entity, Field: f.Name, Type: f.Type})
		}
	}
	switch len(hits) {
	case 1:
		return hits[0], true, nil
	case 0:
		return FieldRef{}, false, b.issue(qferr.CodeUnsupportedQuery, date,
			"%s is a date, but %s has no filterable date field", "{phrase}", b.out.Root)
	default:
		var names []string
		for _, h := range hits {
			names = append(names, h.String())
		}
		return FieldRef{}, false, b.issue(qferr.CodeAmbiguousMatch, date,
			"%s is a date, and %s has several date fields it could restrict: %s",
			"{phrase}", b.out.Root, strings.Join(names, ", "))
	}
}

// bindSelections turns unclaimed field mentions into columns to show.
func (b *binder) bindSelections() {
	for _, m := range b.mentions {
		if !b.isMention(m) {
			continue
		}
		f, err := b.fieldRef(b.items[m])
		if err != nil {
			continue
		}
		cfgField, _ := b.reg.Field(f.Entity, f.Field)
		if cfgField != nil && !cfgField.IsSelectable() {
			b.note("%s is named but not selectable, so it is not shown", f)
			continue
		}
		b.out.Selections = append(b.out.Selections, Selection{Field: f})
	}
}

// bindRelationshipsAndEntities turns relationship and entity mentions that
// nothing else used into existence conditions — or into notes, when the
// relationship is trivially satisfied.
//
// "customers who ordered" is a real restriction (a customer may have no
// orders): the step from the root fans out, so EXISTS. "orders placed by a
// customer" is not: every order has its customer, so it binds nothing and
// says so in a note. The difference is the configured cardinality, not the
// wording.
func (b *binder) bindRelationshipsAndEntities() error {
	// What the paths resolved so far already traverse.
	traversed := map[string]bool{}
	reached := map[string]bool{b.out.Root: true}
	for _, p := range b.paths {
		for _, st := range p.Steps {
			traversed[st.Relationship] = true
		}
		for _, e := range p.Entities() {
			reached[e] = true
		}
	}

	for k, name := range b.relMentions {
		if b.countRels[name] || traversed[name] {
			continue
		}
		rel, _ := b.reg.Relationship(name)
		var far string
		switch b.out.Root {
		case rel.From.Entity:
			far = rel.To.Entity
		case rel.To.Entity:
			far = rel.From.Entity
		default:
			// Not touching the root and not traversed: checked in finishJoins,
			// which refuses a mentioned relationship nothing joins through.
			continue
		}
		if err := b.exists(far, name, b.items[b.relItems[k]]); err != nil {
			return err
		}
		reached[far] = true
		traversed[name] = true
	}
	for _, i := range b.entityMentions {
		e := b.items[i].entry.Entity
		if reached[e] {
			continue
		}
		if err := b.exists(e, "", b.items[i]); err != nil {
			return err
		}
		reached[e] = true
	}
	return nil
}

func (b *binder) exists(target, relationship string, it item) error {
	path, err := b.g.Resolve(b.out.Root, target, b.relMentions)
	if err != nil {
		return err
	}
	if relationship == "" && path.Len() > 0 {
		relationship = path.Steps[len(path.Steps)-1].Relationship
	}
	if !path.ToMany() {
		for _, st := range path.Steps {
			b.trivialRels[st.Relationship] = true
		}
		b.note("%s via %s is always satisfied (each %s has its %s), so it adds no condition",
			target, path, b.out.Root, target)
		return nil
	}
	b.paths[target] = path
	b.out.Conditions = append(b.out.Conditions, Condition{
		Kind: CondExists, Relationship: relationship, Path: path,
		Field: FieldRef{Entity: target}, Start: it.start, End: it.end,
	})
	return nil
}

// resolvePaths attaches a join path to everything that is not on the root,
// and checks that every mentioned relationship is actually traversed.
func (b *binder) resolvePaths() error {
	pathTo := func(entity string) (graph.Path, error) {
		if p, ok := b.paths[entity]; ok {
			return p, nil
		}
		p, err := b.g.Resolve(b.out.Root, entity, b.relMentions)
		if err != nil {
			return p, err
		}
		b.paths[entity] = p
		return p, nil
	}
	for i := range b.out.Conditions {
		c := &b.out.Conditions[i]
		if c.Kind == CondCount || c.Kind == CondExists {
			b.paths[c.Path.To] = c.Path
			continue
		}
		p, err := pathTo(c.Field.Entity)
		if err != nil {
			return err
		}
		c.Path = p
	}
	for i := range b.out.Aggregates {
		a := &b.out.Aggregates[i]
		target := a.Entity
		if a.Field != nil {
			target = a.Field.Entity
		}
		p, err := pathTo(target)
		if err != nil {
			return err
		}
		a.Path = p
	}
	if p := b.out.Pattern; p != nil && p.Metric != nil {
		path, err := pathTo(p.Metric.Entity)
		if err != nil {
			return err
		}
		p.Path = path
	}
	for i := range b.out.Selections {
		p, err := pathTo(b.out.Selections[i].Field.Entity)
		if err != nil {
			return err
		}
		b.out.Selections[i].Path = p
	}

	return nil
}

// finishJoins collects the distinct join paths and checks that every mentioned
// relationship is actually traversed — by a path, a count, an exists, or a
// trivially satisfied step. A verb the binding ignored would be a meaning
// silently dropped.
func (b *binder) finishJoins() error {
	seen := map[string]bool{}
	for _, p := range b.paths {
		if p.Len() == 0 || seen[p.Key()] {
			continue
		}
		seen[p.Key()] = true
		b.out.Joins = append(b.out.Joins, p)
	}
	for k, name := range b.relMentions {
		if b.countRels[name] {
			continue
		}
		used := false
		for _, p := range b.out.Joins {
			if p.Uses(name) {
				used = true
			}
		}
		if b.trivialRels[name] {
			used = true
		}
		if !used {
			return b.issue(qferr.CodeRelationshipNotFound, b.relItems[k],
				"%s names the relationship %s, but nothing else in the sentence is joined through it",
				"{phrase}", name)
		}
	}
	sort.Slice(b.out.Joins, func(i, j int) bool {
		if b.out.Joins[i].To != b.out.Joins[j].To {
			return b.out.Joins[i].To < b.out.Joins[j].To
		}
		return b.out.Joins[i].Key() < b.out.Joins[j].Key()
	})
	return nil
}

// joinConditions orders conditions left to right and records how each joins
// the previous: "or" when an or-connective sits between them, "and"
// otherwise. A sentence that mixes the two is refused — "A and B or C" has two
// readings, and precedence is a guess the user did not make.
func (b *binder) joinConditions() error {
	sort.SliceStable(b.out.Conditions, func(i, j int) bool {
		return b.out.Conditions[i].Start < b.out.Conditions[j].Start
	})
	sawAnd, sawOr := false, false
	for i := range b.out.Conditions {
		if i == 0 {
			continue
		}
		prev, cur := b.out.Conditions[i-1], &b.out.Conditions[i]
		cur.Join = "and"
		for _, it := range b.items {
			if it.kind == kConnective && it.conn == "or" && it.start >= prev.End && it.start < cur.Start {
				cur.Join = "or"
			}
		}
		if cur.Join == "or" {
			sawOr = true
		} else {
			sawAnd = true
		}
	}
	if sawAnd && sawOr {
		return qferr.New(qferr.CodeUnsupportedQuery,
			"the sentence combines conditions with both \"and\" and \"or\"; which groups first is not stated, and binding does not guess precedence")
	}
	return nil
}

// ---------------------------------------------------------------------------
// Helpers
// ---------------------------------------------------------------------------

func (b *binder) fieldRef(it item) (FieldRef, error) {
	f, ok := b.reg.Field(it.entry.Entity, it.entry.Ref)
	if !ok {
		return FieldRef{}, qferr.New(qferr.CodeInternal, "field %s.%s vanished from the registry", it.entry.Entity, it.entry.Ref)
	}
	return FieldRef{Entity: f.Entity, Field: f.Name, Type: f.Type}, nil
}

// isMention reports a field item named without an operand and not yet
// claimed by an aggregate, a pattern or a date window.
func (b *binder) isMention(i int) bool {
	for _, m := range b.mentions {
		if m == i {
			return b.items[i].role != roleClaimed
		}
	}
	return false
}

func (b *binder) claimMention(i int) { b.items[i].role = roleClaimed }

func (b *binder) sameClause(i, j int) bool {
	lo, hi := i, j
	if lo > hi {
		lo, hi = hi, lo
	}
	for k := lo + 1; k < hi; k++ {
		if b.items[k].kind == kConnective && (b.items[k].conn == "and" || b.items[k].conn == "or") {
			return false
		}
	}
	return true
}

func (b *binder) note(format string, args ...any) {
	b.out.Notes = append(b.out.Notes, fmt.Sprintf(format, args...))
}

// complement is the operator "not" turns op into. Windows and exists keep
// their operator and carry Negated instead; the planner renders NOT (...).
func complement(op config.Operator, kind ConditionKind) (config.Operator, bool) {
	if kind == CondWindow || kind == CondExists {
		return op, true
	}
	switch op {
	case config.OpEq:
		return config.OpNeq, true
	case config.OpNeq:
		return config.OpEq, true
	case config.OpGt:
		return config.OpLte, true
	case config.OpGte:
		return config.OpLt, true
	case config.OpLt:
		return config.OpGte, true
	case config.OpLte:
		return config.OpGt, true
	case config.OpIn:
		return config.OpNin, true
	case config.OpNin:
		return config.OpIn, true
	}
	return "", false
}

func containsOp(ops []config.Operator, op config.Operator) bool {
	for _, o := range ops {
		if o == op {
			return true
		}
	}
	return false
}

func containsAgg(aggs []config.Aggregation, a config.Aggregation) bool {
	for _, x := range aggs {
		if x == a {
			return true
		}
	}
	return false
}

// isCount is a positive whole number in int64's range with no currency or
// percent: the same test as ranking's, exact since decision 022 §2.
func isCount(s nlp.Span) bool {
	n := s.Number
	if n == nil || n.Malformed {
		return false
	}
	_, fits := n.Value.Int64()
	return fits && n.Value.Sign() > 0 && n.Currency == "" && !n.Percent
}

// article prefixes a field type with "a" or "an", so messages read as English:
// "an enum field", "a string field".
func article(t config.FieldType) string {
	if strings.IndexByte("aeiou", string(t)[0]) >= 0 {
		return "an " + string(t)
	}
	return "a " + string(t)
}
