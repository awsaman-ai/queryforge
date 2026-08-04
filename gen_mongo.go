package queryforge

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"
)

// MongoGenerator compiles an AST into a MongoDB find() specification. The model
// never contributes a query string: values become typed elements of a Go map
// (the filter document), so there is no string for an attacker to break out of.
// The output describes a read (find); there is no update/delete path.
type MongoGenerator struct{}

// Backend returns the registry id for this generator.
func (MongoGenerator) Backend() string { return "mongo" }

// MongoQuery is the compiled find() call: the collection, the filter document,
// and the usual options. Sort is an ordered slice (not a map) so multi-key sort
// order is preserved deterministically.
type MongoQuery struct {
	Collection string         `json:"collection"`           // physical collection name
	Filter     map[string]any `json:"filter"`               // the query document
	Projection map[string]any `json:"projection,omitempty"` // fields to return (1 = include)
	Sort       []MongoSortKey `json:"sort,omitempty"`       // ordered sort keys
	Limit      int            `json:"limit,omitempty"`      // max documents
	Skip       int            `json:"skip,omitempty"`       // documents to skip (offset)
}

// MongoSortKey is one ordered sort clause; Order is 1 (asc) or -1 (desc).
type MongoSortKey struct {
	Field string `json:"field"`
	Order int    `json:"order"`
}

// Generate builds the MongoQuery from the AST.
func (g MongoGenerator) Generate(q *Query, c *Config, opts GenOptions) (*Result, error) {
	now := opts.now()
	mq := &MongoQuery{
		Collection: mongoCollection(q, c),
		Filter:     map[string]any{}, // empty filter = match all
	}

	// Filter document.
	if q.Filter != nil {
		f, err := mongoCondition(c, q.Filter, now)
		if err != nil {
			return nil, err
		}
		mq.Filter = f
	}

	// Projection: {physicalField: 1, …}. Empty select => return everything, but
	// only when the config hides nothing (see projectionFields).
	if fields, explicit := projectionFields(q, c); explicit {
		// An empty projection document means "return everything" to Mongo — the
		// exact opposite of what an empty allow-list intends. Fail instead.
		if len(fields) == 0 {
			return nil, fmt.Errorf("generate mongo: no returnable fields: the config marks every field returnable:false, so the projection would be empty")
		}
		proj := make(map[string]any, len(fields))
		for _, name := range fields {
			proj[c.PhysicalName(name, "mongo")] = 1
		}
		mq.Projection = proj
	}

	// Sort keys, order preserved; DESC => -1, everything else => 1.
	for _, s := range q.Sort {
		order := 1
		if strings.EqualFold(s.Dir, "DESC") {
			order = -1
		}
		mq.Sort = append(mq.Sort, MongoSortKey{Field: c.PhysicalName(s.Field, "mongo"), Order: order})
	}

	mq.Limit = effectiveLimit(q, c)
	mq.Skip = effectiveOffset(q)

	return &Result{Backend: "mongo", Doc: mq, Warnings: collectIndexWarnings(q, c)}, nil
}

// mongoCollection resolves the physical collection name, defaulting to the
// entity name when no mapping is configured.
func mongoCollection(q *Query, c *Config) string {
	if bc, ok := c.Backends["mongo"]; ok && bc.Source() != "" {
		return bc.Source()
	}
	return q.Entity
}

// mongoCondition dispatches on node type, returning a filter fragment.
func mongoCondition(c *Config, cond *Condition, now time.Time) (map[string]any, error) {
	switch cond.Type {
	case CondLogical:
		return mongoLogical(c, cond, now)
	case CondComparison:
		return mongoComparison(c, cond, now)
	default:
		return nil, fmt.Errorf("mongo: unknown condition type %q", cond.Type)
	}
}

// mongoLogical renders AND/OR/NOT. AND is flattened into a single document when
// the child field keys do not collide (matching idiomatic Mongo), otherwise it
// falls back to $and. OR uses $or; NOT uses $nor over its single child.
func mongoLogical(c *Config, cond *Condition, now time.Time) (map[string]any, error) {
	children := orderedChildren(c, cond.Children) // predicate ordering
	parts := make([]map[string]any, 0, len(children))
	for _, ch := range children {
		m, err := mongoCondition(c, ch, now)
		if err != nil {
			return nil, err
		}
		parts = append(parts, m)
	}
	switch cond.Op {
	case OpAND:
		return mergeAnd(parts), nil
	case OpOR:
		return map[string]any{"$or": toAnySlice(parts)}, nil
	case OpNOT:
		return map[string]any{"$nor": toAnySlice(parts)}, nil // single child guaranteed by validator
	default:
		return nil, fmt.Errorf("mongo: unknown logical operator %q", cond.Op)
	}
}

// mongoComparison renders one predicate as a {field: expr} document.
//
// A field declared inside an array of sub-documents (elemMatch) is rendered
// against its path *relative* to one element and then wrapped in $elemMatch, so
// the predicate is anchored to a single element rather than to the array as a
// whole. Sibling predicates on the same array are folded together later, in
// mergeAnd — that is what makes "sku ABC costing over 100" mean one item.
func mongoComparison(c *Config, cond *Condition, now time.Time) (map[string]any, error) {
	arrayPath, rel, nested := c.MongoElemMatch(cond.Field)
	if nested {
		doc, err := mongoPredicate(c, cond, rel, now) // keyed relative to the element
		if err != nil {
			return nil, err
		}
		return map[string]any{arrayPath: map[string]any{"$elemMatch": doc}}, nil
	}
	return mongoPredicate(c, cond, c.PhysicalName(cond.Field, "mongo"), now)
}

// mongoPredicate renders one comparison against an explicit filter key. The key
// is passed in rather than derived so the same operator logic serves both a
// top-level path ("items.sku") and an $elemMatch-relative one ("sku").
func mongoPredicate(c *Config, cond *Condition, field string, now time.Time) (map[string]any, error) {

	// Determine the element type (for arrays this is the item type) so date
	// array elements can be converted to real Dates.
	elemType := FieldString
	if f, ok := c.FieldByName(cond.Field); ok {
		elemType = f.Type
		if f.Type == FieldArray {
			elemType = f.ItemType
			if elemType == "" {
				elemType = FieldString
			}
		}
	}

	switch cond.Operator {
	case OpIsNull:
		return map[string]any{field: nil}, nil
	case OpIsNotNull:
		return map[string]any{field: map[string]any{"$ne": nil}}, nil
	case OpEquals:
		return map[string]any{field: mongoScalar(cond.Value, now)}, nil
	case OpNotEquals:
		return expr(field, "$ne", mongoScalar(cond.Value, now)), nil
	case OpGt:
		return expr(field, "$gt", mongoScalar(cond.Value, now)), nil
	case OpLt:
		return expr(field, "$lt", mongoScalar(cond.Value, now)), nil
	case OpGte:
		return expr(field, "$gte", mongoScalar(cond.Value, now)), nil
	case OpLte:
		return expr(field, "$lte", mongoScalar(cond.Value, now)), nil
	case OpAfter:
		return expr(field, "$gte", mongoScalar(cond.Value, now)), nil // inclusive, per design doc
	case OpBefore:
		return expr(field, "$lte", mongoScalar(cond.Value, now)), nil
	case OpBetween:
		elems, _ := cond.Value.AsSlice()
		conv := mongoElems(elems, elemType)
		return map[string]any{field: map[string]any{"$gte": conv[0], "$lte": conv[1]}}, nil
	case OpIn:
		elems, _ := cond.Value.AsSlice()
		return expr(field, "$in", mongoElems(elems, elemType)), nil
	case OpNotIn:
		elems, _ := cond.Value.AsSlice()
		return expr(field, "$nin", mongoElems(elems, elemType)), nil
	case OpContainsAll:
		elems, _ := cond.Value.AsSlice()
		return expr(field, "$all", mongoElems(elems, elemType)), nil
	case OpContainsAny:
		elems, _ := cond.Value.AsSlice()
		return expr(field, "$in", mongoElems(elems, elemType)), nil
	case OpContains:
		// Array field: membership. String field: case-insensitive substring.
		if f, ok := c.FieldByName(cond.Field); ok && f.Type == FieldArray {
			return map[string]any{field: mongoScalar(cond.Value, now)}, nil
		}
		s, _ := cond.Value.AsString()
		return regexExpr(field, regexp.QuoteMeta(s), "i"), nil
	case OpStartsWith:
		s, _ := cond.Value.AsString()
		return regexExpr(field, "^"+regexp.QuoteMeta(s), ""), nil
	case OpEndsWith:
		s, _ := cond.Value.AsString()
		return regexExpr(field, regexp.QuoteMeta(s)+"$", ""), nil
	case OpRegex:
		s, _ := cond.Value.AsString()
		return regexExpr(field, s, ""), nil // raw pattern; policy denyRegexOn gates this upstream
	default:
		return nil, fmt.Errorf("mongo: unsupported operator %q", cond.Operator)
	}
}

// expr builds {field: {op: val}}.
func expr(field, op string, val any) map[string]any {
	return map[string]any{field: map[string]any{op: val}}
}

// regexExpr builds a $regex predicate, adding $options only when non-empty.
func regexExpr(field, pattern, options string) map[string]any {
	inner := map[string]any{"$regex": pattern}
	if options != "" {
		inner["$options"] = options
	}
	return map[string]any{field: inner}
}

// mergeAnd flattens AND children into one document when their top-level keys do
// not collide; on any collision it falls back to an explicit $and array.
// Sibling $elemMatch predicates on the same array are folded first, so they
// reach the collision check as a single key.
func mergeAnd(parts []map[string]any) map[string]any {
	parts = foldElemMatches(parts)
	acc := map[string]any{}
	for _, p := range parts {
		for k := range p {
			if _, exists := acc[k]; exists { // key collision (e.g. two predicates on the same field)
				list := make([]any, len(parts))
				for i, pp := range parts {
					list[i] = pp
				}
				return map[string]any{"$and": list}
			}
		}
		for k, v := range p {
			acc[k] = v
		}
	}
	return acc
}

// foldElemMatches merges the AND siblings that target the same array of
// sub-documents into one $elemMatch, and returns the parts unchanged when there
// is nothing to fold.
//
// This is the whole point of the elemMatch declaration. Left alone, two
// predicates on "items" render as two separate documents; ANDed, Mongo then
// satisfies each one independently and a document matches when element #1 has
// the sku and element #7 has the price. Folding them into a single $elemMatch
// forces both onto the same element, which is what the sentence said.
//
// A part qualifies only when it is exactly {path: {"$elemMatch": {…}}} — the
// shape mongoComparison produces — so nothing else in the tree is touched. The
// merged document keeps the position of the group's first member, preserving
// the index/priority ordering applied upstream.
func foldElemMatches(parts []map[string]any) []map[string]any {
	groups := map[string][]map[string]any{} // array path -> inner docs, in order
	for _, p := range parts {
		if path, inner, ok := elemMatchPart(p); ok {
			groups[path] = append(groups[path], inner)
		}
	}
	if len(groups) == 0 {
		return parts // no nested predicates: leave the slice exactly as it was
	}

	out := make([]map[string]any, 0, len(parts))
	done := map[string]bool{} // arrays already emitted, so each appears once
	for _, p := range parts {
		path, _, ok := elemMatchPart(p)
		if !ok {
			out = append(out, p) // ordinary predicate, passed through untouched
			continue
		}
		if done[path] {
			continue // already folded into the group's first occurrence
		}
		done[path] = true
		if len(groups[path]) == 1 {
			out = append(out, p) // single predicate on this array: nothing to merge
			continue
		}
		out = append(out, map[string]any{path: map[string]any{"$elemMatch": mergeElemDocs(groups[path])}})
	}
	return out
}

// elemMatchPart recognises a single-key {path: {"$elemMatch": doc}} document and
// returns its two halves. Anything else is reported as not an $elemMatch.
func elemMatchPart(p map[string]any) (path string, inner map[string]any, ok bool) {
	if len(p) != 1 {
		return "", nil, false
	}
	for k, v := range p {
		m, isMap := v.(map[string]any)
		if !isMap || len(m) != 1 {
			return "", nil, false
		}
		doc, isDoc := m["$elemMatch"].(map[string]any)
		if !isDoc {
			return "", nil, false
		}
		return k, doc, true
	}
	return "", nil, false
}

// mergeElemDocs merges several $elemMatch inner documents into one, so every
// predicate must hold for the same array element.
//
// Two predicates on the same relative key are combined when both are operator
// documents with disjoint operators ($gte + $lte becomes a range on one key).
// Anything else — two equality values, or the same operator twice — cannot be
// expressed as one key without discarding a predicate, so it is preserved in an
// $and *inside* the $elemMatch. That keeps the same-element guarantee even when
// the result is unsatisfiable, which is the honest compilation of a request
// like "sku is A and sku is B".
func mergeElemDocs(docs []map[string]any) map[string]any {
	acc := map[string]any{}
	var conflicts []any // predicates that cannot fold into acc without loss
	for _, d := range docs {
		for _, k := range sortedKeys(d) { // sorted: map order is random, output must not be
			v := d[k]
			prev, exists := acc[k]
			if !exists {
				acc[k] = v
				continue
			}
			if merged, ok := mergeOperatorDocs(prev, v); ok {
				acc[k] = merged
				continue
			}
			conflicts = append(conflicts, map[string]any{k: v})
		}
	}
	if len(conflicts) > 0 {
		acc["$and"] = conflicts
	}
	return acc
}

// mergeOperatorDocs combines {"$gte":100} and {"$lte":500} into one document.
// It refuses (ok=false) unless both sides are operator documents — every key
// starting with "$" — using disjoint operators, because merging a repeated
// operator would silently drop one of the two predicates.
func mergeOperatorDocs(a, b any) (map[string]any, bool) {
	am, ok := operatorDoc(a)
	if !ok {
		return nil, false
	}
	bm, ok := operatorDoc(b)
	if !ok {
		return nil, false
	}
	out := make(map[string]any, len(am)+len(bm))
	for k, v := range am {
		out[k] = v
	}
	for k, v := range bm {
		if _, dup := out[k]; dup { // same operator twice: not mergeable
			return nil, false
		}
		out[k] = v
	}
	return out, true
}

// operatorDoc reports whether v is a non-empty document whose every key is a
// Mongo operator ("$gt", "$in", …), returning it when so.
func operatorDoc(v any) (map[string]any, bool) {
	m, ok := v.(map[string]any)
	if !ok || len(m) == 0 {
		return nil, false
	}
	for k := range m {
		if !strings.HasPrefix(k, "$") {
			return nil, false
		}
	}
	return m, true
}

// sortedKeys returns a document's keys in lexical order, so every merge walks
// them in the same sequence on every run.
func sortedKeys(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// toAnySlice converts a slice of filter documents into []any for $or/$nor.
func toAnySlice(parts []map[string]any) []any {
	out := make([]any, len(parts))
	for i, p := range parts {
		out[i] = p
	}
	return out
}

// mongoScalar converts a scalar Value into its Mongo Go value. Dates (absolute
// and relative) become time.Time so the driver stores a real Date.
func mongoScalar(v *Value, now time.Time) any {
	switch v.Kind {
	case KindRelativeDate:
		return resolveRelative(now, v.Unit, v.Amount)
	case KindDate:
		s, _ := v.AsString()
		return parseDate(s)
	case KindNumber:
		f, _ := v.AsFloat()
		return f
	case KindBoolean:
		b, _ := v.AsBool()
		return b
	default: // string, enum
		s, _ := v.AsString()
		return s
	}
}

// mongoElems converts raw array elements, turning date strings into time.Time
// when the element type is date and leaving all others untouched.
func mongoElems(elems []any, elemType FieldType) []any {
	out := make([]any, len(elems))
	for i, e := range elems {
		if elemType == FieldDate {
			if s, ok := e.(string); ok {
				out[i] = parseDate(s)
				continue
			}
		}
		out[i] = e
	}
	return out
}

// parseDate parses common date layouts into a UTC time.Time, returning the
// original string when it matches none (so unexpected input degrades safely).
func parseDate(s string) any {
	for _, layout := range []string{time.RFC3339, "2006-01-02T15:04:05", "2006-01-02"} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.UTC()
		}
	}
	return s
}
