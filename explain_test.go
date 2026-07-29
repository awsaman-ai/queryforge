package queryforge

import (
	"strings"
	"testing"
)

// TestExplainCanonical renders the design-doc example and checks the prose reads
// correctly end to end.
func TestExplainCanonical(t *testing.T) {
	got := Explain(canonicalQuery(), nil)
	want := `Return all fields from Order where (status equals "DELIVERED" AND refunded equals false AND createdAt is on or after 30 days ago AND tags contains all of [premium, express]), sorted by createdAt (descending), limited to 50 result(s).`
	if got != want {
		t.Errorf("explain mismatch:\n got: %s\nwant: %s", got, want)
	}
}

// TestExplainProjectionAndOps covers projection, between, in, null, and OR.
func TestExplainProjectionAndOps(t *testing.T) {
	q := NewQuery("Order")
	q.Select = []string{"status", "amount"}
	q.Filter = &Condition{Type: CondLogical, Op: OpOR, Children: []*Condition{
		comp("amount", OpBetween, vArr(float64(10), float64(100))),
		comp("status", OpIn, vArr("PLACED", "DELIVERED")),
	}}
	got := Explain(q, nil)

	for _, want := range []string{
		"Return status, amount from Order",
		"amount is between 10 and 100",
		"status is one of [PLACED, DELIVERED]",
		" OR ",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in: %s", want, got)
		}
	}
}

// TestExplainNullAndNot covers a null operator (no value) and NOT.
func TestExplainNullAndNot(t *testing.T) {
	q := NewQuery("Order")
	q.Filter = &Condition{Type: CondLogical, Op: OpNOT, Children: []*Condition{
		comp("status", OpIsNull, nil),
	}}
	got := Explain(q, nil)
	if !strings.Contains(got, "NOT status is empty") {
		t.Errorf("unexpected NOT/null prose: %s", got)
	}
}

// TestExplainRelativeDates checks singular/plural and past/future phrasing.
func TestExplainRelativeDates(t *testing.T) {
	cases := map[string]struct {
		unit   string
		amount int
		want   string
	}{
		"past days":    {"day", -30, "30 days ago"},
		"one day":      {"day", -1, "1 day ago"},
		"future weeks": {"week", 2, "2 weeks from now"},
	}
	for name, tc := range cases {
		q := single(comp("createdAt", OpAfter, vRel(tc.unit, tc.amount)))
		if got := Explain(q, nil); !strings.Contains(got, tc.want) {
			t.Errorf("%s: expected %q in %s", name, tc.want, got)
		}
	}
}

// TestExplainNilQuery is the worst case.
func TestExplainNilQuery(t *testing.T) {
	if got := Explain(nil, nil); got != "(empty query)" {
		t.Errorf("nil query prose = %q", got)
	}
}
