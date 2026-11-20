package queryforge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/awsaman-ai/queryforge/internal/failure"
)

func advEngine(t *testing.T, resp string) *Engine {
	t.Helper()
	return NewWithProvider(scopeConfig(t), &StubProvider{Response: resp})
}

const okAST = `{"version":"1.0","entity":"Order","filter":{"type":"logical","op":"AND","children":[
 {"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"PLACED"}}]}}`

func hasWarn(res *TranslateResult, sub string) bool {
	for _, w := range res.Warnings {
		if strings.Contains(w, sub) {
			return true
		}
	}
	return false
}

// F3: a write-verb request must carry a warning even though it compiles.
func TestMutationVerbWarns(t *testing.T) {
	for _, q := range []string{"close every open order", "Please delete cancelled orders", "can you mark all orders as refunded",
		"show cancelled orders and then remove them"} {
		res, err := advEngine(t, okAST).Translate(context.Background(), q, "sql", nil)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if !hasWarn(res, "only reads") {
			t.Errorf("%q: no mutation warning in %v", q, res.Warnings)
		}
	}
}

// Read questions that merely mention a write word must stay silent.
func TestMutationVerbNoFalsePositive(t *testing.T) {
	for _, q := range []string{"show closed orders", "orders that were deleted last week", "how many orders were cancelled",
		"list orders to close this month", "set of orders placed today"} {
		res, err := advEngine(t, okAST).Translate(context.Background(), q, "sql", nil)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		if hasWarn(res, "only reads") {
			t.Errorf("%q: unexpected mutation warning", q)
		}
	}
}

// P1: two different values for one scalar field under AND can never match.
func TestContradictoryFilterWarns(t *testing.T) {
	bad := `{"version":"1.0","entity":"Order","filter":{"type":"logical","op":"AND","children":[
	 {"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"PLACED"}},
	 {"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}]}}`
	res, err := advEngine(t, bad).Translate(context.Background(), "placed delivered orders", "sql", nil)
	if err != nil {
		t.Fatal(err)
	}
	if !hasWarn(res, "never match") {
		t.Errorf("no contradiction warning: %v", res.Warnings)
	}
	// Same value twice, OR, and array fields must not warn.
	for name, ast := range map[string]string{
		"same value": strings.Replace(bad, `"v":"DELIVERED"`, `"v":"PLACED"`, 1),
		"or":         strings.Replace(bad, `"op":"AND"`, `"op":"OR"`, 1),
	} {
		res, err := advEngine(t, ast).Translate(context.Background(), "orders", "sql", nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if hasWarn(res, "never match") {
			t.Errorf("%s: false positive: %v", name, res.Warnings)
		}
	}
}

// P4: an empty question fails before any model call, with the documented code.
func TestEmptyQuestionIsInvalidRequest(t *testing.T) {
	stub := &StubProvider{Response: okAST}
	eng := NewWithProvider(scopeConfig(t), stub)
	for _, q := range []string{"", "   \n\t"} {
		_, err := eng.Translate(context.Background(), q, "sql", nil)
		if err == nil || failure.Classify(err) != failure.FailureInvalidRequest {
			t.Errorf("%q: err = %v, code = %v", q, err, failure.Classify(err))
		}
	}
	if stub.Calls != 0 {
		t.Errorf("model was called %d time(s) for an empty question", stub.Calls)
	}
}

// P3: exhausting the repair budget keeps the per-attempt record.
func TestExhaustedRepairsKeepHistory(t *testing.T) {
	bad := `{"version":"1.0","entity":"Order","filter":{"type":"comparison","field":"nope","operator":"equals","value":{"kind":"string","v":"x"}}}`
	eng := advEngine(t, bad)
	_, err := eng.Translate(context.Background(), "orders", "sql", nil)
	var te *TranslateError
	if !errors.As(err, &te) {
		t.Fatalf("want *TranslateError, got %T: %v", err, err)
	}
	if len(te.Repairs) != eng.MaxRepairs+1 {
		t.Errorf("repairs = %d, want %d", len(te.Repairs), eng.MaxRepairs+1)
	}
	if failure.Classify(err) != failure.FailureValidation {
		t.Errorf("classification changed: %v", failure.Classify(err))
	}
}

// The public entry point enforces the F1 rule too, not just the internal parser.
func TestParseConfigRejectsMongoOnlyFieldOnSQL(t *testing.T) {
	_, err := ParseConfig([]byte(`{"entity":"Ticket","model":{},
	  "backends":{"sql":{"table":"tickets"},"mongo":{"collection":"tickets"}},
	  "fields":[{"name":"commentDate","type":"date","mapping":{"mongo":"comments.date"},"elemMatch":"comments"}]}`))
	if err == nil || !strings.Contains(err.Error(), "no \"sql\" mapping") {
		t.Fatalf("want the F1 load error, got %v", err)
	}
}

// Warnings must survive alongside a scope and must not change the compiled query.
func TestAdvisoriesDoNotAlterQueryAndSurviveScope(t *testing.T) {
	eng := advEngine(t, okAST)
	plain, err := eng.Translate(context.Background(), "show placed orders", "sql", Scope{"tenantId": "T-1"})
	if err != nil {
		t.Fatal(err)
	}
	warned, err := eng.Translate(context.Background(), "close placed orders", "sql", Scope{"tenantId": "T-1"})
	if err != nil {
		t.Fatal(err)
	}
	if plain.Query.SQL != warned.Query.SQL {
		t.Errorf("a warning changed the SQL:\n%s\n%s", plain.Query.SQL, warned.Query.SQL)
	}
	if !strings.Contains(warned.Query.SQL, "tenant_id") {
		t.Errorf("scope predicate lost: %s", warned.Query.SQL)
	}
	if !hasWarn(warned, "only reads") || hasWarn(plain, "only reads") {
		t.Errorf("warnings wrong: plain=%v warned=%v", plain.Warnings, warned.Warnings)
	}
}

// A translation that succeeds after a repair must still report its history and
// must NOT be a TranslateError.
func TestRepairedSuccessKeepsHistory(t *testing.T) {
	bad := `{"version":"1.0","entity":"Order","filter":{"type":"comparison","field":"nope","operator":"equals","value":{"kind":"string","v":"x"}}}`
	stub := &scriptedProvider{responses: []string{bad, okAST}}
	res, err := NewWithProvider(scopeConfig(t), stub).Translate(context.Background(), "placed orders", "sql", nil)
	if err != nil {
		t.Fatal(err)
	}
	if res.RepairAttempts != 1 || len(res.Repairs) != 1 {
		t.Errorf("attempts=%d repairs=%d, want 1/1", res.RepairAttempts, len(res.Repairs))
	}
}
