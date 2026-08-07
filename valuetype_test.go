package queryforge

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

// A config covering every scalar type plus an array field, so the rule "the
// declared type decides" can be exercised against each type in turn.
const valueTypeConfigJSON = `{"entity":"Order","fields":[
    {"name":"name","type":"string","searchable":true},
    {"name":"status","type":"enum","values":["PLACED","DELIVERED","CANCELLED"]},
    {"name":"amount","type":"number"},
    {"name":"paid","type":"boolean"},
    {"name":"createdAt","type":"date"},
    {"name":"tags","type":"array","itemType":"string"},
    {"name":"secret","type":"string","queryable":false}
]}`

func valueTypeConfig(t *testing.T) *Config {
	t.Helper()
	c, err := ParseConfig([]byte(valueTypeConfigJSON))
	if err != nil {
		t.Fatalf("config: %v", err)
	}
	return c
}

// valueTypeAST decodes a filter into a full Query for the test config's entity.
func valueTypeAST(t *testing.T, filter string) *Query {
	t.Helper()
	var q Query
	if err := json.Unmarshal([]byte(`{"entity":"Order","filter":`+filter+`}`), &q); err != nil {
		t.Fatalf("ast: %v", err)
	}
	return &q
}

// TestKindTagDisagreeingWithConfigIsAccepted is the regression guard for the
// reported failure: "delivered orders" against a config declaring Status as a
// string, where the model tagged the literal "enum". The literal is a string
// and the field takes strings, so the query must run.
func TestKindTagDisagreeingWithConfigIsAccepted(t *testing.T) {
	c := valueTypeConfig(t)

	cases := []struct {
		name   string
		filter string
	}{
		// The reported bug, and its mirror image.
		{"enum tag on a string field", `{"type":"comparison","field":"name","operator":"equals","value":{"kind":"enum","v":"widget"}}`},
		{"string tag on an enum field", `{"type":"comparison","field":"status","operator":"equals","value":{"kind":"string","v":"DELIVERED"}}`},

		// The same slip on every other declared type.
		{"date tag on a string field", `{"type":"comparison","field":"name","operator":"equals","value":{"kind":"date","v":"widget"}}`},
		{"string tag on a date field", `{"type":"comparison","field":"createdAt","operator":"after","value":{"kind":"string","v":"2026-01-31"}}`},
		{"string tag on a number field", `{"type":"comparison","field":"amount","operator":"gt","value":{"kind":"string","v":42}}`},
		{"number tag on a boolean field", `{"type":"comparison","field":"paid","operator":"equals","value":{"kind":"number","v":true}}`},
		{"enum tag on an array element", `{"type":"comparison","field":"tags","operator":"contains","value":{"kind":"enum","v":"sale"}}`},

		// A list payload is a list whatever the tag claims.
		{"enum tag on a list", `{"type":"comparison","field":"status","operator":"in","value":{"kind":"enum","v":["PLACED","DELIVERED"]}}`},
		{"string tag on a date range", `{"type":"comparison","field":"createdAt","operator":"between","value":{"kind":"string","v":["2026-01-01","2026-01-31"]}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := Validate(valueTypeAST(t, tc.filter), c); err != nil {
				t.Fatalf("expected the query to be accepted, got %v", err)
			}
		})
	}
}

// TestMistaggedValueCompilesIdentically is the half that matters more than
// acceptance: a mistagged literal must compile to exactly the query a correctly
// tagged one would. Accepting it and then generating something else would turn
// a loud failure into a quiet wrong answer.
func TestMistaggedValueCompilesIdentically(t *testing.T) {
	c := valueTypeConfig(t)
	now := time.Date(2026, 8, 7, 0, 0, 0, 0, time.UTC)

	pairs := []struct {
		name             string
		slipped, correct string
	}{
		{"enum tag on a string field",
			`{"type":"comparison","field":"name","operator":"equals","value":{"kind":"enum","v":"widget"}}`,
			`{"type":"comparison","field":"name","operator":"equals","value":{"kind":"string","v":"widget"}}`},
		{"string tag on an enum field",
			`{"type":"comparison","field":"status","operator":"equals","value":{"kind":"string","v":"DELIVERED"}}`,
			`{"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}`},
		// The load-bearing one for Mongo: a date tagged as text must still be
		// bound as a real Date, or it is compared against ISODates as a string
		// and matches nothing at all.
		{"string tag on a date field",
			`{"type":"comparison","field":"createdAt","operator":"after","value":{"kind":"string","v":"2026-01-31"}}`,
			`{"type":"comparison","field":"createdAt","operator":"after","value":{"kind":"date","v":"2026-01-31"}}`},
		{"enum tag on a list",
			`{"type":"comparison","field":"status","operator":"in","value":{"kind":"enum","v":["PLACED","DELIVERED"]}}`,
			`{"type":"comparison","field":"status","operator":"in","value":{"kind":"array","v":["PLACED","DELIVERED"]}}`},
	}

	for _, backend := range []string{"sql", "mongo"} {
		gen, ok := DefaultRegistry().Get(backend)
		if !ok {
			t.Fatalf("no generator for %q", backend)
		}
		for _, p := range pairs {
			t.Run(backend+"/"+p.name, func(t *testing.T) {
				slipped := valueTypeAST(t, p.slipped)
				if err := Validate(slipped, c); err != nil {
					t.Fatalf("mistagged query did not validate: %v", err)
				}
				got, err := gen.Generate(slipped, c, GenOptions{Now: now})
				if err != nil {
					t.Fatalf("generate: %v", err)
				}
				want, err := gen.Generate(valueTypeAST(t, p.correct), c, GenOptions{Now: now})
				if err != nil {
					t.Fatalf("generate reference: %v", err)
				}
				if string(mustJSON(t, got)) != string(mustJSON(t, want)) {
					t.Errorf("compiled differently:\n got %s\nwant %s", mustJSON(t, got), mustJSON(t, want))
				}
			})
		}
	}
}

// TestGeneratorsDoNotMutateTheAST: the AST belongs to the caller, who may be
// compiling it on several goroutines at once (TestScopeIsSafeForConcurrentUse
// asserts exactly that). Reading the config instead of rewriting the tags is
// what keeps that true — an earlier version of this fix normalized in place and
// raced.
func TestGeneratorsDoNotMutateTheAST(t *testing.T) {
	c := valueTypeConfig(t)
	raw := `{"type":"comparison","field":"createdAt","operator":"after","value":{"kind":"string","v":"2026-01-31"}}`
	q := valueTypeAST(t, raw)
	before := string(mustJSON(t, q))

	if err := Validate(q, c); err != nil {
		t.Fatalf("validate: %v", err)
	}
	for _, backend := range []string{"sql", "mongo"} {
		gen, _ := DefaultRegistry().Get(backend)
		if _, err := gen.Generate(q, c, GenOptions{Now: time.Now().UTC()}); err != nil {
			t.Fatalf("%s: %v", backend, err)
		}
	}
	if after := string(mustJSON(t, q)); after != before {
		t.Errorf("AST was modified:\nbefore %s\n after %s", before, after)
	}
}

// TestPayloadDecidesTypeCompatibility is the safety half. Reading the payload
// instead of the tag has to cut both ways: a tag that lies about a literal the
// field cannot hold must now be caught, where before the tag alone waved it
// through and the literal was dropped at generation time.
func TestPayloadDecidesTypeCompatibility(t *testing.T) {
	c := valueTypeConfig(t)

	cases := []struct {
		name   string
		filter string
	}{
		// Each of these USED TO VALIDATE on the strength of its tag, then bind
		// a zero value: "42" became 0, "true" became false, 42 became "".
		{"numeric text on a number field", `{"type":"comparison","field":"amount","operator":"equals","value":{"kind":"number","v":"42"}}`},
		{"boolean text on a boolean field", `{"type":"comparison","field":"paid","operator":"equals","value":{"kind":"boolean","v":"true"}}`},
		{"number on a string field", `{"type":"comparison","field":"name","operator":"equals","value":{"kind":"string","v":42}}`},
		{"number on an enum field", `{"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":7}}`},
		{"prose on a date field", `{"type":"comparison","field":"createdAt","operator":"after","value":{"kind":"date","v":"last tuesday"}}`},
		{"unparseable date in a range", `{"type":"comparison","field":"createdAt","operator":"between","value":{"kind":"array","v":["2026-01-01","whenever"]}}`},
		{"list where a scalar is required", `{"type":"comparison","field":"name","operator":"equals","value":{"kind":"string","v":["a","b"]}}`},
		{"scalar where a list is required", `{"type":"comparison","field":"status","operator":"in","value":{"kind":"array","v":"PLACED"}}`},
		{"null payload", `{"type":"comparison","field":"name","operator":"equals","value":{"kind":"string","v":null}}`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			q := valueTypeAST(t, tc.filter)
			err := Validate(q, c)
			if err == nil {
				t.Fatalf("expected a kind mismatch, got a valid query: %+v", q.Filter.Value)
			}
			var verrs ValidationErrors
			if !asValidationErrors(err, &verrs) || verrs[0].Code != CodeKindMismatch {
				t.Fatalf("expected %q, got %v", CodeKindMismatch, err)
			}
		})
	}
}

// TestNoConversionIsEverPerformed pins the line between repairing a tag and
// rewriting a literal. Accepting {"kind":"number","v":"42"} by parsing the text
// into 42 would be the validator inventing data the model never sent.
func TestNoConversionIsEverPerformed(t *testing.T) {
	c := valueTypeConfig(t)
	q := valueTypeAST(t, `{"type":"comparison","field":"amount","operator":"equals","value":{"kind":"number","v":"42"}}`)
	if err := Validate(q, c); err == nil {
		t.Fatal("numeric text was accepted on a number field")
	}
	if got, _ := q.Filter.Value.AsString(); got != "42" {
		t.Errorf("payload was rewritten: %#v", q.Filter.Value.V)
	}
}

// TestTypeRuleDoesNotWeakenTheEnumDomain: an out-of-domain value must still be
// rejected, whichever tag it arrived with. Tolerating the tag must not tolerate
// the value.
func TestTypeRuleDoesNotWeakenTheEnumDomain(t *testing.T) {
	c := valueTypeConfig(t)
	for _, kind := range []string{"enum", "string", "date"} {
		t.Run(kind, func(t *testing.T) {
			q := valueTypeAST(t, `{"type":"comparison","field":"status","operator":"equals","value":{"kind":"`+kind+`","v":"SHIPPED"}}`)
			err := Validate(q, c)
			if err == nil {
				t.Fatal("expected an out-of-domain error")
			}
			var verrs ValidationErrors
			if !asValidationErrors(err, &verrs) || verrs[0].Code != CodeValueOutOfDomain {
				t.Fatalf("expected %q, got %v", CodeValueOutOfDomain, err)
			}
		})
	}
}

// TestTypeRuleDoesNotBypassCapabilityGates: an excluded or unknown field stays
// rejected. Reading the config for the type must not become a way past the
// checks that read the config for permission.
func TestTypeRuleDoesNotBypassCapabilityGates(t *testing.T) {
	c := valueTypeConfig(t)
	for _, field := range []string{"secret", "nope"} {
		t.Run(field, func(t *testing.T) {
			q := valueTypeAST(t, `{"type":"comparison","field":"`+field+`","operator":"equals","value":{"kind":"enum","v":"x"}}`)
			if err := Validate(q, c); err == nil {
				t.Fatalf("expected %q to be rejected", field)
			}
		})
	}
}

// TestTypeRuleReachesNestedPredicates: the reported query was an AND of two
// predicates with the bad tag on a child, so the rule has to hold at depth.
func TestTypeRuleReachesNestedPredicates(t *testing.T) {
	c := valueTypeConfig(t)
	q := valueTypeAST(t, `{"type":"logical","op":"AND","children":[
	  {"type":"logical","op":"NOT","children":[
	    {"type":"comparison","field":"name","operator":"equals","value":{"kind":"enum","v":"widget"}}]},
	  {"type":"comparison","field":"status","operator":"equals","value":{"kind":"string","v":"DELIVERED"}}]}`)
	if err := Validate(q, c); err != nil {
		t.Fatalf("validate: %v", err)
	}
}

// TestMismatchMessageNamesThePayload: the message is what the repair loop feeds
// back to the model, so it has to describe what arrived. "kind number is not
// compatible with type number" would be a riddle.
func TestMismatchMessageNamesThePayload(t *testing.T) {
	c := valueTypeConfig(t)
	q := valueTypeAST(t, `{"type":"comparison","field":"amount","operator":"equals","value":{"kind":"number","v":"42"}}`)
	err := Validate(q, c)
	if err == nil {
		t.Fatal("expected an error")
	}
	want := `value text "42" is not compatible with field type "number"`
	if got := err.Error(); !strings.Contains(got, want) {
		t.Errorf("message = %q, want it to contain %q", got, want)
	}
}

// --- small test helpers ---

func asValidationErrors(err error, out *ValidationErrors) bool {
	var v ValidationErrors
	ok := errors.As(err, &v)
	if ok {
		*out = v
	}
	return ok && len(v) > 0
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
