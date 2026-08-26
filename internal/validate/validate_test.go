package validate_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/awsaman-ai/queryforge/internal/testutil"

	. "github.com/awsaman-ai/queryforge/internal/ast"
	. "github.com/awsaman-ai/queryforge/internal/config"
	. "github.com/awsaman-ai/queryforge/internal/validate"
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

func validatorConfig(t *testing.T) *Config { return testutil.MustParse(t, validatorConfigJSON) }

// TestValidAST is the happy path: a rich, fully-legal AST must pass, and it
// must still pass after a JSON round trip (numbers become float64 — the real
// planner path).
func TestValidAST(t *testing.T) {
	c := validatorConfig(t)
	q := NewQuery("Order")
	q.Filter = testutil.And(
		testutil.Comp("status", OpEquals, testutil.VEnum("DELIVERED")),
		testutil.Comp("refunded", OpEquals, testutil.VBool(false)),
		testutil.Comp("createdAt", OpAfter, testutil.VRel("day", -30)),
		testutil.Comp("tags", OpContainsAll, testutil.VArr("premium", "express")),
		testutil.Comp("customerName", OpContains, testutil.VStr("john")),
		testutil.Comp("amount", OpBetween, testutil.VArr(float64(10), float64(100))),
	)
	q.Sort = []SortSpec{{Field: "createdAt", Dir: "DESC"}}
	q.Select = []string{"status", "amount"}
	q.Limit = IntPtr(50)

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
	err := Validate(testutil.Single(testutil.Comp("statuz", OpEquals, testutil.VEnum("DELIVERED"))), c)
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
	deep := testutil.And(testutil.And(testutil.And(testutil.Comp("refunded", OpEquals, testutil.VBool(true)))))

	cases := []struct {
		name string
		q    *Query
		want string
	}{
		{"operator not allowed", testutil.Single(testutil.Comp("status", OpGt, testutil.VEnum("PLACED"))), "not allowed"},
		{"enum out of domain", testutil.Single(testutil.Comp("status", OpEquals, testutil.VEnum("SHIPPED"))), "not a valid value"},
		{"type mismatch", testutil.Single(testutil.Comp("amount", OpGt, testutil.VStr("lots"))), "not compatible"},
		{"between wrong arity", testutil.Single(testutil.Comp("amount", OpBetween, testutil.VArr(float64(1)))), "exactly 2"},
		{"between not array", testutil.Single(testutil.Comp("amount", OpBetween, testutil.VNum(5))), "expects an array"},
		{"null op with value", testutil.Single(testutil.Comp("status", OpIsNull, testutil.VEnum("PLACED"))), "takes no value"},
		{"missing value", testutil.Single(testutil.Comp("status", OpEquals, nil)), "requires a value"},
		{"text op not searchable", testutil.Single(testutil.Comp("note", OpContains, testutil.VStr("x"))), "not searchable"},
		{"regex denied", testutil.Single(testutil.Comp("customerName", OpRegex, testutil.VStr("^a"))), "regex is denied"},
		{"queryable false", testutil.Single(testutil.Comp("internalId", OpEquals, testutil.VStr("x"))), "excluded from queries"},
		{"filterable false", testutil.Single(testutil.Comp("secret", OpEquals, testutil.VStr("x"))), "not filterable"},
		{"array element wrong type", testutil.Single(testutil.Comp("tags", OpContainsAll, testutil.VArr("a", float64(5)))), "not a valid"},
		{"numeric below min", testutil.Single(testutil.Comp("amount", OpGt, testutil.VNum(-5))), "below minimum"},
		{"date field wrong kind", testutil.Single(testutil.Comp("createdAt", OpAfter, testutil.VNum(5))), "not compatible"},
		{"unknown condition type", &Query{Version: "1.0", Entity: "Order", Filter: &Condition{Type: "weird"}}, "unknown condition type"},
		{"NOT two children", &Query{Version: "1.0", Entity: "Order", Filter: &Condition{Type: CondLogical, Op: OpNOT, Children: []*Condition{testutil.Comp("refunded", OpEquals, testutil.VBool(true)), testutil.Comp("refunded", OpEquals, testutil.VBool(false))}}}, "exactly one child"},
		{"empty logical", &Query{Version: "1.0", Entity: "Order", Filter: testutil.And()}, "no children"},
		{"nesting too deep", &Query{Version: "1.0", Entity: "Order", Filter: deep}, "nesting depth"},
		{"unknown logical op", &Query{Version: "1.0", Entity: "Order", Filter: &Condition{Type: CondLogical, Op: "XOR", Children: []*Condition{testutil.Comp("refunded", OpEquals, testutil.VBool(true))}}}, "unknown logical operator"},
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
	q.Limit = IntPtr(-1)
	if err := Validate(q, c); err == nil || !strings.Contains(err.Error(), "must not be negative") {
		t.Errorf("expected negative-limit error, got %v", err)
	}

	// limit above ceiling
	q = NewQuery("Order")
	q.Limit = IntPtr(9999)
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
	q.Filter = testutil.And(
		testutil.Comp("bogus1", OpEquals, testutil.VStr("x")),
		testutil.Comp("bogus2", OpEquals, testutil.VStr("y")),
	)
	err := Validate(q, c)
	if err == nil {
		t.Fatal("expected errors")
	}
	if !strings.Contains(err.Error(), "bogus1") || !strings.Contains(err.Error(), "bogus2") {
		t.Errorf("expected both errors, got: %s", err.Error())
	}
}
