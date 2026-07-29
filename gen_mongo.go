package queryforge

import (
	"fmt"
	"regexp"
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
func mongoComparison(c *Config, cond *Condition, now time.Time) (map[string]any, error) {
	field := c.PhysicalName(cond.Field, "mongo")

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
func mergeAnd(parts []map[string]any) map[string]any {
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
