package queryforge

import (
	"encoding/json"
	"strings"
	"testing"
)

// validatorConfigJSON is purpose-built to exercise every validation rule:
// each capability flag, the enum domain, numeric bounds, the regex-deny policy,
// the searchable gate, and a shallow nesting limit.
const validatorConfigJSON = `{
  "entity":"Order","model":{},
  "fields":[
    {"name":"status","type":"enum","values":["PLACED","DELIVERED","CANCELLED"],
     "operators":["equals","notEquals","in","notIn","isNull","isNotNull"],"synonyms":["state"]},
    {"name":"refunded","type":"boolean"},
    {"name":"createdAt","type":"date","operators":["before","after","between","isNull"]},
    {"name":"amount","type":"number","operators":["gt","lt","gte","lte","between","in"],
     "validators":{"min":0,"max":10000}},
    {"name":"tags","type":"array","itemType":"string","operators":["contains","containsAny","containsAll"]},
    {"name":"customerName","type":"string","operators":["contains","startsWith","equals","regex"],"searchable":true},
    {"name":"note","type":"string","operators":["contains","equals"],"searchable":false},
    {"name":"internalId","type":"string","queryable":false},
    {"name":"score","type":"number","sortable":false},
    {"name":"secret","type":"string","filterable":false,"returnable":false}
  ],
  "defaults":{"limit":50,"maxLimit":500},
  "policy":{"maxNestingDepth":3,"denyRegexOn":["customerName"]}
}`

// --- tiny builders keep the tests readable ---

func comp(field string, op Operator, v *Value) *Condition {
	return &Condition{Type: CondComparison, Field: field, Operator: op, Value: v}
}
func and(children ...*Condition) *Condition {
	return &Condition{Type: CondLogical, Op: OpAND, Children: children}
}
func vEnum(s string) *Value       { return &Value{Kind: KindEnum, V: s} }
func vStr(s string) *Value        { return &Value{Kind: KindString, V: s} }
func vBool(b bool) *Value         { return &Value{Kind: KindBoolean, V: b} }
func vNum(n float64) *Value       { return &Value{Kind: KindNumber, V: n} }
func vArr(items ...any) *Value    { return &Value{Kind: KindArray, V: items} }
func vRel(u string, a int) *Value { return &Value{Kind: KindRelativeDate, Unit: u, Amount: a} }

// single wraps one comparison into a Query for the adversarial table.
func single(cmp *Condition) *Query { q := NewQuery("Order"); q.Filter = cmp; return q }

func validatorConfig(t *testing.T) *Config { return mustParse(t, validatorConfigJSON) }

// TestValidAST is the happy path: a rich, fully-legal AST must pass, and it
// must still pass after a JSON round trip (numbers become float64 — the real
// planner path).
func TestValidAST(t *testing.T) {
	c := validatorConfig(t)
	q := NewQuery("Order")
	q.Filter = and(
		comp("status", OpEquals, vEnum("DELIVERED")),
		comp("refunded", OpEquals, vBool(false)),
		comp("createdAt", OpAfter, vRel("day", -30)),
		comp("tags", OpContainsAll, vArr("premium", "express")),
		comp("customerName", OpContains, vStr("john")),
		comp("amount", OpBetween, vArr(float64(10), float64(100))),
	)
	q.Sort = []SortSpec{{Field: "createdAt", Dir: "DESC"}}
	q.Select = []string{"status", "amount"}
	q.Limit = intPtr(50)

	if err := Validate(q, c); err != nil {
		t.Fatalf("valid AST rejected: %v", err)
	}

	// Same tree, but forced through JSON like the planner output would be.
	raw, _ := json.Marshal(q)
	var q2 Query
	if err := json.Unmarshal(raw, &q2); err != nil {
		t.Fatalf("reparse: %v", err)
	}
	if err := Validate(&q2, c); err != nil {
		t.Fatalf("valid AST rejected after JSON round trip: %v", err)
	}
}

// TestUnknownFieldSuggests checks the repair-loop signal: an unknown field is
// rejected and the nearest registered field is suggested.
func TestUnknownFieldSuggests(t *testing.T) {
	c := validatorConfig(t)
	err := Validate(single(comp("statuz", OpEquals, vEnum("DELIVERED"))), c)
	if err == nil {
		t.Fatal("expected rejection of unknown field")
	}
	msg := err.Error()
	if !strings.Contains(msg, "unknown field") || !strings.Contains(msg, "status") {
		t.Errorf("expected suggestion of 'status', got: %s", msg)
	}
}

// TestAdversarial is the "try to break it" table. Every entry must be rejected,
// and the message must contain the given fragment so we know it failed for the
// right reason.
func TestAdversarial(t *testing.T) {
	c := validatorConfig(t)

	// depth-4 tree against a maxNestingDepth of 3
	deep := and(and(and(comp("refunded", OpEquals, vBool(true)))))

	cases := []struct {
		name string
		q    *Query
		want string
	}{
		{"operator not allowed", single(comp("status", OpGt, vEnum("PLACED"))), "not allowed"},
		{"enum out of domain", single(comp("status", OpEquals, vEnum("SHIPPED"))), "not a valid value"},
		{"type mismatch", single(comp("amount", OpGt, vStr("lots"))), "not compatible"},
		{"between wrong arity", single(comp("amount", OpBetween, vArr(float64(1)))), "exactly 2"},
		{"between not array", single(comp("amount", OpBetween, vNum(5))), "expects an array"},
		{"null op with value", single(comp("status", OpIsNull, vEnum("PLACED"))), "takes no value"},
		{"missing value", single(comp("status", OpEquals, nil)), "requires a value"},
		{"text op not searchable", single(comp("note", OpContains, vStr("x"))), "not searchable"},
		{"regex denied", single(comp("customerName", OpRegex, vStr("^a"))), "regex is denied"},
		{"queryable false", single(comp("internalId", OpEquals, vStr("x"))), "excluded from queries"},
		{"filterable false", single(comp("secret", OpEquals, vStr("x"))), "not filterable"},
		{"array element wrong type", single(comp("tags", OpContainsAll, vArr("a", float64(5)))), "not a valid"},
		{"numeric below min", single(comp("amount", OpGt, vNum(-5))), "below minimum"},
		{"date field wrong kind", single(comp("createdAt", OpAfter, vNum(5))), "not compatible"},
		{"unknown condition type", &Query{Version: "1.0", Entity: "Order", Filter: &Condition{Type: "weird"}}, "unknown condition type"},
		{"NOT two children", &Query{Version: "1.0", Entity: "Order", Filter: &Condition{Type: CondLogical, Op: OpNOT, Children: []*Condition{comp("refunded", OpEquals, vBool(true)), comp("refunded", OpEquals, vBool(false))}}}, "exactly one child"},
		{"empty logical", &Query{Version: "1.0", Entity: "Order", Filter: and()}, "no children"},
		{"nesting too deep", &Query{Version: "1.0", Entity: "Order", Filter: deep}, "nesting depth"},
		{"unknown logical op", &Query{Version: "1.0", Entity: "Order", Filter: &Condition{Type: CondLogical, Op: "XOR", Children: []*Condition{comp("refunded", OpEquals, vBool(true))}}}, "unknown logical operator"},
	}

	for _, tc := range cases {
		err := Validate(tc.q, c)
		if err == nil {
			t.Errorf("%s: expected rejection, got none", tc.name)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: message %q does not contain %q", tc.name, err.Error(), tc.want)
		}
	}
}

// TestSortAndSelectRules covers projection/sort capability + paging rejections.
func TestSortAndSelectRules(t *testing.T) {
	c := validatorConfig(t)

	// sort on a non-sortable field
	q := NewQuery("Order")
	q.Sort = []SortSpec{{Field: "score", Dir: "ASC"}}
	if err := Validate(q, c); err == nil || !strings.Contains(err.Error(), "not sortable") {
		t.Errorf("expected 'not sortable', got %v", err)
	}

	// invalid sort direction
	q = NewQuery("Order")
	q.Sort = []SortSpec{{Field: "amount", Dir: "SIDEWAYS"}}
	if err := Validate(q, c); err == nil || !strings.Contains(err.Error(), "invalid sort direction") {
		t.Errorf("expected direction error, got %v", err)
	}

	// select a non-returnable field
	q = NewQuery("Order")
	q.Select = []string{"secret"}
	if err := Validate(q, c); err == nil || !strings.Contains(err.Error(), "not returnable") {
		t.Errorf("expected 'not returnable', got %v", err)
	}

	// select an unknown field
	q = NewQuery("Order")
	q.Select = []string{"nope"}
	if err := Validate(q, c); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Errorf("expected 'unknown field', got %v", err)
	}

	// negative limit
	q = NewQuery("Order")
	q.Limit = intPtr(-1)
	if err := Validate(q, c); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Errorf("expected negative-limit error, got %v", err)
	}

	// limit above ceiling
	q = NewQuery("Order")
	q.Limit = intPtr(9999)
	if err := Validate(q, c); err == nil || !strings.Contains(err.Error(), "exceeds maxLimit") {
		t.Errorf("expected maxLimit error, got %v", err)
	}
}

// TestEntityMismatch guards the entity check.
func TestEntityMismatch(t *testing.T) {
	c := validatorConfig(t)
	q := NewQuery("Customer") // wrong entity
	if err := Validate(q, c); err == nil || !strings.Contains(err.Error(), "does not match config entity") {
		t.Errorf("expected entity mismatch, got %v", err)
	}
}

// TestNilQuery is the ultimate worst case.
func TestNilQuery(t *testing.T) {
	c := validatorConfig(t)
	if err := Validate(nil, c); err == nil {
		t.Error("nil query must be rejected")
	}
}

// TestMultipleErrorsCollected confirms the validator reports all problems at
// once (useful context for the repair loop), not just the first.
func TestMultipleErrorsCollected(t *testing.T) {
	c := validatorConfig(t)
	q := NewQuery("Order")
	q.Filter = and(
		comp("bogus1", OpEquals, vStr("x")),
		comp("bogus2", OpEquals, vStr("y")),
	)
	err := Validate(q, c)
	if err == nil {
		t.Fatal("expected errors")
	}
	if !strings.Contains(err.Error(), "bogus1") || !strings.Contains(err.Error(), "bogus2") {
		t.Errorf("expected both errors, got: %s", err.Error())
	}
}
