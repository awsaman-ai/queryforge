package compiler

import (
	"flag"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	"queryforge.local/v2experiment/internal/config"
	"queryforge.local/v2experiment/internal/graph"
	"queryforge.local/v2experiment/internal/numeric"
	"queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
)

// update rewrites the golden files: go test ./internal/compiler -update.
// A golden change is a change to the output contract and must be reviewed.
var update = flag.Bool("update", false, "rewrite testdata/plans/*.golden")

func registry(t *testing.T, name string) (*config.SchemaRegistry, *graph.Graph) {
	t.Helper()
	cfg, err := config.Load(filepath.Join("..", "..", "testdata", "configs", name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	reg := config.NewSchemaRegistry(cfg)
	return reg, graph.New(reg)
}

func compileOn(t *testing.T, cfgName string, p plan.Plan) (Query, error) {
	t.Helper()
	reg, g := registry(t, cfgName)
	return Compile(p, reg, g)
}

func mustCompile(t *testing.T, cfgName string, p plan.Plan) Query {
	t.Helper()
	q, err := compileOn(t, cfgName, p)
	if err != nil {
		t.Fatalf("compile: %v", err)
	}
	return q
}

// planFixtures maps each accepted plan fixture to its config. Every
// *.json in testdata/plans must appear here or in refusedFixtures, so a new
// fixture cannot sit untested.
var planFixtures = map[string]string{
	"brief-example":        "commerce.json",
	"joins-and-subqueries": "commerce.json",
	"exists-and-count":     "commerce.json",
	"group-having":         "commerce.json",
	"multipath-two-joins":  "multipath.json",
}

var refusedFixtures = map[string]qferr.Code{
	"refuse-fanout-sum":           qferr.CodeUnsupportedQuery,
	"refuse-same-row-ambiguity":   qferr.CodeAmbiguousMatch,
	"refuse-identifier-injection": qferr.CodeSecurityRejected,
	"refuse-unknown-key":          qferr.CodeInvalidInput,
	"refuse-unconfigured-join":    qferr.CodeSecurityRejected,
}

func plansDir() string { return filepath.Join("..", "..", "testdata", "plans") }

func loadPlan(t *testing.T, name string) (plan.Plan, error) {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(plansDir(), name+".json"))
	if err != nil {
		t.Fatal(err)
	}
	return plan.Decode(data)
}

// The Phase 6 gate, as a test: each hand-written plan compiles to exactly the
// reviewed SQL and parameters (revealed, so the golden pins the values too).
func TestGoldenPlans(t *testing.T) {
	for name, cfgName := range planFixtures {
		t.Run(name, func(t *testing.T) {
			p, err := loadPlan(t, name)
			if err != nil {
				t.Fatal(err)
			}
			got := mustCompile(t, cfgName, p).Render(true)
			golden := filepath.Join(plansDir(), name+".golden")
			if *update {
				if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
					t.Fatal(err)
				}
			}
			want, err := os.ReadFile(golden)
			if err != nil {
				t.Fatalf("%v (run with -update to create it)", err)
			}
			if got != string(want) {
				t.Errorf("output differs from %s\n--- got\n%s\n--- want\n%s", golden, got, want)
			}
		})
	}
}

func TestRefusedPlanFixtures(t *testing.T) {
	for name, code := range refusedFixtures {
		t.Run(name, func(t *testing.T) {
			p, err := loadPlan(t, name)
			if err == nil {
				_, err = compileOn(t, "commerce.json", p)
			}
			if qferr.CodeOf(err) != code {
				t.Fatalf("err = %v, want %s", err, code)
			}
		})
	}
}

func TestEveryPlanFixtureIsCovered(t *testing.T) {
	files, _ := filepath.Glob(filepath.Join(plansDir(), "*.json"))
	for _, f := range files {
		name := strings.TrimSuffix(filepath.Base(f), ".json")
		_, ok1 := planFixtures[name]
		_, ok2 := refusedFixtures[name]
		if !ok1 && !ok2 {
			t.Errorf("%s is in testdata/plans but no test uses it", name)
		}
	}
}

// Section 14's gate: "The same input plan must always produce byte-for-byte
// equivalent SQL formatting." Compiled repeatedly, with fresh registries and
// graphs, so no cached state can make it pass.
func TestSamePlanSameBytes(t *testing.T) {
	for name, cfgName := range planFixtures {
		p, err := loadPlan(t, name)
		if err != nil {
			t.Fatal(err)
		}
		first := mustCompile(t, cfgName, p).Render(true)
		for i := 0; i < 50; i++ {
			if got := mustCompile(t, cfgName, p).Render(true); got != first {
				t.Fatalf("%s: run %d differs:\n%s\nvs\n%s", name, i, got, first)
			}
		}
	}
}

var (
	quotedIdent = regexp.MustCompile(`"([^"]*)"`)
	placeholder = regexp.MustCompile(`\$([0-9]+)`)
)

// configuredIdentifiers is every identifier the config allows the compiler to
// emit, plus the generated aliases.
func configuredIdentifiers(reg *config.SchemaRegistry) map[string]bool {
	ok := map[string]bool{}
	for _, e := range reg.Entities() {
		ok[e.Table] = true
		for _, f := range reg.FieldsOf(e.Name) {
			ok[f.Column] = true
		}
	}
	for i := 0; i < 64; i++ {
		ok[alias(i)] = true
	}
	return ok
}

// assertOnlyConfigured is the exit criterion as an assertion: every quoted
// identifier in the SQL is configured or a generated alias, nothing
// unquoted-and-unknown sits where an identifier goes, and the SQL holds no
// literal other than LIMIT's integer and the LIKE escape character.
func assertOnlyConfigured(t *testing.T, reg *config.SchemaRegistry, q Query) {
	t.Helper()
	ok := configuredIdentifiers(reg)
	for _, m := range quotedIdent.FindAllStringSubmatch(q.SQL, -1) {
		if !ok[m[1]] {
			t.Errorf("emitted identifier %q is not in the configured schema:\n%s", m[1], q.SQL)
		}
	}
	stripped := quotedIdent.ReplaceAllString(q.SQL, "")
	stripped = strings.ReplaceAll(stripped, `ESCAPE '\'`, "")
	if strings.ContainsAny(stripped, `'"`) {
		t.Errorf("a quoted literal survived outside identifiers:\n%s", q.SQL)
	}
}

// assertParamsAccounted: $1..$n appear in order, each exactly once, and n is
// the parameter count. A value that was bound but never referenced, or a
// placeholder with no value, would mean part of the plan went missing.
func assertParamsAccounted(t *testing.T, q Query) {
	t.Helper()
	ms := placeholder.FindAllStringSubmatch(q.SQL, -1)
	if len(ms) != len(q.Params) {
		t.Fatalf("%d placeholders, %d params:\n%s", len(ms), len(q.Params), q.SQL)
	}
	for i, m := range ms {
		if m[1] != fmt.Sprint(i+1) || q.Params[i].Index != i+1 {
			t.Fatalf("placeholder %d is $%s (param index %d)", i+1, m[1], q.Params[i].Index)
		}
	}
}

func TestGoldenOutputsUseOnlyConfiguredIdentifiers(t *testing.T) {
	for name, cfgName := range planFixtures {
		p, _ := loadPlan(t, name)
		reg, g := registry(t, cfgName)
		q, err := Compile(p, reg, g)
		if err != nil {
			t.Fatal(err)
		}
		assertOnlyConfigured(t, reg, q)
		assertParamsAccounted(t, q)
	}
}

// Values never enter the SQL text, whatever they contain.
func TestHostileValuesStayParameters(t *testing.T) {
	for _, v := range []string{`IN'; DROP TABLE customers; --`, `"; SELECT 1; --`, `$1`, `\`, `%`, "a\nb"} {
		q := mustCompile(t, "commerce.json", plan.Plan{
			Root:   "customer",
			Select: []plan.FieldPath{{Field: "customer.name"}},
			Where: []plan.Condition{
				{Kind: plan.Compare, Field: "customer.country", Op: config.OpEq, Values: []plan.Value{plan.Str(v)}},
				{Kind: plan.Compare, Field: "customer.name", Op: config.OpLike, Values: []plan.Value{plan.Str(v)}},
			},
		})
		if strings.Contains(q.SQL, "DROP") || strings.Contains(q.SQL, "SELECT 1") || strings.Contains(q.SQL, "a\nb") {
			t.Errorf("value %q reached the SQL text:\n%s", v, q.SQL)
		}
		if q.Params[0].Value.String.Reveal() != v {
			t.Errorf("eq parameter altered: %q", q.Params[0].Value.String.Reveal())
		}
		assertParamsAccounted(t, q)
	}
}

func TestLikeEscapesTheUsersMetacharacters(t *testing.T) {
	for in, want := range map[string]string{
		"smith":   "%smith%",
		"50%":     `%50\%%`,
		"a_b":     `%a\_b%`,
		`c:\temp`: `%c:\\temp%`,
	} {
		q := mustCompile(t, "commerce.json", plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "customer.name"}},
			Where: []plan.Condition{{Kind: plan.Compare, Field: "customer.name", Op: config.OpLike, Values: []plan.Value{plan.Str(in)}}}})
		if got := q.Params[0].Value.String.Reveal(); got != want {
			t.Errorf("like %q → %q, want %q", in, got, want)
		}
	}
}

// A registry built from a config that never went through Validate: the
// compiler's own identifier check is the last defence, and it holds.
func TestUnvalidatedRegistryCannotInjectIdentifiers(t *testing.T) {
	for _, evil := range []string{`customers"; DROP TABLE x; --`, "customers users", "", strings.Repeat("a", 64), "1abc"} {
		cfg := &config.Config{
			Entities: []config.Entity{{Name: "customer", Table: evil}},
			Fields:   []config.Field{{Entity: "customer", Name: "name", Column: "name", Type: config.TypeString}},
		}
		reg := config.NewSchemaRegistry(cfg)
		_, err := Compile(plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "customer.name"}}}, reg, graph.New(reg))
		if qferr.CodeOf(err) != qferr.CodeSecurityRejected {
			t.Errorf("table %q: err = %v, want SECURITY_REJECTED", evil, err)
		}
		if evil != "" && err != nil && strings.Contains(err.Error(), "DROP") {
			t.Errorf("the refused identifier was echoed: %v", err)
		}
	}
	cfg := &config.Config{
		Entities: []config.Entity{{Name: "customer", Table: "customers"}},
		Fields:   []config.Field{{Entity: "customer", Name: "name", Column: `name" OR 1=1 --`, Type: config.TypeString}},
	}
	reg := config.NewSchemaRegistry(cfg)
	if _, err := Compile(plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "customer.name"}}}, reg, graph.New(reg)); qferr.CodeOf(err) != qferr.CodeSecurityRejected {
		t.Errorf("column injection: err = %v", err)
	}
}

// Every way a plan can name something the config does not have.
func TestUnconfiguredNamesAreRefused(t *testing.T) {
	cases := map[string]plan.Plan{
		"root":           {Root: "invoice", Select: []plan.FieldPath{{Field: "invoice.id"}}},
		"field":          {Root: "customer", Select: []plan.FieldPath{{Field: "customer.password"}}},
		"entity":         {Root: "customer", Select: []plan.FieldPath{{Field: "invoice.total"}}},
		"relationship":   {Root: "customer", Select: []plan.FieldPath{{Field: "order.amount", Via: []string{"hacked"}}}},
		"operator":       {Root: "customer", Select: []plan.FieldPath{{Field: "customer.name"}}, Where: []plan.Condition{{Kind: plan.Compare, Field: "customer.name", Op: "similar_to", Values: []plan.Value{plan.Str("x")}}}},
		"function":       {Root: "customer", Aggregates: []plan.Aggregate{{Func: "string_agg", Field: "customer.name"}}},
		"not selectable": {Root: "customer", Select: []plan.FieldPath{{Field: "customer.id"}}},
		"not filterable": {Root: "order", Select: []plan.FieldPath{{Field: "order.amount"}}, Where: []plan.Condition{{Kind: plan.Compare, Field: "order.customer_id", Op: config.OpEq, Values: []plan.Value{plan.Num(numeric.MustParse("1"))}}}},
		"hidden order":   {Root: "customer", Select: []plan.FieldPath{{Field: "customer.name"}}, OrderBy: []plan.OrderKey{{Field: "customer.id"}}},
		"hidden agg":     {Root: "customer", Aggregates: []plan.Aggregate{{Func: config.AggMax, Field: "customer.id"}}},
	}
	for name, p := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := compileOn(t, "commerce.json", p); qferr.CodeOf(err) != qferr.CodeSecurityRejected {
				t.Fatalf("err = %v, want SECURITY_REJECTED", err)
			}
		})
	}
}

func cond(kind plan.ConditionKind, field string, via []string, op config.Operator, vals ...plan.Value) plan.Condition {
	return plan.Condition{Kind: kind, Field: field, Via: via, Op: op, Values: vals}
}

func ptr[T any](v T) *T { return &v }

// Malformed and unfaithful plans, one rule each, with the code each must get.
func TestRefusals(t *testing.T) {
	name := []plan.FieldPath{{Field: "customer.name"}}
	jan, feb := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		p    plan.Plan
		code qferr.Code
		msg  string
	}{
		{"selects nothing", plan.Plan{Root: "customer"}, qferr.CodeCompilationError, "selects nothing"},
		{"selected twice", plan.Plan{Root: "customer", Select: append(name, name...)}, qferr.CodeCompilationError, "twice"},
		{"operator outside the field's allowlist", plan.Plan{Root: "customer", Select: name,
			Where: []plan.Condition{cond(plan.Compare, "customer.total_spent", nil, config.OpNeq, plan.Num(numeric.MustParse("1")))}}, qferr.CodeUnsupportedQuery, "does not allow"},
		{"operator outside the type matrix", plan.Plan{Root: "customer", Select: name,
			Where: []plan.Condition{cond(plan.Compare, "customer.name", nil, config.OpGt, plan.Str("m"))}}, qferr.CodeUnsupportedQuery, "does not allow"},
		{"number compared with a string field", plan.Plan{Root: "customer", Select: name,
			Where: []plan.Condition{cond(plan.Compare, "customer.name", nil, config.OpEq, plan.Num(numeric.MustParse("5")))}}, qferr.CodeValueResolutionFailed, "needs a string"},
		{"enum value outside the declared set", plan.Plan{Root: "customer", Select: name,
			Where: []plan.Condition{cond(plan.Compare, "customer.status", nil, config.OpEq, plan.Str("cancelled"))}}, qferr.CodeValueResolutionFailed, "declared values"},
		{"no number", plan.Plan{Root: "customer", Select: name,
			Where: []plan.Condition{cond(plan.Compare, "customer.total_spent", nil, config.OpGt, plan.Value{Kind: plan.NumberValue})}}, qferr.CodeValueResolutionFailed, "not a number"},
		{"NUL in a string", plan.Plan{Root: "customer", Select: name,
			Where: []plan.Condition{cond(plan.Compare, "customer.name", nil, config.OpEq, plan.Str("a\x00b"))}}, qferr.CodeValueResolutionFailed, "NUL"},
		{"eq with two values", plan.Plan{Root: "customer", Select: name,
			Where: []plan.Condition{cond(plan.Compare, "customer.name", nil, config.OpEq, plan.Str("a"), plan.Str("b"))}}, qferr.CodeCompilationError, "exactly one"},
		{"in with none", plan.Plan{Root: "customer", Select: name,
			Where: []plan.Condition{cond(plan.Compare, "customer.name", nil, config.OpIn)}}, qferr.CodeCompilationError, "one or more"},
		{"negated compare", plan.Plan{Root: "customer", Select: name,
			Where: []plan.Condition{{Kind: plan.Compare, Field: "customer.name", Op: config.OpEq, Values: []plan.Value{plan.Str("a")}, Negate: true}}}, qferr.CodeCompilationError, "by its operator"},
		{"range on a non-date", plan.Plan{Root: "customer", Select: name,
			Where: []plan.Condition{{Kind: plan.Range, Field: "customer.name", Start: &jan, End: &feb}}}, qferr.CodeCompilationError, "date field"},
		{"empty range", plan.Plan{Root: "customer", Select: name,
			Where: []plan.Condition{{Kind: plan.Range, Field: "customer.created_at", Start: &feb, End: &jan}}}, qferr.CodeValueResolutionFailed, "empty"},
		{"range missing an end", plan.Plan{Root: "customer", Select: name,
			Where: []plan.Condition{{Kind: plan.Range, Field: "customer.created_at", Start: &jan}}}, qferr.CodeCompilationError, "both start and end"},
		{"count through a to-one path", plan.Plan{Root: "order", Select: []plan.FieldPath{{Field: "order.amount"}},
			Where: []plan.Condition{{Kind: plan.Count, Via: []string{"order_product"}, Op: config.OpGt, Values: []plan.Value{plan.Num(numeric.MustParse("1"))}}}}, qferr.CodeUnsupportedQuery, "always 0 or 1"},
		{"fractional count", plan.Plan{Root: "customer", Select: name,
			Where: []plan.Condition{{Kind: plan.Count, Via: []string{"customer_orders"}, Op: config.OpGt, Values: []plan.Value{plan.Num(numeric.MustParse("2.5"))}}}}, qferr.CodeValueResolutionFailed, "whole"},
		{"exists without a path", plan.Plan{Root: "customer", Select: name,
			Where: []plan.Condition{{Kind: plan.Exists}}}, qferr.CodeCompilationError, "needs a relationship path"},
		{"unknown condition kind", plan.Plan{Root: "customer", Select: name,
			Where: []plan.Condition{{Kind: "subquery"}}}, qferr.CodeCompilationError, "unknown condition kind"},
		{"mixed connective", plan.Plan{Root: "customer", Select: name, WhereJoin: "xor"}, qferr.CodeCompilationError, "whereJoin"},
		{"limit without order", plan.Plan{Root: "customer", Select: name, Limit: 5}, qferr.CodeCompilationError, "arbitrary subset"},
		{"negative limit", plan.Plan{Root: "customer", Select: name, Limit: -1}, qferr.CodeCompilationError, "negative"},
		{"order by an ungrouped field", plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "customer.country"}},
			Aggregates: []plan.Aggregate{{Func: config.AggCount, Rows: "customer"}}, OrderBy: []plan.OrderKey{{Field: "customer.name"}}}, qferr.CodeCompilationError, "grouped"},
		{"order by a field not in a distinct select", plan.Plan{Root: "customer", Distinct: true, Select: []plan.FieldPath{{Field: "customer.country"}},
			OrderBy: []plan.OrderKey{{Field: "customer.name"}}}, qferr.CodeCompilationError, "distinct"},
		{"distinct with aggregates", plan.Plan{Root: "customer", Distinct: true, Aggregates: []plan.Aggregate{{Func: config.AggCount, Rows: "customer"}}}, qferr.CodeCompilationError, "distinct"},
		{"aggregate index out of range", plan.Plan{Root: "customer", Select: name, OrderBy: []plan.OrderKey{{Aggregate: ptr(0)}}}, qferr.CodeCompilationError, "aggregate index"},
		{"order key naming both", plan.Plan{Root: "customer", Aggregates: []plan.Aggregate{{Func: config.AggCount, Rows: "customer"}}, OrderBy: []plan.OrderKey{{Field: "customer.name", Aggregate: ptr(0)}}}, qferr.CodeCompilationError, "exactly one"},
		{"having without aggregates", plan.Plan{Root: "customer", Select: name,
			Having: []plan.HavingCondition{{Aggregate: plan.Aggregate{Func: config.AggCount, Rows: "customer"}, Op: config.OpGt, Value: plan.Num(numeric.MustParse("1"))}}}, qferr.CodeCompilationError, "no aggregate"},
		{"having with like", plan.Plan{Root: "customer", Aggregates: []plan.Aggregate{{Func: config.AggCount, Rows: "customer"}},
			Having: []plan.HavingCondition{{Aggregate: plan.Aggregate{Func: config.AggCount, Rows: "customer"}, Op: config.OpLike, Value: plan.Num(numeric.MustParse("1"))}}}, qferr.CodeCompilationError, "having[0]"},
		{"sum on a string", plan.Plan{Root: "customer", Aggregates: []plan.Aggregate{{Func: config.AggSum, Field: "customer.name"}}}, qferr.CodeUnsupportedQuery, "not allowed"},
		{"count of rows with sum", plan.Plan{Root: "customer", Aggregates: []plan.Aggregate{{Func: config.AggSum, Rows: "customer"}}}, qferr.CodeCompilationError, "only count"},
		{"field on the wrong end of its path", plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "product.name", Via: []string{"customer_orders"}}}}, qferr.CodeCompilationError, "ends at order"},
		{"field written without an entity", plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "name"}}}, qferr.CodeCompilationError, "entity.field"},

		// Fan-out (decision 015).
		{"listing through to-many", plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "customer.name"}, {Field: "order.amount", Via: []string{"customer_orders"}}}},
			qferr.CodeUnsupportedQuery, "repeat each customer"},
		{"root aggregate beside a to-many aggregate", plan.Plan{Root: "customer", Aggregates: []plan.Aggregate{
			{Func: config.AggCount, Rows: "customer"}, {Func: config.AggSum, Field: "order.amount", Via: []string{"customer_orders"}}}},
			qferr.CodeUnsupportedQuery, "multiplied"},
		{"counting to-one related rows", plan.Plan{Root: "order", Aggregates: []plan.Aggregate{{Func: config.AggCount, Rows: "product", Via: []string{"order_product"}}}},
			qferr.CodeUnsupportedQuery, "at most one"},
		{"condition through the aggregated relationship", plan.Plan{Root: "customer",
			Aggregates: []plan.Aggregate{{Func: config.AggSum, Field: "order.amount", Via: []string{"customer_orders"}}},
			Where:      []plan.Condition{cond(plan.Compare, "order.amount", []string{"customer_orders"}, config.OpGt, plan.Num(numeric.MustParse("100")))}},
			qferr.CodeAmbiguousMatch, "aggregates over"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := compileOn(t, "commerce.json", c.p)
			if err == nil || qferr.CodeOf(err) != c.code || !strings.Contains(err.Error(), c.msg) {
				t.Fatalf("err = %v\nwant %s containing %q", err, c.code, c.msg)
			}
		})
	}
}

// The two fan-out shapes that ARE accepted, so the refusals above are not
// just "refuse everything with a to-many step".
func TestFanOutShapesThatCompile(t *testing.T) {
	// OR-joined conditions through the same relationship: EXISTS(a) OR
	// EXISTS(b) equals EXISTS(a OR b), so there is one reading.
	q := mustCompile(t, "commerce.json", plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "customer.name"}}, WhereJoin: plan.Or,
		Where: []plan.Condition{
			cond(plan.Compare, "product.category", []string{"customer_orders", "order_product"}, config.OpEq, plan.Str("laptop")),
			cond(plan.Compare, "order.amount", []string{"customer_orders"}, config.OpGt, plan.Num(numeric.MustParse("1000"))),
		}})
	if strings.Count(q.SQL, "EXISTS") != 2 || strings.Contains(q.SQL, "\nJOIN") || strings.Contains(q.SQL, "LEFT JOIN") {
		t.Errorf("want two EXISTS and no outer join:\n%s", q.SQL)
	}
	// Grouping by a root field while aggregating the related rows.
	q = mustCompile(t, "commerce.json", plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "customer.country"}},
		Aggregates: []plan.Aggregate{{Func: config.AggAvg, Field: "order.amount", Via: []string{"customer_orders"}}}})
	if !strings.Contains(q.SQL, "\nJOIN \"orders\"") || !strings.Contains(q.SQL, "GROUP BY \"t0\".\"country\"") {
		t.Errorf("want an INNER join to the domain and GROUP BY:\n%s", q.SQL)
	}
}

// Shared prefixes are joined once, and distinct paths to the same table get
// distinct aliases.
func TestJoinsAreSharedByPrefix(t *testing.T) {
	q := mustCompile(t, "commerce.json", plan.Plan{Root: "order", Select: []plan.FieldPath{
		{Field: "product.name", Via: []string{"order_product"}},
		{Field: "product.price", Via: []string{"order_product"}},
	}, Where: []plan.Condition{cond(plan.Compare, "product.category", []string{"order_product"}, config.OpEq, plan.Str("phone"))}})
	if n := strings.Count(q.SQL, "JOIN"); n != 1 {
		t.Errorf("want one join, got %d:\n%s", n, q.SQL)
	}
}

func TestRangeBoundariesAreHalfOpen(t *testing.T) {
	jan, feb := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC), time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)
	q := mustCompile(t, "commerce.json", plan.Plan{Root: "order", Select: []plan.FieldPath{{Field: "order.amount"}},
		Where: []plan.Condition{{Kind: plan.Range, Field: "order.placed_at", Start: &jan, End: &feb}}})
	if !strings.Contains(q.SQL, `"t0"."placed_at" >= $1 AND "t0"."placed_at" < $2`) || strings.Contains(q.SQL, "BETWEEN") || strings.Contains(q.SQL, "<=") {
		t.Errorf("range is not half-open:\n%s", q.SQL)
	}
	if !q.Params[0].Value.Time.Equal(jan) || !q.Params[1].Value.Time.Equal(feb) {
		t.Errorf("params = %v", q.Params)
	}
}

func TestOrderByPutsNullsLastBothWays(t *testing.T) {
	q := mustCompile(t, "commerce.json", plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "customer.name"}},
		OrderBy: []plan.OrderKey{{Field: "customer.total_spent", Desc: true}, {Field: "customer.name"}}, Limit: 3})
	if !strings.Contains(q.SQL, `"t0"."total_spent" DESC NULLS LAST, "t0"."name" ASC NULLS LAST`) {
		t.Errorf("NULLS LAST missing:\n%s", q.SQL)
	}
}

// Section 16.7: filter and query-length limits.
func TestLimits(t *testing.T) {
	reg, g := registry(t, "commerce.json")
	var where []plan.Condition
	for i := 0; i <= reg.Security().MaxFilters; i++ {
		where = append(where, cond(plan.Compare, "customer.total_spent", nil, config.OpGt, plan.Num(numeric.FromInt64(int64(i)))))
	}
	_, err := Compile(plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "customer.name"}}, Where: where}, reg, g)
	if qferr.CodeOf(err) != qferr.CodeSecurityRejected || !strings.Contains(err.Error(), "conditions") {
		t.Errorf("maxFilters: %v", err)
	}

	cfg := *reg.Config()
	cfg.Security.MaxQueryLength = 40
	small := config.NewSchemaRegistry(&cfg)
	_, err = Compile(plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "customer.name"}, {Field: "customer.country"}}}, small, graph.New(small))
	if qferr.CodeOf(err) != qferr.CodeSecurityRejected || !strings.Contains(err.Error(), "characters") {
		t.Errorf("maxQueryLength: %v", err)
	}
}

// Redaction: %v, %#v and errors never carry a string value; Render(true) does.
func TestQueryRedaction(t *testing.T) {
	q := mustCompile(t, "commerce.json", plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "customer.name"}},
		Where: []plan.Condition{cond(plan.Compare, "customer.name", nil, config.OpEq, plan.Str("Zorblatt"))}})
	for _, s := range []string{fmt.Sprint(q), fmt.Sprintf("%#v", q), q.Render(false), fmt.Sprintf("%v", q.Params)} {
		if strings.Contains(s, "Zorblatt") {
			t.Errorf("value leaked: %s", s)
		}
	}
	if !strings.Contains(q.Render(true), "Zorblatt") {
		t.Error("Render(true) must reveal")
	}
	_, err := compileOn(t, "commerce.json", plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "customer.name"}},
		Where: []plan.Condition{cond(plan.Compare, "customer.status", nil, config.OpEq, plan.Str("Zorblatt"))}})
	if err == nil || strings.Contains(err.Error(), "Zorblatt") {
		t.Errorf("refusal leaked or did not refuse: %v", err)
	}
}

func TestDriverValues(t *testing.T) {
	cases := []struct {
		v    plan.Value
		want any
	}{
		{plan.Num(numeric.MustParse("50000")), int64(50000)},
		{plan.Num(numeric.MustParse("-5")), int64(-5)},
		// Whole numbers are int64 across int64's whole range now; before the
		// gate, 2^53 + 1 went out as the float64 9007199254740992.
		{plan.Num(numeric.MustParse("9007199254740993")), int64(9007199254740993)},
		{plan.Num(numeric.MustParse("9223372036854775807")), int64(math.MaxInt64)},
		// Everything else is its exact decimal text (decision 022 §2).
		{plan.Num(numeric.MustParse("49.5")), "49.5"},
		{plan.Num(numeric.MustParse("0.1")), "0.1"},
		{plan.Num(numeric.MustParse("9223372036854775808")), "9223372036854775808"},
		{plan.Num(numeric.MustParse("99999999999999999999999")), "99999999999999999999999"},
		{plan.Num(numeric.MustParse("0.1234567890123456789")), "0.1234567890123456789"},
		{plan.Str("IN"), "IN"},
		{plan.Boolean(true), true},
	}
	for _, c := range cases {
		if got := (Param{Value: c.v}).Driver(); got != c.want {
			t.Errorf("%v → %#v, want %#v", c.v.Render(true), got, c.want)
		}
	}
}

// Fuzz: arbitrary names and paths either refuse or compile to SQL that holds
// only configured identifiers and fully accounted parameters. Never a panic.
func FuzzCompileOnlyEmitsConfiguredIdentifiers(f *testing.F) {
	f.Add("customer", "customer.name", "customer_orders", "order.amount", "gt", 5.0, "IN")
	f.Add("order", "product.name", "order_product", "product.category", "eq", 0.0, "laptop")
	f.Add("customer", `customer.name"--`, "x", "customer.country", "like", -1.0, `%'"`)
	reg, g := func() (*config.SchemaRegistry, *graph.Graph) {
		cfg, err := config.Load(filepath.Join("..", "..", "testdata", "configs", "commerce.json"))
		if err != nil {
			f.Fatal(err)
		}
		r := config.NewSchemaRegistry(cfg)
		return r, graph.New(r)
	}()
	f.Fuzz(func(t *testing.T, root, sel, rel, field, op string, n float64, s string) {
		for _, via := range [][]string{nil, {rel}} {
			// The fuzzer's float is turned into exact decimal text; NaN and
			// ±Inf have none and become the zero Decimal, which is not a
			// number and must be refused like any other invalid value.
			num, _ := numeric.Parse(strconv.FormatFloat(n, 'f', -1, 64))
			for _, v := range []plan.Value{plan.Num(num), plan.Str(s)} {
				p := plan.Plan{Root: root, Select: []plan.FieldPath{{Field: sel, Via: via}},
					Where: []plan.Condition{{Kind: plan.Compare, Field: field, Via: via, Op: config.Operator(op), Values: []plan.Value{v}}}}
				q, err := Compile(p, reg, g)
				if err != nil {
					if qferr.CodeOf(err) == qferr.CodeInternal {
						t.Fatalf("internal error: %v", err)
					}
					continue
				}
				assertOnlyConfigured(t, reg, q)
				assertParamsAccounted(t, q)
			}
		}
	})
}

// The operator and function allowlists match the closed vocabularies
// exactly: nothing in config is unreachable, nothing extra is emittable.
func TestAllowlistsMatchTheVocabulary(t *testing.T) {
	var ops []string
	for _, op := range config.Operators() {
		s, ok := sqlOperator(op)
		if !ok {
			t.Errorf("operator %s has no SQL", op)
		}
		ops = append(ops, s)
	}
	sort.Strings(ops)
	if len(ops) != 9 {
		t.Errorf("ops = %v", ops)
	}
	for _, a := range config.Aggregations() {
		if _, ok := sqlFunction(a); !ok {
			t.Errorf("aggregation %s has no SQL", a)
		}
	}
	if _, ok := sqlOperator("similar_to"); ok {
		t.Error("an unknown operator reached SQL")
	}
}

// Two different to-many paths in one output would cross-multiply. multipath
// has exactly that shape: a customer's billed orders and shipped orders.
func TestTwoToManyPathsInTheOutputAreRefused(t *testing.T) {
	_, err := compileOn(t, "multipath.json", plan.Plan{Root: "customer", Aggregates: []plan.Aggregate{
		{Func: config.AggSum, Field: "order.amount", Via: []string{"order_billing_customer"}},
		{Func: config.AggSum, Field: "order.amount", Via: []string{"order_shipping_customer"}},
	}})
	if err == nil || qferr.CodeOf(err) != qferr.CodeUnsupportedQuery || !strings.Contains(err.Error(), "multiply each by the other") {
		t.Fatalf("err = %v", err)
	}
	// One of them alone is a faithful aggregate.
	q := mustCompile(t, "multipath.json", plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "customer.country"}},
		Aggregates: []plan.Aggregate{{Func: config.AggSum, Field: "order.amount", Via: []string{"order_billing_customer"}}}})
	wantIn(t, q.SQL, `JOIN "orders" AS "t1" ON "t1"."billing_customer_id" = "t0"."id"`)
}

func wantIn(t *testing.T, sql, part string) {
	t.Helper()
	if !strings.Contains(sql, part) {
		t.Errorf("SQL lacks %q:\n%s", part, sql)
	}
}

// Decision 017 pins standard SQL NULL semantics as INTENTIONAL. "not from
// India" excludes customers whose country is unknown, and "not in" likewise.
// Rewriting to `<> $1 OR col IS NULL` (or IS DISTINCT FROM) would be a change
// of meaning that needs an explicit include-unknown policy in the query or the
// config, not a bug fix. If this test fails, read decision 017 first.
func TestNegationKeepsStandardNullSemantics(t *testing.T) {
	q := mustCompile(t, "commerce.json", plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "customer.name"}},
		Where: []plan.Condition{
			cond(plan.Compare, "customer.country", nil, config.OpNeq, plan.Str("IN")),
			cond(plan.Compare, "customer.status", nil, config.OpNin, plan.Str("active")),
		}})
	for _, forbidden := range []string{"IS NULL", "IS NOT NULL", "IS DISTINCT FROM", "COALESCE"} {
		if strings.Contains(q.SQL, forbidden) {
			t.Errorf("negation now contains %q; see decision 017:\n%s", forbidden, q.SQL)
		}
	}
	wantIn(t, q.SQL, `"t0"."country" <> $1`)
	wantIn(t, q.SQL, `"t0"."status" NOT IN ($2)`)
}

// Phase 7 finding M06: "customers from India and United States" bound as
// country = IN AND country = US, which returns nothing, silently.
func TestUnsatisfiableConjunctionIsRefused(t *testing.T) {
	name := []plan.FieldPath{{Field: "customer.name"}}
	two := func(join plan.Connective, a, b plan.Value) plan.Plan {
		return plan.Plan{Root: "customer", Select: name, WhereJoin: join, Where: []plan.Condition{
			cond(plan.Compare, "customer.country", nil, config.OpEq, a),
			cond(plan.Compare, "customer.country", nil, config.OpEq, b),
		}}
	}
	_, err := compileOn(t, "commerce.json", two(plan.And, plan.Str("IN"), plan.Str("US")))
	if err == nil || qferr.CodeOf(err) != qferr.CodeAmbiguousMatch || !strings.Contains(err.Error(), "or") {
		t.Fatalf("err = %v", err)
	}
	if strings.Contains(err.Error(), "IN") || strings.Contains(err.Error(), "US\"") {
		t.Errorf("the refusal names a value: %v", err)
	}
	// OR is the other reading, and compiles.
	mustCompile(t, "commerce.json", two(plan.Or, plan.Str("IN"), plan.Str("US")))
	// The same value twice is redundant, not contradictory.
	mustCompile(t, "commerce.json", two(plan.And, plan.Str("IN"), plan.Str("IN")))
	// Different fields, or a range on one field, are satisfiable.
	mustCompile(t, "commerce.json", plan.Plan{Root: "customer", Select: name, Where: []plan.Condition{
		cond(plan.Compare, "customer.total_spent", nil, config.OpGt, plan.Num(numeric.MustParse("1"))),
		cond(plan.Compare, "customer.total_spent", nil, config.OpLt, plan.Num(numeric.MustParse("9"))),
	}})
	// Through a to-one join, the same row: still contradictory.
	_, err = compileOn(t, "commerce.json", plan.Plan{Root: "order", Select: []plan.FieldPath{{Field: "order.amount"}}, Where: []plan.Condition{
		cond(plan.Compare, "customer.country", []string{"customer_orders"}, config.OpEq, plan.Str("IN")),
		cond(plan.Compare, "customer.country", []string{"customer_orders"}, config.OpEq, plan.Str("US")),
	}})
	if err == nil || qferr.CodeOf(err) != qferr.CodeAmbiguousMatch {
		t.Errorf("to-one path: err = %v", err)
	}
}

// Decision 022 §2 at the contradiction check: two numbers are the same only
// if every digit agrees. The pair below is ONE float64 (both round to 1e23),
// so the float comparison called "= a AND = b" redundant and compiled it as
// if one condition had been asked; exactly, they differ, and the conjunction
// can match nothing — refused as the M06 rule refuses strings.
func TestNumericContradictionIsExact(t *testing.T) {
	a, b := numeric.MustParse("99999999999999999999999"), numeric.MustParse("99999999999999999999998")
	if fa, fb := toFloat(a), toFloat(b); fa != fb {
		t.Fatalf("premise: the two literals must be one float64 (%v, %v)", fa, fb)
	}
	two := func(x, y numeric.Decimal) plan.Plan {
		return plan.Plan{Root: "customer", Select: []plan.FieldPath{{Field: "customer.name"}}, WhereJoin: plan.And, Where: []plan.Condition{
			cond(plan.Compare, "customer.total_spent", nil, config.OpEq, plan.Num(x)),
			cond(plan.Compare, "customer.total_spent", nil, config.OpEq, plan.Num(y)),
		}}
	}
	if _, err := compileOn(t, "commerce.json", two(a, b)); err == nil || qferr.CodeOf(err) != qferr.CodeAmbiguousMatch {
		t.Errorf("= a AND = b with a != b past float precision: err = %v", err)
	}
	// And a positive and a negative value of equal magnitude are different.
	if _, err := compileOn(t, "commerce.json", two(numeric.MustParse("5"), numeric.MustParse("-5"))); err == nil {
		t.Error("= 5 AND = -5 compiled")
	}
	mustCompile(t, "commerce.json", two(a, a))
}

func toFloat(d numeric.Decimal) float64 {
	f, _ := strconv.ParseFloat(d.String(), 64)
	return f
}
