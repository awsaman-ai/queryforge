package config

import (
	"errors"
	"strings"
	"testing"

	"queryforge.local/v2experiment/internal/qferr"
)

func TestLoadMissingFile(t *testing.T) {
	_, err := Load(fixture("does-not-exist.json"))
	if err == nil {
		t.Fatal("a missing file was accepted")
	}
	if got := qferr.CodeOf(err); got != qferr.CodeConfigError {
		t.Errorf("code = %s, want %s", got, qferr.CodeConfigError)
	}
	if !strings.Contains(err.Error(), "not found") {
		t.Errorf("message %q should say the file was not found", err.Error())
	}
}

func TestLoadMalformedJSON(t *testing.T) {
	_, err := Load(fixture("invalid-malformed-json.json"))
	if err == nil {
		t.Fatal("malformed JSON was accepted")
	}
	if got := qferr.CodeOf(err); got != qferr.CodeConfigError {
		t.Errorf("code = %s, want %s", got, qferr.CodeConfigError)
	}

	// The cause can quote file content, so it stays wrapped and out of the
	// message printed by default (section 16.10).
	var coded *qferr.Error
	if !errors.As(err, &coded) {
		t.Fatalf("error is not coded: %v", err)
	}
	if coded.Wrapped == nil {
		t.Error("the underlying JSON error should be wrapped for --debug")
	}
	if strings.Contains(coded.Message, coded.Wrapped.Error()) {
		t.Error("the cause leaked into the default message")
	}
}

// A file holding two JSON documents is almost always a bad merge or a stray
// brace. encoding/json stops at the end of the first value, so without an
// explicit check the second half would be ignored in silence.
func TestDecodeRejectsTrailingDocument(t *testing.T) {
	const doc = `{"entities":[{"name":"customer"}]}{"entities":[]}`
	_, err := Decode(strings.NewReader(doc))
	if err == nil {
		t.Fatal("a file with two JSON documents was accepted")
	}
	if !strings.Contains(err.Error(), "more than one JSON document") {
		t.Errorf("message %q should name the real problem", err.Error())
	}
}

// TestNormalizeDefaultsIdentifiers covers the rule that makes identifier
// validation meaningful: the default is validated exactly like an explicit
// value, so a natural name that is not a legal identifier forces an explicit
// table or column.
func TestNormalizeDefaultsIdentifiers(t *testing.T) {
	cfg := &Config{
		Entities: []Entity{{Name: "customer"}, {Name: "order", Table: "sales_orders"}},
		Fields: []Field{
			{Entity: "customer", Name: "country", Type: TypeString},
			{Entity: "customer", Name: "total_spent", Type: TypeNumber, Column: "spend_total"},
		},
	}
	cfg.Normalize()

	if got, want := cfg.Entities[0].Table, "customer"; got != want {
		t.Errorf("defaulted table = %q, want %q", got, want)
	}
	if got, want := cfg.Entities[1].Table, "sales_orders"; got != want {
		t.Errorf("explicit table = %q, want %q", got, want)
	}
	if got, want := cfg.Fields[0].Column, "country"; got != want {
		t.Errorf("defaulted column = %q, want %q", got, want)
	}
	if got, want := cfg.Fields[1].Column, "spend_total"; got != want {
		t.Errorf("explicit column = %q, want %q", got, want)
	}
}

// TestSecurityDefaults covers the "zero means unset" rule: a config overriding
// one limit must not lose the others, and a config with no security block at all
// must still be bounded.
func TestSecurityDefaults(t *testing.T) {
	cfg, err := Load(fixture("commerce.json"))
	if err != nil {
		t.Fatalf("commerce.json rejected: %v", err)
	}

	if got, want := cfg.Security.MaxJoins, 3; got != want {
		t.Errorf("MaxJoins = %d, want the configured %d", got, want)
	}
	if got, want := cfg.Security.MaxFilters, 12; got != want {
		t.Errorf("MaxFilters = %d, want the configured %d", got, want)
	}
	if got, want := cfg.Security.MaxInputLength, DefaultSecurityPolicy.MaxInputLength; got != want {
		t.Errorf("MaxInputLength = %d, want the default %d", got, want)
	}

	bare, err := Load(fixture("customers.json"))
	if err != nil {
		t.Fatalf("customers.json rejected: %v", err)
	}
	if bare.Security != DefaultSecurityPolicy {
		t.Errorf("a config with no security block should get every default, got %+v", bare.Security)
	}
}

// TestFieldFlagsDefaultToTrue covers the *bool convention: absent means
// permitted, which keeps a config that says nothing about a field usable.
func TestFieldFlagsDefaultToTrue(t *testing.T) {
	cfg, err := Load(fixture("commerce.json"))
	if err != nil {
		t.Fatalf("commerce.json rejected: %v", err)
	}
	r := NewSchemaRegistry(cfg)

	name, ok := r.Field("customer", "name")
	if !ok {
		t.Fatal("customer.name missing")
	}
	if !name.IsSelectable() || !name.IsFilterable() {
		t.Error("a field that says nothing should be selectable and filterable")
	}

	joinKey, ok := r.Field("order", "customer_id")
	if !ok {
		t.Fatal("order.customer_id missing")
	}
	if joinKey.IsSelectable() || joinKey.IsFilterable() {
		t.Error("order.customer_id is configured false for both flags")
	}

	id, ok := r.Field("customer", "id")
	if !ok {
		t.Fatal("customer.id missing")
	}
	// Explicitly false for one flag, absent for the other: the absent one must
	// not inherit the explicit one.
	if id.IsSelectable() {
		t.Error("customer.id is configured selectable:false")
	}
	if !id.IsFilterable() {
		t.Error("customer.id says nothing about filterable, so it should be filterable")
	}
}

func TestSummaryLeaksNothing(t *testing.T) {
	cfg, err := Load(fixture("commerce.json"))
	if err != nil {
		t.Fatalf("commerce.json rejected: %v", err)
	}
	summary := cfg.Summary()

	// Summary backs --validate-config, which a user may well run against a
	// production-shaped config. Counts only: no table names, enum values or
	// value aliases.
	for _, secret := range []string{"customers", "suspended", "laptop", "India", "IN"} {
		if strings.Contains(summary, secret) {
			t.Errorf("summary %q leaks config content %q", summary, secret)
		}
	}
	if !strings.Contains(summary, "entities=3") {
		t.Errorf("summary %q should report the entity count", summary)
	}
}
