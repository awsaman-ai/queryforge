package queryforge

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// scopeConfigJSON adds the two shapes a scope key can take to the standard test
// entity: `tenantId`, declared but hidden from the NLP surface (queryable:false)
// so it gets a per-backend mapping and type checks, and `amount`, an ordinary
// queryable field with numeric bounds — used to prove scope honours the config's
// value rules on fields it does declare.
const scopeConfigJSON = `{
  "entity":"Order","model":{},
  "backends":{"sql":{"table":"orders"},"mongo":{"collection":"orders"}},
  "fields":[
    {"name":"status","type":"enum","values":["PLACED","DELIVERED","CANCELLED","REFUNDED"],
     "indexed":true,"priority":10,"mapping":{"sql":"status","mongo":"status"}},
    {"name":"createdAt","type":"date","operators":["before","after","between"],
     "indexed":true,"priority":8,"mapping":{"sql":"created_at","mongo":"createdAt"}},
    {"name":"amount","type":"number","operators":["gt","lt","gte","lte","between","in","equals"],
     "validators":{"min":0,"max":100000},"mapping":{"sql":"amount","mongo":"amount"}},
    {"name":"tags","type":"array","itemType":"string","mapping":{"sql":"tags","mongo":"tags"}},
    {"name":"tenantId","type":"string","queryable":false,
     "mapping":{"sql":"tenant_id","mongo":"tenantId"}}
  ],
  "defaults":{"limit":50,"maxLimit":500}
}`

func scopeConfig(t *testing.T) *Config { return mustParse(t, scopeConfigJSON) }

func strPtr(s string) *string { return &s }

// scopeEngine builds an engine on the scope config with a fixed clock.
func scopeEngine(t *testing.T, p ModelProvider) *Engine {
	t.Helper()
	e := NewWithProvider(scopeConfig(t), p)
	e.Now = func() time.Time { return fixedNow }
	return e
}

// userQuery is a small, realistic model AST: one enum predicate the user asked
// for. Scope is spliced alongside it.
func userQuery() *Query {
	q := NewQuery("Order")
	q.Filter = comp("status", OpEquals, vEnum("DELIVERED"))
	return q
}

// mustSQL compiles to SQL through the public entry point, failing the test on
// any error.
func mustSQL(t *testing.T, e *Engine, q *Query, s Scope) *Result {
	t.Helper()
	r, err := e.GenerateFrom(q, "sql", s)
	if err != nil {
		t.Fatalf("GenerateFrom(sql): %v", err)
	}
	return r
}

// --- happy path -------------------------------------------------------------

// TestScopeInjectsIntoSQL pins the whole contract in one golden: the scope
// predicate is present, it is bound as a parameter, it sorts ahead of the user's
// own predicate, and the user's filter is untouched.
func TestScopeInjectsIntoSQL(t *testing.T) {
	e := scopeEngine(t, nil)

	r := mustSQL(t, e, userQuery(), Scope{"subscriptionId": "SUB-42"})

	want := "SELECT * FROM orders WHERE (subscriptionId = $1 AND status = $2) LIMIT 50"
	if r.SQL != want {
		t.Errorf("SQL mismatch:\n got: %s\nwant: %s", r.SQL, want)
	}
	if len(r.Args) != 2 || r.Args[0] != "SUB-42" || r.Args[1] != "DELIVERED" {
		t.Errorf("args mismatch: %v", r.Args)
	}
}

// TestScopeInjectsIntoMongo checks the same scope compiles to the document
// backend from the identical AST — the multi-backend promise must hold for
// caller-supplied filters too.
func TestScopeInjectsIntoMongo(t *testing.T) {
	e := scopeEngine(t, nil)

	r, err := e.GenerateFrom(userQuery(), "mongo", Scope{"subscriptionId": "SUB-42"})
	if err != nil {
		t.Fatalf("GenerateFrom(mongo): %v", err)
	}
	mq := r.Doc.(*MongoQuery)

	if got := mq.Filter["subscriptionId"]; got != "SUB-42" {
		t.Errorf("scope predicate missing from mongo filter: %#v", mq.Filter)
	}
	if got := mq.Filter["status"]; got != "DELIVERED" {
		t.Errorf("user predicate lost: %#v", mq.Filter)
	}
}

// TestScopeMultipleKeysAreOrderedDeterministically guards the one property that
// makes scope testable at all: Go randomizes map iteration, so without the sort
// in normalizeScope the emitted SQL and its argument order would differ between
// runs of the same program.
func TestScopeMultipleKeysAreOrderedDeterministically(t *testing.T) {
	e := scopeEngine(t, nil)
	s := Scope{"userId": 9, "enterpriseId": "ENT-7", "subscriptionId": "SUB-42"}

	first := mustSQL(t, e, userQuery(), s)
	for i := 0; i < 50; i++ { // enough iterations to shake out map ordering
		got := mustSQL(t, e, userQuery(), s)
		if got.SQL != first.SQL {
			t.Fatalf("non-deterministic SQL on run %d:\n got: %s\nwant: %s", i, got.SQL, first.SQL)
		}
	}

	// Alphabetical by key: enterpriseId, subscriptionId, userId.
	want := "SELECT * FROM orders WHERE (enterpriseId = $1 AND subscriptionId = $2 AND userId = $3 AND status = $4) LIMIT 50"
	if first.SQL != want {
		t.Errorf("SQL mismatch:\n got: %s\nwant: %s", first.SQL, want)
	}
}

// TestScopeWithoutUserFilter covers the case where the question carried no
// predicates at all ("show me my orders"): the scope becomes the whole WHERE
// clause, with no redundant AND wrapper.
func TestScopeWithoutUserFilter(t *testing.T) {
	e := scopeEngine(t, nil)

	r := mustSQL(t, e, NewQuery("Order"), Scope{"subscriptionId": "SUB-42"})

	want := "SELECT * FROM orders WHERE subscriptionId = $1 LIMIT 50"
	if r.SQL != want {
		t.Errorf("SQL mismatch:\n got: %s\nwant: %s", r.SQL, want)
	}
}

// TestScopeListBecomesIn: a slice value means "any of these".
func TestScopeListBecomesIn(t *testing.T) {
	e := scopeEngine(t, nil)

	r := mustSQL(t, e, NewQuery("Order"), Scope{"enterpriseId": []string{"E-1", "E-2"}})

	want := "SELECT * FROM orders WHERE enterpriseId IN ($1, $2) LIMIT 50"
	if r.SQL != want {
		t.Errorf("SQL mismatch:\n got: %s\nwant: %s", r.SQL, want)
	}
	if len(r.Args) != 2 || r.Args[0] != "E-1" || r.Args[1] != "E-2" {
		t.Errorf("args mismatch: %v", r.Args)
	}
}

// TestScopeAcceptsGoScalarTypes: callers pass whatever their session objects
// hold, so every ordinary Go scalar must normalize without a manual conversion.
func TestScopeAcceptsGoScalarTypes(t *testing.T) {
	ts := time.Date(2026, 1, 15, 9, 30, 0, 0, time.UTC)

	cases := []struct {
		name string
		in   any
		want any // the bound SQL argument
	}{
		{"string", "SUB-42", "SUB-42"},
		{"*string", strPtr("SUB-42"), "SUB-42"}, // session structs hold optional ids as pointers
		{"*int", intPtr(9), float64(9)},
		{"int", 9, float64(9)},
		{"int64", int64(9), float64(9)},
		{"uint32", uint32(9), float64(9)},
		{"float64", 1.5, 1.5},
		{"float32", float32(2.5), 2.5},
		{"bool-true", true, true},
		{"bool-false", false, false}, // must survive: a false is not "unset"
		{"time", ts, "2026-01-15T09:30:00Z"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := scopeEngine(t, nil)
			r := mustSQL(t, e, NewQuery("Order"), Scope{"k": tc.in})
			if len(r.Args) != 1 || r.Args[0] != tc.want {
				t.Errorf("got args %#v, want [%#v]", r.Args, tc.want)
			}
		})
	}
}

// TestScopeAcceptsTypedSlices: []string / []int / []any must all work, so a
// caller never has to hand-convert a slice of ids into []any.
func TestScopeAcceptsTypedSlices(t *testing.T) {
	cases := []struct {
		name string
		in   any
	}{
		{"[]string", []string{"a", "b"}},
		{"[]any", []any{"a", "b"}},
		{"[2]string array", [2]string{"a", "b"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := scopeEngine(t, nil)
			r := mustSQL(t, e, NewQuery("Order"), Scope{"k": tc.in})
			if len(r.Args) != 2 || r.Args[0] != "a" || r.Args[1] != "b" {
				t.Errorf("got args %#v", r.Args)
			}
		})
	}
}

// TestScopeOnDeclaredFieldUsesMapping is the recommended multi-backend pattern:
// declare the scope column with queryable:false. It stays invisible to the model
// but picks up the per-backend physical name, so one Scope compiles correctly
// against both SQL and Mongo.
func TestScopeOnDeclaredFieldUsesMapping(t *testing.T) {
	e := scopeEngine(t, nil)
	s := Scope{"tenantId": "T-1"}

	sql := mustSQL(t, e, NewQuery("Order"), s)
	if !strings.Contains(sql.SQL, "tenant_id = $1") {
		t.Errorf("expected the sql mapping tenant_id, got: %s", sql.SQL)
	}

	r, err := e.GenerateFrom(NewQuery("Order"), "mongo", s)
	if err != nil {
		t.Fatalf("GenerateFrom(mongo): %v", err)
	}
	if got := r.Doc.(*MongoQuery).Filter["tenantId"]; got != "T-1" {
		t.Errorf("expected the mongo mapping tenantId, got filter %#v", r.Doc.(*MongoQuery).Filter)
	}
}

// TestScopeOnDeclaredEnumFieldCoercesKind: a Go string scoped onto an enum field
// must be tagged as an enum, or the domain check would never run.
func TestScopeOnDeclaredEnumFieldCoercesKind(t *testing.T) {
	e := scopeEngine(t, nil)

	_, filters, err := e.ApplyScope(NewQuery("Order"), Scope{"status": "DELIVERED"})
	if err != nil {
		t.Fatalf("ApplyScope: %v", err)
	}
	if len(filters) != 1 || filters[0].Value.Kind != KindEnum {
		t.Errorf("expected enum kind, got %#v", filters[0].Value)
	}
	if !filters[0].Declared {
		t.Error("expected Declared=true for a configured field")
	}
}

// TestScopeOnDeclaredArrayFieldUsesMembership: on an array field, "=" is the
// wrong reading. A scalar means the element is present, a list means any of them
// is — otherwise a scope on a tags-style column would silently match nothing.
func TestScopeOnDeclaredArrayFieldUsesMembership(t *testing.T) {
	e := scopeEngine(t, nil)

	cases := []struct {
		name string
		in   any
		want Operator
	}{
		{"scalar", "premium", OpContains},
		{"list", []string{"premium", "express"}, OpContainsAny},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, filters, err := e.ApplyScope(NewQuery("Order"), Scope{"tags": tc.in})
			if err != nil {
				t.Fatalf("ApplyScope: %v", err)
			}
			if filters[0].Operator != tc.want {
				t.Errorf("got operator %q, want %q", filters[0].Operator, tc.want)
			}
		})
	}
}

// TestScopeUndeclaredFieldIsNotDeclared: the flag reported back distinguishes a
// key the config governs from a raw physical name passed through.
func TestScopeUndeclaredFieldIsNotDeclared(t *testing.T) {
	e := scopeEngine(t, nil)

	_, filters, err := e.ApplyScope(NewQuery("Order"), Scope{"subscriptionId": "SUB-42"})
	if err != nil {
		t.Fatalf("ApplyScope: %v", err)
	}
	if filters[0].Declared {
		t.Error("expected Declared=false for a field the config does not register")
	}
	if got := filters[0].String(); got != `subscriptionId equals "SUB-42"` {
		t.Errorf("ScopeFilter.String() = %q", got)
	}
}

// TestScopeNilAndEmptyAreNoOps: the whole feature must be invisible to existing
// callers. Nil, an empty map, and the pre-v0.0.2 behaviour must all agree.
func TestScopeNilAndEmptyAreNoOps(t *testing.T) {
	e := scopeEngine(t, nil)

	base := mustSQL(t, e, userQuery(), nil)
	empty := mustSQL(t, e, userQuery(), Scope{})

	if base.SQL != empty.SQL {
		t.Errorf("empty scope changed the query:\n nil: %s\nempty: %s", base.SQL, empty.SQL)
	}
	if want := "SELECT * FROM orders WHERE status = $1 LIMIT 50"; base.SQL != want {
		t.Errorf("unscoped SQL changed:\n got: %s\nwant: %s", base.SQL, want)
	}
}

// --- the full pipeline ------------------------------------------------------

// TestTranslateAppliesScope runs the AI path end to end against a stub model and
// checks the scope reached the compiled query and the result record.
func TestTranslateAppliesScope(t *testing.T) {
	ast := `{"entity":"Order","filter":{"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}}`
	e := scopeEngine(t, &StubProvider{Response: ast})

	res, err := e.Translate(context.Background(), "delivered orders", "sql", Scope{"subscriptionId": "SUB-42"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if !strings.Contains(res.Query.SQL, "subscriptionId = $1") {
		t.Errorf("scope missing from SQL: %s", res.Query.SQL)
	}
	if len(res.Scope) != 1 || res.Scope[0].Field != "subscriptionId" {
		t.Errorf("scope not reported on the result: %#v", res.Scope)
	}
}

// TestScopeInASTFlagOffKeepsModelAST: the default. The AST stays in the config's
// vocabulary, and re-compiling it with the same scope reproduces the query
// exactly — the round-trip property the flag's default is chosen for.
func TestScopeInASTFlagOffKeepsModelAST(t *testing.T) {
	ast := `{"entity":"Order","filter":{"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}}`
	e := scopeEngine(t, &StubProvider{Response: ast})
	s := Scope{"subscriptionId": "SUB-42"}

	res, err := e.Translate(context.Background(), "delivered orders", "sql", s)
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if res.AST.Filter.Type != CondComparison || res.AST.Filter.Field != "status" {
		t.Errorf("expected the model's untouched AST, got %#v", res.AST.Filter)
	}

	// Round-trip: the reported AST plus the same scope must recompile identically.
	again, err := e.GenerateFrom(res.AST, "sql", s)
	if err != nil {
		t.Fatalf("round-trip GenerateFrom: %v", err)
	}
	if again.SQL != res.Query.SQL {
		t.Errorf("round-trip mismatch:\n got: %s\nwant: %s", again.SQL, res.Query.SQL)
	}
}

// TestScopeInASTFlagOnIncludesScope: the audit mode. One object records every
// predicate that ran.
func TestScopeInASTFlagOnIncludesScope(t *testing.T) {
	ast := `{"entity":"Order","filter":{"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}}`
	e := scopeEngine(t, &StubProvider{Response: ast})
	e.ScopeInAST = true

	res, err := e.Translate(context.Background(), "delivered orders", "sql", Scope{"subscriptionId": "SUB-42"})
	if err != nil {
		t.Fatalf("Translate: %v", err)
	}
	if res.AST.Filter.Type != CondLogical || len(res.AST.Filter.Children) != 2 {
		t.Fatalf("expected an AND of scope + user filter, got %#v", res.AST.Filter)
	}
	if got := res.AST.Filter.Children[0]; got.Field != "subscriptionId" || !got.Scoped {
		t.Errorf("expected the scope predicate first and marked, got %#v", got)
	}
	// The scope record is populated either way — an audit trail must not depend
	// on which mode the engine happens to be in.
	if len(res.Scope) != 1 {
		t.Errorf("scope record missing in ScopeInAST mode: %#v", res.Scope)
	}
}

// TestScopeAppearsInExplain: the readback a user is shown before anything runs
// must state the scope, and must present it as imposed rather than as something
// the question asked for.
func TestScopeAppearsInExplain(t *testing.T) {
	e := scopeEngine(t, nil)

	eff, _, err := e.ApplyScope(userQuery(), Scope{"subscriptionId": "SUB-42", "userId": 9})
	if err != nil {
		t.Fatalf("ApplyScope: %v", err)
	}
	got := Explain(eff, e.Config())

	want := `Return all fields from Order where status equals "DELIVERED". Always scoped to subscriptionId equals "SUB-42", userId equals 9.`
	if got != want {
		t.Errorf("explain mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestExplainUnchangedWithoutScope: the new clause must never appear on the
// existing, unscoped path.
func TestExplainUnchangedWithoutScope(t *testing.T) {
	e := scopeEngine(t, nil)

	got := Explain(userQuery(), e.Config())
	if strings.Contains(got, "scoped") {
		t.Errorf("unscoped explain mentions scope: %s", got)
	}
	want := `Return all fields from Order where status equals "DELIVERED".`
	if got != want {
		t.Errorf("explain mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestScopeOnlyExplainReadsNaturally: with no user predicates the sentence must
// not end up with a dangling "where".
func TestScopeOnlyExplainReadsNaturally(t *testing.T) {
	e := scopeEngine(t, nil)

	eff, _, err := e.ApplyScope(NewQuery("Order"), Scope{"subscriptionId": "SUB-42"})
	if err != nil {
		t.Fatalf("ApplyScope: %v", err)
	}
	want := `Return all fields from Order. Always scoped to subscriptionId equals "SUB-42".`
	if got := Explain(eff, e.Config()); got != want {
		t.Errorf("explain mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// --- security properties ----------------------------------------------------

// TestScopeCannotBeEscapedByOR is the property the whole feature rests on. The
// model produced `A OR B`; if the scope were merged into that tree rather than
// AND-ed above it, a row from another tenant matching B would come back. The
// compiled SQL must show the scope outside the OR.
func TestScopeCannotBeEscapedByOR(t *testing.T) {
	q := NewQuery("Order")
	q.Filter = &Condition{Type: CondLogical, Op: OpOR, Children: []*Condition{
		comp("status", OpEquals, vEnum("DELIVERED")),
		comp("status", OpEquals, vEnum("PLACED")),
	}}
	e := scopeEngine(t, nil)

	r := mustSQL(t, e, q, Scope{"subscriptionId": "SUB-42"})

	want := "SELECT * FROM orders WHERE (subscriptionId = $1 AND (status = $2 OR status = $3)) LIMIT 50"
	if r.SQL != want {
		t.Errorf("scope escaped the OR:\n got: %s\nwant: %s", r.SQL, want)
	}
}

// TestScopeSurvivesNegation: a NOT at the root must negate only the user's
// condition, never the scope.
func TestScopeSurvivesNegation(t *testing.T) {
	q := NewQuery("Order")
	q.Filter = &Condition{Type: CondLogical, Op: OpNOT, Children: []*Condition{
		comp("status", OpEquals, vEnum("CANCELLED")),
	}}
	e := scopeEngine(t, nil)

	r := mustSQL(t, e, q, Scope{"subscriptionId": "SUB-42"})

	want := "SELECT * FROM orders WHERE (subscriptionId = $1 AND NOT (status = $2)) LIMIT 50"
	if r.SQL != want {
		t.Errorf("SQL mismatch:\n got: %s\nwant: %s", r.SQL, want)
	}
}

// TestModelCannotForgeScopedMarker: the Scoped flag is what gives a node its
// privileged handling, so model output claiming it must be ignored. The flag is
// json:"-" precisely so this can never decode.
func TestModelCannotForgeScopedMarker(t *testing.T) {
	raw := `{"entity":"Order","filter":{"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"DELIVERED"},"scoped":true}}`

	var q Query
	if err := json.Unmarshal([]byte(raw), &q); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if q.Filter.Scoped {
		t.Error("a model-supplied AST managed to set the scoped marker")
	}

	// And via the real parser the planner uses.
	parsed, err := parseAST(raw, nil)
	if err != nil {
		t.Fatalf("parseAST: %v", err)
	}
	if parsed.Filter.Scoped {
		t.Error("parseAST let the scoped marker through")
	}
}

// TestScopeFieldStaysOutOfThePrompt: the model must not learn that the scope
// columns exist. Naming them would invite it to reference them, and a hidden
// tenant column is exactly what a prompt-injection attempt would go looking for.
func TestScopeFieldStaysOutOfThePrompt(t *testing.T) {
	c := scopeConfig(t)
	prompt := NewPlanner(c, nil).SystemPrompt(fixedNow)

	for _, name := range []string{"tenantId", "tenant_id", "subscriptionId"} {
		if strings.Contains(prompt, name) {
			t.Errorf("prompt discloses scope field %q:\n%s", name, prompt)
		}
	}
}

// TestScopeWorksOnFieldHiddenFromQueries: the same hidden field the model may
// not reference must still be scopable — that combination is the point of
// declaring it queryable:false.
func TestScopeWorksOnFieldHiddenFromQueries(t *testing.T) {
	e := scopeEngine(t, nil)

	// The model referencing it is rejected...
	bad := NewQuery("Order")
	bad.Filter = comp("tenantId", OpEquals, vStr("T-1"))
	if _, err := e.GenerateFrom(bad, "sql", nil); err == nil {
		t.Error("expected a hidden field to be rejected in a model AST")
	}

	// ...while the application scoping on it works.
	if _, err := e.GenerateFrom(NewQuery("Order"), "sql", Scope{"tenantId": "T-1"}); err != nil {
		t.Errorf("scope on a hidden field should be allowed: %v", err)
	}
}

// TestScopeValuesAreParameterized: a scope value is data, so even a deliberate
// SQL-injection payload must arrive as a bound argument and never as statement
// text.
func TestScopeValuesAreParameterized(t *testing.T) {
	payload := "SUB-42'; DROP TABLE orders; --"
	e := scopeEngine(t, nil)

	r := mustSQL(t, e, NewQuery("Order"), Scope{"subscriptionId": payload})

	if strings.Contains(r.SQL, "DROP") || strings.Contains(r.SQL, payload) {
		t.Errorf("scope value leaked into the statement: %s", r.SQL)
	}
	if len(r.Args) != 1 || r.Args[0] != payload {
		t.Errorf("expected the payload bound as an argument, got %#v", r.Args)
	}
}

// TestScopeNarrowsWhenModelFiltersTheSameField: if a field is both queryable and
// scoped, the two predicates must both survive. AND-ing them can only narrow;
// letting either one win would be a widening the caller did not authorize.
func TestScopeNarrowsWhenModelFiltersTheSameField(t *testing.T) {
	e := scopeEngine(t, nil)

	r := mustSQL(t, e, userQuery(), Scope{"status": "PLACED"})

	want := "SELECT * FROM orders WHERE (status = $1 AND status = $2) LIMIT 50"
	if r.SQL != want {
		t.Errorf("SQL mismatch:\n got: %s\nwant: %s", r.SQL, want)
	}
	if r.Args[0] != "PLACED" || r.Args[1] != "DELIVERED" {
		t.Errorf("expected both predicates bound, got %#v", r.Args)
	}
}

// TestScopeNarrowsOnSameFieldInMongo is the document-store half of the case
// above, and the one with a real trap: a Mongo filter is a map, so merging two
// predicates on the same key would drop one silently. The generator must fall
// back to an explicit $and.
func TestScopeNarrowsOnSameFieldInMongo(t *testing.T) {
	e := scopeEngine(t, nil)

	r, err := e.GenerateFrom(userQuery(), "mongo", Scope{"status": "PLACED"})
	if err != nil {
		t.Fatalf("GenerateFrom(mongo): %v", err)
	}
	mq := r.Doc.(*MongoQuery)

	clauses, ok := mq.Filter["$and"].([]any)
	if !ok {
		t.Fatalf("expected an $and for colliding keys, got %#v", mq.Filter)
	}
	if len(clauses) != 2 {
		t.Fatalf("expected 2 clauses, got %d: %#v", len(clauses), clauses)
	}
	// Scope first, then the user's predicate — and neither one lost.
	if got := clauses[0].(map[string]any)["status"]; got != "PLACED" {
		t.Errorf("scope predicate wrong or missing: %#v", clauses[0])
	}
	if got := clauses[1].(map[string]any)["status"]; got != "DELIVERED" {
		t.Errorf("user predicate wrong or missing: %#v", clauses[1])
	}
}

// TestScopeIsSafeForConcurrentUse: one engine serves every request in a server,
// and scope values differ per caller. A shared engine that leaked one request's
// scope into another's query would be a cross-tenant data leak, so run the two
// entry points concurrently under -race with distinct scopes and check each
// result carries only its own.
func TestScopeIsSafeForConcurrentUse(t *testing.T) {
	ast := `{"entity":"Order","filter":{"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}}`
	e := scopeEngine(t, &StubProvider{Response: ast})

	const workers = 32
	errs := make(chan error, workers*2)
	shared := userQuery() // deliberately shared: proves applyScope copies

	for i := 0; i < workers; i++ {
		tenant := "SUB-" + string(rune('A'+i%26))

		go func() {
			res, err := e.Translate(context.Background(), "delivered orders", "sql", Scope{"subscriptionId": tenant})
			if err != nil {
				errs <- err
				return
			}
			errs <- expectOnlyTenant(res.Query, tenant)
		}()
		go func() {
			r, err := e.GenerateFrom(shared, "sql", Scope{"subscriptionId": tenant})
			if err != nil {
				errs <- err
				return
			}
			errs <- expectOnlyTenant(r, tenant)
		}()
	}
	for i := 0; i < workers*2; i++ {
		if err := <-errs; err != nil {
			t.Error(err)
		}
	}
}

// expectOnlyTenant asserts the compiled query carries exactly the one scope
// value it was given.
func expectOnlyTenant(r *Result, tenant string) error {
	if len(r.Args) != 2 || r.Args[0] != tenant {
		return errors.New("wrong scope in query args: " + r.SQL)
	}
	if strings.Count(r.SQL, "subscriptionId") != 1 {
		return errors.New("scope applied more than once: " + r.SQL)
	}
	return nil
}

// TestScopeStaysReadOnly: injection must not open a write path.
func TestScopeStaysReadOnly(t *testing.T) {
	e := scopeEngine(t, nil)

	r := mustSQL(t, e, userQuery(), Scope{"subscriptionId": "SUB-42"})
	if !strings.HasPrefix(r.SQL, "SELECT ") {
		t.Errorf("expected a SELECT, got: %s", r.SQL)
	}
	for _, verb := range []string{"INSERT", "UPDATE", "DELETE", "DROP", "ALTER"} {
		if strings.Contains(strings.ToUpper(r.SQL), verb) {
			t.Errorf("mutation verb %q in output: %s", verb, r.SQL)
		}
	}
}

// TestScopeDoesNotMutateCallerAST: the engine hands the caller's AST back and
// callers fan one AST out to several backends. If applyScope wrote into the
// tree, the second call would inject the scope twice.
func TestScopeDoesNotMutateCallerAST(t *testing.T) {
	e := scopeEngine(t, nil)
	q := userQuery()
	before := q.Filter
	s := Scope{"subscriptionId": "SUB-42"}

	first := mustSQL(t, e, q, s)
	second := mustSQL(t, e, q, s)

	if q.Filter != before {
		t.Error("the caller's AST was modified")
	}
	if first.SQL != second.SQL {
		t.Errorf("scope accumulated across calls:\nfirst:  %s\nsecond: %s", first.SQL, second.SQL)
	}
}

// --- adversarial: bad scope input -------------------------------------------

// TestScopeRejectsBadInput sweeps the ways a caller can get the map wrong. Each
// must fail loudly, be tagged ErrScope so a facade can answer 400, and say
// enough to fix the call.
func TestScopeRejectsBadInput(t *testing.T) {
	cases := []struct {
		name     string
		scope    Scope
		wantText string // a fragment the message must carry
	}{
		{"nil value", Scope{"userId": nil}, "nil value"},
		{"nil pointer", Scope{"userId": (*string)(nil)}, "unsupported value type"},
		{"empty list", Scope{"userId": []string{}}, "empty list"},
		{"typed nil slice", Scope{"userId": []string(nil)}, "empty list"},
		{"empty key", Scope{"": "x"}, "empty"},
		{"whitespace key", Scope{"   ": "x"}, "empty"},
		{"duplicate after trim", Scope{"userId": 1, " userId": 2}, "both name field"},
		{"struct value", Scope{"userId": struct{ A int }{1}}, "unsupported value type"},
		{"map value", Scope{"userId": map[string]string{"a": "b"}}, "unsupported value type"},
		{"byte slice", Scope{"userId": []byte("abc")}, "unsupported value type"},
		{"nested list", Scope{"userId": []any{[]string{"a"}}}, "unsupported value type"},
		{"nil in list", Scope{"userId": []any{"a", nil}}, "unsupported value type"},
		{"bad enum", Scope{"status": "NOT_A_STATUS"}, "not a valid value"},
		{"wrong type on declared field", Scope{"amount": "not-a-number"}, "not compatible"},
		{"below declared minimum", Scope{"amount": -5}, "below minimum"},
		{"above declared maximum", Scope{"amount": 999999}, "above maximum"},
		{"bad enum inside a list", Scope{"status": []string{"DELIVERED", "NOPE"}}, "not a valid value"},

		// Scope KEYS, not values. Everything above this line checks the shape of
		// a value; the key was checked only for emptiness and duplication, which
		// is how SECURITY-T.md S-1 (and BUGS-T.md QF-T-001) got in: an undeclared
		// key is passed through by PhysicalName and written into the statement as
		// an identifier, where no placeholder can stand for it. The first case is
		// the one that matters most — it does not merely inject, it comments out
		// the rest of the WHERE clause, taking the forced tenant predicate with
		// it.
		{"sql injection in key", Scope{"tenant_id = 'X' OR 1=1 --": "T-1"}, "not a valid field name"},
		{"quote in key", Scope{`userId" = "x`: "T-1"}, "not a valid field name"},
		{"statement terminator in key", Scope{"userId; DROP TABLE orders": "T-1"}, "not a valid field name"},
		{"comment opener in key", Scope{"userId/*": "T-1"}, "not a valid field name"},
		{"space in key", Scope{"user id": "T-1"}, "not a valid field name"},
		{"mongo operator key", Scope{"$where": "T-1"}, "not a valid field name"},
		{"mongo operator in dotted key", Scope{"user.$ne": "T-1"}, "not a valid field name"},
		{"NUL in key", Scope{"userId\x00": "T-1"}, "not a valid field name"},
		{"newline in key", Scope{"userId\nOR 1=1": "T-1"}, "not a valid field name"},
		{"leading digit in key", Scope{"1userId": "T-1"}, "not a valid field name"},
		{"empty dot segment", Scope{"user..id": "T-1"}, "not a valid field name"},
		{"trailing dot", Scope{"userId.": "T-1"}, "not a valid field name"},
		{"hyphen in key", Scope{"user-id": "T-1"}, "not a valid field name"},
		{"backtick in key", Scope{"userId`": "T-1"}, "not a valid field name"},
	}

	e := scopeEngine(t, nil)
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.GenerateFrom(NewQuery("Order"), "sql", tc.scope)
			if err == nil {
				t.Fatalf("expected an error for scope %#v", tc.scope)
			}
			if !errors.Is(err, ErrScope) {
				t.Errorf("error not tagged ErrScope: %v", err)
			}
			if !strings.Contains(err.Error(), tc.wantText) {
				t.Errorf("message %q does not mention %q", err.Error(), tc.wantText)
			}
		})
	}
}

// TestBadScopeFailsBeforeTheModelCall: a malformed scope is a bug in the calling
// code. Spending an API call before reporting it wastes quota and makes the
// error look like the question's fault.
func TestBadScopeFailsBeforeTheModelCall(t *testing.T) {
	stub := &scriptedProvider{responses: []string{`{"entity":"Order"}`}}
	e := scopeEngine(t, stub)

	_, err := e.Translate(context.Background(), "delivered orders", "sql", Scope{"userId": nil})
	if err == nil {
		t.Fatal("expected an error")
	}
	if !errors.Is(err, ErrScope) {
		t.Errorf("error not tagged ErrScope: %v", err)
	}
	if stub.calls != 0 {
		t.Errorf("the model was called %d time(s) despite an invalid scope", stub.calls)
	}
}

// TestScopeErrorIsNotConfusedWithValidation: ErrScope must not fire for an
// ordinary AST validation failure, or a facade would report the caller's scope
// as broken when the model was at fault.
func TestScopeErrorIsNotConfusedWithValidation(t *testing.T) {
	bad := NewQuery("Order")
	bad.Filter = comp("nosuchfield", OpEquals, vStr("x"))
	e := scopeEngine(t, nil)

	_, err := e.GenerateFrom(bad, "sql", Scope{"subscriptionId": "SUB-42"})
	if err == nil {
		t.Fatal("expected a validation error")
	}
	if errors.Is(err, ErrScope) {
		t.Errorf("an AST validation failure was tagged ErrScope: %v", err)
	}
}

// TestScopeRejectedForUnknownBackend: the backend check must still come first,
// so a typo in the backend name is not reported as a scope problem.
func TestScopeRejectedForUnknownBackend(t *testing.T) {
	e := scopeEngine(t, nil)

	_, err := e.GenerateFrom(NewQuery("Order"), "cassandra", Scope{"subscriptionId": "SUB-42"})
	if err == nil || !strings.Contains(err.Error(), "unknown backend") {
		t.Errorf("expected an unknown-backend error, got: %v", err)
	}
}

// TestScopeWithManyKeysStaysBounded: a caller passing an unreasonable number of
// scope keys should still produce a correct, fully parameterized query rather
// than degrading in some surprising way.
func TestScopeWithManyKeysStaysBounded(t *testing.T) {
	s := make(Scope, 200)
	for i := 0; i < 200; i++ {
		s[string(rune('a'+i%26))+string(rune('a'+i/26))] = i
	}
	e := scopeEngine(t, nil)

	r := mustSQL(t, e, NewQuery("Order"), s)
	if len(r.Args) != len(s) {
		t.Errorf("expected %d bound args, got %d", len(s), len(r.Args))
	}
	if strings.Count(r.SQL, " AND ") != len(s)-1 {
		t.Errorf("expected %d AND joins, got %d", len(s)-1, strings.Count(r.SQL, " AND "))
	}
}

// TestScopeAppliesUnderPolicyNestingLimit: scope adds a level to the tree, and
// it is injected after validation. A user filter already at the configured depth
// limit must not start failing because the scope pushed it over.
func TestScopeAppliesUnderPolicyNestingLimit(t *testing.T) {
	c := mustParse(t, strings.Replace(scopeConfigJSON,
		`"defaults":{"limit":50,"maxLimit":500}`,
		`"defaults":{"limit":50,"maxLimit":500},"policy":{"maxNestingDepth":2}`, 1))
	e := NewWithProvider(c, nil)
	e.Now = func() time.Time { return fixedNow }

	q := NewQuery("Order") // depth 2: AND over two comparisons — exactly at the limit
	q.Filter = and(
		comp("status", OpEquals, vEnum("DELIVERED")),
		comp("amount", OpGt, vNum(100)),
	)

	if _, err := e.GenerateFrom(q, "sql", Scope{"subscriptionId": "SUB-42"}); err != nil {
		t.Errorf("scope injection broke a filter that was within the depth limit: %v", err)
	}
}
