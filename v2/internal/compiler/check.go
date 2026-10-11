package compiler

import (
	"errors"
	"strconv"
	"strings"
	"time"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/graph"
	"queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
)

// This file resolves a plan against the schema and refuses what cannot be
// compiled faithfully. It is the compiler's half of section 16: the identifier
// allowlist (every name is looked up, never passed through), the operator and
// function allowlists (by field type and the field's own narrowing), the join
// allowlist (graph.Walk), and the complexity limits.
//
// It is also where the SQL traps of joining are caught. A JOIN through a
// to-many relationship repeats the root row once per related row, and every
// aggregate and every listing downstream of it silently changes meaning. The
// rules are in decision 015; each refusal below names the one it enforces.
//
// Error codes:
//
//	a name the config does not have        SECURITY_REJECTED (allowlist)
//	a limit exceeded                       SECURITY_REJECTED (section 16.7)
//	a value the field cannot hold          VALUE_RESOLUTION_FAILED
//	two readings the plan does not decide  AMBIGUOUS_MATCH
//	a shape SQL cannot answer faithfully   UNSUPPORTED_QUERY
//	a malformed plan                       COMPILATION_ERROR
//
// Names from the plan that are NOT configured are wrapped in qferr.Sensitive:
// they are input, not schema, and are shown only under --debug.

type field struct {
	path  graph.Path
	field *config.Field
}

func (f field) String() string { return f.field.Entity + "." + f.field.Name }

type condition struct {
	kind   plan.ConditionKind
	field  field // compare, range
	path   graph.Path
	op     config.Operator
	values []plan.Value
	start  time.Time
	end    time.Time
	negate bool
}

type aggregate struct {
	fn    config.Aggregation
	field *field // nil for count of rows
	path  graph.Path
}

type having struct {
	agg   aggregate
	op    config.Operator
	value plan.Value
}

type orderKey struct {
	field *field
	agg   int // index into aggregates, or -1
	desc  bool
}

// checked is a plan whose every name resolved and whose shape compiles.
type checked struct {
	root     *config.Entity
	distinct bool
	selects  []field
	aggs     []aggregate
	where    []condition
	whereOr  bool
	having   []having
	havingOr bool
	order    []orderKey
	limit    int
	// domain is the one to-many path the output ranges over, if any: the
	// prefix of a projection path through its last to-many step.
	domain     *graph.Path
	security   config.SecurityPolicy
	projection []graph.Path
}

func fail(code qferr.Code, format string, args ...any) error {
	return qferr.New(code, format, args...)
}

func check(p plan.Plan, reg *config.SchemaRegistry, g *graph.Graph) (*checked, error) {
	root, ok := reg.Entity(p.Root)
	if !ok {
		return nil, fail(qferr.CodeSecurityRejected, "the plan's root is not a configured entity (%s)", qferr.Sensitive(p.Root))
	}
	c := &checked{root: root, distinct: p.Distinct, limit: p.Limit, security: reg.Security()}
	r := resolver{reg: reg, g: g, root: root.Name}

	var err error
	if c.whereOr, err = isOr(p.WhereJoin, "whereJoin"); err != nil {
		return nil, err
	}
	if c.havingOr, err = isOr(p.HavingJoin, "havingJoin"); err != nil {
		return nil, err
	}

	// Section 16.7: the filter limit counts every condition the query applies.
	if max := c.security.MaxFilters; max > 0 && len(p.Where)+len(p.Having) > max {
		return nil, fail(qferr.CodeSecurityRejected,
			"the plan has %d conditions, over the limit of %d", len(p.Where)+len(p.Having), max)
	}

	// --- SELECT --------------------------------------------------------------
	seen := map[string]bool{}
	for i, s := range p.Select {
		f, err := r.field(s.Field, s.Via, at("select", i))
		if err != nil {
			return nil, err
		}
		if !f.field.IsSelectable() {
			return nil, fail(qferr.CodeSecurityRejected, "select[%d]: %s is not selectable", i, f)
		}
		key := f.path.Key() + "#" + f.field.Name
		if seen[key] {
			return nil, fail(qferr.CodeCompilationError, "select[%d]: %s is selected twice", i, f)
		}
		seen[key] = true
		c.selects = append(c.selects, f)
		c.projection = append(c.projection, f.path)
	}

	// --- aggregates ----------------------------------------------------------
	for i, a := range p.Aggregates {
		agg, err := r.aggregate(a, at("aggregates", i))
		if err != nil {
			return nil, err
		}
		c.aggs = append(c.aggs, agg)
		c.projection = append(c.projection, agg.path)
	}
	if len(c.selects) == 0 && len(c.aggs) == 0 {
		return nil, fail(qferr.CodeCompilationError, "the plan selects nothing: give select or aggregates")
	}

	// --- WHERE ---------------------------------------------------------------
	for i, pc := range p.Where {
		cond, err := r.condition(pc, i)
		if err != nil {
			return nil, err
		}
		c.where = append(c.where, cond)
	}

	// --- HAVING --------------------------------------------------------------
	if len(p.Having) > 0 && len(c.aggs) == 0 {
		return nil, fail(qferr.CodeCompilationError, "having restricts groups, but the plan computes no aggregate")
	}
	for i, h := range p.Having {
		agg, err := r.aggregate(h.Aggregate, at("having", i)+".aggregate")
		if err != nil {
			return nil, err
		}
		if !isComparison(h.Op) {
			return nil, fail(qferr.CodeCompilationError, "having[%d]: an aggregate is compared with eq, neq, gt, gte, lt or lte, not %s", i, qferr.Sensitive(string(h.Op)))
		}
		if err := finiteNumber(h.Value, at("having", i)); err != nil {
			return nil, err
		}
		c.having = append(c.having, having{agg: agg, op: h.Op, value: h.Value})
		c.projection = append(c.projection, agg.path)
	}

	// --- ORDER BY ------------------------------------------------------------
	for i, o := range p.OrderBy {
		key := orderKey{agg: -1, desc: o.Desc}
		switch {
		case o.Aggregate != nil && o.Field == "":
			if *o.Aggregate < 0 || *o.Aggregate >= len(c.aggs) {
				return nil, fail(qferr.CodeCompilationError, "orderBy[%d]: aggregate index %d is not one of the plan's %d aggregates", i, *o.Aggregate, len(c.aggs))
			}
			key.agg = *o.Aggregate
		case o.Aggregate == nil && o.Field != "":
			f, err := r.field(o.Field, o.Via, at("orderBy", i))
			if err != nil {
				return nil, err
			}
			if !f.field.IsSelectable() {
				// Sorting by a hidden column discloses its order, which is
				// exactly what hiding it was for.
				return nil, fail(qferr.CodeSecurityRejected, "orderBy[%d]: %s is not selectable, so it cannot order the output", i, f)
			}
			if (len(c.aggs) > 0 || c.distinct) && !seen[f.path.Key()+"#"+f.field.Name] {
				what := "the rows are grouped"
				if c.distinct {
					what = "the rows are distinct"
				}
				return nil, fail(qferr.CodeCompilationError, "orderBy[%d]: %s is not selected, and %s, so it cannot order them", i, f, what)
			}
			key.field = &f
			c.projection = append(c.projection, f.path)
		default:
			return nil, fail(qferr.CodeCompilationError, "orderBy[%d] must name exactly one of field or aggregate", i)
		}
		c.order = append(c.order, key)
	}

	// --- LIMIT ---------------------------------------------------------------
	if p.Limit < 0 {
		return nil, fail(qferr.CodeCompilationError, "limit %d is negative", p.Limit)
	}
	if p.Limit > 0 && len(c.order) == 0 {
		// Without an order, LIMIT returns whichever rows the database reaches
		// first — a different answer on every plan change. "Top N" must say
		// top by what.
		return nil, fail(qferr.CodeCompilationError, "limit without orderBy returns an arbitrary subset of rows")
	}
	if c.distinct && len(c.aggs) > 0 {
		return nil, fail(qferr.CodeCompilationError, "distinct applies to listings; grouped rows are already distinct")
	}

	if err := c.checkFanOut(); err != nil {
		return nil, err
	}
	return c, nil
}

// checkFanOut enforces decision 015's join rules on the output side (the
// "domain") and on conditions.
//
// DOMAIN. If anything shown, aggregated or sorted is reached through a
// to-many step, the query's rows are no longer the root's rows. That is
// correct only in one shape: every aggregate ranges over those related rows
// ("total order amount of customers from India"), and at most one such path
// exists. Everything else is refused, because the SQL would run and return a
// plausible, wrong number.
func (c *checked) checkFanOut() error {
	for _, p := range c.projection {
		last := lastFanOut(p)
		if last < 0 {
			continue
		}
		d := p.Prefix(last + 1)
		if c.domain != nil && c.domain.Key() != d.Key() {
			return fail(qferr.CodeUnsupportedQuery,
				"the output reaches many rows through both %s and %s; joining both would multiply each by the other", c.domain, d)
		}
		c.domain = &d
	}

	if c.domain != nil {
		d := *c.domain
		if len(c.aggs) == 0 {
			return fail(qferr.CodeUnsupportedQuery,
				"listing fields through %s would repeat each %s once per related row; root the query at %s instead",
				d, c.root.Name, d.To)
		}
		for i, a := range c.aggs {
			if err := c.overDomain(a, d, at("aggregates", i)); err != nil {
				return err
			}
		}
		for i, h := range c.having {
			if err := c.overDomain(h.agg, d, at("having", i)); err != nil {
				return err
			}
		}
	} else {
		for i, a := range c.aggs {
			if a.field == nil && a.path.Len() > 0 {
				return fail(qferr.CodeUnsupportedQuery,
					"aggregates[%d] counts %s rows, but each %s has at most one: count %s itself, or count a field",
					i, a.path.To, c.root.Name, c.root.Name)
			}
		}
	}

	// CONTRADICTION (added in Phase 7, finding M06). Two AND-joined equality
	// conditions on the SAME field, reached by the same to-one path, with
	// DIFFERENT values can never both hold for one row: the query would return
	// nothing, silently. In English this is nearly always "and" meaning
	// "either" ("customers from India and United States"). Refused rather
	// than rewritten to OR: which the user meant is theirs to say. Nothing
	// correct is lost, because the conjunction has no answers.
	if !c.whereOr {
		seenEq := map[string]plan.Value{}
		for i, w := range c.where {
			if w.kind != plan.Compare || w.op != config.OpEq || w.path.FanOut() >= 0 || len(w.values) != 1 {
				continue
			}
			key := w.path.Key() + "#" + w.field.field.Name
			if prev, ok := seenEq[key]; ok && !sameValue(prev, w.values[0]) {
				return fail(qferr.CodeAmbiguousMatch,
					"where[%d] requires %s to equal two different values at once, which no row can satisfy; if either value is meant, the request must say \"or\"",
					i, w.field)
			}
			seenEq[key] = w.values[0]
		}
	}

	// CONDITIONS. A condition through a to-many step means "some related row
	// satisfies it" and compiles to its own EXISTS. Two cases do not have one
	// reading, and are refused as ambiguous:
	//   - two AND-joined conditions through the same to-many step: "customers
	//     who bought laptops over 1000" — the SAME order, or any orders?
	//   - a condition through the domain's own to-many step: does it restrict
	//     the related rows being aggregated, or the root rows?
	type entry struct {
		key string
		idx int
	}
	var fanned []entry
	for i, w := range c.where {
		if w.kind != plan.Compare && w.kind != plan.Range {
			continue
		}
		first := w.path.FanOut()
		if first < 0 {
			continue
		}
		cp := w.path.Prefix(first + 1)
		if c.domain != nil && startsWith(*c.domain, cp) {
			return fail(qferr.CodeAmbiguousMatch,
				"where[%d] on %s passes through %s, which the output also aggregates over: it could restrict the related rows aggregated or the %s rows, and the plan does not say which",
				i, w.field, cp, c.root.Name)
		}
		if !c.whereOr {
			for _, e := range fanned {
				if e.key == cp.Key() {
					return fail(qferr.CodeAmbiguousMatch,
						"where[%d] and where[%d] both pass through %s: they could both hold for the SAME related row, or each for any related row, and the plan does not say which",
						e.idx, i, cp)
				}
			}
		}
		fanned = append(fanned, entry{cp.Key(), i})
	}
	return nil
}

func (c *checked) overDomain(a aggregate, d graph.Path, name string) error {
	if a.field == nil {
		if a.path.Key() != d.Key() {
			return fail(qferr.CodeUnsupportedQuery,
				"%s counts %s rows while the output ranges over %s rows through %s; the count would be multiplied",
				name, a.path.To, d.To, d)
		}
		return nil
	}
	if !startsWith(a.path, d) {
		return fail(qferr.CodeUnsupportedQuery,
			"%s aggregates %s, but the output ranges over %s rows through %s, so each %s value would be counted once per related row",
			name, *a.field, d.To, d, a.field.field.Entity)
	}
	return nil
}

func lastFanOut(p graph.Path) int {
	last := -1
	for i, s := range p.Steps {
		if s.ToMany {
			last = i
		}
	}
	return last
}

// startsWith reports whether p begins with every step of prefix.
func startsWith(p, prefix graph.Path) bool {
	if prefix.Len() > p.Len() || p.From != prefix.From {
		return false
	}
	return p.Prefix(prefix.Len()).Key() == prefix.Key()
}

func isOr(c plan.Connective, name string) (bool, error) {
	switch c {
	case "", plan.And:
		return false, nil
	case plan.Or:
		return true, nil
	}
	return false, fail(qferr.CodeCompilationError, "%s must be \"and\" or \"or\", not %s", name, qferr.Sensitive(string(c)))
}

func isComparison(op config.Operator) bool {
	switch op {
	case config.OpEq, config.OpNeq, config.OpGt, config.OpGte, config.OpLt, config.OpLte:
		return true
	}
	return false
}

func finiteNumber(v plan.Value, name string) error {
	if v.Kind != plan.NumberValue {
		return fail(qferr.CodeValueResolutionFailed, "%s needs a number, not a %s", name, v.Kind)
	}
	// The zero Decimal is not a number (decision 022 §2): a hand-built value
	// that names the kind and forgets the number is refused, never read as 0.
	if !v.Number.Valid() {
		return fail(qferr.CodeValueResolutionFailed, "%s is not a number", name)
	}
	return nil
}

// ---------------------------------------------------------------------------
// Name resolution
// ---------------------------------------------------------------------------

type resolver struct {
	reg  *config.SchemaRegistry
	g    *graph.Graph
	root string
}

// field resolves "entity.field" reached through via.
func (r resolver) field(name string, via []string, at string) (field, error) {
	entity, fname, ok := strings.Cut(name, ".")
	if !ok || entity == "" || fname == "" {
		return field{}, fail(qferr.CodeCompilationError, "%s: a field is written \"entity.field\" (%s)", at, qferr.Sensitive(name))
	}
	path, err := r.g.Walk(r.root, via)
	if err != nil {
		return field{}, located(at, err)
	}
	f, ok := r.reg.Field(entity, fname)
	if !ok {
		return field{}, fail(qferr.CodeSecurityRejected, "%s names a field that is not configured (%s)", at, qferr.Sensitive(name))
	}
	if path.To != entity {
		return field{}, fail(qferr.CodeCompilationError, "%s: %s belongs to %s, but its path from %s ends at %s", at, name, entity, r.root, path.To)
	}
	return field{path: path, field: f}, nil
}

func (r resolver) aggregate(a plan.Aggregate, at string) (aggregate, error) {
	if _, ok := sqlFunction(a.Func); !ok {
		return aggregate{}, fail(qferr.CodeSecurityRejected, "%s uses a function that is not in the allowlist (%s)", at, qferr.Sensitive(string(a.Func)))
	}
	switch {
	case a.Field != "" && a.Rows == "":
		f, err := r.field(a.Field, a.Via, at)
		if err != nil {
			return aggregate{}, err
		}
		if !hasAgg(f.field.AllowedAggregations(), a.Func) {
			return aggregate{}, fail(qferr.CodeUnsupportedQuery, "%s: %s is not allowed on %s (%s)", at, a.Func, f, f.field.Type)
		}
		if !f.field.IsSelectable() {
			// An aggregate of a hidden column discloses it (MIN of an id is
			// an id), so it follows the column's selectability.
			return aggregate{}, fail(qferr.CodeSecurityRejected, "%s: %s is not selectable, so it cannot be aggregated", at, f)
		}
		return aggregate{fn: a.Func, field: &f, path: f.path}, nil

	case a.Field == "" && a.Rows != "":
		if a.Func != config.AggCount {
			return aggregate{}, fail(qferr.CodeCompilationError, "%s: only count applies to rows; %s needs a field", at, a.Func)
		}
		path, err := r.g.Walk(r.root, a.Via)
		if err != nil {
			return aggregate{}, located(at, err)
		}
		if path.To != a.Rows {
			return aggregate{}, fail(qferr.CodeCompilationError, "%s counts %s rows, but its path from %s ends at %s", at, qferr.Sensitive(a.Rows), r.root, path.To)
		}
		return aggregate{fn: a.Func, path: path}, nil
	}
	return aggregate{}, fail(qferr.CodeCompilationError, "%s must name exactly one of field or rows", at)
}

func (r resolver) condition(pc plan.Condition, i int) (condition, error) {
	loc := at("where", i)
	c := condition{kind: pc.Kind, op: pc.Op, values: pc.Values, negate: pc.Negate}
	switch pc.Kind {
	case plan.Compare:
		f, err := r.field(pc.Field, pc.Via, loc)
		if err != nil {
			return c, err
		}
		c.field, c.path = f, f.path
		if pc.Negate {
			return c, fail(qferr.CodeCompilationError, "%s: a compare is negated by its operator (gt → lte), not by negate", loc)
		}
		if pc.Start != nil || pc.End != nil {
			return c, fail(qferr.CodeCompilationError, "%s: start and end belong to a range", loc)
		}
		if !f.field.IsFilterable() {
			return c, fail(qferr.CodeSecurityRejected, "%s: %s is not filterable", loc, f)
		}
		if _, ok := sqlOperator(pc.Op); !ok {
			return c, fail(qferr.CodeSecurityRejected, "%s uses an operator that is not in the allowlist (%s)", loc, qferr.Sensitive(string(pc.Op)))
		}
		if !hasOp(f.field.AllowedOperators(), pc.Op) {
			return c, fail(qferr.CodeUnsupportedQuery, "%s: %s does not allow %s", loc, f, pc.Op)
		}
		multi := pc.Op == config.OpIn || pc.Op == config.OpNin
		if len(pc.Values) == 0 || (!multi && len(pc.Values) != 1) {
			return c, fail(qferr.CodeCompilationError, "%s: %s takes %s, and the plan gives %d", loc, pc.Op, map[bool]string{true: "one or more values", false: "exactly one value"}[multi], len(pc.Values))
		}
		for _, v := range pc.Values {
			if err := valueFits(v, f, loc); err != nil {
				return c, err
			}
		}
		return c, nil

	case plan.Range:
		f, err := r.field(pc.Field, pc.Via, loc)
		if err != nil {
			return c, err
		}
		c.field, c.path = f, f.path
		if f.field.Type != config.TypeDate {
			return c, fail(qferr.CodeCompilationError, "%s: a range applies to a date field, and %s is %s", loc, f, f.field.Type)
		}
		if !f.field.IsFilterable() {
			return c, fail(qferr.CodeSecurityRejected, "%s: %s is not filterable", loc, f)
		}
		// A range is emitted as >= and <; the field must allow both.
		if !hasOp(f.field.AllowedOperators(), config.OpGte) || !hasOp(f.field.AllowedOperators(), config.OpLt) {
			return c, fail(qferr.CodeUnsupportedQuery, "%s: a range needs gte and lt, and %s does not allow both", loc, f)
		}
		if pc.Op != "" || len(pc.Values) > 0 {
			return c, fail(qferr.CodeCompilationError, "%s: a range takes start and end, not op or values", loc)
		}
		if pc.Start == nil || pc.End == nil || pc.Start.IsZero() || pc.End.IsZero() {
			return c, fail(qferr.CodeCompilationError, "%s: a range needs both start and end", loc)
		}
		if !pc.Start.Before(*pc.End) {
			return c, fail(qferr.CodeValueResolutionFailed, "%s: the range is empty (start is not before end)", loc)
		}
		c.start, c.end = *pc.Start, *pc.End
		return c, nil

	case plan.Exists, plan.Count:
		if pc.Field != "" || pc.Start != nil || pc.End != nil {
			return c, fail(qferr.CodeCompilationError, "%s: %s takes via only, not a field or a range", loc, pc.Kind)
		}
		path, err := r.g.Walk(r.root, pc.Via)
		if err != nil {
			return c, located(loc, err)
		}
		if path.Len() == 0 {
			return c, fail(qferr.CodeCompilationError, "%s: %s needs a relationship path", loc, pc.Kind)
		}
		c.path = path
		if pc.Kind == plan.Exists {
			if pc.Op != "" || len(pc.Values) > 0 {
				return c, fail(qferr.CodeCompilationError, "%s: exists takes no op or values", loc)
			}
			return c, nil
		}
		if pc.Negate {
			return c, fail(qferr.CodeCompilationError, "%s: a count is negated by its operator, not by negate", loc)
		}
		if path.FanOut() < 0 {
			return c, fail(qferr.CodeUnsupportedQuery, "%s counts through %s, which reaches loc most one row: the count is always 0 or 1", loc, path)
		}
		if !isComparison(pc.Op) {
			return c, fail(qferr.CodeCompilationError, "%s: a count is compared with eq, neq, gt, gte, lt or lte", loc)
		}
		if len(pc.Values) != 1 {
			return c, fail(qferr.CodeCompilationError, "%s: a count compares with exactly one number", loc)
		}
		if err := finiteNumber(pc.Values[0], loc); err != nil {
			return c, err
		}
		if n := pc.Values[0].Number; n.Sign() < 0 || !n.IsInteger() {
			return c, fail(qferr.CodeValueResolutionFailed, "%s: a count compares with a whole, non-negative number", loc)
		}
		return c, nil
	}
	return c, fail(qferr.CodeCompilationError, "%s: unknown condition kind (%s)", loc, qferr.Sensitive(string(pc.Kind)))
}

// valueFits checks a value against the field it is compared with.
func valueFits(v plan.Value, f field, at string) error {
	want := map[config.FieldType]plan.ValueKind{
		config.TypeNumber:  plan.NumberValue,
		config.TypeString:  plan.StringValue,
		config.TypeEnum:    plan.StringValue,
		config.TypeBoolean: plan.BoolValue,
		config.TypeDate:    plan.TimeValue,
	}[f.field.Type]
	if v.Kind != want {
		return fail(qferr.CodeValueResolutionFailed, "%s: %s is %s and needs a %s value, not a %s", at, f, f.field.Type, want, v.Kind)
	}
	switch v.Kind {
	case plan.NumberValue:
		if !v.Number.Valid() {
			return fail(qferr.CodeValueResolutionFailed, "%s: the value is not a number", at)
		}
	case plan.TimeValue:
		if v.Time.IsZero() {
			return fail(qferr.CodeValueResolutionFailed, "%s: the instant is unset", at)
		}
	case plan.StringValue:
		s := v.String.Reveal()
		if strings.ContainsRune(s, 0) {
			// PostgreSQL text cannot hold NUL; the driver would fail later
			// with a less useful error.
			return fail(qferr.CodeValueResolutionFailed, "%s: the value contains a NUL character", at)
		}
		// An enum's declared values are a closed set (Phase 1). A value
		// outside it cannot match anything, and passing it through would turn
		// a resolution failure into an empty result.
		if f.field.Type == config.TypeEnum && !contains(f.field.EnumValues, s) {
			return fail(qferr.CodeValueResolutionFailed, "%s: the value is not one of %s's declared values", at, f)
		}
	}
	return nil
}

func hasOp(ops []config.Operator, op config.Operator) bool {
	for _, o := range ops {
		if o == op {
			return true
		}
	}
	return false
}

func hasAgg(aggs []config.Aggregation, a config.Aggregation) bool {
	for _, x := range aggs {
		if x == a {
			return true
		}
	}
	return false
}

func contains(xs []string, s string) bool {
	for _, x := range xs {
		if x == s {
			return true
		}
	}
	return false
}

// at names a plan element the way a plan author reads the JSON: "where[2]".
func at(list string, i int) string { return list + "[" + strconv.Itoa(i) + "]" }

// located prefixes a graph refusal with the plan element it came from,
// keeping its code. The graph already redacted any unconfigured name.
func located(where string, err error) error {
	var qe *qferr.Error
	if errors.As(err, &qe) {
		return qferr.New(qe.Code, "%s: %s", where, qe.Message)
	}
	return qferr.Wrap(qferr.CodeInternal, err, "%s: unclassified path failure", where)
}

// sameValue compares two plan values of the same field.
func sameValue(a, b plan.Value) bool {
	if a.Kind != b.Kind {
		return false
	}
	switch a.Kind {
	case plan.NumberValue:
		// Exact: two values are the same number only if every digit agrees.
		// float64 equality called 99999999999999999999999 and 1e23 the same.
		return a.Number.Valid() && b.Number.Valid() && a.Number.Cmp(b.Number) == 0
	case plan.StringValue:
		return a.String.Reveal() == b.String.Reveal()
	case plan.BoolValue:
		return a.Bool == b.Bool
	case plan.TimeValue:
		return a.Time.Equal(b.Time)
	}
	return false
}
