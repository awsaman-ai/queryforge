package queryforge

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/awsaman-ai/queryforge/internal/failure"
	"github.com/awsaman-ai/queryforge/internal/testutil"
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

func advCmp(field, op, val string) string {
	return `{"type":"comparison","field":"` + field + `","operator":"` + op + `","value":` + val + `}`
}

func andAST(children ...string) string {
	return `{"version":"1.0","entity":"Order","filter":{"type":"logical","op":"AND","children":[` + strings.Join(children, ",") + `]}}`
}

func advNum(n string) string { return `{"kind":"number","v":` + n + `}` }

// BUG-06: impossible numeric ranges under AND are flagged; possible ones are not.
func TestRangeContradictionWarns(t *testing.T) {
	for name, tc := range map[string]struct {
		ast  string
		warn bool
	}{
		"gt100 and lt10":      {andAST(advCmp("amount", "gt", advNum("100")), advCmp("amount", "lt", advNum("10"))), true},
		"gte5 and lt5":        {andAST(advCmp("amount", "gte", advNum("5")), advCmp("amount", "lt", advNum("5"))), true},
		"gt5 and lte5":        {andAST(advCmp("amount", "gt", advNum("5")), advCmp("amount", "lte", advNum("5"))), true},
		"between vs gt":       {andAST(advCmp("amount", "between", `{"kind":"array","v":[1,5]}`), advCmp("amount", "gt", advNum("50"))), true},
		"gte5 and lte5 is ok": {andAST(advCmp("amount", "gte", advNum("5")), advCmp("amount", "lte", advNum("5"))), false},
		"valid range":         {andAST(advCmp("amount", "gt", advNum("10")), advCmp("amount", "lt", advNum("100"))), false},
		"or is exempt":        {strings.Replace(andAST(advCmp("amount", "gt", advNum("100")), advCmp("amount", "lt", advNum("10"))), `"AND"`, `"OR"`, 1), false},
	} {
		res, err := advEngine(t, tc.ast).Translate(context.Background(), "orders", "sql", nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := hasWarn(res, "never match"); got != tc.warn {
			t.Errorf("%s: warned=%v want %v (%v)", name, got, tc.warn, res.Warnings)
		}
	}
}

// BUG-04: a relative-date window pointing against the request's direction warns.
func TestDateDirectionWarns(t *testing.T) {
	period := func(from, to string) string {
		return andAST(advCmp("createdAt", "between", `{"kind":"period","unit":"week","from":`+from+`,"to":`+to+`}`))
	}
	for name, tc := range map[string]struct {
		text, ast string
		warn      bool
	}{
		"next week resolved to past":   {"orders expiring next week", period("-1", "-1"), true},
		"next 7 days resolved to past": {"orders due in the next 7 days", period("-6", "0"), true},
		"next week correct":            {"orders next week", period("1", "1"), false},
		"last week resolved to future": {"orders from last week", period("1", "1"), true},
		"last week correct":            {"orders from last week", period("-1", "-1"), false},
		"this week no cue":             {"orders this week", period("0", "0"), false},
		"both cues stay silent":        {"orders last week vs next week", period("-1", "-1"), false},
	} {
		res, err := advEngine(t, tc.ast).Translate(context.Background(), tc.text, "sql", nil)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if got := hasWarn(res, "inverted"); got != tc.warn {
			t.Errorf("%s: warned=%v want %v (%v)", name, got, tc.warn, res.Warnings)
		}
	}
}

// BUG-07: widened built-in verbs, noun uses stay quiet, and policy.mutationVerbs extends the list.
func TestMutationVerbDomainCoverage(t *testing.T) {
	run := func(e *Engine, q string) bool {
		res, err := e.Translate(context.Background(), q, "sql", nil)
		if err != nil {
			t.Fatalf("%q: %v", q, err)
		}
		return hasWarn(res, "only reads")
	}
	e := advEngine(t, okAST)
	for _, q := range []string{"ship the pending orders", "dispatch the pending orders", "restock low items", "waive fees on orders", "quarantine flagged orders", "bill the placed orders", "charge all placed orders"} {
		if !run(e, q) {
			t.Errorf("%q: expected a mutation warning", q)
		}
	}
	for _, q := range []string{"ship date of orders", "bill totals over 100", "charge amount above 5", "recall of orders in March"} {
		if run(e, q) {
			t.Errorf("%q: unexpected mutation warning", q)
		}
	}
	// A config-supplied verb outside the built-in list.
	cfgJSON := strings.Replace(scopeConfigJSON, `"defaults"`, `"policy":{"mutationVerbs":["Zap"]},"defaults"`, 1)
	custom := NewWithProvider(testutil.MustParse(t, cfgJSON), &StubProvider{Response: okAST})
	if !run(custom, "zap the cancelled orders") {
		t.Error("policy.mutationVerbs entry should trigger the advisory")
	}
	if run(e, "zap the cancelled orders") {
		t.Error("without the config entry, zap is not a known verb")
	}
	if _, err := ParseConfig([]byte(strings.Replace(scopeConfigJSON, `"defaults"`, `"policy":{"mutationVerbs":["two words"]},"defaults"`, 1))); err == nil {
		t.Error("multi-word mutationVerbs entry must be rejected")
	}
}

// BUG-02: a "different element" request whose AST folds same-array predicates warns,
// and the distinct marker both silences it and survives the engine.
func TestDistinctElementAdvisory(t *testing.T) {
	cfg := `{"entity":"Order","model":{},"backends":{"mongo":{"collection":"o"}},"fields":[
	 {"name":"itemSku","type":"string","operators":["equals"],"mapping":{"mongo":"items.sku"},"elemMatch":"items"},
	 {"name":"itemPrice","type":"number","operators":["gt"],"mapping":{"mongo":"items.price"},"elemMatch":"items"}]}`
	mk := func(distinct string) string {
		return `{"version":"1.0","entity":"Order","filter":{"type":"logical","op":"AND"` + distinct + `,"children":[` +
			advCmp("itemSku", "equals", `{"kind":"string","v":"A"}`) + `,` + advCmp("itemPrice", "gt", advNum("5")) + `]}}`
	}
	run := func(ast, text string) *TranslateResult {
		e := NewWithProvider(testutil.MustParse(t, cfg), &StubProvider{Response: ast})
		res, err := e.Translate(context.Background(), text, "mongo", nil)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := run(mk(""), "orders with an item sku A and a different item over 5"); !hasWarn(r, "DIFFERENT element") {
		t.Errorf("folded AST + 'different' text should warn: %v", r.Warnings)
	}
	if r := run(mk(`,"distinct":true`), "orders with an item sku A and a different item over 5"); hasWarn(r, "DIFFERENT element") {
		t.Errorf("distinct AST must not warn: %v", r.Warnings)
	}
	if r := run(mk(""), "orders where the item sku A costs over 5"); hasWarn(r, "DIFFERENT element") {
		t.Errorf("no cue, no warning: %v", r.Warnings)
	}
	// distinct on OR is rejected by the validator.
	bad := strings.Replace(mk(`,"distinct":true`), `"AND"`, `"OR"`, 1)
	e := NewWithProvider(testutil.MustParse(t, cfg), &StubProvider{Response: bad})
	if _, err := e.Translate(context.Background(), "x", "mongo", nil); err == nil {
		t.Error("distinct on OR must be rejected")
	}
}

// Live finding (Nova Pro, 2026-11-20): a distinct AND on different array elements
// must not draw the "can never match" warning for same-field values; the same
// predicates in a NON-distinct AND (one element) still must.
func TestDistinctElementsDoNotWarnContradiction(t *testing.T) {
	cfg := `{"entity":"Order","model":{},"backends":{"mongo":{"collection":"o"}},"fields":[
	 {"name":"role","type":"string","operators":["equals"],"mapping":{"mongo":"comments.role"},"elemMatch":"comments"}]}`
	mk := func(distinct string) string {
		return `{"version":"1.0","entity":"Order","filter":{"type":"logical","op":"AND"` + distinct + `,"children":[` +
			advCmp("role", "equals", `{"kind":"string","v":"SUPERVISOR"}`) + `,` + advCmp("role", "equals", `{"kind":"string","v":"CUSTOMER"}`) + `]}}`
	}
	run := func(ast string) *TranslateResult {
		e := NewWithProvider(testutil.MustParse(t, cfg), &StubProvider{Response: ast})
		res, err := e.Translate(context.Background(), "x", "mongo", nil)
		if err != nil {
			t.Fatal(err)
		}
		return res
	}
	if r := run(mk(`,"distinct":true`)); hasWarn(r, "never match") {
		t.Errorf("distinct AND across elements is satisfiable, got: %v", r.Warnings)
	}
	if r := run(mk("")); !hasWarn(r, "never match") {
		t.Errorf("same-element AND of two roles is impossible and must still warn: %v", r.Warnings)
	}
}
