package queryforge

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/awsaman-ai/queryforge/internal/gen"
	"github.com/awsaman-ai/queryforge/internal/testutil"
)

// TestSQLGolden pins the full SQL output, including predicate ordering (indexed
// fields first, by priority) and parameterization.
func TestSQLGolden(t *testing.T) {
	c := testutil.GenConfig(t)
	r := testutil.GenSQL(t, c, testutil.CanonicalQuery())

	want := "SELECT * FROM orders WHERE (status = $1 AND created_at >= $2 AND refunded = $3 AND tags @> ARRAY[$4, $5]) ORDER BY created_at DESC LIMIT 50"
	if r.SQL != want {
		t.Errorf("SQL mismatch:\n got: %s\nwant: %s", r.SQL, want)
	}
	if len(r.Args) != 5 {
		t.Fatalf("expected 5 args, got %d: %v", len(r.Args), r.Args)
	}
	if r.Args[0] != "DELIVERED" || r.Args[2] != false || r.Args[3] != "premium" || r.Args[4] != "express" {
		t.Errorf("unexpected args: %v", r.Args)
	}
	wantTime, err := gen.ResolveRelative(testutil.FixedNow, "day", -30)
	if err != nil {
		t.Fatalf("resolveRelative: %v", err)
	}
	if got, ok := r.Args[1].(time.Time); !ok || !got.Equal(wantTime) {
		t.Errorf("arg[1] should be the resolved relative date %v, got %v", wantTime, r.Args[1])
	}
}

// TestMongoGolden checks the flattened Mongo filter and options for the same AST.
func TestMongoGolden(t *testing.T) {
	c := testutil.GenConfig(t)
	mq := testutil.GenMongo(t, c, testutil.CanonicalQuery())

	if mq.Collection != "orders" {
		t.Errorf("collection = %q", mq.Collection)
	}
	if mq.Filter["status"] != "DELIVERED" || mq.Filter["refunded"] != false {
		t.Errorf("filter scalars wrong: %#v", mq.Filter)
	}
	all, ok := mq.Filter["tags"].(map[string]any)
	if !ok || !reflect.DeepEqual(all["$all"], []any{"premium", "express"}) {
		t.Errorf("tags $all wrong: %#v", mq.Filter["tags"])
	}
	created, ok := mq.Filter["createdAt"].(map[string]any)
	if !ok {
		t.Fatalf("createdAt missing: %#v", mq.Filter)
	}
	want, err := gen.ResolveRelative(testutil.FixedNow, "day", -30)
	if err != nil {
		t.Fatalf("resolveRelative: %v", err)
	}
	if got, ok := created["$gte"].(time.Time); !ok || !got.Equal(want) {
		t.Errorf("createdAt $gte wrong: %#v", created["$gte"])
	}
	if len(mq.Sort) != 1 || mq.Sort[0].Field != "createdAt" || mq.Sort[0].Order != -1 {
		t.Errorf("sort wrong: %#v", mq.Sort)
	}
	if mq.Limit != 50 {
		t.Errorf("limit = %d", mq.Limit)
	}
}

// TestInjectionStaysInArgs is the security check: a malicious-looking value must
// end up as a bound argument, never spliced into the SQL text.
func TestInjectionStaysInArgs(t *testing.T) {
	c := testutil.GenConfig(t)
	evil := "O'Brien'; DROP TABLE orders; --"
	r := testutil.GenSQL(t, c, testutil.Single(testutil.Comp("customerName", OpEquals, testutil.VStr(evil))))

	if strings.Contains(strings.ToUpper(r.SQL), "DROP") {
		t.Errorf("value leaked into SQL text: %s", r.SQL)
	}
	if r.SQL != "SELECT * FROM orders WHERE customer_name = $1 LIMIT 50" {
		t.Errorf("unexpected SQL: %s", r.SQL)
	}
	if len(r.Args) != 1 || r.Args[0] != evil {
		t.Errorf("evil value not bound as arg: %v", r.Args)
	}
}

// TestReadOnlySQL confirms every generated statement is a pure read.
func TestReadOnlySQL(t *testing.T) {
	c := testutil.GenConfig(t)
	for _, q := range []*Query{
		testutil.CanonicalQuery(),
		testutil.Single(testutil.Comp("status", OpIsNull, nil)),
		testutil.Single(testutil.Comp("amount", OpBetween, testutil.VArr(float64(10), float64(100)))),
	} {
		r := testutil.GenSQL(t, c, q)
		up := strings.ToUpper(r.SQL)
		if !strings.HasPrefix(up, "SELECT ") {
			t.Errorf("statement is not a SELECT: %s", r.SQL)
		}
		for _, verb := range []string{"INSERT", "UPDATE", "DELETE", "DROP", "TRUNCATE", "ALTER"} {
			if strings.Contains(up, verb) {
				t.Errorf("mutating verb %q in read-only output: %s", verb, r.SQL)
			}
		}
	}
}

// TestSQLOperators spot-checks individual operator renderings.
func TestSQLOperators(t *testing.T) {
	c := testutil.GenConfig(t)
	cases := []struct {
		name string
		cmp  *Condition
		want string
		args []any
	}{
		{"contains string", testutil.Comp("customerName", OpContains, testutil.VStr("amy")), "SELECT * FROM orders WHERE customer_name LIKE $1 LIMIT 50", []any{"%amy%"}},
		{"startsWith", testutil.Comp("customerName", OpStartsWith, testutil.VStr("amy")), "SELECT * FROM orders WHERE customer_name LIKE $1 LIMIT 50", []any{"amy%"}},
		{"endsWith", testutil.Comp("customerName", OpEndsWith, testutil.VStr("amy")), "SELECT * FROM orders WHERE customer_name LIKE $1 LIMIT 50", []any{"%amy"}},
		{"in enum", testutil.Comp("status", OpIn, testutil.VArr("PLACED", "DELIVERED")), "SELECT * FROM orders WHERE status IN ($1, $2) LIMIT 50", []any{"PLACED", "DELIVERED"}},
		{"between number", testutil.Comp("amount", OpBetween, testutil.VArr(float64(10), float64(100))), "SELECT * FROM orders WHERE amount BETWEEN $1 AND $2 LIMIT 50", []any{float64(10), float64(100)}},
		{"isNull", testutil.Comp("status", OpIsNull, nil), "SELECT * FROM orders WHERE status IS NULL LIMIT 50", nil},
		{"tags contains", testutil.Comp("tags", OpContains, testutil.VStr("premium")), "SELECT * FROM orders WHERE tags @> ARRAY[$1] LIMIT 50", []any{"premium"}},
	}
	for _, tc := range cases {
		r := testutil.GenSQL(t, c, testutil.Single(tc.cmp))
		if r.SQL != tc.want {
			t.Errorf("%s: got %q want %q", tc.name, r.SQL, tc.want)
		}
		if !reflect.DeepEqual(r.Args, tc.args) {
			t.Errorf("%s: args got %#v want %#v", tc.name, r.Args, tc.args)
		}
	}
}

// TestProjectionAndOr covers select projection and an OR over the same field
// (which must not flatten in Mongo).
func TestProjectionAndOr(t *testing.T) {
	c := testutil.GenConfig(t)

	// Projection
	q := NewQuery("Order")
	q.Select = []string{"status", "amount"}
	rs := testutil.GenSQL(t, c, q)
	if !strings.HasPrefix(rs.SQL, "SELECT status, amount FROM orders") {
		t.Errorf("projection SQL wrong: %s", rs.SQL)
	}
	mq := testutil.GenMongo(t, c, q)
	if mq.Projection["status"] != 1 || mq.Projection["amount"] != 1 {
		t.Errorf("projection doc wrong: %#v", mq.Projection)
	}

	// OR over the same field -> $or in Mongo, parenthesized OR in SQL.
	orQ := NewQuery("Order")
	orQ.Filter = &Condition{Type: CondLogical, Op: OpOR, Children: []*Condition{
		testutil.Comp("status", OpEquals, testutil.VEnum("PLACED")),
		testutil.Comp("status", OpEquals, testutil.VEnum("DELIVERED")),
	}}
	rs2 := testutil.GenSQL(t, c, orQ)
	if !strings.Contains(rs2.SQL, "(status = $1 OR status = $2)") {
		t.Errorf("OR SQL wrong: %s", rs2.SQL)
	}
	mq2 := testutil.GenMongo(t, c, orQ)
	if _, ok := mq2.Filter["$or"]; !ok {
		t.Errorf("expected $or, got %#v", mq2.Filter)
	}
}

// TestPredicateOrdering isolates the indexed-first reordering.
func TestPredicateOrdering(t *testing.T) {
	c := testutil.GenConfig(t)
	q := NewQuery("Order")
	// amount is non-indexed and listed first; status is indexed and should move up.
	q.Filter = testutil.And(
		testutil.Comp("amount", OpGt, testutil.VNum(10)),
		testutil.Comp("status", OpEquals, testutil.VEnum("DELIVERED")),
	)
	r := testutil.GenSQL(t, c, q)
	if !strings.Contains(r.SQL, "(status = $1 AND amount > $2)") {
		t.Errorf("indexed field was not ordered first: %s", r.SQL)
	}
}

// TestIndexWarnings verifies the non-indexed soft warning fires (and only once
// per field).
func TestIndexWarnings(t *testing.T) {
	c := testutil.GenConfig(t)
	r := testutil.GenSQL(t, c, testutil.Single(testutil.Comp("amount", OpGt, testutil.VNum(10))))
	if len(r.Warnings) != 1 || !strings.Contains(r.Warnings[0], "amount") {
		t.Errorf("expected one non-indexed warning for amount, got %#v", r.Warnings)
	}
	// An indexed-only filter produces no warnings.
	r2 := testutil.GenSQL(t, c, testutil.Single(testutil.Comp("status", OpEquals, testutil.VEnum("DELIVERED"))))
	if len(r2.Warnings) != 0 {
		t.Errorf("indexed filter should not warn, got %#v", r2.Warnings)
	}
}

// TestDefaultLimitApplied checks the config default fills in when the AST omits
// a limit.
func TestDefaultLimitApplied(t *testing.T) {
	c := testutil.GenConfig(t)
	q := NewQuery("Order") // no explicit limit
	if r := testutil.GenSQL(t, c, q); !strings.Contains(r.SQL, "LIMIT 50") {
		t.Errorf("default limit not applied: %s", r.SQL)
	}
	if mq := testutil.GenMongo(t, c, q); mq.Limit != 50 {
		t.Errorf("default limit not applied to mongo: %d", mq.Limit)
	}
}

// TestRegistry exercises the plugin registry.
func TestRegistry(t *testing.T) {
	r := DefaultRegistry()
	if got := r.Backends(); !reflect.DeepEqual(got, []string{"elasticsearch", "mongo", "mysql", "opensearch", "sql"}) {
		t.Errorf("backends = %v", got)
	}
	if _, ok := r.Get("sql"); !ok {
		t.Errorf("sql generator not registered")
	}
	if _, ok := r.Get("nope"); ok {
		t.Errorf("unexpected generator")
	}
}

// hiddenConfigJSON mirrors genConfigJSON but adds a field the config hides from
// results (returnable:false). It exists to pin BUG-004: the wide "all fields"
// form must not be used when the config excludes something.
const hiddenConfigJSON = `{
  "entity":"Order","model":{},
  "backends":{"sql":{"table":"orders"},"mongo":{"collection":"orders"}},
  "fields":[
    {"name":"status","type":"enum","values":["PLACED","CANCELLED"],
     "operators":["equals","notEquals"],"mapping":{"sql":"status","mongo":"status"}},
    {"name":"amount","type":"number","operators":["gt","lt"],
     "mapping":{"sql":"amount","mongo":"amount"}},
    {"name":"internalNote","type":"string","queryable":false,"returnable":false,
     "mapping":{"sql":"internal_note","mongo":"internalNote"}}
  ],
  "defaults":{"limit":50,"maxLimit":500}
}`

// TestProjectionExcludesNonReturnableByDefault covers BUG-004. With no explicit
// select the generators previously emitted "SELECT *" / an unprojected find,
// which returns hidden columns straight from the database and silently defeats
// returnable:false. Both backends must now emit an explicit allow-list.
func TestProjectionExcludesNonReturnableByDefault(t *testing.T) {
	c := testutil.MustParse(t, hiddenConfigJSON)
	q := &Query{Version: "1.0", Entity: "Order"} // no Select: the default path

	sql := testutil.GenSQL(t, c, q)
	if strings.Contains(sql.SQL, "*") {
		t.Errorf("SELECT * leaks non-returnable columns; got %q", sql.SQL)
	}
	if strings.Contains(sql.SQL, "internal_note") {
		t.Errorf("projection includes hidden column; got %q", sql.SQL)
	}
	for _, want := range []string{"status", "amount"} {
		if !strings.Contains(sql.SQL, want) {
			t.Errorf("projection missing returnable column %q; got %q", want, sql.SQL)
		}
	}

	mongo, err := MongoGenerator{}.Generate(q, c, GenOptions{Now: testutil.FixedNow})
	if err != nil {
		t.Fatalf("mongo generate: %v", err)
	}
	doc, ok := mongo.Doc.(*MongoQuery)
	if !ok {
		t.Fatalf("unexpected mongo doc type %T", mongo.Doc)
	}
	proj := doc.Projection
	if len(proj) == 0 {
		t.Fatal("empty mongo projection returns the whole document, leaking hidden fields")
	}
	if _, bad := proj["internalNote"]; bad {
		t.Errorf("mongo projection includes hidden field: %v", proj)
	}
}

// TestProjectionStaysWideWhenNothingHidden guards against over-correcting: a
// config that hides nothing should keep the compact "*" form.
func TestProjectionStaysWideWhenNothingHidden(t *testing.T) {
	c := testutil.GenConfig(t) // every field returnable
	q := &Query{Version: "1.0", Entity: "Order"}
	if got := testutil.GenSQL(t, c, q).SQL; !strings.Contains(got, "SELECT *") {
		t.Errorf("expected SELECT * when no field is hidden; got %q", got)
	}
}

// TestProjectionAllFieldsHiddenErrors pins the pathological config: every field
// non-returnable must be a loud error, not "SELECT  FROM t" or a Mongo {} that
// silently means "return everything".
func TestProjectionAllFieldsHiddenErrors(t *testing.T) {
	c := testutil.MustParse(t, `{
      "entity":"Order","model":{},
      "backends":{"sql":{"table":"orders"},"mongo":{"collection":"orders"}},
      "fields":[{"name":"secret","type":"string","returnable":false,"mapping":{"sql":"secret","mongo":"secret"}}],
      "defaults":{"limit":50,"maxLimit":500}
    }`)
	q := &Query{Version: "1.0", Entity: "Order"}

	if _, err := (SQLGenerator{}).Generate(q, c, GenOptions{Now: testutil.FixedNow}); err == nil {
		t.Error("expected an error when no field is returnable (sql)")
	}
	if _, err := (MongoGenerator{}).Generate(q, c, GenOptions{Now: testutil.FixedNow}); err == nil {
		t.Error("expected an error when no field is returnable (mongo)")
	}
}
