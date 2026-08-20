package queryforge

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// esConfigJSON is the base fixture for DSL-generation tests: a direct index,
// a keyword sub-field on customerName (text search + exact/sort), and a
// nested "items" object with two fields inside it.
const esConfigJSON = `{
  "entity":"Order","model":{},
  "backends":{"elasticsearch":{"index":"orders"}},
  "fields":[
    {"name":"status","type":"enum","values":["PLACED","DELIVERED","CANCELLED"],
     "operators":["equals","notEquals","in","notIn","isNull","isNotNull"],
     "mapping":{"elasticsearch":"status"}},
    {"name":"createdAt","type":"date","operators":["before","after","between","equals"],
     "mapping":{"elasticsearch":"createdAt"}},
    {"name":"amount","type":"number","operators":["gt","lt","gte","lte","between","in"],
     "mapping":{"elasticsearch":"amount"}},
    {"name":"tags","type":"array","itemType":"string","operators":["contains","containsAny","containsAll"],
     "mapping":{"elasticsearch":"tags"}},
    {"name":"customerName","type":"string","operators":["contains","startsWith","endsWith","equals","regex"],
     "searchable":true,"mapping":{"elasticsearch":"customerName"},
     "keywordMapping":{"elasticsearch":"customerName.keyword"}},
    {"name":"region","type":"string","caseInsensitive":true,"operators":["equals","in"],
     "mapping":{"elasticsearch":"region"}},
    {"name":"sku","type":"string","operators":["equals"],"nestedPath":"items",
     "mapping":{"elasticsearch":"items.sku"}},
    {"name":"qty","type":"number","operators":["gt","equals"],"nestedPath":"items",
     "mapping":{"elasticsearch":"items.qty"}}
  ],
  "defaults":{"limit":50,"maxLimit":500}
}`

func esConfig(t *testing.T) *Config { return mustParse(t, esConfigJSON) }

func genES(t *testing.T, c *Config, q *Query) *ESQuery {
	t.Helper()
	r, err := ESGenerator{}.Generate(q, c, GenOptions{Now: fixedNow})
	if err != nil {
		t.Fatalf("es generate: %v", err)
	}
	return r.Doc.(*ESQuery)
}

func vDate(s string) *Value { return &Value{Kind: KindDate, V: s} }

// TestESDirectIndex checks the resolved path/index/sourceType for the plain
// single-index case.
func TestESDirectIndex(t *testing.T) {
	c := esConfig(t)
	q := NewQuery("Order")
	eq := genES(t, c, q)
	if !reflect.DeepEqual(eq.Index, []string{"orders"}) {
		t.Errorf("index = %v", eq.Index)
	}
	if eq.Path != "/orders/_search" {
		t.Errorf("path = %q", eq.Path)
	}
	if eq.SourceType != SourceDirectIndex {
		t.Errorf("sourceType = %q", eq.SourceType)
	}
	if eq.Method != "GET" {
		t.Errorf("method = %q", eq.Method)
	}
	if !reflect.DeepEqual(eq.Query, map[string]any{"match_all": map[string]any{}}) {
		t.Errorf("query = %#v", eq.Query)
	}
}

// TestESMultipleIndexAndAlias checks the other two static source modes.
func TestESMultipleIndexAndAlias(t *testing.T) {
	multi := mustParse(t, `{"entity":"Order","model":{},
		"backends":{"elasticsearch":{"indexes":["orders-2025","orders-2026"]}},
		"fields":[{"name":"status","type":"string"}]}`)
	eq := genES(t, multi, NewQuery("Order"))
	if !reflect.DeepEqual(eq.Index, []string{"orders-2025", "orders-2026"}) {
		t.Errorf("index = %v", eq.Index)
	}
	if eq.Path != "/orders-2025,orders-2026/_search" {
		t.Errorf("path = %q", eq.Path)
	}
	if eq.SourceType != SourceMultipleIndex {
		t.Errorf("sourceType = %q", eq.SourceType)
	}

	alias := mustParse(t, `{"entity":"Order","model":{},
		"backends":{"elasticsearch":{"alias":"orders-alias"}},
		"fields":[{"name":"status","type":"string"}]}`)
	eq = genES(t, alias, NewQuery("Order"))
	if !reflect.DeepEqual(eq.Index, []string{"orders-alias"}) || eq.SourceType != SourceAlias {
		t.Errorf("alias resolution wrong: index=%v sourceType=%q", eq.Index, eq.SourceType)
	}
	if eq.Path != "/orders-alias/_search" {
		t.Errorf("alias path = %q", eq.Path)
	}
}

// TestESKeywordVsTextPath is the core of the text/keyword design: equals
// (and every other non-contains operator) must hit the keyword sub-field,
// and contains must hit the analyzed text field.
func TestESKeywordVsTextPath(t *testing.T) {
	c := esConfig(t)

	eq := genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("customerName", OpEquals, vStr("John Smith"))})
	term, ok := eq.Query["term"].(map[string]any)
	if !ok || term["customerName.keyword"] != "John Smith" {
		t.Errorf("equals should hit the keyword path: %#v", eq.Query)
	}

	eq = genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("customerName", OpContains, vStr("John"))})
	match, ok := eq.Query["match"].(map[string]any)
	if !ok || match["customerName"] != "John" {
		t.Errorf("contains should hit the text path: %#v", eq.Query)
	}

	eq = genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("customerName", OpStartsWith, vStr("Jo"))})
	prefix, ok := eq.Query["prefix"].(map[string]any)
	if !ok {
		t.Fatalf("startsWith should render a prefix query: %#v", eq.Query)
	}
	inner, ok := prefix["customerName.keyword"].(map[string]any)
	if !ok || inner["value"] != "Jo" {
		t.Errorf("startsWith should hit the keyword path: %#v", prefix)
	}
}

// TestESCaseInsensitive checks both the native case_insensitive param path
// (term, via equals) and the OR-of-terms fallback (terms, via `in`, since ES
// has no case_insensitive parameter on the terms query).
func TestESCaseInsensitive(t *testing.T) {
	c := esConfig(t)

	eq := genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("region", OpEquals, vStr("us"))})
	term, ok := eq.Query["term"].(map[string]any)
	if !ok {
		t.Fatalf("expected a term query: %#v", eq.Query)
	}
	inner, ok := term["region"].(map[string]any)
	if !ok || inner["value"] != "us" || inner["case_insensitive"] != true {
		t.Errorf("equals on a caseInsensitive field should set case_insensitive: %#v", term)
	}

	eq = genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("region", OpIn, vArr("us", "eu"))})
	boolQ, ok := eq.Query["bool"].(map[string]any)
	if !ok {
		t.Fatalf("case-insensitive `in` should render bool.should: %#v", eq.Query)
	}
	should, ok := boolQ["should"].([]any)
	if !ok || len(should) != 2 {
		t.Fatalf("expected 2 should branches: %#v", boolQ)
	}
}

// TestESRangeAndBetween checks range-operator DSL and the inclusive
// after/before convention shared with every other generator.
func TestESRangeAndBetween(t *testing.T) {
	c := esConfig(t)

	eq := genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("amount", OpBetween, &Value{Kind: KindArray, V: []any{100.0, 500.0}})})
	rng, ok := eq.Query["range"].(map[string]any)
	if !ok {
		t.Fatalf("expected a range query: %#v", eq.Query)
	}
	amt, ok := rng["amount"].(map[string]any)
	if !ok || amt["gte"] != 100.0 || amt["lte"] != 500.0 {
		t.Errorf("between range wrong: %#v", amt)
	}

	eq = genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("createdAt", OpAfter, vRel("day", -30))})
	rng = eq.Query["range"].(map[string]any)
	created, ok := rng["createdAt"].(map[string]any)
	want, _ := resolveRelative(fixedNow, "day", -30)
	if !ok || created["gte"] != want.Format(time.RFC3339) {
		t.Errorf("after should be inclusive (gte): %#v", created)
	}
}

// TestESLogical checks AND/OR/NOT rendering and predicate ordering.
func TestESLogical(t *testing.T) {
	c := esConfig(t)
	q := &Query{Version: ASTVersion, Entity: "Order", Filter: and(
		comp("status", OpEquals, vEnum("DELIVERED")),
		comp("amount", OpGt, vNum(100)),
	)}
	eq := genES(t, c, q)
	boolQ, ok := eq.Query["bool"].(map[string]any)
	if !ok {
		t.Fatalf("AND should render bool.filter: %#v", eq.Query)
	}
	filter, ok := boolQ["filter"].([]any)
	if !ok || len(filter) != 2 {
		t.Fatalf("expected 2 filter clauses: %#v", boolQ)
	}

	q = &Query{Version: ASTVersion, Entity: "Order", Filter: or(
		comp("status", OpEquals, vEnum("DELIVERED")),
		comp("status", OpEquals, vEnum("PLACED")),
	)}
	eq = genES(t, c, q)
	boolQ = eq.Query["bool"].(map[string]any)
	if boolQ["minimum_should_match"] != 1 {
		t.Errorf("OR should set minimum_should_match:1: %#v", boolQ)
	}
	if should, ok := boolQ["should"].([]any); !ok || len(should) != 2 {
		t.Errorf("expected 2 should clauses: %#v", boolQ)
	}

	q = &Query{Version: ASTVersion, Entity: "Order", Filter: not(
		comp("status", OpEquals, vEnum("CANCELLED")),
	)}
	eq = genES(t, c, q)
	boolQ = eq.Query["bool"].(map[string]any)
	if mustNot, ok := boolQ["must_not"].([]any); !ok || len(mustNot) != 1 {
		t.Errorf("NOT should render bool.must_not: %#v", boolQ)
	}
}

// TestESNestedFolding is the ElemMatch-equivalent test: two sibling AND
// predicates on the same nested path must fold into ONE nested query, so
// "sku ABC costing over 100" cannot match on two different array elements.
func TestESNestedFolding(t *testing.T) {
	c := esConfig(t)
	q := &Query{Version: ASTVersion, Entity: "Order", Filter: and(
		comp("sku", OpEquals, vStr("ABC")),
		comp("qty", OpGt, vNum(10)),
		comp("status", OpEquals, vEnum("PLACED")), // not nested: must stay outside the fold
	)}
	eq := genES(t, c, q)
	boolQ, ok := eq.Query["bool"].(map[string]any)
	if !ok {
		t.Fatalf("expected bool.filter: %#v", eq.Query)
	}
	filter, ok := boolQ["filter"].([]any)
	if !ok || len(filter) != 2 { // one folded nested clause + the plain status clause
		t.Fatalf("expected 2 top-level filter clauses (1 nested + 1 plain), got %d: %#v", len(filter), filter)
	}

	var nested map[string]any
	for _, f := range filter {
		if m, isMap := f.(map[string]any); isMap {
			if n, isNested := m["nested"].(map[string]any); isNested {
				nested = n
			}
		}
	}
	if nested == nil {
		t.Fatalf("no nested clause found: %#v", filter)
	}
	if nested["path"] != "items" {
		t.Errorf("nested path = %v", nested["path"])
	}
	inner, ok := nested["query"].(map[string]any)
	if !ok {
		t.Fatalf("nested query missing: %#v", nested)
	}
	innerBool, ok := inner["bool"].(map[string]any)
	if !ok {
		t.Fatalf("folded nested predicates should be bool.filter: %#v", inner)
	}
	innerFilter, ok := innerBool["filter"].([]any)
	if !ok || len(innerFilter) != 2 {
		t.Fatalf("expected 2 folded predicates inside the nested query, got %#v", innerBool)
	}
}

// TestESNestedSingleNotFolded checks a lone nested predicate (no sibling on
// the same path) is wrapped directly, without an unnecessary bool wrapper.
func TestESNestedSingleNotFolded(t *testing.T) {
	c := esConfig(t)
	q := &Query{Version: ASTVersion, Entity: "Order", Filter: comp("sku", OpEquals, vStr("ABC"))}
	eq := genES(t, c, q)
	nested, ok := eq.Query["nested"].(map[string]any)
	if !ok || nested["path"] != "items" {
		t.Fatalf("expected a nested wrapper: %#v", eq.Query)
	}
	inner, ok := nested["query"].(map[string]any)
	if !ok {
		t.Fatalf("nested query missing: %#v", nested)
	}
	if _, isBool := inner["bool"]; isBool {
		t.Errorf("a lone nested predicate should not be wrapped in an extra bool: %#v", inner)
	}
	if _, isTerm := inner["term"]; !isTerm {
		t.Errorf("expected the bare term clause: %#v", inner)
	}
}

// TestESNegationRequiresExistence pins the same cross-backend guarantee
// gen_mongo.go documents at length: a document missing the field entirely
// must not satisfy a negated predicate, matching SQL's three-valued NULL
// logic (`status <> $1` is NULL, not TRUE, for a NULL status). notEquals,
// notIn, and a logical NOT over a plain comparison must each carry an
// exists guard alongside their must_not.
func TestESNegationRequiresExistence(t *testing.T) {
	c := esConfig(t)

	assertGuarded := func(t *testing.T, boolQ map[string]any) {
		t.Helper()
		filter, ok := boolQ["filter"].([]any)
		if !ok || len(filter) != 1 {
			t.Fatalf("expected an exists guard in filter: %#v", boolQ)
		}
		m, ok := filter[0].(map[string]any)
		if !ok {
			t.Fatalf("guard is not a map: %#v", filter[0])
		}
		if _, ok := m["exists"]; !ok {
			t.Errorf("guard is not an exists clause: %#v", m)
		}
	}

	eq := genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("status", OpNotEquals, vEnum("CANCELLED"))})
	assertGuarded(t, eq.Query["bool"].(map[string]any))

	eq = genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("status", OpNotIn, vArr("CANCELLED", "PLACED"))})
	assertGuarded(t, eq.Query["bool"].(map[string]any))

	eq = genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: not(comp("status", OpEquals, vEnum("CANCELLED")))})
	assertGuarded(t, eq.Query["bool"].(map[string]any))

	// isNull/isNotNull are themselves about absence and must NOT get an
	// extra guard layered on top.
	eq = genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: not(comp("status", OpIsNotNull, nil))})
	if _, has := eq.Query["bool"].(map[string]any)["filter"]; has {
		t.Errorf("NOT(isNotNull) must not get an exists guard: %#v", eq.Query)
	}
}

// TestESProjectionAndSort checks _source, sort (using the keyword path), and
// size/from.
func TestESProjectionAndSort(t *testing.T) {
	c := esConfig(t)
	limit := 25
	offset := 10
	q := &Query{Version: ASTVersion, Entity: "Order",
		Select: []string{"status", "amount"},
		Sort:   []SortSpec{{Field: "customerName", Dir: "DESC"}},
		Limit:  &limit, Offset: &offset,
	}
	eq := genES(t, c, q)
	if !reflect.DeepEqual(eq.Source, []string{"status", "amount"}) {
		t.Errorf("_source = %v", eq.Source)
	}
	if len(eq.Sort) != 1 {
		t.Fatalf("expected 1 sort clause: %v", eq.Sort)
	}
	sortField, ok := eq.Sort[0]["customerName.keyword"].(map[string]any)
	if !ok || sortField["order"] != "desc" {
		t.Errorf("sort should use the keyword path: %#v", eq.Sort[0])
	}
	if eq.Size != 25 || eq.From != 10 {
		t.Errorf("size/from = %d/%d", eq.Size, eq.From)
	}
}

// ---- business-rule index routing ----

const patternRoutingJSON = `{
  "entity":"Order","model":{},
  "backends":{"elasticsearch":{"routing":{
    "strategy":"pattern","field":"tenantId",
    "indexPattern":"tenant-{tenantId}-orders",
    "default":["tenant-default-orders"]
  }}},
  "fields":[
    {"name":"tenantId","type":"string","routingField":true,"operators":["equals"]},
    {"name":"status","type":"string","operators":["equals"]}
  ]
}`

func TestESPatternRouting(t *testing.T) {
	c := mustParse(t, patternRoutingJSON)

	eq := genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("tenantId", OpEquals, vStr("acme"))})
	if !reflect.DeepEqual(eq.Index, []string{"tenant-acme-orders"}) {
		t.Errorf("index = %v", eq.Index)
	}
	if eq.SourceType != SourceBusinessRule {
		t.Errorf("sourceType = %q", eq.SourceType)
	}

	// Field absent from the query: falls back to Default.
	eq = genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("status", OpEquals, vStr("PLACED"))})
	if !reflect.DeepEqual(eq.Index, []string{"tenant-default-orders"}) {
		t.Errorf("expected default fallback, got %v", eq.Index)
	}

	// Field only constrained inside an OR: not sound to route on, falls back.
	eq = genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: or(comp("tenantId", OpEquals, vStr("acme")), comp("status", OpEquals, vStr("PLACED")))})
	if !reflect.DeepEqual(eq.Index, []string{"tenant-default-orders"}) {
		t.Errorf("OR-scoped routing field must not drive resolution, got %v", eq.Index)
	}
}

// TestESPatternRoutingRejectsUnsafeValue is the security check: a routing
// field's value that would compose into an invalid/unsafe index name is
// rejected rather than silently written into the resolved index.
func TestESPatternRoutingRejectsUnsafeValue(t *testing.T) {
	c := mustParse(t, patternRoutingJSON)
	_, err := ESGenerator{}.Generate(&Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("tenantId", OpEquals, vStr("acme/prod"))}, c, GenOptions{Now: fixedNow})
	if err == nil {
		t.Fatalf("expected the unsafe composed index name to be rejected")
	}
}

const dateRoutingJSON = `{
  "entity":"Order","model":{},
  "backends":{"elasticsearch":{"routing":{
    "strategy":"date","field":"createdAt","granularity":"MONTH",
    "indexPattern":"orders-{yyyy-MM}",
    "default":["orders-legacy"]
  }}},
  "fields":[
    {"name":"createdAt","type":"date","routingField":true,"operators":["equals","between"]}
  ]
}`

func TestESDateRoutingEquals(t *testing.T) {
	c := mustParse(t, dateRoutingJSON)
	eq := genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("createdAt", OpEquals, vDate("2026-08-20"))})
	if !reflect.DeepEqual(eq.Index, []string{"orders-2026-08"}) {
		t.Errorf("index = %v", eq.Index)
	}
}

// TestESDateRoutingBetween pins the spec's own worked example: a range
// spanning four calendar months resolves to all four partitions, inclusive.
func TestESDateRoutingBetween(t *testing.T) {
	c := mustParse(t, dateRoutingJSON)
	eq := genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("createdAt", OpBetween, &Value{Kind: KindArray, V: []any{"2025-11-15", "2026-02-10"}})})
	want := []string{"orders-2025-11", "orders-2025-12", "orders-2026-01", "orders-2026-02"}
	if !reflect.DeepEqual(eq.Index, want) {
		t.Errorf("index = %v, want %v", eq.Index, want)
	}
}

// TestESDateRoutingUnboundedFallsBack checks that an unbounded before/after
// on the routing field does NOT expand to an unrestricted partition list — it
// falls back to Default instead, same as an absent field.
func TestESDateRoutingUnboundedFallsBack(t *testing.T) {
	c := mustParse(t, dateRoutingJSON)
	eq := genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("createdAt", OpAfter, vDate("2020-01-01"))})
	if !reflect.DeepEqual(eq.Index, []string{"orders-legacy"}) {
		t.Errorf("unbounded date routing should fall back to default, got %v", eq.Index)
	}
}

const rulesRoutingJSON = `{
  "entity":"Order","model":{},
  "backends":{"elasticsearch":{"routing":{
    "strategy":"rules","default":["orders-global"],
    "rules":[
      {"when":{"field":"region","operator":"equals","value":"US"},"indexes":["orders-us"]},
      {"when":{"field":"region","operator":"equals","value":"EU"},"indexes":["orders-eu"]},
      {"when":{"field":"amount","operator":"gt","value":"1000"},"indexes":["orders-big"],"priority":1},
      {"when":{"field":"amount","operator":"gt","value":"500"},"indexes":["orders-medium"]}
    ]
  }}},
  "fields":[
    {"name":"region","type":"string","routingField":true,"operators":["equals"]},
    {"name":"amount","type":"number","routingField":true,"operators":["gt"]}
  ]
}`

func TestESRulesRouting(t *testing.T) {
	c := mustParse(t, rulesRoutingJSON)

	eq := genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("region", OpEquals, vStr("US"))})
	if !reflect.DeepEqual(eq.Index, []string{"orders-us"}) {
		t.Errorf("index = %v", eq.Index)
	}

	// No rule matches: falls back to default.
	eq = genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("region", OpEquals, vStr("FR"))})
	if !reflect.DeepEqual(eq.Index, []string{"orders-global"}) {
		t.Errorf("index = %v", eq.Index)
	}

	// Two rules match (amount>1000 AND amount>500); priority 1 beats 0.
	eq = genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("amount", OpGt, vNum(2000))})
	if !reflect.DeepEqual(eq.Index, []string{"orders-big"}) {
		t.Errorf("higher-priority rule should win, got %v", eq.Index)
	}
}

// TestESRulesRoutingNotEquals checks the notEquals routing operator.
func TestESRulesRoutingNotEquals(t *testing.T) {
	c := mustParse(t, `{
	  "entity":"Order","model":{},
	  "backends":{"elasticsearch":{"routing":{
	    "strategy":"rules","default":["orders-global"],
	    "rules":[{"when":{"field":"region","operator":"notEquals","value":"US"},"indexes":["orders-non-us"]}]
	  }}},
	  "fields":[{"name":"region","type":"string","routingField":true,"operators":["equals"]}]
	}`)
	eq := genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("region", OpEquals, vStr("FR"))})
	if !reflect.DeepEqual(eq.Index, []string{"orders-non-us"}) {
		t.Errorf("notEquals rule should match a different region: index = %v", eq.Index)
	}
	eq = genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("region", OpEquals, vStr("US"))})
	if !reflect.DeepEqual(eq.Index, []string{"orders-global"}) {
		t.Errorf("notEquals rule should not match the excluded region: index = %v", eq.Index)
	}
}

// TestESRulesRoutingAmbiguous checks that two rules matching at the SAME
// priority is a resolution error, not a guess.
func TestESRulesRoutingAmbiguous(t *testing.T) {
	c := mustParse(t, `{
	  "entity":"Order","model":{},
	  "backends":{"elasticsearch":{"routing":{
	    "strategy":"rules","default":["orders-global"],
	    "rules":[
	      {"when":{"field":"amount","operator":"gt","value":"1000"},"indexes":["orders-a"]},
	      {"when":{"field":"amount","operator":"gt","value":"500"},"indexes":["orders-b"]}
	    ]
	  }}},
	  "fields":[{"name":"amount","type":"number","routingField":true,"operators":["gt"]}]
	}`)
	_, err := ESGenerator{}.Generate(&Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("amount", OpGt, vNum(2000))}, c, GenOptions{Now: fixedNow})
	if err == nil || !strings.Contains(err.Error(), "ambiguous") {
		t.Errorf("expected an ambiguous-routing error, got: %v", err)
	}
}

const ifElseRoutingJSON = `{
  "entity":"Order","model":{},
  "backends":{"elasticsearch":{"routing":{
    "strategy":"ifElse","default":["orders-fallback"],
    "branches":[
      {"if":{"field":"createdAt","operator":"gte","value":"2026-01-01"},"indexes":["orders-2026"]},
      {"else":true,"indexes":["orders-legacy"]}
    ]
  }}},
  "fields":[{"name":"createdAt","type":"date","routingField":true,"operators":["equals"]}]
}`

func TestESIfElseRouting(t *testing.T) {
	c := mustParse(t, ifElseRoutingJSON)

	eq := genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("createdAt", OpEquals, vDate("2026-06-01"))})
	if !reflect.DeepEqual(eq.Index, []string{"orders-2026"}) {
		t.Errorf("index = %v", eq.Index)
	}

	eq = genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("createdAt", OpEquals, vDate("2024-06-01"))})
	if !reflect.DeepEqual(eq.Index, []string{"orders-legacy"}) {
		t.Errorf("expected the else branch, got %v", eq.Index)
	}

	// Field absent entirely: no branch (including else, which still requires
	// evaluation order but always matches) — else still wins here since it is
	// unconditional; Default only applies when even else is absent.
	eq = genES(t, c, &Query{Version: ASTVersion, Entity: "Order"})
	if !reflect.DeepEqual(eq.Index, []string{"orders-legacy"}) {
		t.Errorf("else branch should match unconditionally, got %v", eq.Index)
	}
}

// TestESIfElseRoutingNoElseFallsBackToDefault checks Default is used when no
// branch matches and there is no else branch.
func TestESIfElseRoutingNoElseFallsBackToDefault(t *testing.T) {
	c := mustParse(t, `{
	  "entity":"Order","model":{},
	  "backends":{"elasticsearch":{"routing":{
	    "strategy":"ifElse","default":["orders-fallback"],
	    "branches":[{"if":{"field":"createdAt","operator":"gte","value":"2026-01-01"},"indexes":["orders-2026"]}]
	  }}},
	  "fields":[{"name":"createdAt","type":"date","routingField":true,"operators":["equals"]}]
	}`)
	eq := genES(t, c, &Query{Version: ASTVersion, Entity: "Order",
		Filter: comp("createdAt", OpEquals, vDate("2024-06-01"))})
	if !reflect.DeepEqual(eq.Index, []string{"orders-fallback"}) {
		t.Errorf("index = %v", eq.Index)
	}
}

// TestOpenSearchSharesTheSameCompiler is a spot check that OpenSearchGenerator
// produces byte-identical DSL to ESGenerator for the same AST — the whole
// point of sharing one compiler between the two registry entries.
func TestOpenSearchSharesTheSameCompiler(t *testing.T) {
	c := esConfig(t)
	q := &Query{Version: ASTVersion, Entity: "Order", Filter: and(
		comp("status", OpEquals, vEnum("DELIVERED")),
		comp("amount", OpGt, vNum(100)),
	)}
	es, err := ESGenerator{}.Generate(q, c, GenOptions{Now: fixedNow})
	if err != nil {
		t.Fatalf("es generate: %v", err)
	}
	os, err := OpenSearchGenerator{}.Generate(q, c, GenOptions{Now: fixedNow})
	if err != nil {
		t.Fatalf("opensearch generate: %v", err)
	}
	esq, osq := es.Doc.(*ESQuery), os.Doc.(*ESQuery)
	if !reflect.DeepEqual(esq.Query, osq.Query) {
		t.Errorf("elasticsearch and opensearch DSL diverged:\nes: %#v\nos: %#v", esq.Query, osq.Query)
	}
	if es.Backend != "elasticsearch" || os.Backend != "opensearch" {
		t.Errorf("backend ids wrong: %q / %q", es.Backend, os.Backend)
	}
}
