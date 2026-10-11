package plan

import (
	"time"

	"queryforge.local/v2experiment/internal/binding"
	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/qferr"
)

// Clock is the request's notion of now. Dates are resolved against it, never
// against the wall clock inside the planner, so a plan is a pure function of
// (binding, config, clock) and a test can freeze all three.
type Clock struct {
	Now time.Time
	// Location is the timezone calendar periods are cut in. Nil means UTC.
	Location *time.Location
}

// FromBinding plans a bound sentence.
//
// The planner's contract is ACCOUNTING: every element of the binding —
// each condition, aggregate, pattern, selection, and every attribute of each
// value — either becomes part of the plan or causes a refusal. Nothing is
// dropped because the plan has no slot for it. TestPlannerAccountsForEveryBindingField
// pins that contract against the binding's struct definitions, so a field
// added to binding later fails the test until the planner decides what it
// means.
//
// The refusals it adds on top of binding's (decision 015):
//
//   - a number carrying a currency, a percent sign, or a unit word, compared
//     with a field: the config says nothing about the field's unit, so the
//     plan cannot say what "$500" or "20%" or "500 rupees" means in stored
//     terms — VALUE_RESOLUTION_FAILED. (A unit after a COUNT is kept, as
//     decision 014 set: it cannot change how many rows exist.)
//   - a negated condition through a to-many path: "customers whose order
//     amount is not above 10000" is either "no order above 10000" or "some
//     order not above 10000" — AMBIGUOUS_MATCH.
//   - a top_n pattern whose config declares no order — CONFIG_ERROR.
//   - a pattern whose intent has no planning rule — UNSUPPORTED_QUERY.
func FromBinding(b binding.Binding, reg *config.SchemaRegistry, clock Clock) (Plan, error) {
	if reg == nil {
		return Plan{}, qferr.New(qferr.CodeInternal, "planning needs a registry")
	}
	p := Plan{Root: b.Root}
	pl := planner{reg: reg, clock: clock}

	// Conditions, in sentence order.
	for i, c := range b.Conditions {
		if i > 0 {
			join := Connective(c.Join)
			if join != And && join != Or {
				return Plan{}, qferr.New(qferr.CodeInternal, "condition %d joins with %q", i, c.Join)
			}
			if i > 1 && join != p.WhereJoin {
				// Binding refuses this already; a plan cannot express it.
				return Plan{}, qferr.New(qferr.CodeUnsupportedQuery,
					"the conditions mix \"and\" and \"or\", and a plan does not guess precedence")
			}
			p.WhereJoin = join
		}
		cond, err := pl.condition(c)
		if err != nil {
			return Plan{}, err
		}
		p.Where = append(p.Where, cond)
	}

	for _, a := range b.Aggregates {
		agg := Aggregate{Func: a.Func, Via: a.Path.Names()}
		if a.Field != nil {
			agg.Field = a.Field.String()
		} else {
			agg.Rows = a.Entity
		}
		p.Aggregates = append(p.Aggregates, agg)
	}

	if b.Pattern != nil {
		if err := pl.pattern(*b.Pattern, &p); err != nil {
			return Plan{}, err
		}
	}

	seen := map[string]bool{}
	for _, s := range b.Selections {
		f := FieldPath{Field: s.Field.String(), Via: s.Path.Names()}
		if key := f.String(); !seen[key] {
			seen[key] = true
			p.Select = append(p.Select, f)
		}
	}
	// A listing that names no column shows the root's selectable fields, in
	// the order the config declares them. That is the "configured query
	// policy" section 15 leaves open: the config's own selectable flags decide,
	// not a list in Go.
	if len(p.Select) == 0 && len(p.Aggregates) == 0 {
		for _, f := range reg.Config().Fields {
			if f.Entity == b.Root && f.IsSelectable() {
				p.Select = append(p.Select, FieldPath{Field: f.Entity + "." + f.Name})
			}
		}
		if len(p.Select) == 0 {
			return Plan{}, qferr.New(qferr.CodeUnsupportedQuery,
				"%s has no selectable field, so a listing of it would show nothing", b.Root)
		}
	}

	p.Notes = append(p.Notes, b.Notes...)

	// Every join the binding recorded must be used by something in the plan.
	// Joins are derived from the elements above, so an unused one means an
	// element was lost between binding and plan — an engine bug, reported as
	// one rather than compiled around.
	if err := checkJoinsAccounted(b, p); err != nil {
		return Plan{}, err
	}
	return p, nil
}

type planner struct {
	reg   *config.SchemaRegistry
	clock Clock
}

func (pl planner) condition(c binding.Condition) (Condition, error) {
	via := c.Path.Names()
	fanOut := c.Path.ToMany()

	switch c.Kind {
	case binding.CondExists:
		return Condition{Kind: Exists, Via: via, Negate: c.Negated}, nil

	case binding.CondCount:
		// The unit after a count ("3 times") is decision 014's one exception:
		// kept, not interpreted, and already noted by binding. The plan keeps
		// that note. Currency and percent never reach here: binding refuses
		// them on a count.
		return Condition{Kind: Count, Via: via, Op: c.Op, Values: []Value{Num(c.Value.Number)}}, nil

	case binding.CondWindow:
		if c.Negated && fanOut {
			return Condition{}, negatedFanOut(c)
		}
		start, end, err := pl.resolve(c.Value)
		if err != nil {
			return Condition{}, err
		}
		return Condition{Kind: Range, Field: c.Field.String(), Via: via, Start: &start, End: &end, Negate: c.Negated}, nil

	case binding.CondCompare:
		if c.Negated && fanOut {
			return Condition{}, negatedFanOut(c)
		}
		return pl.compare(c, via)
	}
	return Condition{}, qferr.New(qferr.CodeInternal, "condition kind %q has no planning rule", c.Kind)
}

func negatedFanOut(c binding.Condition) error {
	return qferr.New(qferr.CodeAmbiguousMatch,
		"the negated condition on %s reaches many rows per %s through %s: it can mean that NO related row matches or that SOME related row does not, and the sentence does not say which",
		c.Field, c.Path.From, c.Path)
}

func (pl planner) compare(c binding.Condition, via []string) (Condition, error) {
	cond := Condition{Kind: Compare, Field: c.Field.String(), Via: via, Op: c.Op}
	v := c.Value
	switch v.Kind {
	case binding.ValueNumber:
		// Each of these is something the user SAID about the number that the
		// plan could not carry into the query. Compiling the bare number would
		// answer a different question, so the sentence is refused.
		switch {
		case v.Currency != "":
			return cond, qferr.New(qferr.CodeValueResolutionFailed,
				"the value compared with %s carries a currency, and the config does not say which currency %s is stored in", c.Field, c.Field)
		case v.Percent:
			return cond, qferr.New(qferr.CodeValueResolutionFailed,
				"the value compared with %s is a percentage, and the config does not say whether %s stores a fraction or a percent", c.Field, c.Field)
		case v.Unit != "":
			return cond, qferr.New(qferr.CodeValueResolutionFailed,
				"the value compared with %s is followed by a unit word, and the config declares no unit for %s, so the value cannot be interpreted safely", c.Field, c.Field)
		}
		cond.Values = []Value{Num(v.Number)}

	case binding.ValueAlias, binding.ValueEnum, binding.ValueLiteral:
		cond.Values = []Value{Str(v.Stored.Reveal())}

	case binding.ValueDate:
		start, end, err := pl.resolve(v)
		if err != nil {
			return cond, err
		}
		// A comparison with a PERIOD, mapped onto its half-open bounds:
		//   after 2024      gt  → >= end      before 2024   lt  → < start
		//   from 2024 on    gte → >= start    up to 2024    lte → < end
		//   in 2024         eq  → [start, end)   not in 2024  neq → NOT [start, end)
		switch c.Op {
		case config.OpEq, config.OpNeq:
			return Condition{Kind: Range, Field: cond.Field, Via: via, Start: &start, End: &end,
				Negate: c.Op == config.OpNeq}, nil
		case config.OpGt:
			cond.Op, cond.Values = config.OpGte, []Value{Instant(end)}
		case config.OpGte:
			cond.Values = []Value{Instant(start)}
		case config.OpLt:
			cond.Values = []Value{Instant(start)}
		case config.OpLte:
			cond.Op, cond.Values = config.OpLt, []Value{Instant(end)}
		default:
			return cond, qferr.New(qferr.CodeUnsupportedQuery, "%s cannot compare a date with a period", c.Op)
		}
		// The mapping can change the operator (gt → gte). A field that allows
		// gt but not gte would then be compared with an operator its config
		// never allowed; refuse rather than emit it.
		if f, ok := pl.reg.Field(c.Field.Entity, c.Field.Field); ok && !hasOp(f.AllowedOperators(), cond.Op) {
			return cond, qferr.New(qferr.CodeUnsupportedQuery,
				"comparing %s with a period needs %s, which that field does not allow", c.Field, cond.Op)
		}

	default:
		return cond, qferr.New(qferr.CodeInternal, "value kind %q has no planning rule", v.Kind)
	}
	return cond, nil
}

// resolve turns a date descriptor into its half-open range with the request's
// clock. This is where Phase 3's "resolution happens at planning time" lands.
func (pl planner) resolve(v binding.Value) (time.Time, time.Time, error) {
	if v.Date == nil {
		return time.Time{}, time.Time{}, qferr.New(qferr.CodeInternal, "a date condition has no date")
	}
	if pl.clock.Now.IsZero() {
		return time.Time{}, time.Time{}, qferr.New(qferr.CodeInternal,
			"planning a date needs the request's clock, and none was given")
	}
	start, end := v.Date.Resolve(pl.clock.Now, pl.clock.Location)
	if !start.Before(end) {
		return time.Time{}, time.Time{}, qferr.New(qferr.CodeValueResolutionFailed, "the date resolves to an empty range")
	}
	// A window's count is exact (decision 022 §2), so "the last 2147483647
	// days" is a real period — one that begins millions of years ago. Such an
	// instant is outside what RFC 3339 and a timestamp parameter can carry;
	// it is refused rather than wrapped or clamped into a period nobody
	// asked for.
	if y := start.Year(); y < 1 || y > 9999 {
		return time.Time{}, time.Time{}, qferr.New(qferr.CodeValueResolutionFailed,
			"the period starts outside the years 1 to 9999, which a query parameter cannot carry")
	}
	if y := end.Year(); y < 1 || y > 9999 {
		return time.Time{}, time.Time{}, qferr.New(qferr.CodeValueResolutionFailed,
			"the period ends outside the years 1 to 9999, which a query parameter cannot carry")
	}
	return start, end, nil
}

func (pl planner) pattern(bp binding.Pattern, p *Plan) error {
	switch bp.Intent {
	case config.IntentTopN:
		var cfg *config.QueryPattern
		for _, qp := range pl.reg.QueryPatterns() {
			if qp.Name == bp.Name {
				cfg = qp
			}
		}
		if cfg == nil {
			return qferr.New(qferr.CodeInternal, "pattern %s vanished from the registry", bp.Name)
		}
		if cfg.Order == "" {
			return qferr.New(qferr.CodeConfigError,
				"query pattern %s ranks rows but declares no \"order\" (asc or desc); the direction is not guessed", bp.Name)
		}
		if bp.Metric == nil || bp.N < 1 {
			return qferr.New(qferr.CodeInternal, "pattern %s was bound without a metric or a count", bp.Name)
		}
		p.OrderBy = append(p.OrderBy, OrderKey{Field: bp.Metric.String(), Via: bp.Path.Names(), Desc: cfg.Order == config.OrderDesc})
		p.Limit = bp.N
		return nil

	case config.IntentTimeWindow:
		// Binding already turned the window into a condition, which the loop
		// in FromBinding planned. The pattern itself adds nothing further.
		return nil

	case config.IntentFilter:
		// A filter pattern marks the sentence as a request for matching rows,
		// which is what every plan without aggregates already is.
		return nil

	case config.IntentAggregate:
		// Binding attaches aggregations to their fields from aggregation
		// phrases; an aggregate PATTERN carries no function or field of its
		// own, so there is nothing it could mean in a plan.
		return qferr.New(qferr.CodeUnsupportedQuery,
			"query pattern %s has intent aggregate, which names no function or field; the plan cannot express it", bp.Name)
	}
	// count patterns are aggregates by the time binding is done; anything
	// else is a new intent this planner has not been taught.
	return qferr.New(qferr.CodeInternal, "pattern intent %q has no planning rule", bp.Intent)
}

func checkJoinsAccounted(b binding.Binding, p Plan) error {
	used := map[string]bool{}
	mark := func(via []string) {
		for n := 1; n <= len(via); n++ {
			used[joinKey(via[:n])] = true
		}
	}
	for _, c := range p.Where {
		mark(c.Via)
	}
	for _, a := range p.Aggregates {
		mark(a.Via)
	}
	for _, s := range p.Select {
		mark(s.Via)
	}
	for _, o := range p.OrderBy {
		mark(o.Via)
	}
	for _, j := range b.Joins {
		if !used[joinKey(j.Names())] {
			return qferr.New(qferr.CodeInternal,
				"the binding joins %s, but nothing in the plan uses that join: an element was lost in planning", j)
		}
	}
	return nil
}

func joinKey(via []string) string {
	k := ""
	for _, v := range via {
		k += v + "/"
	}
	return k
}

func hasOp(ops []config.Operator, op config.Operator) bool {
	for _, o := range ops {
		if o == op {
			return true
		}
	}
	return false
}
