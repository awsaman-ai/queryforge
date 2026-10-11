package pipeline

import (
	"encoding/json"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/plan"
	"queryforge.local/v2experiment/internal/qferr"
)

// Decision 022 §2: a number reaches the parameters exactly as the sentence
// wrote it — extraction, binding, the SemanticPlan and the parameter all
// carry the same digits. Before the gate the value was a float64 at every
// step, and "more than 99999999999999999999999" was sent as 1e23 (V02).

func spentMoreThan(t *testing.T, literal string) (Result, error) {
	t.Helper()
	return run(t, "customers who spent more than "+literal, map[string][]hit{
		"customers": hCustomers, "spent": hSpent, "more than": hMoreThan})
}

func TestLiteralIsExactAtEveryStage(t *testing.T) {
	cases := []struct {
		literal string
		exact   string // the value at every stage
		driver  any    // the parameter's driver shape
	}{
		{"99999999999999999999999", "99999999999999999999999", "99999999999999999999999"},
		{"9007199254740993", "9007199254740993", int64(9007199254740993)},
		{"9223372036854775808", "9223372036854775808", "9223372036854775808"},
		{"0.1234567890123456789", "0.1234567890123456789", "0.1234567890123456789"},
		{"12345678901234567890.25", "12345678901234567890.25", "12345678901234567890.25"},
		{"4.35k", "4350", int64(4350)},
		{"-5", "-5", int64(-5)},
		{".5", "0.5", "0.5"},
		{"50,000", "50000", int64(50000)},
	}
	for _, c := range cases {
		res, err := spentMoreThan(t, c.literal)
		if err != nil || res.Stage != StageDone {
			t.Errorf("%s: stage %s err %v", c.literal, res.Stage, err)
			continue
		}
		// Extraction.
		if n := res.Analysis.Numbers; len(n) != 1 || n[0].Value.String() != c.exact {
			t.Errorf("%s: extracted %+v", c.literal, n)
		}
		// Binding.
		if bc := res.Binding.Conditions; len(bc) != 1 || bc[0].Value.Number.String() != c.exact {
			t.Errorf("%s: bound %s", c.literal, res.Binding.Explain(true))
		}
		// The SemanticPlan, in memory and as JSON (its digits, not a float's).
		if w := res.Plan.Where; len(w) != 1 || w[0].Values[0].Number.String() != c.exact {
			t.Errorf("%s: planned %s", c.literal, res.Plan.Explain(true))
		}
		b, err := json.Marshal(res.Plan)
		if err != nil || !strings.Contains(string(b), `{"number":`+c.exact+`}`) {
			t.Errorf("%s: plan JSON %s (%v)", c.literal, b, err)
		}
		back, err := plan.Decode(b)
		if err != nil || back.Where[0].Values[0].Number.String() != c.exact {
			t.Errorf("%s: plan JSON did not round-trip: %v", c.literal, err)
		}
		// The parameter.
		p := res.Query.Params
		if len(p) != 1 || p[0].Value.Number.String() != c.exact || p[0].Driver() != c.driver {
			t.Errorf("%s: parameter %#v / %#v, want %#v", c.literal, p, p[0].Driver(), c.driver)
		}
		if !strings.Contains(res.Query.Render(false), "1 = "+c.exact+" (number)") {
			t.Errorf("%s: rendered\n%s", c.literal, res.Query.Render(false))
		}
	}
}

// A number the parser cannot read exactly is refused, never approximated or
// dropped: before the gate "12x" compared with 12, "1,5" with 15, and "1e309"
// vanished (the operator then had "no value").
func TestUnreadableNumbersAreRefused(t *testing.T) {
	for _, lit := range []string{"12x", "1,5", "1.000,5", "1e5", "1e309", "1.2.3", "30th"} {
		res, err := spentMoreThan(t, lit)
		if err == nil || qferr.CodeOf(err) != qferr.CodeValueResolutionFailed || res.Stage != StageBind {
			t.Errorf("%s: stage %s err %v", lit, res.Stage, err)
			continue
		}
		if !strings.Contains(err.Error(), "not written in a form the engine reads exactly") || res.Query.SQL != "" {
			t.Errorf("%s: %v", lit, err)
		}
	}
	// A malformed token next to an otherwise complete request is not dropped.
	res, err := run(t, "customers from India 1.2.3", map[string][]hit{"customers": hCustomers, "india": hIndia})
	if err == nil || qferr.CodeOf(err) != qferr.CodeValueResolutionFailed {
		t.Errorf("a malformed number beside a complete request: stage %s err %v\n%s", res.Stage, err, res.Query.SQL)
	}
}

// Spelled scale words that repeat or rise end the number; the leftover word
// is refused, where the sum used to invent a value.
func TestInventedSpelledValuesAreRefused(t *testing.T) {
	res, err := spentMoreThan(t, "one billion billion")
	if err == nil || res.Query.SQL != "" {
		t.Errorf("one billion billion compiled: %s %v", res.Query.Render(false), res.Query.Params)
	}
	res, err = spentMoreThan(t, "two million three thousand")
	if err != nil || res.Query.Params[0].Driver() != int64(2003000) {
		t.Errorf("ordinary spelled number: %v %v", err, res.Query.Params)
	}
}
