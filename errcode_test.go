package queryforge

// Tests for the diagnostic surface added alongside ErrCode: every validation
// failure carries a stable machine-readable code, and a translation that needed
// repairs reports why on TranslateResult.Repairs.
//
// The point of the codes is that a caller can branch on them without matching
// English, so the tests assert on Code and never on Message wording — the same
// discipline the feature is asking of its users.

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
)

// codeTestConfig is the fixture for the table below: one field of each shape
// that can fail a different way.
func codeTestConfig(t *testing.T) *Config {
	t.Helper()
	c, err := ParseConfig([]byte(`{
	  "entity": "Order",
	  "backends": { "sql": { "table": "orders" } },
	  "fields": [
	    { "name": "orderId",  "type": "string",
	      "operators": ["equals","notEquals","contains","startsWith","endsWith",
	                    "in","notIn","isNull","isNotNull","regex"] },
	    { "name": "secret",   "type": "string", "queryable": false },
	    { "name": "notes",    "type": "string", "filterable": false },
	    { "name": "code",     "type": "string", "searchable": false },
	    { "name": "rank",     "type": "number", "validators": { "min": 1, "max": 10 } },
	    { "name": "status",   "type": "enum",   "values": ["OPEN","SHUT"] },
	    { "name": "created",  "type": "date",   "sortable": false },
	    { "name": "tags",     "type": "array",  "itemType": "string" }
	  ],
	  "defaults": { "maxLimit": 100 },
	  "policy":   { "maxNestingDepth": 3, "denyRegexOn": ["orderId"] }
	}`))
	if err != nil {
		t.Fatalf("fixture config did not load: %v", err)
	}
	return c
}

// cmp builds a comparison condition inline, to keep the table readable.
func cmp(field string, op Operator, v *Value) *Condition {
	return &Condition{Type: CondComparison, Field: field, Operator: op, Value: v}
}

// strVal / numVal / arrVal are Value constructors for the table.
func strVal(s string) *Value  { return &Value{Kind: KindString, V: s} }
func numVal(f float64) *Value { return &Value{Kind: KindNumber, V: f} }
func arrVal(v ...any) *Value  { return &Value{Kind: KindArray, V: v} }

// TestValidationErrorsCarryCodes walks one AST per failure mode and asserts the
// code. If a new validator is added without a code it will surface here as an
// empty string rather than silently shipping uncoded.
func TestValidationErrorsCarryCodes(t *testing.T) {
	c := codeTestConfig(t)
	limit := func(n int) *int { return &n }

	cases := []struct {
		name string
		q    *Query
		want ErrCode
	}{
		{"unknown field", &Query{Entity: "Order",
			Filter: cmp("nope", OpEquals, strVal("x"))}, CodeUnknownField},

		{"field not queryable", &Query{Entity: "Order",
			Filter: cmp("secret", OpEquals, strVal("x"))}, CodeFieldNotQueryable},

		{"field not filterable", &Query{Entity: "Order",
			Filter: cmp("notes", OpEquals, strVal("x"))}, CodeFieldNotFilterable},

		{"field not searchable", &Query{Entity: "Order",
			Filter: cmp("code", OpContains, strVal("x"))}, CodeFieldNotSearchable},

		{"field not sortable", &Query{Entity: "Order",
			Sort: []SortSpec{{Field: "created", Dir: "ASC"}}}, CodeFieldNotSortable},

		{"unknown operator", &Query{Entity: "Order",
			Filter: cmp("orderId", Operator("sideways"), strVal("x"))}, CodeUnknownOperator},

		{"operator not allowed", &Query{Entity: "Order",
			Filter: cmp("rank", OpStartsWith, strVal("x"))}, CodeOperatorNotAllowed},

		{"regex denied by policy", &Query{Entity: "Order",
			Filter: cmp("orderId", OpRegex, strVal("^A"))}, CodeRegexDenied},

		// A kind mismatch is now about the literal, not the tag. An enum-tagged
		// string on a string field is NOT one — the tag is repaired and the
		// query runs (see kindnorm.go). A number where the config declares text
		// still is one: nothing can make 7 into a name.
		{"kind mismatch", &Query{Entity: "Order",
			Filter: cmp("orderId", OpEquals, &Value{Kind: KindString, V: float64(7)})}, CodeKindMismatch},

		{"value out of domain", &Query{Entity: "Order",
			Filter: cmp("status", OpEquals, strVal("MAYBE"))}, CodeValueOutOfDomain},

		{"value out of bounds", &Query{Entity: "Order",
			Filter: cmp("rank", OpEquals, numVal(99))}, CodeValueOutOfBounds},

		{"value required", &Query{Entity: "Order",
			Filter: cmp("orderId", OpEquals, nil)}, CodeValueRequired},

		{"value not allowed", &Query{Entity: "Order",
			Filter: cmp("orderId", OpIsNull, strVal("x"))}, CodeValueNotAllowed},

		{"invalid arity (between)", &Query{Entity: "Order",
			Filter: cmp("rank", OpBetween, arrVal(1.0))}, CodeInvalidArity},

		{"entity mismatch", &Query{Entity: "Invoice"}, CodeEntityMismatch},

		{"invalid sort direction", &Query{Entity: "Order",
			Sort: []SortSpec{{Field: "orderId", Dir: "sideways"}}}, CodeInvalidSortDir},

		{"invalid paging", &Query{Entity: "Order",
			Limit: limit(-1)}, CodeInvalidPaging},

		{"limit too large", &Query{Entity: "Order",
			Limit: limit(9999)}, CodeLimitTooLarge},

		{"malformed ast", &Query{Entity: "Order",
			Filter: &Condition{Type: CondType("weird")}}, CodeMalformedAST},

		{"unknown logical operator", &Query{Entity: "Order",
			Filter: &Condition{Type: CondLogical, Op: LogicalOp("NAND"),
				Children: []*Condition{cmp("orderId", OpEquals, strVal("x"))}}}, CodeUnknownOperator},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := Validate(tc.q, c)
			if err == nil {
				t.Fatalf("expected a validation error, got nil")
			}
			ves, ok := err.(ValidationErrors)
			if !ok {
				t.Fatalf("expected ValidationErrors, got %T", err)
			}
			for _, ve := range ves {
				if ve.Code == tc.want {
					return // found it
				}
			}
			var got []string
			for _, ve := range ves {
				got = append(got, string(ve.Code)+" ("+ve.Message+")")
			}
			t.Fatalf("no error carried code %q; got: %s", tc.want, strings.Join(got, "; "))
		})
	}
}

// TestEveryValidationErrorHasACode is the guard against an uncoded validator
// slipping in later: whatever the failure, Code must never be empty.
func TestEveryValidationErrorHasACode(t *testing.T) {
	c := codeTestConfig(t)

	// One deliberately awful AST that trips many validators at once.
	bad := &Query{
		Entity: "Wrong",
		Select: []string{"nope", "secret"},
		Sort:   []SortSpec{{Field: "created", Dir: "wat"}, {Field: "ghost"}},
		Filter: &Condition{Type: CondLogical, Op: OpNOT, Children: []*Condition{
			cmp("status", OpEquals, strVal("NOPE")),
			cmp("rank", OpEquals, numVal(-5)),
		}},
	}
	err := Validate(bad, c)
	ves, ok := err.(ValidationErrors)
	if !ok {
		t.Fatalf("expected ValidationErrors, got %T (%v)", err, err)
	}
	if len(ves) < 5 {
		t.Fatalf("expected several errors from this AST, got %d", len(ves))
	}
	for i, ve := range ves {
		if ve.Code == "" {
			t.Errorf("errors[%d] has no Code: path=%q message=%q", i, ve.Path, ve.Message)
		}
	}
}

// TestValidationErrorMarshalsForTheWire pins the json shape, since the whole
// point is that a service can hand these to a caller without a parallel struct.
func TestValidationErrorMarshalsForTheWire(t *testing.T) {
	ve := &ValidationError{
		Code: CodeUnknownField, Path: "filter", Field: "carier",
		Message: `unknown field "carier"`, Suggestions: []string{"Carrier"},
	}
	b, err := json.Marshal(ve)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	for _, key := range []string{"code", "path", "field", "message", "suggestions"} {
		if _, ok := got[key]; !ok {
			t.Errorf("marshalled form is missing %q: %s", key, b)
		}
	}
	if got["code"] != "unknown_field" {
		t.Errorf("code = %v, want unknown_field", got["code"])
	}
}

// TestRepairKindMarshalsAsAWord guards the readability of the wire form: an
// integer kind would be useless in an API response.
func TestRepairKindMarshalsAsAWord(t *testing.T) {
	for kind, want := range map[RepairKind]string{
		RepairValidation: `"validation"`,
		RepairParse:      `"parse"`,
		RepairNone:       `"none"`,
	} {
		b, err := json.Marshal(kind)
		if err != nil {
			t.Fatalf("marshal %v: %v", kind, err)
		}
		if string(b) != want {
			t.Errorf("RepairKind(%d) marshalled as %s, want %s", kind, b, want)
		}
	}
}

// TestRepairsReportWhyEachAttemptFailed is the end-to-end case this feature
// exists for: the first reply is valid JSON that breaks a config rule, the
// second is clean. The translation succeeds, and Repairs explains the cost.
func TestRepairsReportWhyEachAttemptFailed(t *testing.T) {
	c := codeTestConfig(t)

	// Attempt 0 sends an out-of-domain enum value; attempt 1 corrects it.
	bad := `{"version":"1.0","entity":"Order","filter":{"type":"comparison",
	          "field":"status","operator":"equals","value":{"kind":"string","v":"MAYBE"}}}`
	good := `{"version":"1.0","entity":"Order","filter":{"type":"comparison",
	          "field":"status","operator":"equals","value":{"kind":"string","v":"OPEN"}}}`

	e := NewWithProvider(c, &scriptedProvider{responses: []string{bad, good}})
	res, err := e.Translate(context.Background(), "open orders", "sql", nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}

	if res.RepairAttempts != 1 {
		t.Fatalf("RepairAttempts = %d, want 1", res.RepairAttempts)
	}
	if len(res.Repairs) != res.RepairAttempts {
		t.Fatalf("len(Repairs) = %d, want %d (one record per repair)",
			len(res.Repairs), res.RepairAttempts)
	}

	r := res.Repairs[0]
	if r.Attempt != 0 {
		t.Errorf("Attempt = %d, want 0", r.Attempt)
	}
	if r.Kind != RepairValidation {
		t.Errorf("Kind = %v, want validation", r.Kind)
	}
	if r.Message == "" {
		t.Error("Message is empty; it should carry what was fed back to the model")
	}
	if len(r.Errors) == 0 {
		t.Fatal("Errors is empty; a validation repair must carry structured detail")
	}
	if got := r.Errors[0].Code; got != CodeValueOutOfDomain {
		t.Errorf("Errors[0].Code = %q, want %q", got, CodeValueOutOfDomain)
	}
	if got := r.Errors[0].Field; got != "status" {
		t.Errorf("Errors[0].Field = %q, want status", got)
	}
}

// TestNoRepairsOnFirstTrySuccess pins the quiet case: a clean translation must
// not manufacture an empty record, or "did this cost a retry?" stops being
// answerable by a len() check.
func TestNoRepairsOnFirstTrySuccess(t *testing.T) {
	c := codeTestConfig(t)
	good := `{"version":"1.0","entity":"Order","filter":{"type":"comparison",
	          "field":"status","operator":"equals","value":{"kind":"string","v":"OPEN"}}}`

	e := NewWithProvider(c, &scriptedProvider{responses: []string{good}})
	res, err := e.Translate(context.Background(), "open orders", "sql", nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if res.RepairAttempts != 0 {
		t.Fatalf("RepairAttempts = %d, want 0", res.RepairAttempts)
	}
	if len(res.Repairs) != 0 {
		t.Fatalf("Repairs = %+v, want empty on a first-try success", res.Repairs)
	}
}

// TestRepairsRecordParseFailures covers the other Kind: an unusable reply is
// recorded with a message but no structured errors, because nothing parsed.
func TestRepairsRecordParseFailures(t *testing.T) {
	c := codeTestConfig(t)
	good := `{"version":"1.0","entity":"Order","filter":{"type":"comparison",
	          "field":"status","operator":"equals","value":{"kind":"string","v":"OPEN"}}}`

	e := NewWithProvider(c, &scriptedProvider{responses: []string{"not json at all", good}})
	res, err := e.Translate(context.Background(), "open orders", "sql", nil)
	if err != nil {
		t.Fatalf("translate: %v", err)
	}
	if len(res.Repairs) != 1 {
		t.Fatalf("len(Repairs) = %d, want 1", len(res.Repairs))
	}
	if res.Repairs[0].Kind != RepairParse {
		t.Errorf("Kind = %v, want parse", res.Repairs[0].Kind)
	}
	if len(res.Repairs[0].Errors) != 0 {
		t.Errorf("Errors = %+v, want empty: nothing parsed, so there is no AST to fault",
			res.Repairs[0].Errors)
	}
	if res.Repairs[0].Message == "" {
		t.Error("Message is empty; the parse failure text is the only record of what happened")
	}
}
