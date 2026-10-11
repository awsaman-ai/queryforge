package plan_test

import (
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"queryforge.local/v2experiment/internal/binding"
	"queryforge.local/v2experiment/internal/compiler"
	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/graph"
	"queryforge.local/v2experiment/internal/nlp"
	// Dot import: these tests sit outside package plan only because they also
	// import the compiler, which imports plan. They read as if inside it.
	. "queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
)

// A frozen request clock: every date in these tests resolves against it.
var clock = Clock{Now: time.Date(2026, 9, 21, 12, 0, 0, 0, time.UTC), Location: time.UTC}

func planSentence(t *testing.T, sentence string, hits map[string][]hit) (Plan, error) {
	t.Helper()
	reg := registry(t, "commerce.json")
	b, err := bindReg(t, reg, sentence, hits)
	if err != nil {
		t.Fatalf("%q did not bind: %v", sentence, err)
	}
	return FromBinding(b, reg, clock)
}

// compileSentence is planSentence plus the compiler. It exists to check that
// the planner's plans are ones the compiler accepts. It is NOT the Phase 7
// pipeline: there is no CLI path from a sentence to SQL.
func compileSentence(t *testing.T, sentence string, hits map[string][]hit) (Plan, compiler.Query) {
	t.Helper()
	p, err := planSentence(t, sentence, hits)
	if err != nil {
		t.Fatalf("%q did not plan: %v", sentence, err)
	}
	reg := registry(t, "commerce.json")
	q, err := compiler.Compile(p, reg, graph.New(reg))
	if err != nil {
		t.Fatalf("%q: plan does not compile: %v\n%s", sentence, err, p.Explain(true))
	}
	return p, q
}

func wantSQL(t *testing.T, q compiler.Query, parts ...string) {
	t.Helper()
	for _, p := range parts {
		if !strings.Contains(q.SQL, p) {
			t.Errorf("SQL lacks %q:\n%s", p, q.SQL)
		}
	}
}

// ===========================================================================
// The brief's sentences, planned and compiled
// ===========================================================================

func TestPlanCustomersFromIndia(t *testing.T) {
	p, q := compileSentence(t, "customers from India", map[string][]hit{"customers": hCustomers, "india": hIndia})
	// No column named: the root's selectable fields, in config order.
	var sel []string
	for _, s := range p.Select {
		sel = append(sel, s.Field)
	}
	if got := strings.Join(sel, ","); got != "customer.name,customer.country,customer.status,customer.total_spent,customer.created_at" {
		t.Errorf("default select = %s", got)
	}
	wantSQL(t, q, `FROM "customers" AS "t0"`, `WHERE "t0"."country" = $1`)
	if q.Params[0].Value.String.Reveal() != "IN" {
		t.Errorf("the alias target is the parameter, got %q", q.Params[0].Value.String.Reveal())
	}
}

func TestPlanCustomersWhoBoughtLaptops(t *testing.T) {
	p, q := compileSentence(t, "customers who bought laptops", map[string][]hit{
		"customers": hCustomers, "bought": hBought, "laptops": hLaptops})
	if c := p.Where[0]; c.Field != "product.category" || strings.Join(c.Via, ",") != "customer_orders,order_product" {
		t.Errorf("plan:\n%s", p.Explain(true))
	}
	// Through a to-many path: a semi-join, never a row-multiplying JOIN.
	wantSQL(t, q, "WHERE EXISTS (", `FROM "orders" AS "t1"`, `JOIN "products" AS "t2"`, `"t2"."category" = $1`)
	if strings.Contains(q.SQL, "\nJOIN") || strings.Contains(q.SQL, "LEFT JOIN") {
		t.Errorf("the outer query must not join orders:\n%s", q.SQL)
	}
}

func TestPlanCustomersWhoOrderedMoreThan3Times(t *testing.T) {
	p, q := compileSentence(t, "customers who ordered more than 3 times", map[string][]hit{
		"customers": hCustomers, "ordered": hOrdered, "more than": hMoreThan})
	if c := p.Where[0]; c.Kind != Count || c.Op != config.OpGt || c.Values[0].Number.String() != "3" {
		t.Errorf("plan:\n%s", p.Explain(true))
	}
	// Decision 014's unit exception survives as a visible note, not silently.
	if !strings.Contains(strings.Join(p.Notes, "\n"), "unit word after the count") {
		t.Errorf("the unit note was lost: %v", p.Notes)
	}
	wantSQL(t, q, "SELECT COUNT(*)", `"t1"."customer_id" = "t0"."id"`, ") > $1")
}

func TestPlanCustomersWhoseOrderAmountIsAbove10000(t *testing.T) {
	_, q := compileSentence(t, "customers whose order amount is above 10000", map[string][]hit{
		"customers": hCustomers, "order amount": hOrderAmt, "above": hAbove})
	wantSQL(t, q, "WHERE EXISTS (", `"t1"."amount" > $1`)
}

func TestPlanTwoConditions(t *testing.T) {
	p, q := compileSentence(t, "customers from India who spent more than 50000", map[string][]hit{
		"customers": hCustomers, "india": hIndia, "spent": hSpent, "more than": hMoreThan})
	if len(p.Where) != 2 || p.WhereJoin != And {
		t.Fatalf("plan:\n%s", p.Explain(true))
	}
	wantSQL(t, q, `WHERE "t0"."country" = $1`, `  AND "t0"."total_spent" > $2`)
}

// top_n direction comes from the config's new "order" key (decision 015).
func TestPlanTopAndBottomN(t *testing.T) {
	_, q := compileSentence(t, "top 5 customers by spending", map[string][]hit{
		"top":       {{"pattern:top_n", 0.825, "top N"}, {"pattern:bottom_n", 0.4, "bottom N"}},
		"customers": hCustomers, "spending": {{"field:customer.total_spent", 1.0, "spending"}}})
	wantSQL(t, q, `ORDER BY "t0"."total_spent" DESC NULLS LAST`, "LIMIT 5;")

	_, q = compileSentence(t, "bottom 5 customers by spending", map[string][]hit{
		"bottom":    {{"pattern:bottom_n", 0.9, "bottom N"}, {"pattern:top_n", 0.4, "top N"}},
		"customers": hCustomers, "spending": {{"field:customer.total_spent", 1.0, "spending"}}})
	wantSQL(t, q, `ORDER BY "t0"."total_spent" ASC NULLS LAST`, "LIMIT 5;")
}

func TestPlanDateWindowResolvesAgainstTheClock(t *testing.T) {
	p, q := compileSentence(t, "orders last month", map[string][]hit{"orders": hOrders})
	c := p.Where[0]
	if c.Kind != Range || c.Field != "order.placed_at" {
		t.Fatalf("plan:\n%s", p.Explain(true))
	}
	if !c.Start.Equal(time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)) || !c.End.Equal(time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("last month under a 2026-09-21 clock = [%s, %s)", c.Start, c.End)
	}
	wantSQL(t, q, `("t0"."placed_at" >= $1 AND "t0"."placed_at" < $2)`)
}

func TestPlanAggregates(t *testing.T) {
	_, q := compileSentence(t, "how many customers from India", map[string][]hit{
		"how many":  {{"aggregation:count", 1.0, "how many"}, {"pattern:how_many", 1.0, "how many"}},
		"customers": hCustomers, "india": hIndia})
	wantSQL(t, q, "SELECT COUNT(*)\n", `WHERE "t0"."country" = $1`)

	_, q = compileSentence(t, "average order amount", map[string][]hit{
		"average": {{"aggregation:avg", 1.0, "average"}}, "order amount": hOrderAmt})
	wantSQL(t, q, `SELECT AVG("t0"."amount")`, `FROM "orders" AS "t0"`)
}

func TestPlanNegationOnTheRoot(t *testing.T) {
	_, q := compileSentence(t, "customers not from India", map[string][]hit{"customers": hCustomers, "india": hIndia})
	wantSQL(t, q, `"t0"."country" <> $1`)
}

// ===========================================================================
// Nothing the sentence said is silently discarded (the user's Phase 6 rule)
// ===========================================================================

// Each of these binds (Phase 5 keeps the number's currency, percent sign or
// unit word). Compiling the bare number would answer a different question,
// so the planner refuses. Before Phase 6 nothing consumed these attributes.
func TestPlanRefusesWhatTheNumberSaidAboutItself(t *testing.T) {
	spent := map[string][]hit{"customers": hCustomers, "spent": hSpent, "more than": hMoreThan}
	for sentence, want := range map[string]string{
		"customers who spent more than 500 dollars": "unit word",
		"customers who spent more than $500":        "currency",
		"customers who spent more than 500 USD":     "currency",
		"customers who spent more than 50%":         "percentage",
		// Decision 017: no symbol or unit is interpreted from common
		// knowledge. Until a field declares its unit, all are refused.
		"customers who spent more than ₹500":   "currency",
		"customers who spent more than €500":   "currency",
		"customers who spent more than 500 kg": "unit word",
	} {
		t.Run(sentence, func(t *testing.T) {
			_, err := planSentence(t, sentence, spent)
			if qferr.CodeOf(err) != qferr.CodeValueResolutionFailed || !strings.Contains(err.Error(), want) {
				t.Fatalf("err = %v, want VALUE_RESOLUTION_FAILED about the %s", err, want)
			}
		})
	}
}

// Phase 5's strict refusals still hold once a planner exists: the planner
// never sees these, because binding refuses them first.
func TestUnexplainedWordsStillRefusedBeforePlanning(t *testing.T) {
	reg := registry(t, "commerce.json")
	for sentence, hits := range map[string]map[string][]hit{
		"customers from Atlantis":        {"customers": hCustomers},
		"customers who bought something": {"customers": hCustomers, "bought": hBought},
	} {
		_, err := bindReg(t, reg, sentence, hits)
		if qferr.CodeOf(err) != qferr.CodeUnsupportedQuery {
			t.Errorf("%q: err = %v, want UNSUPPORTED_QUERY", sentence, err)
		}
	}
}

// "not above 10000" through orders: NO order above, or SOME order not above?
func TestPlanRefusesNegationThroughAToManyPath(t *testing.T) {
	_, err := planSentence(t, "customers whose order amount is not above 10000", map[string][]hit{
		"customers": hCustomers, "order amount": hOrderAmt, "above": hAbove})
	if qferr.CodeOf(err) != qferr.CodeAmbiguousMatch {
		t.Fatalf("err = %v, want AMBIGUOUS_MATCH", err)
	}
}

// Accounting, per sentence: every condition, aggregate and selection of the
// binding has its counterpart in the plan, and every plan condition has its
// predicate in the SQL. Counted, so an element dropped at either step fails.
func TestEveryBoundElementReachesTheSQL(t *testing.T) {
	cases := map[string]map[string][]hit{
		"customers from India who spent more than 50000": {"customers": hCustomers, "india": hIndia, "spent": hSpent, "more than": hMoreThan},
		"customers who bought laptops":                   {"customers": hCustomers, "bought": hBought, "laptops": hLaptops},
		"customers who ordered more than 3 times":        {"customers": hCustomers, "ordered": hOrdered, "more than": hMoreThan},
		"how many customers from India": {"how many": {{"aggregation:count", 1.0, "how many"}, {"pattern:how_many", 1.0, "how many"}},
			"customers": hCustomers, "india": hIndia},
		"orders last month": {"orders": hOrders},
	}
	reg := registry(t, "commerce.json")
	for sentence, hits := range cases {
		b := mustBind(t, "commerce.json", sentence, hits)
		p, err := FromBinding(b, reg, clock)
		if err != nil {
			t.Fatalf("%q: %v", sentence, err)
		}
		if len(p.Where) != len(b.Conditions) || len(p.Aggregates) != len(b.Aggregates) {
			t.Errorf("%q: binding %d conditions / %d aggregates, plan %d / %d", sentence,
				len(b.Conditions), len(b.Aggregates), len(p.Where), len(p.Aggregates))
		}
		q, err := compiler.Compile(p, reg, graph.New(reg))
		if err != nil {
			t.Fatalf("%q: %v", sentence, err)
		}
		// Each condition contributes exactly one top-level predicate line.
		// These prefixes match only at column 0: subquery lines are indented
		// further and never match them.
		top := strings.Count(q.SQL, "\nWHERE ") + strings.Count(q.SQL, "\n  AND ") + strings.Count(q.SQL, "\n   OR ")
		if top != len(p.Where) {
			t.Errorf("%q: %d plan conditions, %d top-level predicates:\n%s", sentence, len(p.Where), top, q.SQL)
		}
	}
}

// The tripwire. The planner's accounting is only as good as its knowledge of
// what a Binding can hold. If a later phase adds a field to any binding type,
// this fails until someone decides — in planner.go — what the planner does
// with it: plan it, refuse on it, or carry it as a note. A new field can never
// be silently ignored.
func TestPlannerAccountsForEveryBindingField(t *testing.T) {
	handled := map[reflect.Type][]string{
		reflect.TypeOf(binding.Binding{}): {"Root", "Conditions", "Aggregates", "Pattern", "Selections",
			"Joins",  // checked by checkJoinsAccounted
			"Notes"}, // carried into Plan.Notes
		reflect.TypeOf(binding.Condition{}): {"Kind", "Field", "Relationship", "Op", "Value", "Negated",
			"Inferred", // provenance only: the plan uses the field either way
			"Path", "Join",
			"Start", "End"}, // token positions, for errors
		reflect.TypeOf(binding.Value{}):     {"Kind", "Number", "Currency", "Percent", "Date", "Stored", "Unit"},
		reflect.TypeOf(binding.Aggregate{}): {"Func", "Field", "Entity", "Path"},
		reflect.TypeOf(binding.Pattern{}):   {"Name", "Intent", "N", "Metric", "Path"},
		reflect.TypeOf(binding.Selection{}): {"Field", "Path"},
		reflect.TypeOf(binding.FieldRef{}):  {"Entity", "Field", "Type"},
	}
	for typ, want := range handled {
		var got []string
		for i := 0; i < typ.NumField(); i++ {
			got = append(got, typ.Field(i).Name)
		}
		sort.Strings(got)
		sort.Strings(want)
		if !reflect.DeepEqual(got, want) {
			t.Errorf("%s has fields %v; the planner accounts for %v. Decide what the new field means in planner.go, then update this list.", typ, got, want)
		}
	}
}

// ===========================================================================
// Planner rules on hand-built bindings
// ===========================================================================

func rootPath(e string) graph.Path { return graph.Path{From: e, To: e} }

func dateField() binding.FieldRef {
	return binding.FieldRef{Entity: "order", Field: "placed_at", Type: config.TypeDate}
}

// A comparison with a PERIOD lands on the period's half-open bounds.
func TestDateComparisonsMapOntoHalfOpenBounds(t *testing.T) {
	reg := registry(t, "commerce.json")
	y2024 := &nlp.DateExpr{Anchor: nlp.AnchorAbsolute, Unit: nlp.UnitYear, Year: 2024}
	start, end := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	for _, c := range []struct {
		op     config.Operator
		kind   ConditionKind
		wantOp config.Operator
		at     time.Time
		negate bool
	}{
		{config.OpGt, Compare, config.OpGte, end, false},
		{config.OpGte, Compare, config.OpGte, start, false},
		{config.OpLt, Compare, config.OpLt, start, false},
		{config.OpLte, Compare, config.OpLt, end, false},
		{config.OpEq, Range, "", time.Time{}, false},
		{config.OpNeq, Range, "", time.Time{}, true},
	} {
		b := binding.Binding{Root: "order", Conditions: []binding.Condition{{Kind: binding.CondCompare, Field: dateField(),
			Op: c.op, Value: binding.Value{Kind: binding.ValueDate, Date: y2024}, Path: rootPath("order")}}}
		p, err := FromBinding(b, reg, clock)
		if err != nil {
			t.Fatalf("%s: %v", c.op, err)
		}
		w := p.Where[0]
		if w.Kind != c.kind || w.Negate != c.negate {
			t.Errorf("%s → %s negate=%v", c.op, w.Kind, w.Negate)
			continue
		}
		if c.kind == Compare && (w.Op != c.wantOp || !w.Values[0].Time.Equal(c.at)) {
			t.Errorf("%s → %s %s", c.op, w.Op, w.Values[0].Render(true))
		}
		if c.kind == Range && (!w.Start.Equal(start) || !w.End.Equal(end)) {
			t.Errorf("%s → [%s, %s)", c.op, w.Start, w.End)
		}
	}
}

// Calendar periods are cut in the request's timezone, not in UTC.
func TestPeriodsFollowTheClocksTimezone(t *testing.T) {
	reg := registry(t, "commerce.json")
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Skip("no tzdata")
	}
	this := &nlp.DateExpr{Anchor: nlp.AnchorCalendar, Unit: nlp.UnitMonth, Count: 1, Offset: 0}
	b := binding.Binding{Root: "order", Conditions: []binding.Condition{{Kind: binding.CondWindow, Field: dateField(),
		Value: binding.Value{Kind: binding.ValueDate, Date: this}, Path: rootPath("order")}}}
	p, err := FromBinding(b, reg, Clock{Now: clock.Now, Location: kolkata})
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 9, 1, 0, 0, 0, 0, kolkata); !p.Where[0].Start.Equal(want) {
		t.Errorf("this month in Kolkata starts %s, want %s", p.Where[0].Start, want)
	}
}

func TestPlanningADateNeedsAClock(t *testing.T) {
	b := binding.Binding{Root: "order", Conditions: []binding.Condition{{Kind: binding.CondWindow, Field: dateField(),
		Value: binding.Value{Kind: binding.ValueDate, Date: &nlp.DateExpr{Anchor: nlp.AnchorCalendar, Unit: nlp.UnitMonth, Count: 1}}, Path: rootPath("order")}}}
	// err == nil is checked explicitly: qferr.CodeOf(nil) is CodeInternal, so
	// the code comparison alone passes when nothing failed (found by mutation).
	if _, err := FromBinding(b, registry(t, "commerce.json"), Clock{}); err == nil || qferr.CodeOf(err) != qferr.CodeInternal {
		t.Errorf("err = %v", err)
	}
}

func TestTopNWithoutAConfiguredOrderIsRefused(t *testing.T) {
	reg := registry(t, "commerce.json")
	cfg := *reg.Config()
	cfg.QueryPatterns = append([]config.QueryPattern(nil), cfg.QueryPatterns...)
	for i := range cfg.QueryPatterns {
		cfg.QueryPatterns[i].Order = ""
	}
	noOrder := config.NewSchemaRegistry(&cfg)
	metric := binding.FieldRef{Entity: "customer", Field: "total_spent", Type: config.TypeNumber}
	b := binding.Binding{Root: "customer", Pattern: &binding.Pattern{Name: "top_n", Intent: config.IntentTopN, N: 5, Metric: &metric, Path: rootPath("customer")}}
	_, err := FromBinding(b, noOrder, clock)
	if qferr.CodeOf(err) != qferr.CodeConfigError || !strings.Contains(err.Error(), "order") {
		t.Errorf("err = %v, want CONFIG_ERROR naming the missing order", err)
	}
}

func TestAggregatePatternIsRefused(t *testing.T) {
	b := binding.Binding{Root: "customer", Pattern: &binding.Pattern{Name: "summary", Intent: config.IntentAggregate}}
	if _, err := FromBinding(b, registry(t, "commerce.json"), clock); qferr.CodeOf(err) != qferr.CodeUnsupportedQuery {
		t.Errorf("err = %v", err)
	}
}

// A join the binding recorded that no element uses means an element was lost
// between binding and plan.
func TestLostJoinIsAnInternalError(t *testing.T) {
	g := graph.New(registry(t, "commerce.json"))
	p, _ := g.Walk("customer", []string{"customer_orders"})
	b := binding.Binding{Root: "customer", Joins: []graph.Path{p}}
	_, err := FromBinding(b, registry(t, "commerce.json"), clock)
	if err == nil || qferr.CodeOf(err) != qferr.CodeInternal || !strings.Contains(err.Error(), "lost in planning") {
		t.Errorf("err = %v", err)
	}
}

func TestSelectionsAreDeduplicated(t *testing.T) {
	f := binding.FieldRef{Entity: "customer", Field: "name", Type: config.TypeString}
	b := binding.Binding{Root: "customer", Selections: []binding.Selection{{Field: f, Path: rootPath("customer")}, {Field: f, Path: rootPath("customer")}}}
	p, err := FromBinding(b, registry(t, "commerce.json"), clock)
	if err != nil || len(p.Select) != 1 {
		t.Errorf("select = %v, err = %v", p.Select, err)
	}
}

func TestPlannerIsDeterministic(t *testing.T) {
	hits := map[string][]hit{"customers": hCustomers, "india": hIndia, "spent": hSpent, "more than": hMoreThan}
	first, _ := planSentence(t, "customers from India who spent more than 50000", hits)
	for i := 0; i < 20; i++ {
		p, _ := planSentence(t, "customers from India who spent more than 50000", hits)
		if !reflect.DeepEqual(p, first) {
			t.Fatalf("run %d differs", i)
		}
	}
}

// ===========================================================================
// The plan type: strict decoding and redaction
// ===========================================================================

func TestDecodeIsStrict(t *testing.T) {
	for name, doc := range map[string]string{
		"unknown top-level key": `{"root":"customer","filters":[]}`,
		"unknown nested key":    `{"root":"customer","select":[{"field":"customer.name","alias":"x"}]}`,
		"value with two kinds":  `{"root":"c","where":[{"kind":"compare","values":[{"number":1,"string":"a"}]}]}`,
		"value of unknown kind": `{"root":"c","where":[{"kind":"compare","values":[{"sql":"1=1"}]}]}`,
		"trailing content":      `{"root":"customer"} {"root":"order"}`,
		"not an object":         `[]`,
	} {
		if _, err := Decode([]byte(doc)); qferr.CodeOf(err) != qferr.CodeInvalidInput {
			t.Errorf("%s: err = %v", name, err)
		}
	}
	p, err := Decode([]byte(`{"root":"customer","where":[{"kind":"compare","field":"customer.country","op":"eq","values":[{"string":"IN"}]}]}`))
	if err != nil || p.Where[0].Values[0].String.Reveal() != "IN" {
		t.Errorf("valid plan: %v, %+v", err, p)
	}
}

func TestPlanRedaction(t *testing.T) {
	p := Plan{Root: "customer", Where: []Condition{{Kind: Compare, Field: "customer.name", Op: config.OpEq, Values: []Value{Str("Zorblatt")}}}}
	enc, _ := json.Marshal(p)
	for _, s := range []string{fmt.Sprint(p), fmt.Sprintf("%#v", p), fmt.Sprintf("%#v", p.Where[0].Values[0]), string(enc), p.Explain(false)} {
		if strings.Contains(s, "Zorblatt") {
			t.Errorf("value leaked: %s", s)
		}
	}
	if !strings.Contains(p.Explain(true), "Zorblatt") {
		t.Error("Explain(true) must reveal")
	}
}

// Decision 022 §2: a window's count is exact now, so a huge one is a real
// period — one the query cannot carry. It is refused, never wrapped or
// clamped; the largest counts a sane period needs still plan.
func TestAWindowOutsideTheCalendarIsRefused(t *testing.T) {
	window := func(unit nlp.DateUnit, count int) binding.Binding {
		return binding.Binding{Root: "order", Conditions: []binding.Condition{{Kind: binding.CondWindow, Field: dateField(),
			Value: binding.Value{Kind: binding.ValueDate, Date: &nlp.DateExpr{Anchor: nlp.AnchorRolling, Unit: unit, Count: count}}, Path: rootPath("order")}}}
	}
	reg := registry(t, "commerce.json")
	for _, c := range []struct {
		unit  nlp.DateUnit
		count int
	}{{nlp.UnitDay, 2147483647}, {nlp.UnitYear, 2026}, {nlp.UnitMonth, 100000}, {nlp.UnitWeek, 2147483647}} {
		_, err := FromBinding(window(c.unit, c.count), reg, clock)
		if err == nil || qferr.CodeOf(err) != qferr.CodeValueResolutionFailed || !strings.Contains(err.Error(), "years 1 to 9999") {
			t.Errorf("last %d %s: err = %v", c.count, c.unit, err)
		}
	}
	// The last 2025 years from 2026-09-21 start in year 1: in range.
	if _, err := FromBinding(window(nlp.UnitYear, 2025), reg, clock); err != nil {
		t.Errorf("last 2025 years: %v", err)
	}
}
