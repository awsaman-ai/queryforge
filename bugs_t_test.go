package queryforge

// Regression tests for the defects logged in BUGS-T.md.
//
// One test (or one sub-test) per finding, named for the finding, so a future
// reader can go from a row in the log to the thing that stops it coming back.
// Each asserts the FIXED behaviour, and each would have failed before the fix.

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// tConfig is a small config covering the shapes these tests need: an enum, a
// date, a number, an array, and a string that permits regex.
func tConfig(t *testing.T) *Config {
	t.Helper()
	c, err := ParseConfig([]byte(`{
	  "entity": "Order",
	  "model": {"provider":"openai","model":"x"},
	  "backends": {"sql": {"table": "orders"}, "mongo": {"collection": "orders"}},
	  "defaults": {"limit": 50, "maxLimit": 500},
	  "fields": [
	    {"name":"status","type":"enum","values":["PLACED","DELIVERED","CANCELLED"],"indexed":true},
	    {"name":"createdAt","type":"date"},
	    {"name":"amount","type":"number"},
	    {"name":"tags","type":"array","itemType":"string"},
	    {"name":"customerName","type":"string","operators":["equals","contains","regex"]}
	  ]
	}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	return c
}

// tGen compiles an AST for one backend, failing the test on error.
func tGen(t *testing.T, c *Config, q *Query, backend string) *Result {
	t.Helper()
	g, ok := DefaultRegistry().Get(backend)
	if !ok {
		t.Fatalf("no generator for %q", backend)
	}
	r, err := g.Generate(q, c, GenOptions{Now: time.Date(2026, 8, 7, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatalf("generate %s: %v", backend, err)
	}
	return r
}

// tFilter renders a Mongo filter document as canonical JSON for comparison.
func tFilter(t *testing.T, r *Result) string {
	t.Helper()
	mq, ok := r.Doc.(*MongoQuery)
	if !ok {
		t.Fatalf("result.Doc is %T, want *MongoQuery", r.Doc)
	}
	b, err := json.Marshal(mq.Filter)
	if err != nil {
		t.Fatalf("marshal filter: %v", err)
	}
	return string(b)
}

// --- QF-T-001 — scope keys reached SQL as raw identifiers -------------------

func TestQFT001ScopeKeyMustBeAnIdentifier(t *testing.T) {
	c := tConfig(t)
	e := NewWithProvider(c, &StubProvider{})

	bad := []string{
		"tenant_id = 'X' OR 1=1 --",   // the injection from the report
		"$where",                      // a Mongo operator in filter-key position
		"tenant_id); DROP TABLE x;--", // statement termination
		"tenant id",                   // a space is not an identifier
		"1tenant",                     // may not start with a digit
		"items..sku",                  // empty path segment
	}
	for _, k := range bad {
		t.Run(k, func(t *testing.T) {
			_, err := e.GenerateFrom(NewQuery("Order"), "sql", Scope{k: "T-1"})
			if err == nil {
				t.Fatalf("scope key %q was accepted", k)
			}
			if !errors.Is(err, ErrScope) {
				t.Errorf("error is not tagged ErrScope (so a service would not answer 400): %v", err)
			}
		})
	}

	// The ordinary undeclared tenant key still works — the fix must not close the
	// documented pass-through case along with the hole.
	for _, k := range []string{"tenantId", "tenant_id", "org.tenantId"} {
		if _, err := e.GenerateFrom(NewQuery("Order"), "sql", Scope{k: "T-1"}); err != nil {
			t.Errorf("legitimate scope key %q rejected: %v", k, err)
		}
	}
}

func TestQFT001ConfigRejectsInjectableSQLMapping(t *testing.T) {
	_, err := ParseConfig([]byte(`{
	  "entity":"Order","model":{"model":"x"},
	  "fields":[{"name":"status","type":"string","mapping":{"sql":"status = 'X' OR 1=1 --"}}]
	}`))
	if err == nil {
		t.Fatal("a SQL mapping that is not an identifier was accepted")
	}
	if !strings.Contains(err.Error(), "invalid sql mapping") {
		t.Errorf("unexpected error: %v", err)
	}
}

// A Mongo collection or an Elasticsearch index is a parameter, not syntax, so
// the SQL identifier rule must not be imposed on it.
func TestQFT001NonSQLSourcesKeepTheirNames(t *testing.T) {
	if _, err := ParseConfig([]byte(`{
	  "entity":"Order","model":{"model":"x"},
	  "backends":{"es":{"index":"orders-v2"},"mongo":{"collection":"orders-2026"}},
	  "fields":[{"name":"status","type":"string"}]
	}`)); err != nil {
		t.Fatalf("a hyphenated index/collection name was rejected: %v", err)
	}
}

// --- QF-T-002 / QF-T-013 — relative_date unit and amount --------------------

func TestQFT002RelativeDateUnitIsValidated(t *testing.T) {
	c := tConfig(t)
	cases := []struct {
		name   string
		unit   string
		amount int
		ok     bool
	}{
		{"singular day is legal", "day", -30, true},
		{"plural days is rejected", "days", -30, false},
		{"empty unit is rejected", "", -30, false},
		{"nonsense unit is rejected", "fortnight", -1, false},
		{"absurd amount is rejected", "day", 1 << 40, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := NewQuery("Order")
			q.Filter = &Condition{Type: CondComparison, Field: "createdAt", Operator: OpAfter,
				Value: &Value{Kind: KindRelativeDate, Unit: tc.unit, Amount: tc.amount}}

			err := Validate(q, c)
			if tc.ok {
				if err != nil {
					t.Fatalf("valid relative date rejected: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatal("invalid relative date passed validation (it would bind the current time)")
			}
			var errs ValidationErrors
			if !errors.As(err, &errs) || errs[0].Code != CodeInvalidRelDate {
				t.Errorf("want %s, got %v", CodeInvalidRelDate, err)
			}
		})
	}
}

// The generator must not guess either: an unresolvable value is an error, never
// a silent fall back to "now".
func TestQFT002GeneratorRefusesUnknownUnit(t *testing.T) {
	c := tConfig(t)
	q := NewQuery("Order")
	q.Filter = &Condition{Type: CondComparison, Field: "createdAt", Operator: OpAfter,
		Value: &Value{Kind: KindRelativeDate, Unit: "days", Amount: -30}}

	for _, backend := range []string{"sql", "mysql", "mongo"} {
		g, _ := DefaultRegistry().Get(backend)
		if _, err := g.Generate(q, c, GenOptions{}); err == nil {
			t.Errorf("%s: generated a query from an unresolvable relative date", backend)
		} else if !strings.Contains(err.Error(), "unknown unit") {
			t.Errorf("%s: unexpected error: %v", backend, err)
		}
	}
}

// --- QF-T-003 — a degenerate model reply must not become a full-table read --

func TestQFT003EmptyModelReplyFailsClosed(t *testing.T) {
	c := tConfig(t)
	for _, reply := range []string{`{}`, `{"unsupported":""}`, `{"entity":"Order"}`, `{"foo":1}`} {
		t.Run(reply, func(t *testing.T) {
			e := NewWithProvider(c, &StubProvider{Response: reply})
			res, err := e.Translate(context.Background(), "delivered orders over 500 dollars", "sql", nil)
			if err == nil {
				t.Fatalf("empty reply compiled to a query: %s", res.Query.SQL)
			}
			if !errors.Is(err, ErrModelOutput) {
				t.Errorf("want ErrModelOutput so the repair loop can retry, got: %v", err)
			}
		})
	}
}

// A reply that says something — even just a limit — is still an answer.
func TestQFT003NonEmptyReplyStillTranslates(t *testing.T) {
	c := tConfig(t)
	e := NewWithProvider(c, &StubProvider{Response: `{"entity":"Order","limit":10}`})
	if _, err := e.Translate(context.Background(), "ten orders", "sql", nil); err != nil {
		t.Fatalf("a reply carrying a limit was rejected: %v", err)
	}
}

// --- QF-T-004 — negation must select the same rows on both backends ---------

func TestQFT004NegationRequiresTheFieldToExist(t *testing.T) {
	c := tConfig(t)

	cases := []struct {
		name string
		cond *Condition
		want string
	}{
		{
			name: "notEquals",
			cond: &Condition{Type: CondComparison, Field: "status", Operator: OpNotEquals,
				Value: &Value{Kind: KindEnum, V: "CANCELLED"}},
			want: `{"status":{"$exists":true,"$ne":"CANCELLED"}}`,
		},
		{
			name: "notIn",
			cond: &Condition{Type: CondComparison, Field: "status", Operator: OpNotIn,
				Value: &Value{Kind: KindArray, V: []any{"CANCELLED"}}},
			want: `{"status":{"$exists":true,"$nin":["CANCELLED"]}}`,
		},
		{
			name: "NOT over a comparison",
			cond: &Condition{Type: CondLogical, Op: OpNOT, Children: []*Condition{{
				Type: CondComparison, Field: "status", Operator: OpEquals,
				Value: &Value{Kind: KindEnum, V: "CANCELLED"}}}},
			want: `{"$and":[{"$nor":[{"status":"CANCELLED"}]},{"status":{"$exists":true}}]}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := NewQuery("Order")
			q.Filter = tc.cond
			if err := Validate(q, c); err != nil {
				t.Fatalf("test AST is invalid: %v", err)
			}
			if got := tFilter(t, tGen(t, c, q, "mongo")); got != tc.want {
				t.Errorf("mongo filter = %s\nwant %s", got, tc.want)
			}
		})
	}
}

// isNull/isNotNull are ABOUT absence and already agree with SQL, so the
// existence guard must not be applied to them: NOT(isNotNull) has to keep
// matching documents that lack the field, the way SQL returns NULL rows.
func TestQFT004NullOperatorsAreNotGuarded(t *testing.T) {
	c := tConfig(t)
	q := NewQuery("Order")
	q.Filter = &Condition{Type: CondLogical, Op: OpNOT, Children: []*Condition{
		{Type: CondComparison, Field: "status", Operator: OpIsNotNull}}}

	got := tFilter(t, tGen(t, c, q, "mongo"))
	if strings.Contains(got, "$exists") {
		t.Errorf("existence guard wrongly applied to a null operator: %s", got)
	}
}

// --- QF-T-005 — the MySQL dialect ------------------------------------------

func TestQFT005MySQLDialect(t *testing.T) {
	c := tConfig(t)
	q := NewQuery("Order")
	q.Filter = &Condition{Type: CondLogical, Op: OpAND, Children: []*Condition{
		{Type: CondComparison, Field: "status", Operator: OpEquals, Value: &Value{Kind: KindEnum, V: "DELIVERED"}},
		{Type: CondComparison, Field: "amount", Operator: OpBetween, Value: &Value{Kind: KindArray, V: []any{100.0, 500.0}}},
		{Type: CondComparison, Field: "customerName", Operator: OpRegex, Value: &Value{Kind: KindString, V: "^A"}},
		{Type: CondComparison, Field: "tags", Operator: OpContainsAll, Value: &Value{Kind: KindArray, V: []any{"premium"}}},
	}}
	q.Sort = []SortSpec{{Field: "createdAt", Dir: "DESC"}}
	q.Limit = intPtr(10)
	if err := Validate(q, c); err != nil {
		t.Fatalf("test AST is invalid: %v", err)
	}

	r := tGen(t, c, q, "mysql")
	if r.Backend != "mysql" {
		t.Errorf("backend = %q", r.Backend)
	}
	// Identifiers here are all plain, so they stay bare — quoting is reserved for
	// names that need it (TestQFT009ReservedWordsAreQuoted covers that half).
	for _, want := range []string{
		"status = ?",                           // ? placeholders, not $1
		"amount BETWEEN ? AND ?",               //
		"customerName REGEXP ?",                // not the Postgres ~
		"JSON_CONTAINS(tags, CAST(? AS JSON))", // no ARRAY[] / @>
		"FROM orders",                          //
		"ORDER BY createdAt DESC",              //
		"LIMIT 10",                             //
	} {
		if !strings.Contains(r.SQL, want) {
			t.Errorf("missing %q in: %s", want, r.SQL)
		}
	}
	for _, unwanted := range []string{"$1", "ARRAY[", "@>", " ~ "} {
		if strings.Contains(r.SQL, unwanted) {
			t.Errorf("Postgres-only syntax %q leaked into MySQL: %s", unwanted, r.SQL)
		}
	}

	// Postgres output must be untouched by the refactor.
	pg := tGen(t, c, q, "sql")
	for _, want := range []string{"status = $1", "amount BETWEEN $2 AND $3", "customerName ~ $4", "tags @> ARRAY[$5]"} {
		if !strings.Contains(pg.SQL, want) {
			t.Errorf("postgres output changed: missing %q in %s", want, pg.SQL)
		}
	}
}

// MySQL cannot parse OFFSET without LIMIT.
func TestQFT005MySQLOffsetWithoutLimit(t *testing.T) {
	c := tConfig(t)
	c.Defaults.Limit = 0
	q := NewQuery("Order")
	q.Offset = intPtr(20)

	sql := tGen(t, c, q, "mysql").SQL
	if !strings.Contains(sql, "LIMIT 18446744073709551615 OFFSET 20") {
		t.Errorf("offset-only MySQL query is not parseable: %s", sql)
	}
}

// --- QF-T-006 — generators must not panic on an unvalidated AST ------------

func TestQFT006GeneratorsErrorRatherThanPanic(t *testing.T) {
	c := tConfig(t)
	bad := map[string]*Condition{
		"NOT with no children": {Type: CondLogical, Op: OpNOT},
		"AND with no children": {Type: CondLogical, Op: OpAND},
		"NOT with two children": {Type: CondLogical, Op: OpNOT, Children: []*Condition{
			{Type: CondComparison, Field: "status", Operator: OpEquals, Value: &Value{Kind: KindEnum, V: "PLACED"}},
			{Type: CondComparison, Field: "status", Operator: OpEquals, Value: &Value{Kind: KindEnum, V: "DELIVERED"}},
		}},
		"between with one element": {Type: CondComparison, Field: "amount", Operator: OpBetween,
			Value: &Value{Kind: KindArray, V: []any{100.0}}},
		"between with a scalar": {Type: CondComparison, Field: "amount", Operator: OpBetween,
			Value: &Value{Kind: KindNumber, V: 100.0}},
		"in with no value":       {Type: CondComparison, Field: "status", Operator: OpIn},
		"contains with no value": {Type: CondComparison, Field: "customerName", Operator: OpContains},
	}
	for name, cond := range bad {
		for _, backend := range []string{"sql", "mysql", "mongo"} {
			t.Run(name+"/"+backend, func(t *testing.T) {
				defer func() {
					if r := recover(); r != nil {
						t.Fatalf("panicked on an unvalidated AST: %v", r)
					}
				}()
				q := NewQuery("Order")
				q.Filter = cond
				g, _ := DefaultRegistry().Get(backend)
				if _, err := g.Generate(q, c, GenOptions{}); err == nil {
					t.Error("malformed node compiled without error")
				}
			})
		}
	}
}

// --- QF-T-007 — bounds on the work one AST can cause -----------------------

func TestQFT007FilterNodeBudget(t *testing.T) {
	c := tConfig(t)
	c.Policy.MaxFilterNodes = 10

	children := make([]*Condition, 0, 50)
	for i := 0; i < 50; i++ {
		children = append(children, &Condition{Type: CondComparison, Field: "status",
			Operator: OpEquals, Value: &Value{Kind: KindEnum, V: "PLACED"}})
	}
	q := NewQuery("Order")
	q.Filter = &Condition{Type: CondLogical, Op: OpOR, Children: children}

	err := Validate(q, c)
	if err == nil {
		t.Fatal("a 51-node filter passed a 10-node budget")
	}
	var errs ValidationErrors
	if !errors.As(err, &errs) {
		t.Fatalf("unexpected error type: %v", err)
	}
	if errs[0].Code != CodeFilterTooLarge {
		t.Errorf("want %s, got %s", CodeFilterTooLarge, errs[0].Code)
	}
	// Reported once, not once per node past the budget.
	if len(errs) != 1 {
		t.Errorf("want a single error, got %d", len(errs))
	}
}

func TestQFT007ListAndValueBudgets(t *testing.T) {
	c := tConfig(t)
	c.Policy.MaxListLength = 5
	c.Policy.MaxValueLength = 16

	long := make([]any, 20)
	for i := range long {
		long[i] = "PLACED"
	}
	q := NewQuery("Order")
	q.Filter = &Condition{Type: CondComparison, Field: "status", Operator: OpIn,
		Value: &Value{Kind: KindArray, V: long}}
	if err := Validate(q, c); err == nil {
		t.Error("a 20-element IN list passed a 5-element budget")
	}

	q.Filter = &Condition{Type: CondComparison, Field: "customerName", Operator: OpContains,
		Value: &Value{Kind: KindString, V: strings.Repeat("a", 100)}}
	if err := Validate(q, c); err == nil {
		t.Error("a 100-character value passed a 16-character budget")
	}
}

// Depth must be caught as the walk descends, not measured after it.
func TestQFT007DepthIsBoundedDuringTheWalk(t *testing.T) {
	c := tConfig(t)
	c.Policy.MaxNestingDepth = 3

	leaf := &Condition{Type: CondComparison, Field: "status", Operator: OpEquals,
		Value: &Value{Kind: KindEnum, V: "PLACED"}}
	node := leaf
	for i := 0; i < 200; i++ {
		node = &Condition{Type: CondLogical, Op: OpAND, Children: []*Condition{node}}
	}
	q := NewQuery("Order")
	q.Filter = node

	err := Validate(q, c)
	if err == nil {
		t.Fatal("a 201-deep filter passed a depth limit of 3")
	}
	var errs ValidationErrors
	if !errors.As(err, &errs) || errs[0].Code != CodeNestingTooDeep {
		t.Fatalf("want %s, got %v", CodeNestingTooDeep, err)
	}
	if len(errs) != 1 {
		t.Errorf("the walk continued past the depth limit: %d errors", len(errs))
	}
}

// --- QF-T-008 — regex reaches the database bounded -------------------------

func TestQFT008RegexPolicy(t *testing.T) {
	c := tConfig(t)
	pattern := func(s string) *Query {
		q := NewQuery("Order")
		q.Filter = &Condition{Type: CondComparison, Field: "customerName", Operator: OpRegex,
			Value: &Value{Kind: KindString, V: s}}
		return q
	}

	// Default: allowed (deny-list behaviour preserved) but length-capped.
	if err := Validate(pattern("^A"), c); err != nil {
		t.Fatalf("an ordinary pattern was rejected: %v", err)
	}
	if err := Validate(pattern(strings.Repeat("a", defaultMaxRegexLength+1)), c); err == nil {
		t.Error("an over-long pattern passed the default cap")
	}

	// Opt-in allow-list: a field it does not name loses regex.
	c.Policy.AllowRegexOn = []string{"someOtherField"}
	err := Validate(pattern("^A"), c)
	if err == nil {
		t.Fatal("regex was allowed on a field outside policy.allowRegexOn")
	}
	var errs ValidationErrors
	if !errors.As(err, &errs) || errs[0].Code != CodeRegexNotAllowed {
		t.Errorf("want %s, got %v", CodeRegexNotAllowed, err)
	}

	c.Policy.AllowRegexOn = []string{"customerName"}
	if err := Validate(pattern("^A"), c); err != nil {
		t.Errorf("regex rejected on a field the allow-list names: %v", err)
	}
}

// --- QF-T-009 — physical identifiers -------------------------------------

func TestQFT009ReservedWordsAreQuoted(t *testing.T) {
	c, err := ParseConfig([]byte(`{
	  "entity":"Order","model":{"model":"x"},
	  "backends":{"sql":{"table":"order"}},
	  "fields":[{"name":"desc","type":"string"},{"name":"plain","type":"string"}]
	}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	q := NewQuery("Order")
	q.Select = []string{"desc", "plain"}
	q.Filter = &Condition{Type: CondComparison, Field: "desc", Operator: OpEquals,
		Value: &Value{Kind: KindString, V: "x"}}

	pg := tGen(t, c, q, "sql").SQL
	if !strings.Contains(pg, `SELECT "desc", plain FROM "order"`) {
		t.Errorf("reserved words not quoted for postgres: %s", pg)
	}
	if !strings.Contains(pg, `"desc" = $1`) {
		t.Errorf("reserved word not quoted in the predicate: %s", pg)
	}

	my := tGen(t, c, q, "mysql").SQL
	if !strings.Contains(my, "SELECT `desc`, plain FROM `order`") {
		t.Errorf("reserved words not backticked for mysql: %s", my)
	}
}

// An identifier that never passed config load — a Config built in Go, or a
// Mongo-shaped field name — must become an error at the SQL boundary.
func TestQFT009SQLRejectsNonIdentifiers(t *testing.T) {
	c := &Config{Entity: "Order", Fields: []Field{{Name: "x", Type: FieldString}}}
	if err := c.finalize(); err != nil {
		t.Fatalf("finalize: %v", err)
	}
	c.Fields[0].Mapping = map[string]string{"sql": "x); DROP TABLE t;--"}
	c.fieldByName["x"].Mapping = c.Fields[0].Mapping

	q := NewQuery("Order")
	q.Filter = &Condition{Type: CondComparison, Field: "x", Operator: OpEquals,
		Value: &Value{Kind: KindString, V: "v"}}

	g, _ := DefaultRegistry().Get("sql")
	if _, err := g.Generate(q, c, GenOptions{}); err == nil {
		t.Fatal("a non-identifier mapping reached the statement text")
	}
}

// --- QF-T-011 — one clock for the whole engine ----------------------------

// clockProvider records the system prompt it was given.
type clockProvider struct{ system string }

func (p *clockProvider) Complete(_ context.Context, system, _ string) (string, error) {
	p.system = system
	return `{"entity":"Order","limit":1}`, nil
}

func TestQFT011EngineClockReachesThePrompt(t *testing.T) {
	c := tConfig(t)
	p := &clockProvider{}
	e := NewWithProvider(c, p)
	e.Now = func() time.Time { return time.Date(1999, 12, 31, 0, 0, 0, 0, time.UTC) }

	if _, err := e.Translate(context.Background(), "anything", "sql", nil); err != nil {
		t.Fatalf("translate: %v", err)
	}
	if !strings.Contains(p.system, "Today (UTC): 1999-12-31") {
		t.Errorf("the pinned clock did not reach the prompt: %s",
			p.system[strings.Index(p.system, "Today"):min(len(p.system), strings.Index(p.system, "Today")+40)])
	}
}

// --- QF-T-012 — relative-date prose ---------------------------------------

func TestQFT012PluralisationIsExplicit(t *testing.T) {
	cases := map[string]string{
		describeRelative("day", -30):  "30 days ago",
		describeRelative("day", -1):   "1 day ago",
		describeRelative("month", 3):  "3 months from now",
		describeRelative("days", -30): `30 <invalid unit "days"> ago`,
		describeRelative("", -30):     `30 <invalid unit ""> ago`,
	}
	for got, want := range cases {
		if got != want {
			t.Errorf("got %q, want %q", got, want)
		}
	}
}

// --- QF-T-014 — the AST version is enforced -------------------------------

func TestQFT014UnknownVersionIsRejected(t *testing.T) {
	c := tConfig(t)
	q := NewQuery("Order")
	q.Version = "7"
	q.Limit = intPtr(1)

	err := Validate(q, c)
	if err == nil {
		t.Fatal(`an AST tagged version "7" validated`)
	}
	var errs ValidationErrors
	if !errors.As(err, &errs) || errs[0].Code != CodeUnknownVersion {
		t.Errorf("want %s, got %v", CodeUnknownVersion, err)
	}

	q.Version = ASTVersion
	if err := Validate(q, c); err != nil {
		t.Errorf("the current version was rejected: %v", err)
	}
	q.Version = "" // hand-built ASTs may omit it
	if err := Validate(q, c); err != nil {
		t.Errorf("an omitted version was rejected: %v", err)
	}
}

// --- QF-T-015 — duplicate select entries ----------------------------------

func TestQFT015DuplicateSelectIsRejected(t *testing.T) {
	c := tConfig(t)
	q := NewQuery("Order")
	q.Select = []string{"amount", "amount"}

	err := Validate(q, c)
	if err == nil {
		t.Fatal("a duplicated select entry validated (SQL repeats it, Mongo collapses it)")
	}
	var errs ValidationErrors
	if !errors.As(err, &errs) || errs[0].Code != CodeDuplicateField {
		t.Errorf("want %s, got %v", CodeDuplicateField, err)
	}
}

// --- cross-backend equivalence -------------------------------------------

// The claim the README makes is that one validated AST compiles to equivalent
// queries. This pins the one place it had stopped being true.
func TestNegationIsEquivalentAcrossBackends(t *testing.T) {
	c := tConfig(t)
	q := NewQuery("Order")
	q.Filter = &Condition{Type: CondComparison, Field: "status", Operator: OpNotEquals,
		Value: &Value{Kind: KindEnum, V: "CANCELLED"}}

	sql := tGen(t, c, q, "sql").SQL
	if !strings.Contains(sql, "status <> $1") {
		t.Fatalf("unexpected SQL: %s", sql)
	}
	// SQL's <> excludes NULL rows; the Mongo document must exclude missing ones.
	if got := tFilter(t, tGen(t, c, q, "mongo")); !strings.Contains(got, `"$exists":true`) {
		t.Errorf("mongo would return documents SQL excludes: %s", got)
	}
}

// A config written before the MySQL backend existed maps its columns under
// "sql"; those are the MySQL column names too.
func TestMySQLInheritsSQLMappings(t *testing.T) {
	c, err := ParseConfig([]byte(`{
	  "entity":"Order","model":{"model":"x"},
	  "backends":{"sql":{"table":"orders_v2"}},
	  "fields":[{"name":"customerName","type":"string","mapping":{"sql":"customer_name"}}]
	}`))
	if err != nil {
		t.Fatalf("parse config: %v", err)
	}
	q := NewQuery("Order")
	q.Filter = &Condition{Type: CondComparison, Field: "customerName", Operator: OpEquals,
		Value: &Value{Kind: KindString, V: "ada"}}

	sql := tGen(t, c, q, "mysql").SQL
	if !strings.Contains(sql, "FROM orders_v2") || !strings.Contains(sql, "customer_name = ?") {
		t.Errorf("mysql did not inherit the sql mapping: %s", sql)
	}
}
