package ast_test

import (
	"encoding/json"
	"strings"
	"testing"

	. "github.com/awsaman-ai/queryforge/internal/ast"
)

// TestPeriodValueRoundTrip checks the wire shape: kind, unit, from and to, with
// a zero bound ("to":0, i.e. "through today") surviving the trip — omitempty
// would drop it and the decoder would then refuse the value as incomplete.
func TestPeriodValueRoundTrip(t *testing.T) {
	in := `{"kind":"period","unit":"day","from":-29,"to":0}`
	var v Value
	if err := json.Unmarshal([]byte(in), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.Kind != KindPeriod || v.Unit != "day" || v.From != -29 || v.To != 0 {
		t.Fatalf("decoded %+v", v)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("encode: %v", err)
	}
	if string(out) != in {
		t.Errorf("round trip = %s, want %s", out, in)
	}
}

// TestPeriodValueMissingBoundIsRefused is the adversarial half: a model writing
// only one bound must not be read as the other being 0. {"from":-1} meaning
// "yesterday" would otherwise decode to -1..0, yesterday AND today, and pass
// every later check.
func TestPeriodValueMissingBoundIsRefused(t *testing.T) {
	for name, in := range map[string]string{
		"no to":        `{"kind":"period","unit":"day","from":-1}`,
		"no from":      `{"kind":"period","unit":"day","to":-1}`,
		"neither":      `{"kind":"period","unit":"month"}`,
		"null to":      `{"kind":"period","unit":"day","from":-1,"to":null}`,
		"amount given": `{"kind":"period","unit":"day","amount":-1}`, // the relative_date spelling
	} {
		t.Run(name, func(t *testing.T) {
			var v Value
			err := json.Unmarshal([]byte(in), &v)
			if err == nil {
				t.Fatalf("decoded %+v; a period missing a bound must be refused", v)
			}
			if !strings.Contains(err.Error(), `"from" and "to"`) {
				t.Errorf("error should name the missing keys, got: %v", err)
			}
		})
	}
}

// TestValueDecodeResetsPeriodBounds guards against a reused Value (a decoder
// writing into an existing struct) keeping bounds from an earlier period when
// it now decodes a different kind.
func TestValueDecodeResetsPeriodBounds(t *testing.T) {
	v := Value{Kind: KindPeriod, Unit: "day", From: -5, To: -2}
	if err := json.Unmarshal([]byte(`{"kind":"relative_date","unit":"day","amount":-3}`), &v); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v.From != 0 || v.To != 0 {
		t.Errorf("stale period bounds survived a re-decode: %+v", v)
	}
}

// TestPeriodInsideQuery checks a period decodes in its real position, as a
// between's value inside a filter tree.
func TestPeriodInsideQuery(t *testing.T) {
	var q Query
	in := `{"version":"1.0","entity":"Order","filter":{"type":"comparison","field":"createdAt","operator":"between","value":{"kind":"period","unit":"month","from":-1,"to":-1}}}`
	if err := json.Unmarshal([]byte(in), &q); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if v := q.Filter.Value; v == nil || v.Kind != KindPeriod || v.Unit != "month" || v.From != -1 || v.To != -1 {
		t.Errorf("filter value = %+v", q.Filter.Value)
	}
}
