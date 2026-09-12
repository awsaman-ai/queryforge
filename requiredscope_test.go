package queryforge

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/awsaman-ai/queryforge/internal/testutil"
)

// Tests for policy.requiredScope — the config key that turns the Scope argument
// from optional into mandatory.
//
// The feature has two halves and they fail in opposite directions, so both are
// covered separately below:
//
//   - PRESENCE. Scope enforcement was always airtight once a scope existed, but
//     nothing obliged the caller to pass one. A forgotten argument produced a
//     valid, unscoped, cross-tenant query with no error anywhere.
//   - VALUE. A key that is present but blank (`tenantId: ""`) compiles to
//     `tenant_id = ''`, matches nothing, and returns an empty result that is
//     indistinguishable from "this tenant genuinely has no rows".
//
// Both are silent by nature, which is why the tests lean on asserting that the
// specific error fires rather than merely that some error did.

// requiredScopeConfig returns the standard scope test config with a
// policy.requiredScope block spliced in. Reusing scopeConfigJSON keeps these
// tests on the same fields (tenantId hidden, amount bounded) the rest of the
// scope suite uses.
func requiredScopeConfig(t *testing.T, policyJSON string) *Config {
	t.Helper()
	return testutil.MustParse(t, withRequiredScope(policyJSON))
}

// withRequiredScope splices a requiredScope policy into scopeConfigJSON.
func withRequiredScope(policyJSON string) string {
	return strings.Replace(scopeConfigJSON,
		`"defaults":{"limit":50,"maxLimit":500}`,
		`"defaults":{"limit":50,"maxLimit":500},"policy":{"requiredScope":`+policyJSON+`}`, 1)
}

// requiredScopeEngine builds an engine whose config demands scope.
func requiredScopeEngine(t *testing.T, policyJSON string, p ModelProvider) *Engine {
	t.Helper()
	e := NewWithProvider(requiredScopeConfig(t, policyJSON), p)
	e.Now = func() time.Time { return testutil.FixedNow }
	return e
}

// --- config: rules that must load -------------------------------------------

// TestRequiredScopeValidConfigsLoad covers every legal spelling of the key,
// including the one that matters most for the design: naming a scope key that
// is NOT a registered field. A hidden tenant column is the recommended setup, so
// if that case failed to load the safest configuration would be unusable.
func TestRequiredScopeValidConfigsLoad(t *testing.T) {
	for _, tc := range []struct{ name, policy string }{
		{"mode any", `{"mode":"any"}`},
		{"mode fields, one key", `{"mode":"fields","fields":["tenantId"]}`},
		{"mode fields, several keys", `{"mode":"fields","fields":["tenantId","userId","orgId"]}`},
		{"mode fields, unregistered key", `{"mode":"fields","fields":["subscriptionId"]}`},
		{"mode fields, dotted key", `{"mode":"fields","fields":["claims.tenantId"]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, err := ParseConfig([]byte(withRequiredScope(tc.policy)))
			if err != nil {
				t.Fatalf("config should load: %v", err)
			}
			if _, ok := c.RequiredScopeKeys(); !ok {
				t.Errorf("RequiredScopeKeys reports scope optional, but the config requires it")
			}
		})
	}
}

// TestRequiredScopeAbsentKeepsScopeOptional is the backward-compatibility pin.
// Every config written before this key existed must behave exactly as it did.
func TestRequiredScopeAbsentKeepsScopeOptional(t *testing.T) {
	c := scopeConfig(t)
	if keys, ok := c.RequiredScopeKeys(); ok {
		t.Fatalf("scope must stay optional when the key is absent; got required keys %v", keys)
	}

	e := scopeEngine(t, nil)
	if _, err := e.GenerateFrom(userQuery(), "sql", nil); err != nil {
		t.Errorf("a nil scope must still work when no rule is set: %v", err)
	}
}

// --- config: rules that must be rejected ------------------------------------

// TestRequiredScopeInvalidConfigsRejected checks that every malformed rule fails
// at load. A security control that parses but never fires is the precise failure
// this key was introduced to remove, so "tolerated near-miss" is not an option:
// each case must also name the offending part so the author can find it.
func TestRequiredScopeInvalidConfigsRejected(t *testing.T) {
	for _, tc := range []struct{ name, policy, wantSubstr string }{
		{"missing mode", `{}`, "mode"},
		{"empty mode", `{"mode":""}`, "mode"},
		{"unknown mode", `{"mode":"rbac"}`, "mode"},
		{"mode any with fields", `{"mode":"any","fields":["tenantId"]}`, "fields"},
		{"mode fields with no list", `{"mode":"fields"}`, "at least one"},
		{"mode fields with empty list", `{"mode":"fields","fields":[]}`, "at least one"},
		{"blank field name", `{"mode":"fields","fields":["tenantId","  "]}`, "empty"},
		{"field name with a space", `{"mode":"fields","fields":["tenant id"]}`, "not a valid scope key"},
		{"field name starting with a digit", `{"mode":"fields","fields":["1tenant"]}`, "not a valid scope key"},
		{"field name carrying SQL", `{"mode":"fields","fields":["tenant_id' OR 1=1 --"]}`, "not a valid scope key"},
		{"field name starting with $", `{"mode":"fields","fields":["$where"]}`, "not a valid scope key"},
		{"duplicate field name", `{"mode":"fields","fields":["tenantId","tenantId"]}`, "once"},
		{"duplicate after trimming", `{"mode":"fields","fields":["tenantId"," tenantId"]}`, "once"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseConfig([]byte(withRequiredScope(tc.policy)))
			if err == nil {
				t.Fatalf("config %s must be rejected, not silently accepted", tc.policy)
			}
			if !strings.Contains(err.Error(), tc.wantSubstr) {
				t.Errorf("error should mention %q, got: %v", tc.wantSubstr, err)
			}
		})
	}
}

// TestRequiredScopeStillRejectsTheOldNoOpKey pins that adding a working rule did
// not quietly revive the broken one. policy.requireTenantPredicate was removed
// because it read as access control and enforced nothing; a config still using
// it must fail loudly rather than appear to be covered by the new key.
func TestRequiredScopeStillRejectsTheOldNoOpKey(t *testing.T) {
	js := withRequiredScope(`{"mode":"any"}`)
	js = strings.Replace(js, `"policy":{"requiredScope":`,
		`"policy":{"requireTenantPredicate":true,"requiredScope":`, 1)

	_, err := ParseConfig([]byte(js))
	if err == nil {
		t.Fatal("requireTenantPredicate must still be rejected")
	}
	if !strings.Contains(err.Error(), "requireTenantPredicate") {
		t.Errorf("error should name the dead key, got: %v", err)
	}
}

// --- enforcement: mode "any" ------------------------------------------------

// TestRequiredScopeAnyRejectsMissingScope is the core bad path: the caller who
// simply forgot the argument. Both spellings of "no scope" must fail, because
// nil and an empty map are equally easy to arrive at by accident.
func TestRequiredScopeAnyRejectsMissingScope(t *testing.T) {
	e := requiredScopeEngine(t, `{"mode":"any"}`, nil)

	for _, tc := range []struct {
		name  string
		scope Scope
	}{
		{"nil scope", nil},
		{"empty scope", Scope{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.GenerateFrom(userQuery(), "sql", tc.scope)
			if err == nil {
				t.Fatal("a config requiring scope must reject a query with none")
			}
			if !errors.Is(err, ErrScope) {
				t.Errorf("error must be an ErrScope caller error, got: %v", err)
			}
			if !strings.Contains(err.Error(), "requires a scope") {
				t.Errorf("error should say scope is required, got: %v", err)
			}
		})
	}
}

// TestRequiredScopeAnyAcceptsAnyKey confirms mode "any" is satisfied by any
// non-empty scope — it asserts that scope exists, not what is in it.
func TestRequiredScopeAnyAcceptsAnyKey(t *testing.T) {
	e := requiredScopeEngine(t, `{"mode":"any"}`, nil)

	r := mustSQL(t, e, userQuery(), Scope{"anythingAtAll": "X"})
	if !strings.Contains(r.SQL, "anythingAtAll") {
		t.Errorf("scope predicate missing: %s", r.SQL)
	}
}

// --- enforcement: mode "fields" ---------------------------------------------

// TestRequiredScopeFieldsAcceptsCompleteScope is the happy path, checked all the
// way to the emitted SQL so the test proves the query really was confined rather
// than merely that no error was returned.
func TestRequiredScopeFieldsAcceptsCompleteScope(t *testing.T) {
	e := requiredScopeEngine(t, `{"mode":"fields","fields":["tenantId","userId"]}`, nil)

	r := mustSQL(t, e, userQuery(), Scope{"tenantId": "ACME", "userId": "U-1"})

	want := "SELECT * FROM orders WHERE (tenant_id = $1 AND userId = $2 AND status = $3) LIMIT 50"
	if r.SQL != want {
		t.Errorf("SQL mismatch:\n got: %s\nwant: %s", r.SQL, want)
	}
	if len(r.Args) != 3 || r.Args[0] != "ACME" || r.Args[1] != "U-1" {
		t.Errorf("args mismatch: %v", r.Args)
	}
}

// TestRequiredScopeFieldsRejectsMissingKeys covers the subtle bad path mode
// "any" cannot catch: a scope that was passed, looks populated, and is missing
// the one key that confines the query.
func TestRequiredScopeFieldsRejectsMissingKeys(t *testing.T) {
	e := requiredScopeEngine(t, `{"mode":"fields","fields":["tenantId","userId"]}`, nil)

	for _, tc := range []struct {
		name        string
		scope       Scope
		wantNamed   []string
		wantUnnamed []string
	}{
		{"no scope at all", nil, []string{"tenantId", "userId"}, nil},
		{"empty scope", Scope{}, []string{"tenantId", "userId"}, nil},
		{"one of two missing", Scope{"tenantId": "ACME"}, []string{"userId"}, []string{"tenantId"}},
		{"the other missing", Scope{"userId": "U-1"}, []string{"tenantId"}, []string{"userId"}},
		{"populated but wrong keys", Scope{"region": "eu"}, []string{"tenantId", "userId"}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.GenerateFrom(userQuery(), "sql", tc.scope)
			if err == nil {
				t.Fatal("a scope missing a required key must be rejected")
			}
			if !errors.Is(err, ErrScope) {
				t.Errorf("error must be an ErrScope caller error, got: %v", err)
			}
			// Every missing key is reported at once: a caller wiring scope up for
			// the first time should not have to rerun to find the second name.
			for _, name := range tc.wantNamed {
				if !strings.Contains(err.Error(), name) {
					t.Errorf("error should name missing key %q, got: %v", name, err)
				}
			}
			for _, name := range tc.wantUnnamed {
				if strings.Contains(err.Error(), name) {
					t.Errorf("error should not name %q, which WAS provided: %v", name, err)
				}
			}
		})
	}
}

// TestRequiredScopeFieldsAllowsExtraKeys checks the rule is a floor, not a
// whitelist. Requiring tenantId must not stop an application from also scoping
// by region on the same call.
func TestRequiredScopeFieldsAllowsExtraKeys(t *testing.T) {
	e := requiredScopeEngine(t, `{"mode":"fields","fields":["tenantId"]}`, nil)

	r := mustSQL(t, e, userQuery(), Scope{"tenantId": "ACME", "region": "eu"})
	for _, want := range []string{"tenant_id", "region"} {
		if !strings.Contains(r.SQL, want) {
			t.Errorf("expected %q in SQL: %s", want, r.SQL)
		}
	}
}

// TestRequiredScopeMatchesTrimmedKeys keeps the requirement check coherent with
// the predicate builder. Normalize trims keys before building predicates, so
// " tenantId" yields a tenantId predicate — it would be incoherent for the same
// key to be reported as missing.
func TestRequiredScopeMatchesTrimmedKeys(t *testing.T) {
	e := requiredScopeEngine(t, `{"mode":"fields","fields":["tenantId"]}`, nil)

	r := mustSQL(t, e, userQuery(), Scope{"  tenantId  ": "ACME"})
	if !strings.Contains(r.SQL, "tenant_id = $1") {
		t.Errorf("padded key should satisfy the rule and map normally: %s", r.SQL)
	}
}

// TestRequiredScopeRejectsPresentButUnusableValues is the second half of the
// feature. These keys are all present, so the presence check passes; each is
// rejected because its value could never confine anything. Every one of these
// is a realistic bug — an unset env var, a claim missing from a token, a nil
// pointer on an optional session field.
func TestRequiredScopeRejectsPresentButUnusableValues(t *testing.T) {
	e := requiredScopeEngine(t, `{"mode":"fields","fields":["tenantId"]}`, nil)

	var nilStr *string
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"nil", nil},
		{"empty string", ""},
		{"whitespace only", "   "},
		{"tab and newline", "\t\n"},
		{"nil string pointer", nilStr},
		{"pointer to empty string", strPtr("")},
		{"empty list", []string{}},
		{"list holding a blank", []string{"ACME", ""}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.GenerateFrom(userQuery(), "sql", Scope{"tenantId": tc.value})
			if err == nil {
				t.Fatalf("value %#v must be rejected, not compiled into a predicate", tc.value)
			}
			if !errors.Is(err, ErrScope) {
				t.Errorf("error must be an ErrScope caller error, got: %v", err)
			}
		})
	}
}

// TestBlankScopeValueRejectedWithoutAnyPolicy pins that the blank-value check is
// a standalone fix, not a sub-clause of requiredScope. `tenantId: ""` is a bug
// in every config: it compiles to `tenant_id = ”` and returns zero rows that
// read exactly like a tenant with no data.
func TestBlankScopeValueRejectedWithoutAnyPolicy(t *testing.T) {
	e := scopeEngine(t, nil) // no requiredScope block at all

	for _, tc := range []struct {
		name  string
		scope Scope
	}{
		{"blank scalar", Scope{"tenantId": ""}},
		{"whitespace scalar", Scope{"tenantId": " "}},
		{"blank inside a list", Scope{"tenantId": []string{"A", " "}}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := e.GenerateFrom(userQuery(), "sql", tc.scope)
			if err == nil {
				t.Fatal("a blank scope value must be rejected even with no policy set")
			}
			if !errors.Is(err, ErrScope) {
				t.Errorf("error must be an ErrScope caller error, got: %v", err)
			}
		})
	}
}

// TestBlankCheckLeavesLegitimateValuesAlone is the guard against overreach. Zero,
// false and an empty-looking date are all real values a real application scopes
// by, and rejecting them would break callers to no benefit — the check exists for
// blank strings only.
func TestBlankCheckLeavesLegitimateValuesAlone(t *testing.T) {
	e := requiredScopeEngine(t, `{"mode":"fields","fields":["k"]}`, nil)

	for _, tc := range []struct {
		name  string
		value any
	}{
		{"numeric zero", 0},
		{"float zero", 0.0},
		{"false", false},
		{"zero time", time.Time{}},
		{"string zero", "0"},
		{"list containing zero", []int{0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := e.GenerateFrom(userQuery(), "sql", Scope{"k": tc.value}); err != nil {
				t.Errorf("value %#v is legitimate and must be accepted: %v", tc.value, err)
			}
		})
	}
}

// --- whole flow -------------------------------------------------------------

// TestRequiredScopeFailsBeforeCallingTheModel checks where the rule fires, not
// just that it does. A missing scope is a bug in the calling code that no amount
// of re-prompting can fix, so it must cost neither an API call nor the latency
// of one.
func TestRequiredScopeFailsBeforeCallingTheModel(t *testing.T) {
	stub := &StubProvider{Response: `{"entity":"Order"}`}
	e := requiredScopeEngine(t, `{"mode":"fields","fields":["tenantId"]}`, stub)

	_, err := e.Translate(context.Background(), "show me everything", "sql", nil)
	if err == nil {
		t.Fatal("Translate must reject a missing required scope")
	}
	if !errors.Is(err, ErrScope) {
		t.Errorf("error must be an ErrScope caller error, got: %v", err)
	}
	if stub.Calls != 0 {
		t.Errorf("the model was called %d time(s); a caller-side scope bug must cost no API call", stub.Calls)
	}
}

// TestRequiredScopeTranslateEndToEnd runs the full AI path with the rule
// satisfied and follows the scope predicate all the way into the compiled SQL
// and the reported filters.
func TestRequiredScopeTranslateEndToEnd(t *testing.T) {
	astJSON := `{"entity":"Order","filter":{"type":"comparison","field":"status","operator":"equals","value":{"kind":"enum","v":"DELIVERED"}}}`
	stub := &StubProvider{Response: astJSON}
	e := requiredScopeEngine(t, `{"mode":"fields","fields":["tenantId"]}`, stub)

	res, err := e.Translate(context.Background(), "delivered orders", "sql", Scope{"tenantId": "ACME"})
	if err != nil {
		t.Fatalf("Translate with a complete scope: %v", err)
	}
	if stub.Calls != 1 {
		t.Errorf("expected exactly one model call, got %d", stub.Calls)
	}
	if !strings.Contains(res.Query.SQL, "tenant_id = $1") {
		t.Errorf("scope predicate missing from SQL: %s", res.Query.SQL)
	}
	if len(res.Scope) != 1 || res.Scope[0].Field != "tenantId" {
		t.Errorf("scope not reported on the result: %#v", res.Scope)
	}
	// The rule must not leak the hidden column into the prompt: requiring a key
	// says nothing to the model, which still must not know the field exists.
	if strings.Contains(stub.LastSystem, "tenantId") {
		t.Error("tenantId reached the system prompt; a required scope key stays invisible to the model")
	}
}

// TestRequiredScopeEnforcedOnEveryEntryPoint closes the obvious gap: a rule that
// only guarded Translate would be trivially bypassed by the caller who compiles
// a hand-built AST instead.
func TestRequiredScopeEnforcedOnEveryEntryPoint(t *testing.T) {
	e := requiredScopeEngine(t, `{"mode":"fields","fields":["tenantId"]}`, &StubProvider{Response: `{"entity":"Order"}`})

	for _, tc := range []struct {
		name string
		call func() error
	}{
		{"Translate", func() error {
			_, err := e.Translate(context.Background(), "everything", "sql", nil)
			return err
		}},
		{"GenerateFrom sql", func() error {
			_, err := e.GenerateFrom(userQuery(), "sql", nil)
			return err
		}},
		{"GenerateFrom mongo", func() error {
			_, err := e.GenerateFrom(userQuery(), "mongo", nil)
			return err
		}},
		{"ApplyScope", func() error {
			_, _, err := e.ApplyScope(userQuery(), nil)
			return err
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.call()
			if err == nil {
				t.Fatalf("%s must enforce policy.requiredScope", tc.name)
			}
			if !errors.Is(err, ErrScope) {
				t.Errorf("%s returned a non-ErrScope error: %v", tc.name, err)
			}
		})
	}
}

// TestRequiredScopeHoldsAcrossBackends checks the rule travels with the config
// rather than the generator, on both the relational and document paths.
func TestRequiredScopeHoldsAcrossBackends(t *testing.T) {
	e := requiredScopeEngine(t, `{"mode":"fields","fields":["tenantId"]}`, nil)

	// Bad: rejected identically on both.
	for _, backend := range []string{"sql", "mongo"} {
		if _, err := e.GenerateFrom(userQuery(), backend, Scope{"userId": "U-1"}); err == nil {
			t.Errorf("%s accepted a scope missing tenantId", backend)
		}
	}

	// Good: the predicate lands in each backend's native shape.
	r := mustSQL(t, e, userQuery(), Scope{"tenantId": "ACME"})
	if !strings.Contains(r.SQL, "tenant_id = $1") {
		t.Errorf("sql missing scope predicate: %s", r.SQL)
	}
	m, err := e.GenerateFrom(userQuery(), "mongo", Scope{"tenantId": "ACME"})
	if err != nil {
		t.Fatalf("GenerateFrom(mongo): %v", err)
	}
	if got := m.Doc.(*MongoQuery).Filter["tenantId"]; got != "ACME" {
		t.Errorf("mongo missing scope predicate: %#v", m.Doc.(*MongoQuery).Filter)
	}
}

// TestRequiredScopeCannotBeEscapedByOR re-runs the central safety property with
// the rule switched on, confirming the new presence check did not disturb where
// the predicate is spliced. The scope must still AND over the whole user filter.
func TestRequiredScopeCannotBeEscapedByOR(t *testing.T) {
	e := requiredScopeEngine(t, `{"mode":"fields","fields":["tenantId"]}`, nil)

	q := NewQuery("Order")
	q.Filter = testutil.Or(
		testutil.Comp("status", OpEquals, testutil.VEnum("DELIVERED")),
		testutil.Comp("status", OpEquals, testutil.VEnum("PLACED")),
	)

	r := mustSQL(t, e, q, Scope{"tenantId": "ACME"})
	want := "SELECT * FROM orders WHERE (tenant_id = $1 AND (status = $2 OR status = $3)) LIMIT 50"
	if r.SQL != want {
		t.Errorf("scope must AND over the whole OR group:\n got: %s\nwant: %s", r.SQL, want)
	}
}

// TestRequiredScopeErrorNamesEveryMissingKey pins the message quality for the
// widest case, where an application adds a third scope dimension and every call
// site is suddenly incomplete.
func TestRequiredScopeErrorNamesEveryMissingKey(t *testing.T) {
	e := requiredScopeEngine(t, `{"mode":"fields","fields":["tenantId","userId","orgId"]}`, nil)

	_, err := e.GenerateFrom(userQuery(), "sql", Scope{"userId": "U-1"})
	if err == nil {
		t.Fatal("expected an error")
	}
	for _, name := range []string{"tenantId", "orgId"} {
		if !strings.Contains(err.Error(), fmt.Sprintf("%q", name)) {
			t.Errorf("error should quote missing key %q, got: %v", name, err)
		}
	}
}
