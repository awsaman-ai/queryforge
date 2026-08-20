package queryforge

import (
	"fmt"
	"strings"
	"time"
)

// ESGenerator and OpenSearchGenerator compile an AST into an Elasticsearch/
// OpenSearch search request. The two share one compiler — esDialect is
// everything that differs between them, which today is nothing beyond the
// registry id, because the query-DSL surface this library emits (bool/
// filter, term/terms/range/match/prefix/wildcard/regexp/exists/nested, sort,
// _source, size/from) has not diverged between the two products. Adding a
// real divergence later (e.g. a newer ES-only query type) means adding a
// field to esDialect, not a second copy of the walk.
//
// Like every Generator, this is a pure function of its inputs: no network, no
// execution. The compiled ESQuery describes a GET .../_search request; it is
// the caller's job to send it.
type ESGenerator struct{}

// Backend returns the registry id for this generator.
func (ESGenerator) Backend() string { return "elasticsearch" }

// Generate builds the Elasticsearch search request from the AST.
func (ESGenerator) Generate(q *Query, c *Config, opts GenOptions) (*Result, error) {
	return generateES("elasticsearch", q, c, opts)
}

// OpenSearchGenerator is the same compiler targeting the "opensearch" backend
// id. See ESGenerator.
type OpenSearchGenerator struct{}

// Backend returns the registry id for this generator.
func (OpenSearchGenerator) Backend() string { return "opensearch" }

// Generate builds the OpenSearch search request from the AST.
func (OpenSearchGenerator) Generate(q *Query, c *Config, opts GenOptions) (*Result, error) {
	return generateES("opensearch", q, c, opts)
}

// ESQuery is the compiled Elasticsearch/OpenSearch request. Method and Path
// are convenience values a caller can use as-is (GET <Path> with Query/Sort/
// _source/size/from as the JSON body) or ignore in favour of Index and
// SourceType, which report exactly the same resolution in structured form.
type ESQuery struct {
	Method string   `json:"method"` // always "GET" — QueryForge is read-only by design
	Index  []string `json:"index"`  // resolved physical index/alias name(s)
	Path   string   `json:"path"`   // "/<index0>,<index1>/_search", ready to use

	// SourceType names which resolution mode produced Index, so a caller can
	// tell a concrete index from an alias or a business-rule match even
	// though every mode renders the same request shape.
	SourceType SourceType `json:"sourceType"`

	Query  map[string]any   `json:"query"`
	Sort   []map[string]any `json:"sort,omitempty"`
	Source any              `json:"_source,omitempty"` // []string projection; omitted = every field
	Size   int              `json:"size,omitempty"`
	From   int              `json:"from,omitempty"`
}

// generateES is the shared compiler behind both registry entries.
func generateES(backend string, q *Query, c *Config, opts GenOptions) (*Result, error) {
	now := opts.now()

	indexes, sourceType, err := resolveESIndex(q, c, backend, now)
	if err != nil {
		return nil, err
	}
	if len(indexes) == 0 {
		return nil, fmt.Errorf("generate %s: resolved no index to search", backend)
	}

	esq := &ESQuery{
		Method:     "GET",
		Index:      indexes,
		Path:       "/" + strings.Join(indexes, ",") + "/_search",
		SourceType: sourceType,
		Query:      map[string]any{"match_all": map[string]any{}},
	}

	if q.Filter != nil {
		query, err := esCondition(backend, c, q.Filter, now)
		if err != nil {
			return nil, err
		}
		esq.Query = query
	}

	// Projection: same "hide nothing by default, else an explicit allow-list"
	// rule as every other generator — see projectionFields.
	if fields, explicit := projectionFields(q, c); explicit {
		if len(fields) == 0 {
			return nil, fmt.Errorf("generate %s: no returnable fields: the config marks every field returnable:false, so the projection would be empty", backend)
		}
		phys := make([]string, len(fields))
		for i, name := range fields {
			phys[i] = c.PhysicalName(name, backend)
		}
		esq.Source = phys
	}

	for _, s := range q.Sort {
		order := "asc"
		if strings.EqualFold(s.Dir, "DESC") {
			order = "desc"
		}
		esq.Sort = append(esq.Sort, map[string]any{esExactPath(c, backend, s.Field): map[string]any{"order": order}})
	}

	esq.Size = effectiveLimit(q, c)
	esq.From = effectiveOffset(q)

	return &Result{Backend: backend, Doc: esq, Warnings: collectIndexWarnings(q, c)}, nil
}

// esExactPath is the physical path used by every operator except `contains`:
// a field's declared keyword sub-field when one exists, else its ordinary
// mapping. Equality, membership, prefix/wildcard/regex, range, sort, and
// exists all need an unanalyzed field to mean what they say.
func esExactPath(c *Config, backend, field string) string {
	if f, ok := c.FieldByName(field); ok {
		if kw, ok := f.KeywordMapping[backend]; ok && kw != "" {
			return kw
		}
	}
	return c.PhysicalName(field, backend)
}

// esTextPath is the analyzed path `contains` full-text search reads — a
// field's ordinary mapping, since KeywordMapping (when set) names the OTHER
// representation.
func esTextPath(c *Config, backend, field string) string {
	return c.PhysicalName(field, backend)
}

// esCondition dispatches on node type, returning one query clause.
func esCondition(backend string, c *Config, cond *Condition, now time.Time) (map[string]any, error) {
	switch cond.Type {
	case CondLogical:
		return esLogical(backend, c, cond, now)
	case CondComparison:
		return esComparisonWrapped(backend, c, cond, now)
	default:
		return nil, fmt.Errorf("%s: unknown condition type %q", backend, cond.Type)
	}
}

// esLogical renders AND/OR/NOT as a bool query. AND siblings that target the
// same "nested" object are folded into one nested query first — see
// foldNested — so that, like Mongo's ElemMatch, "sku ABC costing over 100"
// means one array element rather than any two.
func esLogical(backend string, c *Config, cond *Condition, now time.Time) (map[string]any, error) {
	// Arity is checked, not assumed — Generator is an exported interface and
	// DefaultRegistry an exported constructor, so a hand-built AST that never
	// passed Validate can reach here directly.
	if len(cond.Children) == 0 {
		return nil, fmt.Errorf("%s: logical %q has no children", backend, cond.Op)
	}
	if cond.Op == OpNOT && len(cond.Children) != 1 {
		return nil, fmt.Errorf("%s: NOT must have exactly one child, got %d", backend, len(cond.Children))
	}

	children := orderedChildren(c, cond.Children) // predicate ordering
	parts := make([]map[string]any, 0, len(children))
	for _, ch := range children {
		if ch == nil {
			return nil, fmt.Errorf("%s: logical %q has a nil child", backend, cond.Op)
		}
		m, err := esCondition(backend, c, ch, now)
		if err != nil {
			return nil, err
		}
		parts = append(parts, m)
	}

	switch cond.Op {
	case OpAND:
		return map[string]any{"bool": map[string]any{"filter": toAnySlice(foldNested(parts))}}, nil
	case OpOR:
		return map[string]any{"bool": map[string]any{"should": toAnySlice(parts), "minimum_should_match": 1}}, nil
	case OpNOT:
		boolQ := map[string]any{"must_not": toAnySlice(parts)}
		// A bare must_not matches a document that lacks the field entirely,
		// where SQL's `NOT (status = $1)` evaluates to NULL for a NULL column
		// and excludes the row. Requiring the field to exist restores that
		// reading — see esExistenceGuard for exactly when it applies and why
		// a NOT over a logical subtree is left alone, mirroring
		// mongoExistenceGuard in gen_mongo.go for the identical reason.
		if guard, ok := esExistenceGuard(backend, c, cond.Children[0]); ok {
			boolQ["filter"] = []any{guard}
		}
		return map[string]any{"bool": boolQ}, nil
	default:
		return nil, fmt.Errorf("%s: unknown logical operator %q", backend, cond.Op)
	}
}

// esExistenceGuard returns the {"exists": {"field": ...}} clause that makes a
// negated predicate agree with SQL's three-valued logic, when one applies.
// See mongoExistenceGuard for the full rationale; this is the same rule.
//
// It does not apply to isNull/isNotNull (already about absence), a NOT over a
// logical subtree (no single field to require), or a nested field (the
// nested object existing says nothing about the leaf inside it).
func esExistenceGuard(backend string, c *Config, child *Condition) (map[string]any, bool) {
	if child == nil || child.Type != CondComparison || isNullOperator(child.Operator) {
		return nil, false
	}
	if f, ok := c.FieldByName(child.Field); ok && f.NestedPath != "" {
		return nil, false
	}
	return map[string]any{"exists": map[string]any{"field": esExactPath(c, backend, child.Field)}}, true
}

// esNegated wraps clause in must_not, with an exists filter on field
// alongside it — the same "missing field must not satisfy the negation" fix
// esExistenceGuard applies at the logical-NOT level, needed here too because
// notEquals/notIn are negations that never pass through esLogical at all.
func esNegated(field string, clause map[string]any) map[string]any {
	return map[string]any{"bool": map[string]any{
		"must_not": []any{clause},
		"filter":   []any{map[string]any{"exists": map[string]any{"field": field}}},
	}}
}

// esComparisonWrapped renders one predicate and, when its field lives inside
// a "nested" object/array (Field.NestedPath), wraps it in a nested query.
// Unlike Mongo's ElemMatch, ES's nested query addresses the field by its
// FULL path even from inside the wrapper, so no relative-path translation is
// needed here — only the wrapping.
func esComparisonWrapped(backend string, c *Config, cond *Condition, now time.Time) (map[string]any, error) {
	clause, err := esComparison(backend, c, cond, now)
	if err != nil {
		return nil, err
	}
	f, ok := c.FieldByName(cond.Field)
	if !ok || f.NestedPath == "" {
		return clause, nil
	}
	return map[string]any{"nested": map[string]any{"path": f.NestedPath, "query": clause}}, nil
}

// esNestedPart recognises a single-key {"nested": {"path": ..., "query": ...}}
// clause and returns its two halves.
func esNestedPart(m map[string]any) (path string, query map[string]any, ok bool) {
	if len(m) != 1 {
		return "", nil, false
	}
	inner, isMap := m["nested"].(map[string]any)
	if !isMap {
		return "", nil, false
	}
	p, ok1 := inner["path"].(string)
	q, ok2 := inner["query"].(map[string]any)
	if !ok1 || !ok2 {
		return "", nil, false
	}
	return p, q, true
}

// foldNested merges AND siblings that target the same nested path into one
// nested query wrapping a bool/filter of their inner clauses, and returns the
// parts unchanged when there is nothing to fold. See NestedPath's doc
// comment for why this matters — it is the same shape as gen_mongo.go's
// foldElemMatches, simplified because ES nested queries need no relative-path
// bookkeeping.
func foldNested(parts []map[string]any) []map[string]any {
	groups := map[string][]map[string]any{}
	for _, p := range parts {
		if path, query, ok := esNestedPart(p); ok {
			groups[path] = append(groups[path], query)
		}
	}
	if len(groups) == 0 {
		return parts
	}

	out := make([]map[string]any, 0, len(parts))
	done := map[string]bool{}
	for _, p := range parts {
		path, _, ok := esNestedPart(p)
		if !ok {
			out = append(out, p) // ordinary predicate, passed through untouched
			continue
		}
		if done[path] {
			continue // already folded into the group's first occurrence
		}
		done[path] = true
		if len(groups[path]) == 1 {
			out = append(out, p) // single predicate on this path: nothing to merge
			continue
		}
		out = append(out, map[string]any{"nested": map[string]any{
			"path":  path,
			"query": map[string]any{"bool": map[string]any{"filter": toAnySlice(groups[path])}},
		}})
	}
	return out
}

// esComparison renders one predicate as a query clause, choosing the exact
// (keyword) path for every operator except `contains`, which uses the text
// path — see esExactPath/esTextPath.
func esComparison(backend string, c *Config, cond *Condition, now time.Time) (map[string]any, error) {
	elemType := condScalarType(c, cond)
	vc := c.valueCaseFor(cond.Field)
	// Reachable straight through the exported Generator with a hand-built
	// AST that never saw validation, same caveat as mongoPredicate — so the
	// type is re-checked rather than trusted.
	ci := elemType == FieldString && c.caseInsensitiveFor(cond.Field)
	exact := esExactPath(c, backend, cond.Field)
	text := esTextPath(c, backend, cond.Field)

	scalar := func() (any, error) { return esValue(cond.Value, elemType, now, vc) }

	switch cond.Operator {
	case OpIsNull:
		return map[string]any{"bool": map[string]any{
			"must_not": []any{map[string]any{"exists": map[string]any{"field": exact}}},
		}}, nil
	case OpIsNotNull:
		return map[string]any{"exists": map[string]any{"field": exact}}, nil
	case OpEquals:
		v, err := scalar()
		if err != nil {
			return nil, err
		}
		return esTerm(exact, v, ci), nil
	case OpNotEquals:
		v, err := scalar()
		if err != nil {
			return nil, err
		}
		// $exists-equivalent guard, same reason as esExistenceGuard: a bare
		// must_not matches a document missing the field, where SQL's
		// `status <> $1` evaluates to NULL (and excludes the row) for a NULL
		// status.
		return esNegated(exact, esTerm(exact, v, ci)), nil
	case OpGt, OpLt, OpGte, OpLte:
		v, err := scalar()
		if err != nil {
			return nil, err
		}
		return map[string]any{"range": map[string]any{exact: map[string]any{esRangeOp(cond.Operator): v}}}, nil
	case OpAfter: // inclusive, per design doc — matches every other generator
		v, err := scalar()
		if err != nil {
			return nil, err
		}
		return map[string]any{"range": map[string]any{exact: map[string]any{"gte": v}}}, nil
	case OpBefore:
		v, err := scalar()
		if err != nil {
			return nil, err
		}
		return map[string]any{"range": map[string]any{exact: map[string]any{"lte": v}}}, nil
	case OpBetween:
		elems, err := sliceValue(backend, OpBetween, cond.Value, 2)
		if err != nil {
			return nil, err
		}
		conv := esElems(elems, elemType, vc)
		return map[string]any{"range": map[string]any{exact: map[string]any{"gte": conv[0], "lte": conv[1]}}}, nil
	case OpIn:
		elems, err := sliceValue(backend, OpIn, cond.Value, 0)
		if err != nil {
			return nil, err
		}
		return esTerms(exact, esElems(elems, elemType, vc), ci), nil
	case OpNotIn:
		elems, err := sliceValue(backend, OpNotIn, cond.Value, 0)
		if err != nil {
			return nil, err
		}
		// Same NULL-semantics correction as notEquals: $nin-equivalent matches
		// a missing field, `NOT IN` does not match a NULL column.
		return esNegated(exact, esTerms(exact, esElems(elems, elemType, vc), ci)), nil
	case OpContainsAll:
		elems, err := sliceValue(backend, OpContainsAll, cond.Value, 0)
		if err != nil {
			return nil, err
		}
		conv := esElems(elems, elemType, vc)
		filters := make([]any, len(conv))
		for i, e := range conv {
			filters[i] = esTerm(exact, e, ci)
		}
		return map[string]any{"bool": map[string]any{"filter": filters}}, nil
	case OpContainsAny:
		elems, err := sliceValue(backend, OpContainsAny, cond.Value, 0)
		if err != nil {
			return nil, err
		}
		return esTerms(exact, esElems(elems, elemType, vc), ci), nil
	case OpContains:
		// Array field: membership (same reading as equality on a multi-valued
		// field). String field: full-text match against the analyzed path.
		if f, ok := c.FieldByName(cond.Field); ok && f.Type == FieldArray {
			v, err := scalar()
			if err != nil {
				return nil, err
			}
			return esTerm(exact, v, ci), nil
		}
		s, err := esString(cond, OpContains)
		if err != nil {
			return nil, err
		}
		return map[string]any{"match": map[string]any{text: vc.apply(s)}}, nil
	case OpStartsWith:
		s, err := esString(cond, OpStartsWith)
		if err != nil {
			return nil, err
		}
		return esPrefix(exact, vc.apply(s), ci), nil
	case OpEndsWith:
		// ES has no native suffix query; a leading wildcard is the correct
		// answer and the expensive one (it cannot use the term index), so the
		// caller is warned the same way a non-indexed filter is elsewhere in
		// this library — see collectIndexWarnings.
		s, err := esString(cond, OpEndsWith)
		if err != nil {
			return nil, err
		}
		return esWildcard(exact, "*"+esEscapeWildcard(vc.apply(s)), ci), nil
	case OpRegex:
		s, err := esString(cond, OpRegex)
		if err != nil {
			return nil, err
		}
		return esRegexp(exact, s, ci), nil // raw pattern; policy gates this upstream
	default:
		return nil, fmt.Errorf("%s: unsupported operator %q", backend, cond.Operator)
	}
}

// esTerm builds a "term" clause, adding case_insensitive when ci is set (a
// native parameter on term/prefix/wildcard/regexp since Elasticsearch 7.10 /
// the OpenSearch fork of it).
func esTerm(field string, value any, ci bool) map[string]any {
	if !ci {
		return map[string]any{"term": map[string]any{field: value}}
	}
	return map[string]any{"term": map[string]any{field: map[string]any{"value": value, "case_insensitive": true}}}
}

// esTerms builds a "terms" clause. The "terms" query has no case_insensitive
// parameter, unlike term/prefix/wildcard/regexp, so the case-insensitive form
// is rendered as an OR of case-insensitive term queries instead.
func esTerms(field string, values []any, ci bool) map[string]any {
	if !ci {
		return map[string]any{"terms": map[string]any{field: values}}
	}
	should := make([]any, len(values))
	for i, v := range values {
		should[i] = esTerm(field, v, true)
	}
	return map[string]any{"bool": map[string]any{"should": should, "minimum_should_match": 1}}
}

func esPrefix(field, value string, ci bool) map[string]any {
	inner := map[string]any{"value": value}
	if ci {
		inner["case_insensitive"] = true
	}
	return map[string]any{"prefix": map[string]any{field: inner}}
}

func esWildcard(field, pattern string, ci bool) map[string]any {
	inner := map[string]any{"value": pattern}
	if ci {
		inner["case_insensitive"] = true
	}
	return map[string]any{"wildcard": map[string]any{field: inner}}
}

func esRegexp(field, pattern string, ci bool) map[string]any {
	inner := map[string]any{"value": pattern}
	if ci {
		inner["case_insensitive"] = true
	}
	return map[string]any{"regexp": map[string]any{field: inner}}
}

// esEscapeWildcard escapes the two characters a wildcard query treats
// specially ('*' and '?'), so a literal value used to build a wildcard
// pattern (endsWith) cannot smuggle in wildcard syntax of its own.
func esEscapeWildcard(s string) string {
	r := strings.NewReplacer(`\`, `\\`, `*`, `\*`, `?`, `\?`)
	return r.Replace(s)
}

func esRangeOp(op Operator) string {
	switch op {
	case OpGt:
		return "gt"
	case OpGte:
		return "gte"
	case OpLt:
		return "lt"
	default: // OpLte
		return "lte"
	}
}

// esString reads a predicate's operand as text, which the pattern-building
// operators require. The validator guarantees it on the Engine path; this is
// the exported path's own check.
func esString(cond *Condition, op Operator) (string, error) {
	if cond.Value == nil {
		return "", fmt.Errorf("elasticsearch/opensearch: operator %q requires a value", op)
	}
	s, ok := cond.Value.AsString()
	if !ok {
		return "", fmt.Errorf("elasticsearch/opensearch: operator %q requires a string value", op)
	}
	return s, nil
}

// esValue converts a scalar Value into its Elasticsearch/OpenSearch literal,
// reading the payload as the type the config declares rather than the kind
// tag the model wrote — same rule every generator in this package follows
// (see valuetype.go). Dates render as RFC3339 strings, which both products
// parse under their default "strict_date_optional_time||epoch_millis" format.
func esValue(v *Value, declared FieldType, now time.Time, vc ValueCase) (any, error) {
	if v == nil {
		return nil, fmt.Errorf("elasticsearch/opensearch: predicate requires a value")
	}
	if v.Kind == KindRelativeDate {
		t, err := resolveRelative(now, v.Unit, v.Amount)
		if err != nil {
			return nil, fmt.Errorf("elasticsearch/opensearch: %w", err)
		}
		return t.Format(time.RFC3339), nil
	}
	switch declared {
	case FieldDate:
		s, _ := v.AsString()
		if t, ok := parseDate(s).(time.Time); ok {
			return t.Format(time.RFC3339), nil
		}
		return s, nil
	case FieldNumber:
		f, _ := v.AsFloat()
		return f, nil
	case FieldBoolean:
		b, _ := v.AsBool()
		return b, nil
	default: // string, enum
		s, _ := v.AsString()
		return vc.apply(s), nil
	}
}

// esElems converts raw array elements the same way esValue converts a
// scalar: dates become RFC3339 strings, and the field's value-case rule
// folds every remaining string.
func esElems(elems []any, elemType FieldType, vc ValueCase) []any {
	out := make([]any, len(elems))
	for i, e := range elems {
		if elemType == FieldDate {
			if s, ok := e.(string); ok {
				if t, ok := parseDate(s).(time.Time); ok {
					out[i] = t.Format(time.RFC3339)
					continue
				}
			}
		}
		out[i] = vc.applyAny(e)
	}
	return out
}
