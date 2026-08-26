package queryforge

import (
	"strings"
	"testing"

	"github.com/awsaman-ai/queryforge/internal/testutil"
)

// --- Tier 1 Explain readback tests.
//
// Business rule under test: res.Explain is what a user is shown BEFORE a
// query runs, so a field's displayName should make that readback more
// natural ("Passport Country contains..." instead of "txt01 contains...").
// The fallback behaviour matters just as much: nothing should break, panic,
// or silently drop a clause when a field carries no label, isn't registered
// in the config at all (a scope key), or when Explain is called with no
// config (existing callers do this today, per TestExplainCanonical).

// TestExplainUsesDisplayNameWhenSet is the positive case.
func TestExplainUsesDisplayNameWhenSet(t *testing.T) {
	c := testutil.MustParse(t, `{
	  "entity":"Order",
	  "fields":[{"name":"txt01","type":"string","customField":true,
	    "displayName":"Passport Country","description":"ISO country of issuance",
	    "valueHint":"agent notes"}]
	}`)
	q := NewQuery("Order")
	q.Filter = testutil.Comp("txt01", OpContains, testutil.VStr("France"))

	got := Explain(q, c)
	if !strings.Contains(got, "Passport Country contains") {
		t.Errorf("expected the display name in the readback, got: %s", got)
	}
	if strings.Contains(got, "txt01") {
		t.Errorf("raw field name should not appear once a displayName is set: %s", got)
	}
}

// TestExplainFallsBackToNameWithoutDisplayName: a field with no displayName
// (the overwhelming majority of fields) reads back exactly as it did before
// this feature existed.
func TestExplainFallsBackToNameWithoutDisplayName(t *testing.T) {
	c := testutil.MustParse(t, `{
	  "entity":"Order",
	  "fields":[{"name":"amount","type":"number"}]
	}`)
	q := NewQuery("Order")
	q.Filter = testutil.Comp("amount", OpGt, testutil.VNum(100))

	got := Explain(q, c)
	if !strings.Contains(got, "amount is greater than 100") {
		t.Errorf("expected the raw field name when no displayName is set, got: %s", got)
	}
}

// TestExplainWithNilConfigStillWorks: Explain(q, nil) is an existing, tested
// call shape (TestExplainCanonical) — a caller with no config handy must not
// be broken by this feature's field-lookup addition.
func TestExplainWithNilConfigStillWorks(t *testing.T) {
	q := NewQuery("Order")
	q.Filter = testutil.Comp("amount", OpGt, testutil.VNum(100))
	got := Explain(q, nil)
	if !strings.Contains(got, "amount is greater than 100") {
		t.Errorf("nil config should fall back to the raw field name, got: %s", got)
	}
}

// TestExplainUnregisteredFieldFallsBackToName: a scope-injected predicate can
// reference a key the config never declares (scope.go documents this
// explicitly). Explain must not panic or drop the clause — it should simply
// have no label to show and fall back to the raw name.
func TestExplainUnregisteredFieldFallsBackToName(t *testing.T) {
	c := testutil.MustParse(t, `{
	  "entity":"Order",
	  "fields":[{"name":"amount","type":"number"}]
	}`)
	q := NewQuery("Order")
	q.Filter = testutil.And(
		testutil.Comp("amount", OpGt, testutil.VNum(100)),
		testutil.Comp("subscriptionId", OpEquals, testutil.VStr("SUB-42")), // not in fields[]
	)
	got := Explain(q, c)
	if !strings.Contains(got, `subscriptionId equals "SUB-42"`) {
		t.Errorf("unregistered field should fall back to its raw name, got: %s", got)
	}
}
