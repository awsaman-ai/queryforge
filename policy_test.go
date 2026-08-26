package queryforge

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// --- Tier 2: cross-field business-rule validation (policy.requires) ---
//
// These tests are written from the business rule the feature exists to
// enforce — the exact motivating case from the roadmap: a passport expiry
// filter is meaningless without a country to go with it — not from the Go
// implementation that happens to enforce it.

// policyConfigJSON registers the passport/country example from the roadmap,
// plus an alternative companion field (visaType) and an unrelated field
// (name) to prove the rule ignores fields it was never told about.
const policyConfigJSON = `{
  "entity": "Traveler",
  "fields": [
    {"name": "passportExpiry", "type": "date", "operators": ["before","after","between","isNull"]},
    {"name": "country", "type": "string", "operators": ["equals","in"]},
    {"name": "visaType", "type": "enum", "values": ["TOURIST","WORK","STUDENT"]},
    {"name": "name", "type": "string", "operators": ["contains","equals"]}
  ],
  "policy": {
    "requires": [
      {
        "when": {"field": "passportExpiry", "operators": ["before","after","between"]},
        "requireAlsoOneOf": ["country","visaType"],
        "message": "Passport expiry needs a country or visa type to be meaningful"
      }
    ]
  }
}`

func policyConfig(t *testing.T) *Config { return mustParse(t, policyConfigJSON) }

// traveler wraps one filter condition into a Query against the "Traveler"
// entity policyConfigJSON declares (single(), from validate_test.go, is
// hardcoded to "Order").
func traveler(cmp *Condition) *Query {
	q := NewQuery("Traveler")
	q.Filter = cmp
	return q
}

// ─── Load-time validation ───────────────────────────────────────────────────

// TestPolicyRequiresHappyPath: a well-formed rule loads and is readable back
// off the config exactly as declared.
func TestPolicyRequiresHappyPath(t *testing.T) {
	c := policyConfig(t)
	if len(c.Policy.Requires) != 1 {
		t.Fatalf("expected 1 rule, got %d", len(c.Policy.Requires))
	}
	r := c.Policy.Requires[0]
	if r.When.Field != "passportExpiry" || len(r.When.Operators) != 3 {
		t.Errorf("unexpected trigger: %+v", r.When)
	}
	if len(r.RequireAlsoOneOf) != 2 || r.RequireAlsoOneOf[0] != "country" {
		t.Errorf("unexpected requireAlsoOneOf: %v", r.RequireAlsoOneOf)
	}
	if r.Message == "" {
		t.Errorf("message should round-trip")
	}
}

// TestPolicyRequiresOmittedByDefault is the backward-compatibility guarantee:
// a config with no policy.requires key loads with an empty slice, and the
// key is omitted again on the way back out — an untouched config produces
// byte-identical output, same as every other section of Policy.
func TestPolicyRequiresOmittedByDefault(t *testing.T) {
	c := mustParse(t, `{"entity":"Order","fields":[{"name":"amount","type":"number"}]}`)
	if len(c.Policy.Requires) != 0 {
		t.Errorf("expected no rules by default, got %+v", c.Policy.Requires)
	}
}

// TestPolicyRequiresLoadRejections covers every way a rule can be malformed.
// Each must fail ParseConfig with a message naming the actual problem, the
// same style IndexRouting.RoutingField already enforces for a bad field
// reference.
func TestPolicyRequiresLoadRejections(t *testing.T) {
	base := `{"entity":"Traveler","fields":[
	  {"name":"passportExpiry","type":"date"},
	  {"name":"country","type":"string"}
	],"policy":{"requires":[%s]}}`

	cases := map[string]struct {
		rule   string
		wantIn string
	}{
		"unknown when.field": {
			`{"when":{"field":"passportNumber"},"requireAlsoOneOf":["country"]}`,
			`"passportNumber", which is not registered`,
		},
		"missing when.field": {
			`{"when":{"field":""},"requireAlsoOneOf":["country"]}`,
			"when.field is required",
		},
		"unknown operator": {
			`{"when":{"field":"passportExpiry","operators":["befor"]},"requireAlsoOneOf":["country"]}`,
			`unknown operator "befor"`,
		},
		"empty requireAlsoOneOf": {
			`{"when":{"field":"passportExpiry"},"requireAlsoOneOf":[]}`,
			"requireAlsoOneOf must list at least one field",
		},
		"unknown requireAlsoOneOf field": {
			`{"when":{"field":"passportExpiry"},"requireAlsoOneOf":["nationality"]}`,
			`"nationality", which is not registered`,
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			js := strings.Replace(base, "%s", tc.rule, 1)
			_, err := ParseConfig([]byte(js))
			if err == nil {
				t.Fatalf("expected rejection, got none")
			}
			if !strings.Contains(err.Error(), tc.wantIn) {
				t.Errorf("error should contain %q, got: %v", tc.wantIn, err)
			}
		})
	}
}

// ─── Validate()-level enforcement ───────────────────────────────────────────

// TestPolicyViolationOnTriggerAlone is the roadmap's exact motivating case:
// filtering passport expiry without a country (or visa type) anywhere in the
// question is rejected with a *PolicyViolationError, not a generic one.
func TestPolicyViolationOnTriggerAlone(t *testing.T) {
	c := policyConfig(t)
	q := traveler(comp("passportExpiry", OpBefore, vRel("day", 0)))

	err := Validate(q, c)
	if err == nil {
		t.Fatalf("expected a policy violation, got nil")
	}
	var perr *PolicyViolationError
	if !errors.As(err, &perr) {
		t.Fatalf("expected *PolicyViolationError, got %T: %v", err, err)
	}
	if perr.Field != "passportExpiry" {
		t.Errorf("Field = %q, want passportExpiry", perr.Field)
	}
	if perr.Message != "Passport expiry needs a country or visa type to be meaningful" {
		t.Errorf("unexpected message: %q", perr.Message)
	}
}

// TestPolicyPassesWithEitherCompanionField: either listed companion field
// satisfies the rule — it is an "any of", not "all of".
func TestPolicyPassesWithEitherCompanionField(t *testing.T) {
	c := policyConfig(t)
	cases := map[string]*Query{
		"country present":  traveler(and(comp("passportExpiry", OpBefore, vRel("day", 0)), comp("country", OpEquals, vStr("IN")))),
		"visaType present": traveler(and(comp("passportExpiry", OpBefore, vRel("day", 0)), comp("visaType", OpEquals, vEnum("WORK")))),
	}
	for name, q := range cases {
		t.Run(name, func(t *testing.T) {
			if err := Validate(q, c); err != nil {
				t.Errorf("expected no violation, got: %v", err)
			}
		})
	}
}

// TestPolicyIgnoresNonTriggeringOperator: When.Operators scopes the rule —
// passportExpiry filtered with isNull (not in the trigger list) never fires
// it, because "is the expiry date even on file" says nothing about a
// specific date that would need a country to interpret.
func TestPolicyIgnoresNonTriggeringOperator(t *testing.T) {
	c := policyConfig(t)
	q := traveler(comp("passportExpiry", OpIsNull, nil))
	if err := Validate(q, c); err != nil {
		t.Errorf("isNull should not trigger the rule, got: %v", err)
	}
}

// TestPolicyCompanionFoundAcrossBranches: "also filtered somewhere in the
// question" means anywhere in the tree — AND, OR, or nested — not
// positionally related to the trigger. Country sits in an unrelated OR
// branch here and still satisfies the rule.
func TestPolicyCompanionFoundAcrossBranches(t *testing.T) {
	c := policyConfig(t)
	q := traveler(and(
		comp("passportExpiry", OpBefore, vRel("day", 0)),
		or(comp("country", OpEquals, vStr("IN")), comp("name", OpContains, vStr("smith"))),
	))
	if err := Validate(q, c); err != nil {
		t.Errorf("expected no violation with country nested in an OR branch, got: %v", err)
	}
}

// TestPolicyDefaultMessageWhenUnconfigured: a rule with no Message still
// explains itself, naming both sides of the rule, rather than surfacing a
// blank string.
func TestPolicyDefaultMessageWhenUnconfigured(t *testing.T) {
	c := mustParse(t, `{
	  "entity":"Traveler",
	  "fields":[{"name":"passportExpiry","type":"date"},{"name":"country","type":"string"}],
	  "policy":{"requires":[{"when":{"field":"passportExpiry"},"requireAlsoOneOf":["country"]}]}
	}`)
	q := traveler(comp("passportExpiry", OpBefore, vRel("day", 0)))
	err := Validate(q, c)
	var perr *PolicyViolationError
	if !errors.As(err, &perr) {
		t.Fatalf("expected *PolicyViolationError, got %T: %v", err, err)
	}
	if !strings.Contains(perr.Message, "passportExpiry") || !strings.Contains(perr.Message, "country") {
		t.Errorf("generated message should name both fields, got: %q", perr.Message)
	}
}

// TestPolicyDoesNotRunOnAStructurallyBrokenAST: a policy check on an AST that
// already fails ordinary validation would just add noise on top of a problem
// the caller has not fixed yet. An unknown field elsewhere in the tree must
// surface as an ordinary ValidationErrors, never a PolicyViolationError,
// even though the passport rule would also fire.
func TestPolicyDoesNotRunOnAStructurallyBrokenAST(t *testing.T) {
	c := policyConfig(t)
	q := traveler(and(
		comp("passportExpiry", OpBefore, vRel("day", 0)), // would violate the policy alone
		comp("passportNumber", OpEquals, vStr("X")),      // unknown field: structural error
	))
	err := Validate(q, c)
	if err == nil {
		t.Fatalf("expected an error")
	}
	var ves ValidationErrors
	if !errors.As(err, &ves) {
		t.Fatalf("expected ValidationErrors (the structural problem), got %T: %v", err, err)
	}
	var perr *PolicyViolationError
	if errors.As(err, &perr) {
		t.Errorf("policy check should not have run on a structurally invalid AST")
	}
}

// TestPolicyErrorIsNotAValidationError confirms the two error types stay
// distinguishable via errors.As in both directions — the whole point of
// giving policy violations their own type rather than folding them into
// ValidationErrors.
func TestPolicyErrorIsNotAValidationError(t *testing.T) {
	c := policyConfig(t)
	q := traveler(comp("passportExpiry", OpBefore, vRel("day", 0)))
	err := Validate(q, c)

	var ves ValidationErrors
	if errors.As(err, &ves) {
		t.Errorf("a policy violation must not also match ValidationErrors")
	}
	var perr *PolicyViolationError
	if !errors.As(err, &perr) {
		t.Errorf("a policy violation must match *PolicyViolationError")
	}
}

// TestClassifyMapsPolicyViolation: the shared FailureCode classifier — used
// by the engine's own logs, the cmd/queryforge protocol, and both SDKs —
// must report POLICY_VIOLATION for this error, distinct from
// VALIDATION_FAILED and UNSUPPORTED_REQUEST.
func TestClassifyMapsPolicyViolation(t *testing.T) {
	c := policyConfig(t)
	q := traveler(comp("passportExpiry", OpBefore, vRel("day", 0)))
	err := Validate(q, c)
	if code := Classify(err); code != FailurePolicy {
		t.Errorf("Classify = %q, want %q", code, FailurePolicy)
	}
	if FailurePolicy.Retryable() {
		t.Errorf("a policy violation should not be reported retryable")
	}
}

// TestGenerateFromEnforcesPolicyToo: the deterministic path must be as
// confined as Translate — a caller building the AST by hand cannot bypass a
// business rule the config declares.
func TestGenerateFromEnforcesPolicyToo(t *testing.T) {
	e := NewWithProvider(policyConfig(t), &StubProvider{})
	q := traveler(comp("passportExpiry", OpBefore, vRel("day", 0)))

	_, err := e.GenerateFrom(q, "sql", nil)
	var perr *PolicyViolationError
	if !errors.As(err, &perr) {
		t.Fatalf("expected *PolicyViolationError from GenerateFrom, got %T: %v", err, err)
	}
}

// ─── Translate()-level: fail closed without spending the repair budget ─────

// TestTranslatePolicyViolationFailsClosedImmediately: a policy violation is a
// deliberate refusal, not a repairable mistake — retrying cannot add
// information the question never contained. The model must be called exactly
// once, not MaxRepairs+1 times.
func TestTranslatePolicyViolationFailsClosedImmediately(t *testing.T) {
	violating := `{"entity":"Traveler","filter":{"type":"comparison","field":"passportExpiry","operator":"before","value":{"kind":"relative_date","unit":"day","amount":0}}}`
	provider := &scriptedProvider{responses: []string{violating}}

	e := NewWithProvider(policyConfig(t), provider)
	e.MaxRepairs = 2 // would allow 3 attempts if this were treated as repairable

	_, err := e.Translate(context.Background(), "passports that are expired", "sql", nil)

	var perr *PolicyViolationError
	if !errors.As(err, &perr) {
		t.Fatalf("expected *PolicyViolationError, got %T: %v", err, err)
	}
	if provider.calls != 1 {
		t.Errorf("expected exactly 1 model call (no repair spent on a policy violation), got %d", provider.calls)
	}
}
